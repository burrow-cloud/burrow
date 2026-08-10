// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ErrLifecycleUnknown marks a bucket lifecycle configuration Burrow could not READ — the vendor
// does not implement the lifecycle API, or the credential is not permitted to read it. It is not a
// failure: the destination still works, and registration proceeds. What it changes is what Burrow
// is allowed to CLAIM. ADR-0063 §3 is explicit that an unverifiable invariant reported as verified
// is worse than one reported as unknown, so the reconciliation is reported as unknown, with the
// reason, and never as "checked".
var ErrLifecycleUnknown = errors.New("bucket lifecycle configuration could not be read")

// bucketPrefix is the readable part of a bucket Burrow creates (ADR-0063 §4). A human listing
// buckets at the vendor can tell what it is; the random component after it is what makes the name
// unique in a GLOBAL namespace and unguessable to someone who would otherwise claim it first.
const bucketPrefix = "burrow-backups-"

// probeKeyPrefix is where the configuration-time probe object is written (ADR-0063 §2). It is
// written and then deleted, so nothing of Burrow's survives the check but the verdict.
const probeKeyPrefix = ".burrow/probe-"

// bucketNamePattern is the intersection of the S3-compatible bucket-naming rules every candidate
// vendor enforces: 3–63 characters, lowercase alphanumerics, hyphens and dots, starting and ending
// alphanumeric. A name Burrow generates is checked against it before any API call, so an unusable
// name fails here rather than as an opaque vendor rejection.
var bucketNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.\-]{1,61}[a-z0-9]$`)

// ObjectStoreConfig is the NON-SECRET half of an object-storage provider registration (ADR-0063
// §1): where the bucket is, which bucket it is, and the NAMES of the two burrow-credentials keys
// that hold the credential pair. It is what lives in the `providers` row, so it can be inspected
// without reading a Secret at all.
//
// The two key names are recorded rather than derived so the layout can change for a NEW provider
// without invalidating one already registered — the same reason ADR-0023 records a provider's
// single secret key instead of assuming it equals the provider name.
type ObjectStoreConfig struct {
	// Endpoint is the S3-compatible API endpoint that answers for this provider. The vendor is
	// whoever answers it (ADR-0063 §1): Burrow maintains no vendor list.
	Endpoint string `json:"endpoint"`
	// Region is the region the request is signed for. Vendors that have no meaningful region still
	// require one in the signature; "auto" or "us-east-1" is the usual answer.
	Region string `json:"region,omitempty"`
	// Bucket is the bucket Burrow writes to. It is RECORDED, never derived (ADR-0063 §4): Burrow
	// only ever writes to the bucket it knows about, because inferring one by name is how a tool
	// ends up writing into something that belonged to somebody else.
	Bucket string `json:"bucket"`
	// Created reports whether Burrow created this bucket, as opposed to being pointed at one that
	// already existed.
	Created bool `json:"created,omitempty"`
	// AccessKeyIDKey is the burrow-credentials key holding the access key ID.
	AccessKeyIDKey string `json:"access_key_id_key"`
	// SecretAccessKeyKey is the burrow-credentials key holding the secret access key.
	SecretAccessKeyKey string `json:"secret_access_key_key"`
	// RetentionDays is how long backups written here must stay RESTORABLE, in days — the window
	// ADR-0063 §3 reconciles the bucket's lifecycle rules against. Zero means no window has been
	// declared, which today is the truthful description of Burrow's backups: nothing prunes them,
	// so they are meant to be restorable indefinitely and ANY expiry rule will eventually delete
	// one that is still retained.
	RetentionDays int `json:"retention_days,omitempty"`
}

// objectStoreKeys returns the two burrow-credentials keys a provider named name stores its
// credential pair under — one namespaced SET of keys per provider, which is how ADR-0063 §1 refines
// ADR-0023's "one key per provider" without a new Secret and without widening burrowd's
// resourceNames-restricted grant on burrow-credentials.
func objectStoreKeys(name string) (accessKeyIDKey, secretAccessKeyKey string) {
	return name + ".access-key-id", name + ".secret-access-key"
}

// LifecycleStatus is the outcome of reconciling a bucket's lifecycle rules against backup
// retention (ADR-0063 §3).
type LifecycleStatus string

const (
	// LifecycleOK means the rules were read and none of them expires an object a retained backup
	// still needs.
	LifecycleOK LifecycleStatus = "ok"
	// LifecycleConflict means a rule would expire objects sooner than retention needs them. It is
	// refused at configuration time, which is the only point where the failure is cheap: the
	// alternative is a backup set that lists fine and cannot be restored.
	LifecycleConflict LifecycleStatus = "conflict"
	// LifecycleUnknown means the lifecycle configuration could not be read, so the invariant is
	// NOT verified and is not reported as if it were.
	LifecycleUnknown LifecycleStatus = "unknown"
)

// LifecycleCheck is what the reconciliation of ADR-0063 §3 observed: the verdict, the offending
// rule and backup when there is one, and a sentence a human can act on.
type LifecycleCheck struct {
	Status LifecycleStatus `json:"status"`
	// Detail explains the verdict in one line. For an unknown status it says WHY the configuration
	// could not be read, so nothing reads as "checked and fine".
	Detail string `json:"detail"`
	// Rule is the identifier of the rule that conflicts, when one does.
	Rule string `json:"rule,omitempty"`
	// Backup is the identifier of a retained backup the rule would already have expired, when
	// there is one. It is the concrete half of the refusal: not "this could be a problem" but
	// "this backup would be gone".
	Backup string `json:"backup,omitempty"`
}

// ProviderVerification reports what Burrow OBSERVED while registering a provider — the probe it
// wrote and deleted, the bucket it created or was pointed at, and the lifecycle reconciliation
// (ADR-0063 §2, §3, §4).
//
// It is deliberately NOT persisted: it describes one moment, and a stored copy would go stale
// while continuing to read as current. The durable answer to "is the destination healthy now?"
// is a status surface that observes it on demand (ADR-0063 §7), which is a separate change.
type ProviderVerification struct {
	// Bucket is the bucket that was verified — the recorded name, which is the only bucket Burrow
	// will ever write to.
	Bucket string `json:"bucket"`
	// BucketCreated reports whether this registration created the bucket.
	BucketCreated bool `json:"bucket_created"`
	// ProbeObject reports that a probe object was written and then deleted at configuration time,
	// so a wrong key or a typo'd endpoint failed here rather than at the first scheduled backup.
	ProbeObject bool `json:"probe_object"`
	// Lifecycle is the reconciliation of the bucket's lifecycle rules against backup retention.
	Lifecycle LifecycleCheck `json:"lifecycle"`
}

// bucketName returns the name Burrow gives a bucket it creates: a readable prefix plus the random
// component from the IDs seam (ADR-0063 §4). Bucket namespaces are GLOBAL per vendor, so a fixed
// name like `burrow-backups` is both likely taken and — worse — guessable by somebody who would
// claim it first to deny it to you.
func bucketName(id string) string {
	clean := strings.ToLower(id)
	clean = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		default:
			return -1
		}
	}, clean)
	if len(clean) > 40 {
		clean = clean[:40]
	}
	return bucketPrefix + clean
}

// reconcileLifecycle is ADR-0063 §3: does anything in this bucket's lifecycle configuration delete
// an object a retained backup still needs? It is a pure function of the rules, the declared
// retention window, and the backups already recorded, so the invariant that justifies this whole
// feature is testable without a vendor.
//
// A rule conflicts when it expires objects sooner than retention needs them. With NO declared
// window (retentionDays == 0) every age-expiring rule conflicts, because Burrow prunes nothing:
// its backups are meant to be restorable indefinitely, so an expiry of any length eventually
// deletes one that is still retained. Declaring a window is how an operator says otherwise.
//
// Where a recorded backup is ALREADY older than the rule's expiry, the refusal names it. That is
// the difference between warning that something might go wrong and reporting which backup would be
// gone.
func reconcileLifecycle(rules []LifecycleRule, retentionDays int, backups []Backup, now time.Time) LifecycleCheck {
	var worst *LifecycleRule
	for i := range rules {
		r := rules[i]
		if !r.Enabled || r.ExpireAfterDays <= 0 {
			continue // a disabled rule, or one that expires nothing by age, deletes no backup
		}
		if retentionDays > 0 && r.ExpireAfterDays >= retentionDays {
			continue // the rule outlives the window, so nothing retained is expired by it
		}
		if worst == nil || r.ExpireAfterDays < worst.ExpireAfterDays {
			worst = &rules[i]
		}
	}
	if worst == nil {
		return LifecycleCheck{
			Status: LifecycleOK,
			Detail: lifecycleOKDetail(rules, retentionDays),
		}
	}

	check := LifecycleCheck{Status: LifecycleConflict, Rule: worst.ID}
	window := fmt.Sprintf("a declared retention window of %d days", retentionDays)
	if retentionDays == 0 {
		window = "backups Burrow keeps indefinitely (no retention window is declared, and nothing prunes them)"
	}
	check.Detail = fmt.Sprintf("lifecycle rule %s expires %s after %d days, sooner than %s",
		ruleName(*worst), ruleScope(*worst), worst.ExpireAfterDays, window)

	if b, ok := oldestBackupBeyond(backups, worst.ExpireAfterDays, now); ok {
		check.Backup = b.ID
		check.Detail += fmt.Sprintf("; backup %s (app %s, taken %s) is already older than that and would be gone",
			b.ID, b.App, b.CreatedAt.UTC().Format(time.RFC3339))
	}
	check.Detail += ". Change the rule at the vendor, or declare the window it enforces with --retention-days"
	return check
}

// lifecycleOKDetail states what was actually checked, so an "ok" is a claim about something rather
// than an absence of complaint.
func lifecycleOKDetail(rules []LifecycleRule, retentionDays int) string {
	switch {
	case len(rules) == 0 && retentionDays > 0:
		return fmt.Sprintf("the bucket has no lifecycle rules, so nothing expires a backup inside the %d-day retention window", retentionDays)
	case len(rules) == 0:
		return "the bucket has no lifecycle rules, so nothing expires a backup"
	case retentionDays > 0:
		return fmt.Sprintf("no lifecycle rule expires an object sooner than the %d-day retention window", retentionDays)
	default:
		return "no lifecycle rule expires objects by age"
	}
}

func ruleName(r LifecycleRule) string {
	if r.ID == "" {
		return "(unnamed)"
	}
	return r.ID
}

func ruleScope(r LifecycleRule) string {
	if r.Prefix == "" {
		return "objects in the whole bucket"
	}
	return fmt.Sprintf("objects under %q", r.Prefix)
}

// oldestBackupBeyond returns the oldest recorded backup older than days, if any — the concrete
// casualty a conflicting rule already has.
func oldestBackupBeyond(backups []Backup, days int, now time.Time) (Backup, bool) {
	cutoff := now.AddDate(0, 0, -days)
	var oldest Backup
	found := false
	for _, b := range backups {
		if b.Status != BackupCompleted || !b.CreatedAt.Before(cutoff) {
			continue
		}
		if !found || b.CreatedAt.Before(oldest.CreatedAt) {
			oldest, found = b, true
		}
	}
	return oldest, found
}

// addObjectStorageProvider registers an object-storage provider (ADR-0063). The order is what makes
// the registration trustworthy, and it is the same validate-then-write shape ADR-0023 established
// for a DNS token — nothing is recorded until the destination has been proven to work:
//
//  1. resolve the bucket: create the one Burrow will own (guarded, ADR-0063 §5), or take the one
//     the operator named and confirm it is actually there;
//  2. PROBE it — write an object and delete it — so a wrong key or a typo'd endpoint fails now and
//     loudly, rather than at the first scheduled backup, silently (ADR-0063 §2);
//  3. reconcile the bucket's lifecycle rules against backup retention and REFUSE on disagreement,
//     naming the rule and the backup (ADR-0063 §3), reporting "unknown" where the configuration
//     cannot be read rather than implying it was checked;
//  4. only then write the credential pair into burrow-credentials and record the row.
//
// The credential values are used in memory here and never logged, audited, returned, or formatted
// into an error.
func (e *Engine) addObjectStorageProvider(ctx context.Context, p Provider, req AddProviderRequest) (Provider, error) {
	if e.objectStore == nil {
		return Provider{}, fmt.Errorf("add provider %s: object storage: %w", p.Name, ErrNotImplemented)
	}
	cfg := ObjectStoreConfig{
		Endpoint:      strings.TrimSpace(req.Endpoint),
		Region:        strings.TrimSpace(req.Region),
		Bucket:        strings.TrimSpace(req.Bucket),
		RetentionDays: req.RetentionDays,
	}
	cfg.AccessKeyIDKey, cfg.SecretAccessKeyKey = objectStoreKeys(p.Name)
	cred := ObjectStoreCredential{AccessKeyID: req.AccessKeyID, SecretAccessKey: req.SecretAccessKey}

	if err := validateObjectStoreRequest(p.Name, cfg, cred, req.CreateBucket); err != nil {
		return Provider{}, err
	}

	store, err := e.objectStore.ObjectStore(cfg.Endpoint, cfg.Region, cred)
	if err != nil {
		return Provider{}, fmt.Errorf("add provider %s: %w", p.Name, err)
	}

	created := false
	if req.CreateBucket {
		name, err := e.createBucket(ctx, store, p.Name, req.Confirm)
		if err != nil {
			return Provider{}, err
		}
		cfg.Bucket, created = name, true
	} else {
		ok, err := store.BucketExists(ctx, cfg.Bucket)
		if err != nil {
			return Provider{}, fmt.Errorf("add provider %s: reaching bucket %s at %s: %w", p.Name, cfg.Bucket, cfg.Endpoint, err)
		}
		if !ok {
			return Provider{}, fmt.Errorf("add provider %s: bucket %s is not present at %s, or this credential cannot reach it; Burrow only writes to a bucket it has verified and never infers one by name (ADR-0063 §4): %w",
				p.Name, cfg.Bucket, cfg.Endpoint, ErrInvalid)
		}
	}
	cfg.Created = created

	if err := e.probeBucket(ctx, store, p.Name, cfg); err != nil {
		return Provider{}, err
	}

	check, err := e.checkLifecycle(ctx, store, cfg)
	if err != nil {
		return Provider{}, fmt.Errorf("add provider %s: reading bucket %s lifecycle configuration: %w", p.Name, cfg.Bucket, err)
	}
	if check.Status == LifecycleConflict {
		return Provider{}, fmt.Errorf("add provider %s: %s: %w", p.Name, check.Detail, ErrInvalid)
	}

	// Verified: write the credential pair — two keys, one Secret, no new RBAC (ADR-0063 §1) — and
	// record the non-secret row. A failure to write the second key leaves the first, which is why
	// the row (the thing that says a provider EXISTS) is written last.
	if err := e.credentials.SetToken(ctx, cfg.AccessKeyIDKey, cred.AccessKeyID); err != nil {
		return Provider{}, fmt.Errorf("add provider %s: storing the access key id: %w", p.Name, err)
	}
	if err := e.credentials.SetToken(ctx, cfg.SecretAccessKeyKey, cred.SecretAccessKey); err != nil {
		return Provider{}, fmt.Errorf("add provider %s: storing the secret access key: %w", p.Name, err)
	}

	p.SecretKey = cfg.AccessKeyIDKey
	p.ObjectStore = &cfg
	if err := e.db.SaveProvider(ctx, p); err != nil {
		return Provider{}, fmt.Errorf("add provider %s: recording in the registry: %w", p.Name, err)
	}
	p.Verification = &ProviderVerification{
		Bucket:        cfg.Bucket,
		BucketCreated: created,
		ProbeObject:   true,
		Lifecycle:     check,
	}
	return p, nil
}

// createBucket mints a globally unique, unguessable name and creates the bucket behind the
// bucket.create guardrail (ADR-0063 §4, §5). Creation is CONFIRM rather than absent or denied: it
// is additive, reversible, and part of a legitimate workflow — but it costs money at a vendor, so a
// human approves it. Deleting a bucket is not here and is not anywhere in Burrow: its blast radius
// is every backup the platform holds and its name is global, so a mistaken argument could reach
// outside the cluster entirely (ADR-0065 tier 1).
func (e *Engine) createBucket(ctx context.Context, store ObjectStore, provider string, confirmed bool) (string, error) {
	policy, err := e.db.Policy(ctx)
	if err != nil {
		return "", fmt.Errorf("add provider %s: reading the guardrail policy: %w", provider, err)
	}
	if err := policy.evaluateGuardrail(ctx, GuardrailScope{}, "create bucket", GuardrailBucketCreate, confirmed,
		fmt.Sprintf("creating a bucket for provider %s", provider)); err != nil {
		return "", err
	}

	name := bucketName(e.ids.NewID())
	if !bucketNamePattern.MatchString(name) {
		return "", fmt.Errorf("add provider %s: generated bucket name %q is not a valid bucket name: %w", provider, name, ErrInvalid)
	}
	if err := store.CreateBucket(ctx, name); err != nil {
		return "", fmt.Errorf("add provider %s: creating bucket %s: %w", provider, name, err)
	}
	return name, nil
}

// probeBucket writes and deletes one small object (ADR-0063 §2). It is the difference between a
// registration that recorded a credential and one that proved the destination works: a wrong key,
// a typo'd endpoint, or a credential with read-only permission fails HERE, at a moment somebody is
// watching, instead of at the first scheduled backup with nobody watching.
func (e *Engine) probeBucket(ctx context.Context, store ObjectStore, provider string, cfg ObjectStoreConfig) error {
	key := probeKeyPrefix + e.ids.NewID()
	body := []byte("burrow destination probe\n")
	if err := store.PutObject(ctx, cfg.Bucket, key, body); err != nil {
		return fmt.Errorf("add provider %s: writing a probe object to %s at %s failed, so this destination is not usable for backups: %w: %w",
			provider, cfg.Bucket, cfg.Endpoint, err, ErrInvalid)
	}
	if err := store.DeleteObject(ctx, cfg.Bucket, key); err != nil {
		return fmt.Errorf("add provider %s: the probe object was written to %s but could not be deleted, so this credential cannot remove what it writes: %w: %w",
			provider, cfg.Bucket, err, ErrInvalid)
	}
	return nil
}

// checkLifecycle reads the bucket's lifecycle rules and reconciles them against retention. An
// unreadable configuration is reported as unknown WITH THE REASON — not as verified, and not as a
// failure that blocks registration (ADR-0063 §3).
func (e *Engine) checkLifecycle(ctx context.Context, store ObjectStore, cfg ObjectStoreConfig) (LifecycleCheck, error) {
	rules, err := store.LifecycleRules(ctx, cfg.Bucket)
	if errors.Is(err, ErrLifecycleUnknown) {
		return LifecycleCheck{
			Status: LifecycleUnknown,
			Detail: fmt.Sprintf("Burrow could not read the lifecycle configuration of bucket %s (%v), so whether a rule expires objects a retained backup still needs is UNKNOWN — it has not been checked. Confirm at the vendor that no rule expires objects sooner than backups must stay restorable",
				cfg.Bucket, err),
		}, nil
	}
	if err != nil {
		return LifecycleCheck{}, err
	}
	backups, err := e.db.ListBackups(ctx, "", "")
	if err != nil {
		return LifecycleCheck{}, fmt.Errorf("reading recorded backups: %w", err)
	}
	return reconcileLifecycle(rules, cfg.RetentionDays, backups, e.clock.Now()), nil
}

// validateObjectStoreRequest rejects a malformed object-storage registration before any vendor call
// and before any credential is written.
func validateObjectStoreRequest(name string, cfg ObjectStoreConfig, cred ObjectStoreCredential, createBucket bool) error {
	switch {
	case cfg.Endpoint == "":
		return fmt.Errorf("add provider %s: an S3-compatible --endpoint is required; the vendor is whoever answers it (ADR-0063 §1): %w", name, ErrInvalid)
	case !strings.HasPrefix(cfg.Endpoint, "https://") && !strings.HasPrefix(cfg.Endpoint, "http://"):
		return fmt.Errorf("add provider %s: endpoint %q must be a URL (https://…): %w", name, cfg.Endpoint, ErrInvalid)
	case cred.AccessKeyID == "" || cred.SecretAccessKey == "":
		return fmt.Errorf("add provider %s: an object-storage credential is a PAIR — both an access key id and a secret access key are required: %w", name, ErrInvalid)
	case createBucket && cfg.Bucket != "":
		return fmt.Errorf("add provider %s: give either --bucket (use this existing bucket) or --create-bucket (Burrow names and creates its own), not both: %w", name, ErrInvalid)
	case !createBucket && cfg.Bucket == "":
		return fmt.Errorf("add provider %s: a --bucket is required, or --create-bucket to have Burrow create one: %w", name, ErrInvalid)
	case cfg.Bucket != "" && !bucketNamePattern.MatchString(cfg.Bucket):
		return fmt.Errorf("add provider %s: bucket %q is not a valid S3 bucket name (3-63 lowercase alphanumerics, hyphens or dots): %w", name, cfg.Bucket, ErrInvalid)
	case cfg.RetentionDays < 0:
		return fmt.Errorf("add provider %s: retention days must not be negative: %w", name, ErrInvalid)
	case !secretKeyPattern.MatchString(cfg.AccessKeyIDKey) || !secretKeyPattern.MatchString(cfg.SecretAccessKeyKey):
		return fmt.Errorf("add provider %s: the credential key names %q/%q are not valid Secret data keys: %w",
			name, cfg.AccessKeyIDKey, cfg.SecretAccessKeyKey, ErrInvalid)
	}
	return nil
}

// ShipBackupCommand is the subcommand the backup Job's shipping container runs: burrowd, from
// Burrow's own image, writing one dump to the object store and reading it back (ADR-0063 §7).
//
// It is named here rather than written twice because it is a contract between two packages that
// never call each other — the Job spec in controlplane/kube passes it as an ARGUMENT to the image's
// entrypoint, and cmd/burrowd dispatches on it before any control-plane wiring runs. The probe's
// two subcommands are named in dependencies.go for the same reason (ProbeInstallCommand,
// ProbeCheckCommand); this one was the odd literal out.
const ShipBackupCommand = "ship-backup"

// BackupDestination is everything the in-cluster backup Job needs to write one dump to the object
// store: which provider it is, where the bucket is, and the credential to sign with (ADR-0063 §7).
//
// It carries a SECRET VALUE and is therefore an internal, in-memory shape only: it has no JSON tags
// and is never returned over the control-plane API, never audited, never logged, and never placed
// in an error. The engine assembles it at call time from the provider row and the credential store,
// hands it to the Kubernetes seam, and the adapter puts the pair into a Job-owned Secret the pod
// mounts — the same shape ADR-0057 §4 established for a build's source token, and the same reason:
// a credential in a Job's env or command line is visible to anything that can read the Job.
type BackupDestination struct {
	// Provider is the registry name of the object-storage provider. It is not secret, and it is what
	// gets recorded on the Backup row.
	Provider string
	// Config is the provider's non-secret configuration — endpoint, region, bucket.
	Config ObjectStoreConfig
	// Credential is the pair the S3 request is signed with. Secret; see the type's own comment.
	Credential ObjectStoreCredential
	// Key is the object key this backup occupies, from BackupObjectKey.
	Key string
}

// BackupJobOutcome is what the backup Job reported: the dump's size, and on a failure the closed
// reason and a Burrow-authored detail (ADR-0063 §7, ADR-0074 §5).
//
// It exists so a failed backup is recorded with WHY rather than with a wrapped error string a
// caller would have to parse. Reason is a member of the closed BackupFailureReason set, or of
// ADR-0074 §2's IssueReason set when the Job never started at all. Detail never carries a vendor
// response body and never carries a credential.
type BackupJobOutcome struct {
	SizeBytes int64
	Reason    string
	Detail    string
}

// resolveBackupDestination returns the object-storage destination a backup should be written to, or
// a nil destination when none is registered (ADR-0063 §7).
//
// A nil destination is NOT an error. An install with no object-storage provider keeps taking the
// in-cluster dumps ADR-0032 gives it, and the row records BackupDestinationCluster so the listing
// says plainly that those backups share a failure domain with the database. Refusing to back up at
// all because nothing durable is configured would remove the weaker backup as well as the stronger
// one.
//
// Ambiguity IS an error. ADR-0063 §6 allows several object-storage providers on purpose — a user
// migrating between vendors has two — and choosing one of them silently is how the backups quietly
// stop arriving at the destination somebody is watching. So the rule mirrors resolveDNSProvider: one
// registered provider is the destination, several means the operation names one.
func (e *Engine) resolveBackupDestination(ctx context.Context, name, app, env, backupID string) (*BackupDestination, error) {
	all, err := e.db.Providers(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading providers: %w", err)
	}
	var stores []Provider
	for _, p := range all {
		if p.Serves(CapabilityObjectStorage) && p.ObjectStore != nil {
			stores = append(stores, p)
		}
	}

	var chosen Provider
	switch {
	case strings.TrimSpace(name) != "":
		found := false
		for _, p := range stores {
			if p.Name == name {
				chosen, found = p, true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("no object-storage provider named %q is registered — add one with `burrow config provider add --type s3`: %w", name, ErrNotFound)
		}
	case len(stores) == 0:
		// Nothing durable is configured: back up to the cluster, and let the row say so.
		return nil, nil
	case len(stores) == 1:
		chosen = stores[0]
	default:
		names := make([]string, len(stores))
		for i, p := range stores {
			names[i] = p.Name
		}
		sort.Strings(names)
		return nil, fmt.Errorf("multiple object-storage providers are registered (%s) — pass --destination to say which one holds this backup; Burrow will not guess, because guessing wrong writes the backup somewhere nobody is watching: %w",
			strings.Join(names, ", "), ErrInvalid)
	}

	// Read at call time so a rotated key is picked up with no restart (ADR-0023). The pair lives in
	// memory from here to the Job-owned Secret and goes nowhere else.
	cred, err := e.ObjectStoreCredentialFor(ctx, chosen)
	if err != nil {
		return nil, err
	}
	return &BackupDestination{
		Provider:   chosen.Name,
		Config:     *chosen.ObjectStore,
		Credential: cred,
		Key:        BackupObjectKey(app, env, backupID),
	}, nil
}

// ObjectStoreCredentialFor reads an object-storage provider's credential PAIR from
// burrow-credentials at call time, so a rotated key is picked up with no restart (ADR-0023,
// ADR-0063 §1). The values are returned to the caller that makes the vendor call and are never
// logged, audited, or returned over any API.
func (e *Engine) ObjectStoreCredentialFor(ctx context.Context, p Provider) (ObjectStoreCredential, error) {
	if p.ObjectStore == nil {
		return ObjectStoreCredential{}, fmt.Errorf("provider %s serves no object storage: %w", p.Name, ErrInvalid)
	}
	id, err := e.credentials.Token(ctx, p.ObjectStore.AccessKeyIDKey)
	if err != nil {
		return ObjectStoreCredential{}, fmt.Errorf("provider %s: reading the access key id: %w", p.Name, err)
	}
	secret, err := e.credentials.Token(ctx, p.ObjectStore.SecretAccessKeyKey)
	if err != nil {
		return ObjectStoreCredential{}, fmt.Errorf("provider %s: reading the secret access key: %w", p.Name, err)
	}
	return ObjectStoreCredential{AccessKeyID: id, SecretAccessKey: secret}, nil
}

// ArchiveDestination is everything one environment's pgBackRest repository needs: where the bucket
// is, the credential to reach it with, the repository path inside it, and how long a backup written
// there must stay restorable (ADR-0063 §3, ADR-0066 §3).
//
// Like BackupDestination it carries a SECRET VALUE, so it is an internal in-memory shape only: no
// JSON tags, never returned over the control-plane API, never audited, never logged, never placed in
// an error. The engine assembles it from the provider row and the credential store at call time, and
// the adapter puts the pair into a Secret in the add-on namespace that the plugin's sidecar reads by
// reference — the pair never appears in the `Stanza`, in a container's environment, or in an argv.
//
// It is separate from BackupDestination rather than an extra field on it because the two address
// different things. A BackupDestination names ONE OBJECT — the dump this backup is writing — and is
// resolved per backup. An ArchiveDestination names a REPOSITORY that outlives every individual
// backup in it, and is resolved when an instance is created, because that is when the write-ahead-log
// path has to exist.
type ArchiveDestination struct {
	// Provider is the registry name of the object-storage provider. Not secret.
	Provider string
	// Config is the provider's non-secret configuration — endpoint, region, bucket, retention.
	Config ObjectStoreConfig
	// Credential is the pair pgBackRest signs its S3 requests with. Secret; see the type comment.
	Credential ObjectStoreCredential
	// RepoPath is the repository path within the bucket, from PgBackRestRepoPath — one per
	// environment, so one environment's repository is not addressable from another's stanza.
	RepoPath string
}

// RetentionDays is the window backups in this repository must stay restorable for, in days.
//
// IT IS BURROW'S NUMBER, AND IT IS THE ONLY ONE. pgBackRest has its own retention and CloudNativePG
// has another; ADR-0063 §3 exists because two retention policies that disagree delete a backup
// somebody was relying on, and the resolution is not to reconcile them afterwards but to have one.
// The window declared on the provider — the same one the bucket's lifecycle rules are refused for
// contradicting — is written INTO the repository's retention, so pgBackRest expires against Burrow's
// window rather than beside it. Zero declares no window, and no retention is configured at all: an
// unbounded repository is a cost problem, and a repository that quietly expires what a lifecycle
// rule was reconciled against is a recovery problem.
func (d ArchiveDestination) RetentionDays() int { return d.Config.RetentionDays }

// resolveArchiveDestination returns the pgBackRest repository environment env's instance archives to,
// or nil when no object-storage provider is registered (ADR-0063 §7, ADR-0066 §3).
//
// A nil destination is NOT an error, for resolveBackupDestination's reason turned up one notch: an
// install with no object storage gets exactly the instance it got before — a `Cluster` with no
// plugin, no sidecar and no archiving — and physical backups are refused by name when one is asked
// for. Refusing to install Postgres at all because nothing durable is configured would take the
// small self-hoster's database away to protect backups they did not ask for.
//
// Ambiguity IS an error, and it is resolveBackupDestination's again: several registered providers is
// a supported state (ADR-0063 §6) and picking one silently is how the archive quietly goes somewhere
// nobody is watching. Unlike a single backup, this choice is made once and then persists in the
// instance's own spec, so guessing wrong is not a bad backup but a bad instance.
func (e *Engine) resolveArchiveDestination(ctx context.Context, name, env string) (*ArchiveDestination, error) {
	all, err := e.db.Providers(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading providers: %w", err)
	}
	var stores []Provider
	for _, p := range all {
		if p.Serves(CapabilityObjectStorage) && p.ObjectStore != nil {
			stores = append(stores, p)
		}
	}

	var chosen Provider
	switch {
	case strings.TrimSpace(name) != "":
		found := false
		for _, p := range stores {
			if p.Name == name {
				chosen, found = p, true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("no object-storage provider named %q is registered to archive to — add one with `burrow config provider add --type s3`: %w", name, ErrNotFound)
		}
	case len(stores) == 0:
		return nil, nil
	case len(stores) == 1:
		chosen = stores[0]
	default:
		names := make([]string, len(stores))
		for i, p := range stores {
			names[i] = p.Name
		}
		sort.Strings(names)
		// The flag is spelled differently on the commands that reach this, so it is described rather
		// than named: `addon install` takes --archive-destination (it is choosing a repository for a
		// new instance) while `addon backup-instance` and `addon restore-instance` take --destination
		// (they are naming the one an instance already has). An error that names a flag the command
		// in front of the operator does not have is worse than one that names none.
		return nil, fmt.Errorf("multiple object-storage providers are registered (%s) — name the one holding this instance's write-ahead log and base backups on the command line (--archive-destination on `addon install`, --destination on `addon backup-instance` and `addon restore-instance`); Burrow will not guess, because an instance keeps the repository it was created against: %w",
			strings.Join(names, ", "), ErrInvalid)
	}

	cred, err := e.ObjectStoreCredentialFor(ctx, chosen)
	if err != nil {
		return nil, err
	}
	return &ArchiveDestination{
		Provider:   chosen.Name,
		Config:     *chosen.ObjectStore,
		Credential: cred,
		RepoPath:   PgBackRestRepoPath(env),
	}, nil
}

// PhysicalBackupOutcome is what a CloudNativePG `Backup` object reported (ADR-0066 §2, §4).
//
// It is BackupJobOutcome's sibling and deliberately not the same type. A Job reports a byte count it
// measured; a `Backup` object carries no size field of any kind, which is the gap ADR-0066 §4 makes
// Burrow's to close, so what comes back here is an identity — pgBackRest's own label for the backup
// — and the size is joined afterwards from the store rather than read off the status.
type PhysicalBackupOutcome struct {
	// Label is pgBackRest's backup label (for example `20260210-101333F`), read from the `Backup`
	// object's status. It is the name the repository knows this backup by, the handle a restore names,
	// and the path component under which the manifest that proves it arrived lives. Empty when the
	// backup did not complete.
	Label string
	// ObjectKey is where the backup's manifest is in the bucket, derived by the adapter from the
	// instance's OWN `Stanza` — its repository path and stanza name — rather than from what Burrow
	// would have configured. It is the key the read-back verifies at and the address the row records.
	// Empty when the backup did not complete.
	ObjectKey string
	// Reason is a member of the closed BackupFailureReason set, or of ADR-0074 §2's IssueReason set.
	Reason string
	// Detail is one Burrow-authored line elaborating it — never a vendor response body, never a
	// credential. CloudNativePG's own `status.error` is a Burrow-safe field to relay: it is the
	// operator's rendering of what pgBackRest exited with, and it holds no credential because the
	// credential never reaches an argv.
	Detail string
}
