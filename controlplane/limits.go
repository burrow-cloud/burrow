// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// An operational limit is a bound a human sets, and it is deliberately NOT a guardrail
// (ADR-0068 §2). A guardrail answers "what happens when this is attempted"; a limit answers
// "where the line is". Merging them is what let `guard set app.replica_ceiling allow` turn the
// replica ceiling off, and a limit you can turn off is not a limit — so a limit has no
// disposition, nothing to relax, and no confirm path. Exceeding one is a validation failure.
//
// Limits are configuration, held in two operator-owned tiers (ADR-0068 §1): CLUSTER
// configuration, which reaches Burrow's behaviour everywhere, and ENVIRONMENT configuration,
// which reaches it in one environment. The test for which tier a limit belongs to is whether a
// sensible answer can differ per environment: a replica ceiling can, the retention of a build
// Job's logs cannot. Both are set only with the admin kubeconfig and neither is reachable from
// the agent surface (ADR-0068 §4) — a bound the agent can raise is not a bound.

// LimitCode identifies an operational limit: it is both the key an operator sets the limit under
// and the machine-readable reason it appears in a refusal, so an agent can branch on the cause
// rather than parse prose (ADR-0068 §2).
type LimitCode string

const (
	// LimitReplicaCeiling is the largest replica count a deploy, a scale, or an autoscaler's
	// maximum may ask for. Environment-scoped: a production ceiling and a development ceiling are
	// exactly the case that motivates limits at all (ADR-0068 §6).
	LimitReplicaCeiling LimitCode = "app.replica_ceiling"
)

// LimitKind is the shape of a limit's value, which decides how it is parsed, formatted, and
// bounds-checked. Two kinds because ADR-0068 §6's occupants need two: the replica ceiling is a
// count, while build-Job retention, add-on metric retention, and the status thresholds are
// durations. Values are stored in their canonical TEXT form ("50", "72h"), so what an operator
// reads out of the table is what they typed.
type LimitKind string

const (
	// LimitKindCount is a positive whole number.
	LimitKindCount LimitKind = "count"
	// LimitKindDuration is a Go duration string ("30s", "72h").
	LimitKindDuration LimitKind = "duration"
)

// The tier an effective limit value came from, reported next to it so a refusal can name where
// the line was drawn and `cluster config list` can show what is set versus inherited.
const (
	LimitScopeEnvironment = "environment"
	LimitScopeCluster     = "cluster"
	LimitScopeDefault     = "default"
)

// limitDef declares one operational limit: what it bounds, the shape of its value, the tier it may
// be set at, and the built-in value that applies when no operator has set one.
//
// EnvScoped is a DECLARATION, not an inference from the code's prefix (ADR-0068 §5). Inferring
// capability from a string prefix is the shortcut that is invisible until it is wrong, and it was:
// `EnvScopable` keyed on `app.`, so a correctly-named code silently was not scopable.
type limitDef struct {
	code        LimitCode
	kind        LimitKind
	description string
	envScoped   bool
	// def, min, and max are in the kind's base unit: a plain number for a count, nanoseconds (a
	// time.Duration) for a duration. min and max bound what an operator may set, so a limit cannot
	// be set to a value the code downstream of it cannot hold.
	def, min, max int64
}

// knownLimits enumerates every operational limit in a stable order, the way knownGuardrails
// enumerates every guardrail, so a listing shows the full set including the ones nobody has set —
// which read as their built-in default. ADR-0068 §6 names the occupants that join the replica
// ceiling here as their constants are moved onto this mechanism.
var knownLimits = []limitDef{
	{
		code:        LimitReplicaCeiling,
		kind:        LimitKindCount,
		description: "the largest replica count a deploy, a scale, or an autoscaler's maximum may ask for",
		envScoped:   true,
		// 50 is the ceiling that was compiled into DefaultPolicy before this became configuration,
		// so an install that never sets one behaves exactly as it did (ADR-0068 §2).
		def: 50,
		min: 1,
		// A replica count is an int32 everywhere below this, so the ceiling cannot exceed one.
		max: math.MaxInt32,
	},
}

// KnownLimit reports whether code names a configurable operational limit.
func KnownLimit(code LimitCode) bool {
	_, ok := lookupLimit(code)
	return ok
}

// EnvScopableLimit reports whether a limit can be set for one named environment (ADR-0068 §5),
// which is a property the limit declares. An unknown code is not scopable.
func EnvScopableLimit(code LimitCode) bool {
	d, ok := lookupLimit(code)
	return ok && d.envScoped
}

// LimitCodes returns every known limit code in listing order, so a caller can name the set without
// reaching into the catalogue.
func LimitCodes() []LimitCode {
	out := make([]LimitCode, len(knownLimits))
	for i, d := range knownLimits {
		out[i] = d.code
	}
	return out
}

func lookupLimit(code LimitCode) (limitDef, bool) {
	for _, d := range knownLimits {
		if d.code == code {
			return d, true
		}
	}
	return limitDef{}, false
}

// parse converts a stored or operator-supplied text value into the limit's base unit, rejecting
// anything outside the declared bounds. The error is written for a human at the operator CLI: it
// names the unit and the range, because the only way past a limit is someone setting a better one.
func (d limitDef) parse(value string) (int64, error) {
	v := strings.TrimSpace(value)
	switch d.kind {
	case LimitKindDuration:
		dur, err := time.ParseDuration(v)
		if err != nil {
			return 0, fmt.Errorf("%q is not a duration (want e.g. %q)", value, d.format(d.def))
		}
		if int64(dur) < d.min || int64(dur) > d.max {
			return 0, fmt.Errorf("%s is outside the permitted range %s to %s", d.format(int64(dur)), d.format(d.min), d.format(d.max))
		}
		return int64(dur), nil
	default:
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%q is not a whole number", value)
		}
		if n < d.min || n > d.max {
			return 0, fmt.Errorf("%d is outside the permitted range %d to %d", n, d.min, d.max)
		}
		return n, nil
	}
}

// format renders a value in the limit's base unit back to its canonical text form — the form
// stored, listed, and quoted in a refusal.
func (d limitDef) format(v int64) string {
	if d.kind == LimitKindDuration {
		return time.Duration(v).String()
	}
	return strconv.FormatInt(v, 10)
}

// LimitInfo describes an operational limit and its effective value, for inspection through
// `burrow cluster config list` (ADR-0068 §4).
type LimitInfo struct {
	Code        LimitCode `json:"code"`
	Value       string    `json:"value"`
	Description string    `json:"description"`
	Kind        LimitKind `json:"kind"`
	// Scope reports which tier the effective value came from: "environment", "cluster", or
	// "default" (the built-in). It is what tells an operator whether the value they are reading is
	// one someone set or one nobody has.
	Scope string `json:"scope"`
	// EnvScoped reports whether a value may be set for one environment at all (ADR-0068 §5).
	EnvScoped bool `json:"env_scoped"`
	// Default is the built-in value, so a listing shows what the limit reverts to.
	Default string `json:"default"`
}

// OperationalConfig is the operator-set configuration the control plane resolves operational
// limits from (ADR-0068 §1). It is the middle and outer tiers together: app config and secrets
// (ADR-0028) belong to the developer and reach the pod, and are a different thing entirely.
type OperationalConfig struct {
	// Values holds every stored limit value keyed by the code it was set under — the bare code for
	// a cluster value and `<env>.<code>` for an environment one. That is the same keying guardrail
	// dispositions already use, so the two configuration surfaces resolve the same way and an
	// operator learns one rule (ADR-0068 §3).
	Values map[LimitCode]string
}

// With returns a copy of the configuration with code's value set, leaving the receiver unchanged —
// the basis for `cluster config set` and for composing configurations in tests.
func (c OperationalConfig) With(code LimitCode, value string) OperationalConfig {
	next := make(map[LimitCode]string, len(c.Values)+1)
	for k, v := range c.Values {
		next[k] = v
	}
	next[code] = value
	return OperationalConfig{Values: next}
}

// Limits returns every known limit with its effective value for the named environment, each
// marking which tier that value came from. An empty env, or `prod`, reports the cluster tier and
// the built-in defaults; a named environment reports its own values where it has them.
func (c OperationalConfig) Limits(env string) []LimitInfo {
	out := make([]LimitInfo, len(knownLimits))
	for i, d := range knownLimits {
		v, scope := c.resolve(env, d)
		out[i] = LimitInfo{
			Code:        d.code,
			Value:       d.format(v),
			Description: d.description,
			Kind:        d.kind,
			Scope:       scope,
			EnvScoped:   d.envScoped,
			Default:     d.format(d.def),
		}
	}
	return out
}

// Count returns the effective value of a count limit for env and the tier it came from. An unknown
// code, or one of another kind, yields zero at the default scope: passing one is a programming
// error, not an operator's.
func (c OperationalConfig) Count(env string, code LimitCode) (int64, string) {
	d, ok := lookupLimit(code)
	if !ok || d.kind != LimitKindCount {
		return 0, LimitScopeDefault
	}
	return c.resolve(env, d)
}

// Duration returns the effective value of a duration limit for env and the tier it came from — the
// sibling of Count for ADR-0068 §6's duration occupants (build-Job retention, add-on metric
// retention, the status thresholds). An unknown code, or one of another kind, yields zero at the
// default scope.
func (c OperationalConfig) Duration(env string, code LimitCode) (time.Duration, string) {
	d, ok := lookupLimit(code)
	if !ok || d.kind != LimitKindDuration {
		return 0, LimitScopeDefault
	}
	v, scope := c.resolve(env, d)
	return time.Duration(v), scope
}

// ReplicaCeiling returns the effective replica ceiling for env and the tier it came from
// (ADR-0068 §6). It is an int32 because every replica count below it is.
func (c OperationalConfig) ReplicaCeiling(env string) (int32, string) {
	n, scope := c.Count(env, LimitReplicaCeiling)
	return int32(n), scope
}

// resolve applies ADR-0068 §3's order: the environment's value wins; absent one, the cluster
// value; absent that, the built-in default. It is the order guardrail dispositions already use for
// `<env>.<code>` and `<code>`.
//
// An empty env, or `prod`, skips the environment lookup and reads the cluster tier, for the same
// reason dispositionSource does (ADR-0067 §2): `prod` is the environment install created and the
// baseline every other environment diverges from, so its value IS the cluster value rather than a
// second layer over it.
//
// A stored value that no longer parses — a limit whose bounds were narrowed, a hand-edited row —
// falls through to the next tier rather than failing the operation. Values are validated on the
// way IN, which is where an operator is present to be told; a read happens on the deploy path,
// where the honest answer is the bound that still holds.
func (c OperationalConfig) resolve(env string, d limitDef) (int64, string) {
	if env != "" && env != DefaultEnvironment && d.envScoped {
		if v, ok := c.Values[LimitCode(env+"."+string(d.code))]; ok {
			if n, err := d.parse(v); err == nil {
				return n, LimitScopeEnvironment
			}
		}
	}
	if v, ok := c.Values[d.code]; ok {
		if n, err := d.parse(v); err == nil {
			return n, LimitScopeCluster
		}
	}
	return d.def, LimitScopeDefault
}

// LimitError is returned when a request exceeds an operational limit (ADR-0068 §2). It is a
// structured VALIDATION failure, not a policy decision: unlike a GuardrailError there is no
// disposition behind it, nothing to relax, and no confirmation that opens it — the only way past
// it is a human raising the bound with the operator CLI. Callers distinguish it with AsLimit.
type LimitError struct {
	// Operation is the operation that was refused (e.g. "deploy", "scale").
	Operation string
	// Code is the machine-readable limit that was exceeded.
	Code LimitCode
	// Requested is the value the caller asked for.
	Requested int32
	// Limit is the effective bound the request exceeded.
	Limit int32
	// Scope is the tier the effective bound came from: "environment", "cluster", or "default".
	Scope string
	// Env is the environment the request targeted, when it named one.
	Env string
	// Message is a human-readable explanation, naming the limit, the tier it came from, and the
	// operator command that changes it.
	Message string
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("%s refused: %s", e.Operation, e.Message)
}

// AsLimit reports whether err is (or wraps) a LimitError and returns it.
func AsLimit(err error) (*LimitError, bool) {
	var l *LimitError
	if errors.As(err, &l) {
		return l, true
	}
	return nil, false
}

// checkReplicaCeiling refuses op when requested exceeds the effective replica ceiling for env
// (ADR-0068 §2). what names what the number is in the caller's terms ("replicas", "a maximum of
// %d replicas"), so the refusal reads the same whether it came from a deploy, a scale, or an
// autoscaler's band. It returns nil when the request is within the bound.
func (c OperationalConfig) checkReplicaCeiling(env, op, what string, requested int32) error {
	ceiling, scope := c.ReplicaCeiling(env)
	if requested <= ceiling {
		return nil
	}
	return &LimitError{
		Operation: op,
		Code:      LimitReplicaCeiling,
		Requested: requested,
		Limit:     ceiling,
		Scope:     scope,
		Env:       env,
		Message: fmt.Sprintf("%s exceeds the replica ceiling of %d, %s. %s",
			what, ceiling, scopePhrase(scope, env), raiseHint(LimitReplicaCeiling, env)),
	}
}

// scopePhrase names where an effective limit came from, so a refusal says which of the two
// operator tiers drew the line rather than only that a line exists.
func scopePhrase(scope, env string) string {
	switch scope {
	case LimitScopeEnvironment:
		return fmt.Sprintf("set for environment %q", env)
	case LimitScopeCluster:
		return "set for the cluster"
	default:
		return "the built-in default (no operator has set one)"
	}
}

// raiseHint names the operator command that would raise a limit, appended to every refusal so the
// agent has something concrete to relay to the human. It leads with the per-environment form
// wherever the limit supports one, for the reason relaxHint does: the shape an operator actually
// wants is a gradient, and the failure mode this text exists to prevent is a refusal in a sandbox
// being answered by raising production's ceiling with it.
func raiseHint(code LimitCode, env string) string {
	if !EnvScopableLimit(code) {
		return fmt.Sprintf("A limit is a bound a human sets, not a guardrail that can be dispositioned away: raise it with `burrow cluster config set %s <value>`, which applies to the whole cluster.", code)
	}
	target := env
	// `prod` takes the placeholder rather than its own name: its value IS the cluster value
	// (ADR-0067 §2), so pointing the operator at `--env prod` would suggest a row that does not exist.
	if target == "" || target == DefaultEnvironment {
		target = "<env>"
	}
	return fmt.Sprintf("A limit is a bound a human sets, not a guardrail that can be dispositioned away: raise it for one environment with `burrow cluster config set --env %s %s <value>`, which is preferable to raising it everywhere.", target, code)
}
