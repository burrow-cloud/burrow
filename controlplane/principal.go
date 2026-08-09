// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// The per-caller identity model (ADR-0084 §2, §3): who Burrow knows, and what they hold.
//
// Today one token sits in a Secret on the cluster and everybody presents that same string, so
// burrowd cannot tell an operator from an agent from a CI job, cannot revoke one of them, and
// records a constant in the audit column that is supposed to say who acted. A principal is
// somebody Burrow knows; a credential is one token they hold, revocable on its own.
//
// NOTHING HERE IS A KUBERNETES OBJECT. The credential is a random string burrowd generates and
// stores only the hash of, so burrowd needs no authority to create ServiceAccounts, no `bind` to
// satisfy escalation prevention and no `tokenreviews` — the permissions that get harder to obtain
// exactly where per-person access matters most (ADR-0084 §2).
//
// Do not confuse this with the Credentials seam (seams.go), which is the one burrow-credentials
// Secret holding VENDOR tokens. That is what Burrow holds on an operator's behalf; this is who is
// asking Burrow for anything at all.

// CredentialKind is what a credential is held by. It is set when the token is issued and read from
// the stored row on every request; it is NEVER read from the request (ADR-0084 §3). A
// caller-declared kind would make a `deny` that binds the agent cooperative, and ADR-0020 requires
// a deny to hold "even against an over-eager or misbehaving agent".
type CredentialKind string

const (
	// CredentialKindUser is a person at a terminal.
	CredentialKindUser CredentialKind = "user"
	// CredentialKindAgent is burrow-agent running on that person's machine. It is separate from
	// their own so that revoking the agent does not log the person out (ADR-0038).
	CredentialKindAgent CredentialKind = "agent"
	// CredentialKindMachine is CI, a script, an automation: no browser and no person. Nothing
	// issues one today; it is named because leaving it out is what produces a second mechanism the
	// first time an automation asks (ADR-0084 §3).
	CredentialKindMachine CredentialKind = "machine"
)

// Valid reports whether k is one of the three kinds.
func (k CredentialKind) Valid() bool {
	switch k {
	case CredentialKindUser, CredentialKindAgent, CredentialKindMachine:
		return true
	}
	return false
}

// Principal is somebody Burrow knows: a person, or an automation that acts as one. It is the thing
// an audit row names and the thing a credential belongs to.
type Principal struct {
	// ID is the principal's opaque, stable identifier. Credentials and audit rows reference it,
	// so it survives a rename of Name.
	ID string
	// Name is the human-readable handle, unique across the install.
	Name string
	// Admin is whether this principal may create another principal, issue a credential for
	// somebody else, and grant the admin bit (ADR-0084 §2).
	//
	// It is a PROPERTY RECORDED ABOUT A PRINCIPAL, never a special case of "the person who ran
	// install". That is what lets an SSO or SAML service later become the thing that answers "is
	// this caller an admin" — an extension rather than a rewrite.
	Admin bool
	// CreatedAt is when the principal was recorded.
	CreatedAt time.Time
	// RevokedAt is when the principal was retired, or the zero time while it is active. A
	// principal is marked rather than deleted so the audit rows that name it keep meaning
	// something.
	RevokedAt time.Time
}

// Active reports whether the principal has not been revoked.
func (p Principal) Active() bool { return p.RevokedAt.IsZero() }

// Credential is one issued token, as burrowd stores it. THE TOKEN ITSELF IS NOT HERE: only its
// hash, because burrowd returns the token once at issuance and never sees it again.
type Credential struct {
	// ID is the credential's opaque identifier — the handle a revocation names.
	ID string
	// PrincipalID is the principal this credential authenticates as.
	PrincipalID string
	// Kind is what holds it (ADR-0084 §3).
	Kind CredentialKind
	// TokenHash is HashToken of the issued token: the only form burrowd keeps.
	TokenHash string
	// CreatedAt is when the credential was issued.
	CreatedAt time.Time
	// ExpiresAt is when the credential stops authenticating, or the zero time when it does not
	// expire. It is checked on every lookup.
	ExpiresAt time.Time
	// RevokedAt is when the credential was revoked, or the zero time while it is live.
	RevokedAt time.Time
	// Enrollment marks an INVITATION rather than a credential anybody operates with: the one thing
	// it may do is be exchanged, once, for the credential its holder will carry. It is what lets a
	// second person be given access without a working token travelling through a chat window — the
	// token they end up with is generated on their own machine and never goes anywhere.
	//
	// Everything an install has issued until now is false, which is the column's default and the
	// right answer: a credential that authenticates is the ordinary case, and an invitation is the
	// exception that has to say so.
	Enrollment bool
}

// Live reports whether the credential authenticates at now: not revoked, and not expired.
func (c Credential) Live(now time.Time) bool {
	if !c.RevokedAt.IsZero() {
		return false
	}
	return c.ExpiresAt.IsZero() || now.Before(c.ExpiresAt)
}

// IssuedCredential is what an issue returns exactly ONCE: the stored row, and the secret alongside
// it. The secret exists nowhere else — not in the database, not in a log, not in an error — so a
// caller that drops it cannot recover it and has to issue another.
type IssuedCredential struct {
	// Credential is the row as it was stored.
	Credential Credential
	// Token is the secret, in the clear, for this one return.
	Token string
}

// String redacts the token. IssuedCredential is the one value in the control plane that carries a
// secret in the clear, so the default formatting verbs must not print it: a %v in a log line or an
// error is the most likely way a credential ends up somewhere it was never meant to be.
func (i IssuedCredential) String() string {
	return "controlplane.IssuedCredential{" + i.Credential.ID + ", token redacted}"
}

// Caller is the authenticated identity behind a request: the principal, and which of their
// credentials presented itself.
//
// IT DELIBERATELY DOES NOT CARRY THE ADMIN BIT. "May this caller do this" is answered in exactly
// one place — the CredentialAuthorizer seam — so no call site can branch on a copy of the bit that
// was true when the request arrived, and so replacing that seam with an identity provider replaces
// the whole answer rather than most of it.
type Caller struct {
	// PrincipalID is the principal the presented credential belongs to.
	PrincipalID string
	// PrincipalName is that principal's handle, carried for messages and the audit trail.
	PrincipalName string
	// Kind is what the presented credential is held by, read from the stored row (ADR-0084 §3).
	Kind CredentialKind
	// CredentialID is which credential presented itself, so a revocation can name it.
	CredentialID string
	// Enrollment is whether the presented credential is an invitation — something to be exchanged
	// once, and not something to operate with. It is read from the stored row like Kind, and the
	// API layer refuses such a caller everywhere but the exchange.
	Enrollment bool
}

// HashToken returns the stored form of a credential token: a hex-encoded SHA-256.
//
// A plain hash rather than a password KDF, and that is deliberate. A KDF exists to slow an attacker
// guessing a human-chosen secret; these tokens are full-entropy random strings from TokenSource, so
// there is nothing to guess and the cost would be paid on every authenticated request instead.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
