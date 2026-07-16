// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube_test

import (
	"testing"
)

// TestBuildIntegration is the honest tracker for the live, end-to-end in-cluster build
// (ADR-0053, the real Builder Job adapter). The push target the live build needs is the optional
// in-cluster Zot registry `burrow cluster registry install` deploys — an internal Service the build
// pushes to in-cluster, with nodes pulling through the cluster ingress over TLS, no node/containerd
// editing (ADR-0054 §5; controlplane/kube installs no registry itself, the CLI does). What still
// blocks a reliable live run in ONE PR, and so keeps this test skipped rather than green:
//
//   - The build container must push to the INTERNAL registry Service over plain HTTP (tls-verify=false
//     for buildah, plain-http for the CNB lifecycle); the Phase 2 build recipe (build.go) pushes with
//     TLS defaults, so an insecure-push mode for the in-cluster endpoint is a deliberate, separate
//     change. (The node PULL needs no such mode: it goes through the ingress over a trusted cert.)
//   - The build clones from a git source, so a live run reaches the network to fetch a tiny fixture
//     repo — a flake surface a single install/config PR should not take on.
//
// It stays skipped rather than absent, per CLAUDE.md: a skipped test that names the ADR is the honest
// record of decided-but-unbuilt behavior. When it lands green it drives a real build of a tiny source
// repo (both the Dockerfile/buildah and the no-Dockerfile/Buildpacks path), pushes to the internal
// registry Service, and asserts the resulting public reference is pullable through the ingress. Until
// then the adapter is covered by the seam-isolated unit tests in build_test.go (the full Job spec, the
// watch-to-completion happy path, and the failure/timeout paths against the fake Kubernetes client),
// and the registry install is covered by the CLI's rendering/wiring unit tests.
func TestBuildIntegration(t *testing.T) {
	t.Skip("ADR-0053: the live in-cluster build+push+pull test needs an insecure-push build mode for the internal registry endpoint; tracked skipped")
}
