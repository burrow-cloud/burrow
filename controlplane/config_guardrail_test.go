// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"strings"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

// A config write is a guarded operation (ADR-0098). The reason is not what the value is — a config
// var is non-secret — but what writing it DOES: the store is re-applied to the running workload, so
// the app rolls and comes back with an environment somebody changed.
//
// These tests are the four things the record decides: the agent is held, a person is not (ADR-0097),
// one code covers both directions, and the decision reaches the audit trail.

// agentCtx is the context a request from an agent credential arrives on, the way the API layer's
// authentication builds it.
func agentCtx() context.Context {
	return cp.ContextWithCaller(context.Background(), cp.Caller{PrincipalID: "p-agent", PrincipalName: "claude", Kind: cp.CredentialKindAgent})
}

// personCtx is the same for a human operator at a terminal.
func personCtx() context.Context {
	return cp.ContextWithCaller(context.Background(), cp.Caller{PrincipalID: "p-1", PrincipalName: "ada", Kind: cp.CredentialKindUser})
}

// configPolicy is the product default for a config write — confirm — with deploy allowed so a test
// can put a workload in place first.
func configPolicy() cp.Policy {
	return cp.DefaultPolicy().With(cp.GuardrailAppDeploy, cp.DispositionAllow)
}

// TestAgentConfigWriteIsHeldForConfirmation is the issue this record closes. Before it there was no
// disposition of any kind on a config write, so an agent performed one freely — including on an app
// whose configuration is checked at startup, where the roll it causes does not come back.
func TestAgentConfigWriteIsHeldForConfirmation(t *testing.T) {
	ctx := agentCtx()
	e, k, _, _ := newEngine(t, configPolicy())
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	err := e.SetConfig(ctx, "web", "", "LOG_LEVEL", "debug", false, false)
	mustGuardrail(t, err, cp.GuardrailAppConfig)
	g, _ := cp.AsGuardrail(err)
	if !g.NeedsConfirmation {
		t.Errorf("a default config write = %v, want a hold for confirmation rather than a refusal", err)
	}
	// The hold names the key and the app, so the human approving it is approving something specific,
	// and says the app rolls — which is the reason the guardrail exists at all.
	if !strings.Contains(g.Message, "LOG_LEVEL") || !strings.Contains(g.Message, "rolls the running app") {
		t.Errorf("hold message = %q, want the key and the roll named", g.Message)
	}

	// Nothing was written and nothing rolled.
	cfg, err := e.ListConfig(ctx, "web", "")
	if err != nil {
		t.Fatalf("ListConfig: %v", err)
	}
	if _, present := cfg["LOG_LEVEL"]; present {
		t.Errorf("store = %+v, want nothing persisted by a held write", cfg)
	}
	if spec, _ := k.Spec("web"); spec.Env["LOG_LEVEL"] != "" {
		t.Errorf("spec env = %+v, want the workload untouched by a held write", spec.Env)
	}

	// The same call, confirmed, proceeds and reaches the running app.
	if err := e.SetConfig(ctx, "web", "", "LOG_LEVEL", "debug", false, true); err != nil {
		t.Fatalf("confirmed SetConfig: %v", err)
	}
	if spec, _ := k.Spec("web"); spec.Env["LOG_LEVEL"] != "debug" {
		t.Errorf("spec env = %+v, want LOG_LEVEL=debug after the confirmed write", spec.Env)
	}
}

// TestAPersonsConfigWriteIsNotHeld: a guardrail holds the agent and nobody else (ADR-0097). A
// person's config write proceeds with no confirmation even where the disposition is deny, because
// their Kubernetes access already decides what they can do to the same Deployment.
func TestAPersonsConfigWriteIsNotHeld(t *testing.T) {
	ctx := personCtx()
	e, _, _, _ := newEngine(t, configPolicy().With(cp.GuardrailAppConfig, cp.DispositionDeny))

	if err := e.SetConfig(ctx, "web", "", "LOG_LEVEL", "debug", true, false); err != nil {
		t.Fatalf("a person's SetConfig = %v, want it to proceed unconfirmed", err)
	}
	if err := e.UnsetConfig(ctx, "web", "", "LOG_LEVEL", true, false); err != nil {
		t.Fatalf("a person's UnsetConfig = %v, want it to proceed unconfirmed", err)
	}
}

// TestConfigUnsetSharesTheSetGuardrail pins the one-code decision (ADR-0098 §1). Removing the
// variable an app reads at startup rolls it into the same place a wrong value does, so there is
// nothing for a second code to express.
func TestConfigUnsetSharesTheSetGuardrail(t *testing.T) {
	ctx := agentCtx()
	e, _, _, _ := newEngine(t, configPolicy())

	mustGuardrail(t, e.UnsetConfig(ctx, "web", "", "LOG_LEVEL", false, false), cp.GuardrailAppConfig)

	// One disposition moves both directions together.
	e2, _, _, _ := newEngine(t, configPolicy().With(cp.GuardrailAppConfig, cp.DispositionAllow))
	if err := e2.SetConfig(ctx, "web", "", "A", "1", true, false); err != nil {
		t.Fatalf("allowed SetConfig: %v", err)
	}
	if err := e2.UnsetConfig(ctx, "web", "", "A", true, false); err != nil {
		t.Fatalf("allowed UnsetConfig: %v", err)
	}
}

// TestNoRestartIsNotAWayPastTheGuardrail. --no-restart skips the immediate roll, not the guardrail:
// the value still lands in the store, and the next deploy — which app.deploy allows by default —
// puts it in front of the app anyway. A gate that only covered the rolling form would gate nothing.
func TestNoRestartIsNotAWayPastTheGuardrail(t *testing.T) {
	ctx := agentCtx()
	e, _, _, _ := newEngine(t, configPolicy())

	err := e.SetConfig(ctx, "web", "", "LOG_LEVEL", "debug", true, false)
	mustGuardrail(t, err, cp.GuardrailAppConfig)
	g, _ := cp.AsGuardrail(err)
	// It still tells the truth about what it does and does not do to the running app.
	if !strings.Contains(g.Message, "not rolled") {
		t.Errorf("hold message = %q, want it to say the running app is not rolled", g.Message)
	}
}

// TestConfigWriteIsScopedPerAppAndEnvironment: app.config is declared env-scoped and app-scopable
// like the other app.* codes, which is what lets an operator deny it on the one app that cannot
// survive a roll while leaving every other app alone.
func TestConfigWriteIsScopedPerAppAndEnvironment(t *testing.T) {
	ctx := agentCtx()
	if !cp.EnvScopable(cp.GuardrailAppConfig) || !cp.NameScopable(cp.GuardrailAppConfig) {
		t.Fatalf("app.config env-scopable = %v, name-scopable = %v, want both",
			cp.EnvScopable(cp.GuardrailAppConfig), cp.NameScopable(cp.GuardrailAppConfig))
	}

	e, _, _, _ := newEngine(t, configPolicy().With(cp.GuardrailAppConfig, cp.DispositionAllow))
	if _, err := e.AddEnvironment(ctx, "staging", "burrow-staging"); err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}
	// Deny it for one app in one environment; everything else keeps the allow underneath.
	if err := e.SetGuardrail(ctx, cp.GuardrailScope{Env: "staging", Name: "burrowd-cloud"}, "", cp.GuardrailAppConfig, cp.DispositionDeny); err != nil {
		t.Fatalf("SetGuardrail: %v", err)
	}

	err := e.SetConfig(ctx, "burrowd-cloud", "staging", "NAMESPACE", "wrong", true, true)
	mustGuardrail(t, err, cp.GuardrailAppConfig)
	if g, _ := cp.AsGuardrail(err); g.NeedsConfirmation {
		t.Errorf("a denied write = %v, want a refusal no confirmation opens", err)
	}
	// A different app in the same environment is unaffected.
	if err := e.SetConfig(ctx, "web", "staging", "LOG_LEVEL", "debug", true, false); err != nil {
		t.Fatalf("another app's SetConfig = %v, want the per-app deny to touch nothing else", err)
	}
}

// TestConfigWriteIsAudited: the decision and the execution both reach the trail (ADR-0027), the
// guardrail code is named on the decision row, and the config VALUE is nowhere in it.
func TestConfigWriteIsAudited(t *testing.T) {
	ctx := agentCtx()
	e, _, d, _ := newEngine(t, configPolicy())

	// A held write records the hold and nothing else.
	mustGuardrail(t, e.SetConfig(ctx, "web", "", "LOG_LEVEL", "s3cret-looking-value", true, false), cp.GuardrailAppConfig)
	rows := auditRows(t, d, "config_set")
	if len(rows) != 1 {
		t.Fatalf("held write recorded %d rows, want 1 decision row", len(rows))
	}
	assertConfigRow(t, rows[0], cp.AuditHeld, string(cp.GuardrailAppConfig), "LOG_LEVEL")

	// The confirmed write records the allowed decision and the execution.
	if err := e.SetConfig(ctx, "web", "", "LOG_LEVEL", "s3cret-looking-value", true, true); err != nil {
		t.Fatalf("confirmed SetConfig: %v", err)
	}
	rows = auditRows(t, d, "config_set")
	if len(rows) != 3 {
		t.Fatalf("confirmed write left %d rows, want 3 (held, allowed, executed)", len(rows))
	}
	assertConfigRow(t, rows[1], cp.AuditAllowed, string(cp.GuardrailAppConfig), "LOG_LEVEL")
	assertConfigRow(t, rows[2], cp.AuditExecuted, "", "LOG_LEVEL")
	if rows[1].Caller != string(cp.CredentialKindAgent) {
		t.Errorf("caller = %q, want the agent kind the decision was made about", rows[1].Caller)
	}

	// The removal is its own operation under the same code.
	if err := e.UnsetConfig(ctx, "web", "", "LOG_LEVEL", true, true); err != nil {
		t.Fatalf("UnsetConfig: %v", err)
	}
	unset := auditRows(t, d, "config_unset")
	if len(unset) != 2 {
		t.Fatalf("unset left %d rows, want 2 (allowed, executed)", len(unset))
	}
	assertConfigRow(t, unset[0], cp.AuditAllowed, string(cp.GuardrailAppConfig), "LOG_LEVEL")
}

// assertConfigRow checks one config audit row: its outcome, the guardrail code it names, the key it
// records — and that no part of it carries the value, which is the redaction boundary (ADR-0027).
func assertConfigRow(t *testing.T, row cp.AuditEntry, outcome cp.AuditOutcome, code, key string) {
	t.Helper()
	if row.Outcome != outcome {
		t.Errorf("outcome = %q, want %q", row.Outcome, outcome)
	}
	if row.GuardrailCode != code {
		t.Errorf("guardrail code = %q, want %q", row.GuardrailCode, code)
	}
	if row.Target != "web" {
		t.Errorf("target = %q, want the app", row.Target)
	}
	if row.Args["key"] != key {
		t.Errorf("args = %+v, want key %q", row.Args, key)
	}
	for name, v := range row.Args {
		if strings.Contains(v, "s3cret-looking-value") {
			t.Errorf("arg %q carries the config value; the trail records key names only", name)
		}
	}
}
