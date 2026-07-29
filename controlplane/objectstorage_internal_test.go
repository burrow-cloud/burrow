// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"strings"
	"testing"
	"time"
)

// reconcileLifecycle is the invariant ADR-0063 §3 says justifies the feature existing, so it is a
// pure function and it is tested directly rather than only through a registration. The failure it
// prevents is silent, delayed, and total: the backup list looks correct and the restore fails.

var reconcileNow = time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

func TestReconcileLifecycle(t *testing.T) {
	old := Backup{ID: "bkp-old", App: "web", Status: BackupCompleted, CreatedAt: reconcileNow.AddDate(0, 0, -60)}
	recent := Backup{ID: "bkp-new", App: "web", Status: BackupCompleted, CreatedAt: reconcileNow.AddDate(0, 0, -1)}
	failed := Backup{ID: "bkp-bad", App: "web", Status: BackupFailed, CreatedAt: reconcileNow.AddDate(0, 0, -90)}

	cases := []struct {
		name       string
		rules      []LifecycleRule
		retention  int
		backups    []Backup
		wantStatus LifecycleStatus
		wantRule   string
		wantBackup string
	}{
		{
			name:       "no rules is the simple pass",
			retention:  30,
			wantStatus: LifecycleOK,
		},
		{
			name:       "a rule that outlives the window expires nothing retained",
			rules:      []LifecycleRule{{ID: "expire-90d", Enabled: true, ExpireAfterDays: 90}},
			retention:  30,
			backups:    []Backup{old, recent},
			wantStatus: LifecycleOK,
		},
		{
			name:       "a rule exactly at the window is not a conflict",
			rules:      []LifecycleRule{{ID: "expire-30d", Enabled: true, ExpireAfterDays: 30}},
			retention:  30,
			wantStatus: LifecycleOK,
		},
		{
			name:       "a disabled rule deletes nothing",
			rules:      []LifecycleRule{{ID: "expire-1d", Enabled: false, ExpireAfterDays: 1}},
			retention:  30,
			backups:    []Backup{old},
			wantStatus: LifecycleOK,
		},
		{
			name:       "a rule that expires nothing by age is not a conflict",
			rules:      []LifecycleRule{{ID: "abort-multipart", Enabled: true}},
			retention:  30,
			wantStatus: LifecycleOK,
		},
		{
			name:       "a rule shorter than the window is refused",
			rules:      []LifecycleRule{{ID: "expire-7d", Enabled: true, ExpireAfterDays: 7}},
			retention:  30,
			wantStatus: LifecycleConflict,
			wantRule:   "expire-7d",
		},
		{
			name:       "the refusal names the backup already beyond the rule",
			rules:      []LifecycleRule{{ID: "expire-7d", Enabled: true, ExpireAfterDays: 7}},
			retention:  30,
			backups:    []Backup{recent, old},
			wantStatus: LifecycleConflict,
			wantRule:   "expire-7d",
			wantBackup: "bkp-old",
		},
		{
			name:       "a backup that never completed is not a casualty",
			rules:      []LifecycleRule{{ID: "expire-7d", Enabled: true, ExpireAfterDays: 7}},
			retention:  30,
			backups:    []Backup{failed, recent},
			wantStatus: LifecycleConflict,
			wantRule:   "expire-7d",
			wantBackup: "",
		},
		{
			// Nothing prunes Burrow's backups today, so with no declared window they are meant to be
			// restorable indefinitely and ANY expiry rule eventually deletes one that is retained.
			// Declaring a window is how an operator says otherwise.
			name:       "with no declared window every expiring rule conflicts",
			rules:      []LifecycleRule{{ID: "expire-365d", Enabled: true, ExpireAfterDays: 365}},
			retention:  0,
			wantStatus: LifecycleConflict,
			wantRule:   "expire-365d",
		},
		{
			name: "the strictest offending rule is the one reported",
			rules: []LifecycleRule{
				{ID: "expire-20d", Enabled: true, ExpireAfterDays: 20},
				{ID: "expire-3d", Enabled: true, ExpireAfterDays: 3},
			},
			retention:  30,
			wantStatus: LifecycleConflict,
			wantRule:   "expire-3d",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reconcileLifecycle(tc.rules, tc.retention, tc.backups, reconcileNow)
			if got.Status != tc.wantStatus {
				t.Fatalf("status = %q (%s), want %q", got.Status, got.Detail, tc.wantStatus)
			}
			if got.Rule != tc.wantRule {
				t.Errorf("rule = %q, want %q", got.Rule, tc.wantRule)
			}
			if got.Backup != tc.wantBackup {
				t.Errorf("backup = %q, want %q", got.Backup, tc.wantBackup)
			}
			if got.Detail == "" {
				t.Error("the verdict carries no detail; a refusal a human cannot act on is not a refusal")
			}
			if tc.wantStatus == LifecycleConflict && !strings.Contains(got.Detail, tc.wantRule) {
				t.Errorf("the refusal does not name the rule: %q", got.Detail)
			}
		})
	}
}

// TestBucketNameIsReadableAndSanitized: the name carries a readable prefix so a human can tell what
// the bucket is, and whatever entropy the IDs seam produced is reduced to characters every
// S3-compatible vendor accepts (ADR-0063 §4).
func TestBucketNameIsReadableAndSanitized(t *testing.T) {
	for _, id := range []string{
		"9f2c1ab34de5678901234567890abcdef",
		"A-B_C.d",
		"0123456789012345678901234567890123456789012345678901234567890123456789",
	} {
		name := bucketName(id)
		if !strings.HasPrefix(name, bucketPrefix) {
			t.Errorf("bucketName(%q) = %q, want the readable prefix", id, name)
		}
		if !bucketNamePattern.MatchString(name) {
			t.Errorf("bucketName(%q) = %q, which is not a valid bucket name", id, name)
		}
	}
}

// TestObjectStoreKeysAreNamespacedPerProvider: one namespaced SET of keys per provider is how
// ADR-0063 §1 extends ADR-0023's one-key-per-provider Secret without a second Secret — which is
// what would have forced burrowd's resourceNames grant wider.
func TestObjectStoreKeysAreNamespacedPerProvider(t *testing.T) {
	idA, secretA := objectStoreKeys("backups")
	idB, secretB := objectStoreKeys("offsite")
	for _, k := range []string{idA, secretA, idB, secretB} {
		if !secretKeyPattern.MatchString(k) {
			t.Errorf("key %q is not a valid Kubernetes Secret data key", k)
		}
	}
	if idA == secretA {
		t.Error("the pair must occupy two distinct keys")
	}
	if idA == idB || secretA == secretB {
		t.Error("two providers must not share a key; the registry is a table, not a singleton (ADR-0063 §6)")
	}
}
