// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"fmt"
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
	// AddonPostgres is a PostgreSQL instance (the official postgres image, PostgreSQL License) the
	// agent attaches an app to — Burrow provisions a database and login role per app inside it and
	// writes the app's DATABASE_URL into the app's per-environment Secret (ADR-0031). There is one
	// instance PER ENVIRONMENT, not one per cluster: the environment, not a naming convention inside
	// a shared server, is what isolates one environment's data from another's (ADR-0067 §1).
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
		Image:   "postgres:17-alpine", // official PostgreSQL image (Alpine variant — smaller, faster to pull), PostgreSQL License (BSD-style)
		Port:    5432,
		// Persistent: a database is durable state, so it gets a volume; the generic stateful path
		// gives it a Recreate Deployment + a PVC. The superuser password Secret and per-app
		// database provisioning are handled by the install/attach special-cases (ADR-0031).
		StorageGi:    10,
		Capabilities: []string{"database"},
		Summary:      "PostgreSQL database (one instance per environment, a database and role per app)",
	},
}

// AddonInstanceName is the name of add-on type t's instance in environment env: the registry key
// (addons.name) and the resource name the instance's Deployment, Service, volume and — for Postgres
// — its superuser Secret carry in the cluster. It is the ONE place an environment is turned into an
// instance (ADR-0067 §1). Isolation comes from the instance rather than from a naming convention
// inside a shared server, so there is deliberately no name two environments can both resolve to:
// sharing one instance across environments is not merely discouraged, it is inexpressible
// (ADR-0067 §5).
//
// The DEFAULT environment keeps the unqualified name (burrow-postgres), which is what lets an
// install predating environments carry on against the instance, the volume, and the superuser
// credential it already has — it gains an environment, and nothing moves (ADR-0067 §3). Every other
// environment is suffixed with its own name.
//
// Note the switch is on the CONSTANT DefaultEnvironment, not on its value. That is load-bearing:
// ADR-0067 §2 renamed the default environment from `default` to `prod`, and because the unqualified
// case keys on the constant, `burrow-postgres` stayed `burrow-postgres` through the rename. What a
// user sees the environment called and what the instance is called are decoupled on purpose — a
// name is legibility, a resource name is live state, and only one of the two is safe to change.
//
// env is REQUIRED: an empty value is an error, not a synonym for the default environment. "A
// signature that can omit it is a signature that will omit it" (ADR-0067 §1) — callers canonicalize
// an operation's environment first (envName), so an empty value here only ever arrives from a path
// that forgot, which is exactly the path that must not silently land on another environment's data.
func AddonInstanceName(t AddonType, env string) (string, error) {
	if t == "" {
		return "", fmt.Errorf("add-on instance: add-on type is empty: %w", ErrInvalid)
	}
	switch env {
	case "":
		return "", fmt.Errorf("add-on instance for %s: no environment named; every add-on instance belongs to exactly one environment: %w", t, ErrInvalid)
	case DefaultEnvironment:
		return "burrow-" + string(t), nil
	}
	if len(env) > maxNameLen || !dns1123Label.MatchString(env) {
		return "", fmt.Errorf("add-on instance for %s: environment %q is not a valid DNS-1123 label: %w", t, env, ErrInvalid)
	}
	return "burrow-" + string(t) + "-" + env, nil
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

// PostgresBackupVolume is the DEFAULT environment's Postgres backup claim, and the only backup claim
// that existed before backups were per-environment: every dump taken before that change is on it.
// It is named here, next to BackupPath, so the engine can report that removing the add-on
// deliberately LEFT IT IN PLACE without importing the kube package — the same reason BackupPath
// lives in this package rather than in the Job builder.
//
// It is a constant rather than a call to BackupVolumeName because it is also the value migration
// 00027 backfilled every pre-existing backup row's volume to, and a backfill is a historical fact
// about bytes on a disk: it must not move if the derivation is ever revised. BackupVolumeName agrees
// with it for the default environment, asserted by a test.
const PostgresBackupVolume = "burrow-postgres-backups"

// BackupVolumeName is the PersistentVolumeClaim holding add-on type t's dumps for environment env —
// ONE CLAIM PER ENVIRONMENT, the same shape ADR-0067 §1 gives the instance those dumps come from.
//
// A dump is only ever taken from, and only ever restored into, one environment's instance. Sharing
// one claim across environments would have put staging's and production's dumps for an app of the
// same name on one disk, which the backup and restore Jobs of EITHER environment mount whole: the
// registry rows would say which environment each dump came from while nothing on the volume did.
// Isolation comes from the claim, not from a naming convention inside a shared one — the sentence
// ADR-0067 §1 uses about the instance, one level down.
//
// THE NAMES CANNOT COLLIDE, and that is by construction rather than by convention:
//
//   - Across environments, because AddonInstanceName is already injective over (type, environment)
//     and appending a fixed token preserves that. Two environments have no name they both resolve to.
//   - Against the INSTANCE names sharing the add-on namespace, because a named environment's claim
//     is separated by a DOT, and an instance name can never contain one: an add-on type has no dot
//     and an environment name is a DNS-1123 *label*. Without that, an environment called
//     `staging-backups` would name its instance, its Deployment, its Service and its data claim
//     exactly what `staging`'s backup claim is called — the same class of collision this fixes.
//
// The DEFAULT environment keeps the unqualified name its claim already carries
// (PostgresBackupVolume), so no dump moves and no existing claim is renamed — the same exemption
// ADR-0067 §3 gives the default environment's instance, for the same reason. That is the one name in
// the family sitting inside the instance family's shape, which is why an environment called
// `backups` is reserved (ReservedEnvironmentNames).
//
// env is REQUIRED, for AddonInstanceName's reason: a signature that can omit it is a signature that
// will omit it, and the value it would default to is another environment's dumps.
func BackupVolumeName(t AddonType, env string) (string, error) {
	instance, err := AddonInstanceName(t, env)
	if err != nil {
		return "", fmt.Errorf("backup volume: %w", err)
	}
	if env == DefaultEnvironment {
		return instance + defaultBackupVolumeSuffix, nil
	}
	return instance + backupVolumeSuffix, nil
}

const (
	// defaultBackupVolumeSuffix makes the default environment's claim the name it already has
	// (`burrow-postgres` + `-backups` = PostgresBackupVolume).
	defaultBackupVolumeSuffix = "-backups"
	// backupVolumeSuffix separates a named environment's claim from its instance with a character no
	// instance name can contain, so the two families cannot meet. A PersistentVolumeClaim name is a
	// DNS-1123 subdomain, which admits the dot; an environment name is a DNS-1123 label, which does
	// not.
	backupVolumeSuffix = ".backups"
)

// Add-on volume roles: what a claim in the add-on namespace holds. The role decides what a retained
// claim is worth keeping for — a data claim comes back to life on reinstall, a backup claim is a
// pile of dumps that outlives the database it came from (ADR-0032, ADR-0064 §4).
const (
	// AddonVolumeData is the add-on's own data volume: the claim its workload mounts.
	AddonVolumeData = "data"
	// AddonVolumeBackup is the Postgres add-on's dump volume (PostgresBackupVolume).
	AddonVolumeBackup = "backup"
)

// AddonVolume is one PersistentVolumeClaim in the add-on namespace that belongs to an add-on: which
// add-on it serves, what it holds, and how big it is. It is the unit `addon list` reports a RETAINED
// volume in — a claim an earlier removal deliberately left behind (ADR-0064 §6). Keeping data by
// default is only defensible while the leftovers are visible: an invisible claim is a silent bill,
// and a bill is a worse way to find out than a listing.
type AddonVolume struct {
	// Name is the claim name, which is also what `kubectl delete pvc` takes.
	Name string `json:"name"`
	// Namespace is the add-on namespace the claim lives in.
	Namespace string `json:"namespace,omitempty"`
	// Addon is the add-on type the claim was created for, read from the claim's own Burrow labels —
	// not inferred from its name.
	Addon AddonType `json:"addon"`
	// Environment is the environment the claim serves, read from the claim's own Burrow labels.
	// Empty for a claim created before add-ons were per-environment, which is the default
	// environment's by construction — it is left empty rather than filled in, because the label is
	// what the cluster actually says. With one instance and one backup claim per environment
	// (ADR-0067 §1) it is the difference between a listing that says which claim is which and one
	// that leaves the operator to read it off a name suffix.
	Environment string `json:"environment,omitempty"`
	// Role is what the claim holds: AddonVolumeData or AddonVolumeBackup.
	Role string `json:"role"`
	// Size is the claim's provisioned capacity where the cluster reports it, falling back to the
	// requested size (e.g. "10Gi"). Empty when neither is known. Size, not cost: cost needs the
	// provider's per-GiB price and can be wrong, and a wrong number about money is worse than an
	// honest one about bytes (ADR-0064 §Deliberately left open).
	Size string `json:"size,omitempty"`
	// ReinstallAdopts reports whether reinstalling the add-on picks this claim back up with its data
	// intact (ADR-0064 §1). True for a data claim — the reinstall lands on the same claim name — and
	// false for a backup claim, which is read by the backup/restore Jobs rather than adopted.
	ReinstallAdopts bool `json:"reinstall_adopts"`
	// CreatedAt is the claim's creation time as the cluster records it.
	CreatedAt time.Time `json:"created_at,omitempty"`
}

// AddonRemoval is what the cluster-side teardown of an add-on actually did: which namespace it
// acted in, whether it destroyed the add-on's data volume, and which volumes it deliberately left
// behind. Removal keeps the data volume unless the caller explicitly asks for it to go, so a
// removal that was meant as "stop this and reinstall it" cannot silently destroy every attached
// app's database (ADR-0025/0031); the retained names are reported so the caller can see what
// survived and reclaim it later.
type AddonRemoval struct {
	// Namespace is the namespace the add-on's resources — and any retained volume — live in.
	Namespace string `json:"namespace,omitempty"`
	// DataDeleted reports whether the add-on's data volume was destroyed. It is true only when the
	// caller explicitly asked for it AND the add-on had a volume to destroy.
	DataDeleted bool `json:"data_deleted"`
	// RetainedDataVolume names the PersistentVolumeClaim holding the add-on's data that was left in
	// place. Empty when the add-on has no volume, or when the caller asked for it to be deleted.
	RetainedDataVolume string `json:"retained_data_volume,omitempty"`
	// RetainedBackupVolume names the backup PersistentVolumeClaim that was left in place — the
	// Postgres add-on's dumps (ADR-0032). Backups deliberately outlive the database they came from,
	// so this survives even a data-deleting removal; it is empty when no backup volume exists.
	RetainedBackupVolume string `json:"retained_backup_volume,omitempty"`
}

// RemoveAddonResult is the structured outcome of removing an add-on (ADR-0025): what was torn down
// and, just as importantly, what was kept. AttachedApps names the apps that held a Burrow-provisioned
// database on the instance — the concrete scope of the consequence, whether that is "these lost their
// data" or "these are disconnected until it is reinstalled".
type RemoveAddonResult struct {
	Name string    `json:"name"`
	Type AddonType `json:"type"`
	// AddonRemoval is embedded so its fields (namespace, retained volumes) flatten into the JSON the
	// agent reads — the removal facts are the result, not a nested detail of it.
	AddonRemoval
	// AttachedApps are the apps with a Burrow-provisioned database on this instance at removal time,
	// sorted. Empty for an add-on type that has no per-app attachments, and empty (not an error) when
	// the instance could not be reached to enumerate them — a wedged add-on must stay removable.
	AttachedApps []string `json:"attached_apps,omitempty"`
	// FinalBackups are the backups taken before the data volume was destroyed, one per attached
	// database (ADR-0064 §5). Each is a completed row at an object-store destination — the removal
	// does not get past them otherwise — so this is the list of copies that outlived the instance,
	// and it is what a restore is addressed with. Empty on a removal that kept its data.
	FinalBackups []Backup `json:"final_backups,omitempty"`
	// FinalBackupSkipped reports that the data volume was destroyed with NO off-cluster copy taken:
	// the override flag was passed, no object-storage provider was registered, or the add-on is not
	// one Burrow can dump. It is reported rather than inferred from an empty FinalBackups, because
	// "nothing was backed up" and "nothing needed backing up" are the two answers an operator must
	// not have to guess between.
	FinalBackupSkipped bool `json:"final_backup_skipped,omitempty"`
	// FinalBackupNote is the one-line reason behind FinalBackupSkipped, safe to print.
	FinalBackupNote string `json:"final_backup_note,omitempty"`
}

// AddonInfo is one installed add-on instance, as seen by `addon list` and the agent. It carries
// no secret — when an add-on needs a credential it lives in a cluster Secret, never here.
type AddonInfo struct {
	Name string    `json:"name"`
	Type AddonType `json:"type"`
	// Environment is the environment this instance serves — the canonical name, with the reserved
	// "default" for the implicit one (ADR-0067 §1). Each environment gets its own instance, so this
	// is what says which one this row is, and it is what an attach reads to decide which server to
	// provision a database on. It is recorded rather than parsed back out of Name: the name is
	// derived from the environment, never the other way round.
	Environment string `json:"environment,omitempty"`
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
