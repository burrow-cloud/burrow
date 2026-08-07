// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube_test

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/kube"
)

// The app PodSpec's first Volumes entry (ADR-0089 §2). These tests pin the four properties the
// record names as load-bearing — only mounted keys are projected, the mount is read-only at 0400,
// the volume is optional, and it is a whole-volume mount rather than a subPath — plus the one that
// keeps every app that mounts nothing exactly as it was.

// mountedSpec is a workload with two secret keys mounted and a third deliberately left alone.
func mountedSpec() cp.WorkloadSpec {
	return cp.WorkloadSpec{
		App: "web", Image: "img:1", Replicas: 1,
		SecretFiles: cp.SecretMounts{Mounts: []cp.SecretMount{
			{App: "web", Key: "GOOGLE_CREDENTIALS", Filename: "GOOGLE_CREDENTIALS"},
			{App: "web", Key: "TLS_KEY", Filename: "tls.key"},
		}},
	}
}

// TestApplyProjectsOnlyMountedKeys is the property that keeps a mount honest: mounting one key must
// not put every other credential the app holds on disk, where a path-traversal bug or a stray `tar`
// would reach them. STRIPE_SECRET_KEY is set on the app and not mounted, so it must not appear.
func TestApplyProjectsOnlyMountedKeys(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns)

	if err := a.ApplyWorkload(ctx, mountedSpec()); err != nil {
		t.Fatalf("ApplyWorkload: %v", err)
	}
	dep, err := client.AppsV1().Deployments(ns).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	vols := dep.Spec.Template.Spec.Volumes
	if len(vols) != 1 {
		t.Fatalf("volumes = %+v, want exactly one", vols)
	}
	src := vols[0].VolumeSource.Secret
	if src == nil {
		t.Fatalf("volume %q is not a Secret volume: %+v", vols[0].Name, vols[0].VolumeSource)
	}
	if got, want := src.SecretName, cp.AppSecretName("web"); got != want {
		t.Errorf("SecretName = %q, want %q", got, want)
	}
	got := map[string]string{}
	for _, item := range src.Items {
		got[item.Key] = item.Path
	}
	if len(got) != 2 || got["GOOGLE_CREDENTIALS"] != "GOOGLE_CREDENTIALS" || got["TLS_KEY"] != "tls.key" {
		t.Errorf("Items = %v, want exactly the two mounted keys with their filenames", got)
	}
	if _, ok := got["STRIPE_SECRET_KEY"]; ok {
		t.Errorf("an unmounted key is in the volume: %v — Items is what keeps a mount from putting every secret the app holds on disk", got)
	}
	// Sorted, so the same set of mounts always renders the same pod template and a reapply does not
	// roll the workload for nothing.
	if src.Items[0].Key != "GOOGLE_CREDENTIALS" || src.Items[1].Key != "TLS_KEY" {
		t.Errorf("Items = %+v, want sorted by key", src.Items)
	}
}

// TestApplyMountIsReadOnlyAt0400AndOptional pins the three flags. Optional matters for the reason
// envFrom's does: a key that is mounted and then unset must leave the pod with no file rather than
// blocking it from starting.
func TestApplyMountIsReadOnlyAt0400AndOptional(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns)

	if err := a.ApplyWorkload(ctx, mountedSpec()); err != nil {
		t.Fatalf("ApplyWorkload: %v", err)
	}
	dep, err := client.AppsV1().Deployments(ns).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	src := dep.Spec.Template.Spec.Volumes[0].VolumeSource.Secret
	if src.DefaultMode == nil || *src.DefaultMode != 0o400 {
		t.Errorf("DefaultMode = %v, want 0400", src.DefaultMode)
	}
	if src.Optional == nil || !*src.Optional {
		t.Errorf("Optional = %v, want true: a workload whose Secret does not exist yet still has to apply", src.Optional)
	}
	mounts := dep.Spec.Template.Spec.Containers[0].VolumeMounts
	if len(mounts) != 1 {
		t.Fatalf("VolumeMounts = %+v, want exactly one", mounts)
	}
	if !mounts[0].ReadOnly {
		t.Errorf("VolumeMounts[0].ReadOnly = false, want true")
	}
	if mounts[0].MountPath != cp.DefaultSecretsDir {
		t.Errorf("MountPath = %q, want %q", mounts[0].MountPath, cp.DefaultSecretsDir)
	}
	// A subPath is the one Secret-volume form the kubelet does not update in place, so rotation
	// without a rollout would be silently lost. There is one whole-volume mount and no subPath.
	if mounts[0].SubPath != "" {
		t.Errorf("SubPath = %q, want empty: a subPath mount is not updated in place by the kubelet", mounts[0].SubPath)
	}
}

// TestApplyMountCarriesThePathNotTheValue: BURROW_SECRETS_DIR names the directory, and the value
// never enters the pod template at all — the whole point of §3.
func TestApplyMountCarriesThePathNotTheValue(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns)

	spec := mountedSpec()
	spec.Env = map[string]string{"LOG_LEVEL": "debug"}
	spec.SecretFiles.Dir = "/etc/app/secrets"
	if err := a.ApplyWorkload(ctx, spec); err != nil {
		t.Fatalf("ApplyWorkload: %v", err)
	}
	dep, err := client.AppsV1().Deployments(ns).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	c := dep.Spec.Template.Spec.Containers[0]
	var dir string
	for _, e := range c.Env {
		if e.Name == cp.SecretsDirEnvVar {
			dir = e.Value
		}
	}
	if dir != "/etc/app/secrets" {
		t.Errorf("%s = %q, want the --dir override", cp.SecretsDirEnvVar, dir)
	}
	if c.VolumeMounts[0].MountPath != "/etc/app/secrets" {
		t.Errorf("MountPath = %q, want the --dir override", c.VolumeMounts[0].MountPath)
	}
	// The pod template holds key names and filenames and nothing else. Every env entry carries a
	// value the caller supplied as CONFIG; a secret value could only arrive here as a KeyToPath,
	// which has no value field at all.
	for _, e := range c.Env {
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			t.Errorf("env %q reads the Secret by key reference; this issue leaves the envFrom path alone", e.Name)
		}
	}
}

// TestApplyBurrowSecretsDirWinsOverConfig: an app that set a config var of the same name would
// otherwise be pointed at a directory Burrow did not mount, and would read nothing.
func TestApplyBurrowSecretsDirWinsOverConfig(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns)

	spec := mountedSpec()
	spec.Env = map[string]string{cp.SecretsDirEnvVar: "/somewhere/else"}
	if err := a.ApplyWorkload(ctx, spec); err != nil {
		t.Fatalf("ApplyWorkload: %v", err)
	}
	dep, err := client.AppsV1().Deployments(ns).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	env := dep.Spec.Template.Spec.Containers[0].Env
	// Kubernetes resolves a duplicated name to the LAST entry, so Burrow's must come after the
	// app's own.
	last := ""
	for _, e := range env {
		if e.Name == cp.SecretsDirEnvVar {
			last = e.Value
		}
	}
	if last != cp.DefaultSecretsDir {
		t.Errorf("the effective %s is %q, want %q: an app pointed at a directory Burrow did not mount reads nothing",
			cp.SecretsDirEnvVar, last, cp.DefaultSecretsDir)
	}
}

// TestApplyNoMountsLeavesThePodUnchanged: an app that mounts nothing gets the pod template it had
// before mounts existed — no volume, no volume mount, no BURROW_SECRETS_DIR.
func TestApplyNoMountsLeavesThePodUnchanged(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns)

	if err := a.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("ApplyWorkload: %v", err)
	}
	dep, err := client.AppsV1().Deployments(ns).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	pod := dep.Spec.Template.Spec
	if len(pod.Volumes) != 0 {
		t.Errorf("Volumes = %+v, want none", pod.Volumes)
	}
	if len(pod.Containers[0].VolumeMounts) != 0 {
		t.Errorf("VolumeMounts = %+v, want none", pod.Containers[0].VolumeMounts)
	}
	for _, e := range pod.Containers[0].Env {
		if e.Name == cp.SecretsDirEnvVar {
			t.Errorf("%s is set on an app that mounts nothing", cp.SecretsDirEnvVar)
		}
	}
}
