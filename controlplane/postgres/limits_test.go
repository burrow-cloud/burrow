// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package postgres_test

import (
	"context"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

// TestOperationalConfigRoundTrip exercises the store side of ADR-0068's configuration: a set is
// visible on the next read, an update replaces rather than duplicates (the code is the primary
// key), and the two tiers are separate rows so a cluster value and an environment value coexist.
//
// Values are keyed by the test name so this is safe against the shared CI database, which is also
// why it asserts on the codes it wrote rather than on the size of the map.
func TestOperationalConfigRoundTrip(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()

	cluster := cp.LimitCode(t.Name() + ".ceiling")
	scoped := cp.LimitCode("staging." + string(cluster))

	if err := s.SetLimit(ctx, cluster, "80"); err != nil {
		t.Fatalf("SetLimit(cluster): %v", err)
	}
	if err := s.SetLimit(ctx, scoped, "200"); err != nil {
		t.Fatalf("SetLimit(staging): %v", err)
	}

	c, err := s.OperationalConfig(ctx)
	if err != nil {
		t.Fatalf("OperationalConfig: %v", err)
	}
	if c.Values[cluster] != "80" {
		t.Errorf("cluster value = %q, want 80", c.Values[cluster])
	}
	if c.Values[scoped] != "200" {
		t.Errorf("staging value = %q, want 200", c.Values[scoped])
	}

	// A second set of the same code updates it in place: an operator raising a bound twice must not
	// leave two rows for one setting.
	if err := s.SetLimit(ctx, cluster, "120"); err != nil {
		t.Fatalf("SetLimit(cluster, again): %v", err)
	}
	c, err = s.OperationalConfig(ctx)
	if err != nil {
		t.Fatalf("OperationalConfig: %v", err)
	}
	if c.Values[cluster] != "120" {
		t.Errorf("updated cluster value = %q, want 120", c.Values[cluster])
	}
	if c.Values[scoped] != "200" {
		t.Errorf("updating the cluster value moved the environment one to %q", c.Values[scoped])
	}

	// An empty value is refused rather than stored: a limit with no value is not a bound.
	if err := s.SetLimit(ctx, cluster, "  "); err == nil {
		t.Errorf("SetLimit with an empty value should be refused")
	}
}

// TestReplicaCeilingDispositionsAreGone pins the migration's correction (ADR-0068 §2): the stored
// dispositions for app.replica_ceiling are DELETED rather than left in the table to be ignored,
// because an unrecognized code sitting in guardrail_policy is a setting an operator believes is in
// force. The migration runs in openStore, so this asserts the state it leaves behind.
func TestReplicaCeilingDispositionsAreGone(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()

	p, err := s.Policy(ctx)
	if err != nil {
		t.Fatalf("Policy: %v", err)
	}
	for code := range p.Dispositions {
		if string(code) == string(cp.LimitReplicaCeiling) {
			t.Errorf("guardrail_policy still holds %q after the migration", code)
		}
	}
	// The listing is derived from the known set, so it cannot offer a disposition for a code that
	// is no longer a guardrail either.
	for _, g := range p.Guardrails() {
		if string(g.Code) == string(cp.LimitReplicaCeiling) {
			t.Errorf("guard list reports %q, which is a limit rather than a guardrail", g.Code)
		}
	}
}
