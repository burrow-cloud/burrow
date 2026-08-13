// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

// TestSetLimitTiersAndPrecedence drives the operator surface end to end through the engine: a
// cluster value applies everywhere, an environment value wins in its own environment, and the two
// tiers are stored under different keys so setting one does not move the other (ADR-0068 §1/§3).
func TestSetLimitTiersAndPrecedence(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newEngine(t, permissive())
	if _, err := e.AddEnvironment(ctx, "staging", "burrow-apps-staging"); err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}

	if err := e.SetLimit(ctx, "", cp.LimitReplicaCeiling, "80"); err != nil {
		t.Fatalf("SetLimit(cluster): %v", err)
	}
	if err := e.SetLimit(ctx, "staging", cp.LimitReplicaCeiling, "200"); err != nil {
		t.Fatalf("SetLimit(staging): %v", err)
	}

	ceilingIn := func(env string) cp.LimitInfo {
		t.Helper()
		ls, err := e.Limits(ctx, env)
		if err != nil {
			t.Fatalf("Limits(%q): %v", env, err)
		}
		for _, l := range ls {
			if l.Code == cp.LimitReplicaCeiling {
				return l
			}
		}
		t.Fatalf("Limits(%q) omitted the replica ceiling", env)
		return cp.LimitInfo{}
	}

	if l := ceilingIn(""); l.Value != "80" || l.Scope != cp.LimitScopeCluster {
		t.Errorf("cluster ceiling = (%q, %q), want (80, cluster)", l.Value, l.Scope)
	}
	if l := ceilingIn("staging"); l.Value != "200" || l.Scope != cp.LimitScopeEnvironment {
		t.Errorf("staging ceiling = (%q, %q), want (200, environment)", l.Value, l.Scope)
	}
	// `prod` IS the cluster tier, so setting it there and setting it cluster-wide are one write
	// (ADR-0067 §2).
	if l := ceilingIn(cp.DefaultEnvironment); l.Value != "80" || l.Scope != cp.LimitScopeCluster {
		t.Errorf("prod ceiling = (%q, %q), want the cluster value", l.Value, l.Scope)
	}

	// And the bounds are the ones the operations actually enforce. Each deploy names its
	// environment, because more than one is registered and a mutating call will not pick one
	// (ADR-0047 §1).
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Env: cp.DefaultEnvironment, Image: "img:1", Replicas: 80}); err != nil {
		t.Errorf("deploy at the cluster ceiling = %v, want it to proceed", err)
	}
	_, err := e.Deploy(ctx, cp.DeployRequest{App: "web2", Env: cp.DefaultEnvironment, Image: "img:1", Replicas: 81})
	mustLimit(t, err, cp.LimitReplicaCeiling)
	// staging's own, higher bound applies there: the same 200 replicas prod would refuse.
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Env: "staging", Image: "img:1", Replicas: 200}); err != nil {
		t.Errorf("deploy at staging's higher ceiling = %v, want it to proceed", err)
	}
	_, err = e.Deploy(ctx, cp.DeployRequest{App: "web3", Env: cp.DefaultEnvironment, Image: "img:1", Replicas: 200})
	mustLimit(t, err, cp.LimitReplicaCeiling)
}

// TestSetLimitRejectsBadInput covers the write-side validation. A limit is only a bound if the value
// stored under it is one, so an unknown code, an unregistered environment, and a value outside the
// limit's range are all refused rather than persisted (ADR-0068).
func TestSetLimitRejectsBadInput(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newEngine(t, permissive())

	if err := e.SetLimit(ctx, "", cp.LimitCode("app.made_up"), "3"); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("unknown limit = %v, want ErrInvalid", err)
	}
	if err := e.SetLimit(ctx, "", cp.LimitReplicaCeiling, "lots"); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("non-numeric value = %v, want ErrInvalid", err)
	}
	if err := e.SetLimit(ctx, "", cp.LimitReplicaCeiling, "0"); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("a ceiling of zero = %v, want ErrInvalid (a ceiling of zero is not a ceiling)", err)
	}
	if err := e.SetLimit(ctx, "nowhere", cp.LimitReplicaCeiling, "9"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("unregistered environment = %v, want ErrNotFound", err)
	}
	if _, err := e.Limits(ctx, "nowhere"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("Limits(unregistered) = %v, want ErrNotFound", err)
	}
}

// TestSetLimitRejectsAGuardrailCode and TestSetGuardrailRejectsALimitCode are the two halves of the
// same correction (ADR-0068 §2): the ceiling left the guardrail set, so each surface has to say
// where the other one is rather than answering "unknown". `guard set app.replica_ceiling allow` was
// the documented way to turn the ceiling off, and someone will type it.
func TestSetLimitRejectsAGuardrailCode(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newEngine(t, permissive())

	err := e.SetLimit(ctx, "", cp.LimitCode(cp.GuardrailAppDelete), "3")
	if !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("setting a guardrail code as a limit = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "burrow guard set") {
		t.Errorf("refusal %q should name the guardrail surface", err)
	}
}

func TestSetGuardrailRejectsALimitCode(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newEngine(t, permissive())

	err := e.SetGuardrail(asOperator(ctx), cp.GuardrailScope{}, "", cp.GuardrailCode(cp.LimitReplicaCeiling), cp.DispositionAllow)
	if !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("guard set on a limit code = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "burrow cluster config set") {
		t.Errorf("refusal %q should name the command that sets the limit", err)
	}
	// And the guardrail listing no longer offers it at all, so an operator reading `guard list`
	// never learns a disposition they cannot set.
	gs, err := e.Guardrails(ctx, cp.GuardrailScope{})
	if err != nil {
		t.Fatalf("Guardrails: %v", err)
	}
	for _, g := range gs {
		if string(g.Code) == string(cp.LimitReplicaCeiling) {
			t.Errorf("guard list still reports %q", g.Code)
		}
	}
}

// TestLimitValuesAreCanonicalized confirms a set stores the limit's canonical text form, so two
// spellings of one bound do not read back as two different settings.
func TestLimitValuesAreCanonicalized(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newEngine(t, permissive())
	if err := e.SetLimit(ctx, "", cp.LimitReplicaCeiling, "  80  "); err != nil {
		t.Fatalf("SetLimit: %v", err)
	}
	ls, err := e.Limits(ctx, "")
	if err != nil {
		t.Fatalf("Limits: %v", err)
	}
	for _, l := range ls {
		if l.Code == cp.LimitReplicaCeiling && l.Value != "80" {
			t.Errorf("stored value = %q, want the canonical %q", l.Value, "80")
		}
	}
}

// TestOverCeilingRefusalWritesNoAuditDecision pins the consequence of §2's reclassification: a bound
// is a validation failure, so crossing one records nothing in the audit log. The log is the record
// of what the GUARDRAILS decided (ADR-0027), and a row saying a limit "denied" an operation would
// invite an operator to look for the disposition that did it.
func TestOverCeilingRefusalWritesNoAuditDecision(t *testing.T) {
	ctx := context.Background()
	e, k, d, _ := newEngine(t, permissive())
	d.SetLimits(ceiling(3))

	_, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 9})
	mustLimit(t, err, cp.LimitReplicaCeiling)

	if rows := auditRows(t, d, "deploy"); len(rows) != 0 {
		t.Errorf("deploy audit rows = %d, want 0 (a limit is not a guardrail decision)", len(rows))
	}
	if _, ok := k.Spec("web"); ok {
		t.Errorf("a refused deploy must not apply to the cluster")
	}
}
