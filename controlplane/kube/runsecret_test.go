// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/burrow-cloud/burrow/controlplane"
)

// A one-off Job gets the app's environment, and ADR-0089 §4 means that is now two doors rather than
// one. `burrow app run`, a lifecycle hook and the deploy-time dependency check all come through
// runJob, and a Job that sourced the Secret wholesale would put a key the app deliberately took out
// of its environment back into the one process most likely to hand it to a child.

// jobPod builds the run Job's pod spec without a cluster.
func jobPod(spec controlplane.RunSpec) *corev1.PodSpec {
	a := New(fake.NewSimpleClientset(), "burrow-apps")
	job := a.runJob("burrow-run-1", spec)
	return &job.Spec.Template.Spec
}

// runSpec is the app's own image and config, as every caller of runJob drives it.
func runSpec() controlplane.RunSpec {
	return controlplane.RunSpec{
		App:     "web",
		Image:   "img:1",
		Command: []string{"sh", "-c", "env"},
		Env:     map[string]string{"LOG_LEVEL": "debug"},
	}
}

// TestRunJobWithNoFileOnlyKeyIsUnchanged: the app that marked nothing keeps the wholesale envFrom the
// run Job always had, so a one-off command still sees DATABASE_URL and every other secret.
func TestRunJobWithNoFileOnlyKeyIsUnchanged(t *testing.T) {
	pod := jobPod(runSpec())

	from := pod.Containers[0].EnvFrom
	if len(from) != 1 || from[0].SecretRef == nil || from[0].SecretRef.Name != controlplane.AppSecretName("web") {
		t.Fatalf("EnvFrom = %+v, want the wholesale reference to the app's Secret", from)
	}
	if len(pod.Volumes) != 0 || len(pod.Containers[0].VolumeMounts) != 0 {
		t.Errorf("a run of an app that mounts nothing carries volumes %+v and mounts %+v, want none",
			pod.Volumes, pod.Containers[0].VolumeMounts)
	}
}

// TestRunJobDoesNotSourceAFileOnlyKey is the leak this closes. The Job must not source the Secret
// wholesale for an app that has left that path, or KUBECONFIG is in the environment of a shell and
// of everything the shell starts — which is the exact thing --no-env was used to prevent.
func TestRunJobDoesNotSourceAFileOnlyKey(t *testing.T) {
	spec := runSpec()
	spec.SecretFiles = controlplane.SecretMounts{Mounts: []controlplane.SecretMount{
		{App: "web", Key: "KUBECONFIG", Filename: "kubeconfig", NoEnv: true},
	}}
	spec.SecretEnvKeys = []string{"DATABASE_URL"}
	pod := jobPod(spec)

	if len(pod.Containers[0].EnvFrom) != 0 {
		t.Fatalf("EnvFrom = %+v, want none: envFrom sources every key the Secret holds, including the one the app took out of its environment",
			pod.Containers[0].EnvFrom)
	}
	var names []string
	for _, e := range pod.Containers[0].Env {
		names = append(names, e.Name)
		if e.Name == "KUBECONFIG" {
			t.Errorf("KUBECONFIG is in the run Job's environment: %+v", e)
		}
	}
	found := false
	for _, e := range pod.Containers[0].Env {
		if e.Name == "DATABASE_URL" && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			found = true
		}
	}
	if !found {
		t.Errorf("the run Job's environment is %v, want DATABASE_URL read from the Secret by key — a run sees what the app sees", names)
	}
	// And the file-only key reaches the command as the file it is, or a run could not use the
	// credential at all.
	if len(pod.Volumes) != 1 || len(pod.Containers[0].VolumeMounts) != 1 {
		t.Fatalf("volumes = %+v, mounts = %+v, want the app's secrets volume", pod.Volumes, pod.Containers[0].VolumeMounts)
	}
	if pod.Containers[0].VolumeMounts[0].MountPath != controlplane.DefaultSecretsDir {
		t.Errorf("MountPath = %q, want %q", pod.Containers[0].VolumeMounts[0].MountPath, controlplane.DefaultSecretsDir)
	}
	var dir string
	for _, e := range pod.Containers[0].Env {
		if e.Name == controlplane.SecretsDirEnvVar {
			dir = e.Value
		}
	}
	if dir != controlplane.DefaultSecretsDir {
		t.Errorf("%s = %q, want the directory the files landed in", controlplane.SecretsDirEnvVar, dir)
	}
}
