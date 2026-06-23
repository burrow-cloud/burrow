// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

// Package controlplane is the Burrow control plane: the product. It runs deploy
// orchestration, rollout and rollback, logs and status, scaling, the safety
// guardrails, and the record of who deployed what. It holds the cluster credentials
// and is the only layer that talks to Kubernetes.
//
// This is the public API surface of the control plane — its interfaces, the
// App/Release/Policy domain types, and the constructor wiring — kept importable by a
// separate private module (the managed product). The implementation guts live in
// controlplane/internal. This package is source-available under FSL-1.1-ALv2, which
// converts to Apache-2.0 two years after each release ships (see LICENSING.md and
// ADR-0001). No implementation has shipped yet; this is a placeholder so the license
// boundary and module layout are real. See docs/PLAN.md.
package controlplane
