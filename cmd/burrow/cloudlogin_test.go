// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/burrow-cloud/burrow/localconfig"
)

// Every seam the device flow touches is substituted here: the base URL and the HTTP client point at
// an httptest server, the browser opener records instead of opening, and the poll's pause records
// instead of sleeping. No test reaches a real burrow-cloud.dev, opens a browser, or waits.

// The stand-ins the stub control plane issues. They are deliberately not credential-shaped: nothing
// in a fixture should be mistakable for a token, and an assertion that "the token is not in the
// output" is only meaningful when the value it looks for could not plausibly be one.
const (
	fakeUserCode    = "WDJB-MJHT"
	fakeDeviceCode  = "device-code-under-test"
	fakeCLISecret   = "fake-cli-credential"
	fakeAgentSecret = "fake-agent-credential"
)

// pollReply is one scripted answer from the token endpoint. The last one in a script repeats, so a
// test that never expects to finish polling does not have to enumerate every attempt.
type pollReply struct {
	status int
	body   string
}

// oauthReply is one of RFC 8628 §3.5's errors, which the control plane returns in a 400.
func oauthReply(code string) pollReply {
	return pollReply{status: http.StatusBadRequest, body: `{"error":"` + code + `","error_description":"a description"}`}
}

// issuedPair is a successful redemption: the person's credential and the agent's, minted together
// (cloud ADR-0028 §2).
func issuedPair() pollReply {
	return pollReply{status: http.StatusOK, body: `{` +
		`"token_type":"Bearer",` +
		`"access_token":"` + fakeCLISecret + `","credential_id":"cred-human",` +
		`"agent_token":"` + fakeAgentSecret + `","agent_credential_id":"cred-agent",` +
		`"tenant_id":"tenant-under-test","name":"test-machine"}`}
}

// fakeCloud is the stub of the managed control plane's two device-flow endpoints, plus a record of
// what the client did: the forms it sent, the URLs it handed the browser, and the pauses it took
// between polls.
type fakeCloud struct {
	t       *testing.T
	server  *httptest.Server
	replies []pollReply

	interval  int
	expiresIn int
	// redirectTo, when set, is the Location the token endpoint sends with a redirect reply.
	redirectTo string

	// The handlers run on the server's own goroutines, so what they record is guarded. The browser
	// and wait seams are called on the test's goroutine and need nothing.
	mu         sync.Mutex
	startForms []url.Values
	tokenForms []url.Values

	browsed []string
	waited  []time.Duration
}

// requests returns copies of the recorded forms, so an assertion reads them without racing a handler.
func (f *fakeCloud) requests() (start, token []url.Values) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]url.Values(nil), f.startForms...), append([]url.Values(nil), f.tokenForms...)
}

// startCloud stands up the stub endpoints and substitutes every seam, restoring them afterwards.
// replies scripts the token endpoint in order; the final entry repeats.
func startCloud(t *testing.T, replies ...pollReply) *fakeCloud {
	t.Helper()
	f := &fakeCloud{t: t, replies: replies, interval: 5, expiresIn: 600}

	mux := http.NewServeMux()
	mux.HandleFunc("POST "+deviceCodePath, f.handleStart)
	mux.HandleFunc("POST "+deviceTokenPath, f.handleToken)
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)

	// Only the TRANSPORT is substituted, not the client. The production client's timeout and its
	// refusal to follow redirects are policy worth testing rather than replacing.
	origBase, origTransport := cloudBaseURL, cloudHTTPClient.Transport
	origOpen, origWait, origHost := openBrowserFn, deviceWaitFn, hostnameFn
	cloudBaseURL = f.server.URL
	cloudHTTPClient.Transport = f.server.Client().Transport
	openBrowserFn = func(u string) error { f.browsed = append(f.browsed, u); return nil }
	deviceWaitFn = func(_ context.Context, d time.Duration) error { f.waited = append(f.waited, d); return nil }
	hostnameFn = func() (string, error) { return "test-machine", nil }

	t.Cleanup(func() {
		cloudBaseURL, cloudHTTPClient.Transport = origBase, origTransport
		openBrowserFn, deviceWaitFn, hostnameFn = origOpen, origWait, origHost
	})
	return f
}

func (f *fakeCloud) handleStart(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		f.t.Errorf("start request body did not parse: %v", err)
	}
	f.mu.Lock()
	f.startForms = append(f.startForms, r.PostForm)
	f.mu.Unlock()

	verification := f.server.URL + "/auth/device"
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{` +
		`"device_code":"` + fakeDeviceCode + `","user_code":"` + fakeUserCode + `",` +
		`"verification_uri":"` + verification + `",` +
		`"verification_uri_complete":"` + verification + `?user_code=` + fakeUserCode + `",` +
		`"expires_in":` + strconv.Itoa(f.expiresIn) + `,"interval":` + strconv.Itoa(f.interval) + `,"name":"test-machine"}`))
}

func (f *fakeCloud) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		f.t.Errorf("token request body did not parse: %v", err)
	}
	f.mu.Lock()
	f.tokenForms = append(f.tokenForms, r.PostForm)
	attempt := len(f.tokenForms) - 1
	f.mu.Unlock()

	reply := pollReply{status: http.StatusBadRequest, body: `{"error":"authorization_pending"}`}
	if len(f.replies) > 0 {
		if attempt >= len(f.replies) {
			attempt = len(f.replies) - 1
		}
		reply = f.replies[attempt]
	}
	if f.redirectTo != "" {
		w.Header().Set("Location", f.redirectTo)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(reply.status)
	_, _ = w.Write([]byte(reply.body))
}

// completeURL is the approval page carrying the code — the one the browser is expected to open.
func (f *fakeCloud) completeURL() string {
	return f.server.URL + "/auth/device?user_code=" + fakeUserCode
}

// cloudRoundTripFunc adapts a function to http.RoundTripper, so a test can fail on any request the
// client makes rather than letting one leave the process.
type cloudRoundTripFunc func(*http.Request) (*http.Response, error)

func (f cloudRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// forbidCloud makes any contact with the managed product a test failure. It is how the self-hosted
// path is held to ADR-0078's promise that choosing a cluster needs no account and makes no call.
func forbidCloud(t *testing.T) {
	t.Helper()
	origBase, origTransport, origOpen := cloudBaseURL, cloudHTTPClient.Transport, openBrowserFn
	cloudBaseURL = "https://managed-product.invalid"
	cloudHTTPClient.Transport = cloudRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("a request reached the managed product: %s %s", r.Method, r.URL.Path)
		return nil, errors.New("no network in tests")
	})
	openBrowserFn = func(u string) error {
		t.Errorf("a browser was opened at %s", u)
		return nil
	}
	t.Cleanup(func() { cloudBaseURL, cloudHTTPClient.Transport, openBrowserFn = origBase, origTransport, origOpen })
}

// burrowHome points $BURROW_CONFIG at a temp directory and returns it, so nothing a test writes goes
// anywhere near the real ~/.burrow.
func burrowHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BURROW_CONFIG", filepath.Join(dir, "config"))
	return dir
}

// readCloudCredential reads back a stored credential file and asserts it is readable only by its
// owner, which is the property that keeps a token on disk from being a token anyone can have.
func readCloudCredential(t *testing.T, path string) cloudCredential {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("credential file %s: %v", path, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("%s is mode %#o, want 0600 — readable only by its owner", path, perm)
	}
	if dir, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Fatalf("credential directory: %v", err)
	} else if perm := dir.Mode().Perm(); perm != 0o700 {
		t.Errorf("%s is mode %#o, want 0700", filepath.Dir(path), perm)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var cred cloudCredential
	if err := json.Unmarshal(data, &cred); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	return cred
}

// TestCloudSignInOpensTheApprovalPageAndStoresBothCredentials is the intended experience end to end:
// the browser opens on the approval page, the code is on screen to compare against it, and the pair
// the approval issues is stored — the person's and the agent's — with neither token displayed.
func TestCloudSignInOpensTheApprovalPageAndStoresBothCredentials(t *testing.T) {
	home := burrowHome(t)
	f := startCloud(t, issuedPair())

	var out bytes.Buffer
	target, err := cloudSignIn(context.Background(), &out, true)
	if err != nil {
		t.Fatalf("cloudSignIn: %v", err)
	}
	if target != localconfig.CloudTarget() {
		t.Errorf("target = %+v, want the managed product's target", target)
	}

	// The browser is opened on the page that already carries the code: two keystrokes and a click.
	if len(f.browsed) != 1 || f.browsed[0] != f.completeURL() {
		t.Errorf("browser opened on %v, want exactly %q", f.browsed, f.completeURL())
	}

	got := out.String()
	// The code is shown even though the browser opened. Comparing it with the browser is what makes
	// approving an act rather than a reflex.
	if !strings.Contains(got, fakeUserCode) {
		t.Errorf("the user code is not in the output, so there is nothing to compare against:\n%s", got)
	}
	if strings.Contains(got, fakeCLISecret) || strings.Contains(got, fakeAgentSecret) {
		t.Fatal("a token was printed to the terminal")
	}

	humanPath := filepath.Join(home, "credentials", cloudCredentialFile)
	agentPath := filepath.Join(home, "agents", cloudCredentialFile)
	if !strings.Contains(got, humanPath) || !strings.Contains(got, agentPath) {
		t.Errorf("login does not name both credential paths, so they cannot be found or deleted:\n%s", got)
	}

	human := readCloudCredential(t, humanPath)
	if human.Token != fakeCLISecret || human.Kind != cloudCredentialKindCLI {
		t.Errorf("the person's credential file holds kind %q and the wrong token", human.Kind)
	}
	if human.CredentialID != "cred-human" || human.TenantID != "tenant-under-test" {
		t.Errorf("credential = %+v, want the id and tenant the control plane named", redact(human))
	}

	agent := readCloudCredential(t, agentPath)
	if agent.Token != fakeAgentSecret || agent.Kind != cloudCredentialKindAgent {
		t.Errorf("the agent's credential file holds kind %q and the wrong token", agent.Kind)
	}
	if agent.CredentialID != "cred-agent" {
		t.Errorf("the agent credential id is %q, want the separately revocable one", agent.CredentialID)
	}
}

// redact renders a credential for a failure message with the token removed, so a test that fails
// does not print one.
func redact(c cloudCredential) cloudCredential {
	c.Token = "(redacted)"
	return c
}

// TestCloudSignInBindsRedemptionToAFreshPKCEVerifier confirms the PKCE contract: an S256 challenge on
// the start request, the verifier sent ONLY on redemption, the two actually related, and a fresh
// verifier per attempt so an abandoned sign-in cannot be completed by whatever saw its device code.
func TestCloudSignInBindsRedemptionToAFreshPKCEVerifier(t *testing.T) {
	burrowHome(t)
	f := startCloud(t, issuedPair())

	var out bytes.Buffer
	if _, err := cloudSignIn(context.Background(), &out, true); err != nil {
		t.Fatalf("first sign-in: %v", err)
	}
	if _, err := cloudSignIn(context.Background(), &out, true); err != nil {
		t.Fatalf("second sign-in: %v", err)
	}

	startForms, tokenForms := f.requests()
	if len(startForms) != 2 || len(tokenForms) != 2 {
		t.Fatalf("start requests = %d, redemptions = %d, want 2 of each", len(startForms), len(tokenForms))
	}
	for i, start := range startForms {
		if start.Get("code_challenge_method") != pkceMethodS256 {
			t.Errorf("attempt %d sent code_challenge_method %q, want %s", i, start.Get("code_challenge_method"), pkceMethodS256)
		}
		if start.Get("code_challenge") == "" {
			t.Errorf("attempt %d sent no code_challenge; the challenge is required", i)
		}
		if start.Get("code_verifier") != "" {
			t.Errorf("attempt %d sent the verifier on the START request; it is only ever sent on redemption", i)
		}
	}
	if a, b := startForms[0].Get("code_challenge"), startForms[1].Get("code_challenge"); a == b {
		t.Error("the second attempt reused the first attempt's challenge; each attempt needs a fresh verifier")
	}

	for i, redeem := range tokenForms {
		if redeem.Get("grant_type") != deviceCodeGrantType {
			t.Errorf("attempt %d redeemed with grant_type %q, want the device-code grant", i, redeem.Get("grant_type"))
		}
		if redeem.Get("device_code") != fakeDeviceCode {
			t.Errorf("attempt %d redeemed the wrong device code", i)
		}
		verifier := redeem.Get("code_verifier")
		if verifier == "" {
			t.Fatalf("attempt %d redeemed without a verifier", i)
		}
		sum := sha256.Sum256([]byte(verifier))
		if want := base64.RawURLEncoding.EncodeToString(sum[:]); want != startForms[i].Get("code_challenge") {
			t.Errorf("attempt %d: the verifier is not the pre-image of the challenge it started with", i)
		}
	}
}

// TestCloudSignInSlowDownWidensThePollInterval confirms RFC 8628 §3.5's slow_down is honoured rather
// than ignored. The control plane ratchets its own interval on an early poll, keyed on the device
// code, so a client that keeps its original cadence only makes its own sign-in slower.
func TestCloudSignInSlowDownWidensThePollInterval(t *testing.T) {
	burrowHome(t)
	f := startCloud(t, oauthReply("slow_down"), oauthReply("authorization_pending"), issuedPair())

	var out bytes.Buffer
	if _, err := cloudSignIn(context.Background(), &out, true); err != nil {
		t.Fatalf("cloudSignIn: %v", err)
	}

	want := []time.Duration{5 * time.Second, 10 * time.Second, 10 * time.Second}
	if len(f.waited) != len(want) {
		t.Fatalf("waited %v, want %v", f.waited, want)
	}
	for i := range want {
		if f.waited[i] != want[i] {
			t.Errorf("pause %d was %s, want %s (slow_down widens by %s and stays widened)", i, f.waited[i], want[i], slowDownStep)
		}
	}
}

// TestCloudSignInStopsOnATerminalError confirms the errors that are not "keep waiting" stop, name
// what happened, and say what to do — and that a sign-in that did not complete writes no credential
// and leaves nothing behind.
func TestCloudSignInStopsOnATerminalError(t *testing.T) {
	cases := map[string]struct {
		code string
		says []string
	}{
		"an expired code":     {code: "expired_token", says: []string{"expired", "burrow auth login"}},
		"a declined approval": {code: "access_denied", says: []string{"declined", "burrow auth login"}},
		"a rejected grant":    {code: "invalid_grant", says: []string{"did not accept", "burrow auth login"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			home := burrowHome(t)
			startCloud(t, oauthReply(tc.code))

			var out bytes.Buffer
			target, err := cloudSignIn(context.Background(), &out, true)
			if err == nil {
				t.Fatalf("cloudSignIn returned target %+v, want a stop on %s", target, tc.code)
			}
			for _, want := range tc.says {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to mention %q", err, want)
				}
			}
			for _, dir := range []string{"credentials", "agents"} {
				if _, statErr := os.Stat(filepath.Join(home, dir, cloudCredentialFile)); !errors.Is(statErr, fs.ErrNotExist) {
					t.Errorf("a credential was written under %s after a sign-in that did not complete", dir)
				}
			}
		})
	}
}

// TestCloudSignInStopsWhenTheCodeOutlivesItsBudget confirms the CLI stops polling a code it knows is
// dead, and that it stops BEFORE the poll that would land past the expiry rather than after it.
func TestCloudSignInStopsWhenTheCodeOutlivesItsBudget(t *testing.T) {
	burrowHome(t)
	f := startCloud(t, oauthReply("authorization_pending"))
	f.expiresIn = 12 // at five-second intervals, the third poll would land at t=15

	var out bytes.Buffer
	if _, err := cloudSignIn(context.Background(), &out, true); err == nil {
		t.Fatal("want a stop once the code's lifetime is spent")
	} else if !strings.Contains(err.Error(), "expired") {
		t.Errorf("error = %q, want it to say the code expired", err)
	}
	if _, tokenForms := f.requests(); len(tokenForms) != 2 {
		t.Errorf("polled %d times in a 12-second budget at 5-second intervals, want 2 (the third would be past expiry)", len(tokenForms))
	}
}

// TestCloudSignInFallsBackToPrintingTheURL confirms the manual path is the FALLBACK and still works:
// with no browser to open, the URL and the code are both on screen (cloud ADR-0028 §1).
func TestCloudSignInFallsBackToPrintingTheURL(t *testing.T) {
	burrowHome(t)
	f := startCloud(t, issuedPair())

	var out bytes.Buffer
	if _, err := cloudSignIn(context.Background(), &out, false); err != nil {
		t.Fatalf("cloudSignIn: %v", err)
	}
	if len(f.browsed) != 0 {
		t.Errorf("a browser was opened on a run with no terminal: %v", f.browsed)
	}
	got := out.String()
	if !strings.Contains(got, f.completeURL()) {
		t.Errorf("the approval URL is not printed, so there is no way to reach it:\n%s", got)
	}
	if !strings.Contains(got, fakeUserCode) {
		t.Errorf("the user code is not printed:\n%s", got)
	}
}

// TestCloudSignInPrintsTheCodeWhenTheBrowserOpens guards the thing most easily lost to tidying: the
// code stays on screen when the browser opened, because comparing the two is the check.
func TestCloudSignInPrintsTheCodeWhenTheBrowserOpens(t *testing.T) {
	burrowHome(t)
	startCloud(t, issuedPair())

	var out bytes.Buffer
	if _, err := cloudSignIn(context.Background(), &out, true); err != nil {
		t.Fatalf("cloudSignIn: %v", err)
	}
	before, _, found := strings.Cut(out.String(), "Waiting for approval")
	if !found {
		t.Fatalf("the flow never says it is waiting:\n%s", out.String())
	}
	if !strings.Contains(before, fakeUserCode) {
		t.Errorf("the code is not shown before the wait begins:\n%s", before)
	}
}

// TestCloudSignInReportsAnUnreachableEndpoint confirms a control plane that cannot be reached is a
// clear stop that records nothing, rather than a wall of transport detail.
func TestCloudSignInReportsAnUnreachableEndpoint(t *testing.T) {
	burrowHome(t)
	f := startCloud(t, issuedPair())
	f.server.Close()

	var out bytes.Buffer
	if _, err := cloudSignIn(context.Background(), &out, true); err == nil {
		t.Fatal("want a failure when the endpoint cannot be reached")
	} else if !strings.Contains(err.Error(), "starting sign-in to "+localconfig.CloudEndpoint) {
		t.Errorf("error = %q, want it to name what failed", err)
	}
}

// TestCloudSignInTightensAnAlreadyWidenedCredentialFile confirms re-authenticating over a file (or a
// directory) whose permissions had been widened leaves the credential readable only by its owner.
// Writing in place would not: a mode argument applies only when the file is created.
func TestCloudSignInTightensAnAlreadyWidenedCredentialFile(t *testing.T) {
	home := burrowHome(t)
	startCloud(t, issuedPair())

	humanPath := filepath.Join(home, "credentials", cloudCredentialFile)
	if err := os.MkdirAll(filepath.Dir(humanPath), 0o755); err != nil {
		t.Fatalf("preparing a widened directory: %v", err)
	}
	if err := os.WriteFile(humanPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("preparing a widened file: %v", err)
	}

	var out bytes.Buffer
	if _, err := cloudSignIn(context.Background(), &out, true); err != nil {
		t.Fatalf("cloudSignIn: %v", err)
	}
	// readCloudCredential asserts 0600 on the file and 0700 on its directory.
	if cred := readCloudCredential(t, humanPath); cred.Token != fakeCLISecret {
		t.Error("the widened file was not replaced with the freshly issued credential")
	}
}

// TestSanitizeServerText confirms text the control plane wrote cannot paint on a terminal or run on
// forever when it is repeated back in an error.
func TestSanitizeServerText(t *testing.T) {
	if got := sanitizeServerText("polling too\x1b[2K often\n"); got != "polling too [2K often" {
		t.Errorf("sanitizeServerText = %q, want the escape neutralised", got)
	}
	if got := sanitizeServerText(strings.Repeat("x", 500)); len([]rune(got)) != maxServerTextRunes+1 {
		t.Errorf("sanitizeServerText returned %d runes, want it capped at %d plus an ellipsis", len([]rune(got)), maxServerTextRunes)
	}
}

// TestCloudSignInDoesNotFollowARedirect confirms the verifier and the device code are not replayed to
// wherever a redirect points. Go repeats a POST body across a 307, and that body is the redemption.
func TestCloudSignInDoesNotFollowARedirect(t *testing.T) {
	burrowHome(t)

	var elsewhere int
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		elsewhere++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(sink.Close)

	f := startCloud(t, pollReply{status: http.StatusTemporaryRedirect, body: ""})
	f.redirectTo = sink.URL + "/auth/device/token"

	var out bytes.Buffer
	if _, err := cloudSignIn(context.Background(), &out, true); err == nil {
		t.Fatal("want a redirected redemption to fail rather than be followed")
	}
	if elsewhere != 0 {
		t.Errorf("the redemption was replayed to another host %d times", elsewhere)
	}
}

// TestOpenApprovalPageRefusesANonBrowserURL confirms the URL handed to the operating system's opener
// is checked first. It arrives in a server response, and handing an arbitrary scheme to the desktop
// is not something to do on trust.
func TestOpenApprovalPageRefusesANonBrowserURL(t *testing.T) {
	orig := openBrowserFn
	opened := 0
	openBrowserFn = func(string) error { opened++; return nil }
	t.Cleanup(func() { openBrowserFn = orig })

	for _, bad := range []string{"", "file:///etc/passwd", "javascript:alert(1)", "https://", "::not a url"} {
		if openApprovalPage(bad) {
			t.Errorf("openApprovalPage(%q) opened a browser", bad)
		}
	}
	if opened != 0 {
		t.Errorf("the opener was invoked %d times for URLs that are not http(s)", opened)
	}
	if !openApprovalPage("https://example.test/auth/device?user_code=X") {
		t.Error("an ordinary https approval URL was refused")
	}
}
