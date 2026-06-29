// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

// Package internal holds the control plane's implementation guts: the deploy state
// machine, the Kubernetes/registry/database seam adapters, and the guardrail policy.
// It is importable only within controlplane/ (Go's internal/ rule).
//
// Licensed Apache-2.0 (see LICENSING.md and ADR-0033). No
// implementation has shipped yet; this is a placeholder so the package layout is real.
package internal
