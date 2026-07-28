// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"fmt"
	"sort"
)

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
	// One instance per add-on type per cluster (ADR-0025), so the type is the ownership key: a claim
	// whose add-on type is installed belongs to that live add-on and is not retained. This is what
	// keeps an installed add-on's own volume — and its backup claim — out of the retained listing.
	installed := make(map[AddonType]bool, len(addons))
	for _, a := range addons {
		if a.Mode == "installed" {
			installed[a.Type] = true
		}
	}
	volumes, err := e.k8s.AddonVolumes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list retained addon volumes: reading the cluster: %w", err)
	}
	retained := make([]AddonVolume, 0, len(volumes))
	for _, v := range volumes {
		if !installed[v.Addon] {
			retained = append(retained, v)
		}
	}
	sort.Slice(retained, func(i, j int) bool { return retained[i].Name < retained[j].Name })
	return retained, nil
}
