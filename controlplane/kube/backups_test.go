// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"errors"
	"reflect"
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
	if _, err := a.RunBackupJob(ctx, "shop", controlplane.DefaultEnvironment, testInstance(controlplane.DefaultEnvironment), "bk1", nil); err != nil {
		t.Fatalf("RunBackupJob: %v", err)
	}

	// The backup PVC was ensured in the add-on namespace, labelled as the Postgres add-on's so
	// `addon list` can attribute it once the add-on is gone (ADR-0064 §6).
	pvc, err := client.CoreV1().PersistentVolumeClaims(addonNS).Get(ctx, controlplane.PostgresBackupVolume, metav1.GetOptions{})
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
	if err := a.RunRestoreJob(ctx, "shop", controlplane.DefaultEnvironment, testInstance(controlplane.DefaultEnvironment), "bk1"); err != nil {
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
	if _, err := a.RunBackupJob(ctx, "Bad_Name", controlplane.DefaultEnvironment, testInstance(controlplane.DefaultEnvironment), "bk1", nil); err == nil {
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
	if _, err := a.RunBackupJob(ctx, "shop", "staging", testInstance("staging"), "bk-staging", nil); err != nil {
		t.Fatalf("RunBackupJob(staging): %v", err)
	}
	if err := a.RunRestoreJob(ctx, "shop", "staging", testInstance("staging"), "bk-staging"); err != nil {
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
		if _, err := a.RunBackupJob(ctx, "shop", env, testInstance(env), "bk2", nil); !errors.Is(err, controlplane.ErrInvalid) {
			t.Errorf("RunBackupJob(shop, %q) err = %v, want ErrInvalid", env, err)
		}
		if err := a.RunRestoreJob(ctx, "shop", env, testInstance(env), "bk2"); !errors.Is(err, controlplane.ErrInvalid) {
			t.Errorf("RunRestoreJob(shop, %q) err = %v, want ErrInvalid", env, err)
		}
	}
	if len(created) != 2 {
		t.Errorf("a refused backup/restore still created a Job: %d jobs", len(created))
	}
}

// TestBackupJobNoPlatformPodMutatorUnchanged is ADR-0073 §4's obligation on the backup path: with no
// platform mutator wired, the Job the adapter authors is byte-for-byte what it was before the hook
// existed. The whole expected object is spelled out, so any accidental change to the backup pod's
// shape fails here rather than on a cluster.
func TestBackupJobNoPlatformPodMutatorUnchanged(t *testing.T) {
	a := New(fake.NewSimpleClientset(), "apps").WithAddonNamespace(addonNS)

	connEnv := []corev1.EnvVar{{Name: "PGHOST", Value: "burrow-postgres." + addonNS + ".svc"}}
	got := a.backupJob("burrow-pg-backup-bk1", controlplane.PostgresBackupVolume, "pg_dump -Fc", connEnv, nil, "")

	labels := map[string]string{
		nameLabel:      "burrow-pg-backup-bk1",
		managedByLabel: managedByValue,
		addonLabel:     string(controlplane.AddonPostgres),
	}
	var backoff int32
	want := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "burrow-pg-backup-bk1", Namespace: addonNS, Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:         "pg",
						Image:        backupImage,
						Command:      []string{"sh", "-c", "pg_dump -Fc"},
						Env:          connEnv,
						VolumeMounts: []corev1.VolumeMount{{Name: "backups", MountPath: backupMountPath}},
					}},
					Volumes: []corev1.Volume{{
						Name:         "backups",
						VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: controlplane.PostgresBackupVolume}},
					}},
				},
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("backup Job built with no platform mutator differs from the pre-hook output (ADR-0073 §4)\n got: %#v\nwant: %#v", got, want)
	}
}

// TestBackupAndRestoreJobsCarryThePlatformMutator covers both callers of the one shared builder. A
// backup Job that cannot schedule burns RunBackupJob's full timeout with Failed and Succeeded both
// zero, so it reports a timeout rather than an unschedulable pod; the restore is worse, because the
// operator finds out during an incident. Sharing the builder means the two necessarily share one
// placement policy, which is what this asserts.
func TestBackupAndRestoreJobsCarryThePlatformMutator(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	var created []*batchv1.Job
	succeedJobs(client, &created)

	tol := corev1.Toleration{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "platform", Effect: corev1.TaintEffectNoSchedule}
	a := New(client, "apps").WithAddonNamespace(addonNS).WithPlatformPodMutator(func(pod *corev1.PodSpec) {
		// Idempotent: replaces rather than appends, as a hook that runs on every write must.
		pod.Tolerations = []corev1.Toleration{tol}
		pod.RuntimeClassName = ptrTo("kata")
		pod.NodeSelector = map[string]string{"pool": "platform"}
	})

	if _, err := a.RunBackupJob(ctx, "shop", controlplane.DefaultEnvironment, testInstance(controlplane.DefaultEnvironment), "bk1", nil); err != nil {
		t.Fatalf("RunBackupJob: %v", err)
	}
	if err := a.RunRestoreJob(ctx, "shop", controlplane.DefaultEnvironment, testInstance(controlplane.DefaultEnvironment), "bk1"); err != nil {
		t.Fatalf("RunRestoreJob: %v", err)
	}
	if len(created) != 2 {
		t.Fatalf("created %d jobs, want 2 (backup and restore)", len(created))
	}
	for _, job := range created {
		pod := job.Spec.Template.Spec
		if len(pod.Tolerations) != 1 || pod.Tolerations[0] != tol {
			t.Errorf("job %q tolerations = %+v, want exactly %+v", job.Name, pod.Tolerations, tol)
		}
		if pod.RuntimeClassName == nil || *pod.RuntimeClassName != "kata" {
			t.Errorf("job %q runtimeClassName = %v, want kata", job.Name, pod.RuntimeClassName)
		}
		if pod.NodeSelector["pool"] != "platform" {
			t.Errorf("job %q nodeSelector = %v, want pool=platform", job.Name, pod.NodeSelector)
		}
		// The hook adjusts placement; it must not have cost the Job what it depends on.
		if pod.RestartPolicy != corev1.RestartPolicyNever {
			t.Errorf("job %q restart policy = %q, want Never", job.Name, pod.RestartPolicy)
		}
		if len(pod.Containers) != 1 || pod.Containers[0].Image != backupImage {
			t.Errorf("job %q containers = %+v, want the one postgres container", job.Name, pod.Containers)
		}
	}
}

// TestBackupJobPlatformMutatorSeesConstructedPodSpec pins the ordering (ADR-0073 §6): the hook runs
// over the FULLY-built pod spec, so a mutator can key its decision off the container and the mounted
// backup volume the engine composed. It also documents what the mutator author must expect to find
// already set — RestartPolicy Never — since overwriting it produces a Job the API server rejects.
func TestBackupJobPlatformMutatorSeesConstructedPodSpec(t *testing.T) {
	var sawContainers int
	var sawImage string
	var sawVolume bool
	var sawRestartPolicy corev1.RestartPolicy

	a := New(fake.NewSimpleClientset(), "apps").WithAddonNamespace(addonNS).WithPlatformPodMutator(func(pod *corev1.PodSpec) {
		sawContainers = len(pod.Containers)
		sawRestartPolicy = pod.RestartPolicy
		if len(pod.Containers) > 0 {
			sawImage = pod.Containers[0].Image
		}
		for _, v := range pod.Volumes {
			if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == controlplane.PostgresBackupVolume {
				sawVolume = true
			}
		}
	})
	a.backupJob("burrow-pg-backup-bk1", controlplane.PostgresBackupVolume, "pg_dump -Fc", nil, nil, "")

	if sawContainers != 1 {
		t.Errorf("mutator saw %d containers, want 1 — it ran before the pod was built", sawContainers)
	}
	if sawImage != backupImage {
		t.Errorf("mutator saw image %q, want %q", sawImage, backupImage)
	}
	if !sawVolume {
		t.Error("mutator did not see the mounted backup volume")
	}
	if sawRestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("mutator saw restart policy %q, want Never — a Job pod arrives with it already set", sawRestartPolicy)
	}
}

// TestBackupJobIgnoresAppPodMutator holds the classification (ADR-0073 §2): the backup Job runs
// Burrow's postgres image, not the app's, so it takes the platform hook and NOT the app one. An
// operator who sandboxed the tenant's image on tenant-only nodes did not ask for their pg_dump
// there, and a path wired to the wrong hook gives them policy they never intended.
func TestBackupJobIgnoresAppPodMutator(t *testing.T) {
	a := New(fake.NewSimpleClientset(), "apps").WithAddonNamespace(addonNS).
		WithPodMutator(func(pod *corev1.PodSpec) { pod.NodeSelector = map[string]string{"pool": "tenant"} })

	pod := a.backupJob("burrow-pg-backup-bk1", controlplane.PostgresBackupVolume, "pg_dump -Fc", nil, nil, "").Spec.Template.Spec
	if len(pod.NodeSelector) != 0 {
		t.Errorf("nodeSelector = %v, want none: the app hook must not reach a Burrow-image pod", pod.NodeSelector)
	}
}

// TestBackupJobPresent: the observer's read reports a Job that is there and one that is not, and it
// looks under the SAME name RunBackupJob creates — a backup row left pending by a burrowd that
// restarted is otherwise indistinguishable from a backup still running (ADR-0074 §6).
func TestBackupJobPresent(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset(&batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: backupJobName("bk1"), Namespace: addonNS},
	})
	a := New(client, "apps").WithAddonNamespace(addonNS)

	present, err := a.BackupJobPresent(ctx, "bk1")
	if err != nil || !present {
		t.Errorf("BackupJobPresent(bk1) = %v, %v; want true, nil", present, err)
	}
	// A Job that is gone is absent, not an error: absence is the answer, not a failure to ask.
	present, err = a.BackupJobPresent(ctx, "bk2")
	if err != nil || present {
		t.Errorf("BackupJobPresent(bk2) = %v, %v; want false, nil", present, err)
	}
}

// TestBackupAndRestoreMountTheirOwnEnvironmentsClaim asserts the Jobs of a NAMED environment create
// and mount that environment's backup claim — not the shared one, which holds every other
// environment's dumps (ADR-0067 §1, issue #349).
//
// The mount is where the isolation is real. The engine also refuses a restore whose row belongs to
// another environment, but a refusal on the record still leaves a Job that had the bytes mounted;
// this is the half that makes another environment's dumps unreachable rather than merely unasked
// for.
func TestBackupAndRestoreMountTheirOwnEnvironmentsClaim(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	var created []*batchv1.Job
	succeedJobs(client, &created)

	want, err := controlplane.BackupVolumeName(controlplane.AddonPostgres, testInstance("staging"))
	if err != nil {
		t.Fatalf("BackupVolumeName: %v", err)
	}
	a := New(client, "apps").WithAddonNamespace(addonNS)
	if _, err := a.RunBackupJob(ctx, "shop", "staging", testInstance("staging"), "bk1", nil); err != nil {
		t.Fatalf("RunBackupJob: %v", err)
	}
	if err := a.RunRestoreJob(ctx, "shop", "staging", testInstance("staging"), "bk1"); err != nil {
		t.Fatalf("RunRestoreJob: %v", err)
	}

	// Staging's claim was created, carrying the labels `addon list` attributes a retained claim by —
	// its name is no longer the compiled-in constant, so the role has to be written down.
	pvc, err := client.CoreV1().PersistentVolumeClaims(addonNS).Get(ctx, want, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("staging's backup PVC not created: %v", err)
	}
	if pvc.Labels[addonEnvLabel] != "staging" || pvc.Labels[addonVolumeRole] != controlplane.AddonVolumeBackup {
		t.Errorf("staging's backup PVC labels = %v, want %s=staging and %s=%s", pvc.Labels, addonEnvLabel, addonVolumeRole, controlplane.AddonVolumeBackup)
	}
	// And the shared claim was NOT: a backup taken in staging must not touch the volume holding the
	// default environment's dumps.
	if _, err := client.CoreV1().PersistentVolumeClaims(addonNS).Get(ctx, controlplane.PostgresBackupVolume, metav1.GetOptions{}); err == nil {
		t.Errorf("a backup taken in staging created the default environment's claim %q", controlplane.PostgresBackupVolume)
	}

	if len(created) != 2 {
		t.Fatalf("created %d jobs, want 2 (backup and restore)", len(created))
	}
	for _, job := range created {
		var claims []string
		for _, v := range job.Spec.Template.Spec.Volumes {
			if v.PersistentVolumeClaim != nil {
				claims = append(claims, v.PersistentVolumeClaim.ClaimName)
			}
		}
		if len(claims) != 1 || claims[0] != want {
			t.Errorf("job %q mounts claims %v, want only staging's own %q", job.Name, claims, want)
		}
	}
}

// TestAddonVolumeOwnerAttributesEachClaim asserts the retained-volume listing attributes a claim
// from its LABELS, so a named environment's backup claim is reported as dumps rather than as an
// add-on's data — and that the two label-less shapes an existing cluster holds are still read
// correctly (ADR-0064 §6).
func TestAddonVolumeOwnerAttributesEachClaim(t *testing.T) {
	staging, err := controlplane.BackupVolumeName(controlplane.AddonPostgres, "staging")
	if err != nil {
		t.Fatalf("BackupVolumeName: %v", err)
	}
	cases := []struct {
		name     string
		pvc      *corev1.PersistentVolumeClaim
		wantRole string
	}{
		{
			name: "a named environment's backup claim, by its role label",
			pvc: &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: staging, Labels: map[string]string{
				addonLabel: string(controlplane.AddonPostgres), addonVolumeRole: controlplane.AddonVolumeBackup,
			}}},
			wantRole: controlplane.AddonVolumeBackup,
		},
		{
			name: "an instance's data claim, by its role label",
			pvc: &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "burrow-postgres-staging", Labels: map[string]string{
				addonLabel: string(controlplane.AddonPostgres), addonVolumeRole: controlplane.AddonVolumeData,
			}}},
			wantRole: controlplane.AddonVolumeData,
		},
		{
			// Written before the role label existed: the compiled-in constant is what identifies it,
			// and it is a constant rather than a prefix guess about what a user might have named
			// something.
			name: "the shared backup claim an existing cluster holds",
			pvc: &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: controlplane.PostgresBackupVolume, Labels: map[string]string{
				addonLabel: string(controlplane.AddonPostgres),
			}}},
			wantRole: controlplane.AddonVolumeBackup,
		},
		{
			name: "a data claim written before the role label",
			pvc: &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "burrow-postgres", Labels: map[string]string{
				addonLabel: string(controlplane.AddonPostgres),
			}}},
			wantRole: controlplane.AddonVolumeData,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addon, role, ok := addonVolumeOwner(tc.pvc)
			if !ok {
				t.Fatalf("claim %q was not attributed to any add-on", tc.pvc.Name)
			}
			if addon != controlplane.AddonPostgres || role != tc.wantRole {
				t.Errorf("claim %q attributed to %s/%s, want postgres/%s", tc.pvc.Name, addon, role, tc.wantRole)
			}
		})
	}
	// A claim Burrow did not create is not attributed at all.
	if _, _, ok := addonVolumeOwner(&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "someone-elses"}}); ok {
		t.Error("a claim with no Burrow labels was attributed to an add-on")
	}
}

// TestRemoveBackupFileUnlinksTheRecordedDump asserts the removal Job mounts the claim the ROW named,
// unlinks the path the row named, and carries no database credential — it deletes a file and has no
// business reaching a Postgres server.
func TestRemoveBackupFileUnlinksTheRecordedDump(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset(&corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: controlplane.PostgresBackupVolume, Namespace: addonNS},
	})
	var created []*batchv1.Job
	succeedJobs(client, &created)

	a := New(client, "apps").WithAddonNamespace(addonNS)
	dump := controlplane.BackupPath("shop", "bk1")
	if err := a.RemoveBackupFile(ctx, "bk1", controlplane.PostgresBackupVolume, dump); err != nil {
		t.Fatalf("RemoveBackupFile: %v", err)
	}
	if len(created) != 1 {
		t.Fatalf("created %d jobs, want 1", len(created))
	}
	job := created[0]
	if job.Name != backupRemoveJobName("bk1") {
		t.Errorf("job name = %q, want %q — a removal must not be mistaken for the backup itself", job.Name, backupRemoveJobName("bk1"))
	}
	c := job.Spec.Template.Spec.Containers[0]
	cmd := strings.Join(c.Command, " ")
	if !strings.Contains(cmd, "rm -f") || !strings.Contains(cmd, dump) {
		t.Errorf("command does not unlink the recorded dump: %q", cmd)
	}
	if len(c.Env) != 0 {
		t.Errorf("the removal Job carries env %v, want none: it needs no database credential", c.Env)
	}
	claim := job.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim
	if claim == nil || claim.ClaimName != controlplane.PostgresBackupVolume {
		t.Errorf("job mounts %+v, want the claim the row named (%s)", claim, controlplane.PostgresBackupVolume)
	}
}

// TestRemoveBackupFileAbsentVolumeIsSuccess asserts a claim that is not there is success and builds
// no Job. Ensuring the claim first — the way backup and restore do — would create a 10Gi volume for
// every backup pruned off a claim that was already reclaimed.
func TestRemoveBackupFileAbsentVolumeIsSuccess(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	var created []*batchv1.Job
	succeedJobs(client, &created)

	a := New(client, "apps").WithAddonNamespace(addonNS)
	if err := a.RemoveBackupFile(ctx, "bk1", "burrow-postgres-staging-backups", controlplane.BackupPath("shop", "bk1")); err != nil {
		t.Fatalf("RemoveBackupFile with no claim = %v, want nil", err)
	}
	if len(created) != 0 {
		t.Errorf("created %d jobs, want 0", len(created))
	}
	if _, err := client.CoreV1().PersistentVolumeClaims(addonNS).Get(ctx, "burrow-postgres-staging-backups", metav1.GetOptions{}); err == nil {
		t.Error("removing a backup created the claim it was removing from")
	}
}

// TestRemoveBackupFileRefusesAPathOffTheLayout asserts the operand of a delete is checked rather
// than trusted: only <mount>/<app>/<id>.dump is accepted, so no row can send the Job at a file
// outside the layout the backup Job writes.
func TestRemoveBackupFileRefusesAPathOffTheLayout(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset(&corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: controlplane.PostgresBackupVolume, Namespace: addonNS},
	})
	var created []*batchv1.Job
	succeedJobs(client, &created)
	a := New(client, "apps").WithAddonNamespace(addonNS)

	for _, path := range []string{
		"/etc/passwd",
		backupMountPath + "/../etc/passwd",
		backupMountPath + "/shop/bk1.dump; rm -rf /",
		backupMountPath + "/shop/notadump",
		backupMountPath + "/shop/sub/bk1.dump",
	} {
		if err := a.RemoveBackupFile(ctx, "bk1", controlplane.PostgresBackupVolume, path); !errors.Is(err, controlplane.ErrInvalid) {
			t.Errorf("RemoveBackupFile(%q) = %v, want ErrInvalid", path, err)
		}
	}
	// A claim name off the alphabet is refused on the same grounds.
	if err := a.RemoveBackupFile(ctx, "bk1", "backups; rm -rf /", controlplane.BackupPath("shop", "bk1")); !errors.Is(err, controlplane.ErrInvalid) {
		t.Errorf("RemoveBackupFile with an invalid claim = %v, want ErrInvalid", err)
	}
	if len(created) != 0 {
		t.Errorf("created %d jobs for refused arguments, want 0", len(created))
	}
}
