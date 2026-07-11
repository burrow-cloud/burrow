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
// error — but reads the releases app-globally, because a release row carries no environment (the
// same reason Status reads its release by app). env therefore fails fast on a bad name and keeps the
// surface consistent with the other per-app, per-environment verbs.
//
// Each entry carries what the deploy record holds today: the image reference (the version), the
// creation timestamp, and the lifecycle status. Extensibility: ADR-0052 §5 will enrich the Release
// record with a deploy's provenance — whether it was an unattended auto-update and the level it ran
// under. Once that field lands on Release it surfaces here automatically, with no change to this
// method: history renders whatever the record carries.
func (e *Engine) History(ctx context.Context, app, env string) ([]Release, error) {
	if err := (App{Name: app}).Validate(); err != nil {
		return nil, fmt.Errorf("history: %w: %w", ErrInvalid, err)
	}
	// Resolve the environment so an unknown name is a clear error, mirroring Status. The release
	// records are app-global, so the resolved namespace is not used to filter them; validation is
	// the point (ADR-0035 phase 2b).
	if _, err := e.resolveNamespace(ctx, env); err != nil {
		return nil, fmt.Errorf("history %s: %w", app, err)
	}
	releases, err := e.db.ListReleases(ctx, app)
	if err != nil {
		return nil, fmt.Errorf("history %s: reading release history: %w", app, err)
	}
	return releases, nil
}
