// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/burrow-cloud/burrow/controlplane"
)

// The store side of who Burrow knows and what they hold (ADR-0084 §2, §3).
//
// NO TOKEN IS WRITTEN OR READ HERE. A credential row carries the SHA-256 of the token the engine
// generated and returned once; the lookup on every authenticated request is by that hash, which is
// why it is unique and therefore a single indexed probe. Nothing in this file reaches the cluster or
// the API server, so authentication keeps working while Kubernetes is having a bad day.

// principalColumns is the principals projection in a stable order shared by every SELECT and the
// scanner below.
const principalColumns = `id, name, admin, created_at, revoked_at`

// credentialColumns is the credentials projection, likewise.
// enrollment marks an invitation, which is the one credential that authenticates nothing but its
// own exchange (ADR-0084 §2, migration 00034). It is part of the projection rather than read
// separately because it is checked on the same per-request lookup the hash is.
const credentialColumns = `id, principal_id, kind, token_hash, created_at, expires_at, revoked_at, enrollment`

// ClaimFirstPrincipal records p as the install's first principal together with the credential issued
// to them, and only when the install has none. It returns ErrAlreadyClaimed when one already exists.
//
// BOTH ROWS LAND OR NEITHER DOES, in one transaction. A claim that recorded the principal and then
// failed to record the credential would leave an install that is claimed — so no second claim is
// possible — and whose claimant holds no token, which is an install nobody can administer.
//
// The transaction takes SHARE ROW EXCLUSIVE on principals before it reads. That is what makes the
// claim single-winner: `INSERT ... WHERE NOT EXISTS` looks atomic and is not, because under READ
// COMMITTED a concurrent transaction's uncommitted row is invisible to the subquery and both
// callers would insert a principal of their own — with different names, so no unique constraint
// would catch it. The lock is held for the length of one insert pair, on a table written at install
// and when somebody joins, so it costs nothing that anybody can measure.
func (s *Store) ClaimFirstPrincipal(ctx context.Context, p controlplane.Principal, c controlplane.Credential) error {
	switch {
	case p.ID == "":
		return fmt.Errorf("postgres: claim first principal: empty id")
	case p.Name == "":
		return fmt.Errorf("postgres: claim first principal: empty name")
	case c.ID == "":
		return fmt.Errorf("postgres: claim first principal %q: the credential has no id", p.Name)
	case c.TokenHash == "":
		return fmt.Errorf("postgres: claim first principal %q: the credential has no token hash", p.Name)
	case c.PrincipalID != p.ID:
		return fmt.Errorf("postgres: claim first principal %q: the credential belongs to principal %q", p.Name, c.PrincipalID)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("postgres: claim first principal %q: %w", p.Name, err)
	}
	// A rollback after a successful commit is a no-op, so this is the whole error path.
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `LOCK TABLE principals IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return fmt.Errorf("postgres: claim first principal %q: %w", p.Name, err)
	}
	var claimed bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM principals)`).Scan(&claimed); err != nil {
		return fmt.Errorf("postgres: claim first principal %q: %w", p.Name, err)
	}
	if claimed {
		return fmt.Errorf("postgres: this install already has a principal: %w", controlplane.ErrAlreadyClaimed)
	}
	const insertPrincipal = `
INSERT INTO principals (id, name, admin, created_at, revoked_at)
VALUES ($1, $2, $3, $4, NULL)`
	if _, err := tx.ExecContext(ctx, insertPrincipal, p.ID, p.Name, p.Admin, p.CreatedAt); err != nil {
		return fmt.Errorf("postgres: claim first principal %q: %w", p.Name, err)
	}
	const insertCredential = `
INSERT INTO credentials (` + credentialColumns + `)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`
	if _, err := tx.ExecContext(ctx, insertCredential, c.ID, c.PrincipalID, string(c.Kind), c.TokenHash,
		c.CreatedAt, nullTime(c.ExpiresAt), nullTime(c.RevokedAt), c.Enrollment); err != nil {
		return fmt.Errorf("postgres: claim first principal %q: recording their credential: %w", p.Name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("postgres: claim first principal %q: %w", p.Name, err)
	}
	return nil
}

// CreatePrincipal records a principal. A name already in use is rejected with an ErrInvalid-wrapped
// error rather than merged into the existing row: a name is a handle a person reads in a listing and
// in an audit trail, and two principals answering to one is an ambiguity there.
func (s *Store) CreatePrincipal(ctx context.Context, p controlplane.Principal) error {
	if p.ID == "" {
		return fmt.Errorf("postgres: create principal: empty id")
	}
	if p.Name == "" {
		return fmt.Errorf("postgres: create principal: empty name")
	}
	const q = `
INSERT INTO principals (id, name, admin, created_at, revoked_at)
VALUES ($1, $2, $3, $4, NULL)
ON CONFLICT DO NOTHING`
	res, err := s.db.ExecContext(ctx, q, p.ID, p.Name, p.Admin, p.CreatedAt)
	if err != nil {
		return fmt.Errorf("postgres: create principal %q: %w", p.Name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("postgres: create principal %q: %w", p.Name, err)
	}
	if n == 0 {
		return fmt.Errorf("postgres: principal %q already exists: %w", p.Name, controlplane.ErrInvalid)
	}
	return nil
}

// Principal returns the principal with the given id, or ErrNotFound.
func (s *Store) Principal(ctx context.Context, id string) (controlplane.Principal, error) {
	const q = `SELECT ` + principalColumns + ` FROM principals WHERE id = $1`
	p, err := scanPrincipal(s.db.QueryRowContext(ctx, q, id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return controlplane.Principal{}, fmt.Errorf("postgres: principal %q: %w", id, controlplane.ErrNotFound)
		}
		return controlplane.Principal{}, fmt.Errorf("postgres: principal %q: %w", id, err)
	}
	return p, nil
}

// PrincipalByName returns the principal with the given name, or ErrNotFound. The name column is
// unique, so this is as single-valued as the lookup by id.
func (s *Store) PrincipalByName(ctx context.Context, name string) (controlplane.Principal, error) {
	const q = `SELECT ` + principalColumns + ` FROM principals WHERE name = $1`
	p, err := scanPrincipal(s.db.QueryRowContext(ctx, q, name))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return controlplane.Principal{}, fmt.Errorf("postgres: principal %q: %w", name, controlplane.ErrNotFound)
		}
		return controlplane.Principal{}, fmt.Errorf("postgres: principal %q: %w", name, err)
	}
	return p, nil
}

// Principals returns every recorded principal in name order, revoked ones included. A revoked
// principal is still somebody the audit trail names, so a listing that hid them would make an old
// row unreadable.
func (s *Store) Principals(ctx context.Context) ([]controlplane.Principal, error) {
	const q = `SELECT ` + principalColumns + ` FROM principals ORDER BY name ASC`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("postgres: principals: %w", err)
	}
	defer rows.Close()

	out := make([]controlplane.Principal, 0)
	for rows.Next() {
		p, err := scanPrincipal(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: principals: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: principals: %w", err)
	}
	return out, nil
}

// RevokePrincipal marks the principal retired at `at`, which stops every credential it holds from
// authenticating. Revoking one already revoked keeps the FIRST timestamp — that is when the access
// actually ended, and a retry of a revocation somebody was not sure landed must not move it.
func (s *Store) RevokePrincipal(ctx context.Context, id string, at time.Time) error {
	const q = `UPDATE principals SET revoked_at = $2 WHERE id = $1 AND revoked_at IS NULL`
	res, err := s.db.ExecContext(ctx, q, id, at)
	if err != nil {
		return fmt.Errorf("postgres: revoke principal %q: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("postgres: revoke principal %q: %w", id, err)
	}
	if n == 1 {
		return nil
	}
	// Nothing was updated: either there is no such principal, or it was already revoked. Only the
	// first is an error, so it costs one read to tell them apart.
	if _, err := s.Principal(ctx, id); err != nil {
		return err
	}
	return nil
}

// SaveCredential records an issued credential. It stores c.TokenHash and never a token: burrowd
// returned the token to its holder once, at issuance, and does not see it again (ADR-0084 §2). A
// credential for a principal that does not exist is refused by the foreign key.
func (s *Store) SaveCredential(ctx context.Context, c controlplane.Credential) error {
	if c.ID == "" {
		return fmt.Errorf("postgres: save credential: empty id")
	}
	if c.PrincipalID == "" {
		return fmt.Errorf("postgres: save credential %q: empty principal", c.ID)
	}
	if c.TokenHash == "" {
		return fmt.Errorf("postgres: save credential %q: empty token hash", c.ID)
	}
	const q = `
INSERT INTO credentials (` + credentialColumns + `)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`
	_, err := s.db.ExecContext(ctx, q, c.ID, c.PrincipalID, string(c.Kind), c.TokenHash,
		c.CreatedAt, nullTime(c.ExpiresAt), nullTime(c.RevokedAt), c.Enrollment)
	if err != nil {
		// The message names the credential and its principal and never the hash, let alone the
		// token: an error string is the likeliest way a credential ends up somewhere it was never
		// meant to be.
		return fmt.Errorf("postgres: save credential %q for principal %q: %w", c.ID, c.PrincipalID, err)
	}
	return nil
}

// CredentialByHash returns the credential whose stored hash is hash, or ErrNotFound. This is the
// per-request lookup, and it is one indexed equality match on a unique column.
//
// It returns the row AS STORED — expired and revoked ones included — because the caller has to be
// able to say which: "this credential expired on Tuesday" and "that is not a token this Burrow
// issued" are different answers to a person holding a token that no longer works.
func (s *Store) CredentialByHash(ctx context.Context, hash string) (controlplane.Credential, error) {
	const q = `SELECT ` + credentialColumns + ` FROM credentials WHERE token_hash = $1`
	c, err := scanCredential(s.db.QueryRowContext(ctx, q, hash))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// No identifier in the message: the only handle the caller supplied is derived from a
			// secret.
			return controlplane.Credential{}, fmt.Errorf("postgres: no such credential: %w", controlplane.ErrNotFound)
		}
		return controlplane.Credential{}, fmt.Errorf("postgres: credential lookup: %w", err)
	}
	return c, nil
}

// Credential returns the credential with the given id, or ErrNotFound. The id is the handle a
// revocation names, which is why one exists separately from the hash.
func (s *Store) Credential(ctx context.Context, id string) (controlplane.Credential, error) {
	const q = `SELECT ` + credentialColumns + ` FROM credentials WHERE id = $1`
	c, err := scanCredential(s.db.QueryRowContext(ctx, q, id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return controlplane.Credential{}, fmt.Errorf("postgres: credential %q: %w", id, controlplane.ErrNotFound)
		}
		return controlplane.Credential{}, fmt.Errorf("postgres: credential %q: %w", id, err)
	}
	return c, nil
}

// PrincipalCredentials returns every credential issued to a principal, newest first, revoked and
// expired ones included. It is the read behind "which of these is my laptop" — the question that
// decides whether the right token gets revoked.
func (s *Store) PrincipalCredentials(ctx context.Context, principalID string) ([]controlplane.Credential, error) {
	const q = `SELECT ` + credentialColumns + ` FROM credentials WHERE principal_id = $1 ORDER BY created_at DESC, id DESC`
	rows, err := s.db.QueryContext(ctx, q, principalID)
	if err != nil {
		return nil, fmt.Errorf("postgres: credentials for principal %q: %w", principalID, err)
	}
	defer rows.Close()

	out := make([]controlplane.Credential, 0)
	for rows.Next() {
		c, err := scanCredential(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: credentials for principal %q: %w", principalID, err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: credentials for principal %q: %w", principalID, err)
	}
	return out, nil
}

// RevokeCredential marks one credential revoked at `at` and touches no other — a lost laptop, a
// departure and a misbehaving agent are three different decisions, and none of them should log
// everybody out. Revoking one already revoked keeps the first timestamp.
func (s *Store) RevokeCredential(ctx context.Context, id string, at time.Time) error {
	const q = `UPDATE credentials SET revoked_at = $2 WHERE id = $1 AND revoked_at IS NULL`
	res, err := s.db.ExecContext(ctx, q, id, at)
	if err != nil {
		return fmt.Errorf("postgres: revoke credential %q: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("postgres: revoke credential %q: %w", id, err)
	}
	if n == 1 {
		return nil
	}
	if _, err := s.Credential(ctx, id); err != nil {
		return err
	}
	return nil
}

// scanPrincipal reads one principals row. revoked_at is NULL while the principal is active, which
// the domain type carries as the zero time.
func scanPrincipal(sc scanner) (controlplane.Principal, error) {
	var (
		p       controlplane.Principal
		revoked sql.NullTime
	)
	if err := sc.Scan(&p.ID, &p.Name, &p.Admin, &p.CreatedAt, &revoked); err != nil {
		return controlplane.Principal{}, err
	}
	if revoked.Valid {
		p.RevokedAt = revoked.Time
	}
	return p, nil
}

// scanCredential reads one credentials row. expires_at NULL means it does not expire and revoked_at
// NULL means it is live; both are the zero time in the domain type.
func scanCredential(sc scanner) (controlplane.Credential, error) {
	var (
		c                controlplane.Credential
		kind             string
		expires, revoked sql.NullTime
	)
	if err := sc.Scan(&c.ID, &c.PrincipalID, &kind, &c.TokenHash, &c.CreatedAt, &expires, &revoked, &c.Enrollment); err != nil {
		return controlplane.Credential{}, err
	}
	c.Kind = controlplane.CredentialKind(kind)
	if expires.Valid {
		c.ExpiresAt = expires.Time
	}
	if revoked.Valid {
		c.RevokedAt = revoked.Time
	}
	return c, nil
}

// nullTime maps the zero time to SQL NULL, so "does not expire" and "not revoked" are absent values
// in the table rather than a sentinel timestamp a later query would have to know about.
func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
