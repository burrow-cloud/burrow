// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import "context"

// How the authenticated caller reaches the engine (ADR-0084 §9).
//
// The API layer is the only thing that can know who is calling: it holds the request, and the
// request is where the credential is. Everything below it — the audit record, and later a guardrail
// that binds one kind of caller and not another — needs the answer without being handed a request.
// So the caller rides the context, put there by one middleware and read through one seam.
//
// The value carried is the Caller the presented credential resolved to (identity.go), never anything
// the request declared about itself. In particular the KIND is the stored row's, because a
// caller-declared kind would make a `deny` that binds the agent cooperative, and ADR-0020 requires a
// deny to hold "even against an over-eager or misbehaving agent".

// callerContextKey keys the authenticated caller on a request context. It is an unexported type so
// no other package can collide with it, and so nothing outside this package can put a Caller on a
// context by any route but ContextWithCaller.
type callerContextKey struct{}

// ContextWithCaller returns a context carrying the authenticated caller behind a request. The API
// layer sets it once, immediately after authentication; the engine reads it back with
// CallerFromContext.
//
// A caller with no principal leaves the context unchanged, which is what makes the shared install
// token and an internal reconcile behave identically to how they did before this existed: there is
// no principal to record, so nothing claims there is one.
func ContextWithCaller(ctx context.Context, c Caller) context.Context {
	if c.PrincipalID == "" {
		return ctx
	}
	return context.WithValue(ctx, callerContextKey{}, c)
}

// CallerFromContext returns the authenticated caller placed on ctx by ContextWithCaller, and whether
// there was one. There is none for a request authenticated by the shared install token, and none on
// a code path that carries no request at all — an internal reconcile, a background sweep — both of
// which are ordinary rather than errors.
func CallerFromContext(ctx context.Context) (Caller, bool) {
	c, ok := ctx.Value(callerContextKey{}).(Caller)
	return c, ok
}

// callerKindFromContext is the guardrail's read of the caller (ADR-0094 §1): the KIND of credential
// behind the request, and nothing else about who holds it. Not the principal id, not the principal
// name, and not the admin bit — which Caller deliberately does not carry, so that "may this caller do
// this" keeps exactly one answering seam.
//
// It is read in ONE PLACE, inside Policy.dispositionSource, rather than threaded through the
// seventeen sites that evaluate a guardrail. That is what keeps ADR-0084 §3's property — the kind
// comes from the stored credential row, never from the request — a property of the system rather than
// of seventeen call sites agreeing, and it is why the kind is not a field on GuardrailScope: a scope
// names what an operation TARGETS, and a kind field would be seventeen chances to omit a value whose
// omission does not fail but resolves to "binds nobody".
//
// An empty answer means NO KIND, which matches only the unbound policy keys (ADR-0094 §4). Three
// situations produce it and all three are ordinary: the shared install token, which puts no Caller on
// the context at all; an internal reconcile, which has no request behind it; and a stored kind that is
// not one of the three ADR-0084 §3 defines. An unknown kind is deliberately NOT read as `agent` — on a
// shared-token install every caller is unknown, so that reading would make every operator their own
// agent, which is the defect ADR-0094 exists to remove.
//
// Kept a package var beside principalFromContext, for the same reason that one is: a test or the
// managed product substitutes a resolver without touching a call site.
var callerKindFromContext = func(ctx context.Context) CredentialKind {
	if c, ok := CallerFromContext(ctx); ok && c.Kind.Valid() {
		return c.Kind
	}
	return ""
}
