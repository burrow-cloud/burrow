// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"errors"
	"fmt"
)

// Disposition is how the control plane enforces a guardrail when an operation trips it:
// allow it silently, hold it for explicit confirmation, or deny it outright (ADR-0020).
type Disposition string

const (
	DispositionAllow   Disposition = "allow"
	DispositionConfirm Disposition = "confirm"
	DispositionDeny    Disposition = "deny"
)

// Valid reports whether d is a known disposition.
func (d Disposition) Valid() bool {
	switch d {
	case DispositionAllow, DispositionConfirm, DispositionDeny:
		return true
	default:
		return false
	}
}

// GuardrailCode identifies a guardrail: it is both the key a Policy configures the
// guardrail's disposition under and the machine-readable reason it appears in a refusal,
// so an agent can branch on the cause rather than parse prose (ADR-0006, ADR-0020).
type GuardrailCode string

const (
	// GuardrailAppDeploy: the operation would deploy a new release of an app. Deploy is the
	// core action, so it is allowed by default and gated only if an operator opts in — set it
	// to confirm to require sign-off before a deploy (e.g. in prod), or to deny to freeze
	// deploys entirely, per environment. Realizes ADR-0007: the explicit deploy call is where
	// the guardrails live.
	GuardrailAppDeploy GuardrailCode = "app.deploy"
	// GuardrailScaleToZero: the operation would scale to zero replicas.
	GuardrailScaleToZero GuardrailCode = "app.scale_to_zero"
	// GuardrailExposePublic: the operation would make an app reachable from outside the
	// cluster (expose it at a hostname).
	GuardrailExposePublic GuardrailCode = "app.expose_public"
	// GuardrailDNSWrite: the operation would create or update a public DNS record at a
	// configured provider (ADR-0018).
	GuardrailDNSWrite GuardrailCode = "dns.write"
	// GuardrailDNSDelete: the operation would delete a public DNS record at a configured
	// provider — the destructive side of DNS management (ADR-0018). Denied by default
	// (ADR-0065 §3): removing the record takes an application off the internet, and the record may
	// not be one Burrow created, so a confirmation the caller can satisfy itself is too weak a
	// control. An operator who wants the agent tidying up DNS sets it to confirm or allow with
	// `guard set dns.delete ...` — cluster-wide, because a DNS operation carries no environment to
	// scope the disposition to: AddDomain and RemoveDomain act on a hostname at a vendor, and
	// nothing in the request says which environment it belongs to. Its declaration below says so
	// (ADR-0068 §5): scoping it is a matter of the operation carrying an environment, not of the
	// code's name.
	GuardrailDNSDelete GuardrailCode = "dns.delete"
	// GuardrailAddonInstall: the operation would install a building-block backing service
	// (a vetted add-on like logs or metrics) onto the cluster (ADR-0025).
	GuardrailAddonInstall GuardrailCode = "addon.install"
	// GuardrailAddonRemove: the operation would remove an installed add-on — the destructive
	// side, since dependent apps may rely on it (ADR-0025).
	GuardrailAddonRemove GuardrailCode = "addon.remove"
	// GuardrailAddonAttach: the operation would give an app its own database on an add-on instance
	// and wire it in (ADR-0031, ADR-0095). Held for confirmation by default — ADR-0065's THIRD tier,
	// the one bucket.create sits in, and for the same sentence: additive, reversible, part of a
	// legitimate workflow, and it spends somebody's disk.
	//
	// Attach was ungated until ADR-0095, on the grounds that it "provisions and destroys nothing".
	// That is true and it is not the whole of what the verb does: it creates a database and a login
	// role on a server every other app in the environment shares, writes a credential into the app's
	// Secret, RESTARTS the app, and on an app that is already attached ROTATES the role's password —
	// which nothing can undo, because the connection string is generated server-side and never
	// returned, so anything else holding the old one (a pooler, a sibling service, a CI migration
	// runner) simply stops connecting.
	//
	// Confirm rather than deny, and the difference from app.delete is reversibility. A wrong attach
	// is undoable by a human: detach removes the credential and drops the role, and the database it
	// leaves behind goes with `detach --delete-data` (ADR-0090). Confirm rather than allow because
	// the confirmation here CAN be an informed one — "give web its own database on burrow-postgres in
	// prod, writing the connection string into DATABASE_URL" is a sentence a reader can act on, which
	// is the test ADR-0087 §5 set when it refused confirm for addon.sql.
	//
	// It is env-scopable, unlike almost every other addon.* code — see knownGuardrails. The expected
	// shape is a gradient: `guard set --env dev addon.attach allow` for a sandbox where the agent
	// building three services should not stop three times, confirm or deny in production.
	//
	// The disposition binds every caller, including the operator. The setting most operators will
	// want is `guard set --binds agent addon.attach deny`, and that axis arrives with ADR-0094; this
	// default is the correct one either way, since a built-in default binds every kind (ADR-0094 §3).
	GuardrailAddonAttach GuardrailCode = "addon.attach"
	// GuardrailAddonDetach: the operation would detach an app from an add-on — for Postgres,
	// dropping the app's login role so that nothing it holds still authenticates, and KEEPING the
	// database (ADR-0090 §1). Held for confirmation by default: what it guards is an app losing
	// access to its data, which is disruptive and worth stopping to say out loud, rather than the
	// data being destroyed, which is no longer what the verb does. Destroying it needs
	// `--delete-data`, which is not reachable from the agent surface and so is not gated here
	// (ADR-0090 §4). Attach is guarded too, by addon.attach above (ADR-0095), so the pair reads
	// symmetrically: one hold for creating an app's access to a database, one for ending it.
	GuardrailAddonDetach GuardrailCode = "addon.detach"
	// GuardrailAddonRestore: the operation would restore an app's database from a backup,
	// overwriting its live contents (ADR-0032). Held for confirmation by default, like detach.
	// (Backup and list are not guarded: they destroy nothing.)
	GuardrailAddonRestore GuardrailCode = "addon.restore"
	// GuardrailAddonRestoreInstance: the operation would rewind a whole Postgres instance to a point
	// in its object-storage repository, taking every app's database on it back together (ADR-0066
	// §4). Held for confirmation by default.
	//
	// IT IS A SEPARATE CODE FROM addon.restore, and the separation is the point ADR-0064's
	// "deliberately left open" section raised about `--delete-data` sharing `addon.remove`'s
	// disposition. The two restores have materially different blast radii — one app's database against
	// every app's — so an operator who wants per-app restore available and instance-wide rewinds
	// denied has to be able to say so. Sharing one disposition would have made that unexpressable.
	//
	// The disposition is not the boundary here, which is worth stating so it is never mistaken for
	// one: `addon restore-instance` is not compiled into `burrow-agent` at all (ADR-0065 §2 tier 1),
	// and this code exists so the hold is legible and configurable for the human who can run it.
	GuardrailAddonRestoreInstance GuardrailCode = "addon.restore_instance"
	// GuardrailAddonSQL: the operation would run a caller-supplied statement against one app's
	// database on a relational add-on (ADR-0087). DENIED by default — ADR-0065 §3's tier 2, not tier
	// 1: the verb is compiled into `burrow-agent` and the agent can see that it exists and that it is
	// closed, because an agent that knows a capability is denied asks for it rather than inventing a
	// way around it.
	//
	// Deny rather than confirm, and the difference from app.run is the reason. `app.run`'s confirm
	// default is defensible because a human reads a command line and recognises `npm run migrate`; a
	// human reading a hundred-line statement is not meaningfully approving it, and where a
	// confirmation cannot be an informed one, holding for confirmation is theatre. There is also no
	// upper bound on what a statement does (ADR-0087 §5).
	//
	// ONE code, gating whether the statement runs and not what it does (ADR-0087 §6). Burrow does not
	// parse the statement and does not branch on its leading keyword: a `SELECT` can delete
	// (`WITH d AS (DELETE … RETURNING *) SELECT * FROM d`), a function call is whatever the function
	// is, and `COPY … TO PROGRAM` is not a query at all. A classifier that is wrong in the permissive
	// direction is a gate people trust and should not.
	//
	// It is env-scopable, and the expected shape is a gradient set with
	// `guard set --env <env> addon.sql …`: allow in development, where the database is disposable and
	// the agent inspecting it is the whole value; confirm or deny in production. It is also the FIRST
	// addon.* code that is env-scopable, which is deliberate rather than an inconsistency — see
	// knownGuardrails below.
	GuardrailAddonSQL GuardrailCode = "addon.sql"
	// GuardrailAppDelete: the operation would delete an app entirely — its workload, routing,
	// and release history — so it disappears from the apps listing. The destructive teardown
	// of a deployed application. Denied by default (ADR-0065 §3): destroying the release history
	// leaves nothing to roll back to, so a confirmation protects only an attentive reader. The
	// deny is a floor, not a fixed setting — app.delete is env-scopable, and the expected shape is
	// a gradient set with `guard set --env <env> app.delete ...`: allow where the agent should tidy
	// up after itself, confirm in staging, deny in production.
	GuardrailAppDelete GuardrailCode = "app.delete"
	// GuardrailRollback: the operation would roll an app back to its previous release. A
	// production mutation, but a recovery one — allowed by default so an agent can restore a
	// broken app quickly; an operator can set it to confirm or deny to require sign-off for
	// server-side, agent-independent enforcement (ADR-0020).
	GuardrailRollback GuardrailCode = "app.rollback"
	// GuardrailAutoscale: the operation would configure (or turn off) autoscaling for an app — apply
	// a HorizontalPodAutoscaler on its Deployment. Allowed by default: autoscaling is helpful and
	// non-destructive, and the autoscaler's max is independently bounded by the replica ceiling
	// (LimitReplicaCeiling), which is an operational limit rather than a guardrail (ADR-0068 §2).
	// An operator can raise it to confirm or deny per environment, e.g. deny in prod so only a
	// human sets the scaling shape there.
	GuardrailAutoscale GuardrailCode = "app.autoscale"
	// GuardrailAppRun: the operation would run a caller-provided one-off command inside the app's own
	// current image and environment (ADR-0048) — a migration, seed, backfill, or maintenance script.
	// Held for confirmation by default: a command runs opaquely and may make destructive changes, so
	// the human sees and approves the exact command before it runs, and prod is the environment to
	// keep gated. The guardrail gates whether the command runs, not what it does (ADR-0048 §5).
	GuardrailAppRun GuardrailCode = "app.run"
	// GuardrailBucketCreate: the operation would create a bucket at an object-storage provider
	// (ADR-0063 §5). Held for confirmation by default, which is ADR-0065's THIRD tier: creating a
	// bucket is additive, reversible, and part of a legitimate workflow, but it costs money at a
	// vendor, so a human approves it.
	//
	// Its counterpart is deliberately absent rather than guarded. Bucket DELETION is not a
	// guardrail code, because it is not an operation Burrow performs at all: its blast radius is
	// every backup the platform holds, and a bucket name lives in a GLOBAL namespace, so a mistaken
	// argument could reach outside the cluster entirely — ADR-0065's tier 1, where the worst case is
	// unbounded rather than merely bad. Deleting a bucket happens at the vendor, by a human.
	GuardrailBucketCreate GuardrailCode = "bucket.create"
)

// GuardrailInfo describes a guardrail and its current disposition, for inspection through
// `guard list` and `burrow-agent`'s read-only `guard` command (ADR-0020).
type GuardrailInfo struct {
	Code        GuardrailCode `json:"code"`
	Disposition Disposition   `json:"disposition"`
	Description string        `json:"description"`
	// Source reports which tier the effective disposition came from when the guardrail is inspected
	// for something narrower than the whole cluster (ADR-0035 phase 2c, ADR-0085 §2): "name" for the
	// override on the one app or add-on instance asked about, "env" for an environment-specific one,
	// "global" for the global policy, or "default" for the built-in default. It is empty for the
	// global listing.
	Source string `json:"source,omitempty"`
}

// GuardrailScope names what one operation targets, so its disposition can be resolved for exactly
// that thing rather than for everything in an environment (ADR-0085 §1). The zero value is the
// global policy.
type GuardrailScope struct {
	// Env is the environment the operation runs in. Empty means the default environment.
	Env string
	// Name is the ONE thing the operation acts on, for a guardrail that declares itself scopable to
	// one thing: an application for the app.* codes, an add-on instance for the addon.* ones, under
	// the instance's LABEL (ADR-0091 §4) — `burrow-postgres` for an environment's own instance,
	// `analytics` for one an operator added. Never the generated cluster name a later instance
	// carries: a key nobody can read is a key nobody will write, and because a first instance's label
	// IS its name, every disposition written before that record keeps matching. There is one field
	// because the code already says which kind it is, so a caller never has to choose between two
	// that mean the same thing in different circumstances.
	Name string
}

// guardrailDef declares one guardrail: its code, what it gates, and what it can be narrowed to.
//
// Each scope flag is a DECLARATION rather than an inference from the code's name (ADR-0068 §5,
// ADR-0085 §3). EnvScopable used to key on the `app.` prefix, which was a reasonable shorthand when
// app-lifecycle codes were the only ones anyone wanted to scope and became a trap once they were
// not: a correctly-named code silently was not scopable, and inferring capability from a string
// prefix is the kind of shortcut that is invisible until it is wrong. The same shortcut is wrong
// again one axis over — `addon.restore_instance` starts with `addon.` and takes every app on the
// instance back together, so a rule reading "addon.* is per-app" would describe its blast radius
// falsely.
//
// envScoped asserts that the guarded operation CARRIES an environment to scope the lookup to. The
// app-level guardrails gate per-app operations that always name one, so they can be locked down per
// environment — strict prod, permissive staging. dns.* has no environment at all, so declaring it
// env-scopable would promise an override that is never read.
//
// The addon.* codes are mostly NOT env-scoped, and not because of their prefix: an add-on
// operation does name an environment, but the instance it acts on is unique within one and its label
// is what the name tier holds (ADR-0091 §4), so the name tier already draws the per-instance line and
// an env tier over the top of it would be a second way to say the same thing.
//
// `addon.sql` and `addon.attach` are the exceptions, and both are DECLARED ones. ADR-0087 §5 asks for
// a gradient set with `guard set --env <env> addon.sql …` — allow in development, deny in production
// — which is a statement about the ENVIRONMENT rather than about one server, and an operator who
// added a second Postgres instance to an environment should not have to repeat the disposition on it.
// `addon.attach` has that want more strongly (ADR-0095 §2): the env tier is what makes its confirm
// default affordable, since without one the only relief from a sandbox's friction is a cluster-wide
// `allow` that relaxes production to unblock development. ADR-0091 is also what weakened the general
// rule — once an environment may hold several instances, per-instance and per-environment stopped
// being the same statement.
//
// names asserts that the operation acts on exactly ONE thing whose name bounds its effect, and says
// which kind of thing that is — an application or an add-on instance. It is what `--name` refers
// to, which is why one flag suffices: the code decides the kind.
//
// Where an operation names two things — `addon detach <addon> <app>` drops one app's database on
// one instance — the name is the INSTANCE, because that is where the data lives and where the reach
// stops. Scoping such an operation by app would let an operator protect `web` while the identical
// verb wipes `api` on the same instance, which reads as protection and is not.
//
// reach is the fragment a refusal prints when somebody tries to narrow a guardrail further than its
// effect goes ("dns.write cannot be scoped to one thing: <reach>"). It is required of every
// guardrail that names nothing, because "unsupported flag" does not tell an operator anything they
// can act on.
type guardrailDef struct {
	code        GuardrailCode
	description string
	envScoped   bool
	names       guardrailTarget
	reach       string
}

// guardrailTarget is the kind of thing a guardrail's `--name` refers to. The empty value means the
// guardrail cannot be narrowed to one thing at all.
type guardrailTarget string

const (
	targetNothing guardrailTarget = ""
	targetApp     guardrailTarget = "app"
	targetAddon   guardrailTarget = "add-on instance"
)

// knownGuardrails enumerates every configurable guardrail in a stable order with a human
// description, so inspection shows the full set — including unset ones, which read as their
// default disposition.
var knownGuardrails = []guardrailDef{
	{code: GuardrailAppDeploy, description: "deploy a new release of an application", envScoped: true, names: targetApp},
	{code: GuardrailScaleToZero, description: "scale an application to zero replicas", envScoped: true, names: targetApp},
	{code: GuardrailExposePublic, description: "expose an application to the public internet at a hostname", envScoped: true, names: targetApp},
	{code: GuardrailDNSWrite, description: "create or update a public DNS record at a configured provider",
		reach: "a DNS record is a name at a provider outside the cluster, which no single app or add-on owns"},
	{code: GuardrailDNSDelete, description: "delete a public DNS record at a configured provider",
		reach: "a DNS record is a name at a provider outside the cluster, which no single app or add-on owns, and the record may not be one Burrow created"},
	{code: GuardrailAddonInstall, description: "install a building-block add-on (backing service) onto the cluster", names: targetAddon},
	{code: GuardrailAddonRemove, description: "remove an installed add-on from the cluster", names: targetAddon},
	{code: GuardrailAddonAttach, description: "give an app its own database on an add-on instance and wire it in (re-attaching rotates its password)", envScoped: true, names: targetAddon},
	{code: GuardrailAddonDetach, description: "detach an app from an add-on, ending its access to its data (e.g. drop its Postgres login role; the database is kept)", names: targetAddon},
	{code: GuardrailAddonRestore, description: "restore an app's database from a backup, overwriting its live contents", names: targetAddon},
	{code: GuardrailAddonRestoreInstance, description: "rewind a whole Postgres instance to a point in its object-storage repository, taking every app's database on it back together", names: targetAddon},
	{code: GuardrailAddonSQL, description: "run a statement against one app's database on a relational add-on", envScoped: true, names: targetAddon},
	{code: GuardrailAppDelete, description: "delete an app entirely (its workload, routing, and release history)", envScoped: true, names: targetApp},
	{code: GuardrailRollback, description: "roll an application back to its previous release", envScoped: true, names: targetApp},
	{code: GuardrailAutoscale, description: "configure autoscaling for an application", envScoped: true, names: targetApp},
	{code: GuardrailAppRun, description: "run a one-off command inside an application's own image and environment", envScoped: true, names: targetApp},
	{code: GuardrailBucketCreate, description: "create a bucket at an object-storage provider (deleting one is not a Burrow operation at all)",
		reach: "a bucket is storage at a provider outside the cluster, in a global namespace no single app or add-on owns"},
}

// KnownGuardrail reports whether code names a configurable guardrail.
func KnownGuardrail(code GuardrailCode) bool {
	_, ok := lookupGuardrail(code)
	return ok
}

// EnvScopable reports whether a guardrail can be scoped to a named environment (ADR-0035 phase
// 2c), which is a property the guardrail declares (ADR-0068 §5). An unknown code is not scopable.
func EnvScopable(code GuardrailCode) bool {
	g, ok := lookupGuardrail(code)
	return ok && g.envScoped
}

// NameScopable reports whether a guardrail can be scoped to one named thing — `guard set … --name`
// (ADR-0085 §3), which is a property the guardrail declares rather than one read off its code. An
// unknown code is not scopable.
func NameScopable(code GuardrailCode) bool {
	g, ok := lookupGuardrail(code)
	return ok && g.names != targetNothing
}

// GuardrailTarget names the kind of thing a guardrail's `--name` refers to — "app" or "add-on
// instance" — so a message can say what a caller should have named. It is empty for a guardrail
// that cannot be scoped to one thing.
func GuardrailTarget(code GuardrailCode) string {
	g, ok := lookupGuardrail(code)
	if !ok {
		return ""
	}
	return string(g.names)
}

// GuardrailReach describes how far a guarded operation's effect extends, for a refusal that has to
// say why a guardrail cannot be narrowed the way somebody asked. It is empty for a guardrail whose
// effect stops at one named thing.
func GuardrailReach(code GuardrailCode) string {
	g, ok := lookupGuardrail(code)
	if !ok {
		return ""
	}
	return g.reach
}

func lookupGuardrail(code GuardrailCode) (guardrailDef, bool) {
	for _, g := range knownGuardrails {
		if g.code == code {
			return g, true
		}
	}
	return guardrailDef{}, false
}

// Guardrails returns each known guardrail with its effective disposition under the global policy
// (ADR-0020). Use GuardrailsFor to inspect the effective policy for an environment, or for one app
// or add-on instance in it.
func (p Policy) Guardrails() []GuardrailInfo {
	return p.guardrails(GuardrailScope{})
}

// GuardrailsFor returns each known guardrail with its effective disposition for the named scope
// (ADR-0035 phase 2c, ADR-0085 §4): the disposition set for the named app or add-on instance,
// falling back to the environment's, then the global one, then the built-in default. Each entry's
// Source records which tier answered ("name", "env", "global", or "default"), so
// `guard list --env prod --name website` can show why a guardrail reads the way it does without
// anyone reconstructing the fallback chain by hand.
//
// A scope that names nothing — no name, and either an empty env or `prod`, the environment install
// created (ADR-0067 §2) — reproduces the global policy exactly and leaves Source unset, as for
// Guardrails: with one environment there is nothing for a per-environment override to differ FROM,
// so the default environment's policy IS the global policy rather than a second layer over it.
// `guard set --env staging …` then reads as the deliberate divergence it is.
//
// That reasoning stops at the environment tier and deliberately does NOT extend to the name tier.
// There is no baseline app or instance whose policy is the global one, so a name-scoped row is
// never "the same policy seen from closer up" — it exists only to differ from its environment's,
// and it does so in `prod` exactly as much as in `staging`. `prod` therefore appears in the key
// (prod.website.app.run) where it is elided from the environment tier, and naming a thing always
// consults its tier and always reports a Source.
func (p Policy) GuardrailsFor(scope GuardrailScope) []GuardrailInfo {
	return p.guardrails(scope)
}

func (p Policy) guardrails(scope GuardrailScope) []GuardrailInfo {
	named := scope.Name != "" || (scope.Env != "" && scope.Env != DefaultEnvironment)
	out := make([]GuardrailInfo, len(knownGuardrails))
	for i, g := range knownGuardrails {
		disp, source := p.dispositionSource(scope, g.code)
		info := GuardrailInfo{Code: g.code, Disposition: disp, Description: g.description}
		if named {
			info.Source = source
		}
		out[i] = info
	}
	return out
}

// GuardrailError is returned when the control plane declines a dangerous operation or holds
// it for confirmation. It is a structured outcome, not a system failure: the operation was
// understood and deliberately gated. Callers distinguish it with AsGuardrail.
type GuardrailError struct {
	// Operation is the operation that was gated (e.g. "deploy", "scale").
	Operation string
	// Code is the machine-readable guardrail that tripped.
	Code GuardrailCode
	// Message is a human-readable explanation.
	Message string
	// Requested is the value the caller asked for (e.g. the replica count).
	Requested int32
	// Limit is the relevant policy limit, when the code involves one.
	Limit int32
	// NeedsConfirmation is true when the operation was not refused outright but requires
	// explicit confirmation to proceed (disposition confirm). A plain deny leaves it false.
	NeedsConfirmation bool
}

func (e *GuardrailError) Error() string {
	if e.NeedsConfirmation {
		return fmt.Sprintf("guardrail holds %s for confirmation: %s", e.Operation, e.Message)
	}
	return fmt.Sprintf("guardrail refused %s: %s", e.Operation, e.Message)
}

// AsGuardrail reports whether err is (or wraps) a GuardrailError and returns it.
func AsGuardrail(err error) (*GuardrailError, bool) {
	var g *GuardrailError
	if errors.As(err, &g) {
		return g, true
	}
	return nil, false
}

// evaluateDeploy applies the guardrails that gate a deploy: the categorical app.deploy gate
// (allow/confirm/deny — default allow), then the scale-to-zero gate on the resolved replica count.
// Realizes ADR-0007 (explicit deploy is where guardrails live) and ADR-0020 (safe defaults).
//
// The replica CEILING is not evaluated here and is not a guardrail: it is an operational limit
// whose breach is a validation failure, checked before any guardrail runs (ADR-0068 §2).
func (p Policy) evaluateDeploy(scope GuardrailScope, replicas int32, confirmed bool) error {
	if err := p.evaluateGuardrail(scope, "deploy", GuardrailAppDeploy, confirmed,
		fmt.Sprintf("deploying a new release to %s", envName(scope.Env))); err != nil {
		return err
	}
	return p.evaluateReplicas(scope, "deploy", replicas, confirmed)
}

// evaluateReplicas evaluates a requested replica count for op against the policy, given
// whether the caller has confirmed. It returns nil to proceed, or a *GuardrailError that
// either denies the operation or marks it as needing confirmation. It assumes replicas is
// already known non-negative (a negative count is a malformed request, validated
// separately, not a guardrail concern) and already within the replica ceiling, which is an
// operational limit checked ahead of the guardrails (ADR-0068 §2).
//
// Zero is the only count a guardrail has an opinion about: scaling to zero takes an app offline,
// which is a question of what may happen rather than of where a line is drawn.
func (p Policy) evaluateReplicas(scope GuardrailScope, op string, replicas int32, confirmed bool) error {
	if replicas == 0 {
		return p.enforce(scope, op, GuardrailScaleToZero, confirmed, "scaling to zero replicas", 0, 0)
	}
	return nil
}

// evaluateAutoscale evaluates an autoscale request against the policy, given whether the caller has
// confirmed: the app.autoscale guardrail gates the operation itself (allow by default). It returns
// nil to proceed, or a *GuardrailError that denies the operation or marks it as needing
// confirmation.
//
// The autoscaler's MAXIMUM is bounded by the replica ceiling exactly as a manual scale is, and for
// the same reason it is checked before this runs rather than here: the ceiling is an operational
// limit, not a guardrail (ADR-0068 §2).
func (p Policy) evaluateAutoscale(scope GuardrailScope, confirmed bool) error {
	return p.evaluateGuardrail(scope, "autoscale", GuardrailAutoscale, confirmed, "configuring autoscaling")
}

// enforce applies the configured disposition for a tripped guardrail, producing the right
// structured outcome: proceed (nil), confirmation required, or denied. The scope narrows the
// disposition lookup (ADR-0035 phase 2c, ADR-0085 §2): the named app's or add-on instance's
// override wins, then an added environment's, then the global policy; a scope naming nothing but an
// empty env, or `prod`, consults the global policy only.
//
// A refusal NAMES the thing the disposition was set for. "app.deploy is denied for burrowd-cloud"
// is a sentence an operator can act on; a bare "app.deploy is denied", on an install where it is
// allowed for everything else, reads like a bug.
func (p Policy) enforce(scope GuardrailScope, op string, code GuardrailCode, confirmed bool, what string, requested, limit int32) error {
	disp, source := p.dispositionSource(scope, code)
	switch disp {
	case DispositionAllow:
		return nil
	case DispositionConfirm:
		if confirmed {
			return nil
		}
		return &GuardrailError{
			Operation:         op,
			Code:              code,
			Requested:         requested,
			Limit:             limit,
			NeedsConfirmation: true,
			Message:           what + " requires confirmation to proceed" + setFor(scope, source, code),
		}
	default: // DispositionDeny, and any unconfigured/unknown disposition → deny (safe default)
		return &GuardrailError{
			Operation: op,
			Code:      code,
			Requested: requested,
			Limit:     limit,
			Message:   what + " is denied by the current guardrail policy" + setFor(scope, source, code) + relaxHint(scope, source, code),
		}
	}
}

// setFor names the app or add-on instance a disposition was set for, when that is the tier that
// answered. It is empty for an environment-wide, global or default disposition, which is not about
// any one thing and should not read as though it were.
func setFor(scope GuardrailScope, source string, code GuardrailCode) string {
	if source != sourceName {
		return ""
	}
	target := GuardrailTarget(code)
	if target == "" {
		target = "thing"
	}
	return fmt.Sprintf(" for the %s %q", target, scope.Name)
}

// relaxHint names the operator command that would relax a denied guardrail, appended to every
// refusal so the agent has something concrete to relay to the human.
//
// It leads with the per-environment form wherever the code supports one. A deny default is a
// floor, not a fixed setting (ADR-0065 §3): the shape an operator actually wants is a gradient —
// allow in development, confirm in staging, deny in production — and the failure mode this text
// exists to prevent is an operator meeting one refusal and reaching for a global `guard set
// app.delete allow`, relaxing production to unblock a sandbox.
//
// Where the disposition came from one named app or add-on instance, the hint names that thing, so
// an operator relaxing a refusal reaches for the narrow lever they already used rather than the
// wide one (ADR-0085 §1).
//
// A cluster-level code gets the global form instead, and says so, because dns.* and bucket.* declare
// themselves un-scopable: the operations they gate act on things outside the cluster that no app or
// add-on owns. ADR-0065 §3 records that as a real limitation of the decision; naming the reach
// honestly beats printing a flag the caller cannot use.
func relaxHint(scope GuardrailScope, source string, code GuardrailCode) string {
	if source == sourceName {
		return fmt.Sprintf(" — a guardrail is a floor, not a fixed setting: this disposition is set for one %s, and an operator can move it with `burrow guard set --env %s --name %s %s confirm`, which touches nothing else",
			GuardrailTarget(code), envName(scope.Env), scope.Name, code)
	}
	if !EnvScopable(code) {
		// An add-on instance is the narrower lever where the code has one, so the operator is
		// steered there rather than at a --env flag this code cannot use on its own.
		if NameScopable(code) && scope.Name != "" {
			return fmt.Sprintf(" — a guardrail is a floor, not a fixed setting: an operator can relax it for this %s with `burrow guard set --env %s --name %s %s confirm`, which is preferable to `burrow guard set %s confirm` — that applies to every add-on in the cluster",
				GuardrailTarget(code), envName(scope.Env), scope.Name, code, code)
		}
		return fmt.Sprintf(" — an operator can relax it with `burrow guard set %s confirm`, which applies to the whole cluster: %s cannot be scoped to one environment", code, code)
	}
	target := "<env>"
	// `prod` takes the placeholder rather than its own name: its disposition IS the global one
	// (ADR-0067 §2), so pointing the operator at `--env prod` would suggest an override that does
	// not exist as a separate row.
	if scope.Env != "" && scope.Env != DefaultEnvironment {
		target = scope.Env
	}
	return fmt.Sprintf(" — a guardrail is a floor, not a fixed setting: an operator can relax it for one environment with `burrow guard set --env %s %s confirm`, which is preferable to relaxing it everywhere", target, code)
}

// evaluateGuardrail applies a categorical guardrail — one that always trips when its
// operation is attempted, like public exposure — using the configured disposition for the scope.
func (p Policy) evaluateGuardrail(scope GuardrailScope, op string, code GuardrailCode, confirmed bool, what string) error {
	return p.enforce(scope, op, code, confirmed, what, 0, 0)
}

// disposition returns the configured disposition for a guardrail in the named scope, defaulting to
// deny when it is unset or invalid — the safe default (ADR-0020, ADR-0035 phase 2c).
func (p Policy) disposition(scope GuardrailScope, code GuardrailCode) Disposition {
	d, _ := p.dispositionSource(scope, code)
	return d
}

// The tiers a disposition can be answered by, most specific first. They are the values GuardrailInfo
// reports as Source, so `guard list` can say which one answered.
const (
	sourceName    = "name"
	sourceEnv     = "env"
	sourceGlobal  = "global"
	sourceDefault = "default"
)

// dispositionSource resolves a guardrail's effective disposition for a scope and reports which tier
// it came from (ADR-0035 phase 2c, ADR-0085 §2). Most specific wins: the named app or add-on
// instance's key, then the env-prefixed code (e.g. staging.app.delete), then the global code, then
// the deny-when-unset default.
//
// The name tier is consulted only where the guardrail DECLARES that one name bounds its effect
// (ADR-0085 §3), so naming a thing on an operation that reaches further cannot quietly produce a
// narrower answer than the truth. The environment tier is likewise consulted only for a code that
// declares itself env-scopable, so an addon.* operation that carries an environment for its message
// does not acquire an environment tier it never had.
//
// An empty env, or `prod`, skips the env-prefixed lookup and reads the global policy: the default
// environment's policy is the baseline the others diverge from, so a deny that protects production
// is a deny everywhere until an environment opts out of it by name (ADR-0067 §2, ADR-0065 §3). The
// name tier has no such baseline and is therefore consulted in `prod` too — see GuardrailsFor.
func (p Policy) dispositionSource(scope GuardrailScope, code GuardrailCode) (Disposition, string) {
	def, known := lookupGuardrail(code)
	if known && def.names != targetNothing && scope.Name != "" {
		if d, ok := p.Dispositions[namePolicyKey(scope.Env, scope.Name, code)]; ok && d.Valid() {
			return d, sourceName
		}
	}
	if (!known || def.envScoped) && scope.Env != "" && scope.Env != DefaultEnvironment {
		if d, ok := p.Dispositions[envPolicyKey(scope.Env, code)]; ok && d.Valid() {
			return d, sourceEnv
		}
	}
	if d, ok := p.Dispositions[code]; ok && d.Valid() {
		return d, sourceGlobal
	}
	return DispositionDeny, sourceDefault
}

// namePolicyKey composes the key a disposition for one app or one add-on instance is stored under:
// the environment, the name, then the code.
//
// The policy key is a composed STRING and nothing ever parses one — every path constructs the key
// it wants and looks it up, which is why a third tier needs no migration of a table whose key
// column is a TEXT PRIMARY KEY (ADR-0085 §2).
//
// The composition still has to be unambiguous, and it is, for two reasons. App, environment and
// add-on instance names are all DNS-1123 labels, so no name contributes a dot of its own. And this
// key ALWAYS carries an environment segment ahead of the name, so `staging.web.app.deploy` could
// only collide with the environment key of an environment called `staging.web`, which cannot exist.
// That is why `prod` is spelled out here rather than elided the way the environment tier elides it:
// eliding it would put app names and environment names in one flat namespace, where an app called
// `staging` would silently share a key with the `staging` environment. It is also why `guard set
// --name` refuses to run without `--env` — there would be no segment to put the name after.
func namePolicyKey(env, name string, code GuardrailCode) GuardrailCode {
	return GuardrailCode(envName(env) + "." + name + "." + string(code))
}

func envPolicyKey(env string, code GuardrailCode) GuardrailCode {
	return GuardrailCode(env + "." + string(code))
}
