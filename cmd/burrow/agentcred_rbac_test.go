// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"os"
	"testing"
)

// TestAgentCredentialRBACConfinement is the placeholder for the live RBAC-confinement verification
// of the scoped agent credential (ADR-0038). The seam-isolated tests in this package pin the
// rendered manifest (the exact Role rules) and the mint/write behavior, but they cannot prove that
// Kubernetes actually ENFORCES the confinement. That requires a live API server.
//
// This is a deferred follow-up: Burrow's integration path today is a heavy k3d cluster
// (scripts/with-k3d.sh) and the module carries no controller-runtime/envtest dependency, so wiring
// a real envtest harness would add a heavy dependency out of scope for ADR-0038 phase 1. The test
// therefore skips cleanly (and the package still compiles) whenever the envtest binaries are absent.
//
// When wired against a live API server, this test must apply the rendered install manifest, wait
// for the token controller to populate the burrow-agent-token Secret, and — acting AS the
// burrow-agent ServiceAccount token — assert via SelfSubjectAccessReview (or live requests) that the
// ServiceAccount:
//
//   - CAN `get`/`create`/`update`/`patch`/`delete` services/proxy for the `burrowd` Service (and
//     its port/scheme resourceName variants) in the control-plane namespace;
//   - CAN `get` the `burrowd-api-token` Secret in the control-plane namespace;
//
// and is DENIED everything else, specifically:
//
//   - proxy to any OTHER Service (e.g. `postgres`);
//   - `get` on any OTHER Secret (e.g. `burrowd-db`, `burrow-credentials`);
//   - `list`/`watch` on Secrets or Pods in any namespace (no enumeration);
//   - `get`/`list` on Pods, Deployments, or any workload;
//   - any access in the app or add-on namespaces;
//   - any cluster-scoped read (e.g. nodes).
//
// The same live harness should also exercise the Phase 3 multi-user JOIN (ADR-0038 §4): install as an
// admin identity, then have a SECOND, restricted identity run the join path (joinAgentCredential /
// `burrow install <context>` on the already-installed cluster) and assert it reads the existing
// burrow-agent-token Secret and writes a WORKING scoped kubeconfig without minting any cluster
// resources; and assert that a third identity with NO read access to that Secret gets the clear,
// actionable error (readAgentToken's forbidden message) rather than a silent success. The
// seam-isolated tests here and in agentcred_test.go pin the join's read/build/write behavior and the
// actionable errors against fakes, but only a live API server proves the RBAC actually gates the
// second and third identities as intended.
func TestAgentCredentialRBACConfinement(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("live RBAC-confinement verification needs a real API server (KUBEBUILDER_ASSETS); " +
			"deferred follow-up — see the test doc comment for the confinement and multi-user-join matrices to assert")
	}
	t.Skip("envtest harness for ADR-0038 RBAC confinement and multi-user join is not yet wired (deferred follow-up)")
}
