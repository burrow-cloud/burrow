// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

// The local CredentialAuthorizer, tested directly rather than only through the engine (ADR-0084 §2).
// It is the one place the admin bit is read, so the cases that must never quietly become permissive
// — an unauthenticated request, a caller who does not exist, a caller who has been revoked — are
// asserted here where an implementation swap would break them loudly.

// authorizerFixture returns the local authorizer over a fake database holding one admin and one
// ordinary principal.
func authorizerFixture(t *testing.T) (cp.CredentialAuthorizer, *fake.Database, cp.Principal, cp.Principal) {
	t.Helper()
	ctx := context.Background()
	d := fake.NewDatabase()
	at := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	admin := cp.Principal{ID: "p-admin", Name: "operator", Admin: true, CreatedAt: at}
	member := cp.Principal{ID: "p-dana", Name: "dana", CreatedAt: at}
	claim := cp.Credential{
		ID: "c-admin", PrincipalID: admin.ID, Kind: cp.CredentialKindUser,
		TokenHash: cp.HashToken("operator-token"), CreatedAt: at,
	}
	if err := d.ClaimFirstPrincipal(ctx, admin, claim); err != nil {
		t.Fatalf("ClaimFirstPrincipal: %v", err)
	}
	if err := d.CreatePrincipal(ctx, member); err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	return cp.NewDatabaseAuthorizer(d), d, admin, member
}

// TestAuthorizerRefusesACallerItCannotIdentify: no principal on the request, and a principal id that
// names nothing, are both refusals. A request with nothing behind it must never read as permitted.
func TestAuthorizerRefusesACallerItCannotIdentify(t *testing.T) {
	ctx := context.Background()
	a, _, _, _ := authorizerFixture(t)

	for _, caller := range []string{"", "p-nobody"} {
		if err := a.AuthorizeCreatePrincipal(ctx, caller); !errors.Is(err, cp.ErrForbidden) {
			t.Errorf("AuthorizeCreatePrincipal(%q) = %v, want ErrForbidden", caller, err)
		}
		if err := a.AuthorizeIssue(ctx, caller, "p-dana", cp.CredentialKindUser); !errors.Is(err, cp.ErrForbidden) {
			t.Errorf("AuthorizeIssue(%q) = %v, want ErrForbidden", caller, err)
		}
		if err := a.AuthorizeRevoke(ctx, caller, caller); !errors.Is(err, cp.ErrForbidden) {
			t.Errorf("AuthorizeRevoke(%q) = %v, want ErrForbidden", caller, err)
		}
	}
}

// TestAuthorizerRefusesARevokedCaller: the row survives the access, so absence is not what says
// somebody lost theirs — the mark is, and the authorizer has to read it.
func TestAuthorizerRefusesARevokedCaller(t *testing.T) {
	ctx := context.Background()
	a, d, admin, _ := authorizerFixture(t)

	if err := a.AuthorizeCreatePrincipal(ctx, admin.ID); err != nil {
		t.Fatalf("the admin cannot create a principal: %v", err)
	}
	if err := d.RevokePrincipal(ctx, admin.ID, time.Now()); err != nil {
		t.Fatalf("RevokePrincipal: %v", err)
	}
	err := a.AuthorizeCreatePrincipal(ctx, admin.ID)
	if !errors.Is(err, cp.ErrForbidden) {
		t.Fatalf("a revoked admin = %v, want ErrForbidden", err)
	}
	if !strings.Contains(err.Error(), "operator") {
		t.Errorf("the refusal does not name who was refused: %v", err)
	}
}

// TestAuthorizerSelfAgentCase: the one non-admin issue. A person may provision their own agent,
// because they already hold a credential that reaches burrowd and the agent's grant is a subset of
// theirs; anything else needs an admin.
func TestAuthorizerSelfAgentCase(t *testing.T) {
	ctx := context.Background()
	a, _, admin, dana := authorizerFixture(t)

	if err := a.AuthorizeIssue(ctx, dana.ID, dana.ID, cp.CredentialKindAgent); err != nil {
		t.Errorf("a person provisioning their own agent was refused: %v", err)
	}
	if err := a.AuthorizeIssue(ctx, dana.ID, dana.ID, cp.CredentialKindMachine); !errors.Is(err, cp.ErrForbidden) {
		t.Errorf("a non-admin minting a machine credential for themselves = %v, want ErrForbidden", err)
	}
	if err := a.AuthorizeIssue(ctx, dana.ID, admin.ID, cp.CredentialKindAgent); !errors.Is(err, cp.ErrForbidden) {
		t.Errorf("a non-admin provisioning somebody else's agent = %v, want ErrForbidden", err)
	}
	if err := a.AuthorizeIssue(ctx, admin.ID, dana.ID, cp.CredentialKindUser); err != nil {
		t.Errorf("an admin issuing for somebody else was refused: %v", err)
	}
}

// TestAuthorizerRevokeIsSelfOrAdmin: revoking your own credential needs nobody's permission;
// revoking somebody else's is an admin act.
func TestAuthorizerRevokeIsSelfOrAdmin(t *testing.T) {
	ctx := context.Background()
	a, _, admin, dana := authorizerFixture(t)

	if err := a.AuthorizeRevoke(ctx, dana.ID, dana.ID); err != nil {
		t.Errorf("revoking their own credential was refused: %v", err)
	}
	if err := a.AuthorizeRevoke(ctx, admin.ID, dana.ID); err != nil {
		t.Errorf("an admin revoking somebody else's credential was refused: %v", err)
	}
	if err := a.AuthorizeRevoke(ctx, dana.ID, admin.ID); !errors.Is(err, cp.ErrForbidden) {
		t.Errorf("a non-admin revoking somebody else's credential = %v, want ErrForbidden", err)
	}
}

// TestCredentialKindValid: three kinds, and nothing else. The set is closed because a kind is what a
// later guardrail disposition binds, and an unrecognised one would bind nothing.
func TestCredentialKindValid(t *testing.T) {
	for _, k := range []cp.CredentialKind{cp.CredentialKindUser, cp.CredentialKindAgent, cp.CredentialKindMachine} {
		if !k.Valid() {
			t.Errorf("%q.Valid() = false", k)
		}
	}
	for _, k := range []cp.CredentialKind{"", "root", "User", "human"} {
		if k.Valid() {
			t.Errorf("%q.Valid() = true, want false", k)
		}
	}
}

// TestCredentialLive: a credential authenticates when it is neither revoked nor past its expiry, and
// a zero expiry means it does not expire.
func TestCredentialLive(t *testing.T) {
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		c    cp.Credential
		want bool
	}{
		{"no expiry, not revoked", cp.Credential{}, true},
		{"expires later", cp.Credential{ExpiresAt: now.Add(time.Hour)}, true},
		{"expired", cp.Credential{ExpiresAt: now.Add(-time.Second)}, false},
		{"expires exactly now", cp.Credential{ExpiresAt: now}, false},
		{"revoked", cp.Credential{RevokedAt: now.Add(-time.Hour)}, false},
	}
	for _, tc := range cases {
		if got := tc.c.Live(now); got != tc.want {
			t.Errorf("%s: Live = %v, want %v", tc.name, got, tc.want)
		}
	}
}
