// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import "errors"

// The control plane classifies its failures with these sentinels so a front end (the
// HTTP API, and through it the `burrow` CLI and `burrow-agent`) can map them to the
// right status without parsing prose.
// They complement the typed GuardrailError (a deliberate policy refusal).
var (
	// ErrInvalid marks a malformed request — a bad app name, an empty image
	// reference, a negative replica count. The caller must fix the request; retrying
	// it unchanged will fail the same way.
	ErrInvalid = errors.New("invalid request")

	// ErrNotImplemented marks an operation whose backing adapter is not wired in this
	// build yet (e.g. the cluster adapter before it ships). It is an honest-status
	// signal (ADR-0009), distinct from a malformed request or a system failure.
	ErrNotImplemented = errors.New("not implemented")

	// ErrNotFound marks a requested record or resource that does not exist. The seams
	// (seams.go) return it — possibly wrapped — so engine logic can branch on absence
	// with errors.Is without depending on a particular adapter.
	ErrNotFound = errors.New("not found")

	// ErrForbidden marks a request the caller is authenticated for and not permitted to make
	// (ADR-0084 §2): issuing a credential for somebody else without the admin bit, recording a
	// second principal, revoking another principal's token. It is distinct from ErrInvalid — the
	// request is well formed, and re-issuing it unchanged as somebody else would succeed.
	ErrForbidden = errors.New("forbidden")

	// ErrAlreadyClaimed marks a first-principal claim on an install that already has one
	// (ADR-0084 §2). The claim is trust-on-first-use with the window closed at install time, so
	// a second attempt is refused rather than merged: whoever holds the first principal holds
	// the admin bit, and a second claimant would be a silent second admin.
	ErrAlreadyClaimed = errors.New("already claimed")
)
