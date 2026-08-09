// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

// The engine tests for the agent's own credential (ADR-0084 §3).

// TestTheAgentsCredentialIsSeparateAndRevocableOnItsOwn is the whole point of the kind: revoking the
// agent has to stop the agent and leave the person signed in.
func TestTheAgentsCredentialIsSeparateAndRevocableOnItsOwn(t *testing.T) {
	ctx := context.Background()
	e, _, _ := newIdentityEngine(t)

	person, personCred, err := e.ClaimFirstPrincipal(ctx, "operator")
	if err != nil {
		t.Fatalf("ClaimFirstPrincipal: %v", err)
	}
	caller, err := e.AuthenticateCredential(ctx, personCred.Token)
	if err != nil {
		t.Fatalf("AuthenticateCredential: %v", err)
	}

	holder, agentCred, err := e.IssueAgentCredential(ctx, caller)
	if err != nil {
		t.Fatalf("IssueAgentCredential: %v", err)
	}
	if holder.ID != person.ID {
		t.Errorf("the agent credential belongs to %q, want the person's principal %q", holder.ID, person.ID)
	}
	if agentCred.Credential.Kind != cp.CredentialKindAgent {
		t.Fatalf("kind = %q, want %q", agentCred.Credential.Kind, cp.CredentialKindAgent)
	}
	if agentCred.Token == personCred.Token {
		t.Fatal("the agent was handed the person's own token")
	}

	// Revoke the agent's. The agent stops; the person does not.
	if err := e.RevokeCredential(ctx, caller, agentCred.Credential.ID); err != nil {
		t.Fatalf("RevokeCredential: %v", err)
	}
	if _, err := e.AuthenticateCredential(ctx, agentCred.Token); !errors.Is(err, cp.ErrForbidden) {
		t.Errorf("the revoked agent credential still authenticates: %v", err)
	}
	if _, err := e.AuthenticateCredential(ctx, personCred.Token); err != nil {
		t.Errorf("revoking the agent signed the person out: %v", err)
	}
}

// TestAnyoneProvisionsTheirOwnAgent: it is the one non-admin case the authorizer allows, because the
// person already holds a credential that reaches burrowd and the agent's is a copy of their own
// reach. Needing an admin for a routine re-provision is how people end up sharing one credential.
func TestAnyoneProvisionsTheirOwnAgent(t *testing.T) {
	ctx := context.Background()
	e, _, _ := newIdentityEngine(t)

	admin, _, err := e.ClaimFirstPrincipal(ctx, "operator")
	if err != nil {
		t.Fatalf("ClaimFirstPrincipal: %v", err)
	}
	_, invite, err := e.InvitePrincipal(ctx, callerFor(admin), "ada", false)
	if err != nil {
		t.Fatalf("InvitePrincipal: %v", err)
	}
	invited, err := e.AuthenticateCredential(ctx, invite.Token)
	if err != nil {
		t.Fatalf("authenticating the invitation: %v", err)
	}
	_, adaCred, err := e.RedeemInvitation(ctx, invited)
	if err != nil {
		t.Fatalf("RedeemInvitation: %v", err)
	}
	ada, err := e.AuthenticateCredential(ctx, adaCred.Token)
	if err != nil {
		t.Fatalf("AuthenticateCredential: %v", err)
	}

	if _, _, err := e.IssueAgentCredential(ctx, ada); err != nil {
		t.Fatalf("a non-admin could not provision their own agent: %v", err)
	}
}

// TestTheSharedTokenHasNoAgent: a request authenticated by the install's shared token carries no
// principal, so there is nobody for an agent credential to belong to.
func TestTheSharedTokenHasNoAgent(t *testing.T) {
	ctx := context.Background()
	e, _, _ := newIdentityEngine(t)

	if _, _, err := e.IssueAgentCredential(ctx, cp.Caller{}); !errors.Is(err, cp.ErrForbidden) {
		t.Fatalf("IssueAgentCredential with no caller = %v, want ErrForbidden", err)
	}
}
