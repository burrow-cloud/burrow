// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"errors"
	"fmt"
)

// GuardrailCode is a machine-readable reason a guardrail refused an operation, so an
// agent can branch on the cause rather than parse prose (ADR-0006).
type GuardrailCode string

const (
	// GuardrailReplicaCeiling: the requested replica count exceeds Policy.MaxReplicas.
	GuardrailReplicaCeiling GuardrailCode = "replica_ceiling"
	// GuardrailScaleToZero: the operation would scale to zero replicas and
	// Policy.AllowScaleToZero is false.
	GuardrailScaleToZero GuardrailCode = "scale_to_zero"
)

// GuardrailError is returned when the control plane refuses a dangerous operation. It
// is a structured outcome, not a failure of the system: the operation was understood
// and deliberately declined. Callers distinguish it with AsGuardrail.
type GuardrailError struct {
	// Operation is the operation that was refused (e.g. "deploy", "scale").
	Operation string
	// Code is the machine-readable reason.
	Code GuardrailCode
	// Message is a human-readable explanation.
	Message string
	// Requested is the value the caller asked for (e.g. the replica count).
	Requested int32
	// Limit is the relevant policy limit, when the code involves one.
	Limit int32
}

func (e *GuardrailError) Error() string {
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

// checkReplicas evaluates a requested replica count for op against the policy. It
// returns a *GuardrailError if the operation should be refused, or nil if it is
// allowed. It assumes replicas is already known non-negative (a negative count is a
// malformed request, validated separately, not a guardrail concern).
func (p Policy) checkReplicas(op string, replicas int32) error {
	if replicas == 0 && !p.AllowScaleToZero {
		return &GuardrailError{
			Operation: op,
			Code:      GuardrailScaleToZero,
			Requested: 0,
			Limit:     0,
			Message:   "scaling to zero replicas is disabled by policy; enable AllowScaleToZero to permit it",
		}
	}
	if replicas > p.MaxReplicas {
		return &GuardrailError{
			Operation: op,
			Code:      GuardrailReplicaCeiling,
			Requested: replicas,
			Limit:     p.MaxReplicas,
			Message:   fmt.Sprintf("requested %d replicas exceeds the policy ceiling of %d", replicas, p.MaxReplicas),
		}
	}
	return nil
}
