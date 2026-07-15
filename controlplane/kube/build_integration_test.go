// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube_test

import (
	"testing"
)

// TestBuildIntegration is the honest tracker for the live, end-to-end in-cluster build
// (ADR-0053, the real Builder Job adapter). Phase 3 shipped the push target the live build needs —
// the optional in-cluster Zot registry and its k3s containerd config (`burrow cluster registry
// install`; controlplane/kube installs no registry itself, the CLI does, ADR-0053 §5). What
// still blocks a reliable live run in ONE PR, and so keeps this test skipped rather than green:
//
//   - The build container must push to the in-cluster registry over plain HTTP (tls-verify=false for
//     buildah, plain-http for the CNB lifecycle); the Phase 2 build recipe (build.go) pushes with
//     TLS defaults, so an insecure-push mode is a deliberate, separate change.
//   - The k3d harness (scripts/with-k3d.sh) creates a bare cluster with no registries.yaml mirror, so
//     k3d's containerd would need the same config `burrow cluster registry install` writes on a
//     k3s node before the node could pull the pushed reference.
//   - The build clones from a git source, so a live run reaches the network to fetch a tiny fixture
//     repo — a flake surface a single install/config PR should not take on.
//
// It stays skipped rather than absent, per CLAUDE.md: a skipped test that names the ADR is the honest
// record of decided-but-unbuilt behavior. When it lands green it drives a real build of a tiny source
// repo (both the Dockerfile/buildah and the no-Dockerfile/Buildpacks path), pushes to the in-cluster
// registry, and asserts the returned digest is pullable. Until then the adapter is covered by the
// seam-isolated unit tests in build_test.go (the full Job spec, the watch-to-completion happy path,
// and the failure/timeout paths against the fake Kubernetes client), and the registry install and its
// containerd config are covered by the CLI's rendering/config unit tests.
func TestBuildIntegration(t *testing.T) {
	t.Skip("ADR-0053: the live in-cluster build+push+pull test needs an insecure-push build mode and a registry-configured k3d harness; tracked skipped")
}
