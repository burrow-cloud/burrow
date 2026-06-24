// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package controlplane

import "errors"

// The control plane classifies its failures with these sentinels so a front end (the
// HTTP API, the MCP server) can map them to the right status without parsing prose.
// They complement the typed GuardrailError (a deliberate policy refusal) and ErrNotFound
// (a missing resource, defined with the seams it is returned from).
var (
	// ErrInvalid marks a malformed request — a bad app name, an empty image
	// reference, a negative replica count. The caller must fix the request; retrying
	// it unchanged will fail the same way.
	ErrInvalid = errors.New("invalid request")

	// ErrNotImplemented marks an operation whose backing adapter is not wired in this
	// build yet (e.g. the cluster adapter before it ships). It is an honest-status
	// signal (ADR-0009), distinct from a malformed request or a system failure.
	ErrNotImplemented = errors.New("not implemented")
)
