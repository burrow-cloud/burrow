// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package fake

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/burrow-cloud/burrow/controlplane"
)

// The fake's half of who Burrow knows and what they hold (ADR-0084 §2, §3). It mirrors the store's
// observable behaviour rather than its storage: the same sentinels, the same orderings, the same
// idempotence on a repeated revoke — so a test that passes here means something about the Postgres
// path it stands in for.
//
// Like the store, it holds a token HASH and never a token.

// ClaimFirstPrincipal records p and their credential, and only when there is no principal — the
// fake's equivalent of the store's locked transaction, which is one critical section holding the
// check and both writes.
func (d *Database) ClaimFirstPrincipal(ctx context.Context, p controlplane.Principal, c controlplane.Credential) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpClaimFirstPrincipal]; err != nil {
		return err
	}
	if len(d.principals) > 0 {
		return fmt.Errorf("database: this install already has a principal: %w", controlplane.ErrAlreadyClaimed)
	}
	if c.PrincipalID != p.ID {
		return fmt.Errorf("database: claim first principal %q: the credential belongs to principal %q", p.Name, c.PrincipalID)
	}
	if err := d.putPrincipal(p); err != nil {
		return err
	}
	if err := d.putCredential(c); err != nil {
		// Neither row lands: the store writes both in one transaction, and a fake that left a
		// claimed install with no credential would hide the failure the transaction exists for.
		delete(d.principals, p.ID)
		return err
	}
	return nil
}

// CreatePrincipal records a principal, rejecting a name already in use with an ErrInvalid-wrapped
// error, matching the store's unique name column.
func (d *Database) CreatePrincipal(ctx context.Context, p controlplane.Principal) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpCreatePrincipal]; err != nil {
		return err
	}
	return d.putPrincipal(p)
}

// putPrincipal inserts p under the lock the callers already hold.
func (d *Database) putPrincipal(p controlplane.Principal) error {
	if p.ID == "" {
		return fmt.Errorf("database: create principal: empty id")
	}
	if p.Name == "" {
		return fmt.Errorf("database: create principal: empty name")
	}
	if _, exists := d.principals[p.ID]; exists {
		return fmt.Errorf("database: principal %q already exists: %w", p.ID, controlplane.ErrInvalid)
	}
	for _, existing := range d.principals {
		if existing.Name == p.Name {
			return fmt.Errorf("database: principal %q already exists: %w", p.Name, controlplane.ErrInvalid)
		}
	}
	d.principals[p.ID] = p
	return nil
}

// Principal returns the principal with the given id, or ErrNotFound. A revoked principal is
// returned, marked.
func (d *Database) Principal(ctx context.Context, id string) (controlplane.Principal, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpPrincipal]; err != nil {
		return controlplane.Principal{}, err
	}
	p, ok := d.principals[id]
	if !ok {
		return controlplane.Principal{}, fmt.Errorf("database: principal %q: %w", id, controlplane.ErrNotFound)
	}
	return p, nil
}

// PrincipalByName returns the principal with the given name, or ErrNotFound.
func (d *Database) PrincipalByName(ctx context.Context, name string) (controlplane.Principal, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpPrincipalByName]; err != nil {
		return controlplane.Principal{}, err
	}
	for _, p := range d.principals {
		if p.Name == name {
			return p, nil
		}
	}
	return controlplane.Principal{}, fmt.Errorf("database: principal %q: %w", name, controlplane.ErrNotFound)
}

// Principals returns every recorded principal in name order, revoked ones included.
func (d *Database) Principals(ctx context.Context) ([]controlplane.Principal, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpPrincipals]; err != nil {
		return nil, err
	}
	out := make([]controlplane.Principal, 0, len(d.principals))
	for _, p := range d.principals {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// RevokePrincipal marks the principal retired at `at`. An unknown principal is ErrNotFound; one
// already revoked keeps its first timestamp.
func (d *Database) RevokePrincipal(ctx context.Context, id string, at time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpRevokePrincipal]; err != nil {
		return err
	}
	p, ok := d.principals[id]
	if !ok {
		return fmt.Errorf("database: principal %q: %w", id, controlplane.ErrNotFound)
	}
	if p.RevokedAt.IsZero() {
		p.RevokedAt = at
		d.principals[id] = p
	}
	return nil
}

// SaveCredential records an issued credential. A credential for a principal that does not exist is
// refused — the store's foreign key does the same — and so is a hash already recorded, which the
// store's unique index refuses.
func (d *Database) SaveCredential(ctx context.Context, c controlplane.Credential) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpSaveCredential]; err != nil {
		return err
	}
	return d.putCredential(c)
}

// putCredential inserts c under the lock the callers already hold.
func (d *Database) putCredential(c controlplane.Credential) error {
	switch {
	case c.ID == "":
		return fmt.Errorf("database: save credential: empty id")
	case c.TokenHash == "":
		return fmt.Errorf("database: save credential %q: empty token hash", c.ID)
	}
	if _, ok := d.principals[c.PrincipalID]; !ok {
		return fmt.Errorf("database: save credential %q: no principal %q: %w", c.ID, c.PrincipalID, controlplane.ErrInvalid)
	}
	if _, exists := d.credentials[c.ID]; exists {
		return fmt.Errorf("database: credential %q already exists: %w", c.ID, controlplane.ErrInvalid)
	}
	for _, existing := range d.credentials {
		if existing.TokenHash == c.TokenHash {
			// No hash in the message: it is derived from a secret, and a value derived from a secret
			// has no business in an error string.
			return fmt.Errorf("database: save credential %q: that token is already recorded: %w", c.ID, controlplane.ErrInvalid)
		}
	}
	d.credentials[c.ID] = c
	d.credentialSeq = append(d.credentialSeq, c.ID)
	return nil
}

// CredentialByHash returns the credential whose stored hash is hash, or ErrNotFound. It returns the
// row as stored — expired and revoked ones included — because only the caller can say which answer
// the holder needs.
func (d *Database) CredentialByHash(ctx context.Context, hash string) (controlplane.Credential, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpCredentialByHash]; err != nil {
		return controlplane.Credential{}, err
	}
	for _, c := range d.credentials {
		if c.TokenHash == hash {
			return c, nil
		}
	}
	return controlplane.Credential{}, fmt.Errorf("database: no such credential: %w", controlplane.ErrNotFound)
}

// Credential returns the credential with the given id, or ErrNotFound.
func (d *Database) Credential(ctx context.Context, id string) (controlplane.Credential, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpCredential]; err != nil {
		return controlplane.Credential{}, err
	}
	c, ok := d.credentials[id]
	if !ok {
		return controlplane.Credential{}, fmt.Errorf("database: credential %q: %w", id, controlplane.ErrNotFound)
	}
	return c, nil
}

// PrincipalCredentials returns a principal's credentials newest first, revoked and expired ones
// included. Credentials issued at the same instant — which a test clock makes the normal case —
// order by id descending, matching the store's tie-break so a listing is deterministic in both.
func (d *Database) PrincipalCredentials(ctx context.Context, principalID string) ([]controlplane.Credential, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpPrincipalCredentials]; err != nil {
		return nil, err
	}
	out := make([]controlplane.Credential, 0)
	for _, id := range d.credentialSeq {
		if c := d.credentials[id]; c.PrincipalID == principalID {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

// RevokeCredential marks one credential revoked at `at` and touches no other. An unknown id is
// ErrNotFound; one already revoked keeps its first timestamp.
func (d *Database) RevokeCredential(ctx context.Context, id string, at time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpRevokeCredential]; err != nil {
		return err
	}
	c, ok := d.credentials[id]
	if !ok {
		return fmt.Errorf("database: credential %q: %w", id, controlplane.ErrNotFound)
	}
	if c.RevokedAt.IsZero() {
		c.RevokedAt = at
		d.credentials[id] = c
	}
	return nil
}
