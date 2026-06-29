// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

// Package mcp is the Burrow MCP server: the thin, agent-neutral control surface
// (ADR-0003) that exposes Burrow's operations as MCP tools to any MCP client and
// translates tool calls into control-plane API calls. It holds NO cluster credentials
// (ADR-0005) and contains no orchestration logic — it is the remote control, not the
// engine. NewServer builds the tool surface over a control-plane API Client; Serve runs
// it over stdio. The binary is cmd/burrow-mcp.
//
// This package is Apache-2.0 licensed (the client surface; see LICENSING.md and
// ADR-0033). It deliberately does not import the controlplane/ packages —
// it talks to the control plane only over the HTTP API, with its own request/response
// types, so the module boundary stays clean.
package mcp
