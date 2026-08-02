// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
	"unicode"

	"github.com/burrow-cloud/burrow/client"
	"github.com/burrow-cloud/burrow/localconfig"
)

// Signing in to the Burrow Cloud target, as an RFC 8628 device authorization grant with PKCE
// (cloud ADR-0028). `burrow auth login` picks burrow-cloud.dev, the browser opens on the approval
// page, the person approves, and this — which has been polling — records the target.
//
// This client is in the open-source CLI deliberately, and there is no seam here for a managed build
// to fill. A device-flow client is standard OAuth with nothing proprietary in it: it posts a
// challenge, prints a code, opens a browser and polls, and PKCE exists precisely so that a public
// client holds no secret. What is commercial is the endpoints, the credential store, and the tenant
// model, none of which are here. A seam only a managed build could fill would mean the open-source
// binary can never reach Burrow Cloud, which is the separate `burrow-cloud` CLI that ADR-0078
// explicitly rejects — arrived at sideways, with the fragmentation hidden inside a build tag.
//
// Nothing in this file runs for a Kubernetes target: choosing "Other" at the picker needs no
// account and makes no call to the managed product (ADR-0078 §2).

// The device-flow endpoints on the managed control plane, and the grant they implement (RFC 8628
// §3.1/§3.4, RFC 7636 §4.2). The verification path is what the person's browser lands on.
const (
	deviceCodePath      = "/auth/device/code"
	deviceTokenPath     = "/auth/device/token"
	deviceCodeGrantType = "urn:ietf:params:oauth:grant-type:device_code"
	pkceMethodS256      = "S256"
)

// The poll's bounds. defaultPollInterval and defaultCodeLifetime apply only when the start response
// names neither, so a server that omits them is still polled politely rather than in a tight loop.
// slowDownStep is RFC 8628 §3.5's five-second widening, and maxPollInterval caps the widening so a
// run of slow_down cannot push the next poll past the code's own lifetime.
const (
	defaultPollInterval = 5 * time.Second
	defaultCodeLifetime = 10 * time.Minute
	slowDownStep        = 5 * time.Second
	maxPollInterval     = 60 * time.Second
)

// maxDeviceResponseBytes bounds what is read from a device-flow response, so a wrong or hostile
// endpoint cannot make the CLI hold an arbitrary amount of memory.
const maxDeviceResponseBytes = 1 << 20

// The seams a test substitutes, so no test opens a browser, sleeps, or reaches a real
// burrow-cloud.dev:
//
//   - cloudBaseURL is the origin the two endpoints hang off. A test points it at an httptest server.
//   - cloudHTTPClient is the only client this file makes requests through.
//   - openBrowserFn hands a URL to the desktop's own opener.
//   - deviceWaitFn is the pause between polls, so a test observes the interval instead of living it.
//   - hostnameFn labels the credential pair in the console's list.
//
// The client follows no redirects. Go replays a POST body across a 307/308, and the body of a
// redemption carries the device code and the PKCE verifier; a redirect off the token endpoint would
// hand both to whatever it pointed at. There is nothing legitimate for these two endpoints to
// redirect to, so a redirect is surfaced as the unexpected status it is.
var (
	cloudBaseURL    = "https://" + localconfig.CloudEndpoint
	cloudHTTPClient = &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	openBrowserFn = openBrowser
	deviceWaitFn  = waitFor
	hostnameFn    = os.Hostname
)

// deviceStart is RFC 8628 §3.2's device authorization response. VerificationURIComplete carries the
// user code in the URL, which is what makes the intended path two keystrokes and a click;
// VerificationURI is what gets printed when there is no browser to open.
type deviceStart struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// deviceTokens is a successful redemption: the person's credential and the agent's, minted together
// and each separately revocable (cloud ADR-0028 §2). There is no refresh token and no expiry, which
// is a statement rather than an omission — these credentials are revoked, not refreshed, so nothing
// here schedules a renewal.
//
// The two token fields are the only secrets this process holds. They go straight to disk and are
// never printed, logged, or put in an error. The response's token_type is not read: a Burrow token
// rides the X-Burrow-Token header rather than an Authorization one (ADR-0015), so there is no
// scheme here for it to name.
type deviceTokens struct {
	AccessToken       string `json:"access_token"`
	CredentialID      string `json:"credential_id"`
	AgentToken        string `json:"agent_token"`
	AgentCredentialID string `json:"agent_credential_id"`
	TenantID          string `json:"tenant_id"`
	Name              string `json:"name"`
}

// oauthError is RFC 6749 §5.2's error body, which RFC 8628 §3.5 reuses for the device flow and
// returns in a 400. The poll branches on Code, so it is carried as a typed error rather than a
// string.
type oauthError struct {
	Code        string `json:"error"`
	Description string `json:"error_description"`
	status      int
}

func (e *oauthError) Error() string {
	msg := e.Code
	// The description is written by the endpoint, and an error message ends up on a terminal, so it is
	// trimmed of anything that could move the cursor, redraw the line, or run on for a screenful.
	if d := sanitizeServerText(e.Description); d != "" {
		msg += ": " + d
	}
	// §3.5's errors arrive as 400s, so a status worth mentioning is one that is not — a rate limit on
	// the start endpoint, say, or something in front of the control plane answering for it.
	if e.status != 0 && e.status != http.StatusBadRequest {
		msg += fmt.Sprintf(" (HTTP %d)", e.status)
	}
	return msg
}

// cloudSignIn runs the device flow against the managed product and returns the target to record. It
// returns an error unless a credential pair was actually issued and written, so a sign-in that did
// not happen never leaves behind a target nothing can reach.
//
// interactive decides only whether a browser is opened: the flow itself works either way, and a
// headless run falls back to printing the URL for manual entry.
func cloudSignIn(ctx context.Context, out io.Writer, interactive bool) (localconfig.Target, error) {
	// A fresh verifier per attempt, so an abandoned sign-in cannot be completed by whatever
	// intercepted its device code (RFC 7636). The verifier is a secret and the challenge is not; only
	// the challenge is sent here.
	verifier, challenge, err := newPKCEPair()
	if err != nil {
		return localconfig.Target{}, err
	}

	start, err := startDeviceAuthorization(ctx, challenge)
	if err != nil {
		return localconfig.Target{}, err
	}
	announceDeviceCode(out, start, interactive)

	tokens, err := pollForTokens(ctx, start, verifier)
	if err != nil {
		return localconfig.Target{}, err
	}

	humanPath, agentPath, err := writeCloudCredentials(tokens)
	if err != nil {
		return localconfig.Target{}, err
	}
	reportCredentialLocations(out, humanPath, agentPath)
	return localconfig.CloudTarget(), nil
}

// newPKCEPair returns a fresh code verifier and its S256 challenge (RFC 7636 §4.1, §4.2). The
// verifier is 32 random bytes rendered base64url, well inside the RFC's 43–128 character range.
func newPKCEPair() (verifier, challenge string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("generating the sign-in verifier: %w", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// startDeviceAuthorization asks the control plane to begin an authorization, binding it to the PKCE
// challenge. The optional name is what the credential pair is called in the console's list, so a
// person who has signed in from several machines can tell which row is which.
//
// No client_id is sent. RFC 8628 §3.1 names one because it assumes several registered clients per
// authorization server; this endpoint has one public client and identifies nothing by it, so sending
// a constant would be a field that looks like an identity and is not one.
func startDeviceAuthorization(ctx context.Context, challenge string) (deviceStart, error) {
	form := url.Values{
		"code_challenge":        {challenge},
		"code_challenge_method": {pkceMethodS256},
	}
	if name := deviceName(); name != "" {
		form.Set("name", name)
	}

	var start deviceStart
	if err := postDeviceForm(ctx, deviceCodePath, form, &start); err != nil {
		return deviceStart{}, fmt.Errorf("starting sign-in to %s: %w", localconfig.CloudEndpoint, err)
	}
	if start.DeviceCode == "" || start.UserCode == "" || approvalURL(start) == "" {
		return deviceStart{}, fmt.Errorf(
			"%s answered the sign-in request without a code to approve, so there is nothing to show you.\n"+
				"Try again in a moment; if it keeps happening the service is having trouble.", localconfig.CloudEndpoint)
	}
	return start, nil
}

// pollForTokens waits for the person to approve in the browser, honouring RFC 8628 §3.5.
//
// slow_down widens the interval rather than being ignored: the control plane ratchets its own
// interval on an early poll, keyed on the device code, so a client that keeps its original cadence
// only makes its own sign-in slower. authorization_pending keeps waiting; every other error is
// terminal and names what to do next.
//
// The budget is spent in interval-sized steps rather than read off the wall clock, so the loop is
// deterministic and carries no ambient time. The control plane refuses an expired code in any case,
// which is the authority; this only stops the CLI polling a code it knows is dead. The guard is on
// the poll that is ABOUT to happen — a wait that would land past the expiry is not taken, rather
// than taken and then asked about a code that cannot answer.
func pollForTokens(ctx context.Context, start deviceStart, verifier string) (deviceTokens, error) {
	interval := time.Duration(start.Interval) * time.Second
	if interval <= 0 {
		interval = defaultPollInterval
	}
	budget := time.Duration(start.ExpiresIn) * time.Second
	if budget <= 0 {
		budget = defaultCodeLifetime
	}

	// The verifier is sent here and only here. Its whole purpose is that redemption proves possession
	// of the secret half of the binding the start request published.
	form := url.Values{
		"grant_type":    {deviceCodeGrantType},
		"device_code":   {start.DeviceCode},
		"code_verifier": {verifier},
	}

	for spent := time.Duration(0); ; {
		// The first poll always happens; after that, a wait that would land past the expiry is not
		// taken at all, rather than taken and then spent asking about a code that cannot answer.
		if spent > 0 && spent+interval > budget {
			return deviceTokens{}, errDeviceExpired()
		}
		if err := deviceWaitFn(ctx, interval); err != nil {
			return deviceTokens{}, fmt.Errorf(
				"sign-in to %s was interrupted before it was approved; nothing was recorded", localconfig.CloudEndpoint)
		}
		spent += interval

		var tokens deviceTokens
		err := postDeviceForm(ctx, deviceTokenPath, form, &tokens)
		if err == nil {
			if tokens.AccessToken == "" || tokens.AgentToken == "" {
				return deviceTokens{}, fmt.Errorf(
					"%s approved the sign-in but did not issue both credentials, so nothing was stored.\n"+
						"Run `burrow auth login` again.", localconfig.CloudEndpoint)
			}
			return tokens, nil
		}

		var oe *oauthError
		if !errors.As(err, &oe) {
			return deviceTokens{}, err
		}
		switch oe.Code {
		case "authorization_pending":
			continue
		case "slow_down":
			if interval += slowDownStep; interval > maxPollInterval {
				interval = maxPollInterval
			}
			continue
		case "expired_token":
			return deviceTokens{}, errDeviceExpired()
		case "access_denied":
			return deviceTokens{}, errors.New(
				"the sign-in was declined in the browser, so nothing was recorded.\n" +
					"If that was not you, no credential was issued and there is nothing to revoke.\n" +
					"Run `burrow auth login` to try again.")
		case "invalid_grant":
			return deviceTokens{}, fmt.Errorf(
				"%s did not accept this sign-in, so nothing was recorded.\n"+
					"The code may already have been used, or this terminal is not the one that started it.\n"+
					"Run `burrow auth login` to start a new one.", localconfig.CloudEndpoint)
		default:
			return deviceTokens{}, fmt.Errorf("signing in to %s failed: %w", localconfig.CloudEndpoint, err)
		}
	}
}

// errDeviceExpired is the message for a code that ran out before anyone approved it — reached both
// when the control plane says so and when the local budget is spent.
func errDeviceExpired() error {
	return errors.New(
		"the sign-in code expired before it was approved, so nothing was recorded.\n" +
			"Run `burrow auth login` to get a new one.")
}

// postDeviceForm posts a form-encoded request to a device-flow endpoint and decodes a 200 into into.
// A non-200 is decoded as RFC 8628's error body and returned as *oauthError, which is what the poll
// branches on.
func postDeviceForm(ctx context.Context, path string, form url.Values, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cloudBaseURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("building the request to %s: %w", cloudBaseURL+path, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", client.ClientNameCLI+"/"+cliVersion())

	resp, err := cloudHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("reaching %s: %w", cloudBaseURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDeviceResponseBytes))
	if err != nil {
		return fmt.Errorf("reading the reply from %s: %w", cloudBaseURL, err)
	}
	if resp.StatusCode != http.StatusOK {
		return decodeOAuthError(resp.StatusCode, body)
	}
	// A JSON decode failure names a position and a Go type, never the document's contents, so this
	// cannot carry a token into an error even though the redemption body holds two.
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("%s replied with something that is not the JSON this expects: %w", cloudBaseURL, err)
	}
	return nil
}

// decodeOAuthError turns a non-200 into the typed error the poll branches on. A body that is not
// RFC 6749's error shape — an HTML error page from something in front of the control plane, say —
// becomes a plain status error rather than being echoed back, since its contents are not known to be
// safe to print.
func decodeOAuthError(status int, body []byte) error {
	e := &oauthError{status: status}
	if err := json.Unmarshal(body, e); err != nil || e.Code == "" {
		return fmt.Errorf("%s answered with HTTP %d", cloudBaseURL, status)
	}
	return e
}

// announceDeviceCode shows the code and gets the person to the approval page.
//
// The browser opening is the intended path and printing the URL is the FALLBACK, for when there is
// no browser to open (cloud ADR-0028 §1) — but the code is printed EITHER WAY. Being able to compare
// what the terminal says with what the browser shows is what makes approval an act rather than a
// reflex, so it is not hidden just because the browser opened.
func announceDeviceCode(out io.Writer, start deviceStart, interactive bool) {
	fmt.Fprintf(out, "\nSigning in to %s.\n\n", localconfig.CloudEndpoint)
	fmt.Fprintf(out, "  Your code:  %s\n\n", emphasize(out, start.UserCode))

	if interactive && openApprovalPage(approvalURL(start)) {
		fmt.Fprintf(out, "Opening %s in your browser.\n", displayURL(start))
		fmt.Fprintln(out, "Check the page shows this same code, then approve it.")
	} else {
		fmt.Fprintln(out, "Open this page, check it shows this same code, then approve it:")
		fmt.Fprintf(out, "\n  %s\n", approvalURL(start))
	}
	fmt.Fprintln(out, "\nWaiting for approval...")
}

// approvalURL is the page to send the person to: the one carrying the code, so the field is already
// filled, falling back to the bare verification URI when the control plane names no complete form.
func approvalURL(start deviceStart) string {
	if start.VerificationURIComplete != "" {
		return start.VerificationURIComplete
	}
	return start.VerificationURI
}

// displayURL is the shorter URL to NAME when a browser has been opened, since the person does not
// have to read or type it and the code is on the line above.
func displayURL(start deviceStart) string {
	if start.VerificationURI != "" {
		return start.VerificationURI
	}
	return start.VerificationURIComplete
}

// openApprovalPage hands the approval URL to the desktop's browser and reports whether that
// succeeded, so the caller can fall back to printing it. The URL comes from a server response, so it
// is checked to be an ordinary http(s) URL before being handed to the operating system.
func openApprovalPage(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	return openBrowserFn(u.String()) == nil
}

// openBrowser opens a URL with the platform's own handler. It does not wait for the browser to exit
// — the CLI is polling in the meantime — and it wires no streams, so nothing the browser prints
// lands in the middle of the sign-in output.
func openBrowser(target string) error {
	var name string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		name = "open"
	case "windows":
		name, args = "rundll32", []string{"url.dll,FileProtocolHandler"}
	default:
		name = "xdg-open"
	}
	// target has already been checked to be an ordinary http(s) URL, and it is passed as an argument
	// rather than through a shell, so there is nothing here for it to be interpreted as.
	cmd := exec.Command(name, append(args, target)...)
	if err := cmd.Start(); err != nil {
		return err
	}
	// Reaped in the background rather than waited on: the opener exits as soon as it has handed the
	// URL over, and the poll runs for minutes. Without this the finished child stays a zombie for the
	// whole sign-in.
	go func() { _ = cmd.Wait() }()
	return nil
}

// deviceName labels the credential pair in the console's credential list. The hostname is what makes
// a list of machines readable to the person deciding which one to revoke; an unavailable hostname is
// not an error, it just leaves the control plane to name the credential itself.
func deviceName() string {
	name, err := hostnameFn()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(name)
}

// waitFor pauses for d, or returns early if the context is cancelled, so ^C during a sign-in stops
// within the keystroke rather than at the end of the current poll interval.
func waitFor(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// maxServerTextRunes bounds how much server-written text is repeated to a terminal.
const maxServerTextRunes = 200

// sanitizeServerText makes a string written by the control plane safe to print: control characters
// (which include the escape that starts an ANSI sequence) become spaces, and the result is capped.
// Nothing in the device flow is a place for a remote endpoint to paint on somebody's terminal.
func sanitizeServerText(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n == maxServerTextRunes {
			b.WriteString("…")
			break
		}
		if unicode.IsControl(r) {
			r = ' '
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

// emphasize renders the user code so it stands out from the surrounding prose on a terminal, and
// plainly everywhere else, matching how the ✓/✗ marks are gated (style.go).
func emphasize(w io.Writer, s string) string {
	if isTerminal(w) {
		return ansiBold + s + ansiReset
	}
	return s
}
