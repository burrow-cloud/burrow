// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"errors"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/burrow-cloud/burrow/controlplane"
)

// succeedJobs installs reactors so a created Job is immediately observed succeeded, letting the
// blocking RunBackupJob/RunRestoreJob return in a unit test. It also captures every created Job so
// the test can assert the Job's spec. The captured Job is returned through created.
func succeedJobs(client *fake.Clientset, created *[]*batchv1.Job) {
	client.PrependReactor("create", "jobs", func(action clienttesting.Action) (bool, runtime.Object, error) {
		job := action.(clienttesting.CreateAction).GetObject().(*batchv1.Job)
		*created = append(*created, job.DeepCopy())
		return false, nil, nil // let the tracker store it too
	})
	client.PrependReactor("get", "jobs", func(action clienttesting.Action) (bool, runtime.Object, error) {
		name := action.(clienttesting.GetAction).GetName()
		return true, &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: addonNS},
			Status:     batchv1.JobStatus{Succeeded: 1},
		}, nil
	})
}

// TestRunBackupJobSpecAndSecretRef asserts RunBackupJob ensures the backup PVC, builds a Job in the
// add-on namespace running the postgres image, mounts the backup PVC, reads the superuser password
// ONLY via secretKeyRef (never an argv or env literal), pg_dumps in custom format, and names no
// password or connection string on the command line (ADR-0032).
func TestRunBackupJobSpecAndSecretRef(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	var created []*batchv1.Job
	succeedJobs(client, &created)

	a := New(client, "apps").WithAddonNamespace(addonNS)
	if _, err := a.RunBackupJob(ctx, "shop", controlplane.DefaultEnvironment, "bk1"); err != nil {
		t.Fatalf("RunBackupJob: %v", err)
	}

	// The backup PVC was ensured in the add-on namespace, labelled as the Postgres add-on's so
	// `addon list` can attribute it once the add-on is gone (ADR-0064 §6).
	pvc, err := client.CoreV1().PersistentVolumeClaims(addonNS).Get(ctx, backupPVCName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("backup PVC not created: %v", err)
	}
	if pvc.Labels[addonLabel] != string(controlplane.AddonPostgres) {
		t.Errorf("backup PVC labels = %v, want %s=postgres", pvc.Labels, addonLabel)
	}

	if len(created) != 1 {
		t.Fatalf("created %d jobs, want 1", len(created))
	}
	job := created[0]
	if job.Namespace != addonNS {
		t.Errorf("job namespace = %q, want %q", job.Namespace, addonNS)
	}
	c := job.Spec.Template.Spec.Containers[0]
	if c.Image != backupImage {
		t.Errorf("image = %q, want %q", c.Image, backupImage)
	}

	// The password reaches the container ONLY as a secretKeyRef env, pointing at the existing
	// superuser Secret and key — never an env literal, never an argv.
	var pgpassword *corev1.EnvVar
	for i := range c.Env {
		if c.Env[i].Name == "PGPASSWORD" {
			pgpassword = &c.Env[i]
		}
		if c.Env[i].Value != "" && looksLikePassword(c.Env[i].Value) {
			t.Errorf("env %q carries a literal that looks like a password: %q", c.Env[i].Name, c.Env[i].Value)
		}
	}
	if pgpassword == nil || pgpassword.ValueFrom == nil || pgpassword.ValueFrom.SecretKeyRef == nil {
		t.Fatal("PGPASSWORD must come from a secretKeyRef")
	}
	if pgpassword.Value != "" {
		t.Errorf("PGPASSWORD must have no inline value, got %q", pgpassword.Value)
	}
	if ref := pgpassword.ValueFrom.SecretKeyRef; ref.Name != PostgresSecretName || ref.Key != PostgresPasswordKey {
		t.Errorf("PGPASSWORD secretKeyRef = %s/%s, want %s/%s", ref.Name, ref.Key, PostgresSecretName, PostgresPasswordKey)
	}

	// The command pg_dumps in custom format to the on-PVC path and names no password/host on argv.
	cmd := strings.Join(c.Command, " ")
	if !strings.Contains(cmd, "pg_dump -Fc") {
		t.Errorf("command does not pg_dump in custom format: %q", cmd)
	}
	if !strings.Contains(cmd, controlplane.BackupPath("shop", "bk1")) {
		t.Errorf("command does not write the on-PVC dump path: %q", cmd)
	}
	if strings.Contains(cmd, "PGPASSWORD") || strings.Contains(cmd, "password=") || strings.Contains(cmd, "postgres://") {
		t.Errorf("command names a password or connection string on argv: %q", cmd)
	}

	// The backup PVC is mounted.
	var mounted bool
	for _, m := range c.VolumeMounts {
		if m.MountPath == backupMountPath {
			mounted = true
		}
	}
	if !mounted {
		t.Errorf("backup PVC not mounted at %q", backupMountPath)
	}
}

// TestRunRestoreJobSpec asserts RunRestoreJob builds a pg_restore Job with --clean --if-exists and
// the same secretKeyRef-only password handling, naming no credential on argv (ADR-0032).
func TestRunRestoreJobSpec(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	var created []*batchv1.Job
	succeedJobs(client, &created)

	a := New(client, "apps").WithAddonNamespace(addonNS)
	if err := a.RunRestoreJob(ctx, "shop", controlplane.DefaultEnvironment, "bk1"); err != nil {
		t.Fatalf("RunRestoreJob: %v", err)
	}
	if len(created) != 1 {
		t.Fatalf("created %d jobs, want 1", len(created))
	}
	c := created[0].Spec.Template.Spec.Containers[0]
	cmd := strings.Join(c.Command, " ")
	if !strings.Contains(cmd, "pg_restore --clean --if-exists") {
		t.Errorf("command does not pg_restore --clean --if-exists: %q", cmd)
	}
	if strings.Contains(cmd, "postgres://") || strings.Contains(cmd, "password=") {
		t.Errorf("restore command names a credential on argv: %q", cmd)
	}
	// Password still only via secretKeyRef.
	for _, e := range c.Env {
		if e.Name == "PGPASSWORD" && (e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil) {
			t.Error("restore PGPASSWORD must come from a secretKeyRef")
		}
	}
}

// TestRunBackupJobRejectsBadApp asserts a bad app identifier is rejected before any Job is built.
func TestRunBackupJobRejectsBadApp(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := New(client, "apps").WithAddonNamespace(addonNS)
	if _, err := a.RunBackupJob(ctx, "Bad_Name", controlplane.DefaultEnvironment, "bk1"); err == nil {
		t.Error("RunBackupJob should reject a bad app identifier")
	}
	jobs, _ := client.BatchV1().Jobs(addonNS).List(ctx, metav1.ListOptions{})
	if len(jobs.Items) != 0 {
		t.Errorf("no Job should be created for a bad app, got %d", len(jobs.Items))
	}
}

// looksLikePassword is a coarse guard for the test: a base64url-ish token of meaningful length that
// is not one of the known non-secret env literals.
func looksLikePassword(v string) bool {
	switch v {
	case PostgresSuperuser, "5432", "shop":
		return false
	}
	return len(v) >= 20 && !strings.ContainsAny(v, "./: ")
}

// TestBackupJobTargetsTheEnvironmentsInstance asserts a dump is taken from the named environment's
// own server: the Job dials that instance's Service and reads that instance's superuser Secret, and
// an unnamed or malformed environment is refused before any Job exists (ADR-0067 §1). A backup that
// silently read the wrong instance would produce a dump labelled for one environment holding
// another's data — and a later restore would then write it into a live database.
func TestBackupJobTargetsTheEnvironmentsInstance(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	var created []*batchv1.Job
	succeedJobs(client, &created)

	a := New(client, "apps").WithAddonNamespace(addonNS)
	if _, err := a.RunBackupJob(ctx, "shop", "staging", "bk-staging"); err != nil {
		t.Fatalf("RunBackupJob(staging): %v", err)
	}
	if err := a.RunRestoreJob(ctx, "shop", "staging", "bk-staging"); err != nil {
		t.Fatalf("RunRestoreJob(staging): %v", err)
	}
	if len(created) != 2 {
		t.Fatalf("created %d jobs, want 2", len(created))
	}

	for _, job := range created {
		c := job.Spec.Template.Spec.Containers[0]
		var host string
		var ref string
		for _, ev := range c.Env {
			switch {
			case ev.Name == "PGHOST":
				host = ev.Value
			case ev.Name == "PGPASSWORD" && ev.ValueFrom != nil && ev.ValueFrom.SecretKeyRef != nil:
				ref = ev.ValueFrom.SecretKeyRef.Name
			}
		}
		if host != "burrow-postgres-staging."+addonNS+".svc" {
			t.Errorf("job %q dials PGHOST %q, want staging's own instance", job.Name, host)
		}
		if ref != "burrow-postgres-staging" {
			t.Errorf("job %q reads the superuser password from Secret %q, want staging's own", job.Name, ref)
		}
	}

	// No environment, no Job: the instance is settled before anything is created.
	for _, env := range []string{"", "Staging"} {
		if _, err := a.RunBackupJob(ctx, "shop", env, "bk2"); !errors.Is(err, controlplane.ErrInvalid) {
			t.Errorf("RunBackupJob(shop, %q) err = %v, want ErrInvalid", env, err)
		}
		if err := a.RunRestoreJob(ctx, "shop", env, "bk2"); !errors.Is(err, controlplane.ErrInvalid) {
			t.Errorf("RunRestoreJob(shop, %q) err = %v, want ErrInvalid", env, err)
		}
	}
	if len(created) != 2 {
		t.Errorf("a refused backup/restore still created a Job: %d jobs", len(created))
	}
}
