// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"errors"
	"fmt"
)

// Who may ask burrowd to mint a credential (ADR-0084 §2).
//
// The decision: THE FIRST SIGNER BECOMES AN ADMIN, and thereafter only an admin creates a
// principal, issues a credential for somebody else, or grants the admin bit. Everyone else
// receives one.
//
// The reasoning is that self-hosted starts low-stakes — the operator already holds admin on the
// cluster, so their own first credential proves nothing that reading the install Secret did not
// already prove. The feature worth building is giving ADDITIONAL people access WITHOUT giving them
// cluster admin, and the design has to make that possible rather than merely fail to prevent it.
//
// The question goes behind a seam from day one because the enterprise shape is the one not to paint
// into a corner: later, an SSO or SAML service is the party that decides who has what access. It
// replaces the implementation below; every call site stays.

// CredentialAuthorizer answers "may this caller do this to the identity model". It is the ONE place
// the admin bit is read, so an identity provider becomes the decider by replacing this and nothing
// else (ADR-0084 §2).
//
// Every method takes the caller as a principal ID rather than a resolved set of permissions,
// because a resolved copy is a copy: an implementation that asks an external provider has to ask it
// at the moment of the decision, not at the moment the request arrived.
//
// A refusal is an error wrapping ErrForbidden and names why in prose a person can act on. A method
// returns nil when the caller may proceed.
type CredentialAuthorizer interface {
	// AuthorizeCreatePrincipal reports whether caller may record a new principal — a second
	// person, or an automation. Creating one is how somebody who is not on the cluster gets
	// access at all, so it is an admin act.
	AuthorizeCreatePrincipal(ctx context.Context, caller string) error

	// AuthorizeIssue reports whether caller may issue a credential of kind for principal.
	//
	// The one non-admin case is a principal minting an AGENT credential FOR ITS OWN PRINCIPAL and
	// no other. That is not a widening: the person already holds a credential that reaches
	// burrowd, and the agent's grant is a subset of theirs. It keeps an admin out of a routine
	// operation — setting up or re-provisioning an agent — which would otherwise need one every
	// time.
	AuthorizeIssue(ctx context.Context, caller, principal string, kind CredentialKind) error

	// AuthorizeRevoke reports whether caller may revoke a credential belonging to principal.
	// Anybody may revoke their own; only an admin may revoke somebody else's.
	AuthorizeRevoke(ctx context.Context, caller, principal string) error
}

// dbAuthorizer is the local CredentialAuthorizer: it reads the admin column. It is the whole of the
// implementation the decision needs today, and the seam above is what makes replacing it with an
// identity provider an extension rather than a rewrite.
type dbAuthorizer struct {
	db Database
}

// NewDatabaseAuthorizer returns the local CredentialAuthorizer, which answers every question from
// the principals table. It is what Engine uses when no authorizer is injected.
func NewDatabaseAuthorizer(db Database) CredentialAuthorizer {
	return dbAuthorizer{db: db}
}

func (a dbAuthorizer) AuthorizeCreatePrincipal(ctx context.Context, caller string) error {
	p, err := a.admin(ctx, caller)
	if err != nil {
		return err
	}
	if !p.Admin {
		return fmt.Errorf("%w: %s is not an admin, and recording a new principal is an admin action; ask an admin to add them", ErrForbidden, p.Name)
	}
	return nil
}

func (a dbAuthorizer) AuthorizeIssue(ctx context.Context, caller, principal string, kind CredentialKind) error {
	p, err := a.admin(ctx, caller)
	if err != nil {
		return err
	}
	if p.Admin {
		return nil
	}
	// The self-agent case: a principal provisioning its own agent. Anything else — a credential
	// for another person, a second `user` credential for themselves — is an admin action.
	if caller == principal && kind == CredentialKindAgent {
		return nil
	}
	if caller != principal {
		return fmt.Errorf("%w: %s is not an admin, and only an admin issues a credential for another principal", ErrForbidden, p.Name)
	}
	return fmt.Errorf("%w: %s is not an admin, and may issue only an %s credential for themselves", ErrForbidden, p.Name, CredentialKindAgent)
}

func (a dbAuthorizer) AuthorizeRevoke(ctx context.Context, caller, principal string) error {
	p, err := a.admin(ctx, caller)
	if err != nil {
		return err
	}
	if p.Admin || caller == principal {
		return nil
	}
	return fmt.Errorf("%w: %s is not an admin, and only an admin revokes another principal's credential", ErrForbidden, p.Name)
}

// admin loads the calling principal and refuses a caller that is absent or retired. A revoked
// principal keeps its row — the audit trail depends on it — so absence is not what says a person
// has lost their access; the mark is.
func (a dbAuthorizer) admin(ctx context.Context, caller string) (Principal, error) {
	if caller == "" {
		return Principal{}, fmt.Errorf("%w: the request carries no authenticated principal", ErrForbidden)
	}
	p, err := a.db.Principal(ctx, caller)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Principal{}, fmt.Errorf("%w: no principal %q", ErrForbidden, caller)
		}
		return Principal{}, fmt.Errorf("reading principal %q: %w", caller, err)
	}
	if !p.Active() {
		return Principal{}, fmt.Errorf("%w: %s has been revoked", ErrForbidden, p.Name)
	}
	return p, nil
}
