// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

// TestHistoryNewestFirst confirms History returns an app's releases newest first, isolated per app,
// and empty for an app with none — the deploy timeline behind `app history`.
func TestHistoryNewestFirst(t *testing.T) {
	e, _, d, _ := newEngine(t, permissive())
	ctx := context.Background()

	r1 := cp.Release{ID: "r1", App: "web", Image: "img:1", Status: cp.ReleaseSuperseded}
	r2 := cp.Release{ID: "r2", App: "web", Image: "img:2", Supersedes: "r1", Status: cp.ReleaseSuperseded}
	r3 := cp.Release{ID: "r3", App: "web", Image: "img:3", Supersedes: "r2", Status: cp.ReleaseDeployed}
	o1 := cp.Release{ID: "o1", App: "api", Image: "api:1", Status: cp.ReleaseDeployed}
	for _, r := range []cp.Release{r1, r2, r3, o1} {
		if err := d.SaveRelease(ctx, r); err != nil {
			t.Fatalf("SaveRelease(%s): %v", r.ID, err)
		}
	}

	got, err := e.History(ctx, "web", "")
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(got) != 3 || got[0].ID != "r3" || got[1].ID != "r2" || got[2].ID != "r1" {
		t.Fatalf("History = %+v, want [r3 r2 r1] newest first", got)
	}
	if got[0].Status != cp.ReleaseDeployed || got[0].Image != "img:3" {
		t.Errorf("newest = %+v, want the deployed img:3", got[0])
	}

	// Per-app isolation: api sees only its own release.
	if oth, err := e.History(ctx, "api", ""); err != nil || len(oth) != 1 || oth[0].ID != "o1" {
		t.Errorf("History(api) = %+v, err=%v, want just o1", oth, err)
	}
	// An app with no releases yields an empty slice and no error.
	if none, err := e.History(ctx, "nobody", ""); err != nil || len(none) != 0 {
		t.Errorf("History(nobody) = %+v, err=%v, want empty", none, err)
	}
}

// TestHistoryValidatesAppAndEnv confirms History validates the app name and resolves the environment
// like Status does: a malformed app is ErrInvalid, an unregistered environment is ErrNotFound, and a
// registered one is accepted, returning that environment's timeline (releases are keyed per
// (app, environment), ADR-0052 Phase 4a).
func TestHistoryValidatesAppAndEnv(t *testing.T) {
	e, _, d, _ := newEngine(t, permissive())
	ctx := context.Background()

	if _, err := e.History(ctx, "Bad Name", ""); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("History(bad app) err = %v, want ErrInvalid", err)
	}
	if _, err := e.History(ctx, "web", "ghost"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("History(unknown env) err = %v, want ErrNotFound", err)
	}

	// A registered environment is accepted; history returns that environment's releases.
	if err := d.CreateEnvironment(ctx, "prod", "burrow-apps-prod"); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	if err := d.SaveRelease(ctx, cp.Release{ID: "r1", App: "web", Environment: "prod", Image: "img:1", Status: cp.ReleaseDeployed}); err != nil {
		t.Fatalf("SaveRelease: %v", err)
	}
	got, err := e.History(ctx, "web", "prod")
	if err != nil || len(got) != 1 || got[0].ID != "r1" {
		t.Errorf("History(web, prod) = %+v, err=%v, want [r1]", got, err)
	}
}
