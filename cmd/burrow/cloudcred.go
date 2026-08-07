// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"fmt"
	"io"

	"github.com/burrow-cloud/burrow/internal/cloudcred"
	"github.com/burrow-cloud/burrow/localconfig"
)

// Storing the two Burrow Cloud credentials a sign-in issues (cloud ADR-0028 §2).
//
// BOTH are stored and NEITHER is displayed. The person runs one command and never sees a token — a
// separate minting step that ends with somebody pasting a credential into a config file is exactly
// the leak the device flow was chosen to avoid, and a token that was on a screen has been in a
// scrollback buffer.
//
// Where the files go, how they are written, and how they are read back belongs to
// internal/cloudcred: both binaries read them, so one package owns the layout and neither can drift
// into writing one path and reading another. Neither file is a copy of anything the ~/.burrow/config
// target holds: a target records where the control plane is and never a credential (ADR-0078 §1).

// Aliases for the shared credential model, so this file and its tests read in the CLI's own terms.
type cloudCredential = cloudcred.Credential

const (
	cloudCredentialKindCLI   = cloudcred.KindCLI
	cloudCredentialKindAgent = cloudcred.KindAgent
	cloudCredentialFile      = cloudcred.File
)

// writeCloudCredentials stores the issued pair and returns the two paths. No target is recorded
// unless this returns cleanly.
//
// A failure on the second write is NOT rolled back by deleting the first. Re-authenticating over an
// existing pair is ordinary, and the credential already on disk may be a working one from an earlier
// sign-in; deleting it because a later write failed would turn a partial failure into a sign-out
// nobody asked for. The error names both paths instead, so the state is legible.
func writeCloudCredentials(t deviceTokens) (humanPath, agentPath string, err error) {
	humanPath, err = cloudcred.Store(cloudCredentialKindCLI, cloudCredential{
		Endpoint:     localconfig.CloudEndpoint,
		Kind:         cloudCredentialKindCLI,
		TenantID:     t.TenantID,
		CredentialID: t.CredentialID,
		Name:         t.Name,
		Token:        t.AccessToken,
	})
	if err != nil {
		return "", "", err
	}
	agentPath, err = cloudcred.Store(cloudCredentialKindAgent, cloudCredential{
		Endpoint:     localconfig.CloudEndpoint,
		Kind:         cloudCredentialKindAgent,
		TenantID:     t.TenantID,
		CredentialID: t.AgentCredentialID,
		Name:         t.Name,
		Token:        t.AgentToken,
	})
	if err != nil {
		return "", "", fmt.Errorf("%w\nThe credential at %s was stored; the agent's was not, so nothing was recorded.\n"+
			"Run `burrow auth login` again once the cause is fixed.", err, humanPath)
	}
	return humanPath, agentPath, nil
}

// reportCredentialLocations names both paths and says what is in them, without printing either
// token. Naming the path is what lets a person find, inspect and delete a credential without reading
// documentation first, and knowing which file is the agent's is what makes revoking one of them
// possible.
func reportCredentialLocations(out io.Writer, humanPath, agentPath string) {
	fmt.Fprintf(out, "\n%s Approved. Two credentials were issued for this machine and stored where only you can read them:\n\n", okMark(out))
	fmt.Fprintf(out, "  yours:           %s\n", humanPath)
	fmt.Fprintf(out, "  burrow-agent's:  %s\n", agentPath)
	fmt.Fprintf(out, "\nNeither token was printed. Delete a file to sign that half of this machine out, or revoke\n"+
		"either credential on its own at https://%s/settings.\n", localconfig.CloudAPIEndpoint)
}
