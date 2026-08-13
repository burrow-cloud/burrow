// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"errors"
	"fmt"
)

// An agent may not rewrite its own limits (ADR-0099).
//
// A guardrail decides what an agent may do (ADR-0006, ADR-0097). That model rests on an assumption
// nobody had checked: THAT THE PARTY BEING HELD CANNOT CHANGE THE THING HOLDING IT. It could, two
// independent ways.
//
//   - The route that sets a disposition asked nothing about the caller, so any credential that
//     authenticated could relax the table that held it. `app.delete deny` refused an agent exactly
//     until that agent set it to `allow`.
//   - The admin bit is a property of the PRINCIPAL rather than of the credential, so an admin's agent
//     credential is an admin's credential: it could create an invitation, redeem it, and come back
//     holding a credential of kind `user`, for which every disposition resolves to allow by design.
//
// Both are closed here, and by the same rule: a credential of kind `agent` may not write policy and
// may not mint identity. No new model and no new axis — the kind is already recorded at issuance and
// already read from the stored row rather than from the request (ADR-0084 §3).
//
// # Reading stays open to everybody
//
// Only the WRITES refuse. An agent that can see what binds it can explain a refusal to its person,
// and that is the whole reason `guard list` exists; a policy an agent cannot read is one it can only
// discover by being refused.
//
// # An unknown kind is treated as an agent, as it is everywhere else
//
// On an install nobody has signed in to, every request carries the shared install token and NOBODY
// has a kind — including the agent. Reading unknown as a person would leave both doors open on
// exactly the installs that have only an agent to hold, which is most of them. So the switch below
// admits `user` and `machine` and refuses everything else, which is the same shape — and the same
// fail-safe direction — as the disposition resolution in guardrails.go.
//
// The cost is stated rather than hidden: `burrow guard set` from a machine that has never run
// `burrow auth login` refuses, and the way through is to sign in and use one's own credential.
//
// # It is deliberately not a guardrail
//
// Making this a disposition of its own would be circular: the agent would relax `guard.set` and
// carry on. A rule about who may change the rules cannot itself be one of the rules. Requiring
// `--confirm` instead fails for the same class of reason — a confirmation is satisfied by the
// caller, so an agent relaxing its own guardrail would simply pass the flag, and a hold the held
// party can satisfy is not a hold.

// OperatorOnlyError is the refusal a policy or identity write returns for a caller that is not a
// person or a machine. "Operator" here means the human running Burrow, not the Kubernetes operator
// in operator/.
//
// It is a distinct type rather than a sentinel for the reason LockedError is one: the answer has
// parts a caller acts on — WHAT was refused, and what the caller is holding, which is what decides
// whether the way through is "ask your person" or "sign in".
//
// IT IS DELIBERATELY NOT A GuardrailError. A guardrail refusal means "policy says this caller may
// not", and a client that reads one knows two things that are false here: that re-issuing with
// confirm=true may work, and that some disposition governs it. Neither is true. No disposition
// governs this, nothing about the request changes it, and the only thing that does is a different
// credential.
type OperatorOnlyError struct {
	// Operation is what was refused, phrased as the act: "setting a guardrail disposition",
	// "creating an invitation". It is in the message so the refusal says what did not happen.
	Operation string
	// Kind is the kind of credential the caller presented, empty when the caller has none — the
	// shared install token, or a code path with no request behind it.
	Kind CredentialKind
}

// Error renders the refusal.
//
// THE WORDING IS PART OF THE MECHANISM, and it has to do two things. It must say that policy and
// identity are not an agent's to change, so an agent relays a fact rather than an obstacle. And it
// must not read like a guardrail denial: a caller told "denied" that has learned the confirm flow
// retries with `--confirm`, and there is nothing here for a confirmation to satisfy.
//
// The two kinds get two remedies because they are two different situations. A caller holding an
// agent credential is answered by the person who runs it. A caller with no kind at all is holding
// the install's shared token, and the remedy is their own credential — which is the behaviour change
// this rule brings, so the refusal spells it out rather than leaving an operator to guess.
func (e *OperatorOnlyError) Error() string {
	const why = "the guardrail policy and this install's identities are what bound an agent, so neither is an agent's to change. " +
		"This is not a guardrail decision: no disposition governs it, and --confirm does not satisfy it."
	if e.Kind == CredentialKindAgent {
		return fmt.Sprintf("%s is refused for an agent credential: %s "+
			"The person who runs this agent can do it with their own credential.", e.Operation, why)
	}
	return fmt.Sprintf("%s is refused for a caller with no credential of its own: %s "+
		"This request presented the install's shared token, which has no kind, and a caller with no kind is held exactly as an agent is — "+
		"otherwise the installs that have only an agent to hold would be the ones left open. "+
		"Sign in with `burrow auth login --context <cluster>` and run it again.", e.Operation, why)
}

// Unwrap makes this an ErrForbidden, because that is what it is: the request was understood, and the
// credential behind it may not make it. Anything classifying errors generically therefore lands on
// 403 rather than on a server fault, while a caller that wants the specific answer asks for it.
func (e *OperatorOnlyError) Unwrap() error { return ErrForbidden }

// AsOperatorOnly reports whether err is (or wraps) this refusal, and returns it. The API layer uses
// it to answer with the `operator_only` code, which is what lets a client — and through it an
// agent — tell this apart from a guardrail refusal without parsing prose.
func AsOperatorOnly(err error) (*OperatorOnlyError, bool) {
	var o *OperatorOnlyError
	if errors.As(err, &o) {
		return o, true
	}
	return nil, false
}

// The operations this rule covers, worded as the act for the refusal message. They are constants
// rather than literals at the call sites so the same operation cannot acquire two spellings, and so
// a reader can see the whole covered set in one place: policy on the left of the rule, identity on
// the right.
const (
	opSetGuardrail     = "setting a guardrail disposition"
	opCreatePrincipal  = "recording a principal"
	opCreateInvitation = "creating an invitation"
	opRedeemInvitation = "exchanging an invitation for a credential"
	opIssueCredential  = "issuing a credential"
)

// refuseAgentWrite is the whole enforcement: a person and a CI machine may write policy and mint
// identity, and everything else may not.
//
// IT TAKES THE KIND RATHER THAN A CONTEXT OR A CALLER, so the one rule can be applied by a method
// that is handed a Caller (the identity writes) and by one that is not (SetGuardrail, which reads
// the kind off the request context through the same seam a disposition does). Either way the value
// came from the stored credential row, never from the request.
//
// THE SWITCH ADMITS RATHER THAN REFUSES, which is the property to preserve. A kind that is not one
// of the recorded three — an empty one, or a row from some future the code does not know — falls
// through to the refusal instead of past it.
func refuseAgentWrite(kind CredentialKind, operation string) error {
	switch kind {
	case CredentialKindUser, CredentialKindMachine:
		return nil
	}
	return &OperatorOnlyError{Operation: operation, Kind: kind}
}
