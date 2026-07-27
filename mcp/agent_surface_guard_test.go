// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package mcp_test

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"testing"
)

// Burrow ships two command surfaces on purpose, and the split between them is a security
// property rather than an accident of packaging.
//
//   - `burrow` (cmd/burrow) is the OPERATOR CLI. It carries install, upgrade, uninstall,
//     bootstrap, join, cluster setup, registry and provider credentials, `env add`, and
//     `guard set` — the verbs that create namespaces, write RBAC, and change the shape of the
//     cluster. A human runs them at their own terminal with their own admin kubeconfig.
//   - The agent-facing surface (this package, and its sibling `burrow-agent`) carries APP
//     LIFECYCLE only: deploy, build, scale, rollback, run, logs, metrics, config, secrets,
//     add-ons, domains, expose. Notably burrow_environments LISTS and SELECTS environments;
//     it does not create one.
//
// The reason for the split is that an agent reads untrusted input by its nature — a repository
// it was pointed at, an error message, a log line — so prompt injection is a live path into
// whatever the agent can express. If the agent surface ever gained a verb that creates a
// namespace or writes RBAC, an agent could WIDEN ITS OWN CONTROL PLANE'S ACCESS on the user's
// cluster: grant burrowd write access to a namespace of the agent's choosing, and the boundary
// the operator believed they had is gone. That is a self-hoster's cluster, their other
// workloads, and their data.
//
// The safety of pointing an agent at a real cluster rests on three independent layers
// ([ADR-0049](../docs/adr/0049-burrow-agent-scoped-cli-control-channel.md) §2): (a) the agent's
// surface structurally lacks the dangerous verb, (b) the scoped agent credential's RBAC lacks
// the permission ([ADR-0038](../docs/adr/0038-scoped-agent-credential.md)), and (c) the
// control-plane guardrails gate the rest server-side
// ([ADR-0006](../docs/adr/0006-guardrails-in-the-control-plane.md),
// [ADR-0021](../docs/adr/0021-guardrails-require-control-plane-only-agent-access.md)). This file
// is the automated check on layer (a) for the MCP tool surface: layer (a) held only because of
// how the tools happen to be registered, with nothing that fails when the shape changes.
//
// The check is a CLOSED allow-list, deliberately: any new tool trips it, including verbs nobody
// predicted, so the author has to decide consciously which side of the line their verb falls on.

// agentSurfaceAllowList is the complete set of tools the agent-facing MCP surface may register,
// each with the reason it is app lifecycle rather than cluster administration. Adding a tool
// means adding it here with its reason — see the failure message on TestAgentSurfaceIsClosed for
// how to decide whether it belongs here at all.
var agentSurfaceAllowList = map[string]string{
	// Compute: the release lifecycle of an app that already has a namespace.
	"burrow_deploy":     "deploys an existing app by image reference into its own namespace",
	"burrow_rollback":   "returns an app to a previous release",
	"burrow_scale":      "sets an app's replica count",
	"burrow_autoscale":  "configures an app's HorizontalPodAutoscaler",
	"burrow_run":        "runs a one-off command in the app's own image, guarded by app.run",
	"burrow_app_delete": "removes an app the agent's control plane already manages, guarded",
	"burrow_status":     "read-only: one app's release and workload state",
	"burrow_apps":       "read-only: the apps the control plane manages",

	// Config and secrets: app-scoped values. Setting a secret VALUE is deliberately absent
	// (ADR-0029) — see TestAgentSurfaceHasNoAdminVerbs.
	"burrow_config_set":    "writes a non-secret config var on one app",
	"burrow_config_list":   "read-only: one app's non-secret config vars",
	"burrow_config_unset":  "removes a non-secret config var from one app",
	"burrow_secret_list":   "read-only: one app's secret KEY names, never values",
	"burrow_secret_unset":  "removes one app secret by key; carries no value",
	"burrow_addon_attach":  "gives one app a database on the installed add-on; the URL is generated server-side",
	"burrow_addon_backup":  "takes a backup of an add-on's data",
	"burrow_addon_backups": "read-only: the backups taken of an add-on",

	// Add-ons: in-cluster building blocks the control plane deploys into its OWN add-on
	// namespace. burrowd holds only namespaced Roles and is forbidden from creating namespaces
	// or RBAC, so an add-on install cannot widen anything — where an add-on needs a
	// ServiceAccount (the metrics scraper), burrowd only VERIFIES the operator staged it and
	// otherwise fails cleanly (controlplane/kube/addons.go).
	"burrow_addon_install": "deploys a building block into the existing add-on namespace; creates no namespace and no RBAC",
	"burrow_addon_remove":  "removes a building block the control plane installed",
	"burrow_addons":        "read-only: the installed add-ons and their capabilities",

	// Routing: Services, Ingresses, and DNS records for an app.
	"burrow_expose":        "creates a Service and Ingress for one app, guarded",
	"burrow_unexpose":      "removes an app's Service and Ingress",
	"burrow_domain_add":    "writes a DNS record at an already-configured provider, guarded",
	"burrow_domain_remove": "removes a DNS record at an already-configured provider",
	"burrow_reachability":  "read-only: whether an app is reachable, link by link",

	// Diagnosis: reads only.
	"burrow_logs":          "read-only: one app's pod logs",
	"burrow_logs_query":    "read-only: queries the installed logs add-on",
	"burrow_metrics_query": "read-only: queries the installed metrics add-on",
	"burrow_capacity":      "read-only: scheduling headroom, from the Kubernetes API",
	"burrow_cluster":       "read-only: the cluster's capabilities",
	"burrow_audit":         "read-only: the audit record of guarded operations",
	"burrow_guard":         "read-only: the current guardrail dispositions; `guard set` is operator-only (ADR-0020)",

	// Targeting: selects among environments the OPERATOR registered. It does not create one —
	// registering an environment is `burrow env add`, and it creates a namespace.
	"burrow_providers":    "read-only: the provider names already configured by the operator",
	"burrow_environments": "read-only: lists the local environment handles; selects, never creates",
}

// adminVerbFragments are name fragments that mark a verb as cluster administration: creating
// namespaces, writing RBAC, installing or replacing the control plane itself, or admitting nodes.
// A tool whose name contains one is almost certainly on the operator's side of the line. This is
// a second, independent net under the allow-list, so an obviously-wrong tool fails loudly even if
// the allow-list were updated carelessly.
var adminVerbFragments = []string{
	"install",     // control-plane and cluster-component installation
	"uninstall",   //
	"upgrade",     // replacing the running control plane
	"bootstrap",   // standing a cluster up
	"join",        // admitting a node
	"node_",       // node lifecycle
	"cluster_",    // cluster lifecycle (burrow_cluster itself is a read; `burrow_cluster_*` is not)
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
	"secret_set", // a secret VALUE never crosses the agent channel (ADR-0029)
}

// adminFragmentExceptions are the allow-listed tools that match an adminVerbFragments entry for a
// reason that has been examined and does not put them on the operator's side of the line. Keep it
// tiny and keep the reason next to it; an entry here is a claim that needs to stay true.
//
// Add-on install/remove deploy a building block (Postgres, logs, metrics) into the control
// plane's OWN add-on namespace through burrowd, which holds only namespaced Roles and is
// deliberately forbidden from creating namespaces or RBAC (controlplane/kube/addons.go). They
// create no namespace, write no RBAC, and touch neither the control plane's own installation nor
// the cluster's membership.
var adminFragmentExceptions = map[string]string{
	"burrow_addon_install": "deploys into the existing add-on namespace; burrowd cannot create a namespace or RBAC",
	"burrow_addon_remove":  "removes what burrow_addon_install deployed",
}

// surfaceGuardRationale is appended to every failure in this file so the message teaches rather
// than merely fails.
const surfaceGuardRationale = `
Why this test exists:

  The agent-facing surface is deliberately NARROWER than the ` + "`burrow`" + ` operator CLI. The
  operator CLI carries install, upgrade, uninstall, bootstrap, join, cluster setup, credentials,
  ` + "`env add`" + `, and ` + "`guard set`" + ` — the verbs that create namespaces, write RBAC, and change
  the shape of the cluster. The agent surface carries app lifecycle only.

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
    cmd/burrow (the operator CLI), run by a human with their own kubeconfig. Move it there.
  - If it genuinely is APP LIFECYCLE, add it to agentSurfaceAllowList in this file with a
    one-line reason, and make sure that reason is true.`

// TestAgentSurfaceIsClosed asserts the registered MCP tool set matches agentSurfaceAllowList
// exactly, in both directions. It is a closed set on purpose: ANY new tool fails this test, so a
// contributor adding one has to state which side of the operator/agent line it falls on rather
// than inheriting the agent's trust by default.
func TestAgentSurfaceIsClosed(t *testing.T) {
	registered := registeredToolNames(t)

	var added []string
	for _, name := range registered {
		if _, ok := agentSurfaceAllowList[name]; !ok {
			added = append(added, name)
		}
	}
	if len(added) > 0 {
		t.Errorf("the agent-facing MCP surface registers %d tool(s) that are not on the allow-list: %s\n%s",
			len(added), strings.Join(added, ", "), surfaceGuardRationale)
	}

	have := map[string]bool{}
	for _, name := range registered {
		have[name] = true
	}
	var stale []string
	for name := range agentSurfaceAllowList {
		if !have[name] {
			stale = append(stale, name)
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Errorf("agentSurfaceAllowList names %d tool(s) that are no longer registered: %s\n"+
			"Remove them from the allow-list so it keeps describing the real surface.",
			len(stale), strings.Join(stale, ", "))
	}
}

// TestAgentSurfaceHasNoAdminVerbs is the independent second net: no registered tool's name
// carries a cluster-administration fragment. It catches an obviously-wrong verb even if the
// allow-list above were updated without thought.
func TestAgentSurfaceHasNoAdminVerbs(t *testing.T) {
	for _, name := range registeredToolNames(t) {
		if frag := matchAdminFragment(name); frag != "" {
			t.Errorf("tool %q contains the cluster-administration fragment %q.\n%s\n\n"+
				"  (If the name merely reads that way and the verb really is app lifecycle, add it to\n"+
				"  adminFragmentExceptions with the reason — but read the reason back first.)",
				name, frag, surfaceGuardRationale)
		}
	}

	// The exceptions list may only excuse a tool the allow-list already accounts for, so it can
	// never become a second, quieter way onto the surface.
	for name := range adminFragmentExceptions {
		if _, ok := agentSurfaceAllowList[name]; !ok {
			t.Errorf("adminFragmentExceptions excuses %q, which is not on agentSurfaceAllowList; "+
				"the exceptions list is not a way onto the agent surface", name)
		}
	}
}

// TestAdminVerbFragmentsCatchTheObviousCases exercises the deny-list itself against tool names
// that do not exist and must never exist, so the net is known to have no hole where the dangerous
// verbs are — and so thinning adminVerbFragments fails here instead of passing silently. The
// second table pins the names that merely READ like administration and must keep passing.
func TestAdminVerbFragmentsCatchTheObviousCases(t *testing.T) {
	mustCatch := []string{
		"burrow_install", "burrow_uninstall", "burrow_upgrade", "burrow_bootstrap",
		"burrow_join", "burrow_node_join", "burrow_cluster_create", "burrow_cluster_join",
		"burrow_namespace_create", "burrow_create_namespace", "burrow_rbac_apply",
		"burrow_role_create", "burrow_rolebinding_create", "burrow_clusterrolebinding_create",
		"burrow_serviceaccount_create", "burrow_service_account_token", "burrow_grant",
		"burrow_permission_add", "burrow_kubeconfig", "burrow_kubectl", "burrow_apply",
		"burrow_manifest_apply", "burrow_helm_install", "burrow_guard_set", "burrow_env_add",
		"burrow_environment_create", "burrow_credential_add", "burrow_secret_set",
	}
	for _, name := range mustCatch {
		if matchAdminFragment(name) == "" {
			t.Errorf("matchAdminFragment(%q) = \"\": a cluster-administration verb would pass the "+
				"deny-list. Add the fragment that names it to adminVerbFragments.", name)
		}
	}

	mustPass := []string{
		"burrow_rollback", "burrow_deploy", "burrow_scale", "burrow_run", "burrow_logs",
		"burrow_cluster", "burrow_environments", "burrow_config_set", "burrow_secret_unset",
		"burrow_expose", "burrow_domain_add", "burrow_capacity",
	}
	for _, name := range mustPass {
		if frag := matchAdminFragment(name); frag != "" {
			t.Errorf("matchAdminFragment(%q) = %q: an app-lifecycle verb is caught by the deny-list; "+
				"narrow the fragment so it does not shadow app operations.", name, frag)
		}
	}
}

// matchAdminFragment reports the first cluster-administration fragment a tool name carries, or ""
// if it carries none (or is an examined exception).
func matchAdminFragment(name string) string {
	if _, exempt := adminFragmentExceptions[name]; exempt {
		return ""
	}
	for _, frag := range adminVerbFragments {
		if strings.Contains(name, frag) {
			return frag
		}
	}
	return ""
}

// registeredToolNames enumerates the tools the MCP server actually registers, over a real client
// session, so the test reads the shipped surface rather than a copy of the registration code.
func registeredToolNames(t *testing.T) []string {
	t.Helper()
	cs := connect(t, func(_ http.ResponseWriter, _ *http.Request) {})
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	names := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	return names
}
