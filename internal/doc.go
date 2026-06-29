// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

// Package internal is the root of Burrow's module-private shared helpers, licensed
// Apache-2.0. It holds only small, cross-cutting utilities shared across the module
// that carry no license restriction.
//
// The licensed product code does NOT live here: the control plane lives under
// controlplane/ and the operator under operator/, kept out of
// this top-level internal/ precisely so the separate private managed module can
// import them. See LICENSING.md, ADR-0001, and CLAUDE.md for the package layout, and
// docs/PLAN.md for what the v0.1 slice adds.
package internal
