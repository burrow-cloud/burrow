// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

// Package internal holds the control plane's implementation guts: the deploy state
// machine, the Kubernetes/registry/database seam adapters, and the guardrail policy.
// It is importable only within controlplane/ (Go's internal/ rule).
//
// Source-available under FSL-1.1-ALv2 (see LICENSING.md and ADR-0001). No
// implementation has shipped yet; this is a placeholder so the package layout is real.
package internal
