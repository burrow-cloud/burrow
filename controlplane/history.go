// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"fmt"
)

// History returns an app's deploy timeline: the releases recorded for it, newest first — what
// versions the app has been rolled to, when, and whether each landed (the Release.Status conveys
// success or failure). It is a read-only view over the deploy records the deploy engine already
// writes (ADR-0007); it records nothing and touches no cluster state, so it is safe over any
// channel. It resolves and validates env exactly as Status does — an unknown environment is a clean
// error — and reads the releases for that environment: releases are keyed per (app, environment)
// (ADR-0052 Phase 4a), so history is the app's timeline in the selected environment.
//
// Each entry carries what the deploy record holds: the image reference (the version), the creation
// timestamp, the lifecycle status, and the deploy's provenance (ADR-0052 §5) — whether it was a
// manual or an unattended auto-update, and the level and tag an auto-update ran under. The record
// carries those fields, so history renders whatever they hold with no per-field logic here.
func (e *Engine) History(ctx context.Context, app, env string) ([]Release, error) {
	if err := (App{Name: app}).Validate(); err != nil {
		return nil, fmt.Errorf("history: %w: %w", ErrInvalid, err)
	}
	// Resolve the environment so an unknown name is a clear error, mirroring Status, then read the
	// releases for that environment (ADR-0052 Phase 4a keys releases per (app, environment)).
	if _, err := e.resolveNamespace(ctx, env); err != nil {
		return nil, fmt.Errorf("history %s: %w", app, err)
	}
	releases, err := e.db.ListReleases(ctx, app, envName(env))
	if err != nil {
		return nil, fmt.Errorf("history %s: reading release history: %w", app, err)
	}
	return releases, nil
}
