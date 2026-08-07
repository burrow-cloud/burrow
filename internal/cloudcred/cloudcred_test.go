// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package cloudcred

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The two tokens the tests store. They are distinct strings so a test can tell which file was read
// from the value alone — the whole point being that reading the wrong one is a real failure mode.
const (
	humanSecret = "human-token-must-never-be-spent-by-the-agent"
	agentSecret = "agent-token-must-never-be-spent-by-the-cli"
)

// burrowHome points $BURROW_CONFIG at a temp directory, so nothing a test writes goes anywhere near
// a real ~/.burrow.
func burrowHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BURROW_CONFIG", filepath.Join(dir, "config"))
	return dir
}

// storePair writes both credentials, the way a sign-in does.
func storePair(t *testing.T) {
	t.Helper()
	if _, err := Store(KindCLI, Credential{Endpoint: "burrow-cloud.dev", Kind: KindCLI, TenantID: "t1", CredentialID: "cred-human", Token: humanSecret}); err != nil {
		t.Fatalf("store the person's credential: %v", err)
	}
	if _, err := Store(KindAgent, Credential{Endpoint: "burrow-cloud.dev", Kind: KindAgent, TenantID: "t1", CredentialID: "cred-agent", Token: agentSecret}); err != nil {
		t.Fatalf("store the agent's credential: %v", err)
	}
}

// TestEachBinaryReadsItsOwnCredential is the load-bearing test of the whole two-credential model
// (cloud ADR-0028 §2). Two files exist so that revoking one does not revoke the other; that promise
// is only true if each side reads its own and never reaches for the other.
func TestEachBinaryReadsItsOwnCredential(t *testing.T) {
	home := burrowHome(t)
	storePair(t)

	human, err := Load(KindCLI)
	if err != nil {
		t.Fatalf("Load(KindCLI): %v", err)
	}
	if human.Token != humanSecret {
		t.Error("burrow read a credential that is not the person's")
	}
	if human.CredentialID != "cred-human" {
		t.Errorf("CredentialID = %q, want the person's", human.CredentialID)
	}

	agent, err := Load(KindAgent)
	if err != nil {
		t.Fatalf("Load(KindAgent): %v", err)
	}
	if agent.Token != agentSecret {
		t.Error("burrow-agent read a credential that is not its own")
	}

	// And the paths are the two documented directories, not one file read twice.
	humanPath, _ := Path(KindCLI)
	agentPath, _ := Path(KindAgent)
	if humanPath != filepath.Join(home, "credentials", File) {
		t.Errorf("the person's credential is at %q", humanPath)
	}
	if agentPath != filepath.Join(home, "agents", File) {
		t.Errorf("the agent's credential is at %q", agentPath)
	}
}

// TestCredentialFileIsKeyedToTheIdentityNotTheAPIHost. The managed product is named by one host and
// addressed at another; the filename follows the NAME, so moving the API's address does not orphan
// every credential already on disk. The name is written out rather than derived from the constant,
// because a derived assertion would agree with any change to it.
func TestCredentialFileIsKeyedToTheIdentityNotTheAPIHost(t *testing.T) {
	if File != "burrow-cloud.dev.json" {
		t.Errorf("credential file = %q — renaming it strands every credential already stored", File)
	}
	// A credential written by hand at that name, the way one already on disk sits, still loads.
	home := burrowHome(t)
	dir := filepath.Join(home, "credentials")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := `{"endpoint":"burrow-cloud.dev","kind":"cli","tenantId":"t1","credentialId":"cred-human","token":"` + humanSecret + `"}`
	if err := os.WriteFile(filepath.Join(dir, "burrow-cloud.dev.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("seed the credential: %v", err)
	}
	cred, err := Load(KindCLI)
	if err != nil {
		t.Fatalf("a credential stored under the old identity no longer loads: %v", err)
	}
	if cred.Token != humanSecret || cred.Endpoint != "burrow-cloud.dev" {
		t.Errorf("credential = %+v, want the one on disk", cred.CredentialID)
	}
}

// TestRevokingOneDoesNotSilentlyPromoteTheOther is the negative half. Deleting the agent's file
// stands in for revoking the agent's credential and cleaning up after it: the agent must then fail,
// not quietly fall back to the person's credential, which would make revocation meaningless.
func TestRevokingOneDoesNotSilentlyPromoteTheOther(t *testing.T) {
	burrowHome(t)
	storePair(t)

	agentPath, err := Path(KindAgent)
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if err := os.Remove(agentPath); err != nil {
		t.Fatalf("remove the agent's credential: %v", err)
	}

	cred, err := Load(KindAgent)
	if err == nil {
		t.Fatalf("Load(KindAgent) succeeded with no agent credential on disk, returning a credential for %q", cred.CredentialID)
	}
	if !errors.Is(err, ErrNotSignedIn) {
		t.Errorf("error = %v, want ErrNotSignedIn", err)
	}
	if strings.Contains(err.Error(), humanSecret) {
		t.Error("the error carried the person's token")
	}

	// The person's credential is untouched, which is the other half of the promise.
	if human, err := Load(KindCLI); err != nil || human.Token != humanSecret {
		t.Errorf("the person's credential stopped working when the agent's was revoked: %v", err)
	}
}

// TestMissingCredentialNamesSignIn: never having signed in is the most common reason a credential is
// absent, and the message has to say so rather than surfacing a bare file-not-found.
func TestMissingCredentialNamesSignIn(t *testing.T) {
	burrowHome(t)

	_, err := Load(KindCLI)
	if err == nil {
		t.Fatal("Load succeeded with no credential on disk")
	}
	if !errors.Is(err, ErrNotSignedIn) {
		t.Errorf("error = %v, want ErrNotSignedIn so a caller can tell it apart from a broken file", err)
	}
	path, _ := Path(KindCLI)
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error = %q, want it to name %s so the file can be found", err, path)
	}
	if !strings.Contains(err.Error(), "burrow auth login") {
		t.Errorf("error = %q, want it to name the command that fixes it", err)
	}
}

// TestCorruptCredentialNeverEchoesItsContents. A half-written or hand-edited file is a legitimate
// thing to hit, and the decoder's own error quotes the input it choked on — which here is a file
// whose largest field is a token. The message must carry the path and the remedy and nothing else.
func TestCorruptCredentialNeverEchoesItsContents(t *testing.T) {
	burrowHome(t)
	storePair(t)

	path, _ := Path(KindCLI)
	// A truncated write, the shape a crashed or interrupted writer would leave behind.
	if err := os.WriteFile(path, []byte(`{"token":"`+humanSecret), 0o600); err != nil {
		t.Fatalf("truncate the credential: %v", err)
	}

	_, err := Load(KindCLI)
	if err == nil {
		t.Fatal("Load accepted a truncated credential file")
	}
	if strings.Contains(err.Error(), humanSecret) {
		t.Errorf("the error quoted the file's contents, token and all: %q", err)
	}
	if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "burrow auth login") {
		t.Errorf("error = %q, want the path and the remedy", err)
	}
}

// TestCredentialInTheWrongPlaceIsRefused: the file records its own kind, so a credential copied or
// moved into the other directory is caught before it is spent rather than presented as the wrong
// identity.
func TestCredentialInTheWrongPlaceIsRefused(t *testing.T) {
	burrowHome(t)
	// The person's credential, written where the agent's belongs.
	if _, err := Store(KindAgent, Credential{Kind: KindCLI, CredentialID: "cred-human", Token: humanSecret}); err != nil {
		t.Fatalf("Store: %v", err)
	}

	_, err := Load(KindAgent)
	if err == nil {
		t.Fatal("burrow-agent accepted the person's credential out of its own directory")
	}
	if strings.Contains(err.Error(), humanSecret) {
		t.Errorf("the error carried the token: %q", err)
	}
	if !strings.Contains(err.Error(), "burrow-agent") || !strings.Contains(err.Error(), "burrow auth login") {
		t.Errorf("error = %q, want it to name which credential belongs there and how to replace it", err)
	}
}

// TestEmptyTokenIsRefused: a well-formed file with nothing in it would otherwise produce a 401 with
// no explanation, which is the failure this whole path exists to avoid.
func TestEmptyTokenIsRefused(t *testing.T) {
	burrowHome(t)
	if _, err := Store(KindCLI, Credential{Kind: KindCLI, CredentialID: "cred-human"}); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if _, err := Load(KindCLI); err == nil {
		t.Fatal("Load accepted a credential with no token")
	} else if !strings.Contains(err.Error(), "burrow auth login") {
		t.Errorf("error = %q, want the remedy", err)
	}
}

// TestStoredCredentialIsReadableOnlyByItsOwner. A token on disk under permissions anyone can read is
// a token anyone can have.
func TestStoredCredentialIsReadableOnlyByItsOwner(t *testing.T) {
	burrowHome(t)
	for _, kind := range []Kind{KindCLI, KindAgent} {
		path, err := Store(kind, Credential{Kind: kind, Token: "t"})
		if err != nil {
			t.Fatalf("Store(%s): %v", kind, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s is mode %#o, want 0600", path, perm)
		}
		dir, err := os.Stat(filepath.Dir(path))
		if err != nil {
			t.Fatalf("stat %s: %v", filepath.Dir(path), err)
		}
		if perm := dir.Mode().Perm(); perm != 0o700 {
			t.Errorf("%s is mode %#o, want 0700", filepath.Dir(path), perm)
		}
	}
}

// TestStoreOverAWorldReadableFileTightensIt covers the reason Store renames a fresh file into place
// instead of writing the destination: os.WriteFile applies its mode only when it CREATES a file, so
// a pre-existing 0644 file would keep holding a credential at 0644.
func TestStoreOverAWorldReadableFileTightensIt(t *testing.T) {
	burrowHome(t)
	path, _ := Path(KindCLI)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatalf("seed a world-readable file: %v", err)
	}

	if _, err := Store(KindCLI, Credential{Kind: KindCLI, Token: "t"}); err != nil {
		t.Fatalf("Store: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("%s is mode %#o after a rewrite, want 0600", path, perm)
	}
	if dir, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Fatalf("stat dir: %v", err)
	} else if perm := dir.Mode().Perm(); perm != 0o700 {
		t.Errorf("%s is mode %#o after a rewrite, want 0700", filepath.Dir(path), perm)
	}
}

// TestRejectedMessageNamesRevocationAndCarriesNoToken. A revoked credential is the likely cause of a
// 401 on a machine that was working yesterday, and saying so is the difference between a legible
// failure and a generic one.
func TestRejectedMessageNamesRevocationAndCarriesNoToken(t *testing.T) {
	msg := RejectedMessage(Credential{Kind: KindCLI, CredentialID: "cred-human", Token: humanSecret})
	for _, want := range []string{"revoked", "cred-human", "burrow auth login"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not mention %q", msg, want)
		}
	}
	if strings.Contains(msg, humanSecret) {
		t.Error("the refusal message carried the token")
	}

	// With no id recorded it still reads as a sentence rather than trailing off into empty brackets.
	if bare := RejectedMessage(Credential{Kind: KindCLI, Token: humanSecret}); strings.Contains(bare, "()") {
		t.Errorf("message = %q", bare)
	}
}
