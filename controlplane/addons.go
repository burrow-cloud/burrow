// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"fmt"
	"regexp"
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
	// AddonPostgres is a PostgreSQL instance the agent attaches an app to — Burrow provisions a
	// database and login role per app inside it and writes the app's DATABASE_URL into the app's
	// per-environment Secret (ADR-0031). Each environment gets its OWN instance, never a shared one:
	// the environment, not a naming convention inside a shared server, is what isolates one
	// environment's data from another's (ADR-0067 §1). One per environment is the DEFAULT and not the
	// maximum — an operator who needs a second server's worth of blast radius asks for one by name
	// (`addon install postgres --name analytics`, ADR-0091 §1) — and an instance still belongs to
	// exactly one environment however many an environment holds. The instance is a CloudNativePG `Cluster` and
	// nothing else — Burrow writes one custom resource and the operator composes the workload, the
	// volume and the services from it (ADR-0066 §1).
	AddonPostgres AddonType = "postgres"
)

// CNPGPostgresImage is the PostgreSQL operand image a Burrow-authored `Cluster` runs. Burrow ships
// no third-party bytes: this names an image the CLUSTER pulls from the publisher who built it.
//
// Three things are deliberate about which image this is.
//
//   - It is CloudNativePG's own operand image rather than a stock `postgres` one. CNPG's instance
//     manager runs as PID 1 inside it and the entrypoint is the operator's, so an arbitrary
//     PostgreSQL image is not a substitution CNPG supports.
//   - It is the MINIMAL variant. CNPG's standard operand images bundle barman-cloud, which shells
//     out to GPL-3.0 tooling; ADR-0066 §3 declines barman on exactly that ground, and its rejection
//     of the WAL-G plugin ("a plugin's licence is not its image's licence") is the record saying
//     this project names images and not just repositories. The minimal image carries PostgreSQL and
//     the instance manager and no backup tooling at all — which is also the right base for §3's
//     pgBackRest plugin, since a CNPG-I plugin ships its own sidecar rather than living in this
//     image.
//   - It is PostgreSQL 17, the major version the add-on has always run, so adopting the operator is
//     not also a major-version jump.
//
// It is pinned to a patch release for the reason every other image in the catalog is: an install
// that happens twice should be the same install. It moves independently of the operator's own pin
// (kube.CNPGVersion) — the operator and the operand are separately released, and CNPG supports a
// range of operands per operator — so it is not derived from it.
const CNPGPostgresImage = "ghcr.io/cloudnative-pg/postgresql:17.10-minimal-trixie"

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
		Type: AddonPostgres,
		// The backend IS the mechanism, because "which concrete implementation serves this add-on" is
		// what Backend has always meant, and for Postgres there is exactly one: CloudNativePG
		// (Apache-2.0, a CNCF project, clearing ADR-0025's licence bar outright).
		Backend: "cloudnative-pg",
		Image:   CNPGPostgresImage,
		Port:    5432,
		// Persistent: a database is durable state, so it gets a volume. Unlike every other add-on the
		// claim is not Burrow's — the operator composes it from the `Cluster` and names it
		// `<instance>-1` (AddonDataVolumeName) — so this size is what the `Cluster` asks for rather
		// than what a Burrow-authored PersistentVolumeClaim requests.
		StorageGi:    10,
		Capabilities: []string{"database"},
		Summary:      "PostgreSQL database (a database and role per app, on one instance per environment by default)",
	},
}

// AddonInstanceName is the name of add-on type t's FIRST instance in environment env: the name that
// instance's Deployment, Service, volume and — for Postgres — its superuser Secret carry in the
// cluster, and the label the operator addresses it by.
//
// IT IS A NAME GENERATOR, NOT A LOOKUP (ADR-0091 §2). An environment may hold more than one instance
// of a type, and the second one's name cannot be a function of `(type, environment)` — so nothing
// resolves an existing instance through this function any more. The registry row is the mapping
// between an instance's label and its name in the cluster, and it is the only mapping (see
// GenerateAddonInstanceName for every instance past the first). This is called at CREATION time, to
// decide what an environment's first instance is called.
//
// EACH ENVIRONMENT'S FIRST INSTANCE KEEPS THE NAME IT ALREADY HAS, which is why this derivation
// survives ADR-0091 unchanged: `burrow-postgres` for the default environment, `burrow-postgres-staging`
// for `staging`. Nothing on a live install moves, and because the label of a first instance is that
// same name, no key an operator has already typed — a guardrail scope, an `addon remove` argument —
// changes either (ADR-0091 §2, ADR-0067 §3).
//
// Isolation still comes from the instance rather than from a naming convention inside a shared
// server, and an instance still belongs to exactly one environment (ADR-0067 §1, ADR-0091 §6).
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

// GenerateAddonInstanceName is the cluster name of an instance PAST an environment's first:
// `burrow-<type>-<id>`, where id is a short generated string from [a-z0-9] (ADR-0091 §2).
//
// THE ID IS GENERATED, NOT DERIVED. It is not a hash of the label and not an encoding of the
// environment, because a name with parts is a name whose parts can be got wrong: an environment name
// and an instance label are drawn from the same alphabet, so `burrow-postgres-staging-x` is both the
// instance `x` in `staging` and the first instance of an environment called `staging-x`. That is the
// composed-name ambiguity cloud ADR-0029 removed from the managed product, where its consequence was
// one tenant reaching another's database. Nothing recovers an environment or a label by splitting a
// name; the registry row is the mapping.
//
// UNIQUENESS IS ENFORCED BY THE REGISTRY RATHER THAN ASSUMED FROM ENTROPY. The name is the registry's
// primary key, so a collision is a refused insert and the caller mints another id — not a silent
// adoption of somebody else's instance.
//
// The alphabet is forced rather than chosen: the same id has to be legal in a Kubernetes object name
// and in everything Burrow composes from it — a CloudNativePG `Cluster` composes its Services as
// `<cluster>-rw`, and a Service name is a DNS-1123 label — so lowercase alphanumeric is the
// intersection.
//
// The operator never types this. `--name` takes the LABEL, and every Burrow surface resolves the
// name back to it (ADR-0091 §1).
func GenerateAddonInstanceName(t AddonType, id string) (string, error) {
	if t == "" {
		return "", fmt.Errorf("add-on instance: add-on type is empty: %w", ErrInvalid)
	}
	if id == "" || !instanceIDAlphabet.MatchString(id) {
		return "", fmt.Errorf("add-on instance for %s: instance id %q is not a lowercase alphanumeric string: %w", t, id, ErrInvalid)
	}
	name := "burrow-" + string(t) + "-" + id
	if len(name) > maxNameLen {
		return "", fmt.Errorf("add-on instance for %s: instance id %q is too long: %w", t, id, ErrInvalid)
	}
	return name, nil
}

// instanceIDAlphabet is cloud ADR-0029's alphabet: lowercase alphanumeric and nothing else.
var instanceIDAlphabet = regexp.MustCompile(`^[a-z0-9]+$`)

// ValidateInstanceLabel checks what `--name` takes: the string a person types, every listing shows,
// and a guardrail key holds (ADR-0091 §1). It is a DNS-1123 label because a first instance's label IS
// its cluster name (ADR-0091 §2), so the two alphabets cannot be allowed to differ — a label that
// could not also be a resource name would be one the first-instance case could not honour.
//
// It is deliberately NOT checked for uniqueness here: that is the registry's answer, and a check made
// anywhere else is a check that can be raced.
func ValidateInstanceLabel(label string) error {
	if label == "" {
		return fmt.Errorf("add-on instance: no name given: %w", ErrInvalid)
	}
	if len(label) > maxNameLen || !dns1123Label.MatchString(label) {
		return fmt.Errorf("add-on instance: name %q is not a valid DNS-1123 label (lowercase letters, digits and dashes): %w", label, ErrInvalid)
	}
	return nil
}

// AddonDataVolumeName is the PersistentVolumeClaim holding the data of add-on type t's instance
// named instance. It exists so a message about a removal names the volume that removal actually acts
// on (ADR-0064 §3) — "this destroys the data volume X" is only informed consent while X is the
// volume being destroyed.
//
// Postgres names it differently from every other add-on, and neither name is a convention Burrow is
// free to pick. The claim Burrow creates for a Deployment-backed add-on (logs, metrics) is named
// after the instance; a Postgres instance is a CloudNativePG `Cluster`, which composes one claim per
// instance and calls it `<instance>-<serial>` — and the single-instance `Cluster` Burrow authors
// (ADR-0066 §1) therefore has exactly one, `<instance>-1`.
//
// IT IS FOR SAYING, NOT FOR ACTING. Every path that DELETES or RETAINS a CloudNativePG claim finds
// it by the label the operator puts on it rather than by this derivation, because a constructed name
// that stopped matching would retain one claim out of a volume group and silently strand the rest.
// A prose name that is wrong costs a confusing sentence; an act aimed at the wrong name costs data.
func AddonDataVolumeName(t AddonType, instance string) string {
	if t == AddonPostgres {
		return instance + "-1"
	}
	return instance
}

// InstallAddonOptions is everything `addon install` needs beyond the add-on's type and its
// environment.
//
// The environment stays a positional argument rather than moving in here, deliberately. ADR-0067 §1
// requires every add-on operation to name the environment it acts on — "a signature that can omit it
// is a signature that will omit it" — and a field on an options struct is exactly the kind of thing
// a caller omits.
type InstallAddonOptions struct {
	// Name is the LABEL of the instance to install: `addon install postgres --name analytics` stands
	// a second instance up beside the environment's own (ADR-0091 §1). Empty means the environment's
	// default instance, which is what every add-on command has always meant and what a
	// single-instance operator never has to say.
	//
	// It is a label rather than a resource name. An environment's FIRST instance is labelled with the
	// name it already carries in the cluster (`burrow-postgres`), so an operator addressing today's
	// instance is addressing it by exactly the string they always have; every later one gets a
	// generated cluster name the operator never types (ADR-0091 §2).
	//
	// Installing a label that already exists in the environment is a re-install of that instance, not
	// a second one: the identity of an instance is its label, and `addon install` has always been
	// idempotent.
	Name string
	// Confirm satisfies the addon.install guardrail's confirmation hold (ADR-0020).
	Confirm bool
	// ArchiveDestination names which registered object-storage provider a Postgres instance archives
	// its write-ahead log and takes its base backups to (ADR-0066 §3). It is only needed when several
	// are registered — ADR-0063 §6 allows that on purpose — and Burrow refuses to guess rather than
	// tying an instance to a repository nobody is watching, because an instance keeps the repository
	// it was created against. Empty with exactly one registered provider means that one; empty with
	// none means the instance does not archive at all, which is not an error.
	ArchiveDestination string
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

// BackupVolumeName is the PersistentVolumeClaim holding the dumps taken from ONE INSTANCE — one
// claim per instance, the same shape ADR-0067 §1 gave the instance those dumps come from and
// ADR-0091 §4 carries down to the instance itself.
//
// A dump is only ever taken from, and only ever restored into, one instance. Sharing one claim
// across instances would have put two servers' dumps for an app of the same name on one disk, which
// the backup and restore Jobs of EITHER instance mount whole: the registry rows would say which
// instance each dump came from while nothing on the volume did. That is issue #339's shape with the
// environment held constant, and it is why the claim follows the instance rather than the
// environment now that an environment may hold more than one.
//
// THE NAMES CANNOT COLLIDE, and that is by construction rather than by convention:
//
//   - Across instances, because the instance name is the registry's primary key and this appends a
//     fixed token to it. Two instances have no name they both resolve to.
//   - Against the INSTANCE names sharing the add-on namespace, because a claim is separated from its
//     instance by a DOT, and an instance name can never contain one: an add-on type has no dot, an
//     environment name is a DNS-1123 *label*, and a generated instance id is lowercase alphanumeric.
//     Without that, an environment called `staging-backups` would name its instance, its Deployment,
//     its Service and its data claim exactly what `staging`'s backup claim is called — the same class
//     of collision this fixes.
//
// THE DEFAULT ENVIRONMENT'S FIRST INSTANCE keeps the unqualified name its claim already carries
// (PostgresBackupVolume), so no dump moves and no existing claim is renamed — the same exemption
// ADR-0067 §3 gives that instance, for the same reason. It is recognised by the instance NAME rather
// than by an environment argument, which is what keeps this a pure function of the instance: the
// unqualified `burrow-<type>` is the one instance that can be the default environment's first.
//
// instance is REQUIRED and is the instance's name in the CLUSTER (the registry key), not its label:
// a claim is a cluster object and a label is a person's word for one. A signature that can omit it is
// a signature that will omit it, and the value it would default to is another instance's dumps.
func BackupVolumeName(t AddonType, instance string) (string, error) {
	if t == "" {
		return "", fmt.Errorf("backup volume: add-on type is empty: %w", ErrInvalid)
	}
	if instance == "" {
		return "", fmt.Errorf("backup volume for %s: no instance named; every backup claim belongs to exactly one instance: %w", t, ErrInvalid)
	}
	if len(instance) > maxNameLen || !dns1123Label.MatchString(instance) {
		return "", fmt.Errorf("backup volume for %s: instance %q is not a valid DNS-1123 label: %w", t, instance, ErrInvalid)
	}
	if instance == "burrow-"+string(t) {
		return instance + defaultBackupVolumeSuffix, nil
	}
	return instance + backupVolumeSuffix, nil
}

const (
	// defaultBackupVolumeSuffix makes the default environment's first instance's claim the name it
	// already has (`burrow-postgres` + `-backups` = PostgresBackupVolume).
	defaultBackupVolumeSuffix = "-backups"
	// backupVolumeSuffix separates every other instance's claim from its instance with a character no
	// instance name can contain, so the two families cannot meet. A PersistentVolumeClaim name is a
	// DNS-1123 subdomain, which admits the dot; an instance name is a DNS-1123 label, which does not.
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
	// Name is the instance's name in the CLUSTER, which is what was actually torn down.
	Name string    `json:"name"`
	Type AddonType `json:"type"`
	// Instance is the LABEL the operator addressed it by (ADR-0091 §1) — the same string for an
	// environment's first instance, and the readable half for every later one. It is reported so the
	// result names the thing the operator typed rather than a generated id they have never seen.
	Instance string `json:"instance,omitempty"`
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
	// Name is the instance's name IN THE CLUSTER and the registry's primary key. For an
	// environment's first instance it is the derived `burrow-<type>[-<env>]` (AddonInstanceName) and
	// equal to Label; for every later one it is `burrow-<type>-<id>` with a generated id, and the
	// operator never types it (ADR-0091 §2).
	Name string    `json:"name"`
	Type AddonType `json:"type"`
	// Label is what a person calls this instance: what `--name` takes, what a guardrail key holds,
	// and what every listing shows beside the cluster name (ADR-0091 §1). It is unique within an
	// environment, which is what makes `<env>.<label>.<code>` an unambiguous guardrail key
	// (ADR-0085 §1) without anything having to parse a composed name.
	//
	// A row written before ADR-0091 has its label backfilled to its name, so nothing an operator has
	// already typed changes meaning. It is empty only on a value that has not been through the
	// registry — a connected backend, or an install result before it is saved — where the name is the
	// answer.
	Label string `json:"label,omitempty"`
	// Environment is the environment this instance serves — the canonical name, with the reserved
	// "default" for the implicit one (ADR-0067 §1). An instance belongs to exactly ONE environment,
	// however many instances an environment holds (ADR-0091 §6), so this is what says which
	// environment's data this row is about, and (with Label) what selects it. It is recorded rather
	// than parsed back out of Name: nothing recovers an environment by splitting a name (ADR-0091 §2).
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
	// Ready is a live property — whether the instance's backing workload is available. It is probed
	// from the cluster at list time and never persisted in the registry.
	Ready bool `json:"ready"`
	// Warning is a non-blocking note about the operation that produced this row. It is NOT persisted
	// — the registry holds what an add-on IS, and this is a fact about one install — so it is empty
	// on every row read back from the registry and only ever set on the value an install returns.
	//
	// It exists for one situation and is deliberately not a refusal: an object-storage destination is
	// registered, so the operator plainly wants backups, and the cluster has no pgBackRest plugin to
	// take them with (ADR-0066 §3). Refusing the install there would take the DATABASE away to
	// protect a backup, on a cluster where installing the plugin may not even be possible yet; and
	// installing silently would hand somebody an instance they believe is archiving. So the instance
	// is created without archiving and the omission is stated.
	Warning string `json:"warning,omitempty"`
	// Backups is what this instance actually does about backups, READ BACK from the cluster after the
	// install rather than inferred from what the install intended. Nil on a row read from the
	// registry, for Warning's reason: it is a fact about an instance at one moment, not a fact about
	// what an add-on is, and a persisted copy would go stale while continuing to read as current.
	Backups *AddonBackups `json:"backups,omitempty"`
}

// AddonAttachment is one recorded attachment: which app, on which instance in which environment, and
// the environment variable its connection string was written under. An app may hold several in one
// environment — one per instance it is attached to (ADR-0091 §3) — and each one names its own
// variable, because the first attachment has `DATABASE_URL` and Burrow refuses to invent a second
// name the application was never told to read.
//
// It carries no connection string. The value lives in the app's Secret and never crosses a seam, an
// API, or an audit row (ADR-0029/0031).
type AddonAttachment struct {
	// Addon is the add-on type the attachment is against; today only postgres has attachments.
	Addon AddonType `json:"addon"`
	// App is the application holding it, which is also the database's name (ADR-0031).
	App string `json:"app"`
	// Environment is the environment whose instance holds the database.
	Environment string `json:"environment,omitempty"`
	// Instance is the instance's name IN THE CLUSTER — the registry key, not the label, because this
	// is what selects a server.
	Instance string `json:"instance"`
	// SecretKey is the variable the connection string was written under.
	SecretKey string `json:"secret_key"`
}

// AddonBackupState is whether an add-on instance archives to object storage. It is a closed set so a
// caller deciding whether backups are on switches on a value instead of parsing prose.
type AddonBackupState string

const (
	// AddonBackupsArchiving means the instance's own wiring — the plugin entry on its CloudNativePG
	// `Cluster`, read back from the API server — says it hands its write-ahead log to an
	// object-storage repository.
	AddonBackupsArchiving AddonBackupState = "archiving"
	// AddonBackupsNone means the instance archives nowhere. It is the ordinary state of an install
	// with no object-storage provider registered, and it is also what an install gets when the
	// cluster has no backup plugin to archive with. Detail says which.
	AddonBackupsNone AddonBackupState = "none"
	// AddonBackupsUnknown means Burrow could not read the wiring back and will not guess. A
	// destination resolved at install time and an instance that actually carries the plugin are
	// different facts; when the second one cannot be read, saying so is the only honest answer.
	AddonBackupsUnknown AddonBackupState = "unknown"
)

// AddonBaseBackupState is whether a base backup — the thing archived write-ahead log is replayed
// ONTO — exists for an archiving instance. Archived write-ahead log with no base backup under it
// cannot be restored, so "archiving" on its own is not the whole answer and this is the rest of it
// (ADR-0066 §2).
type AddonBaseBackupState string

const (
	// AddonBaseBackupPresent means the repository holds at least one full backup, as the instance's
	// own pgBackRest stanza reports it.
	AddonBaseBackupPresent AddonBaseBackupState = "present"
	// AddonBaseBackupRequested means this install asked for one immediately and it has not landed
	// yet. It is deliberately not "present": the request is a fact, the backup is not one until the
	// repository says so.
	AddonBaseBackupRequested AddonBaseBackupState = "requested"
	// AddonBaseBackupNone means the repository holds none and none was requested — an instance
	// installed before immediate first backups existed. It is the state that needs an operator to do
	// something, so it is reported as its own value rather than folded into "requested".
	AddonBaseBackupNone AddonBaseBackupState = "none"
	// AddonBaseBackupUnknown means the repository's own count could not be read.
	AddonBaseBackupUnknown AddonBaseBackupState = "unknown"
)

// AddonBackups is what one add-on instance does about backups, as read from the instance rather than
// from the install's intent. Every field is a name, a number or a member of a closed set — never a
// credential.
//
// It exists because `addon install` used to be silent about this, and silence is the worst shape the
// answer can take: an install with no object-storage provider registered produced exactly the same
// success message as one that archives, and the difference only showed up months later when somebody
// went looking for a backup that had never been taken.
type AddonBackups struct {
	// State says whether the instance archives. Every add-on type carries one, because "this kind of
	// add-on has no backups at all" is an answer somebody needs told too.
	State AddonBackupState `json:"state"`
	// Provider is the registry name of the object-storage provider the instance archives to. A name,
	// never an endpoint credential.
	Provider string `json:"provider,omitempty"`
	// Bucket and RepoPath are where in that object store the repository lives, read back from the
	// instance's own pgBackRest stanza rather than from the destination the install resolved. The two
	// can differ — an instance keeps the repository it was created against — and when they do, what
	// the instance says is the fact.
	Bucket   string `json:"bucket,omitempty"`
	RepoPath string `json:"repo_path,omitempty"`
	// RetentionDays is how long a full backup is kept, 0 when the repository declares no window.
	RetentionDays int `json:"retention_days,omitempty"`
	// Schedule is the cron expression the base backup runs on, in CloudNativePG's six-field form
	// (the leading field is seconds). It is reported so how much a restore could lose is readable
	// without going and finding a custom resource.
	Schedule string `json:"schedule,omitempty"`
	// BaseBackup says whether there is anything for the archived write-ahead log to be replayed onto.
	// Empty when State is not "archiving", where the question does not arise.
	BaseBackup AddonBaseBackupState `json:"base_backup,omitempty"`
	// Detail is one Burrow-authored line elaborating the state — why nothing is archiving, or what to
	// run to fix it. Safe to print: it carries no credential and no vendor response body.
	Detail string `json:"detail,omitempty"`
}

// Archiving reports whether the instance is known to archive. It is a helper for callers rendering a
// line, so the "unknown" state cannot be accidentally read as a yes by a `!= none` test.
func (b *AddonBackups) Archiving() bool { return b != nil && b.State == AddonBackupsArchiving }

// TypeBackups is the backup answer for an add-on type that has no backup mechanism of its own. Only
// Postgres has one; every other add-on in the catalog is reported as backing up nothing, in its own
// words, rather than left silent.
//
// Saying it for the types that do nothing is the point rather than filler. An operator reading a
// successful install has no way to tell "this add-on is backed up" from "this add-on has no backups
// and never will" unless the output distinguishes them, and the second one is a decision they may
// want to argue with — a metrics store holding ten gigabytes of samples on a volume nothing copies
// is a fact worth knowing at install time, not after a node dies.
func TypeBackups(t AddonType) *AddonBackups {
	switch t {
	case AddonCache:
		return &AddonBackups{State: AddonBackupsNone,
			Detail: "a cache holds rebuildable data, so this instance has no volume and nothing to back up"}
	case AddonPostgres:
		// Postgres has one; its state is read from the instance, not from the catalog.
		return nil
	default:
		return &AddonBackups{State: AddonBackupsNone,
			Detail: "Burrow takes no backup of this add-on's data volume; only the postgres add-on has a backup path"}
	}
}
