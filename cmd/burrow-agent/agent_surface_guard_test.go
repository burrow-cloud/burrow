// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"sort"
	"strings"
	"testing"

	"github.com/burrow-cloud/burrow/internal/agentsurface"
)

// Burrow ships two command surfaces on purpose, and the split between them is a security
// property rather than an accident of packaging.
//
//   - `burrow` (cmd/burrow) is the OPERATOR CLI. It carries install, upgrade, uninstall,
//     bootstrap, join, cluster setup, registry and provider credentials, `env add`, and
//     `guard set` — the verbs that create namespaces, write RBAC, and change the shape of the
//     cluster. A human runs them at their own terminal with their own admin kubeconfig.
//   - `burrow-agent` (this binary) is the surface an AI agent drives. It carries APP LIFECYCLE
//     only: deploy, build, scale, rollback, run, logs, metrics, config, secrets, add-ons,
//     domains, expose. Notably `environments` LISTS and SELECTS environments; it does not create
//     one — registering an environment is `burrow env add`, and it creates a namespace.
//
// An agent reads untrusted input by its nature — a repository it was pointed at, an error
// message, a log line — so prompt injection is a live path into whatever the agent can express.
// If this surface ever gained a verb that creates a namespace or writes RBAC, an agent could
// WIDEN ITS OWN CONTROL PLANE'S ACCESS on the user's cluster: grant burrowd write access to a
// namespace of the agent's choosing, and the boundary the operator believed they had is gone.
// That is a self-hoster's cluster, their other workloads, and their data.
//
// [ADR-0049](../../docs/adr/0049-burrow-agent-scoped-cli-control-channel.md) §2 names three
// independent layers: (a) the binary structurally lacks the dangerous verb, (b) the scoped agent
// credential's RBAC lacks the permission
// ([ADR-0038](../../docs/adr/0038-scoped-agent-credential.md)), and (c) the control-plane
// guardrails gate the rest server-side
// ([ADR-0006](../../docs/adr/0006-guardrails-in-the-control-plane.md),
// [ADR-0021](../../docs/adr/0021-guardrails-require-control-plane-only-agent-access.md)).
// TestAdminVerbsAbsent (commands_test.go) checks layer (a) against a hand-listed set of verbs
// that must stay absent. This file is the complementary CLOSED check: the command tree must
// match an explicit allow-list exactly, so ANY new verb fails — including one nobody predicted —
// and its author has to state which side of the line it falls on.
//
// [ADR-0065](../../docs/adr/0065-what-belongs-on-the-agent-surface.md) supplies the MEMBERSHIP rule
// this file enforces. A capability qualifies unless it fails one of two tests: scope (its effect
// reaches beyond the app the agent was asked about) or reversibility (a human cannot restore the
// prior state). Failing scope is disqualifying outright — tier 1, absent from the binary and its
// absence asserted here. Failing reversibility alone means the operator decides, so it ships as a
// guardrail denied by default (tier 2) rather than being removed; §5 prefers that, because a legible
// `denied: app.delete` is a refusal an agent can relay while `unknown command` is a dead end that
// invites it to reach for kubectl. `addon remove` is the tier-1 case: one add-on instance per type
// per environment (ADR-0067 §1), every app in that environment's database on it (ADR-0031), so
// removing it takes them all down.
//
// This is the ONLY place the rule is ENFORCED. Burrow once carried a second agent-facing surface,
// the MCP server, with its own copy of these guards; ADR-0062 removed it precisely so a rule about
// what an agent may do has one enforcement point instead of two that can drift apart. A new
// agent-facing surface must not reintroduce a parallel allow-list.
//
// The membership LIST it enforces against lives in internal/agentsurface rather than in this file,
// because that table has a second reader: `guard` reports the capabilities absent from this binary,
// with what each is and who can perform it, so an agent can relay a refusal instead of hitting
// `unknown command` (ADR-0065 §7 — the reasoning is in that package's doc comment). Two
// hand-maintained lists would drift, and the drift would be silent in the direction that matters:
// one list would say a verb is off the surface while the other told an agent nothing about it.
// One table, each entry tagged with the surface that carries it, cannot drift from itself.

// agentSurfaceAllowList is the complete set of command paths burrow-agent may register, keyed by
// path with the reason each one is app lifecycle rather than cluster administration. It is the
// Agent half of the capability catalogue in internal/agentsurface; the Operator half is what
// `guard` reports as absent. Adding a command means adding it to that catalogue with its reason —
// see the failure message on TestAgentSurfaceIsClosed for how to decide whether it belongs at all.
var agentSurfaceAllowList = agentsurface.AgentSurface()

// adminVerbFragments are fragments that mark a command path as one the agent must not carry:
// cluster administration — creating namespaces, writing RBAC, installing or replacing the control
// plane itself, admitting nodes — plus the tier-1 verbs of
// [ADR-0065](../../docs/adr/0065-what-belongs-on-the-agent-surface.md) §2, whose blast radius
// reaches past the app the agent was asked about. They are matched against the command path with
// its separators normalized to `_`, so `cluster capacity` reads as `cluster_capacity`. This is a
// second, independent net under the allow-list, so an obviously-wrong verb fails loudly even if the
// allow-list were updated carelessly. It is tuned to this binary's naming convention, which is the
// only agent-facing one Burrow has (ADR-0062).
var adminVerbFragments = []string{
	"install",     // control-plane and cluster-component installation
	"uninstall",   //
	"upgrade",     // replacing the running control plane
	"bootstrap",   // standing a cluster up
	"join",        // admitting a node
	"node",        // node lifecycle
	"cluster_",    // cluster lifecycle (`cluster` itself is a read; `cluster <verb>` is not)
	"namespace",   // creating or granting on namespaces
	"rbac",        //
	"role",        // Role / ClusterRole (note: "rollback" does not contain it)
	"binding",     // RoleBinding / ClusterRoleBinding
	"serviceacco", // ServiceAccount, in either spelling
	"service_acc", //
	"permission",  //
	"grant",       //
	"kubeconfig",  // handing out or minting cluster credentials
	"kubectl",     // arbitrary cluster access by another name
	"apply",       // arbitrary manifest application
	"manifest",    //
	"helm",        //
	"guard_set",   // rewriting the agent's own guardrails (ADR-0020)
	"env_add",     // registering an environment creates a namespace
	"env_create",  //
	"environment_add",
	"environment_create",
	"credential",
	"registry_login", // a registry credential is a human/CLI step (ADR-0030)
	"provider_add",   // a provider credential is a human/CLI step (ADR-0030)
	"secret_set",     // a secret VALUE never crosses the agent channel (ADR-0029)
	// Add-ons are one instance per type per cluster, and ADR-0031 puts every app's database on
	// the one shared Postgres, so this removes THE add-on and takes every attached app with it.
	// ADR-0065 §2, tier 1. The whole path is the fragment on purpose: a bare "remove" would
	// shadow `domain remove`, which is app-scoped and allowed.
	"addon_remove",
}

// adminFragmentExceptions are the allow-listed paths that match an adminVerbFragments entry for a
// reason that has been examined and does not put them on the operator's side of the line. Keep it
// tiny and keep the reason next to it; an entry here is a claim that needs to stay true.
var adminFragmentExceptions = map[string]string{
	// Add-on install deploys a building block (Postgres, logs, metrics) into the control plane's
	// OWN add-on namespace through burrowd, which holds only namespaced Roles and is deliberately
	// forbidden from creating namespaces or RBAC (controlplane/kube/addons.go). Its counterpart
	// `addon remove` gets NO exception: it is tier 1 (ADR-0065 §2) and must stay caught.
	"addon install": "deploys into the existing add-on namespace; burrowd cannot create a namespace or RBAC",
	// A read of scheduling headroom from the Kubernetes API. It changes nothing and is grouped
	// under `cluster` for discoverability, not because it administers one.
	"cluster capacity": "read-only scheduling headroom; creates nothing",
}

// surfaceGuardRationale is appended to every failure in this file so the message teaches rather
// than merely fails.
const surfaceGuardRationale = `
Why this test exists:

  burrow-agent is deliberately NARROWER than the ` + "`burrow`" + ` operator CLI. The operator CLI
  carries install, upgrade, uninstall, bootstrap, join, cluster setup, credentials, ` + "`env add`" + `,
  and ` + "`guard set`" + ` — the verbs that create namespaces, write RBAC, and change the shape of the
  cluster. burrow-agent carries app lifecycle only.

  An agent reads untrusted input by its nature (a repository, an error message, a log line), so
  prompt injection is a live path into whatever the agent can express. A verb here that creates a
  namespace or writes RBAC would let an agent WIDEN ITS OWN CONTROL PLANE'S ACCESS on the user's
  cluster — granting burrowd write access to a namespace of the agent's choosing — and the
  boundary the operator believed they had would be gone. That is a self-hoster's cluster, their
  other workloads, and their data.

  See ADR-0049 §2 (three independent layers; this is layer (a), the structural one), ADR-0038
  (the scoped agent credential, layer (b)), and ADR-0006 / ADR-0021 (the control-plane
  guardrails, layer (c)).

What to do:

  - If the verb is CLUSTER ADMINISTRATION — it creates a namespace, writes or modifies RBAC,
    installs/upgrades/uninstalls the control plane, or admits a node — it belongs in
    cmd/burrow (the operator CLI), run by a human with their own kubeconfig. Move it there and
    record it in internal/agentsurface as an Operator capability, so ` + "`guard`" + ` can tell an
    agent what it is and who runs it instead of failing with ` + "`unknown command`" + `.
  - If its effect reaches BEYOND THE APP the agent was asked about, it is ADR-0065 tier 1: it
    stays out of this binary, its absence is asserted here, and it is an Operator entry in the
    catalogue. ` + "`addon remove`" + ` is the case that set the rule.
  - If it is app-scoped but IRREVERSIBLE, it is ADR-0065 tier 2: keep it on the surface and ship
    it with a guardrail whose default disposition is deny, so an operator can relax it per
    environment and the agent gets a refusal it can explain.
  - If it genuinely is APP LIFECYCLE, add it to the catalogue in internal/agentsurface as an Agent
    capability with a one-line reason, name the tier it lands in, and make sure that reason is
    true.`

// TestAgentSurfaceIsClosed asserts burrow-agent's command tree matches agentSurfaceAllowList
// exactly, in both directions. It is a closed set on purpose: ANY new command fails this test, so
// a contributor adding one has to state which side of the operator/agent line it falls on rather
// than inheriting the agent's trust by default.
func TestAgentSurfaceIsClosed(t *testing.T) {
	registered := registeredCommandPaths()

	var added []string
	for _, path := range registered {
		if _, ok := agentSurfaceAllowList[path]; !ok {
			added = append(added, path)
		}
	}
	if len(added) > 0 {
		t.Errorf("burrow-agent registers %d command(s) that are not on the allow-list: %s\n%s",
			len(added), strings.Join(quoteAll(added), ", "), surfaceGuardRationale)
	}

	have := map[string]bool{}
	for _, path := range registered {
		have[path] = true
	}
	var stale []string
	for path := range agentSurfaceAllowList {
		if !have[path] {
			stale = append(stale, path)
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Errorf("the capability catalogue names %d command(s) as agent capabilities that are no longer "+
			"registered: %s\n"+
			"If the verb was deliberately taken off the agent surface, RETAG it in internal/agentsurface\n"+
			"as an Operator capability with why it is held back and who can run it — do not delete the\n"+
			"entry. Retagging keeps it legible in `guard`, which is what lets an agent say \"that is not\n"+
			"something I can do, and here is who can\" instead of hitting `unknown command` and looking\n"+
			"for a way around the control channel (ADR-0065 §5, §7).",
			len(stale), strings.Join(quoteAll(stale), ", "))
	}
}

// TestAgentSurfaceHasNoAdminVerbs is the independent second net: no registered command path
// carries a cluster-administration fragment. It catches an obviously-wrong verb even if the
// allow-list above were updated without thought.
func TestAgentSurfaceHasNoAdminVerbs(t *testing.T) {
	for _, path := range registeredCommandPaths() {
		if frag := matchAdminFragment(path); frag != "" {
			t.Errorf("command %q contains the cluster-administration fragment %q.\n%s\n\n"+
				"  (If the name merely reads that way and the verb really is app lifecycle, add it to\n"+
				"  adminFragmentExceptions with the reason — but read the reason back first.)",
				path, frag, surfaceGuardRationale)
		}
	}

	// The exceptions list may only excuse a path the allow-list already accounts for, so it can
	// never become a second, quieter way onto the surface.
	for path := range adminFragmentExceptions {
		if _, ok := agentSurfaceAllowList[path]; !ok {
			t.Errorf("adminFragmentExceptions excuses %q, which is not on agentSurfaceAllowList; "+
				"the exceptions list is not a way onto the agent surface", path)
		}
	}
}

// TestAdminVerbFragmentsCatchTheObviousCases exercises the deny-list itself against command paths
// that do not exist and must never exist, so the net is known to have no hole where the dangerous
// verbs are — and so thinning adminVerbFragments fails here instead of passing silently. The
// second table pins the paths that merely READ like administration and must keep passing.
func TestAdminVerbFragmentsCatchTheObviousCases(t *testing.T) {
	mustCatch := []string{
		"install", "uninstall", "upgrade", "bootstrap", "join", "node join",
		"cluster create", "cluster join", "cluster bootstrap", "cluster ingress install",
		"namespace create", "create namespace", "rbac apply", "role create",
		"rolebinding create", "clusterrolebinding create", "serviceaccount create",
		"service account token", "grant", "permission add", "kubeconfig", "kubectl",
		"apply", "manifest apply", "helm install", "guard set", "env add", "environment create",
		"credential add", "config registry login", "config provider add", "secret set",
		"addon remove", // tier 1 (ADR-0065 §2): removes THE add-on, taking every attached app with it
	}
	for _, path := range mustCatch {
		if matchAdminFragment(path) == "" {
			t.Errorf("matchAdminFragment(%q) = \"\": a cluster-administration verb would pass the "+
				"deny-list. Add the fragment that names it to adminVerbFragments.", path)
		}
	}

	mustPass := []string{
		"rollback", "deploy", "build", "scale", "run", "logs", "cluster", "environments",
		"config set", "secret unset", "expose", "domain add", "next-tag", "reachability",
	}
	for _, path := range mustPass {
		if frag := matchAdminFragment(path); frag != "" {
			t.Errorf("matchAdminFragment(%q) = %q: an app-lifecycle verb is caught by the deny-list; "+
				"narrow the fragment so it does not shadow app operations.", path, frag)
		}
	}
}

// matchAdminFragment reports the first cluster-administration fragment a command path carries, or
// "" if it carries none (or is an examined exception). Separators are normalized to `_` so a
// multi-word path is matched the same way a single verb is.
func matchAdminFragment(path string) string {
	if _, exempt := adminFragmentExceptions[path]; exempt {
		return ""
	}
	normalized := strings.NewReplacer(" ", "_", "-", "_").Replace(path)
	for _, frag := range adminVerbFragments {
		if strings.Contains(normalized, frag) {
			return frag
		}
	}
	return ""
}

// registeredCommandPaths returns every command path the real burrow-agent command tree registers,
// so the test reads the shipped surface rather than a copy of the registration code. It walks the
// tree with the same commandPaths helper `guard` uses to work out what is absent (surface.go), so
// the enforcement below and the report an agent reads are computed from one traversal, not two
// that could disagree about what "registered" means.
func registeredCommandPaths() []string {
	return commandPaths(newRootCmd())
}

func quoteAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = `"` + s + `"`
	}
	return out
}
