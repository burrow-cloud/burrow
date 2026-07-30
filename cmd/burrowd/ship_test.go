// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/burrow-cloud/burrow/controlplane"
)

// These tests are the failure paths of ADR-0063 §7, exercised against a real HTTP endpoint with no
// cluster and no vendor: a store that refuses the write, a store that fails and then works, a store
// that never answers, and — the one that motivates reading the object back at all — a store that
// accepts the write and then cannot serve it.
//
// They assert on the two things burrowd actually consumes: whether the run reported success, and the
// record it left in the termination log. Everything else the shipper does is diagnostics.

const (
	shipTestKeyID  = "AKIAEXAMPLEKEYID"
	shipTestSecret = "wJalrXUtnFEMIexampleSECRETkey"
)

// fakeStore is an S3-compatible endpoint whose PUT and HEAD behaviour a test scripts.
type fakeStore struct {
	mu sync.Mutex
	// putStatuses is returned by successive PUTs; the last entry repeats. 0 means "accept".
	putStatuses []int
	// headStatus, when non-zero, overrides the HEAD response.
	headStatus int
	// headLength, when non-zero, is the length HEAD reports instead of the stored one.
	headLength int64
	puts       int
	heads      int
	body       []byte
	payloadSum string
}

func (f *fakeStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodPut:
		status := 0
		if f.puts < len(f.putStatuses) {
			status = f.putStatuses[f.puts]
		} else if n := len(f.putStatuses); n > 0 {
			status = f.putStatuses[n-1]
		}
		f.puts++
		if status != 0 {
			w.WriteHeader(status)
			// A vendor's error body: it echoes the request's identifiers, which is exactly why it must
			// not be carried into a Backup row.
			fmt.Fprintf(w, "<Error><Code>Denied</Code><AWSAccessKeyId>%s</AWSAccessKeyId></Error>", shipTestKeyID)
			return
		}
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(r.Body)
		f.body = buf.Bytes()
		f.payloadSum = r.Header.Get("X-Amz-Content-Sha256")
		w.WriteHeader(http.StatusOK)
	case http.MethodHead:
		f.heads++
		if f.headStatus != 0 {
			w.WriteHeader(f.headStatus)
			return
		}
		length := int64(len(f.body))
		if f.headLength != 0 {
			length = f.headLength
		}
		w.Header().Set("Content-Length", fmt.Sprint(length))
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// newShipTest wires a dump file and a scripted endpoint, and returns the config the shipper would be
// given by its Job.
func newShipTest(t *testing.T, store *fakeStore, dump []byte) shipConfig {
	t.Helper()
	srv := httptest.NewServer(store)
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	path := filepath.Join(dir, "web.dump")
	if err := os.WriteFile(path, dump, 0o600); err != nil {
		t.Fatalf("writing the dump: %v", err)
	}
	// Retries are real; the wait between them is not, or the tests would sleep through fourteen
	// seconds of backoff to assert on behaviour that has nothing to do with the wait.
	old := shipBackoff
	shipBackoff = time.Millisecond
	t.Cleanup(func() { shipBackoff = old })

	return shipConfig{
		Endpoint: srv.URL,
		Region:   "us-east-1",
		Bucket:   "burrow-backups-test",
		Key:      "burrow/backups/prod/web/bkp-1.dump",
		File:     path,
	}
}

func shipCred() controlplane.ObjectStoreCredential {
	return controlplane.ObjectStoreCredential{AccessKeyID: shipTestKeyID, SecretAccessKey: shipTestSecret}
}

// TestShipBackupWritesAndVerifies is the happy path, and it asserts the two facts that make a
// completed Backup row honest: the endpoint received the dump's own SHA-256 as the signed payload
// hash (so it validated the bytes it stored), and the object was read back afterwards.
func TestShipBackupWritesAndVerifies(t *testing.T) {
	dump := []byte("PGDMP fake custom-format dump")
	store := &fakeStore{}
	cfg := newShipTest(t, store, dump)

	res, ok := runShipBackup(context.Background(), cfg, shipCred(), new(bytes.Buffer))
	if !ok {
		t.Fatalf("shipping should succeed, got %+v", res)
	}
	if res.Size != int64(len(dump)) {
		t.Errorf("size = %d, want %d", res.Size, len(dump))
	}
	if !bytes.Equal(store.body, dump) {
		t.Error("the endpoint did not receive the dump's bytes")
	}
	sum := sha256.Sum256(dump)
	if store.payloadSum != hex.EncodeToString(sum[:]) {
		t.Errorf("x-amz-content-sha256 = %q, want the dump's own hash — without it the endpoint cannot reject a truncated transfer", store.payloadSum)
	}
	if store.heads != 1 {
		t.Errorf("HEAD count = %d, want 1: a completed backup is one that was read back", store.heads)
	}
}

// TestShipBackupRetriesATransientFailure is ADR-0063 §7's first clause: object storage is a network
// dependency and a transient failure is the common case, so a retried-and-succeeded backup is a
// success and is not reported as an incident.
func TestShipBackupRetriesATransientFailure(t *testing.T) {
	dump := []byte("PGDMP fake custom-format dump")
	store := &fakeStore{putStatuses: []int{http.StatusServiceUnavailable, http.StatusBadGateway, 0}}
	cfg := newShipTest(t, store, dump)

	var stderr bytes.Buffer
	res, ok := runShipBackup(context.Background(), cfg, shipCred(), &stderr)
	if !ok {
		t.Fatalf("a write that succeeded on the third attempt should succeed, got %+v", res)
	}
	if store.puts != 3 {
		t.Errorf("PUT count = %d, want 3", store.puts)
	}
	if res.Reason != "" {
		t.Errorf("a retried-and-succeeded backup reported reason %q; it is not an incident", res.Reason)
	}
	if !bytes.Equal(store.body, dump) {
		t.Error("the retry did not re-send the dump from the beginning")
	}
}

// TestShipBackupUnreachableStoreFailsLoudly asserts a destination that never completes the write is
// retried and then reported — with the reason that says it is worth retrying later, which is a
// different fix from a credential that will never work.
func TestShipBackupUnreachableStoreFailsLoudly(t *testing.T) {
	store := &fakeStore{putStatuses: []int{http.StatusServiceUnavailable}}
	cfg := newShipTest(t, store, []byte("dump"))

	res, ok := runShipBackup(context.Background(), cfg, shipCred(), new(bytes.Buffer))
	if ok {
		t.Fatal("a write that never completed must not report success")
	}
	if res.Reason != controlplane.BackupReasonStoreUnreachable {
		t.Errorf("reason = %q, want %q", res.Reason, controlplane.BackupReasonStoreUnreachable)
	}
	if store.puts != shipAttempts {
		t.Errorf("PUT count = %d, want %d attempts before giving up", store.puts, shipAttempts)
	}
	if res.Size != 0 {
		t.Errorf("size = %d on a failed ship, want 0", res.Size)
	}
}

// TestShipBackupRefusedWriteIsNotRetried asserts the other half of the retry policy: a destination
// that ANSWERS and says no is not retried. A revoked credential does not become a valid one by being
// asked again, and spending the budget on it only delays the loud failure.
func TestShipBackupRefusedWriteIsNotRetried(t *testing.T) {
	store := &fakeStore{putStatuses: []int{http.StatusForbidden}}
	cfg := newShipTest(t, store, []byte("dump"))

	res, ok := runShipBackup(context.Background(), cfg, shipCred(), new(bytes.Buffer))
	if ok {
		t.Fatal("a refused write must not report success")
	}
	if res.Reason != controlplane.BackupReasonStoreRejected {
		t.Errorf("reason = %q, want %q", res.Reason, controlplane.BackupReasonStoreRejected)
	}
	if store.puts != 1 {
		t.Errorf("PUT count = %d, want 1: a 403 is an answer, not a blip", store.puts)
	}
}

// TestShipBackupAcceptedButUnreadableFails is the test the read-back exists for: the endpoint took
// the write and returned 200, and then could not serve the object. Without the read-back this run
// would report success and burrowd would record a completed backup for an object that is not there
// — a false assurance, discovered at restore time.
func TestShipBackupAcceptedButUnreadableFails(t *testing.T) {
	store := &fakeStore{headStatus: http.StatusNotFound}
	cfg := newShipTest(t, store, []byte("dump"))

	res, ok := runShipBackup(context.Background(), cfg, shipCred(), new(bytes.Buffer))
	if ok {
		t.Fatal("an object the store cannot serve back must not be reported as a backup")
	}
	if res.Reason != controlplane.BackupReasonObjectNotReadable {
		t.Errorf("reason = %q, want %q", res.Reason, controlplane.BackupReasonObjectNotReadable)
	}
	if store.puts != 1 {
		t.Errorf("PUT count = %d, want 1: the write succeeded, so it is not retried", store.puts)
	}
}

// TestShipBackupWrongLengthFails asserts the read-back compares the LENGTH, not just presence: a
// store that serves back a truncated object has not stored the backup, however cleanly it answered.
func TestShipBackupWrongLengthFails(t *testing.T) {
	store := &fakeStore{headLength: 7}
	cfg := newShipTest(t, store, []byte("a considerably longer dump than seven bytes"))

	res, ok := runShipBackup(context.Background(), cfg, shipCred(), new(bytes.Buffer))
	if ok {
		t.Fatal("an object served back at the wrong length must not be reported as a backup")
	}
	if res.Reason != controlplane.BackupReasonObjectNotReadable {
		t.Errorf("reason = %q, want %q", res.Reason, controlplane.BackupReasonObjectNotReadable)
	}
}

// TestShipBackupEmptyDumpIsNotABackup asserts the step before this one leaving nothing behind is
// reported as the dump's failure and not the store's — and that the empty file is never written,
// since an object under a backup's key that cannot be restored is worse than no object.
func TestShipBackupEmptyDumpIsNotABackup(t *testing.T) {
	store := &fakeStore{}
	cfg := newShipTest(t, store, nil)

	res, ok := runShipBackup(context.Background(), cfg, shipCred(), new(bytes.Buffer))
	if ok {
		t.Fatal("an empty dump must not be reported as a backup")
	}
	if res.Reason != controlplane.BackupReasonDumpFailed {
		t.Errorf("reason = %q, want %q", res.Reason, controlplane.BackupReasonDumpFailed)
	}
	if store.puts != 0 {
		t.Errorf("PUT count = %d, want 0", store.puts)
	}
}

// TestShipTerminationRecordCarriesNoSecret asserts the record burrowd reads back — which is what
// ends up on the Backup row — carries the closed reason and a Burrow-authored detail, and never the
// vendor's response body or either half of the credential. The vendor's text goes to stderr, which
// becomes the pod log, and is deliberately not carried into the registry.
func TestShipTerminationRecordCarriesNoSecret(t *testing.T) {
	store := &fakeStore{putStatuses: []int{http.StatusForbidden}}
	cfg := newShipTest(t, store, []byte("dump"))

	var stderr bytes.Buffer
	res, _ := runShipBackup(context.Background(), cfg, shipCred(), &stderr)

	path := filepath.Join(t.TempDir(), "termination-log")
	old := terminationLogPath
	terminationLogPath = path
	t.Cleanup(func() { terminationLogPath = old })
	writeTerminationRecord(path, res)

	recorded, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the termination record: %v", err)
	}
	text := string(recorded)
	if !strings.Contains(text, "reason="+controlplane.BackupReasonStoreRejected) {
		t.Errorf("record %q does not carry the closed reason", text)
	}
	for _, secret := range []string{shipTestKeyID, shipTestSecret} {
		if strings.Contains(text, secret) {
			t.Fatal("the termination record carries a credential value; it is the thing that becomes a Backup row")
		}
	}
	if strings.Contains(text, "AWSAccessKeyId") {
		t.Error("the termination record carries the vendor's response body")
	}
	// The record is one key=value per line, so a detail containing a newline cannot break it.
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		if !strings.Contains(line, "=") {
			t.Errorf("record line %q is not key=value", line)
		}
	}
	// The diagnostic detail is still available — just in the pod log, not in the registry.
	if !strings.Contains(stderr.String(), "attempt 1/") {
		t.Errorf("stderr %q does not report the attempt", stderr.String())
	}
}

// TestShipTerminationRecordOnSuccess asserts a successful run reports the verified size and no
// reason, which is what lets burrowd record a completed row with a length it can trust.
func TestShipTerminationRecordOnSuccess(t *testing.T) {
	dump := []byte("PGDMP fake custom-format dump")
	store := &fakeStore{}
	cfg := newShipTest(t, store, dump)

	res, ok := runShipBackup(context.Background(), cfg, shipCred(), new(bytes.Buffer))
	if !ok {
		t.Fatalf("shipping should succeed, got %+v", res)
	}
	path := filepath.Join(t.TempDir(), "termination-log")
	writeTerminationRecord(path, res)
	recorded, _ := os.ReadFile(path)
	if got, want := strings.TrimSpace(string(recorded)), fmt.Sprintf("size=%d", len(dump)); got != want {
		t.Errorf("record = %q, want %q", got, want)
	}
}

// TestReadShipCredentialNamesTheFileNotTheValue asserts the one place a credential is read cannot
// leak it into an error, which is where a value ends up in a log without anybody meaning it to.
func TestReadShipCredentialNamesTheFileNotTheValue(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, objectStoreAccessKeyIDFile), []byte(shipTestKeyID), 0o600); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	// The secret half is missing: the error must name the file and stop there.
	_, err := readShipCredential(dir)
	if err == nil {
		t.Fatal("an incomplete credential should error")
	}
	if strings.Contains(err.Error(), shipTestKeyID) {
		t.Fatal("the error carries a credential value")
	}
	if !strings.Contains(err.Error(), objectStoreSecretAccessKeyFile) {
		t.Errorf("error %q does not name the missing file", err)
	}

	if err := os.WriteFile(filepath.Join(dir, objectStoreSecretAccessKeyFile), []byte(shipTestSecret+"\n"), 0o600); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	cred, err := readShipCredential(dir)
	if err != nil {
		t.Fatalf("readShipCredential: %v", err)
	}
	if cred.AccessKeyID != shipTestKeyID || cred.SecretAccessKey != shipTestSecret {
		t.Error("the credential did not round-trip (a trailing newline in a mounted Secret file is normal)")
	}
}

// TestShipConfigFromEnvNamesWhatIsMissing asserts a misconfigured Job fails with the variable's name
// rather than with a vendor error nobody can act on.
func TestShipConfigFromEnvNamesWhatIsMissing(t *testing.T) {
	t.Setenv("BURROW_SHIP_ENDPOINT", "https://s3.example.com")
	t.Setenv("BURROW_SHIP_BUCKET", "bucket")
	t.Setenv("BURROW_SHIP_KEY", "key")
	t.Setenv("BURROW_SHIP_FILE", "/backups/web/bkp-1.dump")
	t.Setenv("BURROW_SHIP_CREDENTIALS_DIR", "")

	_, err := shipConfigFromEnv()
	if err == nil || !strings.Contains(err.Error(), "BURROW_SHIP_CREDENTIALS_DIR") {
		t.Fatalf("error = %v, want it to name BURROW_SHIP_CREDENTIALS_DIR", err)
	}

	t.Setenv("BURROW_SHIP_CREDENTIALS_DIR", "/objectstore-creds")
	cfg, err := shipConfigFromEnv()
	if err != nil {
		t.Fatalf("shipConfigFromEnv: %v", err)
	}
	if cfg.Bucket != "bucket" || cfg.Key != "key" {
		t.Errorf("config = %+v", cfg)
	}
}
