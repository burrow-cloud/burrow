// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

// Package operator is the Burrow Kubernetes operator: the CRD types and the
// reconcile loops that drive the cluster toward the control plane's desired state.
// It is part of the product and shares the control plane's license.
//
// This package is the public API surface — the CRD types and the reconciler entry
// point — kept importable by a separate private module (the managed product). The
// implementation guts live in operator/internal. Licensed Apache-2.0 (see LICENSING.md
// and ADR-0033). No implementation has shipped yet; this is a placeholder so the module
// layout is real. See docs/PLAN.md.
package operator
