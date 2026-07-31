// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"fmt"
	"sort"
)

// addonInstanceKey identifies one add-on instance: a type in an environment. It is the ownership key
// for a claim, because that is the granularity an instance exists at (ADR-0067 §1).
type addonInstanceKey struct {
	addon AddonType
	env   string
}

// RetainedAddonVolumes returns the add-on volumes no installed add-on owns — the claims an earlier
// `addon remove` deliberately left behind (ADR-0064 §1), reported so reclaiming them is a decision a
// user can make rather than an archaeology exercise in `kubectl get pvc` (ADR-0064 §6).
//
// The registry says what is installed; the cluster says what claims exist. A retained volume is the
// difference: a claim Burrow created for an add-on that is no longer installed. Deriving it from the
// cluster rather than from registry rows is the load-bearing part — a removed add-on has no registry
// row at all, which is precisely why its volume was invisible.
//
// Nothing here reclaims anything. An automatic reaper for the data ADR-0064 exists to protect would
// reintroduce the failure it prevents, by a slower route (ADR-0064 §Consequences).
func (e *Engine) RetainedAddonVolumes(ctx context.Context) ([]AddonVolume, error) {
	addons, err := e.db.Addons(ctx)
	if err != nil {
		return nil, fmt.Errorf("list retained addon volumes: reading the registry: %w", err)
	}
	// One instance per add-on type per ENVIRONMENT (ADR-0067 §1), so the pair is the ownership key: a
	// claim whose add-on type is installed IN ITS OWN ENVIRONMENT belongs to that live add-on and is
	// not retained. This is what keeps an installed add-on's own volume — and its backup claim — out
	// of the retained listing.
	//
	// The environment has to be part of the key. Removing staging's Postgres while production's is
	// installed leaves staging's data and backup claims allocated and billed, and a type-only key
	// would hide exactly those behind production's live instance — the accumulation ADR-0064 §6
	// exists to make visible, invisible again for the multi-environment case.
	installed := make(map[addonInstanceKey]bool, len(addons))
	for _, a := range addons {
		if a.Mode == "installed" {
			installed[addonInstanceKey{a.Type, envName(a.Environment)}] = true
		}
	}
	volumes, err := e.k8s.AddonVolumes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list retained addon volumes: reading the cluster: %w", err)
	}
	retained := make([]AddonVolume, 0, len(volumes))
	for _, v := range volumes {
		// A claim created before add-ons were per-environment carries no environment label and is the
		// default one's, which is the same reading its add-on's registry row gets (ADR-0067 §3).
		if !installed[addonInstanceKey{v.Addon, envName(v.Environment)}] {
			retained = append(retained, v)
		}
	}
	sort.Slice(retained, func(i, j int) bool { return retained[i].Name < retained[j].Name })
	return retained, nil
}
