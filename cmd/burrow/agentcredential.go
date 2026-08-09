// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/burrow-cloud/burrow/client"
	"github.com/burrow-cloud/burrow/internal/clustercred"
)

// The agent's own credential on a self-hosted install (ADR-0084 §3).
//
// `burrow-agent` has always reached burrowd through the scoped kubeconfig `burrow cluster install`
// mints for it (ADR-0038), and then presented the install's shared token — the same string the
// operator presents. So the agent was indistinguishable from the person driving it, and the only way
// to stop it was to rotate the install, which logs everybody out.
//
// SIGNING IN NOW ISSUES A PAIR, which is what the Burrow Cloud sign-in has always done: the person's
// credential, and the agent's. Two rows, two files, two revocations. Cutting the agent off is one of
// them, and the person's terminal keeps working.
//
// WHY THIS HAPPENS AT SIGN-IN AND NOT AT `burrow agent <tool> install`. The agent's credential
// belongs to the INSTALL and to the person, not to the coding tool: it is the same credential
// whether the agent being wired is Claude Code or something else, and `agent <tool> install` writes
// a settings file on this machine and contacts nothing. Sign-in is where a person and an install
// meet, so it is where both credentials are issued — and it is what the managed product already
// does, which keeps one story for both.
//
// IT NEVER FAILS THE SIGN-IN. A person who has just been issued their own credential has a working
// setup; an agent without one falls back to the install's shared token, exactly as it does today.

// agentCredentialResult is what the attempt has to say afterwards: one line, and whether the agent
// actually got one.
type agentCredentialResult struct {
	// Line is the sentence to append to the sign-in's own, or empty.
	Line string
	// Issued is whether the agent's credential was minted and stored.
	Issued bool
}

// issueAgentCredential asks the install for a credential for `burrow-agent` and files it under the
// agent's directory.
//
// It presents the credential that was JUST ISSUED to the person, rather than whatever the sign-in
// itself presented. That is not incidental: the claim path presented the install's shared token,
// which names nobody and cannot be the principal an agent credential belongs to, and the invitation
// path presented an invitation, which has been spent by the time this runs. The person's new
// credential is the only thing on hand that is somebody.
func issueAgentCredential(ctx context.Context, kubeconfig, kubeContext string, cred client.ClusterCredential) agentCredentialResult {
	// Probed before anything is minted, for the reason the sign-in probes: burrowd returns a token
	// once, so a write that fails afterwards has thrown away a credential that exists on the server.
	// The cost is smaller here — another can always be issued — and the probe is cheap.
	if err := clustercred.EnsureWritable(clustercred.KindAgent); err != nil {
		return agentCredentialResult{Line: fmt.Sprintf(
			"burrow-agent has no credential of its own, because there is nowhere to save one: %s\nIt uses the install's shared token, which keeps working.",
			firstLine(err))}
	}

	c, err := signInTransport(kubeconfig, kubeContext, cred.Token).Connect(ctx)
	if err != nil {
		return agentCredentialResult{Line: agentFallbackLine(err)}
	}
	agentCred, err := c.IssueAgentCredential(ctx)
	if err != nil {
		return agentCredentialResult{Line: agentFallbackLine(err)}
	}
	path, err := clustercred.Store(clustercred.KindAgent, clustercred.Credential{
		InstallID:    cred.InstallID,
		PrincipalID:  agentCred.PrincipalID,
		Principal:    agentCred.Principal,
		CredentialID: agentCred.CredentialID,
		Kind:         agentCred.Kind,
		ExpiresAt:    agentCred.ExpiresAt,
		Token:        agentCred.Token,
	})
	if err != nil {
		return agentCredentialResult{Line: fmt.Sprintf(
			"burrow-agent was issued a credential that could not be saved: %s\nIt uses the install's shared token, which keeps working. Signing in again issues another.",
			firstLine(err))}
	}
	return agentCredentialResult{
		Line:   fmt.Sprintf("burrow-agent has a credential of its own (%s), so revoking it does not sign you out.", path),
		Issued: true,
	}
}

// agentFallbackLine words the cases where the install would not issue one. All of them leave a
// working setup, so the line says what the agent will use instead rather than reporting a failure.
func agentFallbackLine(err error) string {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) && apiErr.Code == client.CodeUnknownOperation {
		return "This Burrow is too old to issue burrow-agent a credential of its own, so it uses the install's\n" +
			"shared token, exactly as before. `burrow cluster upgrade` adds it."
	}
	return fmt.Sprintf(
		"burrow-agent has no credential of its own: %s\nIt uses the install's shared token, which keeps working. Signing in again tries once more.",
		firstLine(err))
}
