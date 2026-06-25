// SPDX-License-Identifier: FSL-1.1-ALv2
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

func newEngine(t *testing.T, policy cp.Policy) (*cp.Engine, *fake.Kubernetes, *fake.Registry, *fake.Database, *fake.Clock) {
	t.Helper()
	k := fake.NewKubernetes()
	r := fake.NewRegistry()
	d := fake.NewDatabase()
	d.SetPolicy(policy)
	c := fake.NewClock(time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC))
	e, err := cp.New(cp.Deps{Kubernetes: k, Registry: r, Database: d, Clock: c, IDs: fake.NewIDs()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e, k, r, d, c
}

// permissive avoids guardrail interference for tests not about guardrails.
func permissive() cp.Policy {
	p := cp.DefaultPolicy()
	p.MaxReplicas = 1000
	return p.With(cp.GuardrailScaleToZero, cp.DispositionAllow)
}

// mustGuardrail asserts err is a guardrail refusal with the given code.
func mustGuardrail(t *testing.T, err error, code cp.GuardrailCode) {
	t.Helper()
	g, ok := cp.AsGuardrail(err)
	if !ok {
		t.Fatalf("err = %v, want a GuardrailError", err)
	}
	if g.Code != code {
		t.Fatalf("guardrail code = %q, want %q", g.Code, code)
	}
}

func TestNewValidatesDeps(t *testing.T) {
	k, r, d, c, id := fake.NewKubernetes(), fake.NewRegistry(), fake.NewDatabase(), fake.NewClock(time.Now()), fake.NewIDs()
	good := cp.Deps{Kubernetes: k, Registry: r, Database: d, Clock: c, IDs: id}

	if _, err := cp.New(good); err != nil {
		t.Fatalf("valid deps: %v", err)
	}

	// Each missing seam is rejected.
	bad := good
	bad.Kubernetes = nil
	if _, err := cp.New(bad); err == nil {
		t.Errorf("missing Kubernetes should error")
	}
	bad = good
	bad.IDs = nil
	if _, err := cp.New(bad); err == nil {
		t.Errorf("missing IDs should error")
	}
	bad = good
	bad.Database = nil
	if _, err := cp.New(bad); err == nil {
		t.Errorf("missing Database should error")
	}
}

func TestDeployHappyPath(t *testing.T) {
	ctx := context.Background()
	e, k, r, d, _ := newEngine(t, permissive())
	r.Add("registry.example.com/web:1", "sha256:web1")

	res, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "registry.example.com/web:1", Replicas: 2, Env: map[string]string{"K": "V"}})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if res.Release.Status != cp.ReleaseDeployed {
		t.Errorf("release status = %q, want deployed", res.Release.Status)
	}
	if res.Release.Digest != "sha256:web1" {
		t.Errorf("digest = %q, want sha256:web1", res.Release.Digest)
	}
	if res.SupersededReleaseID != "" {
		t.Errorf("first deploy should supersede nothing, got %q", res.SupersededReleaseID)
	}

	// Applied to the cluster.
	spec, ok := k.Spec("web")
	if !ok || spec.Image != "registry.example.com/web:1" || spec.Replicas != 2 {
		t.Errorf("cluster spec = %+v ok=%v, want web:1 x2", spec, ok)
	}
	// Recorded in the database.
	saved, err := d.LatestRelease(ctx, "web")
	if err != nil || saved.Status != cp.ReleaseDeployed {
		t.Errorf("saved release = %+v err=%v", saved, err)
	}
}

func TestDeployImageNotFound(t *testing.T) {
	ctx := context.Background()
	e, k, _, d, _ := newEngine(t, permissive())

	_, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "registry.example.com/missing:1", Replicas: 1})
	if !errors.Is(err, cp.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	// Nothing applied, nothing recorded.
	if _, ok := k.Spec("web"); ok {
		t.Errorf("no deployment should exist after a failed resolve")
	}
	if all, _ := d.Releases(ctx, "web"); len(all) != 0 {
		t.Errorf("no release should be recorded, got %d", len(all))
	}
}

func TestDeployGuardrails(t *testing.T) {
	ctx := context.Background()
	e, k, r, _, _ := newEngine(t, cp.Policy{MaxReplicas: 5})
	r.Add("img:1", "sha256:1")

	_, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 6})
	mustGuardrail(t, err, cp.GuardrailReplicaCeiling)
	_, err = e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 0})
	mustGuardrail(t, err, cp.GuardrailScaleToZero)
	// A refused deploy touches nothing.
	if _, ok := k.Spec("web"); ok {
		t.Errorf("refused deploy should not apply to the cluster")
	}
}

func TestDeploySupersedesPrevious(t *testing.T) {
	ctx := context.Background()
	e, k, r, d, _ := newEngine(t, permissive())
	r.Add("img:1", "sha256:1")
	r.Add("img:2", "sha256:2")

	v1, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1})
	if err != nil {
		t.Fatalf("deploy v1: %v", err)
	}
	v2, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:2", Replicas: 1})
	if err != nil {
		t.Fatalf("deploy v2: %v", err)
	}

	if v2.Release.Supersedes != v1.Release.ID {
		t.Errorf("v2.Supersedes = %q, want %q", v2.Release.Supersedes, v1.Release.ID)
	}
	if v2.SupersededReleaseID != v1.Release.ID {
		t.Errorf("v2.SupersededReleaseID = %q, want %q", v2.SupersededReleaseID, v1.Release.ID)
	}
	// v1 now superseded, v2 running.
	old, _ := d.Release(ctx, v1.Release.ID)
	if old.Status != cp.ReleaseSuperseded {
		t.Errorf("v1 status = %q, want superseded", old.Status)
	}
	if spec, _ := k.Spec("web"); spec.Image != "img:2" {
		t.Errorf("cluster image = %q, want img:2", spec.Image)
	}
}

func TestStatus(t *testing.T) {
	ctx := context.Background()
	e, _, r, _, _ := newEngine(t, permissive())
	r.Add("img:1", "sha256:1")
	_, _ = e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 3})

	st, err := e.Status(ctx, "web")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.HasRelease || !st.Running {
		t.Fatalf("status = %+v, want hasRelease and running", st)
	}
	if st.Workload.DesiredReplicas != 3 || !st.Workload.Available {
		t.Errorf("deployment = %+v, want desired 3 available", st.Workload)
	}
	if st.Release.Image != "img:1" {
		t.Errorf("release image = %q, want img:1", st.Release.Image)
	}
}

func TestStatusUnknownApp(t *testing.T) {
	e, _, _, _, _ := newEngine(t, permissive())
	if _, err := e.Status(context.Background(), "ghost"); !errors.Is(err, cp.ErrNotFound) {
		t.Fatalf("Status(ghost) err = %v, want ErrNotFound", err)
	}
}

func TestLogs(t *testing.T) {
	ctx := context.Background()
	e, k, r, _, _ := newEngine(t, permissive())
	r.Add("img:1", "sha256:1")
	_, _ = e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1})
	k.SetLogs("web", []cp.LogLine{{Pod: "web-1", Message: "hello"}})

	lines, err := e.Logs(ctx, "web", cp.LogOptions{})
	if err != nil || len(lines) != 1 || lines[0].Message != "hello" {
		t.Fatalf("Logs = %+v, err=%v", lines, err)
	}
	if _, err := e.Logs(ctx, "ghost", cp.LogOptions{}); !errors.Is(err, cp.ErrNotFound) {
		t.Fatalf("Logs(ghost) err = %v, want ErrNotFound", err)
	}
}

func TestScale(t *testing.T) {
	ctx := context.Background()
	e, k, r, _, _ := newEngine(t, cp.Policy{MaxReplicas: 10})
	r.Add("img:1", "sha256:1")
	_, _ = e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 2})

	res, err := e.Scale(ctx, "web", 5, false)
	if err != nil {
		t.Fatalf("Scale: %v", err)
	}
	if res.PreviousReplicas != 2 || res.Replicas != 5 {
		t.Errorf("scale result = %+v, want prev 2 new 5", res)
	}
	if st, _ := k.WorkloadStatus(ctx, "web"); st.DesiredReplicas != 5 {
		t.Errorf("cluster desired = %d, want 5", st.DesiredReplicas)
	}

	// Guardrails apply to scale too.
	_, err = e.Scale(ctx, "web", 0, false)
	mustGuardrail(t, err, cp.GuardrailScaleToZero)
	_, err = e.Scale(ctx, "web", 99, false)
	mustGuardrail(t, err, cp.GuardrailReplicaCeiling)
	// Unknown app.
	if _, err := e.Scale(ctx, "ghost", 3, false); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("scale ghost err = %v, want ErrNotFound", err)
	}
}

// TestPolicyReadLive proves the engine reads the guardrail policy from the database on each
// operation, so a `guard set` takes effect without a restart (ADR-0020).
func TestPolicyReadLive(t *testing.T) {
	ctx := context.Background()
	e, _, r, d, _ := newEngine(t, permissive())
	r.Add("img:1", "sha256:1")
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 2}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	// Tighten the policy at runtime; the next operation must observe it.
	d.SetPolicy(cp.Policy{MaxReplicas: 1})
	_, err := e.Scale(ctx, "web", 5, false)
	mustGuardrail(t, err, cp.GuardrailReplicaCeiling)
}

func TestRollback(t *testing.T) {
	ctx := context.Background()
	e, k, r, d, _ := newEngine(t, permissive())
	r.Add("img:1", "sha256:1")
	r.Add("img:2", "sha256:2")
	v1, _ := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1})
	v2, _ := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:2", Replicas: 1})

	res, err := e.Rollback(ctx, "web")
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if res.RolledBackToReleaseID != v1.Release.ID {
		t.Errorf("rolled back to %q, want %q", res.RolledBackToReleaseID, v1.Release.ID)
	}
	if res.SupersededReleaseID != v2.Release.ID {
		t.Errorf("superseded %q, want %q", res.SupersededReleaseID, v2.Release.ID)
	}
	if res.Release.Image != "img:1" {
		t.Errorf("rollback release image = %q, want img:1 (the prior reference)", res.Release.Image)
	}
	// Cluster restored to img:1.
	if spec, _ := k.Spec("web"); spec.Image != "img:1" {
		t.Errorf("cluster image = %q, want img:1", spec.Image)
	}
	// v2 now superseded.
	old, _ := d.Release(ctx, v2.Release.ID)
	if old.Status != cp.ReleaseSuperseded {
		t.Errorf("v2 status = %q, want superseded", old.Status)
	}
}

func TestRollbackNothingToRollBack(t *testing.T) {
	ctx := context.Background()
	e, _, r, _, _ := newEngine(t, permissive())

	// No releases at all.
	if _, err := e.Rollback(ctx, "web"); !errors.Is(err, cp.ErrNotFound) {
		t.Fatalf("rollback with no releases err = %v, want ErrNotFound", err)
	}
	// A single deploy has no prior to roll back to.
	r.Add("img:1", "sha256:1")
	_, _ = e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1})
	if _, err := e.Rollback(ctx, "web"); !errors.Is(err, cp.ErrNotFound) {
		t.Fatalf("rollback with one release err = %v, want ErrNotFound", err)
	}
}
