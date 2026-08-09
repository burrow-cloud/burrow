// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The engine side of per-caller identity (ADR-0084 §2): claiming the first principal, recording
// further ones, issuing and revoking credentials, and turning a presented token back into a Caller.
//
// Every write here passes through the CredentialAuthorizer seam (authorize.go) except the first
// claim, which by definition has no caller to authorize. That is the whole shape of the decision:
// the first signer becomes an admin, and thereafter an admin — or, later, the identity provider
// that replaces the seam — decides who gets what.

// maxPrincipalName bounds a principal's handle. It is a name, not a document; an unbounded one is a
// row somebody has to read in a listing and in an audit trail.
const maxPrincipalName = 128

// IssueCredentialRequest asks for one credential.
type IssueCredentialRequest struct {
	// PrincipalID is who the credential is for. Issuing for another principal is an admin action.
	PrincipalID string
	// Kind is what will hold it, set here and read from the stored row on every request
	// thereafter — never from the request itself (ADR-0084 §3).
	Kind CredentialKind
	// TTL is how long the credential authenticates for. Zero means it does not expire, which is
	// the right default for a person's own terminal credential and the wrong one for an
	// automation, so the caller states it rather than inheriting it.
	TTL time.Duration
}

// ClaimFirstPrincipal records the install's first principal as an admin and issues them a `user`
// credential, in one call. It returns ErrAlreadyClaimed when the install already has a principal.
//
// THE CLAIM IS THE BOOTSTRAP, and the trust it rests on is deployment-shaped rather than
// credential-shaped: reaching burrowd at all requires cluster RBAC, so whoever can make this call
// already holds the access being bootstrapped from. Trust-on-first-use nonetheless leaves a window
// between burrowd starting and the first principal existing, and the answer to the window is to
// close it at install rather than to leave it open on purpose — `burrow cluster install` claims as
// part of installing, and `burrow cluster upgrade` claims for an install that predates this.
//
// The principal and their credential are written by ONE store call, which the store makes atomic
// against a concurrent claim. Two admins cannot both be first, and a claimed install always has
// somebody holding a token for it — a claim that recorded the principal and then failed to record
// the credential would produce an install that nobody can administer and nobody can claim again.
func (e *Engine) ClaimFirstPrincipal(ctx context.Context, name string) (Principal, IssuedCredential, error) {
	name, err := validPrincipalName(name)
	if err != nil {
		return Principal{}, IssuedCredential{}, err
	}
	if e.tokens == nil {
		return Principal{}, IssuedCredential{}, fmt.Errorf("%w: this control plane cannot issue credentials (no token source is wired)", ErrNotImplemented)
	}
	p := Principal{
		ID:        e.ids.NewID(),
		Name:      name,
		Admin:     true,
		CreatedAt: e.clock.Now(),
	}
	issued, err := e.mint(p.ID, CredentialKindUser, 0)
	if err != nil {
		return Principal{}, IssuedCredential{}, err
	}
	if err := e.db.ClaimFirstPrincipal(ctx, p, issued.Credential); err != nil {
		if errors.Is(err, ErrAlreadyClaimed) {
			return Principal{}, IssuedCredential{}, fmt.Errorf(
				"%w: this Burrow already has principals, so its first admin was claimed when it was installed; ask an admin for a credential", ErrAlreadyClaimed)
		}
		return Principal{}, IssuedCredential{}, fmt.Errorf("claiming the first principal: %w", err)
	}
	return p, issued, nil
}

// CreatePrincipal records a further principal — a second person, or an automation. It is an admin
// action: this is how somebody who has no access to the cluster gets access to Burrow at all, which
// is the point of the whole record and therefore the thing that needs a decision behind it.
//
// admin grants the new principal the bit as well, which is likewise admin-only.
func (e *Engine) CreatePrincipal(ctx context.Context, caller Caller, name string, admin bool) (Principal, error) {
	name, err := validPrincipalName(name)
	if err != nil {
		return Principal{}, err
	}
	if err := e.authz.AuthorizeCreatePrincipal(ctx, caller.PrincipalID); err != nil {
		return Principal{}, err
	}
	p := Principal{
		ID:        e.ids.NewID(),
		Name:      name,
		Admin:     admin,
		CreatedAt: e.clock.Now(),
	}
	if err := e.db.CreatePrincipal(ctx, p); err != nil {
		return Principal{}, fmt.Errorf("recording principal %q: %w", name, err)
	}
	return p, nil
}

// IssueCredential mints one credential for a principal and returns the token ONCE. The token is
// generated here, hashed, and only the hash is stored — burrowd does not see it again, so a caller
// that loses it issues another rather than reading it back.
//
// Authorization is the seam's, not this function's: an admin issues for anybody, and a non-admin
// issues an agent credential for themselves and nothing else (ADR-0084 §2).
func (e *Engine) IssueCredential(ctx context.Context, caller Caller, req IssueCredentialRequest) (IssuedCredential, error) {
	if !req.Kind.Valid() {
		return IssuedCredential{}, fmt.Errorf("%w: credential kind %q is not one of %s, %s, %s",
			ErrInvalid, req.Kind, CredentialKindUser, CredentialKindAgent, CredentialKindMachine)
	}
	if req.TTL < 0 {
		return IssuedCredential{}, fmt.Errorf("%w: a credential's lifetime cannot be negative (%s)", ErrInvalid, req.TTL)
	}
	if e.tokens == nil {
		return IssuedCredential{}, fmt.Errorf("%w: this control plane cannot issue credentials (no token source is wired)", ErrNotImplemented)
	}
	target, err := e.db.Principal(ctx, req.PrincipalID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return IssuedCredential{}, fmt.Errorf("%w: no principal %q to issue a credential for", ErrNotFound, req.PrincipalID)
		}
		return IssuedCredential{}, fmt.Errorf("reading principal %q: %w", req.PrincipalID, err)
	}
	if !target.Active() {
		return IssuedCredential{}, fmt.Errorf("%w: %s has been revoked, so a new credential for them would not authenticate", ErrInvalid, target.Name)
	}
	if err := e.authz.AuthorizeIssue(ctx, caller.PrincipalID, req.PrincipalID, req.Kind); err != nil {
		return IssuedCredential{}, err
	}
	return e.issue(ctx, req.PrincipalID, req.Kind, req.TTL)
}

// mint builds a credential and its secret without storing anything: the token comes from the seam,
// the row carries only its hash. It is separate from the write so the claim can hand the row to the
// store in the same call that records the principal.
func (e *Engine) mint(principalID string, kind CredentialKind, ttl time.Duration) (IssuedCredential, error) {
	now := e.clock.Now()
	token := e.tokens.NewToken()
	if token == "" {
		return IssuedCredential{}, fmt.Errorf("issuing a credential: the token source returned an empty token")
	}
	c := Credential{
		ID:          e.ids.NewID(),
		PrincipalID: principalID,
		Kind:        kind,
		TokenHash:   HashToken(token),
		CreatedAt:   now,
	}
	if ttl > 0 {
		c.ExpiresAt = now.Add(ttl)
	}
	return IssuedCredential{Credential: c, Token: token}, nil
}

// issue is the unauthorized half: it mints, hashes and stores. Every caller above it has already
// decided that this may happen — the claim because it is the bootstrap, IssueCredential because the
// seam said so.
func (e *Engine) issue(ctx context.Context, principalID string, kind CredentialKind, ttl time.Duration) (IssuedCredential, error) {
	issued, err := e.mint(principalID, kind, ttl)
	if err != nil {
		return IssuedCredential{}, err
	}
	c := issued.Credential
	if err := e.db.SaveCredential(ctx, c); err != nil {
		// The error names the principal and the kind and never the token: an error message is the
		// likeliest place for a credential to end up somewhere it was never meant to be.
		return IssuedCredential{}, fmt.Errorf("recording the %s credential for principal %q: %w", kind, principalID, err)
	}
	return issued, nil
}

// AuthenticateCredential turns a presented token into the Caller behind it: hash, one indexed
// lookup, and the expiry and revocation checks. It reaches no cluster and no API server — the whole
// answer is in a table burrowd already owns, which is what keeps authentication working when the
// Kubernetes API is having a bad day (ADR-0084 §2).
//
// It distinguishes its failures so a message can be actionable: an unknown token wraps ErrNotFound,
// and a token that was real and no longer authenticates wraps ErrForbidden and says which. Neither
// error carries the token.
func (e *Engine) AuthenticateCredential(ctx context.Context, token string) (Caller, error) {
	if token == "" {
		return Caller{}, fmt.Errorf("%w: no credential was presented", ErrNotFound)
	}
	c, err := e.db.CredentialByHash(ctx, HashToken(token))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Caller{}, fmt.Errorf("%w: the credential presented is not one this Burrow issued", ErrNotFound)
		}
		return Caller{}, fmt.Errorf("looking up the presented credential: %w", err)
	}
	now := e.clock.Now()
	if !c.RevokedAt.IsZero() {
		return Caller{}, fmt.Errorf("%w: this credential was revoked on %s; sign in again to get a new one",
			ErrForbidden, c.RevokedAt.UTC().Format(time.RFC3339))
	}
	if !c.Live(now) {
		return Caller{}, fmt.Errorf("%w: this credential expired on %s; sign in again to get a new one",
			ErrForbidden, c.ExpiresAt.UTC().Format(time.RFC3339))
	}
	p, err := e.db.Principal(ctx, c.PrincipalID)
	if err != nil {
		return Caller{}, fmt.Errorf("reading the principal behind the presented credential: %w", err)
	}
	if !p.Active() {
		return Caller{}, fmt.Errorf("%w: %s has been revoked", ErrForbidden, p.Name)
	}
	return Caller{
		PrincipalID:   p.ID,
		PrincipalName: p.Name,
		Kind:          c.Kind,
		CredentialID:  c.ID,
	}, nil
}

// RevokeCredential stops one credential authenticating, without touching any other. That is the
// point of a credential per holder: a lost laptop, a departure, an agent behaving badly are three
// different decisions, and none of them should log everybody out.
//
// Revoking one already revoked is a no-op, so a caller retrying a revocation they are not sure
// landed does not have to reason about it.
func (e *Engine) RevokeCredential(ctx context.Context, caller Caller, credentialID string) error {
	if strings.TrimSpace(credentialID) == "" {
		return fmt.Errorf("%w: no credential id to revoke", ErrInvalid)
	}
	c, err := e.db.Credential(ctx, credentialID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("%w: no credential %q", ErrNotFound, credentialID)
		}
		return fmt.Errorf("reading credential %q: %w", credentialID, err)
	}
	if err := e.authz.AuthorizeRevoke(ctx, caller.PrincipalID, c.PrincipalID); err != nil {
		return err
	}
	if err := e.db.RevokeCredential(ctx, credentialID, e.clock.Now()); err != nil {
		return fmt.Errorf("revoking credential %q: %w", credentialID, err)
	}
	return nil
}

// validPrincipalName trims and bounds a principal's handle. A name is required because it is what
// an audit row and a listing read as; an empty one would make every principal indistinguishable in
// the surface the whole record exists to fix.
func validPrincipalName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("%w: a principal needs a name", ErrInvalid)
	}
	if len(name) > maxPrincipalName {
		return "", fmt.Errorf("%w: principal name is %d characters, the limit is %d", ErrInvalid, len(name), maxPrincipalName)
	}
	if strings.ContainsAny(name, "\n\r\t") {
		return "", fmt.Errorf("%w: a principal name cannot contain a line break or a tab", ErrInvalid)
	}
	return name, nil
}
