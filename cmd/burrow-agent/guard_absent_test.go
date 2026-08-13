// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/burrow-cloud/burrow/internal/agentsurface"
)

// These tests cover the half of `guard` that is not about dispositions: the capabilities this
// binary does not carry, reported so an agent can relay a refusal
// ([ADR-0065](../../docs/adr/0065-what-belongs-on-the-agent-surface.md) §7).
//
// The behaviour they protect is a security property, not a convenience. ADR-0065 §5: an absent
// verb answers `unknown command`, which is a DEAD END, and a dead end is what pushes an agent to
// route around the control channel entirely — reaching for `kubectl` or a shell, the failure
// ADR-0021 says Burrow cannot close from the inside. A verb that is absent AND legible is a
// refusal the agent can hand to a human instead.

// TestGuardReportsAbsentCapabilities asserts `guard` answers with both kinds of limit and keeps
// them apart in JSON: a guardrail disposition (which an operator can relax with `burrow guard set`)
// and a capability that is not in this binary at all (which they cannot).
func TestGuardReportsAbsentCapabilities(t *testing.T) {
	srv := cannedControlPlane(t)
	out := runAgent(t, srv, "guard")

	var got agentsurface.GuardReport
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("guard output is not a guard report: %v\n%s", err, out)
	}
	if len(got.Guardrails) == 0 || got.Guardrails[0].Code != "app.deploy" {
		t.Errorf("guard dropped the control plane's dispositions: %+v", got.Guardrails)
	}
	if len(got.AbsentCapabilities) == 0 {
		t.Fatal("guard reported no absent capabilities; an agent hitting `addon remove` would get " +
			"`unknown command` with no account of what it was or who can run it")
	}

	byPath := map[string]agentsurface.Capability{}
	for _, c := range got.AbsentCapabilities {
		byPath[c.Path] = c
	}
	// The capabilities ADR-0065 §2 and issue #337 name explicitly.
	for _, path := range []string{
		"addon remove", "addon remove --delete-data", "addon detach", "addon detach --delete-data",
		"addon restore", "guard set", "secret set", "env add", "cluster install",
	} {
		c, ok := byPath[path]
		if !ok {
			t.Errorf("guard does not report %q as absent", path)
			continue
		}
		// "What it is" and "who can do it" are the point: the agent must be able to explain, not
		// merely be refused.
		if c.What == "" || c.Who == "" || c.Command == "" {
			t.Errorf("guard reports %q as absent without what/who/command (%+v); that is a dead end "+
				"with extra steps", path, c)
		}
	}

	// Nothing the binary actually registers is reported as absent.
	for _, path := range commandPaths(newRootCmd()) {
		if _, ok := byPath[path]; ok {
			t.Errorf("guard reports %q as absent, but this binary registers it", path)
		}
	}
}

// TestAbsentReportTracksTheBinary is the anti-drift property. The report is not a hand-kept list
// beside the surface guard: it is the capability catalogue minus the paths the command tree
// actually registers. So taking a verb OUT of the binary makes it legible in `guard` immediately,
// with no second edit — which is what stops the report from quietly going stale and turning a
// removed verb back into a dead end.
func TestAbsentReportTracksTheBinary(t *testing.T) {
	root := newRootCmd()
	before := absentPaths(absentCapabilities(root, false))
	if before["addon attach"] {
		t.Fatal("`addon attach` is reported absent while the binary still registers it")
	}

	// Remove `addon attach`, as a change that took the verb off the agent surface would.
	addon := findCommand(t, root, "addon")
	addon.RemoveCommand(findCommand(t, addon, "attach"))

	after := absentCapabilities(root, false)
	if !absentPaths(after)["addon attach"] {
		t.Fatal("a verb removed from the command tree is not reported as absent; the report would have " +
			"to be edited a second time to stay true, and an un-edited report is a dead end")
	}
	for _, c := range after {
		if c.Path != "addon attach" {
			continue
		}
		if c.What == "" || c.Who == "" {
			t.Errorf("the newly absent capability is reported without what/who: %+v", c)
		}
	}
}

// TestGuardStaysReadOnly pins ADR-0065's load-bearing assumption: the middle tier is trustworthy
// only because the agent cannot change its own guardrails. `guard` on this binary reads — it has no
// subcommands (so no `set` can hide under it) and no flag that writes.
func TestGuardStaysReadOnly(t *testing.T) {
	guard := findCommand(t, newRootCmd(), "guard")
	for _, sub := range guard.Commands() {
		t.Errorf("burrow-agent `guard` has the subcommand %q; it must carry no verb at all, and "+
			"`guard set` least of all (ADR-0020, ADR-0065)", sub.Name())
	}
	guard.Flags().VisitAll(func(f *pflag.Flag) {
		switch f.Name {
		// --name narrows what is READ to one app or add-on instance (ADR-0085). It is on this list
		// because a scope is not a verb: the command it rides has no way to write whatever it names.
		case "control-plane", "token", "env", "name", "context", "namespace", "timeout", "kubeconfig":
		default:
			t.Errorf("burrow-agent `guard` grew the flag --%s; check it cannot write", f.Name)
		}
	})

	// Reporting the absent capabilities is a local computation over this binary's own command
	// tree, so the read adds no request and no write to the control plane.
	if len(absentCapabilities(newRootCmd(), false)) == 0 {
		t.Error("absentCapabilities reported nothing for the real command tree")
	}
}

func absentPaths(caps []agentsurface.Capability) map[string]bool {
	out := map[string]bool{}
	for _, c := range caps {
		out[c.Path] = true
	}
	return out
}

func findCommand(t *testing.T, parent *cobra.Command, name string) *cobra.Command {
	t.Helper()
	for _, sub := range parent.Commands() {
		if sub.Name() == name {
			return sub
		}
	}
	t.Fatalf("command %q has no subcommand %q", strings.TrimSpace(parent.CommandPath()), name)
	return nil
}

// TestBucketDeletionIsAbsentAndLegible asserts ADR-0063 §5's tier-1 half directly, rather than
// leaving it as a property of whatever the command tree happens to register today.
//
// The two bucket operations sit in different tiers on purpose. Creating a bucket is additive and
// reversible, so it ships as a `confirm` guardrail and a human approves the cost. DELETING one
// fails ADR-0065's scope test twice over: it destroys every backup the platform holds, and a bucket
// name lives in a GLOBAL namespace, so a mistaken argument can delete something outside this
// cluster entirely — past the reach of any guardrail. That is the unbounded worst case tier 1 is
// reserved for, so no verb for it is compiled in, and `guard` reports it with who can do it
// instead: the vendor, and a human.
func TestBucketDeletionIsAbsentAndLegible(t *testing.T) {
	for _, path := range commandPaths(newRootCmd()) {
		normalized := strings.NewReplacer(" ", "_", "-", "_").Replace(path)
		if !strings.Contains(normalized, "bucket") {
			continue
		}
		for _, verb := range []string{"delete", "remove", "destroy", "rb"} {
			if strings.Contains(normalized, verb) {
				t.Errorf("burrow-agent registers %q, which deletes a bucket. That is ADR-0065 tier 1: "+
					"the blast radius is every backup Burrow holds, and a bucket name is global, so a "+
					"mistaken argument reaches outside the cluster (ADR-0063 §5). It must not be "+
					"compiled into this binary at all.", path)
			}
		}
	}

	var deletion *agentsurface.Capability
	for _, c := range absentCapabilities(newRootCmd(), false) {
		if c.Path == "bucket delete" {
			cap := c
			deletion = &cap
		}
	}
	if deletion == nil {
		t.Fatal("`guard` does not report bucket deletion as an absent capability; an agent asked to " +
			"clean up storage would get `unknown command` with no account of what it was or who can " +
			"do it, which is the dead end ADR-0065 §7 exists to prevent")
	}
	if deletion.What == "" || deletion.Why == "" || deletion.Who == "" || deletion.Command == "" {
		t.Errorf("bucket deletion is reported absent without what/why/who/command (%+v)", *deletion)
	}
}
