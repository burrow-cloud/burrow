// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

// stubPushCredentials records the target it was asked about and answers with a fixed credential or
// error, so the tests below can assert BOTH that the seam is consulted and that it is told WHICH
// image is about to be pushed — the property that lets a resolver scope what it returns to one
// registry and one repository.
type stubPushCredentials struct {
	gotTarget string
	calls     int
	cred      cp.PushCredential
	err       error
}

func (s *stubPushCredentials) PushCredential(_ context.Context, targetImage string) (cp.PushCredential, error) {
	s.calls++
	s.gotTarget = targetImage
	return s.cred, s.err
}

// newBuildEngineWithPushResolver wires an engine whose push credential comes from a resolver. The
// in-cluster registry is configured so a build with no explicit target exercises the default push
// path, where the engine — not the caller — decides what the target is.
func newBuildEngineWithPushResolver(t *testing.T, res cp.PushCredentialResolver) (*cp.Engine, *fake.Builder) {
	t.Helper()
	b := fake.NewBuilder()
	return newBuildEngineWithPushResolverAndBuilder(t, res, b), b
}

func newBuildEngineWithPushResolverAndBuilder(t *testing.T, res cp.PushCredentialResolver, builder cp.Builder) *cp.Engine {
	t.Helper()
	d := fake.NewDatabase()
	d.SetPolicy(permissive())
	e, err := cp.New(cp.Deps{
		Kubernetes: fake.NewKubernetes(), Database: d,
		Clock: fake.NewClock(time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)),
		IDs:   fake.NewIDs(), Resolver: fake.NewResolver(), Credentials: fake.NewCredentials(),
		DNS: fake.NewDNSFactory(), Builder: builder, BuildRegistry: "reg.internal:5000",
		PushCredentials: res,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

// TestBuildWithNoPushResolverPushesAnonymously is the behaviour every self-hosted install has and
// must keep: nothing wired, nothing resolved, an anonymous push to the in-cluster registry. The push
// credential is an addition for a registry that authorizes per tenant; it is not a new requirement on
// an install whose registry is inside its own cluster.
func TestBuildWithNoPushResolverPushesAnonymously(t *testing.T) {
	e, _, _, b := newBuildEngineWithCredentials(t)

	if _, err := e.Build(context.Background(), cp.BuildRequest{
		App:         "web",
		Source:      cp.SourceRef{Repo: "https://github.com/acme/web", Ref: "v1.0.0"},
		TargetImage: "reg.internal:5000/web:1.0.0",
	}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := b.LastPushCredential(); !got.IsZero() {
		t.Errorf("builder push credential = %+v, want the zero credential (anonymous push)", got)
	}
	// And it took the seam it always took. A builder that also implements the push seam must behave
	// identically when there is no credential to carry, or "wire the interface" would silently change
	// how every existing build runs.
	if b.Calls() != 1 {
		t.Errorf("builder calls = %d, want 1", b.Calls())
	}
	if b.ProgressCalls() != 0 {
		t.Errorf("the reporting seam was taken %d time(s) for a build that asked for no progress", b.ProgressCalls())
	}
}

// TestBuildAsksThePushResolverForTheTargetBeingPushed is the point of passing the target: the engine
// is the only place that knows it — it either took the caller's or defaulted to the in-cluster
// registry — so it is the only place that can tell the resolver what to scope a credential to.
func TestBuildAsksThePushResolverForTheTargetBeingPushed(t *testing.T) {
	res := &stubPushCredentials{cred: cp.PushCredential{
		Registry: "reg.internal:5000", Username: "tenant-42", Password: "s3cret",
	}}
	e, b := newBuildEngineWithPushResolver(t, res)

	if _, err := e.Build(context.Background(), cp.BuildRequest{
		App:    "web",
		Source: cp.SourceRef{Repo: "https://github.com/acme/web", Ref: "v1.0.0"},
	}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	// The default in-cluster target, composed by the engine — not the empty reference the caller sent.
	if want := "reg.internal:5000/web:build"; res.gotTarget != want {
		t.Errorf("resolver was asked about %q, want the push target %q", res.gotTarget, want)
	}
	got := b.LastPushCredential()
	if got.Registry != "reg.internal:5000" || got.Username != "tenant-42" || got.Password != "s3cret" {
		t.Errorf("builder push credential = {%q, %q, <redacted>}, want the resolver's", got.Registry, got.Username)
	}
	// The source credential is untouched by any of this: the two authenticate different parties.
	if !b.LastCredential().IsZero() {
		t.Errorf("source credential = %+v, want zero — a push credential is not a source credential", b.LastCredential())
	}
}

// TestBuildPushResolverNothingPushesAnonymously keeps the anonymous path available even to a
// deployment that HAS a resolver: a resolver with nothing for this target returns the zero credential
// and no error, and the push proceeds exactly as an unconfigured install's does.
func TestBuildPushResolverNothingPushesAnonymously(t *testing.T) {
	res := &stubPushCredentials{}
	e, b := newBuildEngineWithPushResolver(t, res)

	if _, err := e.Build(context.Background(), cp.BuildRequest{
		App:    "web",
		Source: cp.SourceRef{Repo: "https://github.com/acme/web", Ref: "v1.0.0"},
	}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.calls != 1 {
		t.Errorf("resolver calls = %d, want 1", res.calls)
	}
	if got := b.LastPushCredential(); !got.IsZero() {
		t.Errorf("builder push credential = %+v, want the zero credential", got)
	}
}

// TestBuildPushResolverErrorFailsTheBuild is the fail-closed direction, and it is not the same
// argument as the clone's. A clone that loses its credential fails at the git fetch; a PUSH that
// loses its credential may well SUCCEED, because a registry that has to accept anonymous writes
// accepts this one — storing an image under no identity at all. The build must stop instead.
func TestBuildPushResolverErrorFailsTheBuild(t *testing.T) {
	res := &stubPushCredentials{err: errors.New("minting the registry token")}
	e, b := newBuildEngineWithPushResolver(t, res)

	_, err := e.Build(context.Background(), cp.BuildRequest{
		App:    "web",
		Source: cp.SourceRef{Repo: "https://github.com/acme/web", Ref: "v1.0.0"},
	})
	if err == nil {
		t.Fatal("Build with a failing push-credential resolver: want an error, got nil")
	}
	if b.Calls() != 0 {
		t.Error("the builder ran despite the push credential resolver failing; a build must not start with a credential nobody could resolve")
	}
}

// TestBuildRefusesAPushCredentialForAnotherRegistry pins the host-match rule. The resolver is handed
// the exact target, so a credential for a different host is a resolver bug — and a silent one:
// a docker config.json is keyed by host, so the mismatched entry would simply never be presented and
// the push would go out anonymous, failing at the registry as a bare 401 that names neither the
// credential nor what it was for.
func TestBuildRefusesAPushCredentialForAnotherRegistry(t *testing.T) {
	const password = "s3cret-for-somewhere-else"
	res := &stubPushCredentials{cred: cp.PushCredential{
		Registry: "ghcr.io", Username: "tenant-42", Password: password,
	}}
	e, b := newBuildEngineWithPushResolver(t, res)

	_, err := e.Build(context.Background(), cp.BuildRequest{
		App:         "web",
		Source:      cp.SourceRef{Repo: "https://github.com/acme/web", Ref: "v1.0.0"},
		TargetImage: "reg.internal:5000/web:1.0.0",
	})
	if !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("Build with a mismatched push credential: error = %v, want ErrInvalid", err)
	}
	// The refusal names both hosts, because naming one explains nothing. It never names the password.
	if !strings.Contains(err.Error(), "ghcr.io") || !strings.Contains(err.Error(), "reg.internal:5000") {
		t.Errorf("error %q names neither the credential's registry nor the push target", err)
	}
	if strings.Contains(err.Error(), password) {
		t.Error("the error carries the push password")
	}
	if b.Calls() != 0 {
		t.Error("the builder ran with a credential for the wrong registry")
	}
}

// TestBuildAcceptsAPushCredentialWhoseHostDiffersInCase keeps the host match from being stricter than
// the thing it is matching. DNS is case-insensitive, so two spellings of one host are one host — and
// what reaches the builder is the TARGET's spelling, because the docker config.json the builder
// writes is looked up by the host in the reference being pushed, character for character.
func TestBuildAcceptsAPushCredentialWhoseHostDiffersInCase(t *testing.T) {
	res := &stubPushCredentials{cred: cp.PushCredential{
		Registry: "Reg.Internal:5000", Username: "tenant-42", Password: "s3cret",
	}}
	e, b := newBuildEngineWithPushResolver(t, res)

	if _, err := e.Build(context.Background(), cp.BuildRequest{
		App:         "web",
		Source:      cp.SourceRef{Repo: "https://github.com/acme/web", Ref: "v1.0.0"},
		TargetImage: "reg.internal:5000/web:1.0.0",
	}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	got := b.LastPushCredential()
	if got.IsZero() {
		t.Fatal("the credential was dropped over a difference in case")
	}
	if got.Registry != "reg.internal:5000" {
		t.Errorf("builder push credential registry = %q, want the push target's own spelling of the host", got.Registry)
	}
}

// TestBuildRefusesAPushCredentialWithNoRegistry catches the credential that cannot be written down at
// all: a docker config.json entry is keyed by host, so a password with no host is a secret with
// nowhere to be presented.
func TestBuildRefusesAPushCredentialWithNoRegistry(t *testing.T) {
	res := &stubPushCredentials{cred: cp.PushCredential{Username: "tenant-42", Password: "s3cret"}}
	e, b := newBuildEngineWithPushResolver(t, res)

	_, err := e.Build(context.Background(), cp.BuildRequest{
		App:    "web",
		Source: cp.SourceRef{Repo: "https://github.com/acme/web", Ref: "v1.0.0"},
	})
	if !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("Build with a hostless push credential: error = %v, want ErrInvalid", err)
	}
	if b.Calls() != 0 {
		t.Error("the builder ran with a credential naming no registry")
	}
}

// pushBlindBuilder is a Builder that never opted into carrying a push credential — the shape of any
// implementation behind the seam that predates it, including an executor supplied by something other
// than this repo.
type pushBlindBuilder struct{ calls int }

func (b *pushBlindBuilder) Build(context.Context, cp.SourceRef, string, bool, cp.SourceCredential) (string, error) {
	b.calls++
	return "sha256:beef", nil
}

// TestABuilderThatCannotCarryAPushCredentialIsRefused is the one case where the optional interface is
// not merely a granularity difference. A builder that cannot report progress still builds correctly;
// a builder handed no credential still pushes — but it pushes as NOBODY, to a registry an operator
// deliberately configured a credential for. Where that registry accepts anonymous writes the build
// would succeed and nothing would ever say the credential was dropped, so the engine refuses instead.
func TestABuilderThatCannotCarryAPushCredentialIsRefused(t *testing.T) {
	res := &stubPushCredentials{cred: cp.PushCredential{
		Registry: "reg.internal:5000", Username: "tenant-42", Password: "s3cret",
	}}
	b := &pushBlindBuilder{}
	e := newBuildEngineWithPushResolverAndBuilder(t, res, b)

	_, err := e.Build(context.Background(), cp.BuildRequest{
		App:    "web",
		Source: cp.SourceRef{Repo: "https://github.com/acme/web", Ref: "v1.0.0"},
	})
	if !errors.Is(err, cp.ErrNotImplemented) {
		t.Fatalf("Build: error = %v, want ErrNotImplemented", err)
	}
	if b.calls != 0 {
		t.Error("the build ran anonymously with a push credential the builder could not carry")
	}
}

// TestABuilderThatCannotCarryAPushCredentialStillBuilds is the other half, and the one that matters
// for every existing install: the refusal above is about a credential that EXISTS. With no resolver
// wired there is nothing to carry, and a builder that never heard of a push credential builds exactly
// as it did before.
func TestABuilderThatCannotCarryAPushCredentialStillBuilds(t *testing.T) {
	b := &pushBlindBuilder{}
	e := newBuildEngineWithPushResolverAndBuilder(t, nil, b)

	if _, err := e.Build(context.Background(), cp.BuildRequest{
		App:    "web",
		Source: cp.SourceRef{Repo: "https://github.com/acme/web", Ref: "v1.0.0"},
	}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if b.calls != 1 {
		t.Errorf("builder calls = %d, want 1", b.calls)
	}
}
