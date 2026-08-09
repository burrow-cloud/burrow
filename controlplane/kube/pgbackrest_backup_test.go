// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/kube"
)

// These tests cover ADR-0066 §2's adapter half. What they are mostly asserting is what ISN'T there:
// a physical backup is one custom resource and a read of its status, so the failures worth testing
// are about telling the terminal phases apart rather than about orchestration.

// settledBackup pre-creates the `Backup` object the adapter is about to ask for, already in a
// terminal phase. The adapter tolerates AlreadyExists on create and then reads status, so seeding the
// object is how a fake dynamic client — which reconciles nothing — models an operator that answered.
func settledBackup(t *testing.T, dyn dynamic.Interface, backupID string, status map[string]any) {
	t.Helper()
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": kube.CNPGAPIGroup + "/v1",
		"kind":       "Backup",
		"metadata":   map[string]any{"name": "burrow-pg-backup-" + backupID, "namespace": cnpgTestNamespace},
		"spec":       map[string]any{"method": "plugin"},
		"status":     status,
	}}
	if _, err := dyn.Resource(backupGVR).Namespace(cnpgTestNamespace).Create(context.Background(), obj, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seeding the Backup object: %v", err)
	}
}

// archivingInstance installs an archiving Postgres instance into a fresh fake cluster and returns the
// adapter and the dynamic client, so the physical-backup tests start from the state a real install
// leaves.
func archivingInstance(t *testing.T) (*kube.Adapter, dynamic.Interface) {
	t.Helper()
	client, dyn := archivingCluster()
	a := kube.New(client, "burrow").WithDynamicClient(dyn)
	if _, err := a.DeployAddon(context.Background(), postgresSpec(t), controlplane.DefaultEnvironment, testInstanceOf(postgresSpec(t), controlplane.DefaultEnvironment),
		testArchive(controlplane.DefaultEnvironment, 30)); err != nil {
		t.Fatalf("DeployAddon: %v", err)
	}
	return a, dyn
}

// TestRunPhysicalBackupCreatesTheBackupObject asserts the whole mechanism: Burrow composes one
// `postgresql.cnpg.io/v1 Backup` with method `plugin`, pointed at the environment's `Cluster`, and
// reads pgBackRest's own label back off its status. No Job, no argv, no credential.
func TestRunPhysicalBackupCreatesTheBackupObject(t *testing.T) {
	ctx := context.Background()
	a, dyn := archivingInstance(t)
	settledBackup(t, dyn, "b1", map[string]any{"phase": "completed", "backupName": "20260801-020000F"})

	out, err := a.RunPhysicalBackup(ctx, controlplane.DefaultEnvironment, testInstance(controlplane.DefaultEnvironment), "b1", testArchive(controlplane.DefaultEnvironment, 30))
	if err != nil {
		t.Fatalf("RunPhysicalBackup: %v", err)
	}
	if out.Label != "20260801-020000F" {
		t.Errorf("label = %q, want pgBackRest's own backup label", out.Label)
	}
	// The key is derived from the INSTANCE's repository path and stanza, which is what makes the
	// read-back look where this instance actually wrote.
	want := controlplane.PgBackRestManifestKey(controlplane.PgBackRestRepoPath(controlplane.DefaultEnvironment),
		"burrow-postgres", "20260801-020000F")
	if out.ObjectKey != want {
		t.Errorf("object key = %q, want %q", out.ObjectKey, want)
	}

	obj, err := dyn.Resource(backupGVR).Namespace(cnpgTestNamespace).Get(ctx, "burrow-pg-backup-b1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the Backup object: %v", err)
	}
	if method, _, _ := unstructured.NestedString(obj.Object, "spec", "method"); method != "plugin" {
		t.Errorf("method = %q, want plugin", method)
	}
}

// TestRunPhysicalBackupTellsTheTwoFailuresApart is ADR-0063 §7's distinction on the physical path. A
// `Backup` object that FAILED is the backup not happening, with nothing offered to the store;
// `walArchivingFailing` is the store not accepting what the instance is producing. Reporting both as
// "the backup failed" would send an operator to the database when the answer is at the vendor.
func TestRunPhysicalBackupTellsTheTwoFailuresApart(t *testing.T) {
	for _, tc := range []struct {
		phase  string
		reason string
	}{
		{phase: "failed", reason: controlplane.BackupReasonDumpFailed},
		{phase: "invalid backup definition", reason: controlplane.BackupReasonDumpFailed},
		{phase: "walArchivingFailing", reason: controlplane.BackupReasonStoreUnreachable},
	} {
		t.Run(tc.phase, func(t *testing.T) {
			ctx := context.Background()
			a, dyn := archivingInstance(t)
			settledBackup(t, dyn, "b1", map[string]any{"phase": tc.phase, "error": "pgbackrest exited 1"})

			out, err := a.RunPhysicalBackup(ctx, controlplane.DefaultEnvironment, testInstance(controlplane.DefaultEnvironment), "b1", testArchive(controlplane.DefaultEnvironment, 30))
			if err == nil {
				t.Fatalf("RunPhysicalBackup on phase %q must fail", tc.phase)
			}
			if out.Reason != tc.reason {
				t.Errorf("reason = %q, want %q", out.Reason, tc.reason)
			}
			if !strings.Contains(out.Detail, "pgbackrest exited 1") {
				t.Errorf("detail = %q, want CloudNativePG's own error text carried through", out.Detail)
			}
		})
	}
}

// TestRunPhysicalBackupRefusesAnInstanceThatDoesNotArchive asserts the refusal happens BEFORE any
// object is written. A `Backup` created against a `Cluster` with no plugin has nowhere to write and
// would sit in `pending` until the wait timed out — ten minutes to learn something one read answers
// now, and the refusal can say what to do about it.
func TestRunPhysicalBackupRefusesAnInstanceThatDoesNotArchive(t *testing.T) {
	ctx := context.Background()
	client, dyn := archivingCluster()
	a := kube.New(client, "burrow").WithDynamicClient(dyn)
	if _, err := a.DeployAddon(ctx, postgresSpec(t), controlplane.DefaultEnvironment, testInstanceOf(postgresSpec(t), controlplane.DefaultEnvironment), nil); err != nil {
		t.Fatalf("DeployAddon: %v", err)
	}

	_, err := a.RunPhysicalBackup(ctx, controlplane.DefaultEnvironment, testInstance(controlplane.DefaultEnvironment), "b1", testArchive(controlplane.DefaultEnvironment, 30))
	if !errors.Is(err, controlplane.ErrInvalid) {
		t.Fatalf("RunPhysicalBackup on a non-archiving instance = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "addon install postgres") {
		t.Errorf("the refusal must name the step that wires an existing instance: %v", err)
	}
	list, lerr := dyn.Resource(backupGVR).Namespace(cnpgTestNamespace).List(ctx, metav1.ListOptions{})
	if lerr != nil {
		t.Fatalf("listing Backup objects: %v", lerr)
	}
	if len(list.Items) != 0 {
		t.Errorf("%d Backup objects were written for a refused backup, want 0", len(list.Items))
	}
}

// TestRunPhysicalBackupRefusesAMissingInstance asserts an environment with no instance is
// ErrNotFound, not a confusing failure about a plugin.
func TestRunPhysicalBackupRefusesAMissingInstance(t *testing.T) {
	client, dyn := archivingCluster()
	a := kube.New(client, "burrow").WithDynamicClient(dyn)

	_, err := a.RunPhysicalBackup(context.Background(), controlplane.DefaultEnvironment, testInstance(controlplane.DefaultEnvironment), "b1", testArchive(controlplane.DefaultEnvironment, 30))
	if !errors.Is(err, controlplane.ErrNotFound) {
		t.Fatalf("RunPhysicalBackup with no instance = %v, want ErrNotFound", err)
	}
}

// TestPhysicalBackupPresent asserts the ADR-0074 §6 sweep's physical half, including the part that
// makes it work at all: a `Backup` object is owned by the `Cluster` and Burrow never deletes it, so
// mere EXISTENCE would report every pending row as still running for ever. A settled object is not
// something that will finish a pending row, and this asserts the phase is what decides.
func TestPhysicalBackupPresent(t *testing.T) {
	ctx := context.Background()
	a, dyn := archivingInstance(t)

	present, err := a.PhysicalBackupPresent(ctx, "b1")
	if err != nil || present {
		t.Fatalf("PhysicalBackupPresent for an absent object = (%v, %v), want (false, nil)", present, err)
	}
	settledBackup(t, dyn, "b1", map[string]any{"phase": "running"})
	present, err = a.PhysicalBackupPresent(ctx, "b1")
	if err != nil || !present {
		t.Fatalf("PhysicalBackupPresent for a live object = (%v, %v), want (true, nil)", present, err)
	}

	// A settled object outlives the backup by the life of the instance, so it must NOT keep a stranded
	// pending row looking like work in progress.
	settledBackup(t, dyn, "b2", map[string]any{"phase": "completed", "backupName": "20260801-020000F"})
	present, err = a.PhysicalBackupPresent(ctx, "b2")
	if err != nil || present {
		t.Fatalf("PhysicalBackupPresent for a completed object = (%v, %v), want (false, nil): a settled backup will never finish a pending row", present, err)
	}
}

// TestRunPhysicalBackupRefusesAMismatchedDestination asserts the instance's own `Stanza` is the
// authority on where its backups go, not the destination the caller resolved. They can differ — a
// second registered provider named on the command line, or a provider re-registered against a new
// bucket — and following the caller would verify a perfectly good backup against the wrong bucket
// and record the wrong provider on the row.
func TestRunPhysicalBackupRefusesAMismatchedDestination(t *testing.T) {
	ctx := context.Background()
	a, dyn := archivingInstance(t)
	settledBackup(t, dyn, "b1", map[string]any{"phase": "completed", "backupName": "20260801-020000F"})

	other := testArchive(controlplane.DefaultEnvironment, 30)
	other.Config.Bucket = "somebody-elses-bucket"
	_, err := a.RunPhysicalBackup(ctx, controlplane.DefaultEnvironment, testInstance(controlplane.DefaultEnvironment), "b1", other)
	if !errors.Is(err, controlplane.ErrInvalid) {
		t.Fatalf("RunPhysicalBackup against a mismatched destination = %v, want ErrInvalid", err)
	}
}
