// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"
)

// Publish makes an app reachable at a hostname in ONE operation (ADR-0041 §3): it routes the host
// to the app, points DNS at the cluster when a provider is configured, obtains the TLS certificate,
// and reports whether the app is actually live at the end of it. It is the whole chain rather than
// the first link of it — an exposure without DNS and without a certificate is not a reachable app,
// and reporting one as a success is the defect this operation exists to remove (issue #476).
//
// # The order is the decision, not an implementation detail
//
// Publish creates the Ingress WITHOUT a certificate annotation first, then verifies the path from
// the public internet, and only then attaches TLS:
//
//  1. route the host to the app over plain HTTP (no ACME order exists yet);
//  2. wait for the ingress controller to assign an external address;
//  3. write the DNS record at the configured provider, pointing the host at that address;
//  4. PRE-FLIGHT: the host must resolve to this cluster — at the zone's own authoritative
//     nameservers when that seam is wired — and a plain-HTTP request to the ACME challenge path
//     must be answered for it;
//  5. only then attach TLS, which is what opens the ACME order;
//  6. wait for the certificate, and report the converged verdict.
//
// Steps 4 and 5 are in that order because Let's Encrypt rate-limits failed authorizations per
// account per hostname per hour. A publish against a host whose DNS is wrong, or whose :80 path an
// intermediary swallows, would burn that budget on an order that cannot possibly complete, and the
// account is then locked out of the retry that WOULD have worked. cert-manager's own self-check
// covers the same ground, but it runs after the order is created; verifying first means a broken
// path costs nothing.
//
// # Honesty about the outcome
//
// A publish that did not end live returns a result with Reachable false, BlockedOn naming the one
// link to fix and Next naming the action that fixes it — never a bare URL that a browser will
// refuse to open. Publishing over plain HTTP to a host on an HSTS-preloaded TLD (`.dev` and the
// rest of publicHSTSTLDs) is refused outright rather than reported as executed: the browser rejects
// `http://` on those names before a request leaves it, so there is no sense in which that publish
// worked.
//
// # Guardrails
//
// Publish performs no guardrail evaluation of its own. It composes the guarded primitives — Expose
// (app.expose_public) and AddDomain (dns.write) — and each enforces its own, which is what ADR-0041
// §3 asks for. A held or denied link surfaces as that link's own outcome, so an agent sees
// `held: dns.write` rather than a generic publish failure. Because the links run in order, a
// guardrail that holds the DNS write holds it with the Ingress already created; re-running the
// publish with the confirmation is idempotent and picks up where it stopped.
func (e *Engine) Publish(ctx context.Context, req PublishRequest) (PublishResult, error) {
	host := strings.TrimSpace(req.Host)
	wantTLS := !req.NoTLS
	if err := (App{Name: req.App}).Validate(); err != nil {
		return PublishResult{}, fmt.Errorf("publish: %w: %w", ErrInvalid, err)
	}
	if host == "" {
		return PublishResult{}, fmt.Errorf("publish %s: host is empty: %w", req.App, ErrInvalid)
	}
	if req.Port <= 0 {
		return PublishResult{}, fmt.Errorf("publish %s: port %d must be positive: %w", req.App, req.Port, ErrInvalid)
	}
	issuer := strings.TrimSpace(req.Issuer)
	if wantTLS && issuer == "" {
		issuer = DefaultTLSIssuer
	}
	if !wantTLS {
		if tld, mandatory := httpsMandatory(host); mandatory {
			return PublishResult{}, fmt.Errorf(
				"publish %s: .%s is HSTS-preloaded, so a browser refuses http://%s before it sends a request — publish %s with TLS, or use a host on a domain that serves plain HTTP: %w",
				req.App, tld, host, host, ErrInvalid)
		}
	}

	// Every prerequisite for the WHOLE intent, checked before anything is created: an exposure that
	// gets as far as an Ingress and then discovers cert-manager is missing has already written DNS
	// (ADR-0006's structured checklist, ADR-0041 §4).
	if err := e.exposePrerequisites(ctx, ExposeRequest{App: req.App, Env: req.Env, Host: host, Port: req.Port, TLS: wantTLS}); err != nil {
		return PublishResult{}, fmt.Errorf("publish %s: %w", req.App, err)
	}

	res := PublishResult{App: req.App, Env: envName(req.Env), Host: host, Port: req.Port, TLSRequested: wantTLS}

	ns, err := e.resolveMutatingNamespace(ctx, req.Env)
	if err != nil {
		return PublishResult{}, fmt.Errorf("publish %s: %w", req.App, err)
	}
	k := e.k8s.WithNamespace(ns)

	// A re-publish of a host that ALREADY has its certificate request keeps it. Taking the
	// annotation off and putting it back is not a no-op: cert-manager owns the Certificate through
	// the annotation, so removing it deletes the Certificate and re-adding it opens a FRESH ACME
	// order — a publish run twice would then spend the rate limit this whole ordering exists to
	// protect. Nothing is being opened here, so there is nothing to hold back.
	current, err := k.ExposureStatus(ctx, req.App)
	if err != nil {
		// Refused rather than assumed: not knowing whether a certificate request is already there
		// is not knowing whether re-applying the Ingress would delete a Certificate and open a new
		// order for it.
		return PublishResult{}, fmt.Errorf("publish %s: reading the current exposure: %w", req.App, err)
	}
	if current.Exposed && current.Host == host {
		res.TLSAttached = wantTLS && current.TLS
	}

	// 1. Route the host over plain HTTP. TLS is deliberately NOT requested here on a first publish:
	// the Ingress carries no cert-manager annotation, so no ACME order exists yet.
	if _, err := e.Expose(ctx, ExposeRequest{App: req.App, Env: req.Env, Host: host, Port: req.Port, TLS: res.TLSAttached, Issuer: issuer, Confirm: req.Confirm}); err != nil {
		return PublishResult{}, err
	}
	res.Exposed = true

	// 2. The controller's external address: DNS has nothing to point at until one is assigned, and
	// a cluster whose LoadBalancer is still being provisioned reports none for a minute or two.
	address, err := e.awaitAddress(ctx, k, req.App)
	if err != nil {
		return res, fmt.Errorf("publish %s: %w", req.App, err)
	}
	res.Address = address
	if address == "" {
		e.publishVerdict(&res)
		return res, nil
	}

	// 3. DNS, when Burrow can write it. With no provider configured the record is the user's to
	// make — the address is in the result and the verdict names the action — and the pre-flight
	// still runs, because a host already pointed at the cluster by hand is a publish that can
	// finish (ADR-0041 §3).
	if !req.SkipDNS {
		configured, err := e.dnsProviderConfigured(ctx, req.Provider)
		if err != nil {
			return res, fmt.Errorf("publish %s: %w", req.App, err)
		}
		if configured {
			dns, err := e.AddDomain(ctx, AddDomainRequest{Host: host, Address: address, Provider: req.Provider, Confirm: req.Confirm})
			if err != nil {
				return res, err
			}
			res.DNSProvider = dns.Provider
			res.DNSRecordType = dns.Type
		}
	}

	// 4. The pre-flight: nothing below this line runs until the path a certificate authority would
	// take has been walked without one.
	//
	// A record Burrow just wrote is waited for, because it is expected to appear. A record Burrow
	// did NOT write is somebody else's to make, so it is checked once rather than waited on: nothing
	// is in flight, and minutes spent waiting for a change nobody has started are minutes the caller
	// could have spent being told which record to add. Re-running the publish afterwards is cheap.
	if res.DNSProvider != "" {
		res.DNSPointsAtCluster = e.awaitDNS(ctx, host, address)
	} else {
		res.DNSPointsAtCluster = e.dnsPointsAt(ctx, host, address)
	}
	if res.DNSPointsAtCluster {
		res.HTTPVerified = e.awaitHTTP(ctx, host)
	}

	// 5. Attach TLS — the step that opens the ACME order — only with the path proven.
	if wantTLS && res.DNSPointsAtCluster && (res.HTTPVerified || e.probe == nil) {
		if !res.TLSAttached {
			if _, err := e.Expose(ctx, ExposeRequest{App: req.App, Env: req.Env, Host: host, Port: req.Port, TLS: true, Issuer: issuer, Confirm: req.Confirm}); err != nil {
				return res, err
			}
			res.TLSAttached = true
		}
		// 6. Wait for cert-manager to complete the order it was just handed.
		res.CertReady = e.awaitCert(ctx, k, req.App)
	}

	// A publish the CALLER gave up on is an error, not a verdict: the waits above end early when the
	// context does, and "blocked on dns" read off an abandoned wait would be indistinguishable from
	// a link that was genuinely checked and found unready.
	if err := ctx.Err(); err != nil {
		return res, fmt.Errorf("publish %s: %w", req.App, err)
	}

	e.publishVerdict(&res)
	return res, nil
}

// PublishRequest describes making an app reachable at a hostname in one operation (ADR-0041 §3).
//
// Both negative fields are negative ON PURPOSE. This request crosses the API as JSON, so an absent
// field arrives as its zero value; with `TLS bool` a caller that forgot the field would silently
// publish in plain HTTP, which is the failure this operation exists to stop. Naming them NoTLS and
// SkipDNS makes the zero value the complete publish, and asking for less is something a caller has
// to say.
type PublishRequest struct {
	App string `json:"app"`
	// Env is the environment whose namespace the app lives in (ADR-0035); empty targets the default.
	Env  string `json:"env,omitempty"`
	Host string `json:"host"`
	Port int32  `json:"port"`
	// NoTLS publishes over plain HTTP, requesting no certificate. It is refused for a host on an
	// HSTS-preloaded TLD, where plain HTTP cannot be opened at all.
	NoTLS bool `json:"no_tls,omitempty"`
	// Issuer names the cert-manager ClusterIssuer the certificate is requested from; empty applies
	// DefaultTLSIssuer.
	Issuer string `json:"issuer,omitempty"`
	// SkipDNS leaves DNS alone even when a provider is configured, for a host whose record is
	// managed elsewhere.
	SkipDNS bool `json:"skip_dns,omitempty"`
	// Provider names the configured DNS provider to write the record at; empty auto-selects when
	// exactly one is configured.
	Provider string `json:"provider,omitempty"`
	// Confirm acknowledges the guardrails the links trip (app.expose_public, dns.write).
	Confirm bool `json:"confirm,omitempty"`
}

// PublishResult reports what a publish achieved, link by link, and whether the app ended up live.
// It is deliberately more than a URL: a caller — usually an agent relaying to a human — has to be
// able to tell "live at https://x" from "routed, but the certificate has not issued", and the
// second is not a success.
type PublishResult struct {
	App  string `json:"app"`
	Env  string `json:"env,omitempty"`
	Host string `json:"host"`
	Port int32  `json:"port"`
	// URL is where the app is live, set only when Reachable.
	URL string `json:"url,omitempty"`
	// Address is the external address the ingress controller assigned, and what DNS must point at.
	Address string `json:"address,omitempty"`
	Exposed bool   `json:"exposed"`
	// DNSProvider is the provider Burrow wrote the record at, empty when it wrote none (none
	// configured, SkipDNS, or the record was already the user's to manage).
	DNSProvider string `json:"dns_provider,omitempty"`
	// DNSRecordType is the record Burrow wrote ("A" or "CNAME"), empty when it wrote none.
	DNSRecordType string `json:"dns_record_type,omitempty"`
	// DNSPointsAtCluster reports the pre-flight's first half: the host resolves to this cluster.
	DNSPointsAtCluster bool `json:"dns_points_at_cluster"`
	// HTTPVerified reports the pre-flight's second half: a plain-HTTP request to the ACME challenge
	// path was answered for this host from outside the cluster. It is what gates the ACME order.
	HTTPVerified bool `json:"http_verified"`
	// TLSRequested is what the caller asked for; TLSAttached is whether the Ingress now carries the
	// cert-manager annotation. They differ exactly when the pre-flight held the order back.
	TLSRequested bool `json:"tls_requested"`
	TLSAttached  bool `json:"tls_attached"`
	CertReady    bool `json:"cert_ready"`
	// Reachable is the converged verdict: the app is live at URL.
	Reachable bool `json:"reachable"`
	// BlockedOn names the one link to fix when not Reachable; Next is the action that fixes it.
	BlockedOn string `json:"blocked_on,omitempty"`
	Next      string `json:"next,omitempty"`
	Summary   string `json:"summary"`
}

// DefaultTLSIssuer is the cert-manager ClusterIssuer a publish requests its certificate from when
// the caller names none. It matches the issuer `burrow cluster ingress install` creates.
const DefaultTLSIssuer = "letsencrypt"

const (
	// publishPollInterval is how often a publish re-reads the link it is waiting on.
	publishPollInterval = 5 * time.Second

	// acmeChallengePath is the path a certificate authority fetches over plain HTTP to satisfy an
	// HTTP-01 challenge. The pre-flight probes THIS path rather than `/` so what it verifies is the
	// request that actually has to work; any HTTP response proves the path (a 404 from the app is a
	// pass — the request reached the app through the cluster's ingress).
	acmeChallengePath = "/.well-known/acme-challenge/burrow-publish-preflight"
)

// publicHSTSTLDs are top-level domains on the HSTS preload list in their entirety, so every host
// under them is HTTPS-only in every mainstream browser: the browser upgrades or refuses the request
// before it is sent, and no server behaviour can opt out. Publishing to one over plain HTTP
// produces a URL nobody can open, so Publish refuses instead of reporting it as done.
//
// The list is the Google-operated TLDs preloaded as a whole; it is deliberately short and
// conservative — a name missing from it costs a publish that reports plain HTTP honestly, while a
// name wrongly on it would refuse a publish that would have worked.
var publicHSTSTLDs = []string{
	"app", "bank", "boo", "channel", "dad", "day", "dev", "esq", "foo", "gle", "google",
	"how", "ing", "insurance", "meme", "mov", "nexus", "new", "page", "phd", "prof", "rsvp",
	"soy", "zip",
}

// httpsMandatory reports whether host sits under an HSTS-preloaded TLD, and names that TLD.
func httpsMandatory(host string) (string, bool) {
	h := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	i := strings.LastIndex(h, ".")
	if i < 0 {
		return "", false
	}
	tld := h[i+1:]
	for _, t := range publicHSTSTLDs {
		if tld == t {
			return tld, true
		}
	}
	return "", false
}

// dnsProviderConfigured reports whether this publish writes the DNS record itself.
//
// A caller who NAMED a provider always gets a write attempt, so a name that is not configured, or
// one that does not serve DNS, comes back as AddDomain's own error naming it — silently skipping
// the record because the name was mistyped would leave the verdict telling the user to configure a
// provider they had already configured. With no name, exactly one configured DNS provider is
// written at. Several with no name given is not an error here: the publish carries on and leaves
// the record to the user, since `domain add --provider` is the way to choose between them.
func (e *Engine) dnsProviderConfigured(ctx context.Context, name string) (bool, error) {
	if strings.TrimSpace(name) != "" {
		return true, nil
	}
	providers, err := e.db.Providers(ctx)
	if err != nil {
		return false, fmt.Errorf("reading providers: %w", err)
	}
	serving := 0
	for _, p := range providers {
		if p.Serves(CapabilityDNS) {
			serving++
		}
	}
	return serving == 1, nil
}

// awaitAddress polls the app's exposure until the ingress controller has assigned an external
// address, returning "" when the bound elapses with none. A missing address is an answer (the
// verdict names it), not an error; a failing cluster read is an error.
func (e *Engine) awaitAddress(ctx context.Context, k Kubernetes, app string) (string, error) {
	var last error
	for elapsed := time.Duration(0); ; elapsed += publishPollInterval {
		exp, err := k.ExposureStatus(ctx, app)
		switch {
		case err != nil:
			last = fmt.Errorf("reading the exposure of %s: %w", app, err)
		case exp.Address != "":
			return exp.Address, nil
		default:
			last = nil // the read worked; there is simply no address yet, which is an answer
		}
		if elapsed >= PublishAddressTimeout {
			return "", last
		}
		if err := e.sleep(ctx, publishPollInterval); err != nil {
			return "", err
		}
	}
}

// awaitCert polls the app's exposure until cert-manager reports the certificate issued, or the
// bound elapses. A certificate that has not issued is an answer, not an error: the order is open
// and the verdict says so.
func (e *Engine) awaitCert(ctx context.Context, k Kubernetes, app string) bool {
	for elapsed := time.Duration(0); ; elapsed += publishPollInterval {
		if exp, err := k.ExposureStatus(ctx, app); err == nil && exp.CertReady {
			return true
		}
		if elapsed >= PublishCertTimeout {
			return false
		}
		if err := e.sleep(ctx, publishPollInterval); err != nil {
			return false
		}
	}
}

// awaitDNS polls until host resolves to the cluster's address, or the bound elapses. A record
// Burrow has just written is usually visible immediately at the zone's own nameservers and takes
// longer to reach a recursive resolver, which is why the authoritative seam is preferred when it
// is wired.
func (e *Engine) awaitDNS(ctx context.Context, host, address string) bool {
	for elapsed := time.Duration(0); ; elapsed += publishPollInterval {
		if e.dnsPointsAt(ctx, host, address) {
			return true
		}
		if elapsed >= PublishDNSTimeout {
			return false
		}
		if err := e.sleep(ctx, publishPollInterval); err != nil {
			return false
		}
	}
}

// dnsPointsAt reports whether host resolves to the cluster's external address, asking the zone's
// authoritative nameservers first and a public recursive resolver second.
//
// The address is an IP for a LoadBalancer that has one and a HOSTNAME for a cloud that hands out a
// name instead, so a hostname address is compared by what it resolves to rather than by string:
// the record is then a CNAME, and the authoritative nameservers of the app's own zone will not
// follow it.
func (e *Engine) dnsPointsAt(ctx context.Context, host, address string) bool {
	addrs := e.lookupHost(ctx, host)
	if len(addrs) == 0 {
		return false
	}
	if slices.Contains(addrs, address) {
		return true
	}
	if net.ParseIP(address) != nil {
		return false
	}
	target := e.lookupHost(ctx, address)
	for _, a := range addrs {
		if slices.Contains(target, a) {
			return true
		}
	}
	return false
}

// lookupHost resolves host at the zone's authoritative nameservers when that seam is wired, falling
// back to the public resolver — which every build has — when it is not, or when the authoritative
// answer is empty (a CNAME the authoritative server does not follow, a nameserver that will not
// answer). It returns no error: an unresolvable host is a link that is not ready yet.
func (e *Engine) lookupHost(ctx context.Context, host string) []string {
	if e.authoritative != nil {
		if addrs, err := e.authoritative.LookupHostAuthoritative(ctx, host); err == nil && len(addrs) > 0 {
			return addrs
		}
	}
	addrs, err := e.resolver.LookupHost(ctx, host)
	if err != nil {
		return nil
	}
	return addrs
}

// awaitHTTP polls the ACME challenge path over plain HTTP until the cluster answers for this host,
// or the bound elapses. Without a probe seam wired it reports false and the caller treats the DNS
// half of the pre-flight as the whole of it.
func (e *Engine) awaitHTTP(ctx context.Context, host string) bool {
	if e.probe == nil {
		return false
	}
	url := "http://" + host + acmeChallengePath
	for elapsed := time.Duration(0); ; elapsed += publishPollInterval {
		if _, err := e.probe.ProbeHTTP(ctx, url); err == nil {
			return true
		}
		if elapsed >= PublishHTTPTimeout {
			return false
		}
		if err := e.sleep(ctx, publishPollInterval); err != nil {
			return false
		}
	}
}

// sleep waits d through the injected timer seam, so a test drives a publish's waits without real
// time and the engine reads no ambient clock (ADR-0010). It returns ctx.Err() if the caller gave up.
func (e *Engine) sleep(ctx context.Context, d time.Duration) error {
	after := e.after
	if after == nil {
		after = time.After
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-after(d):
		return nil
	}
}

// publishVerdict fills in the converged verdict: the first unready link, the action that unblocks
// it, the URL when there is one, and a one-line summary for a non-expert. It is pure — everything
// it reads is already on the result — so the honesty of a publish is testable without a cluster.
func (e *Engine) publishVerdict(res *PublishResult) {
	switch {
	case !res.Exposed:
		res.BlockedOn = "ingress"
		res.Next = fmt.Sprintf("run `burrow app publish %s --host %s --port %d` again", res.App, res.Host, res.Port)
	case res.Address == "":
		res.BlockedOn = "ingress controller"
		res.Next = "install an ingress controller with `burrow cluster ingress install`, then publish again"
	case !res.DNSPointsAtCluster:
		res.BlockedOn = "dns"
		if res.DNSProvider != "" {
			res.Next = fmt.Sprintf("the %s record for %s was written at %s but %s does not resolve to %s yet — a slow propagation, or another record for the same name — check with `burrow app reachability %s`",
				res.DNSRecordType, res.Host, res.DNSProvider, res.Host, res.Address, res.App)
		} else {
			res.Next = fmt.Sprintf("point %s at %s in your DNS, or configure a provider with `burrow config provider add` and publish again", res.Host, res.Address)
		}
	case !res.CertReady && e.probe != nil && !res.HTTPVerified:
		res.BlockedOn = "http path"
		res.Next = fmt.Sprintf("%s resolves to the cluster but nothing answered http://%s — check the ingress controller is serving and publish again", res.Host, res.Host)
	case res.TLSRequested && !res.CertReady:
		res.BlockedOn = "tls certificate"
		res.Next = fmt.Sprintf("cert-manager is still issuing the certificate — watch it with `burrow app reachability %s --wait`", res.App)
	default:
		res.Reachable = true
		res.URL = "http://" + res.Host
		if res.TLSRequested {
			res.URL = "https://" + res.Host
		}
	}
	res.Summary = publishSummary(*res)
}

// publishSummary turns the verdict into one plain-English line: what happened, and what happens
// next when the app is not live. A publish that did not finish never reads as one that did.
func publishSummary(r PublishResult) string {
	if r.Reachable {
		return fmt.Sprintf("%s is live at %s.", r.App, r.URL)
	}
	return fmt.Sprintf("%s is published at %s but not live yet — waiting on %s. %s", r.App, r.Host, r.BlockedOn, r.Next)
}
