// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/burrow-cloud/burrow/controlplane"
)

// The Job that runs Burrow's check inside an image that may contain nothing (ADR-0076 §4).

// probeJob builds the check Job the engine would ask for, without a cluster.
func probeJob(t *testing.T, spec controlplane.RunSpec) *corev1.PodSpec {
	t.Helper()
	a := New(fake.NewSimpleClientset(), "burrow-apps")
	job := a.runJob("burrow-run-check-1", spec)
	return &job.Spec.Template.Spec
}

// checkSpec is a dependency check as the engine drives it: the app's own image, the app's config, and
// the probe request.
func checkSpec() controlplane.RunSpec {
	return controlplane.RunSpec{
		App:     "web",
		ID:      "check-1",
		Image:   "repo/web:1.0.0",
		Command: []string{controlplane.ProbePath, controlplane.ProbeCheckCommand},
		Env:     map[string]string{"LOG_LEVEL": "debug"},
		Probe:   &controlplane.ProbeSpec{Env: map[string]string{controlplane.ProbePlanEnv: `{"checks":[]}`}},
	}
}

// TestProbeRunsInTheAppsImageWithBurrowsBinary is the mechanism decision, asserted. The CHECK
// container is the app's own image — its filesystem, its service account, its network policy, its
// injected Secret — and the binary it executes arrives from an INIT container running Burrow's image,
// so the app's image is required to provide nothing at all. Requiring it to carry psql or curl would
// have failed on exactly the minimal images this check is most valuable on.
func TestProbeRunsInTheAppsImageWithBurrowsBinary(t *testing.T) {
	pod := probeJob(t, checkSpec())

	if len(pod.Containers) != 1 {
		t.Fatalf("containers = %d, want one", len(pod.Containers))
	}
	if got := pod.Containers[0].Image; got != "repo/web:1.0.0" {
		t.Errorf("check container image = %q, want the APP's image: a check run elsewhere proves the cluster can reach the database, not that the app can", got)
	}
	if len(pod.InitContainers) != 1 {
		t.Fatalf("init containers = %d, want one that installs the probe", len(pod.InitContainers))
	}
	init := pod.InitContainers[0]
	if !strings.Contains(init.Image, "burrowd") {
		t.Errorf("init container image = %q, want Burrow's own image", init.Image)
	}
	// ARGS, never command: the image's entrypoint runs the binary wherever the build put it, and this
	// container names only the subcommand and its argument. It used to name the path too —
	// `/burrowd`, which ko has never produced — so this init container failed to start on every
	// release and every dependency check ever run skipped (issue #478). See burrowdcontainer.go.
	if len(init.Command) != 0 {
		t.Errorf("init command = %v, want none: overriding the entrypoint means naming a path the build owns", init.Command)
	}
	if len(init.Args) < 1 || init.Args[0] != controlplane.ProbeInstallCommand {
		t.Errorf("init args = %v, want the %s subcommand first", init.Args, controlplane.ProbeInstallCommand)
	}
	// The init container copies its own executable, so it needs no shell and no `cp` — the same
	// problem one layer up, since Burrow's base image is distroless too.
	for _, arg := range init.Args {
		if arg == "sh" || arg == "/bin/sh" || arg == "cp" {
			t.Errorf("init args = %v, want no shell or coreutils: Burrow's own base image has neither", init.Args)
		}
	}
	if got := pod.Containers[0].Command; len(got) != 2 || got[0] != controlplane.ProbePath {
		t.Errorf("check command = %v, want the copied binary at %s", got, controlplane.ProbePath)
	}
}

// TestProbeVolumeIsSharedAndReadOnlyInTheAppsContainer: the app's container executes Burrow's binary
// out of the shared directory and has no business writing to it.
func TestProbeVolumeIsSharedAndReadOnlyInTheAppsContainer(t *testing.T) {
	pod := probeJob(t, checkSpec())

	var found bool
	for _, v := range pod.Volumes {
		if v.Name == controlplane.ProbeVolumeName {
			found = true
			if v.EmptyDir == nil {
				t.Errorf("probe volume = %+v, want an emptyDir", v)
			}
		}
	}
	if !found {
		t.Fatalf("no %q volume on the pod", controlplane.ProbeVolumeName)
	}

	mountOf := func(c corev1.Container) (corev1.VolumeMount, bool) {
		for _, m := range c.VolumeMounts {
			if m.Name == controlplane.ProbeVolumeName {
				return m, true
			}
		}
		return corev1.VolumeMount{}, false
	}
	init, ok := mountOf(pod.InitContainers[0])
	if !ok {
		t.Fatal("the init container does not mount the probe volume")
	}
	if init.ReadOnly {
		t.Error("the init container's mount is read-only, so it cannot write the binary")
	}
	check, ok := mountOf(pod.Containers[0])
	if !ok {
		t.Fatal("the check container does not mount the probe volume")
	}
	if !check.ReadOnly {
		t.Error("the check container's mount is writable; the app's image has no business writing where it executes Burrow's binary from")
	}
	if init.MountPath != controlplane.ProbeMountPath || check.MountPath != controlplane.ProbeMountPath {
		t.Errorf("mount paths = %q / %q, want both at %s", init.MountPath, check.MountPath, controlplane.ProbeMountPath)
	}
}

// TestProbeEnvWinsOverTheAppsConfig: the app's config is applied first and the probe's plan after, so
// an app that happens to define the plan variable cannot shadow what Burrow is asking for.
// Kubernetes applies a container's env in order, and `env` already beats the `envFrom` the Secret
// arrives through.
func TestProbeEnvWinsOverTheAppsConfig(t *testing.T) {
	spec := checkSpec()
	spec.Env = map[string]string{controlplane.ProbePlanEnv: "an app-supplied value", "LOG_LEVEL": "debug"}
	pod := probeJob(t, spec)

	var lastPlan, lastIdx = "", -1
	for i, e := range pod.Containers[0].Env {
		if e.Name == controlplane.ProbePlanEnv {
			lastPlan, lastIdx = e.Value, i
		}
	}
	if lastIdx < 0 {
		t.Fatal("the plan is not on the check container")
	}
	if lastPlan != `{"checks":[]}` {
		t.Errorf("the effective plan is %q, want Burrow's; an app's own value shadowed it", lastPlan)
	}
}

// TestProbeCarriesNoCredentialToBurrowsOwnContainer: the init container runs Burrow's binary with
// Burrow's image, and the credential belongs to the container that is meant to use it. Only the
// check container — the app's own image — is given the app's environment and its Secret.
func TestProbeCarriesNoCredentialToBurrowsOwnContainer(t *testing.T) {
	pod := probeJob(t, checkSpec())
	init := pod.InitContainers[0]
	if len(init.Env) != 0 {
		t.Errorf("the init container carries env %+v, want none", init.Env)
	}
	if len(init.EnvFrom) != 0 {
		t.Errorf("the init container sources %+v, want nothing: the app's Secret is not Burrow's image's business", init.EnvFrom)
	}
	// The check container does get the Secret, which is the whole point of running there.
	var sourcesSecret bool
	for _, f := range pod.Containers[0].EnvFrom {
		if f.SecretRef != nil && f.SecretRef.Name == controlplane.AppSecretName("web") {
			sourcesSecret = true
		}
	}
	if !sourcesSecret {
		t.Error("the check container does not source the app's Secret, so it cannot check with the app's own credential")
	}
}

// TestOrdinaryRunIsUnchangedByTheProbeSeam: `burrow app run` passes no ProbeSpec, and a run without
// one must be byte-for-byte the Job it was before ADR-0076 §4 existed.
func TestOrdinaryRunIsUnchangedByTheProbeSeam(t *testing.T) {
	pod := probeJob(t, controlplane.RunSpec{App: "web", ID: "1", Image: "repo/web:1.0.0", Command: []string{"./migrate"}})
	if len(pod.InitContainers) != 0 {
		t.Errorf("init containers = %+v, want none on an ordinary run", pod.InitContainers)
	}
	if len(pod.Volumes) != 0 {
		t.Errorf("volumes = %+v, want none on an ordinary run", pod.Volumes)
	}
	if len(pod.Containers[0].VolumeMounts) != 0 {
		t.Errorf("mounts = %+v, want none on an ordinary run", pod.Containers[0].VolumeMounts)
	}
}

// TestProbePodTakesPlacementPolicy: the check pod runs the tenant's image in the tenant's namespace,
// so it is admitted and scheduled under the same constraints the app itself is (ADR-0061). A pod
// whose init container and check container could land under different constraints would be a pod that
// never schedules, which is why the mutator runs over the fully-assembled spec.
func TestProbePodTakesPlacementPolicy(t *testing.T) {
	a := New(fake.NewSimpleClientset(), "burrow-apps").WithPodMutator(func(p *corev1.PodSpec) {
		p.NodeSelector = map[string]string{"pool": "tenant"}
	})
	job := a.runJob("burrow-run-check-1", checkSpec())
	pod := job.Spec.Template.Spec
	if pod.NodeSelector["pool"] != "tenant" {
		t.Errorf("nodeSelector = %v, want the mutator's; a check pod that cannot schedule reports as a check that did not run", pod.NodeSelector)
	}
	if len(pod.InitContainers) != 1 {
		t.Fatalf("the mutator ran before the probe was attached: init containers = %d", len(pod.InitContainers))
	}
}

// TestProbeJobIsCreatedInTheAppsNamespace pins that the check lands beside the app rather than in
// Burrow's own namespace, which is what subjects it to the app's network policy.
func TestProbeJobIsCreatedInTheAppsNamespace(t *testing.T) {
	cs := fake.NewSimpleClientset()
	a := New(cs, "burrow-apps")
	job := a.runJob("burrow-run-check-1", checkSpec())
	if _, err := cs.BatchV1().Jobs("burrow-apps").Create(context.Background(), job, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := cs.BatchV1().Jobs("burrow-apps").Get(context.Background(), "burrow-run-check-1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Namespace != "burrow-apps" {
		t.Errorf("namespace = %q, want the app's", got.Namespace)
	}
}
