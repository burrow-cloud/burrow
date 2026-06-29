// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

// Package controlplane is the Burrow control plane: the product. It runs deploy
// orchestration, rollout and rollback, logs and status, scaling, the safety
// guardrails, and the record of who deployed what. It holds the cluster credentials
// and is the only layer that talks to Kubernetes.
//
// This is the public API surface of the control plane — kept importable by a separate
// private module (the managed product). It currently holds the domain types
// (App, Release, Policy) and the seam interfaces (Clock, Kubernetes, Registry,
// Database) that core logic is written against (ADR-0010); the deploy engine and its
// constructor land in later phases (docs/PLAN.md). The implementation guts live in
// controlplane/internal, and in-memory fakes of the seams live in
// controlplane/internal/fake for tests.
//
// This package is source-available under FSL-1.1-ALv2, which converts to Apache-2.0
// two years after each release ships (see LICENSING.md and ADR-0001).
package controlplane
