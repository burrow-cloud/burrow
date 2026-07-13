// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
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
