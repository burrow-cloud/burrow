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
	// AddonMetrics is a metrics store (VictoriaMetrics single-node, Apache-2.0) paired with a
	// vmagent scraper that collects app-pod metrics and remote-writes them into it.
	AddonMetrics AddonType = "metrics"
	// AddonCache is an in-memory cache (ValKey, BSD-3) the agent wires an app to — a backing
	// service the app connects to, not one the agent queries, so it has no query seam.
	AddonCache AddonType = "cache"
	// AddonPostgres is a shared PostgreSQL instance (the official postgres image, PostgreSQL
	// License) the agent attaches an app to — Burrow provisions a database and login role per app
	// inside the one instance and writes the app's DATABASE_URL into its per-app Secret (ADR-0031).
	AddonPostgres AddonType = "postgres"
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
	AddonMetrics: {
		Type:         AddonMetrics,
		Backend:      "victoriametrics",
		Image:        "victoriametrics/victoria-metrics:v1.115.0", // VictoriaMetrics single-node, Apache-2.0
		Port:         8428,
		StorageGi:    10,
		Capabilities: []string{"metrics"},
		Summary:      "metrics (VictoriaMetrics + a vmagent scraper)",
	},
	AddonCache: {
		Type:    AddonCache,
		Backend: "valkey",
		Image:   "valkey/valkey:8.0", // ValKey, BSD-3
		Port:    6379,
		// Ephemeral: a cache is rebuildable, so it gets no persistent volume and no collector —
		// the generic deploy path (Deployment + Service) is all it needs. The agent reads the
		// endpoint from `addon list` and wires the app to it.
		StorageGi:    0,
		Capabilities: []string{"cache"},
		Summary:      "in-memory cache (ValKey)",
	},
	AddonPostgres: {
		Type:    AddonPostgres,
		Backend: "postgres",
		Image:   "postgres:17.4", // official PostgreSQL image, PostgreSQL License (BSD-style)
		Port:    5432,
		// Persistent: a database is durable state, so it gets a volume; the generic stateful path
		// gives it a Recreate Deployment + a PVC. The superuser password Secret and per-app
		// database provisioning are handled by the install/attach special-cases (ADR-0031).
		StorageGi:    10,
		Capabilities: []string{"database"},
		Summary:      "PostgreSQL database (one shared instance, a database and role per app)",
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

// ConnectBackend is a catalog entry for an existing backend the user already runs that Burrow can
// register and query (ADR-0026). Unlike an AddonSpec it carries no image or storage: connect is
// registration-only — Burrow never deploys a connected backend, it queries the one already there.
type ConnectBackend struct {
	// Name is the backend identifier (e.g. "loki"), used as the add-on Backend and as the
	// querier key the engine dispatches on.
	Name string
	// Capabilities are what the agent can query a connected instance of this backend for. They
	// are derived from the backend, not declared by the user (ADR-0026): a single-capability
	// backend like Loki implies "logs".
	Capabilities []string
	// Summary is a one-line description for the connectable-backend listing.
	Summary string
}

// connectCatalog is the curated set of existing backends Burrow can connect to and query. The
// license bar does not apply to connect — Burrow queries these, it does not distribute them
// (ADR-0026) — so AGPL backends like Loki are fine here.
var connectCatalog = map[string]ConnectBackend{
	"loki":       {Name: "loki", Capabilities: []string{"logs"}, Summary: "Grafana Loki (existing log store)"},
	"prometheus": {Name: "prometheus", Capabilities: []string{"metrics"}, Summary: "Prometheus (existing metrics store)"},
}

// ConnectCatalog returns the connectable backends in a stable name order.
func ConnectCatalog() []ConnectBackend {
	out := make([]ConnectBackend, 0, len(connectCatalog))
	for _, b := range connectCatalog {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// LookupConnectBackend returns the catalog entry for name, or false if name is not a known
// connectable backend.
func LookupConnectBackend(name string) (ConnectBackend, bool) {
	b, ok := connectCatalog[name]
	return b, ok
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
	// SecretKey is the non-secret key under which this add-on's bearer token lives in the
	// burrow-credentials Secret (ADR-0023). Empty means the backend is unauthenticated; the
	// token itself never travels here — only the key (ADR-0004).
	SecretKey string `json:"secret_key,omitempty"`
	// CreatedAt is when the add-on was registered, read from the injected clock.
	CreatedAt time.Time `json:"created_at,omitempty"`
	// Ready is a live property — whether the backing Deployment is available. It is probed
	// from the cluster at list time and never persisted in the registry.
	Ready bool `json:"ready"`
}
