// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

// publishFixture is a cluster a publish can run against: an app deployed, an ingress controller and
// cert-manager present, and an external address assigned. Each seam a publish reads is returned so a
// test can decide what the outside world says — which host resolves, what answers on port 80, and
// whether the certificate ever issues.
type publishFixture struct {
	engine   *cp.Engine
	k        *fake.Kubernetes
	db       *fake.Database
	dns      *fake.DNSFactory
	resolver *fake.Resolver
	auth     *fake.Resolver
	creds    *fake.Credentials
	probe    *orderProbe
}

// orderProbe answers the pre-flight for the hosts it was told about, and records the state of the
// app's Ingress at the moment it was asked. That snapshot is what makes the ORDER testable: the
// certificate must not have been requested yet when the pre-flight runs, or a publish against a
// host that cannot answer the challenge would have already opened an ACME order against the
// account's rate limit.
type orderProbe struct {
	k       *fake.Kubernetes
	app     string
	answers map[string]bool
	probed  []string
	// tlsWhenProbed is whether the Ingress carried a certificate request at the first probe.
	tlsWhenProbed bool
	seen          bool
}

func (p *orderProbe) ProbeHTTP(_ context.Context, raw string) (int, error) {
	if !p.seen {
		exp, _ := p.k.Exposure(p.app)
		p.tlsWhenProbed = exp.TLS
		p.seen = true
	}
	p.probed = append(p.probed, raw)
	u, err := url.Parse(raw)
	if err != nil {
		return 0, err
	}
	if !p.answers[u.Hostname()] {
		return 0, errors.New("nothing answered")
	}
	return 404, nil // the ingress reached the app, which knows nothing of the challenge path
}

// newPublishFixture builds the engine and deploys the app. policy decides the guardrails; pass
// permissivePublish for the tests that are not about them.
func newPublishFixture(t *testing.T, policy cp.Policy) *publishFixture {
	t.Helper()
	k := fake.NewKubernetes()
	d := fake.NewDatabase()
	d.SetPolicy(policy)
	dnsFactory := fake.NewDNSFactory()
	resolver := fake.NewResolver()
	auth := fake.NewResolver()
	probe := &orderProbe{k: k, app: "web", answers: map[string]bool{}}
	creds := fake.NewCredentials()
	e, err := cp.New(cp.Deps{
		Kubernetes: k, Database: d,
		Clock: fake.NewClock(time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)),
		IDs:   fake.NewIDs(), Resolver: resolver, AuthoritativeResolver: auth,
		Credentials: creds, DNS: dnsFactory, HTTPProbe: probe,
		ClusterProber: fake.NewClusterProber(cp.ClusterCapabilities{
			Ingress:     cp.IngressCapability{Present: true, Classes: []string{"nginx"}},
			CertManager: cp.CertManagerCapability{Present: true},
		}),
		// Every wait returns at once, so a publish's polling is exercised in full without any
		// real time passing (ADR-0010: no ambient clock in core logic).
		After: func(time.Duration) <-chan time.Time {
			ch := make(chan time.Time, 1)
			ch <- time.Time{}
			return ch
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := e.Deploy(context.Background(), cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	k.SetIngressAddress("web", "203.0.113.10")
	return &publishFixture{engine: e, k: k, db: d, dns: dnsFactory, resolver: resolver, auth: auth, creds: creds, probe: probe}
}

// dnsProvider seeds a configured DigitalOcean DNS provider and the token burrowd reads to reach it.
func (f *publishFixture) dnsProvider(t *testing.T) {
	t.Helper()
	dnsProvider(t, f.db)
	f.creds.Set("do-dns", "token")
}

// permissivePublish allows the two guardrails a publish trips, for the tests about the chain rather
// than about the gates.
func permissivePublish() cp.Policy {
	return cp.DefaultPolicy().
		With(cp.GuardrailExposePublic, cp.DispositionAllow).
		With(cp.GuardrailDNSWrite, cp.DispositionAllow)
}

// pointed makes host resolve to the cluster's address at both resolvers, standing in for a DNS
// record that has taken effect.
func (f *publishFixture) pointed(host string) {
	f.resolver.Set(host, "203.0.113.10")
	f.auth.Set(host, "203.0.113.10")
}

// answering makes the cluster answer a plain-HTTP request for host.
func (f *publishFixture) answering(host string) { f.probe.answers[host] = true }

// TestPublishIsTheWholeChain asserts one publish leaves the app live over HTTPS: routed, DNS record
// written at the configured provider, certificate attached and issued, and a result that says so
// (ADR-0041 §3). It also asserts the ORDER — the pre-flight walked the ACME challenge path while
// the Ingress still carried no certificate request.
func TestPublishIsTheWholeChain(t *testing.T) {
	ctx := context.Background()
	f := newPublishFixture(t, permissivePublish())
	f.dnsProvider(t)
	f.pointed("web.example.com")
	f.answering("web.example.com")
	f.k.SetCertReady("web", true)

	res, err := f.engine.Publish(ctx, cp.PublishRequest{App: "web", Host: "web.example.com", Port: 8080})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if !res.Reachable || res.URL != "https://web.example.com" {
		t.Fatalf("result = %+v, want reachable at https://web.example.com", res)
	}
	if !res.TLSAttached || !res.CertReady || !res.HTTPVerified || !res.DNSPointsAtCluster {
		t.Errorf("chain = %+v, want every link green", res)
	}
	if res.DNSProvider != "do-dns" || res.DNSRecordType != "A" {
		t.Errorf("dns = %q %q, want an A record at do-dns", res.DNSProvider, res.DNSRecordType)
	}
	if rec, ok := f.dns.Provider().Record("web.example.com"); !ok || rec.Value != "203.0.113.10" {
		t.Errorf("written record = %+v ok=%v, want it pointing at the cluster", rec, ok)
	}
	if exp, ok := f.k.Exposure("web"); !ok || !exp.TLS {
		t.Errorf("exposure = %+v ok=%v, want TLS attached at the end", exp, ok)
	}
	if f.probe.tlsWhenProbed {
		t.Error("the certificate was requested BEFORE the pre-flight ran — an unreachable host would open an ACME order it cannot complete")
	}
	if len(f.probe.probed) == 0 || !strings.Contains(f.probe.probed[0], "/.well-known/acme-challenge/") {
		t.Errorf("probed = %v, want the ACME challenge path", f.probe.probed)
	}
	if !strings.Contains(res.Summary, "live at https://web.example.com") {
		t.Errorf("summary = %q", res.Summary)
	}
}

// TestPublishRequestsNoCertificateWhenPort80DoesNotAnswer is the rate-limit rule: DNS points at the
// cluster but nothing answers over plain HTTP, so the Ingress is left without its cert-manager
// annotation and no order is ever opened. The result says which link it is waiting on.
func TestPublishRequestsNoCertificateWhenPort80DoesNotAnswer(t *testing.T) {
	ctx := context.Background()
	f := newPublishFixture(t, permissivePublish())
	f.dnsProvider(t)
	f.pointed("web.example.com") // resolves, but nothing is seeded to answer the probe

	res, err := f.engine.Publish(ctx, cp.PublishRequest{App: "web", Host: "web.example.com", Port: 8080})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if res.Reachable || res.TLSAttached || res.HTTPVerified {
		t.Fatalf("result = %+v, want no certificate requested and not reachable", res)
	}
	if res.BlockedOn != "http path" {
		t.Errorf("blocked_on = %q, want %q", res.BlockedOn, "http path")
	}
	if exp, ok := f.k.Exposure("web"); !ok || exp.TLS {
		t.Errorf("exposure = %+v, want the Ingress still carrying no certificate request", exp)
	}
	if res.URL != "" {
		t.Errorf("url = %q, want none — the app is not live", res.URL)
	}
}

// TestPublishRequestsNoCertificateWhenDNSDoesNotPointAtTheCluster asserts the pre-flight's first
// half gates the second: with the host unresolved, the plain-HTTP probe is not even attempted, and
// no certificate is requested.
func TestPublishRequestsNoCertificateWhenDNSDoesNotPointAtTheCluster(t *testing.T) {
	ctx := context.Background()
	f := newPublishFixture(t, permissivePublish())
	f.dnsProvider(t)
	f.answering("web.example.com") // the cluster would answer, but nothing resolves to it

	res, err := f.engine.Publish(ctx, cp.PublishRequest{App: "web", Host: "web.example.com", Port: 8080})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if res.DNSPointsAtCluster || res.TLSAttached {
		t.Fatalf("result = %+v, want no certificate requested", res)
	}
	if res.BlockedOn != "dns" {
		t.Errorf("blocked_on = %q, want %q", res.BlockedOn, "dns")
	}
	if len(f.probe.probed) != 0 {
		t.Errorf("probed %v, want no probe before DNS resolves", f.probe.probed)
	}
	if !strings.Contains(res.Next, "do-dns") {
		t.Errorf("next = %q, want it to name the provider the record was written at", res.Next)
	}
}

// TestPublishWithoutADNSProviderNamesTheAddress covers the self-hoster who points DNS by hand: no
// provider is configured, so Burrow writes no record, and the verdict hands back the address to
// point at rather than failing.
func TestPublishWithoutADNSProviderNamesTheAddress(t *testing.T) {
	ctx := context.Background()
	f := newPublishFixture(t, permissivePublish())

	res, err := f.engine.Publish(ctx, cp.PublishRequest{App: "web", Host: "web.example.com", Port: 8080})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if res.DNSProvider != "" || res.BlockedOn != "dns" {
		t.Fatalf("result = %+v, want no record written and dns as the blocked link", res)
	}
	if !strings.Contains(res.Next, "203.0.113.10") {
		t.Errorf("next = %q, want the address to point the host at", res.Next)
	}

	// The same publish, run again once the human has pointed the record themselves, finishes.
	f.pointed("web.example.com")
	f.answering("web.example.com")
	f.k.SetCertReady("web", true)
	res, err = f.engine.Publish(ctx, cp.PublishRequest{App: "web", Host: "web.example.com", Port: 8080})
	if err != nil {
		t.Fatalf("second publish: %v", err)
	}
	if !res.Reachable || res.URL != "https://web.example.com" {
		t.Errorf("result = %+v, want reachable over HTTPS", res)
	}
}

// TestPublishRefusesPlainHTTPOnAnHSTSDomain is the honesty rule at its sharpest: `.dev` is
// HSTS-preloaded, so an http:// URL there cannot be opened by any browser. A publish that would
// produce one is refused before anything is created, rather than reported as executed (issue #476).
func TestPublishRefusesPlainHTTPOnAnHSTSDomain(t *testing.T) {
	ctx := context.Background()
	f := newPublishFixture(t, permissivePublish())

	_, err := f.engine.Publish(ctx, cp.PublishRequest{App: "web", Host: "console.burrow-cloud.dev", Port: 8080, NoTLS: true})
	if !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("publish = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "HSTS") {
		t.Errorf("error = %q, want it to say why plain HTTP cannot work there", err)
	}
	if _, ok := f.k.Exposure("web"); ok {
		t.Error("the app was exposed anyway; a refused publish must create nothing")
	}
}

// TestPublishPlainHTTPOnAnOrdinaryDomain confirms the refusal above is scoped to the domains that
// mandate HTTPS: elsewhere, --tls=false is a supported publish that ends live over http://. It also
// pins the probe as part of the plain-HTTP verdict — "live" means the cluster answered for the host,
// not merely that the host resolves to it.
func TestPublishPlainHTTPOnAnOrdinaryDomain(t *testing.T) {
	ctx := context.Background()
	f := newPublishFixture(t, permissivePublish())
	f.pointed("web.example.com")

	// Nothing answers yet: the publish is honest about it rather than handing back a URL.
	res, err := f.engine.Publish(ctx, cp.PublishRequest{App: "web", Host: "web.example.com", Port: 8080, NoTLS: true})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if res.Reachable || res.BlockedOn != "http path" {
		t.Fatalf("result = %+v, want not reachable, blocked on the http path", res)
	}

	f.answering("web.example.com")
	res, err = f.engine.Publish(ctx, cp.PublishRequest{App: "web", Host: "web.example.com", Port: 8080, NoTLS: true})
	if err != nil {
		t.Fatalf("second publish: %v", err)
	}
	if !res.Reachable || res.URL != "http://web.example.com" {
		t.Fatalf("result = %+v, want reachable at http://web.example.com", res)
	}
	if res.TLSRequested || res.TLSAttached {
		t.Errorf("result = %+v, want no certificate requested", res)
	}
}

// TestPublishCertificatePendingIsNotSuccess asserts a publish whose certificate has not issued
// reports the app as not live, with the link named — the case an agent must not relay as done.
func TestPublishCertificatePendingIsNotSuccess(t *testing.T) {
	ctx := context.Background()
	f := newPublishFixture(t, permissivePublish())
	f.dnsProvider(t)
	f.pointed("web.example.com")
	f.answering("web.example.com")
	// cert-manager never reports the certificate issued.

	res, err := f.engine.Publish(ctx, cp.PublishRequest{App: "web", Host: "web.example.com", Port: 8080})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if res.Reachable || res.URL != "" {
		t.Fatalf("result = %+v, want not reachable and no URL", res)
	}
	if !res.TLSAttached || res.CertReady {
		t.Errorf("result = %+v, want the order open and the certificate not issued", res)
	}
	if res.BlockedOn != "tls certificate" {
		t.Errorf("blocked_on = %q, want %q", res.BlockedOn, "tls certificate")
	}
	if strings.Contains(res.Summary, "live at") {
		t.Errorf("summary = %q, want it not to read as a success", res.Summary)
	}
}

// TestPublishHonoursEachLinksGuardrail asserts publish evaluates none of its own: the exposure is
// held by app.expose_public and, past that, the record is held by dns.write — each surfacing as its
// own code so an agent knows which confirmation it is relaying (ADR-0041 §3, ADR-0006).
func TestPublishHonoursEachLinksGuardrail(t *testing.T) {
	ctx := context.Background()
	f := newPublishFixture(t, cp.DefaultPolicy())
	f.dnsProvider(t)
	f.pointed("web.example.com")
	f.answering("web.example.com")
	f.k.SetCertReady("web", true)

	// Unconfirmed, the first link holds and nothing is created.
	_, err := f.engine.Publish(ctx, cp.PublishRequest{App: "web", Host: "web.example.com", Port: 8080})
	if g, ok := cp.AsGuardrail(err); !ok || g.Code != cp.GuardrailExposePublic || !g.NeedsConfirmation {
		t.Fatalf("publish = %v, want app.expose_public held", err)
	}
	if _, ok := f.k.Exposure("web"); ok {
		t.Fatal("the app was exposed by a held publish")
	}

	// With the DNS write denied outright, the publish stops at that link and says so — with the
	// routing it had already applied left in place.
	f.db.SetPolicy(cp.DefaultPolicy().
		With(cp.GuardrailExposePublic, cp.DispositionAllow).
		With(cp.GuardrailDNSWrite, cp.DispositionDeny))
	_, err = f.engine.Publish(ctx, cp.PublishRequest{App: "web", Host: "web.example.com", Port: 8080})
	if g, ok := cp.AsGuardrail(err); !ok || g.Code != cp.GuardrailDNSWrite {
		t.Fatalf("publish = %v, want dns.write denied", err)
	}
	if exp, ok := f.k.Exposure("web"); !ok || exp.TLS {
		t.Errorf("exposure = %+v ok=%v, want the plain-HTTP routing applied and no certificate requested", exp, ok)
	}
}

// TestPublishFallsBackFromTheAuthoritativeResolver asserts the pre-flight still passes when the
// zone's own nameservers will not answer: the recursive resolver's answer is the fallback, not a
// reason to hold the publish back.
func TestPublishFallsBackFromTheAuthoritativeResolver(t *testing.T) {
	ctx := context.Background()
	f := newPublishFixture(t, permissivePublish())
	f.auth.SetError(errors.New("no nameserver answered"))
	f.resolver.Set("web.example.com", "203.0.113.10")
	f.answering("web.example.com")
	f.k.SetCertReady("web", true)

	res, err := f.engine.Publish(ctx, cp.PublishRequest{App: "web", Host: "web.example.com", Port: 8080})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if !res.Reachable {
		t.Fatalf("result = %+v, want reachable on the recursive resolver's answer", res)
	}
}

// TestPublishMatchesAHostnameAddress covers the cluster whose ingress address is a NAME rather than
// an IP: the record is a CNAME, so the host and the address are compared by what they resolve to.
func TestPublishMatchesAHostnameAddress(t *testing.T) {
	ctx := context.Background()
	f := newPublishFixture(t, permissivePublish())
	f.dnsProvider(t)
	f.k.SetIngressAddress("web", "lb-1.example-cloud.net")
	f.resolver.Set("web.example.com", "198.51.100.7")
	f.resolver.Set("lb-1.example-cloud.net", "198.51.100.7")
	f.auth.Set("web.example.com", "198.51.100.7")
	f.answering("web.example.com")
	f.k.SetCertReady("web", true)

	res, err := f.engine.Publish(ctx, cp.PublishRequest{App: "web", Host: "web.example.com", Port: 8080})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if !res.DNSPointsAtCluster || !res.Reachable {
		t.Fatalf("result = %+v, want the CNAME target recognised as this cluster", res)
	}
	if res.DNSRecordType != "CNAME" {
		t.Errorf("record type = %q, want CNAME for a hostname address", res.DNSRecordType)
	}
}

// TestPublishSkipDNSLeavesTheRecordAlone confirms a host whose DNS is managed elsewhere can be
// published without Burrow touching the provider it happens to have configured.
func TestPublishSkipDNSLeavesTheRecordAlone(t *testing.T) {
	ctx := context.Background()
	f := newPublishFixture(t, permissivePublish())
	f.dnsProvider(t)
	f.pointed("web.example.com")
	f.answering("web.example.com")
	f.k.SetCertReady("web", true)

	res, err := f.engine.Publish(ctx, cp.PublishRequest{App: "web", Host: "web.example.com", Port: 8080, SkipDNS: true})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if res.DNSProvider != "" {
		t.Errorf("dns provider = %q, want none written with SkipDNS", res.DNSProvider)
	}
	if ensure, _ := f.dns.Provider().Calls(); ensure != 0 {
		t.Errorf("provider was called %d time(s), want none", ensure)
	}
	if !res.Reachable {
		t.Errorf("result = %+v, want reachable on the record that already existed", res)
	}
}

// TestPublishValidates asserts the arguments a publish cannot proceed without.
func TestPublishValidates(t *testing.T) {
	ctx := context.Background()
	f := newPublishFixture(t, permissivePublish())

	for _, tc := range []struct {
		name string
		req  cp.PublishRequest
	}{
		{"no host", cp.PublishRequest{App: "web", Port: 8080}},
		{"no port", cp.PublishRequest{App: "web", Host: "web.example.com"}},
		{"bad app", cp.PublishRequest{App: "Not A Name", Host: "web.example.com", Port: 8080}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := f.engine.Publish(ctx, tc.req); !errors.Is(err, cp.ErrInvalid) {
				t.Errorf("publish = %v, want ErrInvalid", err)
			}
		})
	}
}

// TestPublishReportsMissingPrerequisites asserts the whole intent is checked before anything is
// created: publishing with TLS onto a cluster with no ingress controller returns the structured
// checklist rather than creating an Ingress nothing will serve (ADR-0006, ADR-0041 §4).
func TestPublishReportsMissingPrerequisites(t *testing.T) {
	ctx := context.Background()
	k := fake.NewKubernetes()
	d := fake.NewDatabase()
	d.SetPolicy(permissivePublish())
	e, err := cp.New(cp.Deps{
		Kubernetes: k, Database: d, Clock: fake.NewClock(time.Now()), IDs: fake.NewIDs(),
		Resolver: fake.NewResolver(), Credentials: fake.NewCredentials(), DNS: fake.NewDNSFactory(),
		ClusterProber: fake.NewClusterProber(cp.ClusterCapabilities{}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	_, err = e.Publish(ctx, cp.PublishRequest{App: "web", Host: "web.example.com", Port: 8080})
	m, ok := cp.AsMissingPrerequisites(err)
	if !ok {
		t.Fatalf("publish = %v, want MissingPrerequisitesError", err)
	}
	names := prereqNames(m)
	for _, want := range []string{"ingress controller", "cert-manager"} {
		if !names[want] {
			t.Errorf("missing prerequisite %q not reported; got %v", want, names)
		}
	}
	if _, exposed := k.Exposure("web"); exposed {
		t.Error("the app was exposed despite the missing prerequisites")
	}
}

// TestRepublishKeepsTheCertificateItAlreadyHas asserts a second publish of a host that already has
// its certificate request does not take the cert-manager annotation off and put it back. That
// round trip is not a no-op: cert-manager owns the Certificate through the annotation, so removing
// it deletes the Certificate and re-adding it opens a FRESH ACME order — a publish run twice would
// spend the rate limit the whole ordering exists to protect.
func TestRepublishKeepsTheCertificateItAlreadyHas(t *testing.T) {
	ctx := context.Background()
	f := newPublishFixture(t, permissivePublish())
	f.dnsProvider(t)
	f.pointed("web.example.com")
	f.answering("web.example.com")
	f.k.SetCertReady("web", true)

	if _, err := f.engine.Publish(ctx, cp.PublishRequest{App: "web", Host: "web.example.com", Port: 8080}); err != nil {
		t.Fatalf("first publish: %v", err)
	}

	f.probe.seen = false // observe the second publish's pre-flight rather than the first
	res, err := f.engine.Publish(ctx, cp.PublishRequest{App: "web", Host: "web.example.com", Port: 8080})
	if err != nil {
		t.Fatalf("second publish: %v", err)
	}
	if !res.Reachable {
		t.Fatalf("result = %+v, want still reachable", res)
	}
	if !f.probe.tlsWhenProbed {
		t.Error("the second publish stripped the certificate request before its pre-flight; re-adding it opens a new ACME order")
	}
}

// TestPublishNamedProviderMustExist asserts a `--provider` nobody configured is an error naming it,
// not a silently skipped DNS record: skipping would leave the verdict telling the user to configure
// a provider they believe they already have.
func TestPublishNamedProviderMustExist(t *testing.T) {
	ctx := context.Background()
	f := newPublishFixture(t, permissivePublish())
	f.dnsProvider(t)

	_, err := f.engine.Publish(ctx, cp.PublishRequest{App: "web", Host: "web.example.com", Port: 8080, Provider: "do-dsn"})
	if !errors.Is(err, cp.ErrNotFound) && !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("publish = %v, want a refusal naming the provider", err)
	}
	if !strings.Contains(err.Error(), "do-dsn") {
		t.Errorf("error = %q, want it to name the provider that is not configured", err)
	}
}

// TestPublishAbandonedByItsCallerIsAnError asserts a cancelled publish reports the cancellation
// rather than a verdict: a chain read off a wait nobody finished would be indistinguishable from
// one that was checked and found unready.
func TestPublishAbandonedByItsCallerIsAnError(t *testing.T) {
	f := newPublishFixture(t, permissivePublish())
	f.dnsProvider(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := f.engine.Publish(ctx, cp.PublishRequest{App: "web", Host: "web.example.com", Port: 8080}); !errors.Is(err, context.Canceled) {
		t.Fatalf("publish = %v, want context.Canceled", err)
	}
}
