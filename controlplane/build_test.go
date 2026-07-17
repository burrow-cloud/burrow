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

// newBuildEngine wires an engine with a fake Builder in addition to the standard fakes, so the
// in-cluster build orchestration can be exercised end to end against the guarded deploy path.
func newBuildEngine(t *testing.T, policy cp.Policy) (*cp.Engine, *fake.Kubernetes, *fake.Database, *fake.Builder) {
	t.Helper()
	k := fake.NewKubernetes()
	d := fake.NewDatabase()
	d.SetPolicy(policy)
	b := fake.NewBuilder()
	e, err := cp.New(cp.Deps{
		Kubernetes: k, Database: d, Clock: fake.NewClock(time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)),
		IDs: fake.NewIDs(), Resolver: fake.NewResolver(), Credentials: fake.NewCredentials(),
		DNS: fake.NewDNSFactory(), Builder: b,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e, k, d, b
}

// newBuildEngineWithRegistry wires a build engine that also carries a default in-cluster registry
// (ADR-0053 §5), so a build with no explicit target defaults its push target to the local registry.
func newBuildEngineWithRegistry(t *testing.T, registry string) (*cp.Engine, *fake.Kubernetes, *fake.Builder) {
	t.Helper()
	k := fake.NewKubernetes()
	d := fake.NewDatabase()
	d.SetPolicy(permissive())
	b := fake.NewBuilder()
	e, err := cp.New(cp.Deps{
		Kubernetes: k, Database: d, Clock: fake.NewClock(time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)),
		IDs: fake.NewIDs(), Resolver: fake.NewResolver(), Credentials: fake.NewCredentials(),
		DNS: fake.NewDNSFactory(), Builder: b, BuildRegistry: registry,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e, k, b
}

// TestBuildDefaultsTargetToInClusterRegistry asserts that a build with no explicit target defaults
// its push target to the configured in-cluster registry — the zero-config default push target
// (ADR-0053 §5). The builder is called with the composed reference, and the resulting deploy pins the
// exact bytes by digest.
func TestBuildDefaultsTargetToInClusterRegistry(t *testing.T) {
	const registry = "burrow-registry.burrow.svc.cluster.local:5000"
	e, k, b := newBuildEngineWithRegistry(t, registry)
	b.SetDigest("sha256:def456")

	res, err := e.Build(context.Background(), cp.BuildRequest{
		App:    "web",
		Source: cp.SourceRef{Repo: "https://github.com/acme/web", Ref: "v1.0.0"},
		// No TargetImage: the in-cluster registry is the default.
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	wantTarget := registry + "/web:build"
	if got := b.LastTarget(); got != wantTarget {
		t.Errorf("builder target = %q, want the in-cluster default %q", got, wantTarget)
	}
	wantImage := wantTarget + "@sha256:def456"
	if res.Deploy.Release.Image != wantImage {
		t.Errorf("deployed image = %q, want %q", res.Deploy.Release.Image, wantImage)
	}
	spec, ok := k.Spec("web")
	if !ok || spec.Image != wantImage {
		t.Errorf("applied workload image = %q (ok=%v), want %q", spec.Image, ok, wantImage)
	}
}

// newBuildEngineWithPublicRegistry wires a build engine that carries BOTH an internal push endpoint
// and a distinct public pull host (ADR-0054 §5), so a default build pushes to the internal Service but
// the resulting deploy references the public host the node pulls through the ingress.
func newBuildEngineWithPublicRegistry(t *testing.T, internal, public string) (*cp.Engine, *fake.Kubernetes, *fake.Builder) {
	t.Helper()
	k := fake.NewKubernetes()
	d := fake.NewDatabase()
	d.SetPolicy(permissive())
	b := fake.NewBuilder()
	e, err := cp.New(cp.Deps{
		Kubernetes: k, Database: d, Clock: fake.NewClock(time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)),
		IDs: fake.NewIDs(), Resolver: fake.NewResolver(), Credentials: fake.NewCredentials(),
		DNS: fake.NewDNSFactory(), Builder: b, BuildRegistry: internal, BuildPublicRegistry: public,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e, k, b
}

// TestBuildPushesInternalDeploysPublic asserts the crux of ADR-0054 §5: a default in-cluster build
// PUSHES to the internal Service endpoint but the resulting deploy REFERENCES the public host, sharing
// the same repository path and digest so the internally pushed image and the publicly pulled reference
// resolve to the same stored image. The node pulls the public host through the ingress.
func TestBuildPushesInternalDeploysPublic(t *testing.T) {
	const internal = "burrow-registry.burrow.svc.cluster.local:5000"
	const public = "registry.example.com"
	e, k, b := newBuildEngineWithPublicRegistry(t, internal, public)
	b.SetDigest("sha256:beef")

	res, err := e.Build(context.Background(), cp.BuildRequest{
		App:    "web",
		Source: cp.SourceRef{Repo: "https://github.com/acme/web", Ref: "v1.0.0"},
		// No TargetImage: the in-cluster registry is the default push target.
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// The builder pushed to the INTERNAL endpoint, marked insecure (the in-cluster registry is
	// plain HTTP, ADR-0054 §5).
	if got, want := b.LastTarget(), internal+"/web:build"; got != want {
		t.Errorf("builder push target = %q, want the internal endpoint %q", got, want)
	}
	if !b.LastInsecure() {
		t.Error("a default in-cluster build must push insecure (the in-cluster registry serves plain HTTP)")
	}
	// The deploy references the PUBLIC host at the SAME repository path and digest.
	wantImage := public + "/web:build@sha256:beef"
	if res.Deploy.Release.Image != wantImage {
		t.Errorf("deployed image = %q, want the public reference %q", res.Deploy.Release.Image, wantImage)
	}
	spec, ok := k.Spec("web")
	if !ok || spec.Image != wantImage {
		t.Errorf("applied workload image = %q (ok=%v), want %q", spec.Image, ok, wantImage)
	}
}

// TestBuildExplicitTargetOverridesRegistry asserts a caller-supplied target is used verbatim even
// when an in-cluster registry is configured — external registries stay fully supported (ADR-0053 §5).
func TestBuildExplicitTargetOverridesRegistry(t *testing.T) {
	e, _, b := newBuildEngineWithRegistry(t, "burrow-registry.burrow.svc.cluster.local:5000")
	b.SetDigest("sha256:abc")

	if _, err := e.Build(context.Background(), cp.BuildRequest{
		App:         "web",
		Source:      cp.SourceRef{Repo: "https://github.com/acme/web", Ref: "v1.0.0"},
		TargetImage: "ghcr.io/acme/web:1.0.0",
	}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := b.LastTarget(); got != "ghcr.io/acme/web:1.0.0" {
		t.Errorf("builder target = %q, want the caller's external target verbatim", got)
	}
	// An explicit external target is pushed over TLS, never marked insecure.
	if b.LastInsecure() {
		t.Error("an explicit external target must be pushed over TLS, not insecure")
	}
}

// TestBuildEmptyTargetWithoutRegistryErrors asserts that with no in-cluster registry configured, a
// build with no explicit target is a clean validation error — there is nowhere to push (ADR-0053 §5).
func TestBuildEmptyTargetWithoutRegistryErrors(t *testing.T) {
	e, _, _, b := newBuildEngine(t, permissive())
	b.SetDigest("sha256:abc")
	_, err := e.Build(context.Background(), cp.BuildRequest{
		App:    "web",
		Source: cp.SourceRef{Repo: "https://github.com/acme/web", Ref: "v1.0.0"},
	})
	if !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("empty target with no registry: err = %v, want ErrInvalid", err)
	}
}

// TestBuildSuccessFeedsGuardedDeploy asserts a successful build hands the digest-pinned reference of
// the image it produced into the existing guarded deploy path: a release is recorded and the workload
// is applied with that exact reference, and the builder is called with the source ref and target image.
func TestBuildSuccessFeedsGuardedDeploy(t *testing.T) {
	e, k, _, b := newBuildEngine(t, permissive())
	b.SetDigest("sha256:abc123")

	req := cp.BuildRequest{
		App:         "api",
		Source:      cp.SourceRef{Repo: "https://github.com/acme/api", Ref: "v1.2.3"},
		TargetImage: "ghcr.io/acme/api:1.2.3",
	}
	res, err := e.Build(context.Background(), req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	wantImage := "ghcr.io/acme/api:1.2.3@sha256:abc123"
	if res.Digest != "sha256:abc123" {
		t.Errorf("result digest = %q, want %q", res.Digest, "sha256:abc123")
	}
	if res.Deploy.Release.Image != wantImage {
		t.Errorf("deployed release image = %q, want %q", res.Deploy.Release.Image, wantImage)
	}
	if res.Deploy.Release.Status != cp.ReleaseDeployed {
		t.Errorf("release status = %q, want %q", res.Deploy.Release.Status, cp.ReleaseDeployed)
	}
	// The deploy actually applied the built reference to the cluster — the build ended where deploy
	// begins (ADR-0053 §4).
	spec, ok := k.Spec("api")
	if !ok {
		t.Fatalf("no workload applied for api; the build did not reach the deploy path")
	}
	if spec.Image != wantImage {
		t.Errorf("applied workload image = %q, want %q", spec.Image, wantImage)
	}
	// The builder saw the source ref and target image — only metadata, never code (ADR-0004).
	if got := b.LastSource(); got != req.Source {
		t.Errorf("builder source = %+v, want %+v", got, req.Source)
	}
	if got := b.LastTarget(); got != req.TargetImage {
		t.Errorf("builder target = %q, want %q", got, req.TargetImage)
	}
	if b.Calls() != 1 {
		t.Errorf("builder calls = %d, want 1", b.Calls())
	}
}

// TestBuildFailureDoesNotDeploy asserts a builder error is surfaced structurally and the deploy path
// is never touched: no workload is applied and no release is recorded.
func TestBuildFailureDoesNotDeploy(t *testing.T) {
	e, k, d, b := newBuildEngine(t, permissive())
	buildErr := errors.New("clone failed: repository not found")
	b.SetError(buildErr)

	req := cp.BuildRequest{
		App:         "api",
		Source:      cp.SourceRef{Repo: "https://github.com/acme/api", Ref: "v1.2.3"},
		TargetImage: "ghcr.io/acme/api:1.2.3",
	}
	_, err := e.Build(context.Background(), req)
	if err == nil {
		t.Fatalf("Build succeeded, want error")
	}
	if !errors.Is(err, buildErr) {
		t.Errorf("error = %v, want it to wrap the builder error", err)
	}
	if b.Calls() != 1 {
		t.Errorf("builder calls = %d, want 1", b.Calls())
	}
	// The deploy path must not have been touched: no workload, no release.
	if _, ok := k.Spec("api"); ok {
		t.Errorf("a workload was applied for api despite the build failing")
	}
	if _, err := d.LatestRelease(context.Background(), "api", cp.DefaultEnvironment); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("LatestRelease err = %v, want ErrNotFound (no release should have been recorded)", err)
	}
}

// TestBuildGoesThroughDeployGuardrail asserts the build rejoins the GUARDED deploy path: with
// app.deploy set to deny, the build runs (the builder is called) but the deploy is refused, so no
// workload is applied. The guardrails are never bypassed (ADR-0053 §4).
func TestBuildGoesThroughDeployGuardrail(t *testing.T) {
	pol := permissive().With(cp.GuardrailAppDeploy, cp.DispositionDeny)
	e, k, _, b := newBuildEngine(t, pol)

	req := cp.BuildRequest{
		App:         "api",
		Source:      cp.SourceRef{Repo: "https://github.com/acme/api", Ref: "v1.2.3"},
		TargetImage: "ghcr.io/acme/api:1.2.3",
	}
	_, err := e.Build(context.Background(), req)
	if err == nil {
		t.Fatalf("Build succeeded, want a guardrail refusal")
	}
	mustGuardrail(t, err, cp.GuardrailAppDeploy)
	if b.Calls() != 1 {
		t.Errorf("builder calls = %d, want 1 (the build runs before the deploy guardrail)", b.Calls())
	}
	if _, ok := k.Spec("api"); ok {
		t.Errorf("a workload was applied despite the deploy guardrail denying it")
	}
}

// TestBuildNotConfigured asserts that with no Builder seam wired the build errors cleanly with
// ErrNotImplemented and never touches the deploy path — Burrow stays client-build-first (ADR-0053 §1).
func TestBuildNotConfigured(t *testing.T) {
	e, k, _, _ := newEngine(t, permissive()) // no Builder wired

	req := cp.BuildRequest{
		App:         "api",
		Source:      cp.SourceRef{Repo: "https://github.com/acme/api", Ref: "v1.2.3"},
		TargetImage: "ghcr.io/acme/api:1.2.3",
	}
	_, err := e.Build(context.Background(), req)
	if !errors.Is(err, cp.ErrNotImplemented) {
		t.Fatalf("Build err = %v, want ErrNotImplemented", err)
	}
	if _, ok := k.Spec("api"); ok {
		t.Errorf("a workload was applied despite no builder being wired")
	}
}

// TestBuildValidation asserts a malformed build request is rejected as ErrInvalid before the builder
// is ever called.
func TestBuildValidation(t *testing.T) {
	good := cp.BuildRequest{
		App:         "api",
		Source:      cp.SourceRef{Repo: "https://github.com/acme/api", Ref: "v1.2.3"},
		TargetImage: "ghcr.io/acme/api:1.2.3",
	}
	cases := map[string]func(cp.BuildRequest) cp.BuildRequest{
		"empty app":         func(r cp.BuildRequest) cp.BuildRequest { r.App = ""; return r },
		"bad app name":      func(r cp.BuildRequest) cp.BuildRequest { r.App = "Bad_Name"; return r },
		"empty source repo": func(r cp.BuildRequest) cp.BuildRequest { r.Source.Repo = ""; return r },
		"empty source ref":  func(r cp.BuildRequest) cp.BuildRequest { r.Source.Ref = ""; return r },
		"empty target":      func(r cp.BuildRequest) cp.BuildRequest { r.TargetImage = ""; return r },
	}
	for name, mangle := range cases {
		t.Run(name, func(t *testing.T) {
			e, _, _, b := newBuildEngine(t, permissive())
			_, err := e.Build(context.Background(), mangle(good))
			if !errors.Is(err, cp.ErrInvalid) {
				t.Fatalf("Build err = %v, want ErrInvalid", err)
			}
			if b.Calls() != 0 {
				t.Errorf("builder calls = %d, want 0 (validation must precede the builder)", b.Calls())
			}
		})
	}
}

// newBuildEngineWithCredentials wires a build engine returning handles to the fake credential store
// and database, so a test can register a source provider and seed its token to exercise the
// source-provider credential resolution (ADR-0057).
func newBuildEngineWithCredentials(t *testing.T) (*cp.Engine, *fake.Database, *fake.Credentials, *fake.Builder) {
	t.Helper()
	k := fake.NewKubernetes()
	d := fake.NewDatabase()
	d.SetPolicy(permissive())
	creds := fake.NewCredentials()
	b := fake.NewBuilder()
	e, err := cp.New(cp.Deps{
		Kubernetes: k, Database: d, Clock: fake.NewClock(time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)),
		IDs: fake.NewIDs(), Resolver: fake.NewResolver(), Credentials: creds,
		DNS: fake.NewDNSFactory(), Builder: b,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e, d, creds, b
}

// TestBuildResolvesConfiguredSourceCredential asserts that when a source provider is configured for
// the clone URL's host, the engine reads its token from the guarded credential store and hands it to
// the builder — the private-source path (ADR-0057). The token is resolved server-side; only metadata
// (the ref and target) otherwise crosses into the builder.
func TestBuildResolvesConfiguredSourceCredential(t *testing.T) {
	const token = "ghp_private_source_token"
	e, d, creds, b := newBuildEngineWithCredentials(t)

	// Register a github source provider and seed its token exactly as `provider add` would (ADR-0030).
	if err := d.SaveProvider(context.Background(), cp.Provider{
		Name: "github", Type: cp.ProviderGitHub, Capabilities: cp.ProviderGitHub.Capabilities(), SecretKey: "github",
	}); err != nil {
		t.Fatalf("SaveProvider: %v", err)
	}
	creds.Set("github", token)

	res, err := e.Build(context.Background(), cp.BuildRequest{
		App:         "web",
		Source:      cp.SourceRef{Repo: "https://github.com/acme/private-web", Ref: "v1.0.0"},
		TargetImage: "ghcr.io/acme/web:1.0.0",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	got := b.LastCredential()
	if got.Provider != cp.ProviderGitHub || got.Token != token {
		t.Errorf("builder credential = {%q, %q}, want {github, %q}", got.Provider, got.Token, token)
	}

	// The token is a secret: it must never surface in the structured result. BuildResult carries only
	// the digest and the deploy record — never the credential.
	if res.Deploy.Release.Image == "" {
		t.Fatal("expected a deployed image")
	}
	if strings.Contains(res.Deploy.Release.Image, token) {
		t.Errorf("deployed image %q contains the source token", res.Deploy.Release.Image)
	}
}

// TestBuildPublicSourceHasNoCredential asserts the credential-free path is unchanged: with no source
// provider configured, a build resolves the ZERO credential and clones anonymously (ADR-0057) — the
// pre-existing public-source behavior (issue #279).
func TestBuildPublicSourceHasNoCredential(t *testing.T) {
	e, _, _, b := newBuildEngineWithCredentials(t)

	if _, err := e.Build(context.Background(), cp.BuildRequest{
		App:         "web",
		Source:      cp.SourceRef{Repo: "https://github.com/acme/public-web", Ref: "v1.0.0"},
		TargetImage: "ghcr.io/acme/web:1.0.0",
	}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := b.LastCredential(); !got.IsZero() {
		t.Errorf("builder credential = %+v, want the zero credential for an unconfigured (public) source", got)
	}
}

// TestBuildNonProviderHostHasNoCredential asserts a clone URL on a host Burrow does not front (a
// self-hosted git) resolves to no credential even when a github provider is configured — the token
// only authenticates its own provider's host (ADR-0057 §1).
func TestBuildNonProviderHostHasNoCredential(t *testing.T) {
	e, d, creds, b := newBuildEngineWithCredentials(t)
	if err := d.SaveProvider(context.Background(), cp.Provider{
		Name: "github", Type: cp.ProviderGitHub, Capabilities: cp.ProviderGitHub.Capabilities(), SecretKey: "github",
	}); err != nil {
		t.Fatalf("SaveProvider: %v", err)
	}
	creds.Set("github", "ghp_token")

	if _, err := e.Build(context.Background(), cp.BuildRequest{
		App:         "web",
		Source:      cp.SourceRef{Repo: "https://git.acme.internal/web", Ref: "v1.0.0"},
		TargetImage: "ghcr.io/acme/web:1.0.0",
	}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := b.LastCredential(); !got.IsZero() {
		t.Errorf("builder credential = %+v, want zero for a non-provider host", got)
	}
}
