// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

// Package credfile writes a credential to disk safely, and is the one place that decides what
// "safely" means for every credential Burrow stores locally.
//
// There is more than one kind now — the Burrow Cloud pair (cloud ADR-0028 §2) and the credential a
// self-hosted install issues at sign-in (ADR-0084 §1) — and the storage discipline is identical for
// all of them, so it lives here rather than being reimplemented per kind. Security-critical file
// handling written twice is security-critical file handling that drifts.
//
// The discipline: 0600 inside a 0700 directory, written to a fresh O_EXCL temporary and renamed into
// place. That gives three properties os.WriteFile does not. The token is never on disk under
// permissions somebody else set (WriteFile applies its mode only when it CREATES the file, so an
// existing 0644 file would stay 0644 while holding a credential). A symlink where the destination
// should be cannot redirect the token out of the directory. And a reader never sees a half-written
// file.
//
// NOTHING HERE PUTS A CREDENTIAL IN AN ERROR. An error message is the most likely place for a token
// to end up somewhere it was never meant to be, so every error below carries the PATH and never the
// contents.
package credfile

import (
	"fmt"
	"os"
	"path/filepath"
)

// Write puts data at path, 0600 under a 0700 directory, atomically. It creates the directory when it
// is missing and tightens it when it is not: MkdirAll only applies its mode on creation, so an
// existing directory is restricted explicitly rather than trusted.
func Write(path string, data []byte) error {
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
