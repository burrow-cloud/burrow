// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

// Package mcp is the Burrow MCP server: the thin, agent-neutral control surface
// that exposes Burrow's tools to any MCP client and translates tool calls into
// control-plane API calls. It holds NO cluster credentials and contains no
// orchestration logic — it is the remote control, not the engine.
//
// This package is Apache-2.0 licensed (the client surface; see LICENSING.md and
// ADR-0001). It is a thin translator over the control-plane API and does not import
// the FSL-licensed controlplane/ guts. No implementation has shipped yet; this is a
// placeholder so the license boundary and module layout are real. See docs/PLAN.md.
package mcp
