// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

// These tests cover the DESTINATION-REGISTRATION half of ADR-0063: an object-storage provider on
// the existing credential registry, its credential pair in the one burrow-credentials Secret, a
// bucket Burrow named and recorded, a probe object written and deleted, and the reconciliation of
// bucket lifecycle against backup retention. The backup WRITE path (§7) is a separate change.

const (
	testEndpoint = "https://s3.us-west-002.example.com"
	testKeyID    = "AKIAEXAMPLEKEYID"
	testSecret   = "wJalrXUtnFEMIexampleSECRETkey"
)

// newObjectStoreEngine builds an engine with the object-store seam wired, returning the pieces a
// test asserts against: the credential store (which is the one burrow-credentials Secret), the
// object store, and the database.
func newObjectStoreEngine(t *testing.T) (*cp.Engine, *fake.Credentials, *fake.ObjectStoreFactory, *fake.Database) {
	t.Helper()
	d := fake.NewDatabase()
	creds := fake.NewCredentials()
	osf := fake.NewObjectStoreFactory()
	e, err := cp.New(cp.Deps{
		Kubernetes: fake.NewKubernetes(), Database: d,
		Clock: fake.NewClock(time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)),
		IDs:   fake.NewIDs(), Resolver: fake.NewResolver(),
		Credentials: creds, DNS: fake.NewDNSFactory(), ObjectStore: osf,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e, creds, osf, d
}

func s3Request() cp.AddProviderRequest {
	return cp.AddProviderRequest{
		Type:            cp.ProviderS3,
		Endpoint:        testEndpoint,
		Region:          "us-west-002",
		CreateBucket:    true,
		Confirm:         true,
		RetentionDays:   30,
		AccessKeyID:     testKeyID,
		SecretAccessKey: testSecret,
	}
}

// TestAddObjectStorageProviderRecordsPairAndDestination is the shape of ADR-0063 §1: object storage
// is a provider type on the EXISTING registry, its credential is a pair held as TWO KEYS in the one
// burrow-credentials Secret, and the row records the key names plus the non-secret endpoint, region
// and bucket.
func TestAddObjectStorageProviderRecordsPairAndDestination(t *testing.T) {
	e, creds, osf, db := newObjectStoreEngine(t)

	p, err := e.AddProvider(context.Background(), s3Request())
	if err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	if !p.Serves(cp.CapabilityObjectStorage) {
		t.Errorf("capabilities %v should include object-storage", p.Capabilities)
	}
	if p.ObjectStore == nil {
		t.Fatal("the provider row carries no object-storage configuration")
	}
	if p.ObjectStore.Endpoint != testEndpoint || p.ObjectStore.Region != "us-west-002" {
		t.Errorf("recorded destination = %s / %s, want the configured endpoint and region",
			p.ObjectStore.Endpoint, p.ObjectStore.Region)
	}

	// TWO keys, one Secret. The names are recorded in the row, and both values are in the same
	// credential store — no second Secret, which is what would have forced burrowd's
	// resourceNames-restricted grant wider (ADR-0063 §1).
	idKey, secretKey := p.ObjectStore.AccessKeyIDKey, p.ObjectStore.SecretAccessKeyKey
	if idKey == secretKey {
		t.Fatal("the pair must be two DISTINCT keys")
	}
	for name, want := range map[string]string{idKey: testKeyID, secretKey: testSecret} {
		got, ok := creds.Get(name)
		if !ok {
			t.Errorf("burrow-credentials has no key %q", name)
			continue
		}
		if got != want {
			t.Errorf("key %q holds the wrong half of the pair", name)
		}
	}
	if !strings.HasPrefix(idKey, p.Name+".") || !strings.HasPrefix(secretKey, p.Name+".") {
		t.Errorf("keys %q/%q are not namespaced under the provider name %q", idKey, secretKey, p.Name)
	}

	// The registry row is what a later consumer reads, so the destination must survive the save.
	saved, err := db.Provider(context.Background(), p.Name)
	if err != nil {
		t.Fatalf("Provider: %v", err)
	}
	if saved.ObjectStore == nil || saved.ObjectStore.Bucket != p.ObjectStore.Bucket {
		t.Errorf("the saved row lost the recorded bucket: %+v", saved.ObjectStore)
	}

	// The credential pair reaches the vendor call, and nothing but the pair does.
	if osf.Cred.AccessKeyID != testKeyID || osf.Cred.SecretAccessKey != testSecret {
		t.Errorf("the object store was built with the wrong credential")
	}
	if osf.Endpoint != testEndpoint {
		t.Errorf("the object store was built for endpoint %q, want %q", osf.Endpoint, testEndpoint)
	}
}

// TestCreatedBucketNameIsUniqueRecordedAndReadable is ADR-0063 §4. Bucket namespaces are GLOBAL per
// vendor, so a fixed name is both likely taken and — worse — guessable by someone who would claim
// it first. Burrow generates its own, records what it created, and never infers one.
func TestCreatedBucketNameIsUniqueRecordedAndReadable(t *testing.T) {
	e, _, osf, _ := newObjectStoreEngine(t)
	ctx := context.Background()

	first, err := e.AddProvider(ctx, s3Request())
	if err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	second := s3Request()
	second.Name = "backup-b"
	other, err := e.AddProvider(ctx, second)
	if err != nil {
		t.Fatalf("AddProvider (second): %v", err)
	}

	if first.ObjectStore.Bucket == other.ObjectStore.Bucket {
		t.Errorf("two registrations produced the same bucket name %q; a fixed name is unavailable "+
			"the moment anyone else has taken it, and a guessable one can be claimed first to deny "+
			"it to you", first.ObjectStore.Bucket)
	}
	for _, name := range []string{first.ObjectStore.Bucket, other.ObjectStore.Bucket} {
		if !strings.HasPrefix(name, "burrow-backups-") {
			t.Errorf("bucket %q has no readable prefix; a human listing buckets at the vendor cannot "+
				"tell what it is", name)
		}
		if name == "burrow-backups-" {
			t.Errorf("bucket %q carries no random component", name)
		}
	}
	// It created exactly the buckets it recorded, and it recorded exactly what it created.
	if want := []string{first.ObjectStore.Bucket, other.ObjectStore.Bucket}; !equalStrings(osf.Store.Created, want) {
		t.Errorf("created buckets = %v, want the two recorded names %v", osf.Store.Created, want)
	}
	if !first.ObjectStore.Created {
		t.Error("the row does not record that Burrow created this bucket")
	}
}

// TestProbeObjectIsWrittenAndDeleted is ADR-0063 §2: configuration time is when a bad key or a
// typo'd endpoint must fail, so registration writes an object and deletes it again. It leaves
// nothing behind.
func TestProbeObjectIsWrittenAndDeleted(t *testing.T) {
	e, _, osf, _ := newObjectStoreEngine(t)

	p, err := e.AddProvider(context.Background(), s3Request())
	if err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	if len(osf.Store.Wrote) != 1 || len(osf.Store.Deleted) != 1 {
		t.Fatalf("probe wrote %v and deleted %v, want exactly one of each", osf.Store.Wrote, osf.Store.Deleted)
	}
	if osf.Store.Wrote[0] != osf.Store.Deleted[0] {
		t.Errorf("the probe deleted %q but wrote %q", osf.Store.Deleted[0], osf.Store.Wrote[0])
	}
	if objs := osf.Store.Objects(p.ObjectStore.Bucket); len(objs) != 0 {
		t.Errorf("the probe left %v in the bucket", objs)
	}
	if p.Verification == nil || !p.Verification.ProbeObject {
		t.Errorf("the registration does not report that it probed the destination: %+v", p.Verification)
	}
}

// TestProbeFailureRefusesRegistration is the point of the probe: a credential that cannot write is
// rejected NOW, loudly, and nothing is recorded — not the credential, not the row. A registration
// that stored a broken credential would fail at the first scheduled backup instead, silently.
func TestProbeFailureRefusesRegistration(t *testing.T) {
	e, creds, osf, db := newObjectStoreEngine(t)
	osf.Store.PutErr = errors.New("403 SignatureDoesNotMatch")

	_, err := e.AddProvider(context.Background(), s3Request())
	if err == nil {
		t.Fatal("AddProvider succeeded with a credential that cannot write to the destination")
	}
	if !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("error = %v, want ErrInvalid (the caller must fix the request)", err)
	}
	if strings.Contains(err.Error(), testSecret) {
		t.Error("the error leaked the secret access key")
	}
	if _, ok := creds.Get("s3.access-key-id"); ok {
		t.Error("a rejected destination still wrote a credential into burrow-credentials")
	}
	if _, err := db.Provider(context.Background(), "s3"); !errors.Is(err, cp.ErrNotFound) {
		t.Error("a rejected destination was still recorded in the registry")
	}
}

// TestProbeDeleteFailureRefusesRegistration covers the other half of the probe: a credential that
// can write but cannot delete would fill the bucket with objects nothing can clean up.
func TestProbeDeleteFailureRefusesRegistration(t *testing.T) {
	e, creds, osf, _ := newObjectStoreEngine(t)
	osf.Store.DeleteErr = errors.New("403 AccessDenied")

	_, err := e.AddProvider(context.Background(), s3Request())
	if err == nil || !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("AddProvider error = %v, want ErrInvalid", err)
	}
	if _, ok := creds.Get("s3.secret-access-key"); ok {
		t.Error("a credential that cannot delete what it writes was still stored")
	}
}

// TestExistingBucketMustBePresent is the second half of ADR-0063 §4: pointing Burrow at an existing
// bucket is supported, but inferring one is not — an absent or unreachable bucket is refused rather
// than assumed into existence.
func TestExistingBucketMustBePresent(t *testing.T) {
	e, _, osf, _ := newObjectStoreEngine(t)
	ctx := context.Background()

	req := s3Request()
	req.CreateBucket, req.Confirm = false, false
	req.Bucket = "my-existing-backups"

	if _, err := e.AddProvider(ctx, req); !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("AddProvider with an absent bucket: error = %v, want ErrInvalid", err)
	}

	osf.Store.AddBucket("my-existing-backups")
	p, err := e.AddProvider(ctx, req)
	if err != nil {
		t.Fatalf("AddProvider with a present bucket: %v", err)
	}
	if p.ObjectStore.Bucket != "my-existing-backups" || p.ObjectStore.Created {
		t.Errorf("recorded %+v, want the named existing bucket, not created", p.ObjectStore)
	}
	if len(osf.Store.Created) != 0 {
		t.Errorf("pointing at an existing bucket created %v", osf.Store.Created)
	}
}

// TestBucketCreationIsConfirmGuarded is ADR-0063 §5's tier-3 half: creating a bucket is additive and
// reversible, but it is a billable resource at a third party, so it is held for confirmation rather
// than performed silently.
func TestBucketCreationIsConfirmGuarded(t *testing.T) {
	e, creds, osf, _ := newObjectStoreEngine(t)

	req := s3Request()
	req.Confirm = false
	_, err := e.AddProvider(context.Background(), req)
	g, ok := cp.AsGuardrail(err)
	if !ok {
		t.Fatalf("AddProvider error = %v, want a guardrail outcome", err)
	}
	if g.Code != cp.GuardrailBucketCreate || !g.NeedsConfirmation {
		t.Errorf("guardrail = %s (needs confirmation: %v), want bucket.create held for confirmation",
			g.Code, g.NeedsConfirmation)
	}
	if len(osf.Store.Created) != 0 {
		t.Errorf("a held creation still created %v", osf.Store.Created)
	}
	if _, ok := creds.Get("s3.access-key-id"); ok {
		t.Error("a held creation still wrote a credential")
	}
}

// TestLifecycleConflictIsRefusedNamingRuleAndBackup is ADR-0063 §3, the invariant that justifies the
// feature existing. A rule that expires objects sooner than a retained backup needs them leaves a
// backup set that lists fine and cannot be restored — discovered during recovery, which is the worst
// possible moment. Configuration time is the only point where refusing is cheap.
func TestLifecycleConflictIsRefusedNamingRuleAndBackup(t *testing.T) {
	e, creds, osf, db := newObjectStoreEngine(t)
	ctx := context.Background()

	if err := db.RecordBackup(ctx, cp.Backup{
		ID: "bkp-old", App: "web", Status: cp.BackupCompleted,
		CreatedAt: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), // ~85 days before the fake clock
	}); err != nil {
		t.Fatalf("RecordBackup: %v", err)
	}
	osf.Store.AddBucket("existing")
	osf.Store.SetLifecycle("existing", cp.LifecycleRule{ID: "expire-7d", Enabled: true, ExpireAfterDays: 7})

	req := s3Request()
	req.CreateBucket, req.Confirm = false, false
	req.Bucket = "existing"
	_, err := e.AddProvider(ctx, req)
	if err == nil {
		t.Fatal("AddProvider accepted a bucket whose lifecycle rule expires a retained backup")
	}
	if !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("error = %v, want ErrInvalid", err)
	}
	for _, want := range []string{"expire-7d", "bkp-old", "7 days"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q, so the operator cannot act on it:\n%v", want, err)
		}
	}
	if _, ok := creds.Get("s3.access-key-id"); ok {
		t.Error("a refused destination still wrote a credential")
	}
}

// TestLifecycleWithinRetentionIsAccepted: a rule that outlives the declared window expires nothing
// that is still needed, so it is not a conflict.
func TestLifecycleWithinRetentionIsAccepted(t *testing.T) {
	e, _, osf, _ := newObjectStoreEngine(t)
	osf.Store.AddBucket("existing")
	osf.Store.SetLifecycle("existing",
		cp.LifecycleRule{ID: "expire-90d", Enabled: true, ExpireAfterDays: 90},
		cp.LifecycleRule{ID: "disabled-1d", Enabled: false, ExpireAfterDays: 1},
		cp.LifecycleRule{ID: "abort-multipart", Enabled: true},
	)

	req := s3Request()
	req.CreateBucket, req.Confirm = false, false
	req.Bucket = "existing"
	req.RetentionDays = 30
	p, err := e.AddProvider(context.Background(), req)
	if err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	if p.Verification.Lifecycle.Status != cp.LifecycleOK {
		t.Errorf("lifecycle status = %s (%s), want ok",
			p.Verification.Lifecycle.Status, p.Verification.Lifecycle.Detail)
	}
}

// TestUnreadableLifecycleIsReportedUnknownNotVerified is the honesty clause of ADR-0063 §3. Where
// Burrow cannot read the configuration — the vendor does not serve the API, or the credential may
// not read it — it says so. An unverifiable invariant reported as verified is worse than one
// reported as unknown, because the operator stops looking.
func TestUnreadableLifecycleIsReportedUnknownNotVerified(t *testing.T) {
	e, _, osf, _ := newObjectStoreEngine(t)
	osf.Store.LifecycleErr = fmt.Errorf("this endpoint does not serve the bucket lifecycle API (http 501): %w", cp.ErrLifecycleUnknown)

	p, err := e.AddProvider(context.Background(), s3Request())
	if err != nil {
		t.Fatalf("AddProvider: an unreadable lifecycle configuration must not fail the registration: %v", err)
	}
	check := p.Verification.Lifecycle
	if check.Status != cp.LifecycleUnknown {
		t.Fatalf("lifecycle status = %q, want unknown", check.Status)
	}
	if !strings.Contains(strings.ToUpper(check.Detail), "UNKNOWN") {
		t.Errorf("the detail does not say the check is unknown, so it reads as verified: %q", check.Detail)
	}
	if check.Rule != "" || check.Backup != "" {
		t.Errorf("an unknown check named a rule or backup it never read: %+v", check)
	}
}

// TestLifecycleErrorThatIsNotUnknownFails: a genuine failure reading the configuration is a failure,
// not a shrug. Only the two "cannot answer" cases degrade to unknown.
func TestLifecycleErrorThatIsNotUnknownFails(t *testing.T) {
	e, _, osf, _ := newObjectStoreEngine(t)
	osf.Store.LifecycleErr = errors.New("connection reset by peer")

	if _, err := e.AddProvider(context.Background(), s3Request()); err == nil {
		t.Fatal("AddProvider succeeded despite failing to read the lifecycle configuration")
	}
}

// TestObjectStorageRequestValidation rejects malformed registrations before any vendor call and
// before any credential is written.
func TestObjectStorageRequestValidation(t *testing.T) {
	cases := map[string]func(r *cp.AddProviderRequest){
		"no endpoint":            func(r *cp.AddProviderRequest) { r.Endpoint = "" },
		"endpoint is not a URL":  func(r *cp.AddProviderRequest) { r.Endpoint = "s3.example.com" },
		"no access key id":       func(r *cp.AddProviderRequest) { r.AccessKeyID = "" },
		"no secret access key":   func(r *cp.AddProviderRequest) { r.SecretAccessKey = "" },
		"bucket and create both": func(r *cp.AddProviderRequest) { r.Bucket = "b" },
		"neither bucket nor create": func(r *cp.AddProviderRequest) {
			r.CreateBucket = false
		},
		"invalid bucket name": func(r *cp.AddProviderRequest) {
			r.CreateBucket, r.Bucket = false, "Not A Bucket"
		},
		"negative retention": func(r *cp.AddProviderRequest) { r.RetentionDays = -1 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			e, creds, osf, _ := newObjectStoreEngine(t)
			req := s3Request()
			mutate(&req)
			if _, err := e.AddProvider(context.Background(), req); !errors.Is(err, cp.ErrInvalid) {
				t.Fatalf("error = %v, want ErrInvalid", err)
			}
			if len(osf.Store.Created) != 0 || len(osf.Store.Wrote) != 0 {
				t.Error("a malformed request still reached the vendor")
			}
			if _, ok := creds.Get("s3.access-key-id"); ok {
				t.Error("a malformed request still wrote a credential")
			}
		})
	}
}

// TestObjectStorageWithoutSeamIsNotImplemented: the seam is optional, and a build without it says
// so cleanly rather than panicking (ADR-0009).
func TestObjectStorageWithoutSeamIsNotImplemented(t *testing.T) {
	e, _, _, _, _ := newProviderEngine(t) // no ObjectStore seam
	if _, err := e.AddProvider(context.Background(), s3Request()); !errors.Is(err, cp.ErrNotImplemented) {
		t.Fatalf("error = %v, want ErrNotImplemented", err)
	}
}

// TestObjectStoreCredentialForReadsBothKeys: a consumer reads the pair back from the one Secret at
// call time, so a rotated key is picked up with no restart (ADR-0023).
func TestObjectStoreCredentialForReadsBothKeys(t *testing.T) {
	e, creds, _, _ := newObjectStoreEngine(t)
	ctx := context.Background()

	p, err := e.AddProvider(ctx, s3Request())
	if err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	got, err := e.ObjectStoreCredentialFor(ctx, p)
	if err != nil {
		t.Fatalf("ObjectStoreCredentialFor: %v", err)
	}
	if got.AccessKeyID != testKeyID || got.SecretAccessKey != testSecret {
		t.Error("the pair did not round-trip through burrow-credentials")
	}

	creds.Set(p.ObjectStore.SecretAccessKeyKey, "rotated")
	got, err = e.ObjectStoreCredentialFor(ctx, p)
	if err != nil || got.SecretAccessKey != "rotated" {
		t.Errorf("a rotated key was not picked up: %v / %v", got.SecretAccessKey, err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
