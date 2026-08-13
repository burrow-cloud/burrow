// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

// exposeApp records the exposure that makes an app "published" — the only way Burrow learns a
// container port, and therefore the only input to ADR-0076 §3's conservative default.
func exposeApp(t *testing.T, d *fake.Database, app string, port int32) {
	t.Helper()
	if err := d.RecordExposure(context.Background(), cp.Exposure{App: app, Environment: cp.DefaultEnvironment, Host: app + ".example.com", Port: port}); err != nil {
		t.Fatalf("RecordExposure: %v", err)
	}
}

// TestResolveReadinessIsConservative is ADR-0076 §3 in a table: a known port gets a TCP check, an
// unknown one gets NOTHING, and a declared path is used only because the user supplied it. The
// no-probe rows are the load-bearing ones — they are the difference between a platform that fails a
// working deploy on a guess and one that does not.
func TestResolveReadinessIsConservative(t *testing.T) {
	cases := []struct {
		name        string
		ep          cp.HealthEndpoint
		exposedPort int32
		want        cp.ReadinessCheck
	}{
		{
			name: "nothing declared and not published: no probe at all",
			want: cp.ReadinessCheck{},
		},
		{
			name:        "published, nothing declared: a TCP check on the published port",
			exposedPort: 8080,
			want:        cp.ReadinessCheck{Port: 8080},
		},
		{
			name:        "declared path, published: an HTTP check on the published port",
			ep:          cp.HealthEndpoint{Path: "/healthz"},
			exposedPort: 8080,
			want:        cp.ReadinessCheck{Port: 8080, Path: "/healthz"},
		},
		{
			name:        "declared path and port: the declared port wins over the published one",
			ep:          cp.HealthEndpoint{Path: "/healthz", Port: 9000},
			exposedPort: 8080,
			want:        cp.ReadinessCheck{Port: 9000, Path: "/healthz"},
		},
		{
			name: "declared path and port, not published: probes the declared port",
			ep:   cp.HealthEndpoint{Path: "/healthz", Port: 9000},
			want: cp.ReadinessCheck{Port: 9000, Path: "/healthz"},
		},
		{
			// Declared before the app was published. There is no port to reach it on, and Burrow
			// does not invent one: no probe, and the health surface says why.
			name: "declared path, no port anywhere: still no probe, never a guessed port",
			ep:   cp.HealthEndpoint{Path: "/healthz"},
			want: cp.ReadinessCheck{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := cp.ResolveReadiness(c.ep, c.exposedPort); got != c.want {
				t.Errorf("ResolveReadiness(%+v, %d) = %+v, want %+v", c.ep, c.exposedPort, got, c.want)
			}
		})
	}
}

// TestReadinessNeverLeavesThePod is the guard on ADR-0076 §2, the rule a future contributor will
// break while trying to help. It asserts the two things that make "check the database from
// readiness" unreachable rather than merely discouraged: the resolved check can only ever name a
// PORT and a PATH (there is nowhere to put a host, so the probe is structurally pod-local), and a
// path that looks like a URL or a host is refused at the boundary before it can be stored.
//
// The failure this prevents is not subtle. One shared Postgres backs every app in an environment
// (ADR-0031, ADR-0067 §1), so a readiness probe that touched it would fail EVERY replica of EVERY
// app at the same instant on one blip, and Kubernetes would pull them all from their Services at
// once — a degraded dependency converted into a total outage, with a recovery slower than the blip.
func TestReadinessNeverLeavesThePod(t *testing.T) {
	offPod := []string{
		"http://postgres:5432/",
		"https://db.internal/healthz",
		"//db.internal/healthz",
		"postgres://user@db/health",
		"db.internal/healthz",
		"healthz",
		"",
		"/health z",
		"/health\nz",
	}
	for _, path := range offPod {
		t.Run(path, func(t *testing.T) {
			if err := cp.ValidateHealthPath(path); err == nil {
				t.Fatalf("ValidateHealthPath(%q) accepted a path that is not a pod-local path", path)
			}
		})
	}
	// The paths a real app serves are accepted; the rule is not "reject everything".
	for _, path := range []string{"/healthz", "/health", "/_status/ready", "/api/v1/health?deep=0"} {
		if err := cp.ValidateHealthPath(path); err != nil {
			t.Errorf("ValidateHealthPath(%q) = %v, want accepted", path, err)
		}
	}
	// And whatever is declared, the resolved check carries a port and a path and nothing else —
	// there is no field in which a host could travel to the probe.
	got := cp.ResolveReadiness(cp.HealthEndpoint{Path: "/healthz", Port: 9000}, 8080)
	if got != (cp.ReadinessCheck{Port: 9000, Path: "/healthz"}) {
		t.Errorf("ResolveReadiness = %+v, want only a port and a path", got)
	}
}

// TestValidateHealthPortRejectsNonPorts covers the numeric boundary. Zero is legal and means "the
// port the app is published on".
func TestValidateHealthPortRejectsNonPorts(t *testing.T) {
	for _, p := range []int32{-1, 65536, 100000} {
		if err := cp.ValidateHealthPort(p); err == nil {
			t.Errorf("ValidateHealthPort(%d) accepted a non-port", p)
		}
	}
	for _, p := range []int32{0, 1, 8080, 65535} {
		if err := cp.ValidateHealthPort(p); err != nil {
			t.Errorf("ValidateHealthPort(%d) = %v, want accepted", p, err)
		}
	}
}

// TestDeployUnpublishedAppGetsNoProbe is the conservative default's most important case: an app
// whose port Burrow does not know is deployed EXACTLY as it was before probes existed. No scan, no
// 8080, no /healthz.
func TestDeployUnpublishedAppGetsNoProbe(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())

	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "worker", Image: "ghcr.io/u/worker:1.0.0", Replicas: 1}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	spec, ok := k.Spec("worker")
	if !ok {
		t.Fatal("no workload applied")
	}
	if spec.Readiness.Enabled() {
		t.Errorf("Readiness = %+v, want none for an app whose port Burrow does not know", spec.Readiness)
	}
}

// TestDeployPublishedAppGetsTCPProbe: a published app gets a TCP check on the port its exposure
// routes to — the port the Service already targets, so a probe that cannot connect is a probe on an
// app that could not have been serving through that exposure anyway.
func TestDeployPublishedAppGetsTCPProbe(t *testing.T) {
	ctx := context.Background()
	e, k, d, _ := newEngine(t, permissive())
	exposeApp(t, d, "web", 8080)

	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "ghcr.io/u/web:1.0.0", Replicas: 1}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	spec, _ := k.Spec("web")
	if want := (cp.ReadinessCheck{Port: 8080}); spec.Readiness != want {
		t.Errorf("Readiness = %+v, want %+v (a TCP check on the published port)", spec.Readiness, want)
	}
	if spec.Readiness.HTTP() {
		t.Error("the default probe must be a TCP check: Burrow does not guess a path")
	}
}

// TestDeployUsesDeclaredEndpoint: once the user (or their agent) declares a path, the probe becomes
// an HTTP check against it — the only readiness signal that can say the app is able to do its job.
func TestDeployUsesDeclaredEndpoint(t *testing.T) {
	ctx := context.Background()
	e, k, d, _ := newEngine(t, permissive())
	exposeApp(t, d, "web", 8080)
	if _, err := e.SetAppHealth(ctx, "web", "", "/healthz", 0); err != nil {
		t.Fatalf("SetAppHealth: %v", err)
	}

	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "ghcr.io/u/web:1.0.0", Replicas: 1}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	spec, _ := k.Spec("web")
	if want := (cp.ReadinessCheck{Port: 8080, Path: "/healthz"}); spec.Readiness != want {
		t.Errorf("Readiness = %+v, want %+v", spec.Readiness, want)
	}
}

// TestDeployHintsAtAHealthEndpoint proves ADR-0076 §5's guidance reaches the agent where it acts —
// on the deploy result — and that it carries the do-not-check-dependencies warning, which is the
// half an agent gets wrong by default.
func TestDeployHintsAtAHealthEndpoint(t *testing.T) {
	ctx := context.Background()
	e, _, d, _ := newEngine(t, permissive())
	exposeApp(t, d, "web", 8080)

	res, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "ghcr.io/u/web:1.0.0", Replicas: 1})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	hint := findHint(res.Hints, "health endpoint")
	if hint == "" {
		t.Fatalf("Hints = %v, want a health-endpoint nudge for an app that declares none", res.Hints)
	}
	for _, want := range []string{"never its database", "readiness to serve"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint %q does not warn about %q", hint, want)
		}
	}

	// Once an endpoint is declared the nudge stops: it is guidance toward a state, not a permanent
	// scold.
	if _, err := e.SetAppHealth(ctx, "web", "", "/healthz", 0); err != nil {
		t.Fatalf("SetAppHealth: %v", err)
	}
	res, err = e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "ghcr.io/u/web:1.0.1", Replicas: 1})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if h := findHint(res.Hints, "health endpoint"); h != "" {
		t.Errorf("Hints = %v, want no health nudge once an endpoint is declared", res.Hints)
	}
}

func findHint(hints []string, substr string) string {
	for _, h := range hints {
		if strings.Contains(h, substr) {
			return h
		}
	}
	return ""
}

// TestSetAppHealthRollsTheWorkload: declaring an endpoint re-applies the running workload, so the
// probe reaches the pods rather than waiting for a deploy the user did not know they needed.
func TestSetAppHealthRollsTheWorkload(t *testing.T) {
	ctx := context.Background()
	e, k, d, _ := newEngine(t, permissive())
	exposeApp(t, d, "web", 8080)
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "ghcr.io/u/web:1.0.0", Replicas: 1}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	rep, err := e.SetAppHealth(ctx, "web", "", "/healthz", 9000)
	if err != nil {
		t.Fatalf("SetAppHealth: %v", err)
	}
	if rep.Probe != "http" || rep.ProbePath != "/healthz" || rep.ProbePort != 9000 {
		t.Errorf("report = %+v, want an http probe on /healthz:9000", rep)
	}
	if rep.Liveness {
		t.Error("report claims a liveness probe; Burrow never sets one by default (ADR-0076 §1)")
	}
	if rep.AppliesOn != "" {
		t.Errorf("AppliesOn = %q, want empty: the workload was re-applied, so the probe is live", rep.AppliesOn)
	}
	spec, _ := k.Spec("web")
	if want := (cp.ReadinessCheck{Port: 9000, Path: "/healthz"}); spec.Readiness != want {
		t.Errorf("applied Readiness = %+v, want %+v", spec.Readiness, want)
	}

	// And unsetting returns the app to §3's default rather than to nothing: it is still published.
	rep, err = e.UnsetAppHealth(ctx, "web", "")
	if err != nil {
		t.Fatalf("UnsetAppHealth: %v", err)
	}
	if rep.Probe != "tcp" || rep.ProbePort != 8080 {
		t.Errorf("report after unset = %+v, want the default tcp probe on the published port", rep)
	}
	spec, _ = k.Spec("web")
	if want := (cp.ReadinessCheck{Port: 8080}); spec.Readiness != want {
		t.Errorf("applied Readiness after unset = %+v, want %+v", spec.Readiness, want)
	}
}

// TestSetAppHealthWithNoRunningReleaseIsNotAnError: an endpoint may be declared before the app has
// ever been deployed. The setting persists and the report says when it will apply.
func TestSetAppHealthWithNoRunningReleaseIsNotAnError(t *testing.T) {
	ctx := context.Background()
	e, _, d, _ := newEngine(t, permissive())
	exposeApp(t, d, "web", 8080)

	rep, err := e.SetAppHealth(ctx, "web", "", "/healthz", 0)
	if err != nil {
		t.Fatalf("SetAppHealth: %v", err)
	}
	if rep.AppliesOn == "" {
		t.Error("AppliesOn is empty, want it to say the probe lands on the next release")
	}
	got, err := e.AppHealth(ctx, "web", "")
	if err != nil {
		t.Fatalf("AppHealth: %v", err)
	}
	if got.Path != "/healthz" || got.Probe != "http" {
		t.Errorf("AppHealth = %+v, want the declared endpoint reported back", got)
	}
}

// TestSetAppHealthRejectsAnOffPodPath is §2 at the API boundary: a path that names another host is
// refused rather than stored, so it can never reach a cluster object.
func TestSetAppHealthRejectsAnOffPodPath(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newEngine(t, permissive())
	for _, path := range []string{"http://postgres:5432/", "//db/healthz", "healthz"} {
		if _, err := e.SetAppHealth(ctx, "web", "", path, 0); !errors.Is(err, cp.ErrInvalid) {
			t.Errorf("SetAppHealth(%q) err = %v, want ErrInvalid", path, err)
		}
	}
	if _, err := e.SetAppHealth(ctx, "web", "", "/healthz", 70000); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("SetAppHealth(port 70000) err = %v, want ErrInvalid", err)
	}
}

// TestAppHealthReportsTheDefaultAndTheGuidance: the read surface tells an agent what probe is in
// force, that no liveness probe is set, and — when nothing is declared — what to do about it.
func TestAppHealthReportsTheDefaultAndTheGuidance(t *testing.T) {
	ctx := context.Background()
	e, _, d, _ := newEngine(t, permissive())
	exposeApp(t, d, "web", 8080)

	rep, err := e.AppHealth(ctx, "web", "")
	if err != nil {
		t.Fatalf("AppHealth: %v", err)
	}
	if rep.Probe != "tcp" || rep.ProbePort != 8080 || rep.Source != cp.HealthSourceExposure {
		t.Errorf("report = %+v, want a tcp probe on 8080 sourced from the exposure", rep)
	}
	if rep.Liveness {
		t.Error("report claims a liveness probe")
	}
	if !strings.Contains(rep.Hint, "never its database") {
		t.Errorf("Hint = %q, want ADR-0076 §5's dependency warning", rep.Hint)
	}

	// An unpublished, undeclared app reports no probe — honestly, rather than pretending.
	rep, err = e.AppHealth(ctx, "worker", "")
	if err != nil {
		t.Fatalf("AppHealth: %v", err)
	}
	if rep.Probe != "none" || rep.Source != cp.HealthSourceNone {
		t.Errorf("report = %+v, want no probe for an unpublished app", rep)
	}
}

// TestRollbackKeepsTheCurrentProbe: the probe is current state, not a per-release snapshot, so a
// rollback restores the prior image without reinstating a probe the operator has since changed. It
// is the same rule ADR-0028 applies to env.
func TestRollbackKeepsTheCurrentProbe(t *testing.T) {
	ctx := context.Background()
	e, k, d, _ := newEngine(t, permissive())
	exposeApp(t, d, "web", 8080)
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "ghcr.io/u/web:1.0.0", Replicas: 1}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "ghcr.io/u/web:1.0.1", Replicas: 1}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if _, err := e.SetAppHealth(ctx, "web", "", "/healthz", 0); err != nil {
		t.Fatalf("SetAppHealth: %v", err)
	}

	if _, err := e.Rollback(ctx, "web", "", cp.RollbackOptions{}); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	spec, _ := k.Spec("web")
	if want := (cp.ReadinessCheck{Port: 8080, Path: "/healthz"}); spec.Readiness != want {
		t.Errorf("Readiness after rollback = %+v, want %+v (current state, not the target release's)", spec.Readiness, want)
	}
}

// TestConfigReapplyCarriesTheProbe: every path that re-renders the workload resolves the probe, so a
// config change cannot silently strip it.
func TestConfigReapplyCarriesTheProbe(t *testing.T) {
	ctx := context.Background()
	e, k, d, _ := newEngine(t, permissive())
	exposeApp(t, d, "web", 8080)
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "ghcr.io/u/web:1.0.0", Replicas: 1}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if err := e.SetConfig(ctx, "web", "", "LOG_LEVEL", "debug", false, false); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	spec, _ := k.Spec("web")
	if want := (cp.ReadinessCheck{Port: 8080}); spec.Readiness != want {
		t.Errorf("Readiness after a config reapply = %+v, want %+v", spec.Readiness, want)
	}
}

// TestExposeRollsTheProbeOntoTheRunningApp: publishing an app is one of the two things that can
// change what the probe resolves to, so it re-applies the workload rather than leaving the probe
// absent until the next release.
func TestExposeRollsTheProbeOntoTheRunningApp(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive().With(cp.GuardrailExposePublic, cp.DispositionAllow))
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "ghcr.io/u/web:1.0.0", Replicas: 1}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if spec, _ := k.Spec("web"); spec.Readiness.Enabled() {
		t.Fatalf("Readiness = %+v before publishing, want none", spec.Readiness)
	}

	if _, err := e.Expose(ctx, cp.ExposeRequest{App: "web", Host: "web.example.com", Port: 8080}); err != nil {
		t.Fatalf("Expose: %v", err)
	}
	spec, _ := k.Spec("web")
	if want := (cp.ReadinessCheck{Port: 8080}); spec.Readiness != want {
		t.Errorf("Readiness after publishing = %+v, want %+v", spec.Readiness, want)
	}

	// Unpublishing withdraws the port the probe was checking, so the probe goes with it — a
	// relaxation, which is the safe direction (§6).
	if err := e.Unexpose(ctx, "web", ""); err != nil {
		t.Fatalf("Unexpose: %v", err)
	}
	spec, _ = k.Spec("web")
	if spec.Readiness.Enabled() {
		t.Errorf("Readiness after unpublishing = %+v, want none", spec.Readiness)
	}
}

// TestDeleteAppForgetsItsHealthEndpoint: a redeployed app of the same name starts from the
// conservative default rather than inheriting the previous occupant's declared path.
func TestDeleteAppForgetsItsHealthEndpoint(t *testing.T) {
	ctx := context.Background()
	e, _, d, _ := newEngine(t, permissive().With(cp.GuardrailAppDelete, cp.DispositionAllow))
	exposeApp(t, d, "web", 8080)
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "ghcr.io/u/web:1.0.0", Replicas: 1}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if _, err := e.SetAppHealth(ctx, "web", "", "/healthz", 0); err != nil {
		t.Fatalf("SetAppHealth: %v", err)
	}
	if err := e.DeleteApp(ctx, "web", "", true); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}
	ep, err := d.HealthEndpoint(ctx, "web", cp.DefaultEnvironment)
	if err != nil {
		t.Fatalf("HealthEndpoint: %v", err)
	}
	if ep.Declared() {
		t.Errorf("health endpoint %+v survived the app's deletion", ep)
	}
}
