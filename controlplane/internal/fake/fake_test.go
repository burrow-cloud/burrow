// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package fake

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/burrow-cloud/burrow/controlplane"
)

func TestClock(t *testing.T) {
	start := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	c := NewClock(start)
	if !c.Now().Equal(start) {
		t.Fatalf("Now() = %v, want %v", c.Now(), start)
	}
	c.Advance(90 * time.Minute)
	if got, want := c.Now(), start.Add(90*time.Minute); !got.Equal(want) {
		t.Fatalf("after Advance Now() = %v, want %v", got, want)
	}
	c.Set(start)
	if !c.Now().Equal(start) {
		t.Fatalf("after Set Now() = %v, want %v", c.Now(), start)
	}
}

func TestKubernetesApplyStatusScale(t *testing.T) {
	ctx := context.Background()
	k := NewKubernetes()

	spec := controlplane.WorkloadSpec{App: "web", Image: "img:1", Replicas: 3}
	if err := k.ApplyWorkload(ctx, spec); err != nil {
		t.Fatalf("ApplyWorkload: %v", err)
	}

	st, err := k.WorkloadStatus(ctx, "web")
	if err != nil {
		t.Fatalf("WorkloadStatus: %v", err)
	}
	if st.DesiredReplicas != 3 || st.ReadyReplicas != 3 || !st.Available {
		t.Fatalf("status = %+v, want desired=3 ready=3 available", st)
	}
	if st.Image != "img:1" {
		t.Fatalf("status image = %q, want img:1", st.Image)
	}

	if err := k.ScaleWorkload(ctx, "web", 5); err != nil {
		t.Fatalf("ScaleWorkload: %v", err)
	}
	st, _ = k.WorkloadStatus(ctx, "web")
	if st.DesiredReplicas != 5 || st.ReadyReplicas != 5 {
		t.Fatalf("after scale status = %+v, want desired=5 ready=5", st)
	}

	// Partial readiness => not available.
	k.SetReady("web", 2)
	st, _ = k.WorkloadStatus(ctx, "web")
	if st.Available {
		t.Fatalf("with 2/5 ready, Available should be false")
	}

	if got, ok := k.Spec("web"); !ok || got.Replicas != 5 {
		t.Fatalf("Spec(web) = %+v ok=%v, want replicas=5", got, ok)
	}
}

func TestKubernetesNotFound(t *testing.T) {
	ctx := context.Background()
	k := NewKubernetes()
	for _, op := range []string{"status", "scale", "logs", "delete"} {
		var err error
		switch op {
		case "status":
			_, err = k.WorkloadStatus(ctx, "ghost")
		case "scale":
			err = k.ScaleWorkload(ctx, "ghost", 1)
		case "logs":
			_, err = k.Logs(ctx, "ghost", controlplane.LogOptions{})
		case "delete":
			err = k.DeleteWorkload(ctx, "ghost")
		}
		if !errors.Is(err, controlplane.ErrNotFound) {
			t.Errorf("%s on missing app: err = %v, want ErrNotFound", op, err)
		}
	}
}

func TestKubernetesLogsTail(t *testing.T) {
	ctx := context.Background()
	k := NewKubernetes()
	_ = k.ApplyWorkload(ctx, controlplane.WorkloadSpec{App: "web", Image: "img:1", Replicas: 1})
	lines := []controlplane.LogLine{
		{Pod: "web-1", Message: "a"},
		{Pod: "web-1", Message: "b"},
		{Pod: "web-1", Message: "c"},
	}
	k.SetLogs("web", lines)

	all, err := k.Logs(ctx, "web", controlplane.LogOptions{})
	if err != nil || len(all) != 3 {
		t.Fatalf("Logs all = %v (n=%d), err=%v", all, len(all), err)
	}
	tail, _ := k.Logs(ctx, "web", controlplane.LogOptions{TailLines: 2})
	if len(tail) != 2 || tail[0].Message != "b" || tail[1].Message != "c" {
		t.Fatalf("tail = %+v, want last two lines b,c", tail)
	}
}

func TestKubernetesErrorInjection(t *testing.T) {
	ctx := context.Background()
	k := NewKubernetes()
	boom := errors.New("boom")
	k.SetError(OpApply, boom)
	if err := k.ApplyWorkload(ctx, controlplane.WorkloadSpec{App: "web", Image: "img:1"}); !errors.Is(err, boom) {
		t.Fatalf("injected apply error = %v, want boom", err)
	}
	k.SetError(OpApply, nil) // clear
	if err := k.ApplyWorkload(ctx, controlplane.WorkloadSpec{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("after clearing error, ApplyWorkload = %v", err)
	}
}

func TestDatabaseSaveAndQuery(t *testing.T) {
	ctx := context.Background()
	d := NewDatabase()

	r1 := controlplane.Release{ID: "r1", App: "web", Image: "img:1", Replicas: 1, Env: map[string]string{"K": "V"}}
	r2 := controlplane.Release{ID: "r2", App: "web", Image: "img:2", Replicas: 2, Supersedes: "r1"}
	other := controlplane.Release{ID: "x1", App: "api", Image: "api:1", Replicas: 1}
	for _, r := range []controlplane.Release{r1, r2, other} {
		if err := d.SaveRelease(ctx, r); err != nil {
			t.Fatalf("SaveRelease(%s): %v", r.ID, err)
		}
	}

	got, err := d.Release(ctx, "r1")
	if err != nil || got.Image != "img:1" {
		t.Fatalf("Release(r1) = %+v, err=%v", got, err)
	}

	latest, err := d.LatestRelease(ctx, "web", "default")
	if err != nil || latest.ID != "r2" {
		t.Fatalf("LatestRelease(web) = %+v, err=%v, want r2", latest, err)
	}

	all, err := d.Releases(ctx, "web", "default")
	if err != nil || len(all) != 2 || all[0].ID != "r1" || all[1].ID != "r2" {
		t.Fatalf("Releases(web) = %+v, err=%v, want [r1 r2] oldest first", all, err)
	}

	none, err := d.Releases(ctx, "nobody", "default")
	if err != nil || len(none) != 0 {
		t.Fatalf("Releases(nobody) = %+v, err=%v, want empty", none, err)
	}
	if _, err := d.LatestRelease(ctx, "nobody", "default"); !errors.Is(err, controlplane.ErrNotFound) {
		t.Fatalf("LatestRelease(nobody) err = %v, want ErrNotFound", err)
	}
	if _, err := d.Release(ctx, "missing"); !errors.Is(err, controlplane.ErrNotFound) {
		t.Fatalf("Release(missing) err = %v, want ErrNotFound", err)
	}
}

// TestDatabaseReleasesPerEnvironment proves the fake keys releases per (app, environment): a save in
// one environment is invisible to another, a release stored with an empty Environment reads back under
// "default", and LatestRelease/Releases/ListReleases all filter by env (ADR-0052 Phase 4a).
func TestDatabaseReleasesPerEnvironment(t *testing.T) {
	ctx := context.Background()
	d := NewDatabase()

	// One release with no Environment set (reads back as "default"), one explicitly in prod.
	def := controlplane.Release{ID: "d1", App: "web", Image: "img:1", Status: controlplane.ReleaseDeployed}
	prod := controlplane.Release{ID: "p1", App: "web", Environment: "prod", Image: "img:2", Status: controlplane.ReleaseDeployed}
	for _, r := range []controlplane.Release{def, prod} {
		if err := d.SaveRelease(ctx, r); err != nil {
			t.Fatalf("SaveRelease(%s): %v", r.ID, err)
		}
	}

	// The default environment sees only the empty-Environment release, under "default".
	if got, err := d.Releases(ctx, "web", "default"); err != nil || len(got) != 1 || got[0].ID != "d1" {
		t.Fatalf("Releases(web, default) = %+v, err=%v, want [d1]", got, err)
	}
	if got, err := d.LatestRelease(ctx, "web", "default"); err != nil || got.ID != "d1" {
		t.Fatalf("LatestRelease(web, default) = %+v, err=%v, want d1", got, err)
	}
	// prod sees only its own release.
	if got, err := d.Releases(ctx, "web", "prod"); err != nil || len(got) != 1 || got[0].ID != "p1" {
		t.Fatalf("Releases(web, prod) = %+v, err=%v, want [p1]", got, err)
	}
	if got, err := d.ListReleases(ctx, "web", "prod"); err != nil || len(got) != 1 || got[0].ID != "p1" {
		t.Fatalf("ListReleases(web, prod) = %+v, err=%v, want [p1]", got, err)
	}
	// An environment with no releases: empty slice from the lists, ErrNotFound from LatestRelease.
	if got, err := d.Releases(ctx, "web", "staging"); err != nil || len(got) != 0 {
		t.Fatalf("Releases(web, staging) = %+v, err=%v, want empty", got, err)
	}
	if _, err := d.LatestRelease(ctx, "web", "staging"); !errors.Is(err, controlplane.ErrNotFound) {
		t.Fatalf("LatestRelease(web, staging) err = %v, want ErrNotFound", err)
	}
}

// TestDatabaseDisableAutoDeploy proves the fake's rollback/downgrade safety stop: DisableAutoDeploy
// sets the level to off with a reason, AutoDeployReason reads it, and SetAutoDeployLevel (the human
// re-enable) clears it — all keyed per (app, env) (ADR-0052 §5).
func TestDatabaseDisableAutoDeploy(t *testing.T) {
	ctx := context.Background()
	d := NewDatabase()

	if err := d.DisableAutoDeploy(ctx, "web", "default", "disabled by rollback"); err != nil {
		t.Fatalf("DisableAutoDeploy: %v", err)
	}
	if lvl, _ := d.AutoDeployLevel(ctx, "web", "default"); lvl != controlplane.AutoDeployOff {
		t.Fatalf("level after disable = %q, want off", lvl)
	}
	if reason, _ := d.AutoDeployReason(ctx, "web", "default"); reason != "disabled by rollback" {
		t.Fatalf("reason = %q, want disabled by rollback", reason)
	}
	// A different environment is untouched.
	if reason, _ := d.AutoDeployReason(ctx, "web", "prod"); reason != "" {
		t.Fatalf("prod reason = %q, want empty (isolated)", reason)
	}
	// A human re-enable clears the reason.
	if err := d.SetAutoDeployLevel(ctx, "web", "default", controlplane.AutoDeployMinor); err != nil {
		t.Fatalf("SetAutoDeployLevel: %v", err)
	}
	if reason, _ := d.AutoDeployReason(ctx, "web", "default"); reason != "" {
		t.Fatalf("reason after re-enable = %q, want empty", reason)
	}
}

func TestDatabaseOverwriteKeepsOrder(t *testing.T) {
	ctx := context.Background()
	d := NewDatabase()
	_ = d.SaveRelease(ctx, controlplane.Release{ID: "r1", App: "web", Image: "img:1", Status: controlplane.ReleasePending})
	_ = d.SaveRelease(ctx, controlplane.Release{ID: "r2", App: "web", Image: "img:2"})
	// Re-save r1 with an updated status (a rollout transition). Order must not change
	// and r1 must not be duplicated.
	_ = d.SaveRelease(ctx, controlplane.Release{ID: "r1", App: "web", Image: "img:1", Status: controlplane.ReleaseSuperseded})

	all, _ := d.Releases(ctx, "web", "default")
	if len(all) != 2 || all[0].ID != "r1" || all[1].ID != "r2" {
		t.Fatalf("Releases after overwrite = %+v, want [r1 r2]", all)
	}
	got, _ := d.Release(ctx, "r1")
	if got.Status != controlplane.ReleaseSuperseded {
		t.Fatalf("r1 status = %q, want superseded (overwrite should apply)", got.Status)
	}
}

func TestDatabaseDeepCopies(t *testing.T) {
	ctx := context.Background()
	d := NewDatabase()
	env := map[string]string{"K": "V"}
	cmd := []string{"run"}
	_ = d.SaveRelease(ctx, controlplane.Release{ID: "r1", App: "web", Image: "img:1", Env: env, Command: cmd})

	// Mutating the caller's maps/slices after save must not affect the stored record.
	env["K"] = "TAMPERED"
	cmd[0] = "tampered"

	got, _ := d.Release(ctx, "r1")
	if got.Env["K"] != "V" {
		t.Errorf("stored Env was aliased: got %q, want V", got.Env["K"])
	}
	if got.Command[0] != "run" {
		t.Errorf("stored Command was aliased: got %q, want run", got.Command[0])
	}

	// Mutating a returned copy must not affect the store either.
	got.Env["K"] = "AGAIN"
	again, _ := d.Release(ctx, "r1")
	if again.Env["K"] != "V" {
		t.Errorf("returned Env aliases the store: got %q, want V", again.Env["K"])
	}
}

func TestDatabaseSaveEmptyID(t *testing.T) {
	if err := NewDatabase().SaveRelease(context.Background(), controlplane.Release{App: "web", Image: "img:1"}); err == nil {
		t.Fatalf("SaveRelease with empty ID should error")
	}
}
