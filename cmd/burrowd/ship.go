// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/objectstore"
)

// `burrowd ship-backup` is the second half of a backup with a durable destination: it runs INSIDE
// the backup Job's pod, reads the dump the pg_dump step wrote to the backup volume, writes it to the
// object store, and reads it back (ADR-0063 §7).
//
// It is a subcommand of burrowd rather than a separate binary or a shell script because the S3
// client, the SigV4 signer, and the error taxonomy already exist in this module. Running the same
// binary means a destination verified by `burrow config provider add` is written to by the same code
// that verified it — there is no second implementation to drift, and no vendor CLI or hand-rolled
// shell signing to get subtly wrong on the one path whose failure is discovered at restore time.
//
// The exit code says whether the backup reached the store, and NOTHING ELSE decides that: burrowd
// reads the Job's terminal state and records a completed Backup row only when this process exited
// zero. A row saying "succeeded" for bytes that never left the cluster is worse than no row at all,
// so every path here that is not a verified object is a non-zero exit.

const (
	// shipAttempts is how many times the write is tried before the backup is called failed. Object
	// storage is a network dependency and a transient failure is the common case (ADR-0063 §7), so a
	// backup that succeeds on the second attempt is not an incident and must not be reported as one.
	// Four is enough to ride out a vendor's brief 5xx or a reset connection; more would mostly delay
	// the loud failure that a genuinely unreachable destination deserves.
	shipAttempts = 4
	// shipTimeout caps a single attempt's transfer. A dump is large and a slow link is not a
	// failure, so this is generous; it exists so a connection that hangs open forever cannot consume
	// the whole Job deadline in one attempt and leave no budget to retry.
	shipTimeout = 30 * time.Minute
)

// shipBackoff is the wait before the second attempt; it doubles each time (2s, 4s, 8s), so the whole
// retry window is about fourteen seconds plus the transfers. It is bounded well inside the Job's own
// deadline on purpose: the retry is for a blip, and a destination that is down for longer than this
// is a thing to alert on rather than to keep a Job waiting for. It is a var so the tests that
// exercise the retry can shorten it without sleeping through the real one.
var shipBackoff = 2 * time.Second

// shipConfig is everything the shipping step needs, read from the environment the Job set and the
// Secret it mounted. The connection details are configuration and travel as env; the credential PAIR
// is a secret value and travels only as files in a mounted Secret volume, because a Job's env is
// readable by anything that can read Jobs in the namespace.
type shipConfig struct {
	Endpoint string
	Region   string
	Bucket   string
	Key      string
	File     string
	CredsDir string
}

// shipConfigFromEnv reads the shipping configuration, rejecting a missing required value by name.
func shipConfigFromEnv() (shipConfig, error) {
	cfg := shipConfig{
		Endpoint: os.Getenv("BURROW_SHIP_ENDPOINT"),
		Region:   os.Getenv("BURROW_SHIP_REGION"),
		Bucket:   os.Getenv("BURROW_SHIP_BUCKET"),
		Key:      os.Getenv("BURROW_SHIP_KEY"),
		File:     os.Getenv("BURROW_SHIP_FILE"),
		CredsDir: os.Getenv("BURROW_SHIP_CREDENTIALS_DIR"),
	}
	for name, value := range map[string]string{
		"BURROW_SHIP_ENDPOINT":        cfg.Endpoint,
		"BURROW_SHIP_BUCKET":          cfg.Bucket,
		"BURROW_SHIP_KEY":             cfg.Key,
		"BURROW_SHIP_FILE":            cfg.File,
		"BURROW_SHIP_CREDENTIALS_DIR": cfg.CredsDir,
	} {
		if strings.TrimSpace(value) == "" {
			return shipConfig{}, fmt.Errorf("%s is required", name)
		}
	}
	return cfg, nil
}

// readShipCredential reads the credential pair from the mounted Secret. The values are held in
// memory and handed to the signer; they are never logged, never written to the termination log, and
// never placed in an error — a read failure names the FILE, never its contents.
func readShipCredential(dir string) (controlplane.ObjectStoreCredential, error) {
	read := func(name string) (string, error) {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return "", fmt.Errorf("reading the destination credential file %q: %w", name, errWithoutValue(err))
		}
		return strings.TrimSpace(string(b)), nil
	}
	id, err := read(objectStoreAccessKeyIDFile)
	if err != nil {
		return controlplane.ObjectStoreCredential{}, err
	}
	secret, err := read(objectStoreSecretAccessKeyFile)
	if err != nil {
		return controlplane.ObjectStoreCredential{}, err
	}
	if id == "" || secret == "" {
		return controlplane.ObjectStoreCredential{}, errors.New("the mounted destination credential is incomplete")
	}
	return controlplane.ObjectStoreCredential{AccessKeyID: id, SecretAccessKey: secret}, nil
}

// The credential filenames the backup Job mounts. They are duplicated from controlplane/kube rather
// than imported because they are the Job's wire format between two processes, and a shipper built
// from a different burrowd version must keep reading the files the Job that launched it wrote.
const (
	objectStoreAccessKeyIDFile     = "access-key-id"
	objectStoreSecretAccessKeyFile = "secret-access-key"
)

// errWithoutValue strips a file's contents out of an os error, keeping the path and the cause. It
// exists so the "no secret in an error" rule is enforced at the one place a credential file is read
// rather than relied upon.
func errWithoutValue(err error) error {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return fmt.Errorf("%s: %v", pathErr.Op, pathErr.Err)
	}
	return err
}

// shipResult is what the run reports back to burrowd through /dev/termination-log: the verified size
// on success, or the closed reason and a Burrow-authored detail on failure.
type shipResult struct {
	Size   int64
	Reason string
	Detail string
}

// runShipBackup is the subcommand's body, split out so it is testable against an httptest endpoint
// with no cluster: it returns the result to report and whether the backup reached the store.
func runShipBackup(ctx context.Context, cfg shipConfig, cred controlplane.ObjectStoreCredential, stderr io.Writer) (shipResult, bool) {
	factory := objectstore.NewFactory()
	store, err := factory.ObjectStore(cfg.Endpoint, cfg.Region, cred)
	if err != nil {
		return shipResult{Reason: controlplane.BackupReasonStoreRejected, Detail: "the destination endpoint is not usable"}, false
	}

	// The dump is opened ONCE and rewound between attempts, rather than reopened: a retry must start
	// from the beginning of the body, and a file that vanished between attempts would otherwise be
	// reported as the store's failure instead of the dump's.
	dump, err := os.Open(cfg.File)
	if err != nil {
		fmt.Fprintf(stderr, "ship-backup: opening the dump: %v\n", errWithoutValue(err))
		return shipResult{Reason: controlplane.BackupReasonDumpFailed, Detail: "the dump could not be opened on the backup volume"}, false
	}
	defer dump.Close()

	size, sum, err := hashFile(dump)
	if err != nil {
		// The dump is not readable, which means the step before this one did not leave what it said
		// it did. That is the dump's failure, not the store's, and reporting it as the store's would
		// send an operator to the wrong place.
		fmt.Fprintf(stderr, "ship-backup: reading the dump: %v\n", err)
		return shipResult{Reason: controlplane.BackupReasonDumpFailed, Detail: "the dump could not be read from the backup volume"}, false
	}

	for attempt := 1; attempt <= shipAttempts; attempt++ {
		refused, err := putOnce(ctx, store, cfg, dump, size, sum)
		if err == nil {
			break
		}
		// The vendor's own error text goes to the pod log, which is the operator's to read and is no
		// wider an exposure than the credential this pod already mounts. It is deliberately NOT
		// carried into the Backup row: a vendor's error body is the one place an access key id is
		// known to be echoed back, and the registry is not a place a credential may reach.
		fmt.Fprintf(stderr, "ship-backup: attempt %d/%d: %v\n", attempt, shipAttempts, err)
		if refused {
			// The endpoint answered and said no. Asking again produces the same answer, so fail now
			// and say which kind of failure it was, rather than spending the budget proving it.
			return shipResult{
				Reason: controlplane.BackupReasonStoreRejected,
				Detail: fmt.Sprintf("the destination refused the write on attempt %d of %d; the Job's pod log has the vendor's response", attempt, shipAttempts),
			}, false
		}
		if attempt == shipAttempts {
			return shipResult{
				Reason: controlplane.BackupReasonStoreUnreachable,
				Detail: fmt.Sprintf("the destination did not complete the write after %d attempts; the Job's pod log has each attempt's error", shipAttempts),
			}, false
		}
		select {
		case <-ctx.Done():
			return shipResult{
				Reason: controlplane.BackupReasonStoreUnreachable,
				Detail: fmt.Sprintf("the backup was cancelled while retrying the write (attempt %d of %d)", attempt, shipAttempts),
			}, false
		case <-time.After(shipBackoff << (attempt - 1)):
		}
	}
	// The write completed. That is the endpoint's word that it accepted the request, and it is NOT
	// yet the fact the Backup row is about to assert. Read the object back before claiming it.
	stored, err := store.StatObject(ctx, cfg.Bucket, cfg.Key)
	if err != nil {
		fmt.Fprintf(stderr, "ship-backup: reading the object back: %v\n", err)
		detail := "the destination accepted the write and then could not serve the object back"
		if errors.Is(err, controlplane.ErrNotFound) {
			detail = "the destination accepted the write and then reported the object as absent"
		}
		return shipResult{Reason: controlplane.BackupReasonObjectNotReadable, Detail: detail}, false
	}
	if stored != 0 && stored != size {
		return shipResult{
			Reason: controlplane.BackupReasonObjectNotReadable,
			Detail: fmt.Sprintf("the destination served the object back at %d bytes, not the %d that were written", stored, size),
		}, false
	}
	return shipResult{Size: size}, true
}

// putOnce is one attempt: rewind the dump, stream it, and report whether a failure was a refusal.
// The rewind is what makes a retry send the whole body again rather than the tail of it.
func putOnce(ctx context.Context, store controlplane.ObjectStore, cfg shipConfig, dump *os.File, size int64, sum string) (bool, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, shipTimeout)
	defer cancel()
	// Rewind before every attempt: a retry must send the whole body again, not the tail of what the
	// last one managed. The seam does not close this handle (see PutObjectStream), so the same file
	// serves every attempt.
	if _, err := dump.Seek(0, io.SeekStart); err != nil {
		// The dump became unreadable mid-run; that is not the store refusing anything, and retrying
		// the same handle will not fix it.
		return true, fmt.Errorf("rewinding the dump: %w", errWithoutValue(err))
	}
	return store.PutObjectStream(attemptCtx, cfg.Bucket, cfg.Key, dump, size, sum)
}

// hashFile returns the file's length and the hex SHA-256 of its contents — the pass over the dump
// that lets the write be signed with its own payload hash, so the endpoint validates the bytes it
// received against the bytes that were sent and refuses a truncated transfer rather than storing a
// backup that will not restore.
func hashFile(f *os.File) (int64, string, error) {
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, "", err
	}
	if n == 0 {
		// An empty dump is not a backup. pg_dump -Fc always writes a header, so zero bytes means the
		// dump step produced nothing, and shipping it would put an unrestorable object in the store
		// under a key a Backup row is about to call a backup.
		return 0, "", errors.New("the dump is empty")
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// writeTerminationRecord writes the small key=value record burrowd reads off the pod's terminated
// state. Only Burrow-authored text goes here: a size, a closed reason, and a detail this file wrote.
// Best-effort — an unwritable termination log must not change whether the backup succeeded.
func writeTerminationRecord(path string, res shipResult) {
	var b strings.Builder
	if res.Size > 0 {
		fmt.Fprintf(&b, "size=%d\n", res.Size)
	}
	if res.Reason != "" {
		fmt.Fprintf(&b, "reason=%s\ndetail=%s\n", res.Reason, oneLine(res.Detail))
	}
	_ = os.WriteFile(path, []byte(b.String()), 0o644)
}

// oneLine keeps a detail on a single line, so the key=value record cannot be broken by a detail that
// happened to contain a newline.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// shipBackup is the subcommand entry point: read the configuration and the credential, ship, report
// through the termination log, and exit non-zero if the object is not verifiably in the store.
func shipBackup(ctx context.Context, stderr io.Writer) error {
	cfg, err := shipConfigFromEnv()
	if err != nil {
		return err
	}
	cred, err := readShipCredential(cfg.CredsDir)
	if err != nil {
		writeTerminationRecord(terminationLogPath, shipResult{
			Reason: controlplane.BackupReasonStoreRejected,
			Detail: "the destination credential could not be read from its mounted Secret",
		})
		return err
	}
	res, ok := runShipBackup(ctx, cfg, cred, stderr)
	writeTerminationRecord(terminationLogPath, res)
	if !ok {
		return fmt.Errorf("the backup did not reach %s: %s", cfg.Bucket, res.Detail)
	}
	return nil
}

// terminationLogPath is where Kubernetes reads a container's termination message from. It is a var
// so a test can point it at a temporary file and assert what a run reports back to burrowd — the
// record IS the interface between the two processes, so it is worth pinning.
var terminationLogPath = "/dev/termination-log"
