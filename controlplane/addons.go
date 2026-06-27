// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"sort"
	"time"
)

// AddonType identifies a building-block backing service in the curated catalog (ADR-0025).
type AddonType string

const (
	// AddonLogs is a log-aggregation store (VictoriaLogs, Apache-2.0).
	AddonLogs AddonType = "logs"
)

// AddonSpec is a catalog entry: how to deploy and reach one vetted backing service. The catalog
// is curated and permissively licensed (Apache / MIT / BSD) so Burrow can bundle it without
// copyleft friction (ADR-0025) — which is why logs is VictoriaLogs (Apache), not AGPL Loki.
type AddonSpec struct {
	Type AddonType
	// Backend is the concrete adapter implementation that backs this add-on (e.g.
	// "victorialogs"), recorded in the registry so the agent knows which adapter serves it.
	Backend string
	// Image is the pinned container image for the backing service.
	Image string
	// Port is the service port the app (or the agent, for an observability add-on) reaches it on.
	Port int32
	// StorageGi requests a persistent volume of this size in GiB; 0 is ephemeral. Stateful
	// stores (logs, metrics) persist so data survives a restart.
	StorageGi int
	// Capabilities are what the agent can query this add-on for (e.g. "logs"). For an
	// installed default it is fixed; a connected backend may derive or probe its own (ADR-0026).
	Capabilities []string
	// Summary is a one-line description for the catalog listing.
	Summary string
}

// addonCatalog is the curated, compiled-in set of add-ons Burrow can install. Only
// permissively-licensed backing services belong here (ADR-0025).
var addonCatalog = map[AddonType]AddonSpec{
	AddonLogs: {
		Type:         AddonLogs,
		Backend:      "victorialogs",
		Image:        "victoriametrics/victoria-logs:v1.51.0", // VictoriaLogs, Apache-2.0
		Port:         9428,
		StorageGi:    10,
		Capabilities: []string{"logs"},
		Summary:      "log aggregation (VictoriaLogs)",
	},
}

// AddonCatalog returns the catalog entries in a stable order.
func AddonCatalog() []AddonSpec {
	out := make([]AddonSpec, 0, len(addonCatalog))
	for _, s := range addonCatalog {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out
}

// LookupAddon returns the catalog spec for t, or false if t is not a known add-on type.
func LookupAddon(t AddonType) (AddonSpec, bool) {
	s, ok := addonCatalog[t]
	return s, ok
}

// AddonInfo is one installed add-on instance, as seen by `addon list` and the agent. It carries
// no secret — when an add-on needs a credential it lives in a cluster Secret, never here.
type AddonInfo struct {
	Name string    `json:"name"`
	Type AddonType `json:"type"`
	// Mode is how the backend is provided: "installed" (Burrow deployed it) or "connected"
	// (an existing backend the user runs). Installed-only for now; connect lands later (ADR-0026).
	Mode string `json:"mode"`
	// Backend is the concrete adapter implementation backing this add-on (e.g. "victorialogs").
	Backend      string   `json:"backend,omitempty"`
	Image        string   `json:"image,omitempty"`
	Endpoint     string   `json:"endpoint"` // in-cluster host:port the app or agent reaches it on
	Capabilities []string `json:"capabilities"`
	// CreatedAt is when the add-on was registered, read from the injected clock.
	CreatedAt time.Time `json:"created_at,omitempty"`
	// Ready is a live property — whether the backing Deployment is available. It is probed
	// from the cluster at list time and never persisted in the registry.
	Ready bool `json:"ready"`
}
