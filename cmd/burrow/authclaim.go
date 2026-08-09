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
var signInTransport = func(kubeconfig, kubeContext string) client.Transport {
	return connect.KubeconfigTransport{Options: connect.Options{
		Kubeconfig:    kubeconfig,
		Context:       kubeContext,
		ClientName:    client.ClientNameCLI,
		ClientVersion: cliVersion(),
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
}

// signInToCluster claims this install's first principal, stores the credential it returns, and
// records the install's id on tgt. tgt is mutated rather than returned so the caller writes one
// target with both facts on it — a credential recorded against no install would name no file, and an
// install id recorded with no credential would send the header and present the shared token.
func signInToCluster(ctx context.Context, kubeconfig, name string, tgt *localconfig.Target) signInResult {
	c, err := signInTransport(kubeconfig, tgt.Context).Connect(ctx)
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
	path, err := clustercred.Store(clustercred.Credential{
		InstallID:    cred.InstallID,
		PrincipalID:  cred.PrincipalID,
		Principal:    cred.Principal,
		CredentialID: cred.CredentialID,
		Kind:         cred.Kind,
		ExpiresAt:    cred.ExpiresAt,
		Token:        cred.Token,
	})
	if err != nil {
		// The credential exists on the server and cannot be written down. Say so plainly: it is not
		// recoverable — burrowd returns a token once — and the way out is to sign in again.
		return signInResult{Line: fmt.Sprintf(
			"A credential was issued and could not be saved: %s\nIt cannot be shown again. Fix the path and run `burrow auth login --context %s` to be issued another.",
			firstLine(err), tgt.Context)}
	}
	tgt.InstallID = cred.InstallID

	line := fmt.Sprintf("You are signed in as %s, with a credential of your own (%s).", cred.Principal, path)
	if cred.Admin {
		line += "\nYou are this install's admin, so you are the one who gives other people access to it."
	}
	return signInResult{Line: line, Issued: true}
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
