// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

// Package e2e holds end-to-end tests that drive the deploy engine through its real
// adapters — the client-go Kubernetes adapter against a live cluster and the registry
// resolver against a real registry — proving the whole vertical slice composes
// (ADR-0010). The tests are gated on BURROW_TEST_KUBECONFIG and run in CI's k3d job.
package e2e
