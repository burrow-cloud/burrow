// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/burrow-cloud/burrow/controlplane"
)

const (
	// backupPVCName is the ReadWriteOnce volume in the add-on namespace that holds dump bytes
	// (ADR-0032). Backup/restore Jobs mount it; the instance pod does not. burrowd is never mounted
	// to it — the list of backups comes from the control-plane database, not this volume.
	backupPVCName = "burrow-postgres-backups"
	// backupMountPath is where the backup PVC is mounted inside the backup/restore Job container.
	// It mirrors controlplane.BackupPath's prefix so the Job writes where the engine records.
	backupMountPath = "/backups"
	// backupPVCSizeGi is the backup volume's requested size.
	backupPVCSizeGi = 10
	// backupImage is the image the backup/restore Jobs run: the official postgres image carries
	// pg_dump and pg_restore. It matches the add-on instance image (ADR-0031/0032) and is already on
	// the CI preload list, so e2e adds no new image.
	backupImage = "postgres:17-alpine"
	// backupJobTimeout caps how long burrowd waits for a backup/restore Job to complete.
	backupJobTimeout = 10 * time.Minute
	// backupJobPoll is the interval between Job-status reads while waiting.
	backupJobPoll = 2 * time.Second
)

// backupDumpPath is the on-PVC path of app's dump for backupID — the same layout the engine records
// via controlplane.BackupPath, so the Job writes where burrowd records.
func backupDumpPath(app, backupID string) string {
	return controlplane.BackupPath(app, backupID)
}

// ensureBackupPVC creates the backup PVC in the add-on namespace if absent (idempotent, like the
// add-on data PVC). It is created on first backup so a cluster that never backs up carries no extra
// volume.
func (a *Adapter) ensureBackupPVC(ctx context.Context) error {
	labels := map[string]string{nameLabel: backupPVCName, managedByLabel: managedByValue}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: backupPVCName, Namespace: a.addonNamespace, Labels: labels},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(fmt.Sprintf("%dGi", backupPVCSizeGi))},
			},
		},
	}
	if _, err := a.client.CoreV1().PersistentVolumeClaims(a.addonNamespace).Create(ctx, pvc, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("kube: creating backup volume %q: %w", backupPVCName, err)
	}
	return nil
}

// backupInstanceHost is the in-cluster host:port the backup/restore Jobs dial: the add-on Postgres
// Service. The Jobs run in-cluster, so they resolve the .svc name directly (no port-forward).
func (a *Adapter) backupInstanceHost() string {
	return fmt.Sprintf("%s.%s.svc", PostgresSecretName, a.addonNamespace)
}

// superuserPasswordEnv wires PGPASSWORD from the burrow-postgres Secret via secretKeyRef (ADR-0032).
// libpq reads PGPASSWORD, so pg_dump/pg_restore authenticate with the superuser password WITHOUT it
// ever appearing in an argv or a log — it is injected by Kubernetes into the container env from the
// existing Secret, exactly as the provisioner reads it.
func superuserPasswordEnv() corev1.EnvVar {
	return corev1.EnvVar{
		Name: "PGPASSWORD",
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: PostgresSecretName},
				Key:                  PostgresPasswordKey,
			},
		},
	}
}

// backupConnEnv is the connection env shared by the backup and restore Jobs: the host, port, user,
// and database — all non-secret. The password is added separately via secretKeyRef. PGHOST/PGPORT/
// PGUSER/PGDATABASE are libpq variables, so pg_dump/pg_restore need no host or credential on the
// command line.
func (a *Adapter) backupConnEnv(app string) []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "PGHOST", Value: a.backupInstanceHost()},
		{Name: "PGPORT", Value: "5432"},
		{Name: "PGUSER", Value: PostgresSuperuser},
		{Name: "PGDATABASE", Value: app},
		superuserPasswordEnv(),
	}
}

// RunBackupJob pg_dumps app's database to the backup PVC via a one-shot Job and waits for it to
// finish (ADR-0032). The dump command names no host or password — those come from libpq env, the
// password via secretKeyRef. The container also writes the dump's byte size to the termination-log
// so burrowd can read size_bytes from the pod's terminated state without mounting the volume.
func (a *Adapter) RunBackupJob(ctx context.Context, app, backupID string) (int64, error) {
	if err := validateAppIdentifier(app); err != nil {
		return 0, err
	}
	if err := a.ensureBackupPVC(ctx); err != nil {
		return 0, err
	}
	dump := backupDumpPath(app, backupID)
	dir := fmt.Sprintf("%s/%s", backupMountPath, app)
	// -Fc is the custom (compressed, restorable) format. The size is echoed to the termination-log
	// so burrowd reads it from the pod's terminated-state message; nothing here logs the password.
	script := fmt.Sprintf(`set -e
mkdir -p %q
pg_dump -Fc -f %q
stat -c%%s %q > /dev/termination-log`, dir, dump, dump)

	name := fmt.Sprintf("burrow-pg-backup-%s", backupID)
	job := a.backupJob(name, app, script)
	return a.runJobAwaitSize(ctx, job, name)
}

// RunRestoreJob pg_restores app's dump from the backup PVC into its database via a one-shot Job and
// waits for it (ADR-0032). --clean --if-exists replaces current contents. Like backup, the command
// names no host or password.
func (a *Adapter) RunRestoreJob(ctx context.Context, app, backupID string) error {
	if err := validateAppIdentifier(app); err != nil {
		return err
	}
	dump := backupDumpPath(app, backupID)
	// No --no-owner: the dump records the app role as the owner of the app's objects, and the Job
	// connects as the burrow_admin superuser, so pg_restore reassigns ownership back to the app role
	// (app_<app>). That matters because the app connects as that role — were the restored objects
	// left owned by burrow_admin, the app would lose access to its own tables after a restore.
	// --clean --if-exists replaces current contents idempotently.
	script := fmt.Sprintf(`set -e
pg_restore --clean --if-exists -d %q %q`, app, dump)

	name := fmt.Sprintf("burrow-pg-restore-%s", backupID)
	job := a.backupJob(name, app, script)
	_, err := a.runJobAwaitSize(ctx, job, name)
	return err
}

// backupJob builds a one-shot Job in the add-on namespace running the postgres image with script,
// mounting the backup PVC and the connection env (host/user/db non-secret, password via
// secretKeyRef). BackoffLimit 0: a failed attempt fails the Job rather than retrying forever.
func (a *Adapter) backupJob(name, app, script string) *batchv1.Job {
	labels := map[string]string{nameLabel: name, managedByLabel: managedByValue, addonLabel: string(controlplane.AddonPostgres)}
	var backoff int32
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: a.addonNamespace, Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:    "pg",
						Image:   backupImage,
						Command: []string{"sh", "-c", script},
						Env:     a.backupConnEnv(app),
						VolumeMounts: []corev1.VolumeMount{
							{Name: "backups", MountPath: backupMountPath},
						},
					}},
					Volumes: []corev1.Volume{{
						Name:         "backups",
						VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: backupPVCName}},
					}},
				},
			},
		},
	}
}

// runJobAwaitSize creates the Job, polls until it succeeds or fails (or times out), and on success
// reaps it and returns the byte size the container wrote to its termination-log (0 when absent or
// unparsable). On failure it returns an error WITHOUT deleting the Job, so its pod logs remain for
// diagnosis.
func (a *Adapter) runJobAwaitSize(ctx context.Context, job *batchv1.Job, name string) (int64, error) {
	jobs := a.client.BatchV1().Jobs(a.addonNamespace)
	if _, err := jobs.Create(ctx, job, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return 0, fmt.Errorf("kube: creating job %q: %w", name, err)
	}

	deadline := time.Now().Add(backupJobTimeout)
	for {
		j, err := jobs.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return 0, fmt.Errorf("kube: reading job %q: %w", name, err)
		}
		if j.Status.Failed > 0 {
			// Leave the Job (and its pod logs) for diagnosis; do not reap a failure.
			return 0, fmt.Errorf("kube: job %q failed", name)
		}
		if j.Status.Succeeded > 0 {
			size := a.jobTerminationSize(ctx, name)
			// Reap on success: delete the Job and its pods so they do not accumulate.
			policy := metav1.DeletePropagationBackground
			_ = jobs.Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &policy})
			return size, nil
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("kube: job %q did not complete within %s", name, backupJobTimeout)
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(backupJobPoll):
		}
	}
}

// jobTerminationSize reads the byte size the backup container wrote to /dev/termination-log from the
// terminated container's state message. Best-effort: any miss or parse failure yields 0 (size
// unknown), never an error — a missing size must not fail a successful backup.
func (a *Adapter) jobTerminationSize(ctx context.Context, jobName string) int64 {
	pods, err := a.client.CoreV1().Pods(a.addonNamespace).List(ctx, metav1.ListOptions{LabelSelector: nameLabel + "=" + jobName})
	if err != nil || len(pods.Items) == 0 {
		return 0
	}
	for _, pod := range pods.Items {
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.State.Terminated == nil {
				continue
			}
			var size int64
			if _, err := fmt.Sscanf(cs.State.Terminated.Message, "%d", &size); err == nil && size > 0 {
				return size
			}
		}
	}
	return 0
}
