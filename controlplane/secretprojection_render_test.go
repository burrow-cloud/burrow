// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/kube"
)

// The end of the ADR-0089 §4 path, asserted where a user would feel it: the container's environment.
//
// This file reaches through the real adapter rather than stopping at the WorkloadSpec the engine
// applied, and the reason is the defect it was written for. `addon attach` writes a NEW key straight
// through the Kubernetes seam and then rolls the app; on an app that enumerates its secret
// environment, a restart brings the pod back with the connection string in the Secret and no
// DATABASE_URL in the container — attach reported successful, the value present, the variable simply
// absent. A test that asserted "the workload was re-applied" would pass on a reapply that still
// rendered the wrong environment. This one asserts the variable.

// containerEnv renders spec through the real adapter and returns the container's environment.
func containerEnv(t *testing.T, spec cp.WorkloadSpec) []corev1.EnvVar {
	t.Helper()
	client := k8sfake.NewSimpleClientset()
	if err := kube.New(client, "burrow-apps").ApplyWorkload(context.Background(), spec); err != nil {
		t.Fatalf("ApplyWorkload: %v", err)
	}
	dep, err := client.AppsV1().Deployments("burrow-apps").Get(context.Background(), spec.App, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return dep.Spec.Template.Spec.Containers[0].Env
}

// named reports whether env carries a variable of that name, however it is sourced.
func named(env []corev1.EnvVar, name string) bool {
	for _, e := range env {
		if e.Name == name {
			return true
		}
	}
	return false
}

// TestAttachReachesAnEnumeratedAppsEnvironment. Attaching Postgres to an app that marked a key
// file-only has to leave DATABASE_URL in the container, and it cannot get there on a restart: the
// enumerated pod template was written before the key existed, so only a re-apply names it.
func TestAttachReachesAnEnumeratedAppsEnvironment(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newPostgresEngine(t)
	k.SetSecret("web", "KUBECONFIG", "apiVersion: v1")
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if _, err := e.MountSecret(ctx, "web", "", "KUBECONFIG", "", "", boolPtr(true)); err != nil {
		t.Fatalf("MountSecret --no-env: %v", err)
	}

	if _, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", "", cp.AttachAddonOptions{}); err != nil {
		t.Fatalf("AttachAddon: %v", err)
	}
	spec, ok := k.Spec("web")
	if !ok {
		t.Fatal("no workload applied for web")
	}
	env := containerEnv(t, spec)
	if !named(env, cp.AppDatabaseURLKey) {
		t.Errorf("%s is not in the container's environment after an attach: %+v — the attach succeeded, the Secret holds the connection string, and the app cannot see it",
			cp.AppDatabaseURLKey, env)
	}
	// And the attach did not undo what the mount did.
	if named(env, "KUBECONFIG") {
		t.Errorf("KUBECONFIG is back in the container's environment after an attach: %+v", env)
	}
}

// TestAttachOnTheFastPathIsUnchanged: the app that marked nothing file-only keeps the cheap roll and
// the wholesale envFrom, so the attach reaches it exactly as it always did.
func TestAttachOnTheFastPathIsUnchanged(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newPostgresEngine(t)
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	if _, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", "", cp.AttachAddonOptions{}); err != nil {
		t.Fatalf("AttachAddon: %v", err)
	}
	if _, rolled := k.RestartedAt("web"); !rolled {
		t.Error("an attach on an app with no file-only key must still roll it by bumping the restart annotation")
	}
	spec, _ := k.Spec("web")
	if len(spec.SecretEnvKeys) != 0 {
		t.Errorf("the workload enumerates %v, want nothing: envFrom already delivers whatever the Secret holds, including the key the attach just wrote",
			spec.SecretEnvKeys)
	}
}
