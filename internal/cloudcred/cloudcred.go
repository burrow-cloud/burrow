// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

// Package cloudcred is where the two Burrow Cloud credentials live on disk, and the only place that
// decides which file is which.
//
// Signing in issues a pair (cloud ADR-0028 §2): the person's, which authenticates `burrow`, and the
// agent's, which authenticates `burrow-agent`. They are two files rather than one so that revoking
// either leaves the other working — losing a laptop and cutting off the agent that runs on it are
// different decisions, and a single credential cannot express both.
//
// Both files hold a token, so both are written to a fresh O_EXCL temporary and renamed into place
// under a 0700 directory. That gives three properties os.WriteFile does not: the token is never on
// disk under permissions somebody else set (WriteFile applies its mode only when it CREATES the
// file, so an existing 0644 file would stay 0644 while holding a credential); a symlink where the
// destination should be cannot redirect the token out of the directory; and a reader never sees a
// half-written file.
//
// Nothing here ever puts a token in an error, a log line, or a returned string. An error message is
// the most likely place for a credential to end up somewhere it was never meant to be, so the
// errors below carry the PATH and never the contents — including the JSON decoder's own error,
// which is deliberately dropped rather than wrapped, because a syntax error quotes the byte it
// choked on and that byte can be part of a token.
package cloudcred

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/burrow-cloud/burrow/internal/credfile"
	"github.com/burrow-cloud/burrow/localconfig"
)

// Kind is which of the pair a credential is. It is stored in the file as well as implied by the
// directory, so somebody who opens one can see what they are holding without inferring it from the
// path — and so a file that ends up in the wrong directory is caught rather than spent.
type Kind string

const (
	// KindCLI is the person's credential: what `burrow` authenticates with.
	KindCLI Kind = "cli"
	// KindAgent is the agent's credential: what `burrow-agent` authenticates with.
	KindAgent Kind = "agent"
)

// File is the credential file's name. It is the endpoint, so a second managed endpoint would be a
// second file rather than an overwrite, and the extension says what is inside.
//
// It is keyed to the product's IDENTITY (localconfig.CloudEndpoint) and deliberately not to the host
// its API is served on (localconfig.CloudAPIEndpoint): a credential is issued for the product, so
// the file it lands in must not move when the API's address does. Every credential already on disk
// is named for the identity, and it keeps loading unchanged.
const File = localconfig.CloudEndpoint + ".json"

// The two directories, both siblings of ~/.burrow/config so $BURROW_CONFIG keeps a person's whole
// Burrow state together.
//
// The agent's goes under agents/, beside the scoped kubeconfig `burrow cluster install` mints for a cluster
// (ADR-0038) — the one directory the agent's credential material has ever lived in. The person's
// goes under credentials/, so the two are never confused for each other and either can be deleted on
// its own.
const (
	humanDirName = "credentials"
	agentDirName = "agents"
)

// Credential is one stored token and the facts needed to act on it: which endpoint it is for, which
// tenant it is scoped to, and the id the console's credential list addresses it by, so revoking the
// right row does not mean guessing.
type Credential struct {
	Endpoint     string `json:"endpoint"`
	Kind         Kind   `json:"kind"`
	TenantID     string `json:"tenantId"`
	CredentialID string `json:"credentialId"`
	Name         string `json:"name,omitempty"`
	Token        string `json:"token"`
}

// Dir returns the directory a credential of this kind lives in.
func Dir(kind Kind) (string, error) {
	p, err := localconfig.Path()
	if err != nil {
		return "", err
	}
	switch kind {
	case KindCLI:
		return filepath.Join(filepath.Dir(p), humanDirName), nil
	case KindAgent:
		return filepath.Join(filepath.Dir(p), agentDirName), nil
	default:
		return "", fmt.Errorf("cloudcred: unknown credential kind %q", kind)
	}
}

// Path returns the file a credential of this kind is read from and written to. It is the single
// answer to "where is it", used by both the writer and the reader, so the two cannot drift into
// writing one place and reading another.
func Path(kind Kind) (string, error) {
	dir, err := Dir(kind)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, File), nil
}

// ErrNotSignedIn reports that no credential of the requested kind is on disk. Callers match on it to
// distinguish "never signed in" from "signed in and something is wrong with the file".
var ErrNotSignedIn = errors.New("not signed in to Burrow Cloud")

// Load reads the credential of this kind. Every failure names the path and says to sign in again,
// because that is the one action that fixes all of them, and none of them quotes the file's
// contents.
func Load(kind Kind) (Credential, error) {
	path, err := Path(kind)
	if err != nil {
		return Credential{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Credential{}, fmt.Errorf("%w: there is no credential at %s. Sign in with \"burrow auth login\"", ErrNotSignedIn, path)
		}
		return Credential{}, fmt.Errorf("cloudcred: reading %s: %w", path, err)
	}

	var c Credential
	// The decoder's error is dropped on purpose: it quotes the input it choked on, and this input is
	// a file whose largest field is a token. The path plus the remedy is everything a reader needs.
	if err := json.Unmarshal(data, &c); err != nil {
		return Credential{}, fmt.Errorf("cloudcred: %s is not readable as a credential; it may have been edited or truncated. Sign in again with \"burrow auth login\" to replace it", path)
	}
	if c.Kind != kind {
		return Credential{}, fmt.Errorf("cloudcred: %s says it is the %s credential, but that path holds the %s one; delete it and sign in again with \"burrow auth login\"", path, describe(c.Kind), describe(kind))
	}
	if c.Token == "" {
		return Credential{}, fmt.Errorf("cloudcred: %s holds no token. Sign in again with \"burrow auth login\" to replace it", path)
	}
	return c, nil
}

// describe names a kind the way a message should read. An empty or unrecognised kind is quoted
// rather than guessed at.
func describe(k Kind) string {
	switch k {
	case KindCLI:
		return "burrow"
	case KindAgent:
		return "burrow-agent"
	case "":
		return "unlabelled"
	default:
		return string(k)
	}
}

// Store writes one credential to its kind's path and returns the path it wrote. The file handling —
// 0600 under a 0700 directory, an O_EXCL temporary and a rename — is internal/credfile's, shared
// with every other credential Burrow keeps on disk so the discipline cannot drift between them.
func Store(kind Kind, cred Credential) (string, error) {
	path, err := Path(kind)
	if err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(cred, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encoding the %s credential: %w", describe(kind), err)
	}
	if err := credfile.Write(path, append(data, '\n')); err != nil {
		return "", err
	}
	return path, nil
}
