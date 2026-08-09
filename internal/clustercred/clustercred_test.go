// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package clustercred

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// theToken is the value that must never appear in an error message. It is distinctive so a test can
// assert its absence rather than hoping.
const theToken = "s3cret-token-value-nobody-should-print"

// isolate points $BURROW_CONFIG at a temporary tree, so a test writes and reads its own credentials
// and never the developer's. The credential directory is a sibling of the config file, so naming the
// config file is enough to move the whole of a person's Burrow state.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("BURROW_CONFIG", filepath.Join(home, "config"))
	return home
}

// TestStoreAndLoadRoundTrip: what was written is what comes back, field for field.
func TestStoreAndLoadRoundTrip(t *testing.T) {
	isolate(t)
	want := Credential{
		InstallID: "install-abc", PrincipalID: "p-1", Principal: "ada",
		CredentialID: "c-1", Kind: "user", Token: theToken,
	}

	path, err := Store(want)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	got, err := Load("install-abc")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != want {
		t.Errorf("loaded %+v, want %+v", got, want)
	}
	if !strings.HasSuffix(path, "cluster-install-abc.json") {
		t.Errorf("path = %q, want it named for the install that issued the credential", path)
	}
}

// TestTheCredentialIsOwnerOnlyUnderAnOwnerOnlyDirectory: a token on disk under permissions somebody
// else can read is the whole exposure this file format accepts, so the mode is asserted rather than
// assumed. It is the property internal/credfile exists to hold.
func TestTheCredentialIsOwnerOnlyUnderAnOwnerOnlyDirectory(t *testing.T) {
	isolate(t)
	path, err := Store(Credential{InstallID: "install-abc", Token: theToken})
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("credential mode = %o, want 600", perm)
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("directory mode = %o, want 700", perm)
	}
}

// TestStoreTightensADirectorySomebodyElseCreated: MkdirAll applies its mode only when it CREATES the
// directory, so a credentials directory that already exists world-readable would keep holding a
// token under permissions nobody chose.
func TestStoreTightensADirectorySomebodyElseCreated(t *testing.T) {
	isolate(t)
	dir, err := Dir()
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if _, err := Store(Credential{InstallID: "install-abc", Token: theToken}); err != nil {
		t.Fatalf("Store: %v", err)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("directory mode = %o, want 700 — an existing directory is tightened, not trusted", perm)
	}
}

// TestLoadWithNoCredentialIsRecognisable: the ordinary state of every install today is that nobody
// has signed in to it. A caller has to be able to tell that from a credential that exists and is
// broken, because the first falls back to the shared install token and the second does not.
func TestLoadWithNoCredentialIsRecognisable(t *testing.T) {
	isolate(t)

	_, err := Load("install-abc")
	if !errors.Is(err, ErrNoCredential) {
		t.Fatalf("Load = %v, want ErrNoCredential", err)
	}
	if !strings.Contains(err.Error(), "burrow auth login") {
		t.Errorf("error %q should name the one command that fixes it", err.Error())
	}
}

// TestACredentialFiledUnderTheWrongInstallIsRefused: the file says which install issued it as well as
// being named for one, so a credential moved or copied between installs is caught rather than spent
// against a Burrow that never issued it.
func TestACredentialFiledUnderTheWrongInstallIsRefused(t *testing.T) {
	home := isolate(t)
	path := filepath.Join(home, "credentials", "cluster-install-abc.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"installId":"install-other","token":"`+theToken+`"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := Load("install-abc")
	if err == nil {
		t.Fatal("Load accepted a credential issued by a different install")
	}
	if strings.Contains(err.Error(), theToken) {
		t.Errorf("the error carries the token: %q", err.Error())
	}
}

// TestNoErrorCarriesTheToken across the failures a broken file produces. An error message is the
// likeliest place for a credential to end up somewhere it was never meant to be, and the decoder's
// own error is the sharp case: it quotes the byte it choked on, and that byte can be part of a token.
func TestNoErrorCarriesTheToken(t *testing.T) {
	home := isolate(t)
	path := filepath.Join(home, "credentials", "cluster-install-abc.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Valid JSON up to the token, then truncated: the decoder's error quotes what it was reading.
	if err := os.WriteFile(path, []byte(`{"installId":"install-abc","token":"`+theToken), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := Load("install-abc")
	if err == nil {
		t.Fatal("Load accepted a truncated file")
	}
	if strings.Contains(err.Error(), theToken) {
		t.Errorf("the error carries the token: %q", err.Error())
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q should name the path, which is the actionable part", err.Error())
	}
}

// TestAnInstallIDCannotNameAFileElsewhere: the install id arrives from a control plane over the
// network and is then used to build a path. A burrowd that answered with a traversal must not be
// able to choose where a token is written or which file is read as one.
func TestAnInstallIDCannotNameAFileElsewhere(t *testing.T) {
	isolate(t)
	for _, id := range []string{"../../.ssh/id_rsa", "a/b", `a\b`, "..", " ", ""} {
		if _, err := Path(id); err == nil {
			t.Errorf("Path(%q) was accepted; a control-plane-supplied id must not escape the credential directory", id)
		}
	}
}

// TestTokenFallsBackToNothing: every failure reading a credential yields an empty token, because of
// where Token is called from — a caller about to connect, whose fallback is the install's shared
// token. An install nobody has signed in to must connect exactly as it did (ADR-0084 "Existing
// installs keep working"), not refuse.
func TestTokenFallsBackToNothing(t *testing.T) {
	isolate(t)

	if got := Token(""); got != "" {
		t.Errorf("Token(\"\") = %q, want empty: a target with no install id has no credential to find", got)
	}
	if got := Token("install-nobody-signed-in-to"); got != "" {
		t.Errorf("Token(absent) = %q, want empty", got)
	}
	if _, err := Store(Credential{InstallID: "install-abc", Token: theToken}); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if got := Token("install-abc"); got != theToken {
		t.Errorf("Token(stored) did not return the stored token")
	}
}
