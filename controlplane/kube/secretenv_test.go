// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube_test

import (
	"context"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/kube"
)

// How the app's Secret reaches its ENVIRONMENT (ADR-0089 §4). One test here matters more than the
// rest: an app with no file-only key must render the pod template it rendered before any of this
// existed, because nearly every app is that app and none of them asked for this.

// applied renders spec through the real adapter and returns the pod spec Kubernetes received.
func applied(t *testing.T, spec cp.WorkloadSpec) corev1.PodSpec {
	t.Helper()
	client := fake.NewSimpleClientset()
	if err := kube.New(client, ns).ApplyWorkload(context.Background(), spec); err != nil {
		t.Fatalf("ApplyWorkload: %v", err)
	}
	dep, err := client.AppsV1().Deployments(ns).Get(context.Background(), spec.App, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return dep.Spec.Template.Spec
}

// ordinarySpec is a workload with everything an ordinary app has and nothing to do with §4.
func ordinarySpec() cp.WorkloadSpec {
	return cp.WorkloadSpec{
		App: "web", Image: "img:1", Replicas: 2, ReleaseID: "rel-1",
		Env:         map[string]string{"LOG_LEVEL": "debug", "PORT": "8080"},
		Command:     []string{"/bin/app", "serve"},
		MetricsPort: 9090,
		Readiness:   cp.ReadinessCheck{Port: 8080, Path: "/healthz"},
	}
}

// secretRefNames returns the names of the container's env entries that read the Secret by key
// reference, and the values of the ones that carry a value inline.
func secretRefNames(env []corev1.EnvVar) []string {
	var names []string
	for _, e := range env {
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			names = append(names, e.Name)
		}
	}
	return names
}

// TestApplyWithoutAFileOnlyKeyIsUnchanged is the load-bearing test of this whole change. An app that
// marks no key file-only has to render the pod template it rendered before §4 existed — the same
// wholesale envFrom, and not one enumerated reference — and it must do so EVEN IF the enumerated key
// list is populated, because that is the state every app is in the moment the machinery exists.
func TestApplyWithoutAFileOnlyKeyIsUnchanged(t *testing.T) {
	before := applied(t, ordinarySpec())

	withKeys := ordinarySpec()
	withKeys.SecretEnvKeys = []string{"DATABASE_URL", "STRIPE_SECRET_KEY"}
	after := applied(t, withKeys)

	if !reflect.DeepEqual(before, after) {
		t.Errorf("an app with no file-only key rendered a different pod spec once enumeration existed:\nbefore = %+v\nafter  = %+v", before, after)
	}
	// And the fast path is what both of them are on: one optional envFrom over the whole Secret.
	from := after.Containers[0].EnvFrom
	if len(from) != 1 || from[0].SecretRef == nil || from[0].SecretRef.Name != cp.AppSecretName("web") {
		t.Fatalf("EnvFrom = %+v, want the wholesale reference to the app's Secret", from)
	}
	if from[0].SecretRef.Optional == nil || !*from[0].SecretRef.Optional {
		t.Errorf("EnvFrom optional = %v, want true: a workload whose Secret does not exist yet still applies", from[0].SecretRef.Optional)
	}
	if names := secretRefNames(after.Containers[0].Env); len(names) != 0 {
		t.Errorf("env reads %v by key reference; an app with no file-only key enumerates nothing", names)
	}
}

// TestApplyMountedButNotFileOnlyKeepsEnvFrom: mounting is not what costs the fast path — marking is.
// A key that is both a file and a variable adds the volume and leaves the environment alone.
func TestApplyMountedButNotFileOnlyKeepsEnvFrom(t *testing.T) {
	spec := ordinarySpec()
	spec.SecretFiles = cp.SecretMounts{Mounts: []cp.SecretMount{{App: "web", Key: "TLS_KEY", Filename: "tls.key"}}}
	spec.SecretEnvKeys = []string{"TLS_KEY"}
	pod := applied(t, spec)

	if len(pod.Containers[0].EnvFrom) != 1 {
		t.Errorf("EnvFrom = %+v, want the wholesale reference: a mount adds a file and does not touch the environment",
			pod.Containers[0].EnvFrom)
	}
	if names := secretRefNames(pod.Containers[0].Env); len(names) != 0 {
		t.Errorf("env reads %v by key reference, want none", names)
	}
	if len(pod.Volumes) != 1 {
		t.Errorf("Volumes = %+v, want the mount's volume", pod.Volumes)
	}
}

// TestApplyFileOnlyKeyLeavesTheEnvFromFastPath: the switch itself. envFrom cannot exclude a key, so
// the only way to keep TLS_KEY out of the environment is to stop sourcing the Secret wholesale and
// name the rest — and TLS_KEY must not be among the names.
func TestApplyFileOnlyKeyLeavesTheEnvFromFastPath(t *testing.T) {
	spec := ordinarySpec()
	spec.SecretFiles = cp.SecretMounts{Mounts: []cp.SecretMount{{App: "web", Key: "TLS_KEY", Filename: "tls.key", NoEnv: true}}}
	spec.SecretEnvKeys = []string{"STRIPE_SECRET_KEY", "DATABASE_URL"}
	pod := applied(t, spec)

	if len(pod.Containers[0].EnvFrom) != 0 {
		t.Fatalf("EnvFrom = %+v, want none: envFrom sources the Secret wholesale, so a file-only key cannot survive it",
			pod.Containers[0].EnvFrom)
	}
	// Sorted, so the same set of keys renders the same template and a reapply rolls nothing.
	if got := secretRefNames(pod.Containers[0].Env); !reflect.DeepEqual(got, []string{"DATABASE_URL", "STRIPE_SECRET_KEY"}) {
		t.Errorf("enumerated keys = %v, want the two remaining keys sorted", got)
	}
	for _, e := range pod.Containers[0].Env {
		if e.Name == "TLS_KEY" {
			t.Errorf("TLS_KEY is in the container's environment: %+v — --no-env is the one thing that has to take it out", e)
		}
		if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
			continue
		}
		ref := e.ValueFrom.SecretKeyRef
		if ref.Name != cp.AppSecretName("web") || ref.Key != e.Name {
			t.Errorf("env %q reads %+v, want key %q of the app's own Secret", e.Name, ref, e.Name)
		}
		// Optional exactly as envFrom is: a key unset between two applies must leave the variable
		// absent, not wedge every pod at CreateContainerConfigError.
		if ref.Optional == nil || !*ref.Optional {
			t.Errorf("env %q reference optional = %v, want true", e.Name, ref.Optional)
		}
		if e.Value != "" {
			t.Errorf("env %q carries an inline value; a secret reaches the pod template only by reference", e.Name)
		}
	}
	// The file is still a file, and BURROW_SECRETS_DIR still names the directory.
	if len(pod.Volumes) != 1 || len(pod.Containers[0].VolumeMounts) != 1 {
		t.Errorf("a file-only key must still be a FILE: volumes = %+v, mounts = %+v", pod.Volumes, pod.Containers[0].VolumeMounts)
	}
}

// TestApplyEveryKeyFileOnly: an app whose only secret is file-only reads its Secret from disk and
// from nowhere else. There is no envFrom to fall back to and nothing to enumerate.
func TestApplyEveryKeyFileOnly(t *testing.T) {
	spec := ordinarySpec()
	spec.SecretFiles = cp.SecretMounts{Mounts: []cp.SecretMount{{App: "web", Key: "TLS_KEY", Filename: "tls.key", NoEnv: true}}}
	pod := applied(t, spec)

	if len(pod.Containers[0].EnvFrom) != 0 {
		t.Errorf("EnvFrom = %+v, want none", pod.Containers[0].EnvFrom)
	}
	if names := secretRefNames(pod.Containers[0].Env); len(names) != 0 {
		t.Errorf("enumerated keys = %v, want none: every key this app holds is file-only", names)
	}
	// The app's own config is untouched by any of it.
	for _, want := range []string{"LOG_LEVEL", "PORT", cp.SecretsDirEnvVar} {
		found := false
		for _, e := range pod.Containers[0].Env {
			found = found || e.Name == want
		}
		if !found {
			t.Errorf("%s is missing from the container's environment", want)
		}
	}
}

// TestApplyConfigStillBeatsAnEnumeratedSecret. The kubelet applies envFrom first and lets env
// override it, so under the fast path a config var of the same name won. Enumeration must not
// quietly reverse that: a key a config var already named is skipped, not emitted a second time.
func TestApplyConfigStillBeatsAnEnumeratedSecret(t *testing.T) {
	spec := ordinarySpec()
	spec.Env = map[string]string{"DATABASE_URL": "postgres://override"}
	spec.SecretFiles = cp.SecretMounts{Mounts: []cp.SecretMount{{App: "web", Key: "TLS_KEY", Filename: "tls.key", NoEnv: true}}}
	spec.SecretEnvKeys = []string{"DATABASE_URL", "STRIPE_SECRET_KEY"}
	pod := applied(t, spec)

	var seen int
	for _, e := range pod.Containers[0].Env {
		if e.Name != "DATABASE_URL" {
			continue
		}
		seen++
		if e.Value != "postgres://override" {
			t.Errorf("DATABASE_URL = %+v, want the config value: config beat the Secret under envFrom and has to here too", e)
		}
	}
	if seen != 1 {
		t.Errorf("DATABASE_URL appears %d times in the container's environment, want once", seen)
	}
}
