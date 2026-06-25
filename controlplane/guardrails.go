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
)

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

// disposition returns the configured disposition for a guardrail, defaulting to deny when
// it is unset or invalid — the safe default (ADR-0020).
func (p Policy) disposition(code GuardrailCode) Disposition {
	if d, ok := p.Dispositions[code]; ok && d.Valid() {
		return d
	}
	return DispositionDeny
}
