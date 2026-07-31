// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestLimitResolutionOrder pins ADR-0068 §3: the environment's value wins; absent one, the cluster
// value; absent that, the built-in default. It is the same order guardrail dispositions use, so an
// operator learns one rule rather than two.
func TestLimitResolutionOrder(t *testing.T) {
	c := OperationalConfig{}.
		With(LimitReplicaCeiling, "80").
		With(LimitCode("staging."+string(LimitReplicaCeiling)), "200")

	cases := []struct {
		name      string
		env       string
		want      int32
		wantScope string
	}{
		{"the environment's own value wins", "staging", 200, LimitScopeEnvironment},
		{"an environment with none falls back to the cluster", "dev", 80, LimitScopeCluster},
		{"an empty environment reads the cluster value", "", 80, LimitScopeCluster},
		{"the default environment prod IS the cluster value", DefaultEnvironment, 80, LimitScopeCluster},
	}
	for _, c2 := range cases {
		t.Run(c2.name, func(t *testing.T) {
			got, scope := c.ReplicaCeiling(c2.env)
			if got != c2.want || scope != c2.wantScope {
				t.Errorf("ReplicaCeiling(%q) = (%d, %q), want (%d, %q)", c2.env, got, scope, c2.want, c2.wantScope)
			}
		})
	}

	// With nothing set anywhere, every environment reads the built-in default.
	empty := OperationalConfig{}
	if got, scope := empty.ReplicaCeiling("staging"); got != 50 || scope != LimitScopeDefault {
		t.Errorf("unset ceiling = (%d, %q), want (50, default)", got, scope)
	}
}

// TestLimitIsRaisableNotJustLowerable is the correction ADR-0068 exists for. The guardrail this
// replaced could be turned OFF (`guard set app.replica_ceiling allow`) but never turned UP, which
// left an operator who legitimately needed 80 replicas with one option: remove the limit.
func TestLimitIsRaisableNotJustLowerable(t *testing.T) {
	raised := OperationalConfig{}.With(LimitReplicaCeiling, "80")
	if err := raised.checkReplicaCeiling("", "scale", "80 replicas", 80); err != nil {
		t.Errorf("80 replicas under a ceiling of 80 = %v, want allowed", err)
	}
	if err := raised.checkReplicaCeiling("", "scale", "81 replicas", 81); err == nil {
		t.Errorf("81 replicas under a ceiling of 80 should be refused")
	}

	lowered := OperationalConfig{}.With(LimitReplicaCeiling, "3")
	if err := lowered.checkReplicaCeiling("", "scale", "4 replicas", 4); err == nil {
		t.Errorf("4 replicas under a ceiling of 3 should be refused")
	}
}

// TestLimitRefusalNamesTheLimitScopeAndRemedy covers what a refused caller is told (ADR-0068 §2):
// the limit, the tier the effective bound came from, and that a human with the operator CLI can
// change it. The tier matters because an operator who cannot see WHERE the line was drawn cannot
// tell a cluster-wide bound from one their environment set.
func TestLimitRefusalNamesTheLimitScopeAndRemedy(t *testing.T) {
	envSet := OperationalConfig{}.With(LimitCode("staging."+string(LimitReplicaCeiling)), "10")
	err := envSet.checkReplicaCeiling("staging", "scale", "99 replicas", 99)
	l, ok := AsLimit(err)
	if !ok {
		t.Fatalf("err = %v, want a LimitError", err)
	}
	if l.Code != LimitReplicaCeiling || l.Requested != 99 || l.Limit != 10 || l.Scope != LimitScopeEnvironment {
		t.Errorf("limit error = %+v, want app.replica_ceiling requested 99 limit 10 scope environment", l)
	}
	for _, want := range []string{"99 replicas", "replica ceiling of 10", `set for environment "staging"`, "burrow cluster config set --env staging app.replica_ceiling"} {
		if !strings.Contains(l.Message, want) {
			t.Errorf("refusal %q missing %q", l.Message, want)
		}
	}

	// A cluster value says so, and an unset one says nobody has set a bound at all — which is a
	// different fact and a more useful one.
	clusterSet := OperationalConfig{}.With(LimitReplicaCeiling, "10")
	l, _ = AsLimit(clusterSet.checkReplicaCeiling("", "deploy", "99 replicas", 99))
	if !strings.Contains(l.Message, "set for the cluster") {
		t.Errorf("cluster refusal %q should say where the bound came from", l.Message)
	}
	l, _ = AsLimit(OperationalConfig{}.checkReplicaCeiling("", "deploy", "99 replicas", 99))
	if !strings.Contains(l.Message, "built-in default") {
		t.Errorf("unset refusal %q should say the bound is the built-in default", l.Message)
	}
	// With no named environment there is no name to print, so the placeholder keeps the --env shape.
	if !strings.Contains(l.Message, "--env <env>") {
		t.Errorf("unscoped refusal %q should still lead with the per-environment form", l.Message)
	}
}

// TestLimitRefusalIsNotAGuardrailRefusal pins the distinction the whole record turns on. A
// GuardrailError can carry NeedsConfirmation and names a disposition an operator may relax; a
// LimitError does neither, because a bound is not a policy decision (ADR-0068 §2).
func TestLimitRefusalIsNotAGuardrailRefusal(t *testing.T) {
	err := OperationalConfig{}.checkReplicaCeiling("", "scale", "99 replicas", 99)
	if _, ok := AsGuardrail(err); ok {
		t.Errorf("a limit refusal must not read as a guardrail refusal: %v", err)
	}
	if _, ok := AsLimit(err); !ok {
		t.Fatalf("err = %v, want a LimitError", err)
	}
}

// TestLimitEnvScopingIsDeclared pins ADR-0068 §5 on the limit side: scopability is a property the
// limit declares, not one read off the code's `app.` prefix. A limit that declares itself
// cluster-wide ignores an environment key rather than honouring one nothing should have written.
func TestLimitEnvScopingIsDeclared(t *testing.T) {
	if !EnvScopableLimit(LimitReplicaCeiling) {
		t.Errorf("the replica ceiling should be environment-scoped (ADR-0068 §6)")
	}
	if EnvScopableLimit(LimitCode("app.not_a_real_limit")) {
		t.Errorf("an unknown code beginning with `app.` is scopable, so scopability is still inferred from the name")
	}
	if KnownLimit(LimitCode("app.not_a_real_limit")) {
		t.Errorf("an unknown code should not be a known limit")
	}

	// Prove the declaration is what the resolution consults, by resolving a cluster-only limit
	// against a stub definition rather than by trusting the catalogue's current membership.
	clusterOnly := limitDef{code: "test.cluster_only", kind: LimitKindCount, def: 7, min: 1, max: 100}
	c := OperationalConfig{Values: map[LimitCode]string{"staging.test.cluster_only": "42"}}
	if v, scope := c.resolve("staging", clusterOnly); v != 7 || scope != LimitScopeDefault {
		t.Errorf("a cluster-only limit resolved an environment value: got (%d, %q), want (7, default)", v, scope)
	}
}

// TestLimitValueParsing covers the kinds a limit value can take and the bounds each is held to. A
// value is validated on the way IN, where an operator is present to be told what is wrong.
func TestLimitValueParsing(t *testing.T) {
	count := limitDef{code: "test.count", kind: LimitKindCount, def: 10, min: 1, max: 100}
	dur := limitDef{code: "test.duration", kind: LimitKindDuration, def: int64(30 * time.Second), min: int64(time.Second), max: int64(time.Hour)}

	if v, err := count.parse(" 42 "); err != nil || v != 42 {
		t.Errorf("count.parse(\" 42 \") = (%d, %v), want (42, nil)", v, err)
	}
	for _, bad := range []string{"lots", "", "0", "101", "1.5"} {
		if _, err := count.parse(bad); err == nil {
			t.Errorf("count.parse(%q) should be refused", bad)
		}
	}

	if v, err := dur.parse("90s"); err != nil || v != int64(90*time.Second) {
		t.Errorf("duration.parse(\"90s\") = (%d, %v), want 90s", v, err)
	}
	for _, bad := range []string{"90", "soon", "0s", "2h"} {
		if _, err := dur.parse(bad); err == nil {
			t.Errorf("duration.parse(%q) should be refused", bad)
		}
	}

	// Formatting is the canonical text form the value is stored and listed in, so `72h0m0s` and
	// `72h` do not read back as two different settings.
	if got := dur.format(int64(72 * time.Hour)); got != "72h0m0s" {
		t.Errorf("duration.format(72h) = %q", got)
	}
	if got := count.format(42); got != "42" {
		t.Errorf("count.format(42) = %q", got)
	}
}

// TestLimitsListing confirms the listing reports every known limit, including ones nobody has set,
// with the tier each effective value came from and the default it reverts to.
func TestLimitsListing(t *testing.T) {
	c := OperationalConfig{}.With(LimitCode("staging."+string(LimitReplicaCeiling)), "200")
	all := c.Limits("staging")
	if len(all) != len(knownLimits) {
		t.Fatalf("listing has %d entries, want %d (every known limit)", len(all), len(knownLimits))
	}
	for _, l := range all {
		if l.Description == "" {
			t.Errorf("%s has no description", l.Code)
		}
		if l.Default == "" {
			t.Errorf("%s reports no built-in default", l.Code)
		}
		if l.Code == LimitReplicaCeiling {
			if l.Value != "200" || l.Scope != LimitScopeEnvironment || !l.EnvScoped {
				t.Errorf("staging replica ceiling = %+v, want value 200 at environment scope", l)
			}
		}
	}
	// The cluster listing of the same configuration sees only the default, since the value above is
	// staging's alone.
	for _, l := range c.Limits("") {
		if l.Code == LimitReplicaCeiling && (l.Value != "50" || l.Scope != LimitScopeDefault) {
			t.Errorf("cluster replica ceiling = %+v, want the built-in 50", l)
		}
	}
}

// TestStoredValueThatNoLongerParsesFallsThrough covers the read path's posture on a value it cannot
// use — a hand-edited row, or one written before a limit's bounds were narrowed. It falls through to
// the next tier rather than failing the deploy: the honest answer on the operation path is the bound
// that still holds, and the place to refuse a bad value is the write, where a human is present.
func TestStoredValueThatNoLongerParsesFallsThrough(t *testing.T) {
	c := OperationalConfig{Values: map[LimitCode]string{
		LimitCode("staging." + string(LimitReplicaCeiling)): "not a number",
		LimitReplicaCeiling: "80",
	}}
	if v, scope := c.ReplicaCeiling("staging"); v != 80 || scope != LimitScopeCluster {
		t.Errorf("unparseable environment value = (%d, %q), want the cluster value (80, cluster)", v, scope)
	}

	broken := OperationalConfig{Values: map[LimitCode]string{LimitReplicaCeiling: ""}}
	if v, scope := broken.ReplicaCeiling(""); v != 50 || scope != LimitScopeDefault {
		t.Errorf("unparseable cluster value = (%d, %q), want the built-in default", v, scope)
	}
}

// TestSection6OccupantsAreConfiguration covers the constants ADR-0068 §6 named, now that each is a
// row in the catalogue rather than a number in whatever file first needed it. The DEFAULTS are the
// load-bearing part: each is exactly the value its constant held, so an install that sets nothing
// behaves precisely as it did before the move.
func TestSection6OccupantsAreConfiguration(t *testing.T) {
	cases := []struct {
		code LimitCode
		def  time.Duration
		was  string
	}{
		{LimitBuildJobRetention, 3 * 24 * time.Hour, "the three days build.go compiled in"},
		{LimitAddonMetricRetention, 744 * time.Hour, "the month `-retentionPeriod=1` meant (VictoriaMetrics counts 31 days to a month)"},
		{LimitUnschedulableGrace, 30 * time.Second, "the thirty seconds adapter.go compiled in"},
	}
	for _, c := range cases {
		t.Run(string(c.code), func(t *testing.T) {
			if !KnownLimit(c.code) {
				t.Fatalf("%s is not a known limit", c.code)
			}
			d, _ := lookupLimit(c.code)
			if d.kind != LimitKindDuration {
				t.Errorf("%s is a %q, want a duration", c.code, d.kind)
			}
			if got, scope := (OperationalConfig{}).Duration("", c.code); got != c.def || scope != LimitScopeDefault {
				t.Errorf("unset %s = (%s, %q), want (%s, default) — %s", c.code, got, scope, c.def, c.was)
			}
			// Every §6 occupant except the ceiling is CLUSTER-scoped, so an environment key written
			// for one is not honoured — it would encode a cluster fact twice.
			if EnvScopableLimit(c.code) {
				t.Errorf("%s should be cluster-scoped (ADR-0068 §6)", c.code)
			}
			env := OperationalConfig{}.With(LimitCode("staging."+string(c.code)), d.format(d.min))
			if got, scope := env.Duration("staging", c.code); got != c.def || scope != LimitScopeDefault {
				t.Errorf("%s resolved an environment value: got (%s, %q), want the built-in default", c.code, got, scope)
			}
			// A cluster value IS honoured, which is the whole point of the move.
			cluster := OperationalConfig{}.With(c.code, d.format(d.min))
			if got, scope := cluster.Duration("staging", c.code); got != time.Duration(d.min) || scope != LimitScopeCluster {
				t.Errorf("%s cluster value = (%s, %q), want (%s, cluster)", c.code, got, scope, time.Duration(d.min))
			}
		})
	}
}

// TestUnschedulableGraceMayBeZeroButNotNegative pins the one occupant whose floor is deliberately
// zero: reporting the scheduler's first refusal immediately is noisy on a cluster that autoscales
// and exactly right on one with fixed capacity, which is why it is the operator's to choose.
func TestUnschedulableGraceMayBeZeroButNotNegative(t *testing.T) {
	d, _ := lookupLimit(LimitUnschedulableGrace)
	if _, err := d.parse("0s"); err != nil {
		t.Errorf("a zero grace should be settable: %v", err)
	}
	for _, bad := range []string{"-1s", "2h", "30"} {
		if _, err := d.parse(bad); err == nil {
			t.Errorf("parse(%q) should be refused", bad)
		}
	}
}

// TestClusterConfigFuncResolvesForAdapters covers the supplier the adapters read cluster-tier limits
// through (ADR-0068 §6). A NIL supplier is valid and yields the built-in defaults, which is what lets
// an adapter nobody wired behave exactly as it did when these were constants.
func TestClusterConfigFuncResolvesForAdapters(t *testing.T) {
	var unwired ClusterConfigFunc
	if got := unwired.ClusterDuration(context.Background(), LimitUnschedulableGrace); got != 30*time.Second {
		t.Errorf("nil supplier = %s, want the built-in 30s", got)
	}

	set := ClusterConfigFunc(func(context.Context) OperationalConfig {
		return OperationalConfig{}.With(LimitUnschedulableGrace, "90s")
	})
	if got := set.ClusterDuration(context.Background(), LimitUnschedulableGrace); got != 90*time.Second {
		t.Errorf("configured supplier = %s, want 90s", got)
	}

	// A read that fails resolves to the defaults rather than propagating: these values are read on
	// the deploy and status paths, where a briefly unavailable database must not become a failed
	// deploy or a status call that errors instead of answering.
	broken := ClusterConfigFrom(func(context.Context) (OperationalConfig, error) {
		return OperationalConfig{}, errors.New("database unavailable")
	})
	if got := broken.ClusterDuration(context.Background(), LimitUnschedulableGrace); got != 30*time.Second {
		t.Errorf("unreadable configuration = %s, want the built-in 30s", got)
	}

	// A limit of the wrong KIND, or one that does not exist, is a programming error rather than an
	// operator's, and reads as zero rather than as a plausible-looking duration.
	if got := set.ClusterDuration(context.Background(), LimitReplicaCeiling); got != 0 {
		t.Errorf("a count read as a duration = %s, want 0", got)
	}
}
