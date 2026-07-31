// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/burrow-cloud/burrow/controlplane"
)

const (
	// backupMountPath is where the backup PVC is mounted inside the backup/restore Job container.
	// It mirrors controlplane.BackupPath's prefix so the Job writes where the engine records.
	backupMountPath = "/backups"
	// backupPVCSizeGi is the backup volume's requested size.
	backupPVCSizeGi = 10
	// backupImage is the image the backup/restore Jobs run: the official postgres image carries
	// pg_dump and pg_restore. It matches the add-on instance image (ADR-0031/0032) and is already on
	// the CI preload list, so e2e adds no new image.
	backupImage = "postgres:17-alpine"
	// backupJobTimeout caps how long burrowd waits for a backup/restore Job to complete. It is
	// declared in controlplane/apiwait.go rather than here, because a client's own bound has to
	// outlast it (issue #404).
	backupJobTimeout = controlplane.BackupJobTimeout
	// backupJobPoll is the interval between Job-status reads while waiting.
	backupJobPoll = 2 * time.Second

	// backupDumpContainer and backupShipContainer are the two halves of a backup that has a durable
	// destination (ADR-0063 §7). The dump runs FIRST, as an init container, so the shipping step can
	// be an ordinary container that starts only once there is something to ship; a pod whose dump
	// failed never reaches the ship step at all, which is what keeps "the store was not asked" and
	// "the store said no" from being reported as the same failure.
	backupDumpContainer = "pg"
	backupShipContainer = "ship"

	// shipperImageRepo is the repository the shipping container's image comes from: burrowd's own.
	// The shipping step is an S3 PUT, a read-back, and a retry loop over both, all of which already
	// exist in this module — so it runs the SAME binary as the control plane under a different
	// subcommand rather than adding an image, a vendor CLI, or a shell reimplementation of SigV4.
	// That also means the shipper cannot drift from the adapter the configuration-time probe used:
	// a destination that verified at `provider add` is verified by the same code that writes to it.
	shipperImageRepo = "ghcr.io/burrow-cloud/burrowd"
	// defaultShipperImage is the floating tag a dev or e2e build falls back to; a released burrowd
	// pins its OWN version instead (ShipperImageForVersion), so the shipper is the binary that was
	// released alongside it.
	defaultShipperImage = shipperImageRepo + ":latest"

	// objectStoreCredsPath is where the destination credential PAIR is mounted into the shipping
	// container, as files in a tmpfs-backed Secret volume. Files, not env vars, and not command-line
	// arguments: an env var is visible in the Job spec to anything that can read Jobs in the
	// namespace, and an argument is visible in the process table. This is ADR-0057 §4's shape for a
	// build's source token, applied to the more consequential key of the two (ADR-0063).
	objectStoreCredsPath = "/objectstore-creds"
	// objectStoreAccessKeyIDFile / objectStoreSecretAccessKeyFile are the filenames inside it.
	objectStoreAccessKeyIDFile     = "access-key-id"
	objectStoreSecretAccessKeyFile = "secret-access-key"
)

// ShipperImageForVersion returns the pinned shipper image for a stamped release version, so a
// released burrowd ships backups with the binary published under the SAME release tag. For an
// unstamped dev build (version "" or "v0.0.0") it returns "" — there is no published image at a
// pseudo-version — and the caller leaves the :latest default, or an explicit BURROW_SHIPPER_IMAGE
// override, in place.
func ShipperImageForVersion(version string) string {
	if version == "" || version == "v0.0.0" {
		return ""
	}
	return shipperImageRepo + ":" + version
}

// backupDumpPath is the path of app's dump for backupID WITHIN its environment's claim — the same
// layout the engine records via controlplane.BackupPath, so the Job writes where burrowd records.
// The environment is carried by the CLAIM the Job mounts (backupVolumeName), not by this path, which
// is why the layout is unchanged and every dump taken before backups were per-environment is still
// exactly where its row says it is.
func backupDumpPath(app, backupID string) string {
	return controlplane.BackupPath(app, backupID)
}

// backupVolumeName is the claim environment env's dumps live on — one per environment, the same
// shape as the instance they came from (ADR-0067 §1, controlplane.BackupVolumeName). It is derived
// in the controlplane package so the engine's recorded row and the Job's mount cannot disagree about
// which disk a dump is on.
func backupVolumeName(env string) (string, error) {
	return controlplane.BackupVolumeName(controlplane.AddonPostgres, env)
}

// ensureBackupPVC creates environment env's backup PVC in the add-on namespace if absent
// (idempotent, like the add-on data PVC). It is created on first backup so a cluster that never
// backs up carries no extra volume — and an environment that never backs up carries none either.
func (a *Adapter) ensureBackupPVC(ctx context.Context, env, backupPVCName string) error {
	// The add-on label records which add-on this claim serves and the role label what it holds, so
	// `addon list` can attribute a retained claim without reading its name (ADR-0064 §6) — which now
	// matters, because a named environment's backup claim is no longer the one compiled-in name.
	// Claims created before these labels were written are still attributed, by that constant name
	// (addonVolumeOwner). The environment label matches the one the instance carries.
	labels := map[string]string{
		nameLabel:       backupPVCName,
		managedByLabel:  managedByValue,
		addonLabel:      string(controlplane.AddonPostgres),
		addonEnvLabel:   env,
		addonVolumeRole: controlplane.AddonVolumeBackup,
	}
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

// backupInstanceHost is the in-cluster host the backup/restore Jobs dial for environment env: that
// environment's Postgres Service (ADR-0067 §1). The Jobs run in-cluster, so they resolve the .svc
// name directly (no port-forward). A dump is only meaningful against the server it came from, so the
// environment travels with the Job exactly as it does with the provisioner.
func (a *Adapter) backupInstanceHost(env string) (string, error) {
	instance, err := postgresSecretName(env)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s.%s.svc", instance, a.addonNamespace), nil
}

// superuserPasswordEnv wires PGPASSWORD from the instance's Secret via secretKeyRef (ADR-0032).
// libpq reads PGPASSWORD, so pg_dump/pg_restore authenticate with the superuser password WITHOUT it
// ever appearing in an argv or a log — it is injected by Kubernetes into the container env from the
// existing Secret, exactly as the provisioner reads it. Each environment's instance has its own
// Secret, named after the instance (ADR-0067 §1), so the Job is given the credential for the one
// server it is meant to talk to.
func superuserPasswordEnv(instance string) corev1.EnvVar {
	return corev1.EnvVar{
		Name: "PGPASSWORD",
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: instance},
				Key:                  PostgresPasswordKey,
			},
		},
	}
}

// backupConnEnv is the connection env shared by the backup and restore Jobs for environment env: the
// host, port, user, and database — all non-secret. The password is added separately via
// secretKeyRef. PGHOST/PGPORT/PGUSER/PGDATABASE are libpq variables, so pg_dump/pg_restore need no
// host or credential on the command line.
func (a *Adapter) backupConnEnv(app, env string) ([]corev1.EnvVar, error) {
	host, err := a.backupInstanceHost(env)
	if err != nil {
		return nil, err
	}
	instance, err := postgresSecretName(env)
	if err != nil {
		return nil, err
	}
	return []corev1.EnvVar{
		{Name: "PGHOST", Value: host},
		{Name: "PGPORT", Value: "5432"},
		{Name: "PGUSER", Value: PostgresSuperuser},
		{Name: "PGDATABASE", Value: app},
		superuserPasswordEnv(instance),
	}, nil
}

// RunBackupJob pg_dumps app's database to the backup PVC via a one-shot Job and waits for it to
// finish (ADR-0032), and — when dest is non-nil — writes that dump on to the object store and reads
// it back before the Job is allowed to succeed (ADR-0063 §7). The dump command names no host or
// password: those come from libpq env, the password via secretKeyRef.
//
// The two destinations are a TIER, not alternatives. The PVC is where pg_dump writes and where
// pg_restore reads, and ADR-0066 §4 keeps that single-app logical path deliberately; the object
// store is what takes the backup out of the database's failure domain, which is why ADR-0063 exists.
// A dump therefore always lands on the volume, and the Job's SUCCESS means whichever destination the
// caller resolved was actually reached — so the Backup row the caller writes from this outcome is
// never a claim about bytes that stayed in the cluster.
//
// A failure returns the closed reason the Job reported alongside the error, so the caller records
// WHY rather than that it failed: the dump itself, a store that would not answer, a store that
// answered and refused, or a store that took the write and could not serve it back.
func (a *Adapter) RunBackupJob(ctx context.Context, app, env, backupID string, dest *controlplane.BackupDestination) (controlplane.BackupJobOutcome, error) {
	if err := validateAppIdentifier(app); err != nil {
		return controlplane.BackupJobOutcome{}, err
	}
	// The environment is resolved to an instance before the PVC is touched or a Job is built, so a
	// dump can never be taken from a server other than the named environment's (ADR-0067 §1).
	connEnv, err := a.backupConnEnv(app, env)
	if err != nil {
		return controlplane.BackupJobOutcome{}, err
	}
	// The claim is this environment's, resolved from the same environment that chose the instance —
	// so a dump taken from staging's server can only land on staging's disk (ADR-0067 §1).
	claim, err := backupVolumeName(env)
	if err != nil {
		return controlplane.BackupJobOutcome{}, err
	}
	if err := a.ensureBackupPVC(ctx, env, claim); err != nil {
		return controlplane.BackupJobOutcome{}, err
	}
	dump := backupDumpPath(app, backupID)
	dir := fmt.Sprintf("%s/%s", backupMountPath, app)
	// -Fc is the custom (compressed, restorable) format. The size is echoed to the termination-log
	// so burrowd reads it from the pod's terminated-state message; nothing here logs the password.
	script := fmt.Sprintf(`set -e
mkdir -p %q
pg_dump -Fc -f %q
stat -c%%s %q > /dev/termination-log`, dir, dump, dump)

	name := backupJobName(backupID)
	job := a.backupJob(name, claim, script, connEnv, dest, dump)
	return a.runBackupJobAwait(ctx, job, name, dest)
}

// backupJobName is the Job name for a backup id. It is one function rather than a format string at
// each use so the name the observer looks for cannot drift from the name the backup creates.
func backupJobName(backupID string) string { return "burrow-pg-backup-" + backupID }

// BackupJobPresent reports whether the Job for a backup id still exists (ADR-0074 §6). It is a plain
// read, and it answers the one question the registry cannot: a row left `pending` by a burrowd that
// restarted mid-backup looks exactly like a backup still running, and the difference is otherwise
// discovered at restore time. A missing Job is absent (false, nil), not an error.
func (a *Adapter) BackupJobPresent(ctx context.Context, backupID string) (bool, error) {
	_, err := a.client.BatchV1().Jobs(a.addonNamespace).Get(ctx, backupJobName(backupID), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("kube: reading backup job for %q: %w", backupID, err)
	}
	return true, nil
}

// RunRestoreJob pg_restores app's dump from the backup PVC into its database via a one-shot Job and
// waits for it (ADR-0032). --clean --if-exists replaces current contents. Like backup, the command
// names no host or password.
func (a *Adapter) RunRestoreJob(ctx context.Context, app, env, backupID string) error {
	if err := validateAppIdentifier(app); err != nil {
		return err
	}
	connEnv, err := a.backupConnEnv(app, env)
	if err != nil {
		return err
	}
	// The restore Job mounts THIS environment's backup claim and no other, so a dump belonging to
	// another environment is not merely refused by the caller — it is not on a disk this Job can
	// read (ADR-0067 §1).
	claim, err := backupVolumeName(env)
	if err != nil {
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
	job := a.backupJob(name, claim, script, connEnv, nil, "")
	_, rerr := a.runJobAwaitSize(ctx, job, name)
	return rerr
}

// backupJob builds a one-shot Job in the add-on namespace running the postgres image with script,
// mounting the named backup claim and the caller-resolved connection env (host/user/db non-secret,
// password via secretKeyRef). The environment is resolved by the caller into BOTH the instance the
// Job dials and the claim it mounts, so both are settled before any Job exists (ADR-0067 §1) — and a
// Job can only ever reach one environment's server and one environment's dumps. BackoffLimit 0: a
// failed attempt fails the Job rather than retrying forever.
//
// The pod runs the postgres image — Burrow's choice, not the app's — so it takes the PLATFORM hook
// (ADR-0073 §2), not the app one. Putting a pg_dump on the tenant pool is not what an operator who
// sandboxed the tenant's image asked for. Both callers build their Job here, so backup and restore
// necessarily share one placement policy; that matters most for restore, where an unschedulable Job
// is discovered during an incident.
func (a *Adapter) backupJob(name, claim, script string, connEnv []corev1.EnvVar, dest *controlplane.BackupDestination, dumpPath string) *batchv1.Job {
	labels := map[string]string{nameLabel: name, managedByLabel: managedByValue, addonLabel: string(controlplane.AddonPostgres)}
	var backoff int32
	pg := corev1.Container{
		Name:    backupDumpContainer,
		Image:   backupImage,
		Command: []string{"sh", "-c", script},
		Env:     connEnv,
		VolumeMounts: []corev1.VolumeMount{
			{Name: "backups", MountPath: backupMountPath},
		},
	}
	volumes := []corev1.Volume{{
		Name:         "backups",
		VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim}},
	}}
	containers := []corev1.Container{pg}
	var initContainers []corev1.Container

	// With a durable destination the dump becomes the FIRST step rather than the whole Job: the pg
	// container moves to initContainers and the shipping container becomes the one whose success is
	// the Job's success. That ordering is what makes the Job's terminal state mean what the caller
	// needs it to mean — a Job that succeeded is a backup that reached the store, and there is no
	// arrangement of these two steps where it succeeds because only the first one did.
	if dest != nil {
		initContainers = []corev1.Container{pg}
		containers = []corev1.Container{a.shipContainer(dest, dumpPath)}
		volumes = append(volumes, corev1.Volume{
			Name: "objectstore-creds",
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: backupCredSecretName(name),
			}},
		})
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: a.addonNamespace, Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:  corev1.RestartPolicyNever,
					InitContainers: initContainers,
					Containers:     containers,
					Volumes:        volumes,
				},
			},
		},
	}
	// Applied last, over the fully-constructed pod spec, so the hook can key off the container and
	// volume the engine composed. The mutator must leave RestartPolicy Never alone: it is already
	// set above, and a Job whose pod restarts is rejected by the API server. No mutator leaves the
	// Job exactly as built (ADR-0073 §4).
	a.applyPlatformPodMutator(&job.Spec.Template.Spec)
	return job
}

// runJobAwaitSize creates the Job, waits for it, and on success reaps it and returns the byte size
// the container wrote to its termination-log (0 when absent or unparsable). On failure it returns an
// error WITHOUT deleting the Job, so its pod logs remain for diagnosis.
//
// The wait is awaitJob's, so a backup or restore whose pod cannot start — a missing superuser
// Secret, an unpullable postgres image, an add-on namespace with no room — fails in seconds naming
// the fix, instead of burning ten minutes and reporting elapsed time (issue #352). That matters most
// for RESTORE, which is run during an incident by someone who has already lost something.
func (a *Adapter) runJobAwaitSize(ctx context.Context, job *batchv1.Job, name string) (int64, error) {
	jobs := a.client.BatchV1().Jobs(a.addonNamespace)
	if _, err := jobs.Create(ctx, job, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return 0, fmt.Errorf("kube: creating job %q: %w", name, err)
	}

	j, err := awaitJob(ctx, a.client, a.addonNamespace, name, backupJobTimeout, backupJobPoll, unschedulableGrace(ctx, a.limits))
	if err != nil {
		return 0, err
	}
	if j.Status.Failed > 0 {
		// Leave the Job (and its pod logs) for diagnosis; do not reap a failure.
		return 0, fmt.Errorf("kube: job %q failed", name)
	}
	size := a.jobTerminationSize(ctx, name)
	// Reap on success: delete the Job and its pods so they do not accumulate.
	policy := metav1.DeletePropagationBackground
	_ = jobs.Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &policy})
	return size, nil
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

// shipContainer is the second half of a backup with a durable destination: burrowd's own image,
// running the `ship-backup` subcommand over the dump the init container just wrote (ADR-0063 §7).
//
// Everything it needs that is NOT secret travels as env: the endpoint, region, bucket and key are
// configuration, and putting them in the Job spec is what makes a backup's destination legible from
// `kubectl describe job` without reading a Secret. The credential pair travels the other way, as
// files in a mounted Secret volume, because a Job's env is readable by anything that can read Jobs
// in the namespace and this is the key that grants write access to every backup Burrow holds.
//
// The PVC is mounted READ-ONLY here. The shipper's job is to read the dump and put it somewhere
// else; it has no business writing to the volume holding every other backup, and saying so in the
// mount is cheaper than trusting it not to.
func (a *Adapter) shipContainer(dest *controlplane.BackupDestination, dumpPath string) corev1.Container {
	return corev1.Container{
		Name:    backupShipContainer,
		Image:   a.shipImage(),
		Command: []string{"/burrowd", "ship-backup"},
		Env: []corev1.EnvVar{
			{Name: "BURROW_SHIP_ENDPOINT", Value: dest.Config.Endpoint},
			{Name: "BURROW_SHIP_REGION", Value: dest.Config.Region},
			{Name: "BURROW_SHIP_BUCKET", Value: dest.Config.Bucket},
			{Name: "BURROW_SHIP_KEY", Value: dest.Key},
			{Name: "BURROW_SHIP_FILE", Value: dumpPath},
			{Name: "BURROW_SHIP_CREDENTIALS_DIR", Value: objectStoreCredsPath},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "backups", MountPath: backupMountPath, ReadOnly: true},
			{Name: "objectstore-creds", MountPath: objectStoreCredsPath, ReadOnly: true},
		},
	}
}

// shipImage is the image the shipping container runs: the override wired at install, else the
// floating default. It is resolved per call rather than captured, so an adapter constructed before
// the override was applied still uses it.
func (a *Adapter) shipImage() string {
	if a.shipperImage != "" {
		return a.shipperImage
	}
	return defaultShipperImage
}

// backupCredSecretName is the deterministic name of the destination-credential Secret for a backup
// Job. It is derived from the Job name so it is unique per backup and so the Job can own it.
func backupCredSecretName(jobName string) string { return jobName + "-objectstore" }

// ensureBackupCredentials writes the destination credential PAIR into a Secret in the add-on
// namespace that the shipping container mounts (ADR-0063 §1, following ADR-0057 §4's shape).
//
// It is created BEFORE the Job, not after, and that ordering is deliberate: a pod whose mounted
// Secret does not exist yet reports CreateContainerConfigError, which is a member of ADR-0074 §2's
// blocking set, and the Job waiter fails fast on exactly that (issue #352). Creating the Secret
// second would therefore race the kubelet and turn a working backup into a reported configuration
// failure. The Job adopts it afterwards (adoptBackupCredentials) so Kubernetes garbage-collects it
// even if burrowd dies before its own cleanup runs.
//
// The values are written straight into the Secret data. They are never a Job env var, a command-line
// argument, a log line, or an error: a write failure names the Secret only.
func (a *Adapter) ensureBackupCredentials(ctx context.Context, secretName string, cred controlplane.ObjectStoreCredential) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: a.addonNamespace,
			Labels:    map[string]string{nameLabel: secretName, managedByLabel: managedByValue, addonLabel: string(controlplane.AddonPostgres)},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			objectStoreAccessKeyIDFile:     []byte(cred.AccessKeyID),
			objectStoreSecretAccessKeyFile: []byte(cred.SecretAccessKey),
		},
	}
	secrets := a.client.CoreV1().Secrets(a.addonNamespace)
	if _, err := secrets.Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			// The error names the Secret only — never a credential value.
			return fmt.Errorf("kube: writing backup destination credentials %s/%s: %w", a.addonNamespace, secretName, err)
		}
		// A leftover from an interrupted run of the same backup id: overwrite it, so a rotated key is
		// used rather than a stale one that would fail the write for a reason nobody could see.
		if _, err := secrets.Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("kube: updating backup destination credentials %s/%s: %w", a.addonNamespace, secretName, err)
		}
	}
	return nil
}

// adoptBackupCredentials points the credential Secret's ownerReference at the Job, so Kubernetes
// deletes it whenever the Job goes — including the case burrowd never gets to its own cleanup
// because it was killed mid-backup. Best-effort: the explicit delete on the way out is the normal
// path, and failing to add a backstop must not fail a backup that is otherwise fine.
func (a *Adapter) adoptBackupCredentials(ctx context.Context, secretName string, owner *batchv1.Job) {
	secrets := a.client.CoreV1().Secrets(a.addonNamespace)
	existing, err := secrets.Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return
	}
	existing.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Name:       owner.Name,
		UID:        owner.UID,
	}}
	_, _ = secrets.Update(ctx, existing, metav1.UpdateOptions{})
}

// runBackupJobAwait creates the Job (and, with a destination, its credential Secret first), waits
// for it, and turns its terminal state into a BackupJobOutcome.
//
// The credential Secret is deleted on EVERY path out, success or failure. A failed Job is left
// behind for diagnosis — its pod logs are where the vendor's own error text is — but the key that
// writes to every backup Burrow holds is not left lying in the add-on namespace to keep it company.
func (a *Adapter) runBackupJobAwait(ctx context.Context, job *batchv1.Job, name string, dest *controlplane.BackupDestination) (controlplane.BackupJobOutcome, error) {
	jobs := a.client.BatchV1().Jobs(a.addonNamespace)
	secretName := backupCredSecretName(name)
	if dest != nil {
		if err := a.ensureBackupCredentials(ctx, secretName, dest.Credential); err != nil {
			return controlplane.BackupJobOutcome{}, err
		}
		defer func() {
			// Detached from ctx: a cancelled backup must still take its credential with it.
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			_ = a.client.CoreV1().Secrets(a.addonNamespace).Delete(cleanup, secretName, metav1.DeleteOptions{})
		}()
	}

	created, err := jobs.Create(ctx, job, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return controlplane.BackupJobOutcome{}, fmt.Errorf("kube: creating job %q: %w", name, err)
	}
	if dest != nil && created != nil && created.UID != "" {
		a.adoptBackupCredentials(ctx, secretName, created)
	}

	j, err := awaitJob(ctx, a.client, a.addonNamespace, name, backupJobTimeout, backupJobPoll, unschedulableGrace(ctx, a.limits))
	if err != nil {
		// A pod that could not start reports a reason from ADR-0074 §2's closed set; carry it through
		// so the Backup row records a blocked Job as precisely as it records a refused write. The
		// rendered Issue is safe to store: an Issue never carries a secret value (ADR-0074 §9).
		var blocked *controlplane.JobBlockedError
		if errors.As(err, &blocked) {
			return controlplane.BackupJobOutcome{Reason: blocked.Reason, Detail: blocked.Issue}, err
		}
		return controlplane.BackupJobOutcome{}, err
	}
	record := a.backupTerminationRecord(ctx, name)
	if j.Status.Failed > 0 {
		// Leave the Job (and its pod logs) for diagnosis; do not reap a failure.
		outcome := controlplane.BackupJobOutcome{Reason: record.reason, Detail: record.detail}
		if outcome.Reason == "" {
			// No container said why. With a destination the dump runs first, so the step that has not
			// reported is the one that did not get to run — pg_dump — and that is the honest reading.
			outcome.Reason = controlplane.BackupReasonDumpFailed
			outcome.Detail = "the backup Job failed without reporting a reason; its pod log has the command's own output"
		}
		return outcome, fmt.Errorf("kube: job %q failed: %s", name, outcome.Detail)
	}
	// Reap on success: delete the Job and its pods so they do not accumulate.
	policy := metav1.DeletePropagationBackground
	_ = jobs.Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &policy})
	return controlplane.BackupJobOutcome{SizeBytes: record.size}, nil
}

// terminationRecord is what a backup Job's containers wrote to /dev/termination-log: the dump's byte
// size on the way through, and on a failure the closed reason and the Burrow-authored detail the
// shipping container reported.
type terminationRecord struct {
	size   int64
	reason string
	detail string
}

// backupTerminationRecord reads the termination messages of a backup Job's pods. Best-effort: any
// miss or parse failure yields a zero record, never an error — a missing size must not fail a
// successful backup, and a missing reason is handled by the caller rather than by inventing one.
//
// Both the init container (the dump) and the main container (the shipping step) are read, in that
// order, so the later step's record wins: the dump reports a size and the shipper reports the size
// it actually wrote or the reason it could not. That ordering is the difference between reporting
// the length of a file that is still only in the cluster and reporting the length of an object in
// the store.
func (a *Adapter) backupTerminationRecord(ctx context.Context, jobName string) terminationRecord {
	pods, err := a.client.CoreV1().Pods(a.addonNamespace).List(ctx, metav1.ListOptions{LabelSelector: nameLabel + "=" + jobName})
	if err != nil || len(pods.Items) == 0 {
		return terminationRecord{}
	}
	var out terminationRecord
	for _, pod := range pods.Items {
		for _, statuses := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
			for _, cs := range statuses {
				if cs.State.Terminated == nil {
					continue
				}
				merge(&out, parseTerminationMessage(cs.State.Terminated.Message))
			}
		}
	}
	return out
}

// merge folds a parsed message into the record, letting a later container overwrite what an earlier
// one reported without letting an empty field erase a populated one.
func merge(into *terminationRecord, from terminationRecord) {
	if from.size > 0 {
		into.size = from.size
	}
	if from.reason != "" {
		into.reason, into.detail = from.reason, from.detail
	}
}

// parseTerminationMessage reads the small key=value record a backup container writes to
// /dev/termination-log. The dump container writes a bare byte count (the ADR-0032 format, kept so a
// backup with no destination is byte-for-byte the Job it always was); the shipping container writes
// `size=`, `reason=` and `detail=` lines.
//
// An unrecognised message parses to a zero record rather than an error. The termination log is a
// diagnostic channel, and a Job that succeeded must not be turned into a failure because its last
// line was unexpected.
func parseTerminationMessage(msg string) terminationRecord {
	var rec terminationRecord
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return rec
	}
	// The bare-integer form the pg_dump container has always written.
	if n, err := strconv.ParseInt(msg, 10, 64); err == nil {
		rec.size = n
		return rec
	}
	for _, line := range strings.Split(msg, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "size":
			if n, err := strconv.ParseInt(value, 10, 64); err == nil {
				rec.size = n
			}
		case "reason":
			rec.reason = value
		case "detail":
			rec.detail = value
		}
	}
	return rec
}
