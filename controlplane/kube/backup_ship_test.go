// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/burrow-cloud/burrow/controlplane"
)

// These tests cover the SHAPE of a backup Job that has a durable destination (ADR-0063 §7): the two
// steps in the order that makes the Job's success mean the store was reached, the credential
// reaching the pod only through a mounted Secret, and that Secret not outliving the run.

func testDestination() *controlplane.BackupDestination {
	return &controlplane.BackupDestination{
		Provider: "backups",
		Config: controlplane.ObjectStoreConfig{
			Endpoint: "https://s3.us-west-002.example.com",
			Region:   "us-west-002",
			Bucket:   "burrow-backups-abc",
		},
		Credential: controlplane.ObjectStoreCredential{
			AccessKeyID:     "AKIAEXAMPLEKEYID",
			SecretAccessKey: "wJalrXUtnFEMIexampleSECRETkey",
		},
		Key: "burrow/backups/prod/shop/bk1.dump",
	}
}

// TestBackupJobWithDestinationDumpsThenShips asserts the ordering that carries the whole invariant:
// the dump is an INIT container and the shipping step is the container whose exit decides the Job.
// There is no arrangement of these two in which the Job succeeds because only the dump did.
func TestBackupJobWithDestinationDumpsThenShips(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	var created []*batchv1.Job
	succeedJobs(client, &created)

	a := New(client, "apps").WithAddonNamespace(addonNS).WithShipperImage("ghcr.io/burrow-cloud/burrowd:v9.9.9")
	if _, err := a.RunBackupJob(ctx, "shop", controlplane.DefaultEnvironment, testInstance(controlplane.DefaultEnvironment), "bk1", testDestination()); err != nil {
		t.Fatalf("RunBackupJob: %v", err)
	}
	if len(created) != 1 {
		t.Fatalf("created %d jobs, want 1", len(created))
	}
	pod := created[0].Spec.Template.Spec

	if len(pod.InitContainers) != 1 || pod.InitContainers[0].Name != backupDumpContainer {
		t.Fatalf("init containers = %+v, want the pg_dump step first", pod.InitContainers)
	}
	if len(pod.Containers) != 1 || pod.Containers[0].Name != backupShipContainer {
		t.Fatalf("containers = %+v, want the shipping step to be the one that decides the Job", pod.Containers)
	}
	if !strings.Contains(strings.Join(pod.InitContainers[0].Command, " "), "pg_dump -Fc") {
		t.Errorf("the init container does not pg_dump: %v", pod.InitContainers[0].Command)
	}

	ship := pod.Containers[0]
	if ship.Image != "ghcr.io/burrow-cloud/burrowd:v9.9.9" {
		t.Errorf("shipper image = %q, want the pinned burrowd image", ship.Image)
	}
	// ARGS, never command: the image's entrypoint runs the binary wherever the build put it, and
	// this container names only the subcommand. It used to name the path too — `/burrowd`, which ko
	// has never produced — so this container failed to start on every release and no backup ever
	// reached the object store (issue #478). See burrowdcontainer.go.
	if len(ship.Command) != 0 {
		t.Errorf("shipper command = %v, want none: overriding the entrypoint means naming a path the build owns", ship.Command)
	}
	if got := strings.Join(ship.Args, " "); got != controlplane.ShipBackupCommand {
		t.Errorf("shipper args = %q, want %q", got, controlplane.ShipBackupCommand)
	}

	// The connection details are configuration and travel as env, so a backup's destination is
	// legible from the Job without reading a Secret.
	env := map[string]string{}
	for _, e := range ship.Env {
		env[e.Name] = e.Value
	}
	for name, want := range map[string]string{
		"BURROW_SHIP_ENDPOINT":        "https://s3.us-west-002.example.com",
		"BURROW_SHIP_REGION":          "us-west-002",
		"BURROW_SHIP_BUCKET":          "burrow-backups-abc",
		"BURROW_SHIP_KEY":             "burrow/backups/prod/shop/bk1.dump",
		"BURROW_SHIP_FILE":            controlplane.BackupPath("shop", "bk1"),
		"BURROW_SHIP_CREDENTIALS_DIR": objectStoreCredsPath,
	} {
		if env[name] != want {
			t.Errorf("%s = %q, want %q", name, env[name], want)
		}
	}

	// The shipper reads the dump; it has no business writing to the volume holding every other
	// backup, and the mount says so rather than trusting it not to.
	var mounted bool
	for _, m := range ship.VolumeMounts {
		if m.MountPath == backupMountPath {
			mounted = true
			if !m.ReadOnly {
				t.Error("the shipping container mounts the backup volume writable")
			}
		}
	}
	if !mounted {
		t.Error("the shipping container does not mount the backup volume")
	}
}

// TestBackupJobKeepsTheCredentialOutOfTheJobSpec is the standing rule at the one place it would be
// easiest to break: a Job's env and command are readable by anything that can read Jobs in the
// namespace, so the destination credential travels only as files in a mounted Secret volume.
func TestBackupJobKeepsTheCredentialOutOfTheJobSpec(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	var created []*batchv1.Job
	succeedJobs(client, &created)

	dest := testDestination()
	a := New(client, "apps").WithAddonNamespace(addonNS)
	if _, err := a.RunBackupJob(ctx, "shop", controlplane.DefaultEnvironment, testInstance(controlplane.DefaultEnvironment), "bk1", dest); err != nil {
		t.Fatalf("RunBackupJob: %v", err)
	}
	rendered := renderJob(created[0])
	for _, secret := range []string{dest.Credential.AccessKeyID, dest.Credential.SecretAccessKey} {
		if strings.Contains(rendered, secret) {
			t.Fatal("the Job spec carries a credential value")
		}
	}

	// It reaches the pod through a Secret volume, mounted read-only at the path the shipper reads.
	var secretVolume string
	for _, v := range created[0].Spec.Template.Spec.Volumes {
		if v.Secret != nil {
			secretVolume = v.Secret.SecretName
		}
	}
	if secretVolume != backupCredSecretName(created[0].Name) {
		t.Fatalf("secret volume = %q, want %q", secretVolume, backupCredSecretName(created[0].Name))
	}
}

// TestBackupCredentialSecretExistsBeforeTheJobAndNotAfter asserts the two things that stop the
// credential Secret from being a problem of its own: it is written BEFORE the Job (a pod whose
// mounted Secret is missing reports CreateContainerConfigError, which the Job waiter fails fast on,
// so creating it second would race the kubelet and fail a working backup), and it does not outlive
// the run.
func TestBackupCredentialSecretExistsBeforeTheJobAndNotAfter(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	var created []*batchv1.Job
	succeedJobs(client, &created)

	// The ORDER of the two creates is the whole assertion, so it is recorded as it happens rather
	// than inferred afterwards.
	var order []string
	client.PrependReactor("create", "*", func(action clienttesting.Action) (bool, runtime.Object, error) {
		order = append(order, action.GetResource().Resource)
		return false, nil, nil
	})

	a := New(client, "apps").WithAddonNamespace(addonNS)
	if _, err := a.RunBackupJob(ctx, "shop", controlplane.DefaultEnvironment, testInstance(controlplane.DefaultEnvironment), "bk1", testDestination()); err != nil {
		t.Fatalf("RunBackupJob: %v", err)
	}
	secretAt, jobAt := indexOfAction(order, "secrets"), indexOfAction(order, "jobs")
	if secretAt < 0 || jobAt < 0 || secretAt > jobAt {
		t.Errorf("create order = %v; the credential Secret must exist before the Job, or the kubelet races into CreateContainerConfigError and the waiter fails a working backup", order)
	}

	name := backupCredSecretName(created[0].Name)
	if _, err := client.CoreV1().Secrets(addonNS).Get(ctx, name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the credential Secret outlived the backup (err = %v); the key that writes to every backup must not be left in the add-on namespace", err)
	}
}

// TestBackupCredentialSecretIsRemovedWhenTheJobFails asserts the cleanup is on EVERY path. A failed
// Job is deliberately left for diagnosis — its pod log holds the vendor's own error text — but the
// credential is not left lying beside it.
func TestBackupCredentialSecretIsRemovedWhenTheJobFails(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	var created []*batchv1.Job
	client.PrependReactor("create", "jobs", func(action clienttesting.Action) (bool, runtime.Object, error) {
		created = append(created, action.(clienttesting.CreateAction).GetObject().(*batchv1.Job).DeepCopy())
		return false, nil, nil
	})
	client.PrependReactor("get", "jobs", func(action clienttesting.Action) (bool, runtime.Object, error) {
		name := action.(clienttesting.GetAction).GetName()
		return true, &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: addonNS},
			Status:     batchv1.JobStatus{Failed: 1},
		}, nil
	})

	a := New(client, "apps").WithAddonNamespace(addonNS)
	if _, err := a.RunBackupJob(ctx, "shop", controlplane.DefaultEnvironment, testInstance(controlplane.DefaultEnvironment), "bk1", testDestination()); err == nil {
		t.Fatal("RunBackupJob should error when the Job fails")
	}
	name := backupCredSecretName(created[0].Name)
	if _, err := client.CoreV1().Secrets(addonNS).Get(ctx, name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the credential Secret survived a failed backup (err = %v)", err)
	}
}

// TestBackupJobWithoutDestinationIsUnchanged asserts an install with no object-storage provider gets
// exactly the Job ADR-0032 always built: one container, no Secret volume, no shipping step. The new
// path must not change what a cluster with no destination does.
func TestBackupJobWithoutDestinationIsUnchanged(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	var created []*batchv1.Job
	succeedJobs(client, &created)

	a := New(client, "apps").WithAddonNamespace(addonNS)
	if _, err := a.RunBackupJob(ctx, "shop", controlplane.DefaultEnvironment, testInstance(controlplane.DefaultEnvironment), "bk1", nil); err != nil {
		t.Fatalf("RunBackupJob: %v", err)
	}
	pod := created[0].Spec.Template.Spec
	if len(pod.InitContainers) != 0 {
		t.Errorf("init containers = %+v, want none", pod.InitContainers)
	}
	if len(pod.Containers) != 1 || pod.Containers[0].Name != backupDumpContainer {
		t.Errorf("containers = %+v, want just the pg_dump step", pod.Containers)
	}
	for _, v := range pod.Volumes {
		if v.Secret != nil {
			t.Errorf("a backup with no destination mounts a Secret volume: %+v", v)
		}
	}
	secrets, _ := client.CoreV1().Secrets(addonNS).List(ctx, metav1.ListOptions{})
	if len(secrets.Items) != 0 {
		t.Errorf("a backup with no destination created %d Secrets, want 0", len(secrets.Items))
	}
}

// TestBackupOutcomeCarriesTheShippersReason asserts the closed reason the shipping container wrote to
// its termination log reaches the caller, which is what lets the Backup row say WHY rather than that
// something failed.
func TestBackupOutcomeCarriesTheShippersReason(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	client.PrependReactor("get", "jobs", func(action clienttesting.Action) (bool, runtime.Object, error) {
		name := action.(clienttesting.GetAction).GetName()
		return true, &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: addonNS},
			Status:     batchv1.JobStatus{Failed: 1},
		}, nil
	})
	// The pod the shipping container ran in, with the record it left behind.
	jobName := "burrow-pg-backup-bk1"
	if _, err := client.CoreV1().Pods(addonNS).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: jobName + "-xyz", Namespace: addonNS, Labels: map[string]string{nameLabel: jobName}},
		Status: corev1.PodStatus{
			InitContainerStatuses: []corev1.ContainerStatus{{
				Name:  backupDumpContainer,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: "2048"}},
			}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: backupShipContainer,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					Message: "reason=" + controlplane.BackupReasonObjectNotReadable + "\ndetail=the destination accepted the write and then reported the object as absent\n",
				}},
			}},
		},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seeding the pod: %v", err)
	}

	a := New(client, "apps").WithAddonNamespace(addonNS)
	outcome, err := a.RunBackupJob(ctx, "shop", controlplane.DefaultEnvironment, testInstance(controlplane.DefaultEnvironment), "bk1", testDestination())
	if err == nil {
		t.Fatal("RunBackupJob should error when the Job failed")
	}
	if outcome.Reason != controlplane.BackupReasonObjectNotReadable {
		t.Errorf("reason = %q, want %q", outcome.Reason, controlplane.BackupReasonObjectNotReadable)
	}
	if !strings.Contains(outcome.Detail, "reported the object as absent") {
		t.Errorf("detail = %q, want the shipper's own line", outcome.Detail)
	}
	// The dump's size does not travel on a failure: a length on a failed backup reads like a partial
	// success, and there is nothing to restore.
	if outcome.SizeBytes != 0 {
		t.Errorf("size = %d on a failed backup, want 0", outcome.SizeBytes)
	}
}

// TestParseTerminationMessage pins both forms of the record, because it is the interface between two
// processes: the bare byte count the pg_dump container has always written (kept, so a backup with no
// destination is the Job it always was) and the key=value record the shipper writes.
func TestParseTerminationMessage(t *testing.T) {
	if got := parseTerminationMessage("2048"); got.size != 2048 || got.reason != "" {
		t.Errorf("bare count parsed as %+v", got)
	}
	got := parseTerminationMessage("size=4096\nreason=StoreRejected\ndetail=the destination refused the write\n")
	if got.size != 4096 || got.reason != "StoreRejected" || got.detail != "the destination refused the write" {
		t.Errorf("record parsed as %+v", got)
	}
	// An unrecognised message is a zero record, never an error: the termination log is a diagnostic
	// channel, and a Job that succeeded must not be failed by an unexpected last line.
	if got := parseTerminationMessage("something else entirely"); got != (terminationRecord{}) {
		t.Errorf("unrecognised message parsed as %+v", got)
	}
}

// indexOfAction returns the position of a resource in the recorded create order, or -1.
func indexOfAction(order []string, value string) int {
	for i, v := range order {
		if v == value {
			return i
		}
	}
	return -1
}

// renderJob flattens a Job's env, commands and volumes into one string, so a test can assert that a
// value appears NOWHERE in it.
func renderJob(job *batchv1.Job) string {
	var b strings.Builder
	pod := job.Spec.Template.Spec
	for _, cs := range [][]corev1.Container{pod.InitContainers, pod.Containers} {
		for _, c := range cs {
			b.WriteString(strings.Join(c.Command, " "))
			b.WriteString(strings.Join(c.Args, " "))
			for _, e := range c.Env {
				b.WriteString(e.Name + "=" + e.Value + " ")
			}
		}
	}
	for _, v := range pod.Volumes {
		b.WriteString(v.Name + " ")
		if v.Secret != nil {
			b.WriteString(v.Secret.SecretName + " ")
		}
	}
	return b.String()
}
