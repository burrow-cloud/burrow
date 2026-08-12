// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

// TestAuditRecordsTheAuthenticatedCaller: an operation whose context carries an authenticated caller
// (as the API layer's authentication puts it there) records that principal and the KIND of
// credential it held, rather than the shared-agent constant (ADR-0084 §9).
func TestAuditRecordsTheAuthenticatedCaller(t *testing.T) {
	e, _, d, _ := newEngine(t, permissive())

	ctx := cp.ContextWithCaller(context.Background(), cp.Caller{
		PrincipalID:   "p-1",
		PrincipalName: "ada",
		Kind:          cp.CredentialKindAgent,
		CredentialID:  "c-1",
	})
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1, Confirm: true}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	rows := targetRows(auditRows(t, d, "deploy"), "web")
	if len(rows) == 0 {
		t.Fatal("no deploy audit rows")
	}
	for i, r := range rows {
		if r.Principal != "ada" {
			t.Errorf("row[%d] principal = %q, want ada", i, r.Principal)
		}
		// The caller column says WHAT acted; the principal says WHO. An agent credential and its
		// holder's own are two different callers and one principal, which is the distinction the
		// separate columns exist to keep.
		if r.Caller != string(cp.CredentialKindAgent) {
			t.Errorf("row[%d] caller = %q, want agent", i, r.Caller)
		}
	}
}

// TestSharedInstallTokenStillRecordsTheConstants: a request with no per-caller credential — the
// shared install token, which keeps working, or an internal reconcile that has no request at all —
// records exactly what it recorded before credentials existed. This is the property that makes the
// seam additive: an install that never issues a credential sees no audit row change.
func TestSharedInstallTokenStillRecordsTheConstants(t *testing.T) {
	e, _, d, _ := newEngine(t, permissive())

	if _, err := e.Deploy(context.Background(), cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1, Confirm: true}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	for i, r := range targetRows(auditRows(t, d, "deploy"), "web") {
		if r.Principal != "shared-agent" {
			t.Errorf("row[%d] principal = %q, want shared-agent", i, r.Principal)
		}
		if r.Caller != "control-plane" {
			t.Errorf("row[%d] caller = %q, want control-plane", i, r.Caller)
		}
	}
}

// TestContextWithCallerIgnoresAnEmptyPrincipal: a Caller with no principal puts nothing on the
// context. Nothing should be able to produce a context that CLAIMS to be authenticated and names
// nobody — a row recording an empty principal is worse than one recording the honest constant.
func TestContextWithCallerIgnoresAnEmptyPrincipal(t *testing.T) {
	ctx := cp.ContextWithCaller(context.Background(), cp.Caller{PrincipalName: "ghost", Kind: cp.CredentialKindUser})
	if c, ok := cp.CallerFromContext(ctx); ok {
		t.Fatalf("CallerFromContext = %+v, true; want no caller on the context", c)
	}
}

// TestCallerRoundTripsThroughTheContext: what the API layer puts on is what the engine reads back,
// field for field. The kind in particular has to survive, because it is the one thing that must come
// from the stored row rather than from anything the request said (ADR-0084 §3).
func TestCallerRoundTripsThroughTheContext(t *testing.T) {
	want := cp.Caller{PrincipalID: "p-1", PrincipalName: "ada", Kind: cp.CredentialKindMachine, CredentialID: "c-9"}
	got, ok := cp.CallerFromContext(cp.ContextWithCaller(context.Background(), want))
	if !ok {
		t.Fatal("CallerFromContext found no caller on a context that carries one")
	}
	if got != want {
		t.Errorf("caller = %+v, want %+v", got, want)
	}
}

// TestAuditRowOutlivesThePrincipalItNames: the trail keeps naming ada after her principal is
// retired and her credential stops authenticating. This is the property that decides what "who"
// means in the column (AuditEntry.Principal): the NAME, recorded at write time, not a reference
// resolved at read time.
//
// The name can be recorded on its own because it cannot come to mean somebody else. A principal is
// marked revoked rather than deleted, the name is unique across the install, and nothing renames
// one — so "ada" resolves to exactly one principal forever, including after she is gone. A row that
// stored an id instead would be true and unreadable; a row that resolved a reference at read time
// would go blank exactly when somebody is reading the trail to find out what a departed colleague
// did.
func TestAuditRowOutlivesThePrincipalItNames(t *testing.T) {
	ctx := context.Background()
	e, d, clock := newIdentityEngine(t)
	d.SetPolicy(permissive())

	p, issued, err := e.ClaimFirstPrincipal(ctx, "ada")
	if err != nil {
		t.Fatalf("ClaimFirstPrincipal: %v", err)
	}
	caller, err := e.AuthenticateCredential(ctx, issued.Token)
	if err != nil {
		t.Fatalf("AuthenticateCredential: %v", err)
	}
	if _, err := e.Deploy(cp.ContextWithCaller(ctx, caller), cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1, Confirm: true}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	// She leaves. Her principal is retired, which stops every credential she holds.
	if err := d.RevokePrincipal(ctx, p.ID, clock.Now()); err != nil {
		t.Fatalf("RevokePrincipal: %v", err)
	}
	if _, err := e.AuthenticateCredential(ctx, issued.Token); err == nil {
		t.Fatal("a retired principal's credential still authenticates; the rest of this test proves nothing")
	}

	rows := targetRows(auditRows(t, d, "deploy"), "web")
	if len(rows) == 0 {
		t.Fatal("no deploy audit rows")
	}
	for i, r := range rows {
		if r.Principal != "ada" {
			t.Errorf("row[%d] principal = %q after the principal was retired, want ada — the row records what was "+
				"true when it was written", i, r.Principal)
		}
		if r.Caller != string(cp.CredentialKindUser) {
			t.Errorf("row[%d] caller = %q, want user", i, r.Caller)
		}
	}
}

// TestAnUnrecognisedKindRecordsTheBoundaryAndKeepsThePrincipal: a caller whose stored kind is not
// one of ADR-0084 §3's three records the control-plane boundary in the CALLER column and still
// records WHO acted.
//
// The kind is read through callerKindFromContext, the one seam the guardrail evaluates a
// caller-bound disposition through (ADR-0094 §1), and that is the point. The row carries both the
// disposition that applied and the credential it applied to; if the audit read the kind by a second
// route it could name a kind the guardrail never saw, and a reader would conclude a binding matched
// when it did not. Reading it once means the trail describes the caller the way the decision was
// made about it.
//
// The principal is unaffected, because it is a different fact and it is known. The row says who
// acted and declines to say what they held, which is the honest pair.
func TestAnUnrecognisedKindRecordsTheBoundaryAndKeepsThePrincipal(t *testing.T) {
	e, _, d, _ := newEngine(t, permissive())

	ctx := cp.ContextWithCaller(context.Background(), cp.Caller{
		PrincipalID:   "p-1",
		PrincipalName: "ada",
		Kind:          cp.CredentialKind("root"),
		CredentialID:  "c-1",
	})
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1, Confirm: true}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	rows := targetRows(auditRows(t, d, "deploy"), "web")
	if len(rows) == 0 {
		t.Fatal("no deploy audit rows")
	}
	for i, r := range rows {
		if r.Caller != "control-plane" {
			t.Errorf("row[%d] caller = %q, want control-plane: an unrecognised kind is bound by nothing to the "+
				"guardrail, so the trail must not name it as though it were", i, r.Caller)
		}
		if r.Principal != "ada" {
			t.Errorf("row[%d] principal = %q, want ada: who acted is known, and is a different fact from what "+
				"they held", i, r.Principal)
		}
	}
}
