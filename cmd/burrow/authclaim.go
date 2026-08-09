// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/burrow-cloud/burrow/client"
	"github.com/burrow-cloud/burrow/connect"
	"github.com/burrow-cloud/burrow/internal/clustercred"
	"github.com/burrow-cloud/burrow/localconfig"
)

// Signing in to a self-hosted install (ADR-0084 §1).
//
// `burrow auth login --context <cluster>` used to record a name and stop. Now it also asks the
// Burrow behind that context for a credential of the person's own, and records which install issued
// it. The kubeconfig is used ONCE, to prove the caller is an operator of that cluster: reaching
// burrowd through the API-server proxy and presenting the shared install token means having read a
// Secret that cluster RBAC guards, so the claim bootstraps from access the caller already holds
// rather than granting anything new.
//
// AFTERWARDS THE KUBECONFIG IS STILL THE ROUTE AND NO LONGER THE IDENTITY. burrowd has no address of
// its own, so requests keep going through the proxy; what changes is that the token in
// X-Burrow-Token says who is asking (ADR-0084 §2).
//
// NOTHING HERE FAILS THE LOGIN. Choosing where you use Burrow has to work against a cluster that is
// unreachable, that has no Burrow in it yet, that runs a control plane too old to have this route,
// or that somebody has already claimed. Every one of those leaves the target recorded and the shared
// install token working, which is what ADR-0084's "existing installs keep working" means in
// practice, and each says which one it was rather than reporting that signing in broke.

// signInTransport is how the sign-in reaches the cluster's control plane: the kubeconfig, through
// the API-server proxy, with the shared install token read from the install Secret. That read is the
// proof of operator-ship this whole exchange rests on, so it is deliberately the ordinary transport
// and not a special one.
//
// NO INSTALL ID IS SENT. This call is how the id is LEARNED, so asserting one here would refuse
// exactly the case the exchange exists to establish (ADR-0084 §5).
//
// It is a package var for the reason listContexts and stdinIsTerminal are: a test substitutes a
// control plane it can drive, rather than needing a cluster to assert what the CLI does with each
// answer.
//
// token is the credential to present. It is empty on the claim path, which is what makes that path
// read the shared install token out of the Secret and thereby prove operator-ship. On the invitation
// path it is the invitation, and the Secret is never read — which is the point: the second person
// has no cluster RBAC to read it with, and the whole reason they can be given access at all is that
// they no longer need any (ADR-0084 §2).
var signInTransport = func(kubeconfig, kubeContext, token string) client.Transport {
	return connect.KubeconfigTransport{Options: connect.Options{
		Kubeconfig:    kubeconfig,
		Context:       kubeContext,
		ClientName:    client.ClientNameCLI,
		ClientVersion: cliVersion(),
		Token:         token,
	}}
}

// signInResult is what the sign-in attempt has to say afterwards: the line to print, and whether a
// credential was actually issued. The caller prints; this function decides.
type signInResult struct {
	// Line is one line for the person, already worded for its case. Empty prints nothing.
	Line string
	// Issued is whether a credential was minted and stored. False means the shared install token is
	// still what this target's commands present.
	Issued bool
	// Agent is what happened to burrow-agent's own credential, which a sign-in issues alongside the
	// person's (ADR-0084 §3). It is reported separately because it succeeds and fails separately: a
	// person can be signed in with their own credential while the agent falls back to the shared
	// install token, and saying so under one mark would make one of the two read wrong.
	Agent agentCredentialResult
}

// signInToCluster claims this install's first principal, stores the credential it returns, and
// records the install's id on tgt. tgt is mutated rather than returned so the caller writes one
// target with both facts on it — a credential recorded against no install would name no file, and an
// install id recorded with no credential would send the header and present the shared token.
func signInToCluster(ctx context.Context, kubeconfig, name string, tgt *localconfig.Target) signInResult {
	// Whether the credential can be SAVED is checked before one is asked for. burrowd returns a token
	// once, so a write that fails afterwards destroys a credential that already exists on the server —
	// and on a first claim that leaves the install claimed by a principal whose only token is gone.
	if err := clustercred.EnsureWritable(clustercred.KindCLI); err != nil {
		return signInResult{Line: fmt.Sprintf(
			"No credential was requested, because there is nowhere to save one: %s\nNothing was changed on the cluster. Your commands use the install's shared token, which keeps working.",
			firstLine(err))}
	}

	c, err := signInTransport(kubeconfig, tgt.Context, "").Connect(ctx)
	if err != nil {
		return signInResult{Line: fmt.Sprintf(
			"No credential of your own was issued: %s\nYour commands use the install's shared token, which keeps working. Run `burrow auth login --context %s` again once the cluster answers.",
			firstLine(err), tgt.Context)}
	}

	cred, err := c.ClaimFirstPrincipal(ctx, name)
	if err != nil {
		return signInResult{Line: claimRefusalLine(err)}
	}

	// The id first: it is what names the file, so a credential is never written anywhere the reader
	// would not look for it.
	if cred.InstallID == "" {
		return signInResult{Line: "This Burrow issued a credential but does not know its own install id, so there is nowhere to file it.\n" +
			"It predates install ids; run `burrow cluster upgrade` and sign in again. Your commands use the install's shared token meanwhile."}
	}
	path, err := storeIssuedCredential(cred, tgt)
	if err != nil {
		// The write was probed before anything was minted, so reaching here means the filesystem
		// changed underneath the sign-in. Say what it costs rather than offering a remedy that will
		// not work: the token is gone for good, this install is now claimed, and a second claim is
		// refused — so signing in again does NOT hand out another. What still works is the shared
		// install token, which is exactly what ADR-0084 §8 keeps it for.
		return signInResult{Line: fmt.Sprintf(
			"A credential was issued and could not be saved: %s\nIt cannot be shown again, and this install now has an admin, so signing in will not issue another.\n"+
				"Your commands use the install's shared token, which keeps working and reaches everything it did before.",
			firstLine(err))}
	}

	line := fmt.Sprintf("You are signed in as %s, with a credential of your own (%s).", cred.Principal, path)
	if cred.Admin {
		line += "\nYou are this install's admin, so you are the one who gives other people access to it."
	}
	return signInResult{Line: line, Issued: true, Agent: issueAgentCredential(ctx, kubeconfig, tgt.Context, cred)}
}

// acceptInvitation is the second person's side of ADR-0084 §2: it exchanges the invitation an admin
// issued for the credential this person will carry, and records the install that issued it.
//
// THE CREDENTIAL IS CREATED BY THIS CALL, on this machine. What the admin sent is spent here and is
// worth nothing afterwards, so the token this person operates with has never been through anybody
// else's chat window, mailbox or terminal scrollback.
//
// UNLIKE THE CLAIM, A FAILURE HERE FAILS THE COMMAND. The claim can fall back and say so, because
// the shared install token still reaches everything and the person had it all along. Somebody
// exchanging an invitation has nothing to fall back to: they were given access precisely so that
// they would not need the install's shared token, and recording a target they cannot authenticate
// to would leave them with a Burrow that answers every command with a 401 and no clue why.
func acceptInvitation(ctx context.Context, kubeconfig, invite string, tgt *localconfig.Target) (signInResult, error) {
	// Probed before the exchange, for the reason the claim probes: burrowd returns the token once,
	// and an invitation that has been spent cannot be spent again — so a write that fails afterwards
	// costs this person their way in and an admin a second invitation.
	if err := clustercred.EnsureWritable(clustercred.KindCLI); err != nil {
		return signInResult{}, fmt.Errorf(
			"the invitation was not exchanged, because there is nowhere to save the credential it would return: %w\n"+
				"Nothing was used up: the invitation still works once this is fixed", err)
	}

	c, err := signInTransport(kubeconfig, tgt.Context, invite).Connect(ctx)
	if err != nil {
		return signInResult{}, fmt.Errorf("reaching the Burrow at context %q: %w", tgt.Context, err)
	}

	cred, err := c.RedeemInvitation(ctx)
	if err != nil {
		return signInResult{}, invitationRefusal(err, tgt.Context)
	}
	if cred.InstallID == "" {
		return signInResult{}, errors.New(
			"this Burrow issued a credential but does not know its own install id, so there is nowhere to file it.\n" +
				"It predates install ids; ask an admin to run `burrow cluster upgrade`, and to invite you again")
	}
	path, err := storeIssuedCredential(cred, tgt)
	if err != nil {
		// The invitation has already been spent, so there is no honest remedy but another one.
		return signInResult{}, fmt.Errorf(
			"the credential was issued and could not be saved: %w\nIt cannot be shown again and the invitation has been used, so ask an admin for another", err)
	}

	line := fmt.Sprintf("You are signed in as %s, with a credential of your own (%s).", cred.Principal, path)
	line += "\nIt was created here, by that exchange, so the invitation you were sent is now spent."
	if cred.Admin {
		line += "\nYou are an admin of this install, so you can give other people access to it too."
	}
	return signInResult{Line: line, Issued: true, Agent: issueAgentCredential(ctx, kubeconfig, tgt.Context, cred)}, nil
}

// invitationRefusal words the ways an exchange is refused. Each one is a different thing to do next,
// and the person reading it has just been handed a string by a colleague and told it would work.
func invitationRefusal(err error, kubeContext string) error {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Code {
		case client.CodeUnauthorized, client.CodeCredentialNotLive:
			return fmt.Errorf("this Burrow did not accept that invitation: %s\n"+
				"An invitation expires, and can be exchanged only once. Ask whoever invited you for another", firstLine(err))
		case client.CodeUnknownOperation:
			return fmt.Errorf("the Burrow at context %q is too old to exchange an invitation.\n"+
				"Ask an admin to run `burrow cluster upgrade`, then try again", kubeContext)
		}
	}
	return fmt.Errorf("exchanging the invitation: %w", err)
}

// storeIssuedCredential writes an issued credential to disk and records the install on tgt. Both
// facts belong to one write: a credential filed under an install the target does not name is a
// credential no later command looks for, and an install id recorded with no credential beside it
// sends the header and then presents the shared token.
func storeIssuedCredential(cred client.ClusterCredential, tgt *localconfig.Target) (string, error) {
	path, err := clustercred.Store(clustercred.KindCLI, clustercred.Credential{
		InstallID:    cred.InstallID,
		PrincipalID:  cred.PrincipalID,
		Principal:    cred.Principal,
		CredentialID: cred.CredentialID,
		Kind:         cred.Kind,
		ExpiresAt:    cred.ExpiresAt,
		Token:        cred.Token,
	})
	if err != nil {
		return "", err
	}
	tgt.InstallID = cred.InstallID
	return path, nil
}

// claimRefusalLine words the cases where burrowd answered and would not issue. Each has a different
// next step, and a person reading this is deciding what to do rather than debugging Burrow.
func claimRefusalLine(err error) string {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Code {
		case client.CodeAlreadyClaimed:
			// The ordinary answer for the second person onwards, and not a failure of anything.
			return "This Burrow already has an admin, so no credential was issued to you here.\n" +
				"Ask them to issue you one. Your commands use the install's shared token meanwhile."
		case client.CodeUnknownOperation:
			// A control plane older than this CLI. Nothing is wrong with the install.
			return "This Burrow is too old to issue per-person credentials, so none was issued.\n" +
				"Your commands use the install's shared token, exactly as before. `burrow cluster upgrade` adds it."
		}
	}
	return fmt.Sprintf("No credential of your own was issued: %s\nYour commands use the install's shared token, which keeps working.", firstLine(err))
}

// firstLine trims a multi-line error down to its first line, so a note stays a note. The full error
// belongs to a command that failed; this one did not.
func firstLine(err error) string {
	s := strings.TrimSpace(err.Error())
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// checkInvite refuses the flag combinations an invitation cannot mean, by name rather than by
// quietly ignoring one of them. Somebody exchanging an invitation is doing it for the first time,
// following instructions from a colleague, and a flag that was accepted and had no effect is the
// worst outcome available: they would end up signed in with the install's shared token, or with no
// credential at all, and nothing would have said so.
func (o authLoginOpts) checkInvite() error {
	if o.invite == "" {
		return nil
	}
	switch {
	case o.cloud:
		return errors.New("--invite is an invitation to a cluster's Burrow, and --cloud selects the managed product,\n" +
			"which issues its own credentials when you sign in. Pass --context <cluster> with the invitation")
	case o.kubeContext == "":
		return errors.New("--invite needs --context <cluster>: the invitation says who you are, and the kubeconfig context\n" +
			"says which cluster to reach. Both are required, and Burrow will not guess the second")
	case strings.TrimSpace(o.name) != "":
		return errors.New("--name and --invite cannot both be given: the name was recorded by whoever invited you, and\n" +
			"the invitation is what says which principal it belongs to. Drop --name")
	}
	return nil
}

// principalName is the name to record for this sign-in: what --name said, else the environment's
// idea of who is at the terminal.
func (o authLoginOpts) principalName() string {
	if n := strings.TrimSpace(o.name); n != "" {
		return n
	}
	return defaultPrincipalName()
}

// defaultPrincipalName is the name recorded for the person signing in when they do not give one.
//
// It reads the environment rather than the password database on purpose. The name is a label on an
// audit row, not an authentication of anybody — burrowd authenticates the token, not the string —
// so the cheap answer is the right one, and an environment variable is something a test can set
// where the machine's own user is something a test would read.
func defaultPrincipalName() string {
	for _, key := range []string{"BURROW_PRINCIPAL", "USER", "USERNAME", "LOGNAME"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return "operator"
}
