// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

// Package clustercred is where the credential a self-hosted Burrow issued lives on disk (ADR-0084
// §1).
//
// Until now a self-hosted install had one token, in a Secret on the cluster, that everybody
// presented. Signing in mints a credential for one person, and this is where it is kept: beside the
// Burrow Cloud credential, under the same 0700 directory, written with the same discipline
// (internal/credfile). One shape for both, because the mechanism is the same one — a random token
// the control plane stores only the hash of — and the whole point of ADR-0084 §2 converging on it
// was to stop having two.
//
// IT IS KEYED BY INSTALL ID, NOT BY CONTEXT NAME. A kube context name is a label: it can be renamed,
// two merged kubeconfigs can share one, and a provider regenerates it deterministically for a
// rebuilt cluster (ADR-0084 §5). The credential belongs to the Burrow that issued it, so the install
// is what names the file — a renamed context still finds it, and a rebuilt cluster does not
// accidentally present the old install's token to the new one. Two targets pointed at the same
// install share one credential, which is correct: it is one install and one principal.
//
// NOTHING HERE PUTS A TOKEN IN AN ERROR, a log line, or a returned string, on exactly the terms
// internal/cloudcred does not.
package clustercred

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/burrow-cloud/burrow/internal/credfile"
	"github.com/burrow-cloud/burrow/localconfig"
)

// dirName is the directory the person's credentials live in, a sibling of ~/.burrow/config so
// $BURROW_CONFIG keeps a person's whole Burrow state together. It is the same directory the Burrow
// Cloud credential uses, because it holds the same thing: what `burrow` authenticates with.
const dirName = "credentials"

// filePrefix distinguishes a cluster credential from the managed product's in the same directory.
// The managed one is named for its endpoint; an install id is opaque, so the prefix is what makes
// the file legible to somebody who opens the directory.
const filePrefix = "cluster-"

// ErrNoCredential reports that no credential for this install is on disk. Callers match on it to
// tell "never signed in to this install" — the ordinary state of every install today, and the one
// that falls back to the shared install token — from "signed in and something is wrong with the
// file".
var ErrNoCredential = errors.New("no Burrow credential for this install")

// Credential is one credential a self-hosted install issued, as stored.
//
// It records WHICH INSTALL issued it as well as living in a file named for it, so a file that ends
// up under the wrong name is caught rather than spent against an install that never issued it — the
// same check cloudcred makes on its kind.
type Credential struct {
	// InstallID is the Burrow that issued this (ADR-0084 §5).
	InstallID string `json:"installId"`
	// PrincipalID is the opaque identity it authenticates as.
	PrincipalID string `json:"principalId"`
	// Principal is that principal's handle, carried so a message can name who is signed in without
	// a round trip.
	Principal string `json:"principal"`
	// CredentialID is which credential this is, so a revocation can name it.
	CredentialID string `json:"credentialId"`
	// Kind is what burrowd RECORDED this credential as (`user`, `agent`, `machine`). It is stored
	// for display only: what a credential is, is the control plane's answer, read from its own row
	// on every request and never taken from anything the client says (ADR-0084 §3).
	Kind string `json:"kind,omitempty"`
	// ExpiresAt is when it stops authenticating, RFC 3339, or empty when it does not expire.
	ExpiresAt string `json:"expiresAt,omitempty"`
	// Token is the secret.
	Token string `json:"token"`
}

// Dir returns the directory cluster credentials live in.
func Dir() (string, error) {
	p, err := localconfig.Path()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(p), dirName), nil
}

// Path returns the file an install's credential is read from and written to. It is the single answer
// to "where is it", used by both the writer and the reader, so the two cannot drift into writing one
// place and reading another.
func Path(installID string) (string, error) {
	if err := validInstallID(installID); err != nil {
		return "", err
	}
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, filePrefix+installID+".json"), nil
}

// validInstallID refuses an id that could name a file outside the credential directory. An install
// id arrives from a control plane over the network and is then used to build a path, so it is
// checked here rather than trusted: a burrowd that returned "../../.ssh/id_rsa" must not be able to
// choose where a token is written or which file is read as one.
func validInstallID(installID string) error {
	if strings.TrimSpace(installID) == "" {
		return fmt.Errorf("clustercred: no install id, so there is no credential to name")
	}
	if strings.ContainsAny(installID, `/\`) || strings.Contains(installID, "..") || installID != strings.TrimSpace(installID) {
		return fmt.Errorf("clustercred: %q is not a usable install id", installID)
	}
	return nil
}

// Load reads the credential for an install. Every failure names the path and says how to replace it,
// and none of them quotes the file's contents.
func Load(installID string) (Credential, error) {
	path, err := Path(installID)
	if err != nil {
		return Credential{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Credential{}, fmt.Errorf("%w: there is no credential at %s. Sign in with \"burrow auth login\"", ErrNoCredential, path)
		}
		return Credential{}, fmt.Errorf("clustercred: reading %s: %w", path, err)
	}

	var c Credential
	// The decoder's error is dropped on purpose: it quotes the input it choked on, and this input is
	// a file whose largest field is a token. The path plus the remedy is everything a reader needs.
	if err := json.Unmarshal(data, &c); err != nil {
		return Credential{}, fmt.Errorf("clustercred: %s is not readable as a credential; it may have been edited or truncated. Sign in again with \"burrow auth login\" to replace it", path)
	}
	if c.InstallID != installID {
		return Credential{}, fmt.Errorf("clustercred: %s says it was issued by install %s, not %s; delete it and sign in again with \"burrow auth login\"", path, c.InstallID, installID)
	}
	if c.Token == "" {
		return Credential{}, fmt.Errorf("clustercred: %s holds no token. Sign in again with \"burrow auth login\" to replace it", path)
	}
	return c, nil
}

// Token returns the token to present to an install, or empty when there is none to present.
//
// EVERY FAILURE IS AN EMPTY STRING, deliberately, because of where this is called from: the caller
// is about to connect, and its fallback is the shared install token, which still works (ADR-0084
// "Existing installs keep working"). An install nobody has signed in to, a credential file somebody
// deleted, a home directory that cannot be read — all of them mean "present nothing extra" rather
// than "refuse to connect". A credential that exists and is broken is reported by Load, which is
// what `burrow auth status` and the sign-in path call.
func Token(installID string) string {
	if installID == "" {
		return ""
	}
	c, err := Load(installID)
	if err != nil {
		return ""
	}
	return c.Token
}

// Store writes an install's credential and returns the path it wrote. The file handling — 0600 under
// a 0700 directory, an O_EXCL temporary and a rename — is internal/credfile's.
func Store(cred Credential) (string, error) {
	path, err := Path(cred.InstallID)
	if err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(cred, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encoding the credential for install %s: %w", cred.InstallID, err)
	}
	if err := credfile.Write(path, append(data, '\n')); err != nil {
		return "", err
	}
	return path, nil
}
