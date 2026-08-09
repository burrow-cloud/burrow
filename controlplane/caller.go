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
