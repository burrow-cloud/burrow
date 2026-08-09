// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"strings"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

// ADR-0095: attaching a database is held for a human.
//
// Attach was the one mutating add-on verb the control plane did not gate, on the grounds that it
// "provisions and destroys nothing". These tests pin what the record decided instead: the hold
// itself, that a held attach provisions NOTHING, what the confirmation says, and the two tiers the
// disposition can be narrowed to.

// TestAttachIsHeldForConfirmation is the default. An attach on an install where nobody has
// configured anything stops and asks, and the refusal carries the code an agent branches on rather
// than prose it has to read.
func TestAttachIsHeldForConfirmation(t *testing.T) {
	ctx := context.Background()
	e, _, d, _ := newPostgresEngine(t)
	d.SetPolicy(cp.DefaultPolicy())

	_, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", "", cp.AttachAddonOptions{})
	mustGuardrail(t, err, cp.GuardrailAddonAttach)
	g, _ := cp.AsGuardrail(err)
	if !g.NeedsConfirmation {
		t.Fatalf("attach was DENIED rather than held: %v", err)
	}
}

// TestHeldAttachProvisionsNothing is the property that makes the hold worth having. A confirmation
// that arrives after the database exists is a notification, not a gate: the disk is spent, the role
// is created, and on a re-attach the password is already rotated.
func TestHeldAttachProvisionsNothing(t *testing.T) {
	ctx := context.Background()
	e, k, d, prov := newPostgresEngine(t)
	d.SetPolicy(cp.DefaultPolicy())

	if _, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", "", cp.AttachAddonOptions{}); err == nil {
		t.Fatal("the attach proceeded")
	}
	if got := prov.Databases(cp.DefaultEnvironment); len(got) != 0 {
		t.Errorf("the held attach provisioned %v; the guardrail runs after the reads and before the first effect", got)
	}
	if _, ok := k.SecretValue("web", cp.AppDatabaseURLKey); ok {
		t.Error("the held attach wrote a connection string into the app's Secret")
	}
	if _, recorded, err := d.AddonEnvKey(ctx, string(cp.AddonPostgres), "web", cp.DefaultEnvironment, defaultInstance(cp.DefaultEnvironment)); err != nil || recorded {
		t.Errorf("the held attach recorded an attachment (recorded=%v, err=%v)", recorded, err)
	}
}

// TestConfirmedAttachProceeds. The hold is a hold, not a refusal: the same call with the caller's
// confirmation does exactly what an attach has always done.
func TestConfirmedAttachProceeds(t *testing.T) {
	ctx := context.Background()
	e, k, d, _ := newPostgresEngine(t)
	d.SetPolicy(cp.DefaultPolicy())

	res, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", "", cp.AttachAddonOptions{Confirm: true})
	if err != nil {
		t.Fatalf("AttachAddon(confirmed): %v", err)
	}
	if res.SecretKey != cp.AppDatabaseURLKey {
		t.Errorf("secret key = %q, want %q", res.SecretKey, cp.AppDatabaseURLKey)
	}
	if _, ok := k.SecretValue("web", cp.AppDatabaseURLKey); !ok {
		t.Error("the confirmed attach wrote no connection string")
	}
}

// TestTheHoldNamesWhatAFirstAttachDoes (ADR-0095 §4). The reader of the message is usually an agent
// relaying to a human, so the sentence has to carry every consequence: which instance, which
// environment, which variable, and that the app restarts.
func TestTheHoldNamesWhatAFirstAttachDoes(t *testing.T) {
	ctx := context.Background()
	e, _, d, _ := newPostgresEngine(t)
	d.SetPolicy(cp.DefaultPolicy())

	_, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", "", cp.AttachAddonOptions{EnvKey: "PG_DSN"})
	g, ok := cp.AsGuardrail(err)
	if !ok {
		t.Fatalf("err = %v, want a GuardrailError", err)
	}
	for _, want := range []string{`attaching "web"`, defaultInstance(cp.DefaultEnvironment), cp.DefaultEnvironment, "PG_DSN", "restarts the app", "creates a database and a login role"} {
		if !strings.Contains(g.Message, want) {
			t.Errorf("the held message does not mention %q: %s", want, g.Message)
		}
	}
	if strings.Contains(g.Message, "ROTATES") {
		t.Errorf("a FIRST attach was described as a rotation, which trains a reader to discount the words: %s", g.Message)
	}
}

// TestTheHoldOnAReAttachLeadsWithTheRotation is the half of §4 that matters most. A re-attach's
// password rotation is the one part of an attach nothing can undo — the connection string is
// generated server-side and never returned — so a message that described it as "creates a database"
// would be describing a consequence the call does not have and omitting the one it does.
func TestTheHoldOnAReAttachLeadsWithTheRotation(t *testing.T) {
	ctx := context.Background()
	e, _, d, _ := newPostgresEngine(t)
	d.SetPolicy(cp.DefaultPolicy())

	if _, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", "", cp.AttachAddonOptions{Confirm: true}); err != nil {
		t.Fatalf("first AttachAddon: %v", err)
	}

	_, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", "", cp.AttachAddonOptions{})
	g, ok := cp.AsGuardrail(err)
	if !ok {
		t.Fatalf("the re-attach was not held: %v", err)
	}
	for _, want := range []string{`re-attaching "web"`, "ROTATES ITS PASSWORD", "stops connecting"} {
		if !strings.Contains(g.Message, want) {
			t.Errorf("the held message does not mention %q: %s", want, g.Message)
		}
	}
}

// TestTheHoldOnARenameNamesTheVacatedKey. `--as` on an attached app MOVES the variable, so the old
// name is removed after the rotation. An app reading it finds nothing, which is a consequence worth
// a sentence of its own.
func TestTheHoldOnARenameNamesTheVacatedKey(t *testing.T) {
	ctx := context.Background()
	e, _, d, _ := newPostgresEngine(t)
	d.SetPolicy(cp.DefaultPolicy())

	if _, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", "", cp.AttachAddonOptions{Confirm: true}); err != nil {
		t.Fatalf("first AttachAddon: %v", err)
	}

	_, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", "", cp.AttachAddonOptions{EnvKey: "PG_DSN"})
	g, ok := cp.AsGuardrail(err)
	if !ok {
		t.Fatalf("the rename was not held: %v", err)
	}
	if !strings.Contains(g.Message, cp.AppDatabaseURLKey+" is removed") {
		t.Errorf("the held message does not say the old variable goes away: %s", g.Message)
	}
}

// TestAttachIsEnvScopable (ADR-0095 §2). addon.attach is only the second addon.* code that can be
// set per environment, and the tier is what makes a confirm default affordable: a sandbox relaxes on
// its own without relaxing production. Without it the only relief would be a cluster-wide allow.
func TestAttachIsEnvScopable(t *testing.T) {
	ctx := context.Background()
	e, _, d, _ := newPostgresEngine(t)
	if _, err := e.AddEnvironment(ctx, "dev", "burrow-apps-dev"); err != nil {
		t.Fatalf("AddEnvironment(dev): %v", err)
	}
	installPostgresIn(t, e, "dev")
	d.SetPolicy(cp.DefaultPolicy().With(cp.GuardrailCode("dev."+string(cp.GuardrailAddonAttach)), cp.DispositionAllow))

	if _, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", "dev", cp.AttachAddonOptions{}); err != nil {
		t.Fatalf("an unconfirmed attach in dev, where the operator allowed it: %v", err)
	}
	if _, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", cp.DefaultEnvironment, cp.AttachAddonOptions{}); err == nil {
		t.Fatal("relaxing dev also relaxed the default environment")
	}
}

// TestAttachIsScopedByTheInstanceAndNotTheApp (ADR-0095 §2, ADR-0085 §1). The disposition follows
// the server the database lands on, because that is where the reach stops. An operator who set it on
// an app would be protecting one name while the identical verb put a database on the same instance
// for the next one — which reads as protection and is not.
func TestAttachIsScopedByTheInstanceAndNotTheApp(t *testing.T) {
	ctx := context.Background()
	e, _, d, _ := newPostgresEngine(t)
	second, err := e.InstallAddon(ctx, cp.AddonPostgres, cp.DefaultEnvironment, cp.InstallAddonOptions{Name: "analytics", Confirm: true})
	if err != nil {
		t.Fatalf("InstallAddon(analytics): %v", err)
	}
	own := defaultInstance(cp.DefaultEnvironment)
	d.SetPolicy(cp.DefaultPolicy().
		With(cp.GuardrailCode(cp.DefaultEnvironment+"."+own+"."+string(cp.GuardrailAddonAttach)), cp.DispositionAllow).
		With(cp.GuardrailCode(cp.DefaultEnvironment+"."+second.Label+"."+string(cp.GuardrailAddonAttach)), cp.DispositionDeny))

	if _, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", "", cp.AttachAddonOptions{}); err != nil {
		t.Fatalf("the environment's own instance was allowed and still held: %v", err)
	}
	_, err = e.AttachAddon(ctx, cp.AddonPostgres, "web", "", cp.AttachAddonOptions{Instance: second.Label, EnvKey: "ANALYTICS_URL", Confirm: true})
	g, ok := cp.AsGuardrail(err)
	if !ok {
		t.Fatalf("the attach to the denied instance = %v, want a GuardrailError", err)
	}
	if g.NeedsConfirmation {
		t.Error("a deny was reported as a hold, so a caller would retry with confirm and be refused again")
	}
	// A refusal names the thing the disposition was set for and the narrow lever that relaxes it,
	// so an operator does not reach for the cluster-wide one (ADR-0085's consequences).
	for _, want := range []string{`for the add-on instance "` + second.Label + `"`, "guard set --env " + cp.DefaultEnvironment + " --name " + second.Label} {
		if !strings.Contains(g.Message, want) {
			t.Errorf("the refusal does not mention %q: %s", want, g.Message)
		}
	}
}

// TestAttachDenyBindsEveryCaller is the honest limit ADR-0095 §5 states, asserted rather than left in
// prose: the disposition has no caller dimension, so a deny meant for an over-eager agent refuses the
// operator's identical call too. ADR-0094 is the record that adds the axis; until it is built, this
// is what `deny` means, and the test is here to fail loudly when that changes.
func TestAttachDenyBindsEveryCaller(t *testing.T) {
	ctx := context.Background()
	e, _, d, _ := newPostgresEngine(t)
	d.SetPolicy(cp.DefaultPolicy().With(cp.GuardrailAddonAttach, cp.DispositionDeny))

	_, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", "", cp.AttachAddonOptions{Confirm: true})
	g, ok := cp.AsGuardrail(err)
	if !ok {
		t.Fatalf("err = %v, want a GuardrailError", err)
	}
	if g.NeedsConfirmation {
		t.Fatal("a deny is satisfiable by confirming, which is not a deny")
	}
}
