// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"errors"
	"fmt"
)

// Disposition is how the control plane enforces a guardrail when an operation trips it:
// allow it silently, hold it for explicit confirmation, or deny it outright (ADR-0020).
type Disposition string

const (
	DispositionAllow   Disposition = "allow"
	DispositionConfirm Disposition = "confirm"
	DispositionDeny    Disposition = "deny"
)

// Valid reports whether d is a known disposition.
func (d Disposition) Valid() bool {
	switch d {
	case DispositionAllow, DispositionConfirm, DispositionDeny:
		return true
	default:
		return false
	}
}

// GuardrailCode identifies a guardrail: it is both the key a Policy configures the
// guardrail's disposition under and the machine-readable reason it appears in a refusal,
// so an agent can branch on the cause rather than parse prose (ADR-0006, ADR-0020).
type GuardrailCode string

const (
	// GuardrailReplicaCeiling: the requested replica count exceeds Policy.MaxReplicas.
	GuardrailReplicaCeiling GuardrailCode = "replica_ceiling"
	// GuardrailScaleToZero: the operation would scale to zero replicas.
	GuardrailScaleToZero GuardrailCode = "scale_to_zero"
	// GuardrailExposePublic: the operation would make an app reachable from outside the
	// cluster (expose it at a hostname).
	GuardrailExposePublic GuardrailCode = "expose_public"
	// GuardrailDNSWrite: the operation would create or update a public DNS record at a
	// configured provider (ADR-0018).
	GuardrailDNSWrite GuardrailCode = "dns_write"
	// GuardrailDNSDelete: the operation would delete a public DNS record at a configured
	// provider — the destructive side of DNS management (ADR-0018).
	GuardrailDNSDelete GuardrailCode = "dns_delete"
	// GuardrailAddonInstall: the operation would install a building-block backing service
	// (a vetted add-on like logs or metrics) onto the cluster (ADR-0025).
	GuardrailAddonInstall GuardrailCode = "addon_install"
	// GuardrailAddonRemove: the operation would remove an installed add-on — the destructive
	// side, since dependent apps may rely on it (ADR-0025).
	GuardrailAddonRemove GuardrailCode = "addon_remove"
	// GuardrailAddonDetach: the operation would detach an app from an add-on — for Postgres,
	// dropping the app's database and role and destroying its data (ADR-0031). Held for
	// confirmation by default. (Attach is not guarded: it provisions, it destroys nothing.)
	GuardrailAddonDetach GuardrailCode = "addon_detach"
	// GuardrailAddonRestore: the operation would restore an app's database from a backup,
	// overwriting its live contents (ADR-0032). Held for confirmation by default, like detach and
	// app delete. (Backup and list are not guarded: they destroy nothing.)
	GuardrailAddonRestore GuardrailCode = "addon_restore"
	// GuardrailAppDelete: the operation would delete an app entirely — its workload, routing,
	// and release history — so it disappears from the apps listing. The destructive teardown
	// of a deployed application.
	GuardrailAppDelete GuardrailCode = "app_delete"
	// GuardrailRollback: the operation would roll an app back to its previous release. A
	// production mutation, but a recovery one — allowed by default so an agent can restore a
	// broken app quickly; an operator can set it to confirm or deny to require sign-off for
	// server-side, agent-independent enforcement (ADR-0020).
	GuardrailRollback GuardrailCode = "rollback"
)

// GuardrailInfo describes a guardrail and its current disposition, for inspection through
// `guard list` and the read-only MCP guard tool (ADR-0020).
type GuardrailInfo struct {
	Code        GuardrailCode `json:"code"`
	Disposition Disposition   `json:"disposition"`
	Description string        `json:"description"`
}

// knownGuardrails enumerates every configurable guardrail in a stable order with a human
// description, so inspection shows the full set — including unset ones, which read as their
// default disposition.
var knownGuardrails = []GuardrailInfo{
	{Code: GuardrailReplicaCeiling, Description: "deploy or scale above the replica ceiling"},
	{Code: GuardrailScaleToZero, Description: "scale an application to zero replicas"},
	{Code: GuardrailExposePublic, Description: "expose an application to the public internet at a hostname"},
	{Code: GuardrailDNSWrite, Description: "create or update a public DNS record at a configured provider"},
	{Code: GuardrailDNSDelete, Description: "delete a public DNS record at a configured provider"},
	{Code: GuardrailAddonInstall, Description: "install a building-block add-on (backing service) onto the cluster"},
	{Code: GuardrailAddonRemove, Description: "remove an installed add-on from the cluster"},
	{Code: GuardrailAddonDetach, Description: "detach an app from an add-on, destroying its data (e.g. drop its Postgres database)"},
	{Code: GuardrailAddonRestore, Description: "restore an app's database from a backup, overwriting its live contents"},
	{Code: GuardrailAppDelete, Description: "delete an app entirely (its workload, routing, and release history)"},
	{Code: GuardrailRollback, Description: "roll an application back to its previous release"},
}

// KnownGuardrail reports whether code names a configurable guardrail.
func KnownGuardrail(code GuardrailCode) bool {
	for _, g := range knownGuardrails {
		if g.Code == code {
			return true
		}
	}
	return false
}

// Guardrails returns each known guardrail with its effective disposition under the policy.
func (p Policy) Guardrails() []GuardrailInfo {
	out := make([]GuardrailInfo, len(knownGuardrails))
	for i, g := range knownGuardrails {
		out[i] = GuardrailInfo{Code: g.Code, Disposition: p.disposition(g.Code), Description: g.Description}
	}
	return out
}

// GuardrailError is returned when the control plane declines a dangerous operation or holds
// it for confirmation. It is a structured outcome, not a system failure: the operation was
// understood and deliberately gated. Callers distinguish it with AsGuardrail.
type GuardrailError struct {
	// Operation is the operation that was gated (e.g. "deploy", "scale").
	Operation string
	// Code is the machine-readable guardrail that tripped.
	Code GuardrailCode
	// Message is a human-readable explanation.
	Message string
	// Requested is the value the caller asked for (e.g. the replica count).
	Requested int32
	// Limit is the relevant policy limit, when the code involves one.
	Limit int32
	// NeedsConfirmation is true when the operation was not refused outright but requires
	// explicit confirmation to proceed (disposition confirm). A plain deny leaves it false.
	NeedsConfirmation bool
}

func (e *GuardrailError) Error() string {
	if e.NeedsConfirmation {
		return fmt.Sprintf("guardrail holds %s for confirmation: %s", e.Operation, e.Message)
	}
	return fmt.Sprintf("guardrail refused %s: %s", e.Operation, e.Message)
}

// AsGuardrail reports whether err is (or wraps) a GuardrailError and returns it.
func AsGuardrail(err error) (*GuardrailError, bool) {
	var g *GuardrailError
	if errors.As(err, &g) {
		return g, true
	}
	return nil, false
}

// evaluateReplicas evaluates a requested replica count for op against the policy, given
// whether the caller has confirmed. It returns nil to proceed, or a *GuardrailError that
// either denies the operation or marks it as needing confirmation. It assumes replicas is
// already known non-negative (a negative count is a malformed request, validated
// separately, not a guardrail concern).
func (p Policy) evaluateReplicas(op string, replicas int32, confirmed bool) error {
	if replicas == 0 {
		return p.enforce(op, GuardrailScaleToZero, confirmed, "scaling to zero replicas", 0, 0)
	}
	if replicas > p.MaxReplicas {
		return p.enforce(op, GuardrailReplicaCeiling, confirmed,
			fmt.Sprintf("requested %d replicas exceeds the policy ceiling of %d", replicas, p.MaxReplicas),
			replicas, p.MaxReplicas)
	}
	return nil
}

// enforce applies the configured disposition for a tripped guardrail, producing the right
// structured outcome: proceed (nil), confirmation required, or denied.
func (p Policy) enforce(op string, code GuardrailCode, confirmed bool, what string, requested, limit int32) error {
	switch p.disposition(code) {
	case DispositionAllow:
		return nil
	case DispositionConfirm:
		if confirmed {
			return nil
		}
		return &GuardrailError{
			Operation:         op,
			Code:              code,
			Requested:         requested,
			Limit:             limit,
			NeedsConfirmation: true,
			Message:           what + " requires confirmation to proceed",
		}
	default: // DispositionDeny, and any unconfigured/unknown disposition → deny (safe default)
		return &GuardrailError{
			Operation: op,
			Code:      code,
			Requested: requested,
			Limit:     limit,
			Message:   what + " is denied by the current guardrail policy",
		}
	}
}

// evaluateGuardrail applies a categorical guardrail — one that always trips when its
// operation is attempted, like public exposure — using the configured disposition.
func (p Policy) evaluateGuardrail(op string, code GuardrailCode, confirmed bool, what string) error {
	return p.enforce(op, code, confirmed, what, 0, 0)
}

// disposition returns the configured disposition for a guardrail, defaulting to deny when
// it is unset or invalid — the safe default (ADR-0020).
func (p Policy) disposition(code GuardrailCode) Disposition {
	if d, ok := p.Dispositions[code]; ok && d.Valid() {
		return d
	}
	return DispositionDeny
}
