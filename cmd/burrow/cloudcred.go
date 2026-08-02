// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/burrow-cloud/burrow/localconfig"
)

// Where the two Burrow Cloud credentials go (cloud ADR-0028 §2).
//
// A sign-in issues a pair: the person's and the agent's. BOTH are stored and NEITHER is displayed.
// The person runs one command and never sees a token — a separate minting step that ends with
// somebody pasting a credential into a config file is exactly the leak the device flow was chosen to
// avoid, and a token that was on a screen has been in a scrollback buffer.
//
// The agent's goes under ~/.burrow/agents/, beside the scoped kubeconfig `burrow install` mints for
// a cluster (ADR-0038) — the one directory the agent's credential material has ever lived in. The
// person's goes under ~/.burrow/credentials/, a sibling of it, so the two are never confused for
// each other and either can be deleted on its own.
//
// Both files are written 0600 under a 0700 directory, and `burrow auth login` names both paths, so
// they can be found, read and deleted without documentation. Neither is a copy of anything the
// ~/.burrow/config target holds: a target records where the control plane is and never a credential
// (ADR-0078 §1).
//
// Nothing READS these yet, and that is the current shape of the product rather than an oversight:
// every command reaches its control plane through a kubeconfig, so a selected Burrow Cloud target is
// reported and refused (localconfig.resolveWithTarget). Storing the pair at sign-in is what makes
// the credential exist without anyone having to handle it; the transport that spends it is separate
// work.

// The two kinds of credential a sign-in issues, recorded in the file so somebody who opens one can
// see which it is without inferring it from the path.
const (
	cloudCredentialKindCLI   = "cli"
	cloudCredentialKindAgent = "agent"
)

// cloudCredentialFile names both files. It is the endpoint, so a second managed endpoint would be a
// second file rather than an overwrite, and the extension says what is inside.
const cloudCredentialFile = localconfig.CloudEndpoint + ".json"

// cloudCredential is one stored token and the facts needed to act on it: which endpoint it is for,
// which tenant it is scoped to, and the id the console's credential list addresses it by, so
// revoking the right row does not mean guessing.
type cloudCredential struct {
	Endpoint     string `json:"endpoint"`
	Kind         string `json:"kind"`
	TenantID     string `json:"tenantId"`
	CredentialID string `json:"credentialId"`
	Name         string `json:"name,omitempty"`
	Token        string `json:"token"`
}

// cloudCredentialsDir is ~/.burrow/credentials: a sibling of the local config file, so $BURROW_CONFIG
// keeps a person's whole Burrow state together the way agentDir does.
func cloudCredentialsDir() (string, error) {
	p, err := localconfig.Path()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(p), "credentials"), nil
}

// writeCloudCredentials stores the issued pair and returns the two paths. No target is recorded
// unless this returns cleanly.
//
// A failure on the second write is NOT rolled back by deleting the first. Re-authenticating over an
// existing pair is ordinary, and the credential already on disk may be a working one from an earlier
// sign-in; deleting it because a later write failed would turn a partial failure into a sign-out
// nobody asked for. The error names both paths instead, so the state is legible.
func writeCloudCredentials(t deviceTokens) (humanPath, agentPath string, err error) {
	credDir, err := cloudCredentialsDir()
	if err != nil {
		return "", "", err
	}
	agentCredDir, err := agentDir()
	if err != nil {
		return "", "", err
	}

	humanPath = filepath.Join(credDir, cloudCredentialFile)
	agentPath = filepath.Join(agentCredDir, cloudCredentialFile)

	if err := writeCloudCredential(humanPath, cloudCredential{
		Endpoint:     localconfig.CloudEndpoint,
		Kind:         cloudCredentialKindCLI,
		TenantID:     t.TenantID,
		CredentialID: t.CredentialID,
		Name:         t.Name,
		Token:        t.AccessToken,
	}); err != nil {
		return "", "", err
	}
	if err := writeCloudCredential(agentPath, cloudCredential{
		Endpoint:     localconfig.CloudEndpoint,
		Kind:         cloudCredentialKindAgent,
		TenantID:     t.TenantID,
		CredentialID: t.AgentCredentialID,
		Name:         t.Name,
		Token:        t.AgentToken,
	}); err != nil {
		return "", "", fmt.Errorf("%w\nThe credential at %s was stored; the agent's was not, so nothing was recorded.\n"+
			"Run `burrow auth login` again once the cause is fixed.", err, humanPath)
	}
	return humanPath, agentPath, nil
}

// writeCloudCredential writes one credential file 0600 under a 0700 directory.
//
// It writes a fresh file with O_EXCL and renames it over the destination rather than writing the
// destination in place. Three properties come from that, and none of them from os.WriteFile: the
// token is never on disk under permissions somebody else could have set (WriteFile applies its mode
// only when it CREATES the file, so an existing 0644 file would stay 0644 while holding a
// credential); a symlink where the destination should be cannot redirect the token out of the
// directory; and a reader never sees a half-written file. MkdirAll likewise only applies its mode on
// creation, so an existing directory is tightened explicitly.
//
// The error never carries the credential — only the path — because an error message is the most
// likely place for a token to end up somewhere it was never meant to be.
func writeCloudCredential(path string, cred cloudCredential) error {
	data, err := json.MarshalIndent(cred, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding the %s credential: %w", cred.Kind, err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("restricting %s to its owner: %w", dir, err)
	}

	// os.CreateTemp opens 0600 with O_EXCL, which is exactly the mode and the exclusivity wanted here.
	tmp, err := os.CreateTemp(dir, ".credential-*")
	if err != nil {
		return fmt.Errorf("creating a temporary file in %s: %w", dir, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // a no-op once the rename below has succeeded

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing the credential to %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tmp.Name(), err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("moving the credential into place at %s: %w", path, err)
	}
	return nil
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
		"either credential on its own at https://%s/settings.\n", localconfig.CloudEndpoint)
}
