// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

// The store tests for who Burrow knows and what they hold (ADR-0084 §2, §3).
//
// The principals table is INSTALL-WIDE and this suite shares one database, so nothing here claims a
// first principal — the claim only succeeds on an empty table, and a test that emptied the shared one
// would be reaching into every other test's rows. These isolate themselves by id and name; the claim
// itself is tested against a schema of its own in credentials_claim_test.go.

// principalName builds an id or name scoped to the running test, so tests stay independent against
// the shared database.
func principalName(t *testing.T, suffix string) string {
	t.Helper()
	return strings.ToLower(t.Name()) + "-" + suffix
}

// TestStorePrincipalRoundTrip: record, read back by id and by name, list, revoke, and re-revoke.
func TestStorePrincipalRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	at := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC).UTC()
	id := principalName(t, "id")
	name := principalName(t, "dana")

	if err := s.CreatePrincipal(ctx, cp.Principal{ID: id, Name: name, Admin: true, CreatedAt: at}); err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	got, err := s.Principal(ctx, id)
	if err != nil {
		t.Fatalf("Principal: %v", err)
	}
	if got.Name != name || !got.Admin || !got.CreatedAt.Equal(at) {
		t.Errorf("Principal = %+v, want %s admin at %s", got, name, at)
	}
	if !got.Active() {
		t.Errorf("a freshly recorded principal is not active: %+v", got)
	}
	byName, err := s.PrincipalByName(ctx, name)
	if err != nil {
		t.Fatalf("PrincipalByName: %v", err)
	}
	if byName.ID != id {
		t.Errorf("PrincipalByName = %q, want %q", byName.ID, id)
	}

	// A duplicate NAME is a conflict, not a second identity answering to the same handle.
	dup := s.CreatePrincipal(ctx, cp.Principal{ID: id + "-2", Name: name, CreatedAt: at})
	if !errors.Is(dup, cp.ErrInvalid) {
		t.Errorf("a duplicate name = %v, want ErrInvalid", dup)
	}

	if _, err := s.Principal(ctx, id+"-missing"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("an unknown principal = %v, want ErrNotFound", err)
	}
	if _, err := s.PrincipalByName(ctx, name+"-missing"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("an unknown name = %v, want ErrNotFound", err)
	}

	// Listing is name-ordered and includes revoked principals: an audit row that names one has to
	// keep meaning something.
	found := false
	all, err := s.Principals(ctx)
	if err != nil {
		t.Fatalf("Principals: %v", err)
	}
	for i, p := range all {
		if p.ID == id {
			found = true
		}
		if i > 0 && all[i-1].Name > p.Name {
			t.Fatalf("Principals is not name-ordered: %q before %q", all[i-1].Name, p.Name)
		}
	}
	if !found {
		t.Errorf("Principals did not include %q", id)
	}

	if err := s.RevokePrincipal(ctx, id, at.Add(time.Hour)); err != nil {
		t.Fatalf("RevokePrincipal: %v", err)
	}
	revoked, err := s.Principal(ctx, id)
	if err != nil {
		t.Fatalf("Principal after revoke: %v", err)
	}
	if revoked.Active() || !revoked.RevokedAt.Equal(at.Add(time.Hour)) {
		t.Errorf("revoked principal = %+v, want revoked at %s", revoked, at.Add(time.Hour))
	}
	// A retried revoke keeps the FIRST timestamp: that is when the access actually ended.
	if err := s.RevokePrincipal(ctx, id, at.Add(48*time.Hour)); err != nil {
		t.Fatalf("re-revoking: %v", err)
	}
	again, err := s.Principal(ctx, id)
	if err != nil {
		t.Fatalf("Principal: %v", err)
	}
	if !again.RevokedAt.Equal(revoked.RevokedAt) {
		t.Errorf("re-revoking moved the timestamp to %s, want %s", again.RevokedAt, revoked.RevokedAt)
	}
	if err := s.RevokePrincipal(ctx, id+"-missing", at); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("revoking an unknown principal = %v, want ErrNotFound", err)
	}
}

// TestStoreCredentialRoundTrip: issue, look up by hash and by id, list newest first, revoke one and
// leave the other alone.
func TestStoreCredentialRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	at := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC).UTC()
	principal := principalName(t, "p")

	if err := s.CreatePrincipal(ctx, cp.Principal{ID: principal, Name: principal, CreatedAt: at}); err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	user := cp.Credential{
		ID: principal + "-user", PrincipalID: principal, Kind: cp.CredentialKindUser,
		TokenHash: cp.HashToken(principal + "-user-token"), CreatedAt: at,
	}
	agent := cp.Credential{
		ID: principal + "-agent", PrincipalID: principal, Kind: cp.CredentialKindAgent,
		TokenHash: cp.HashToken(principal + "-agent-token"), CreatedAt: at.Add(time.Minute),
		ExpiresAt: at.Add(24 * time.Hour),
	}
	for _, c := range []cp.Credential{user, agent} {
		if err := s.SaveCredential(ctx, c); err != nil {
			t.Fatalf("SaveCredential(%s): %v", c.ID, err)
		}
	}

	got, err := s.CredentialByHash(ctx, user.TokenHash)
	if err != nil {
		t.Fatalf("CredentialByHash: %v", err)
	}
	if got.ID != user.ID || got.Kind != cp.CredentialKindUser {
		t.Errorf("CredentialByHash = %+v, want the user credential", got)
	}
	// A credential with no expiry reads back as the zero time, not as a sentinel timestamp.
	if !got.ExpiresAt.IsZero() || !got.RevokedAt.IsZero() {
		t.Errorf("an unexpiring, unrevoked credential = %+v, want both timestamps zero", got)
	}
	if !got.Live(at) {
		t.Errorf("a fresh credential does not read as live: %+v", got)
	}

	withExpiry, err := s.Credential(ctx, agent.ID)
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	if !withExpiry.ExpiresAt.Equal(agent.ExpiresAt) {
		t.Errorf("expires_at = %s, want %s", withExpiry.ExpiresAt, agent.ExpiresAt)
	}
	if withExpiry.Live(at.Add(48 * time.Hour)) {
		t.Errorf("an expired credential reads as live: %+v", withExpiry)
	}

	if _, err := s.CredentialByHash(ctx, cp.HashToken("never issued")); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("an unknown hash = %v, want ErrNotFound", err)
	}
	if _, err := s.Credential(ctx, "no-such-credential"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("an unknown id = %v, want ErrNotFound", err)
	}

	list, err := s.PrincipalCredentials(ctx, principal)
	if err != nil {
		t.Fatalf("PrincipalCredentials: %v", err)
	}
	if len(list) != 2 || list[0].ID != agent.ID || list[1].ID != user.ID {
		t.Fatalf("PrincipalCredentials = %+v, want the agent credential first (newest first)", list)
	}

	// Revoking one leaves the other alone, which is the whole point of a credential per holder.
	if err := s.RevokeCredential(ctx, agent.ID, at.Add(2*time.Hour)); err != nil {
		t.Fatalf("RevokeCredential: %v", err)
	}
	after, err := s.Credential(ctx, agent.ID)
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	if after.Live(at.Add(3 * time.Hour)) {
		t.Errorf("a revoked credential reads as live: %+v", after)
	}
	other, err := s.Credential(ctx, user.ID)
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	if !other.Live(at.Add(3 * time.Hour)) {
		t.Errorf("revoking one credential took the other with it: %+v", other)
	}
	// Re-revoking keeps the first timestamp; revoking an unknown id is ErrNotFound.
	if err := s.RevokeCredential(ctx, agent.ID, at.Add(72*time.Hour)); err != nil {
		t.Fatalf("re-revoking: %v", err)
	}
	again, err := s.Credential(ctx, agent.ID)
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	if !again.RevokedAt.Equal(after.RevokedAt) {
		t.Errorf("re-revoking moved the timestamp to %s, want %s", again.RevokedAt, after.RevokedAt)
	}
	if err := s.RevokeCredential(ctx, "no-such-credential", at); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("revoking an unknown credential = %v, want ErrNotFound", err)
	}
}

// TestStoreCredentialNeedsAPrincipal: the foreign key refuses a credential for somebody who does not
// exist, so an issuance path cannot leave an orphan token that authenticates as nobody.
func TestStoreCredentialNeedsAPrincipal(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	at := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)

	err := s.SaveCredential(ctx, cp.Credential{
		ID: principalName(t, "orphan"), PrincipalID: principalName(t, "nobody"),
		Kind: cp.CredentialKindUser, TokenHash: cp.HashToken(principalName(t, "token")), CreatedAt: at,
	})
	if err == nil {
		t.Fatal("a credential for an unknown principal was accepted")
	}
}

// TestStoreCredentialHashIsUnique: two credentials cannot share a hash, which is what makes the
// per-request lookup a single indexed probe with one answer.
func TestStoreCredentialHashIsUnique(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	at := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	principal := principalName(t, "p")
	hash := cp.HashToken(principalName(t, "token"))

	if err := s.CreatePrincipal(ctx, cp.Principal{ID: principal, Name: principal, CreatedAt: at}); err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	first := cp.Credential{ID: principal + "-1", PrincipalID: principal, Kind: cp.CredentialKindUser, TokenHash: hash, CreatedAt: at}
	if err := s.SaveCredential(ctx, first); err != nil {
		t.Fatalf("SaveCredential: %v", err)
	}
	second := first
	second.ID = principal + "-2"
	if err := s.SaveCredential(ctx, second); err == nil {
		t.Fatal("a second credential with the same hash was accepted")
	}
}

// TestStoreCredentialEnrollmentRoundTrip: an invitation is a credential with one column set, and the
// column has to survive the round trip — every route but the exchange refuses a caller on the
// strength of it, so a value that did not come back would make an invitation a working credential
// (ADR-0084 §2, migration 00034).
func TestStoreCredentialEnrollmentRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	at := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC).UTC()
	id := principalName(t, "id")
	name := principalName(t, "ada")

	if err := s.CreatePrincipal(ctx, cp.Principal{ID: id, Name: name, CreatedAt: at}); err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	invitation := cp.Credential{
		ID: id + "-invite", PrincipalID: id, Kind: cp.CredentialKindUser,
		TokenHash: cp.HashToken(name + "-invitation"), CreatedAt: at,
		ExpiresAt: at.Add(cp.DefaultInvitationTTL), Enrollment: true,
	}
	ordinary := cp.Credential{
		ID: id + "-cred", PrincipalID: id, Kind: cp.CredentialKindUser,
		TokenHash: cp.HashToken(name + "-credential"), CreatedAt: at,
	}
	for _, c := range []cp.Credential{invitation, ordinary} {
		if err := s.SaveCredential(ctx, c); err != nil {
			t.Fatalf("SaveCredential %q: %v", c.ID, err)
		}
	}

	got, err := s.CredentialByHash(ctx, invitation.TokenHash)
	if err != nil {
		t.Fatalf("CredentialByHash: %v", err)
	}
	if !got.Enrollment {
		t.Error("the invitation came back with Enrollment false, so it would authenticate every route")
	}
	if !got.ExpiresAt.Equal(invitation.ExpiresAt) {
		t.Errorf("invitation expires at %v, want %v", got.ExpiresAt, invitation.ExpiresAt)
	}

	// And the default is the one every credential written before this column existed has.
	plain, err := s.Credential(ctx, ordinary.ID)
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	if plain.Enrollment {
		t.Error("an ordinary credential came back marked as an invitation")
	}
}
