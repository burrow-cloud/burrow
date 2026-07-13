// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube_test

import (
	"testing"
)

// TestBuildIntegration is the honest tracker for the live, end-to-end in-cluster build
// (ADR-0053 Phase 2, the real Builder Job adapter). A live build must PUSH the image it produces to
// a registry the cluster can pull from, and Burrow's zero-config push target — the optional
// in-cluster Zot registry (ADR-0053 §5) — is Phase 3 work that is not yet built. Without a push
// target reachable from inside the k3d cluster, a real buildah/Buildpacks build has nowhere to land,
// so this test would assert nothing it can stand behind.
//
// It is deliberately skipped rather than absent, per CLAUDE.md: a skipped test that names the ADR is
// the honest record of decided-but-unbuilt behavior. It lands, unskipped, with the Phase 3
// in-cluster registry — driving a real build of a tiny source repo (both the Dockerfile/buildah path
// and the no-Dockerfile/Buildpacks path), pushing to the in-cluster registry, and asserting the
// returned digest is pullable. Until then the adapter is covered by the seam-isolated unit tests in
// build_test.go, which assert the full Job spec, the watch-to-completion happy path, and the
// failure/timeout paths against the fake Kubernetes client.
func TestBuildIntegration(t *testing.T) {
	t.Skip("ADR-0053 Phase 2: the live in-cluster build test lands with the Phase 3 in-cluster registry (its push target)")
}
