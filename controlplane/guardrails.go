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
	// GuardrailAddonDetach: the operation would detach an app from an add-on — for Postgres,
	// dropping the app's database and role and destroying its data (ADR-0031). Held for
	// confirmation by default. (Attach is not guarded: it provisions, it destroys nothing.)
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
	// Source reports where the effective disposition came from when the guardrail is inspected for a
	// named environment (ADR-0035 phase 2c): "env" for an environment-specific override, "global" for
	// the global policy, or "default" for the built-in default. It is empty for the global listing.
	Source string `json:"source,omitempty"`
}

// guardrailDef declares one guardrail: its code, what it gates, and whether it can be scoped to a
// named environment.
//
// envScoped is a DECLARATION rather than an inference from the code's name (ADR-0068 §5).
// EnvScopable used to key on the `app.` prefix, which was a reasonable shorthand when app-lifecycle
// codes were the only ones anyone wanted to scope and became a trap once they were not: a
// correctly-named code silently was not scopable, and inferring capability from a string prefix is
// the kind of shortcut that is invisible until it is wrong.
//
// What the flag asserts is that the guarded operation CARRIES an environment to scope the lookup
// to. The app-level guardrails gate per-app operations that always name one, so they can be locked
// down per environment — strict prod, permissive staging. dns.* and addon.* are evaluated with an
// empty environment today (the DNS request has no environment at all; an add-on operation names one
// but looks its disposition up globally), so declaring them scopable would promise an override that
// is never read. Widening one is now a change to this line plus the lookup at its call site, rather
// than a rename.
type guardrailDef struct {
	code        GuardrailCode
	description string
	envScoped   bool
}

// knownGuardrails enumerates every configurable guardrail in a stable order with a human
// description, so inspection shows the full set — including unset ones, which read as their
// default disposition.
var knownGuardrails = []guardrailDef{
	{code: GuardrailAppDeploy, description: "deploy a new release of an application", envScoped: true},
	{code: GuardrailScaleToZero, description: "scale an application to zero replicas", envScoped: true},
	{code: GuardrailExposePublic, description: "expose an application to the public internet at a hostname", envScoped: true},
	{code: GuardrailDNSWrite, description: "create or update a public DNS record at a configured provider"},
	{code: GuardrailDNSDelete, description: "delete a public DNS record at a configured provider"},
	{code: GuardrailAddonInstall, description: "install a building-block add-on (backing service) onto the cluster"},
	{code: GuardrailAddonRemove, description: "remove an installed add-on from the cluster"},
	{code: GuardrailAddonDetach, description: "detach an app from an add-on, destroying its data (e.g. drop its Postgres database)"},
	{code: GuardrailAddonRestore, description: "restore an app's database from a backup, overwriting its live contents"},
	{code: GuardrailAddonRestoreInstance, description: "rewind a whole Postgres instance to a point in its object-storage repository, taking every app's database on it back together"},
	{code: GuardrailAppDelete, description: "delete an app entirely (its workload, routing, and release history)", envScoped: true},
	{code: GuardrailRollback, description: "roll an application back to its previous release", envScoped: true},
	{code: GuardrailAutoscale, description: "configure autoscaling for an application", envScoped: true},
	{code: GuardrailAppRun, description: "run a one-off command inside an application's own image and environment", envScoped: true},
	{code: GuardrailBucketCreate, description: "create a bucket at an object-storage provider (deleting one is not a Burrow operation at all)"},
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

func lookupGuardrail(code GuardrailCode) (guardrailDef, bool) {
	for _, g := range knownGuardrails {
		if g.code == code {
			return g, true
		}
	}
	return guardrailDef{}, false
}

// Guardrails returns each known guardrail with its effective disposition under the global policy
// (ADR-0020). Use GuardrailsFor to inspect a named environment's effective policy.
func (p Policy) Guardrails() []GuardrailInfo {
	return p.guardrails("")
}

// GuardrailsFor returns each known guardrail with its effective disposition for the named
// environment (ADR-0035 phase 2c): the disposition under the env-prefixed override, falling back to
// the global override, then the built-in default. Each entry's Source records where the effective
// disposition came from ("env", "global", or "default") so `guard list --env` can show which
// guardrails are env-specific and which are inherited. An empty env, or `prod` — the environment
// install created (ADR-0067 §2) — reproduces the global policy exactly and leaves Source unset, as
// for Guardrails: with one environment there is nothing for a per-environment override to differ
// FROM, so the default environment's policy IS the global policy rather than a second layer over
// it. `guard set --env staging …` then reads as the deliberate divergence it is.
func (p Policy) GuardrailsFor(env string) []GuardrailInfo {
	return p.guardrails(env)
}

func (p Policy) guardrails(env string) []GuardrailInfo {
	named := env != "" && env != DefaultEnvironment
	out := make([]GuardrailInfo, len(knownGuardrails))
	for i, g := range knownGuardrails {
		disp, source := p.dispositionSource(env, g.code)
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
func (p Policy) evaluateDeploy(env string, replicas int32, confirmed bool) error {
	if err := p.evaluateGuardrail(env, "deploy", GuardrailAppDeploy, confirmed,
		fmt.Sprintf("deploying a new release to %s", envName(env))); err != nil {
		return err
	}
	return p.evaluateReplicas(env, "deploy", replicas, confirmed)
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
func (p Policy) evaluateReplicas(env, op string, replicas int32, confirmed bool) error {
	if replicas == 0 {
		return p.enforce(env, op, GuardrailScaleToZero, confirmed, "scaling to zero replicas", 0, 0)
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
func (p Policy) evaluateAutoscale(env string, confirmed bool) error {
	return p.evaluateGuardrail(env, "autoscale", GuardrailAutoscale, confirmed, "configuring autoscaling")
}

// enforce applies the configured disposition for a tripped guardrail, producing the right
// structured outcome: proceed (nil), confirmation required, or denied. The env scopes the
// disposition lookup (ADR-0035 phase 2c): an added environment's override wins, falling back to the
// global policy; an empty env, or `prod`, consults the global policy only.
func (p Policy) enforce(env, op string, code GuardrailCode, confirmed bool, what string, requested, limit int32) error {
	switch p.disposition(env, code) {
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
			Message:           what + " requires confirmation to proceed",
		}
	default: // DispositionDeny, and any unconfigured/unknown disposition → deny (safe default)
		return &GuardrailError{
			Operation: op,
			Code:      code,
			Requested: requested,
			Limit:     limit,
			Message:   what + " is denied by the current guardrail policy" + relaxHint(env, code),
		}
	}
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
// A cluster-level code gets the global form instead, and says so, because dns.*, addon.* and the
// rest declare themselves un-scopable: the operations they gate are evaluated with no environment,
// so an override for one would never be read. ADR-0065 §3 records that as a real limitation of the
// decision; naming the reach honestly beats printing a `--env` flag the caller cannot use.
func relaxHint(env string, code GuardrailCode) string {
	if !EnvScopable(code) {
		return fmt.Sprintf(" — an operator can relax it with `burrow guard set %s confirm`, which applies to the whole cluster: %s cannot be scoped to one environment", code, code)
	}
	target := "<env>"
	// `prod` takes the placeholder rather than its own name: its disposition IS the global one
	// (ADR-0067 §2), so pointing the operator at `--env prod` would suggest an override that does
	// not exist as a separate row.
	if env != "" && env != DefaultEnvironment {
		target = env
	}
	return fmt.Sprintf(" — a guardrail is a floor, not a fixed setting: an operator can relax it for one environment with `burrow guard set --env %s %s confirm`, which is preferable to relaxing it everywhere", target, code)
}

// evaluateGuardrail applies a categorical guardrail — one that always trips when its
// operation is attempted, like public exposure — using the configured disposition for env.
func (p Policy) evaluateGuardrail(env, op string, code GuardrailCode, confirmed bool, what string) error {
	return p.enforce(env, op, code, confirmed, what, 0, 0)
}

// disposition returns the configured disposition for a guardrail in the named environment,
// defaulting to deny when it is unset or invalid — the safe default (ADR-0020, ADR-0035 phase 2c).
func (p Policy) disposition(env string, code GuardrailCode) Disposition {
	d, _ := p.dispositionSource(env, code)
	return d
}

// dispositionSource resolves a guardrail's effective disposition for env and reports where it came
// from (ADR-0035 phase 2c). For a named environment it first consults the env-prefixed code
// (e.g. staging.app.delete), so an environment can lock down or relax an operation independently;
// absent that, it falls back to the global code, then to the deny-when-unset default. An empty env,
// or `prod`, skips the env-prefixed lookup and reads the global policy: the default environment's
// policy is the baseline the others diverge from, so a deny that protects production is a deny
// everywhere until an environment opts out of it by name (ADR-0067 §2, ADR-0065 §3).
func (p Policy) dispositionSource(env string, code GuardrailCode) (Disposition, string) {
	if env != "" && env != DefaultEnvironment {
		if d, ok := p.Dispositions[GuardrailCode(env+"."+string(code))]; ok && d.Valid() {
			return d, "env"
		}
	}
	if d, ok := p.Dispositions[code]; ok && d.Valid() {
		return d, "global"
	}
	return DispositionDeny, "default"
}
