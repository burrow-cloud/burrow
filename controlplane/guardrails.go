// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"errors"
	"fmt"
	"strings"
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
	// GuardrailReplicaCeiling: the requested replica count exceeds Policy.MaxReplicas.
	GuardrailReplicaCeiling GuardrailCode = "app.replica_ceiling"
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
	// `guard set dns.delete ...` — cluster-wide, because EnvScopable keys on the `app.` prefix, so
	// unlike app.delete this one cannot be relaxed for a single environment (ADR-0068 proposes
	// widening that prefix).
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
	// (GuardrailReplicaCeiling). An operator can raise it to confirm or deny per environment, e.g.
	// deny in prod so only a human sets the scaling shape there.
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

// knownGuardrails enumerates every configurable guardrail in a stable order with a human
// description, so inspection shows the full set — including unset ones, which read as their
// default disposition.
var knownGuardrails = []GuardrailInfo{
	{Code: GuardrailAppDeploy, Description: "deploy a new release of an application"},
	{Code: GuardrailReplicaCeiling, Description: "deploy or scale above the replica ceiling"},
	{Code: GuardrailScaleToZero, Description: "scale an application to zero replicas"},
	{Code: GuardrailExposePublic, Description: "expose an application to the public internet at a hostname"},
	{Code: GuardrailDNSWrite, Description: "create or update a public DNS record at a configured provider"},
	{Code: GuardrailDNSDelete, Description: "delete a public DNS record at a configured provider"},
	{Code: GuardrailAddonInstall, Description: "install a building-block add-on (backing service) onto the cluster"},
	{Code: GuardrailAddonRemove, Description: "remove an installed add-on from the cluster"},
	{Code: GuardrailAddonDetach, Description: "detach an app from an add-on, destroying its data (e.g. drop its Postgres database)"},
	{Code: GuardrailAddonRestore, Description: "restore an app's database from a backup, overwriting its live contents"},
	{Code: GuardrailAppDelete, Description: "delete an app entirely (its workload, routing, and release history)"},
	{Code: GuardrailRollback, Description: "roll an application back to its previous release"},
	{Code: GuardrailAutoscale, Description: "configure autoscaling for an application"},
	{Code: GuardrailAppRun, Description: "run a one-off command inside an application's own image and environment"},
	{Code: GuardrailBucketCreate, Description: "create a bucket at an object-storage provider (deleting one is not a Burrow operation at all)"},
}

// KnownGuardrail reports whether code names a configurable guardrail.
func KnownGuardrail(code GuardrailCode) bool {
	for _, g := range knownGuardrails {
		if g.Code == code {
			return true
		}
	}
	return false
}

// EnvScopable reports whether a guardrail can be scoped to a named environment (ADR-0035 phase
// 2c). The app-level guardrails (app.*) gate per-app operations that always carry an environment,
// so they can be locked down per environment — strict prod, permissive staging. The cluster-level
// guardrails (addon.*, dns.*) gate cluster-wide operations that are not env-scoped (installing an
// add-on or writing DNS affects the whole cluster), so they are only ever set globally.
func EnvScopable(code GuardrailCode) bool {
	return strings.HasPrefix(string(code), "app.")
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
		disp, source := p.dispositionSource(env, g.Code)
		info := GuardrailInfo{Code: g.Code, Disposition: disp, Description: g.Description}
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

// evaluateDeploy applies the guardrails that gate a deploy: first the categorical app.deploy
// gate (allow/confirm/deny — default allow), then the replica ceiling bound. The categorical
// gate is checked first so a deny/confirm on deploying at all takes precedence; a within-policy
// deploy still cannot exceed the replica ceiling. Realizes ADR-0007 (explicit deploy is where
// guardrails live) and ADR-0020 (safe defaults).
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
// separately, not a guardrail concern).
func (p Policy) evaluateReplicas(env, op string, replicas int32, confirmed bool) error {
	if replicas == 0 {
		return p.enforce(env, op, GuardrailScaleToZero, confirmed, "scaling to zero replicas", 0, 0)
	}
	if replicas > p.MaxReplicas {
		return p.enforce(env, op, GuardrailReplicaCeiling, confirmed,
			fmt.Sprintf("requested %d replicas exceeds the policy ceiling of %d", replicas, p.MaxReplicas),
			replicas, p.MaxReplicas)
	}
	return nil
}

// evaluateAutoscale evaluates an autoscale request against the policy, given whether the caller has
// confirmed. It applies two guardrails in order: the app.autoscale guardrail gates the operation
// itself (allow by default), and the app.replica_ceiling guardrail bounds the autoscaler's max the
// same way it bounds a manual scale — a max above the ceiling is denied exactly like scaling above
// it (ADR-0006). It returns nil to proceed, or the first *GuardrailError that denies the operation
// or marks it as needing confirmation. The spec is assumed already validated (min >= 1, max >= min),
// so the ceiling is the only replica bound a guardrail concerns itself with here.
func (p Policy) evaluateAutoscale(env string, spec AutoscaleSpec, confirmed bool) error {
	if err := p.evaluateGuardrail(env, "autoscale", GuardrailAutoscale, confirmed, "configuring autoscaling"); err != nil {
		return err
	}
	if spec.MaxReplicas > p.MaxReplicas {
		return p.enforce(env, "autoscale", GuardrailReplicaCeiling, confirmed,
			fmt.Sprintf("requested max of %d replicas exceeds the policy ceiling of %d", spec.MaxReplicas, p.MaxReplicas),
			spec.MaxReplicas, p.MaxReplicas)
	}
	return nil
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
// A cluster-level code gets the global form instead, and says so, because EnvScopable keys on the
// `app.` prefix: dns.*, addon.* and the rest are all-or-nothing today. ADR-0065 §3 records that as
// a real limitation of the decision and ADR-0068 proposes widening the prefix; until then, naming
// the reach honestly beats printing a `--env` flag the caller cannot use.
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
