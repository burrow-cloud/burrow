// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package client

import (
	"context"
	"net/http"
)

// The client half of signing in to a self-hosted install (ADR-0084 §1).

// CodeAlreadyClaimed is the machine-readable code burrowd returns when an install already has a
// first principal. It is a conflict rather than a failure: the caller's existing access is untouched
// and the way in is to be issued a credential by an admin, so a client branches on this to say that
// rather than reporting that signing in broke.
const CodeAlreadyClaimed = "already_claimed"

// ClusterCredential is one credential a self-hosted install issued, as it comes back over the wire.
//
// IT CARRIES THE TOKEN IN THE CLEAR, exactly once. burrowd stored only a hash and will not produce
// the token again, so a caller that drops this value has to be issued another credential. Nothing
// should log it, put it in an error, or return it in a message; the fields around it — the ids, the
// name, the kind — are the ones that are safe to print.
type ClusterCredential struct {
	// PrincipalID is the opaque, stable identity this credential authenticates as.
	PrincipalID string `json:"principal_id"`
	// Principal is that principal's human-readable handle.
	Principal string `json:"principal"`
	// Admin is whether the principal may issue a credential for somebody else.
	Admin bool `json:"admin"`
	// CredentialID is which credential this is, so a revocation can name it.
	CredentialID string `json:"credential_id"`
	// Kind is what the credential is held by, as burrowd STORED it. It is reported, never chosen:
	// a caller-declared kind would make a `deny` that binds the agent cooperative (ADR-0084 §3).
	Kind string `json:"kind"`
	// ExpiresAt is when the credential stops authenticating, RFC 3339, or empty when it does not.
	ExpiresAt string `json:"expires_at,omitempty"`
	// InstallID is the id of the install that issued this. It is what lets the caller record, in the
	// same round trip, which Burrow the credential belongs to (ADR-0084 §5) — a fact a second person
	// has no other authenticated way to learn before they hold a credential.
	InstallID string `json:"install_id,omitempty"`
	// Token is the secret. See the type's own note.
	Token string `json:"token"`
}

// ClaimFirstPrincipal records this install's first principal under name and returns the credential
// issued to them (ADR-0084 §1, §2). The first claimant becomes the install's admin.
//
// It is refused with CodeAlreadyClaimed on an install that already has a principal, which is the
// ordinary answer for anybody but the first person and is not a failure of anything: the caller's
// existing access is untouched, and an admin issues them a credential of their own.
func (c *Client) ClaimFirstPrincipal(ctx context.Context, name string) (ClusterCredential, error) {
	var out ClusterCredential
	err := c.do(ctx, http.MethodPost, "/v1/auth/claim", map[string]string{"name": name}, &out)
	return out, err
}
