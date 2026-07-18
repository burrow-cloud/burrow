// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/burrow-cloud/burrow/controlplane"
)

// validDigest is a well-formed sha256 content digest the build container "writes" to its
// termination-log in the happy-path tests.
const validDigest = "sha256:" +
	"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// stubCapacity is a controlplane.CapacityProber whose resource state (and optional error) are seeded
// by the test, so the build's pre-flight (issue #274) can be driven without a cluster.
type stubCapacity struct {
	state controlplane.ClusterResourceState
	err   error
}

func (s stubCapacity) ReadResourceState(context.Context) (controlplane.ClusterResourceState, error) {
	return s.state, s.err
}

// ampleHeadroom is a resource state a build (¼ CPU / 512Mi) comfortably fits on: one large,
// near-empty node.
func ampleHeadroom() controlplane.ClusterResourceState {
	return controlplane.ClusterResourceState{
		Nodes: []controlplane.NodeAllocatable{{Name: "n1", CPUMillis: 4000, MemBytes: 8 << 30}},
	}
}

// noHeadroom mirrors issue #274: a small node already committed past what a build needs, so no single
// node has ¼ CPU and 512Mi free at once.
func noHeadroom() controlplane.ClusterResourceState {
	return controlplane.ClusterResourceState{
		Nodes: []controlplane.NodeAllocatable{{Name: "n1", CPUMillis: 1000, MemBytes: 1 << 30}},
		Pods: []controlplane.PodRequest{
			{Namespace: "kube-system", Name: "overhead", Node: "n1", CPUMillis: 900, MemBytes: 900 << 20},
		},
	}
}

// buildFakeSucceeding returns a fake clientset whose build Job is observed Succeeded on Get, plus a
// terminated pod carrying digest in its termination message so the adapter can read it back. It also
// records every created Job into created. The pod is seeded in the dedicated build namespace, where
// the adapter reads it back (issue #278).
func buildFakeSucceeding(t *testing.T, source controlplane.SourceRef, target, digest string) (*fake.Clientset, *[]*batchv1.Job) {
	t.Helper()
	client := fake.NewSimpleClientset()
	created := &[]*batchv1.Job{}
	client.PrependReactor("create", "jobs", func(a clienttesting.Action) (bool, runtime.Object, error) {
		*created = append(*created, a.(clienttesting.CreateAction).GetObject().(*batchv1.Job).DeepCopy())
		return false, nil, nil // let the tracker store it too
	})
	client.PrependReactor("get", "jobs", func(a clienttesting.Action) (bool, runtime.Object, error) {
		name := a.(clienttesting.GetAction).GetName()
		return true, &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: buildNamespace},
			Status:     batchv1.JobStatus{Succeeded: 1},
		}, nil
	})

	// A finished pod for the build Job, labelled like the Job so the digest read-back finds it.
	seedDigestPod(t, client, buildJobName(source, target), digest)
	return client, created
}

// seedDigestPod seeds a terminated build pod for jobName in the build namespace, labelled like the
// Job so jobTerminationDigest reads digest back from its termination-log.
func seedDigestPod(t *testing.T, client *fake.Clientset, jobName, digest string) {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: jobName + "-abc", Namespace: buildNamespace,
			Labels: map[string]string{nameLabel: jobName},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  buildContainerName,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: digest + "\n"}},
			}},
		},
	}
	if _, err := client.CoreV1().Pods(buildNamespace).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed pod: %v", err)
	}
}

// alreadyExistsOnFirstCreate makes the fake's first jobs.Create for jobName return AlreadyExists,
// simulating a Job left behind under the deterministic name by a previous re-run (issue #298). Every
// later Create (the recreate after replacing a failed Job) is recorded into created and succeeds. It
// returns a pointer to the running create count so a test can assert whether a rebuild happened.
func alreadyExistsOnFirstCreate(client *fake.Clientset, jobName string, created *[]*batchv1.Job) *int {
	count := new(int)
	client.PrependReactor("create", "jobs", func(a clienttesting.Action) (bool, runtime.Object, error) {
		*count++
		if *count == 1 {
			return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Group: "batch", Resource: "jobs"}, jobName)
		}
		obj := a.(clienttesting.CreateAction).GetObject().(*batchv1.Job).DeepCopy()
		*created = append(*created, obj)
		return true, obj, nil
	})
	return count
}

// TestBuildReplacesFailedJob asserts the least-surprising default for issue #298: when a re-run finds
// a previous build Job under the same deterministic name that FAILED, the adapter deletes it and
// creates a fresh Job so the re-run actually retries — rather than returning the stale failure until
// the TTL controller reaps it ~3 days later (issue #280). The "is it failed?" decision reuses the
// wait loop's interpretation (Status.Failed > 0).
func TestBuildReplacesFailedJob(t *testing.T) {
	ctx := context.Background()
	source := controlplane.SourceRef{Repo: "https://github.com/acme/shop", Ref: "v1"}
	const target = "reg.burrow.svc/acme/shop:1"

	client := fake.NewSimpleClientset()
	jobName := buildJobName(source, target)
	created := &[]*batchv1.Job{}
	createCount := alreadyExistsOnFirstCreate(client, jobName, created)

	client.PrependReactor("get", "jobs", func(a clienttesting.Action) (bool, runtime.Object, error) {
		name := a.(clienttesting.GetAction).GetName()
		st := batchv1.JobStatus{Failed: 1} // the reuse-decision Get sees the previous build's failure
		if len(*created) > 0 {
			st = batchv1.JobStatus{Succeeded: 1} // the fresh Job then succeeds so the build completes
		}
		return true, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: buildNamespace}, Status: st}, nil
	})
	deletedBeforeRecreate := false
	client.PrependReactor("delete", "jobs", func(clienttesting.Action) (bool, runtime.Object, error) {
		if len(*created) == 0 {
			deletedBeforeRecreate = true // the failed Job was deleted BEFORE any recreate
		}
		return true, nil, nil
	})
	seedDigestPod(t, client, jobName, validDigest)

	digest, err := NewBuilder(client).Build(ctx, source, target, false, controlplane.SourceCredential{})
	if err != nil {
		t.Fatalf("Build must retry after a failed Job, not reuse it (issue #298): %v", err)
	}
	if digest != validDigest {
		t.Errorf("digest = %q, want %q (the rebuild's result)", digest, validDigest)
	}
	if !deletedBeforeRecreate {
		t.Error("the failed Job was not deleted before the rebuild; a re-run must replace it, not reuse the stale failure (issue #298)")
	}
	if *createCount != 2 {
		t.Errorf("create attempts = %d, want 2 (the initial AlreadyExists, then a fresh Job after deleting the failed one)", *createCount)
	}
	if len(*created) != 1 {
		t.Errorf("recorded %d recreated jobs, want 1", len(*created))
	}
}

// TestBuildReusesSucceededJob asserts today's behavior is preserved: a re-run that finds a previously
// SUCCEEDED Job under the same name reuses its result — cheap and idempotent — and never rebuilds.
func TestBuildReusesSucceededJob(t *testing.T) {
	ctx := context.Background()
	source := controlplane.SourceRef{Repo: "https://github.com/acme/shop", Ref: "v1"}
	const target = "reg.burrow.svc/acme/shop:1"

	client := fake.NewSimpleClientset()
	jobName := buildJobName(source, target)
	created := &[]*batchv1.Job{}
	createCount := alreadyExistsOnFirstCreate(client, jobName, created)

	client.PrependReactor("get", "jobs", func(a clienttesting.Action) (bool, runtime.Object, error) {
		name := a.(clienttesting.GetAction).GetName()
		return true, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: buildNamespace}, Status: batchv1.JobStatus{Succeeded: 1}}, nil
	})
	seedDigestPod(t, client, jobName, validDigest)

	digest, err := NewBuilder(client).Build(ctx, source, target, false, controlplane.SourceCredential{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if digest != validDigest {
		t.Errorf("digest = %q, want %q (a succeeded Job's result is reused)", digest, validDigest)
	}
	if *createCount != 1 {
		t.Errorf("create attempts = %d, want 1 (a succeeded Job is reused, never rebuilt)", *createCount)
	}
	if len(*created) != 0 {
		t.Errorf("recreated %d jobs, want 0 (no rebuild of a succeeded Job)", len(*created))
	}
}

// TestBuildReusesActiveJob asserts a re-run that finds an ACTIVE Job for the same ref reuses it — an
// in-flight build must not be replaced or duplicated.
func TestBuildReusesActiveJob(t *testing.T) {
	ctx := context.Background()
	source := controlplane.SourceRef{Repo: "https://github.com/acme/shop", Ref: "v1"}
	const target = "reg.burrow.svc/acme/shop:1"

	client := fake.NewSimpleClientset()
	jobName := buildJobName(source, target)
	created := &[]*batchv1.Job{}
	createCount := alreadyExistsOnFirstCreate(client, jobName, created)

	completed := false
	var getCount int
	client.PrependReactor("get", "jobs", func(a clienttesting.Action) (bool, runtime.Object, error) {
		getCount++
		name := a.(clienttesting.GetAction).GetName()
		st := batchv1.JobStatus{Active: 1} // the reuse-decision Get: an in-flight build for the same ref
		if getCount > 1 {
			st = batchv1.JobStatus{Succeeded: 1} // it then finishes so the wait loop completes
			completed = true
		}
		return true, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: buildNamespace}, Status: st}, nil
	})
	deletedWhileActive := false
	client.PrependReactor("delete", "jobs", func(clienttesting.Action) (bool, runtime.Object, error) {
		if !completed {
			deletedWhileActive = true
		}
		return true, nil, nil
	})
	seedDigestPod(t, client, jobName, validDigest)

	digest, err := NewBuilder(client).Build(ctx, source, target, false, controlplane.SourceCredential{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if digest != validDigest {
		t.Errorf("digest = %q, want %q (the in-flight Job is reused)", digest, validDigest)
	}
	if *createCount != 1 {
		t.Errorf("create attempts = %d, want 1 (an active Job is reused, never replaced or duplicated)", *createCount)
	}
	if deletedWhileActive {
		t.Error("deleted the active Job before it finished; an in-flight build must not be replaced (issue #298)")
	}
}

// TestBuildJobSpec asserts the build Job is a hardened, in-cluster clone-and-build in the dedicated
// burrow-builds namespace (issue #278): an init container clones the git ref (the ref plumbed in as
// env, never source bytes), the build container selects buildah-or-Buildpacks at runtime and pushes
// to the target, and both carry resource caps. The clone keeps the full restricted floor; the build
// container's context is relaxed to run the rootless builder (ADR-0056) — asserted separately.
func TestBuildJobSpec(t *testing.T) {
	ctx := context.Background()
	source := controlplane.SourceRef{Repo: "https://github.com/acme/shop", Ref: "v1.2.3"}
	const target = "reg.burrow.svc/acme/shop:1.2.3"
	client, created := buildFakeSucceeding(t, source, target, validDigest)

	b := NewBuilder(client)
	digest, err := b.Build(ctx, source, target, false, controlplane.SourceCredential{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if digest != validDigest {
		t.Errorf("digest = %q, want %q (read from the pod termination-log)", digest, validDigest)
	}

	if len(*created) != 1 {
		t.Fatalf("created %d jobs, want 1", len(*created))
	}
	job := (*created)[0]

	if job.Namespace != buildNamespace {
		t.Errorf("job namespace = %q, want %q (the dedicated build namespace, issue #278)", job.Namespace, buildNamespace)
	}
	if !strings.HasPrefix(job.Name, "burrow-build-") {
		t.Errorf("job name = %q, want a burrow-build- prefix", job.Name)
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Errorf("backoffLimit = %v, want 0 (a single attempt, no retry masking a failure)", job.Spec.BackoffLimit)
	}
	pod := job.Spec.Template.Spec
	if pod.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restart policy = %q, want Never", pod.RestartPolicy)
	}

	// The clone runs as an init container so the workspace is populated before the build container
	// starts. Only the ref crosses the seam — the source is cloned INSIDE the cluster (ADR-0053 §3).
	if len(pod.InitContainers) != 1 {
		t.Fatalf("init containers = %d, want 1 (the clone)", len(pod.InitContainers))
	}
	clone := pod.InitContainers[0]
	gotRepo, gotRef := envValue(clone.Env, "REPO"), envValue(clone.Env, "REF")
	if gotRepo != source.Repo || gotRef != source.Ref {
		t.Errorf("clone env REPO/REF = %q/%q, want %q/%q", gotRepo, gotRef, source.Repo, source.Ref)
	}
	// The ref is passed as env, never interpolated into the clone script — so it is data, not shell.
	if strings.Contains(strings.Join(clone.Command, " "), source.Ref) {
		t.Errorf("clone command interpolates the ref (%q) instead of passing it as env", source.Ref)
	}

	if len(pod.Containers) != 1 {
		t.Fatalf("containers = %d, want 1 (the build)", len(pod.Containers))
	}
	build := pod.Containers[0]
	if got := envValue(build.Env, "TARGET_IMAGE"); got != target {
		t.Errorf("build env TARGET_IMAGE = %q, want %q", got, target)
	}
	// A secure (external) push carries no TARGET_INSECURE hint, so the push stays over TLS by default.
	if got := envValue(build.Env, "TARGET_INSECURE"); got != "" {
		t.Errorf("build env TARGET_INSECURE = %q, want empty for a secure push", got)
	}
	script := strings.Join(build.Command, " ")
	// The push carries the conditional insecure flag, gated on TARGET_INSECURE at runtime.
	if !strings.Contains(script, "--tls-verify=false") {
		t.Errorf("build script has no conditional --tls-verify=false for the plain-HTTP in-cluster push:\n%s", script)
	}
	// Builder selection happens at runtime, after the clone, from the cloned tree — buildah when a
	// Dockerfile is present, the Buildpacks lifecycle when it is not (ADR-0053 §4).
	if !strings.Contains(script, "Dockerfile") {
		t.Errorf("build script does not detect a Dockerfile:\n%s", script)
	}
	if !strings.Contains(script, "buildah") {
		t.Errorf("build script has no buildah branch (the Dockerfile case):\n%s", script)
	}
	if !strings.Contains(script, "creator") {
		t.Errorf("build script has no Buildpacks lifecycle branch (the no-Dockerfile case):\n%s", script)
	}
	if !strings.Contains(script, "/dev/termination-log") {
		t.Errorf("build script does not write the digest to the termination-log:\n%s", script)
	}

	// No source bytes anywhere in the Job — only the ref, repo, and target reference cross (ADR-0004).
	assertNoSourceBytes(t, job)

	// Pod-level restricted PodSecurity (ADR-0053 §7): non-root, fixed UID, fsGroup, RuntimeDefault.
	sc := pod.SecurityContext
	if sc == nil {
		t.Fatal("pod securityContext is nil, want the restricted floor")
	}
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Error("pod runAsNonRoot is not true")
	}
	if sc.RunAsUser == nil || *sc.RunAsUser == 0 {
		t.Error("pod runAsUser is root or unset, want a non-zero UID")
	}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Error("pod seccompProfile is not RuntimeDefault")
	}
	if sc.FSGroup == nil {
		t.Error("pod fsGroup is unset, want the shared emptyDir group-writable")
	}

	// Resource caps on both containers so a build cannot starve the node (ADR-0053 Consequences).
	for _, c := range []corev1.Container{clone, build} {
		if c.SecurityContext == nil {
			t.Fatalf("container %q securityContext is nil", c.Name)
		}
		if c.Resources.Limits.Cpu().IsZero() || c.Resources.Limits.Memory().IsZero() {
			t.Errorf("container %q has no CPU/memory limit (a build must not starve the node)", c.Name)
		}
		if c.Resources.Requests.Cpu().IsZero() || c.Resources.Requests.Memory().IsZero() {
			t.Errorf("container %q has no CPU/memory request", c.Name)
		}
	}

	// Every write path is a writable emptyDir so the read-only root filesystem is feasible.
	if len(pod.Volumes) == 0 {
		t.Error("build pod has no volumes, want emptyDir scratch for the read-only root")
	}
	for _, v := range pod.Volumes {
		if v.EmptyDir == nil {
			t.Errorf("volume %q is not an emptyDir", v.Name)
		}
	}
}

// TestBuildContainerSecurityContexts asserts the split: the clone init container keeps the full
// restricted §7 floor, while the build container runs privileged. buildah's layer extraction remounts
// the container root mount private, which a managed CRI (DOKS/containerd) locks — nothing short of
// privileged completes a build there (validated live), so the OSS build container, which builds
// trusted user-owned source, runs privileged; the untrusted-source case is hardened behind the
// Builder seam, not here (issue #282, supersedes ADR-0056's narrower relaxation).
func TestBuildContainerSecurityContexts(t *testing.T) {
	ctx := context.Background()
	source := controlplane.SourceRef{Repo: "https://github.com/acme/shop", Ref: "v1"}
	const target = "reg.burrow.svc/acme/shop:1"
	client, created := buildFakeSucceeding(t, source, target, validDigest)

	if _, err := NewBuilder(client).Build(ctx, source, target, false, controlplane.SourceCredential{}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	job := (*created)[0]
	clone := job.Spec.Template.Spec.InitContainers[0].SecurityContext
	build := job.Spec.Template.Spec.Containers[0].SecurityContext

	// Clone keeps the tightest floor.
	if clone.AllowPrivilegeEscalation == nil || *clone.AllowPrivilegeEscalation {
		t.Error("clone container allows privilege escalation, want the restricted floor")
	}
	if clone.SeccompProfile != nil {
		t.Errorf("clone container overrides seccomp (%v), want the pod-level RuntimeDefault floor", clone.SeccompProfile)
	}
	if clone.Capabilities == nil || len(clone.Capabilities.Add) != 0 {
		t.Errorf("clone container adds capabilities %v, want none", clone.Capabilities)
	}
	if !hasCapability(clone.Capabilities.Drop, "ALL") {
		t.Error("clone container does not drop ALL capabilities")
	}

	// Build runs privileged — the only context that completes a buildah layer-apply on a managed CRI
	// whose container root mount is locked (validated live on DOKS).
	if build.Privileged == nil || !*build.Privileged {
		t.Error("build container is not privileged; buildah's layer remount is denied without it on a managed CRI (issue #282)")
	}
	if build.SeccompProfile == nil || build.SeccompProfile.Type != corev1.SeccompProfileTypeUnconfined {
		t.Errorf("build seccompProfile = %v, want Unconfined", build.SeccompProfile)
	}
	// The read-only root filesystem is NOT kept: buildah remounts the root mount during layer
	// extraction, which requires it to be writable.
	if build.ReadOnlyRootFilesystem == nil || *build.ReadOnlyRootFilesystem {
		t.Error("build container root filesystem must be writable so buildah can remount it during layer extraction")
	}
}

// TestBuildDoesNotCreateBuildNamespace asserts burrowd never creates the build namespace itself.
// `burrow install` provisions burrow-builds and burrowd's Role in it kubeconfig-side, because burrowd
// holds only namespaced Roles and cannot create namespaces or cluster RBAC (least privilege, issue
// #278) — the same reason `burrow env add` creates per-environment namespaces kubeconfig-side. A
// build that tried to create its own namespace would need a cluster-scoped grant it must not hold.
func TestBuildDoesNotCreateBuildNamespace(t *testing.T) {
	ctx := context.Background()
	source := controlplane.SourceRef{Repo: "https://github.com/acme/shop", Ref: "v1"}
	const target = "reg.burrow.svc/acme/shop:1"
	client, _ := buildFakeSucceeding(t, source, target, validDigest)

	if _, err := NewBuilder(client).Build(ctx, source, target, false, controlplane.SourceCredential{}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, a := range client.Actions() {
		if a.GetVerb() == "create" && a.GetResource().Resource == "namespaces" {
			t.Errorf("burrowd created a namespace; install must provision %q, burrowd holds only namespaced Roles (issue #278)", buildNamespace)
		}
	}
}

// TestBuildJobTTL asserts the Job carries ttlSecondsAfterFinished so BOTH succeeded and failed build
// Jobs are reaped by the TTL controller and do not accumulate (issue #280).
func TestBuildJobTTL(t *testing.T) {
	ctx := context.Background()
	source := controlplane.SourceRef{Repo: "https://github.com/acme/shop", Ref: "v1"}
	const target = "reg.burrow.svc/acme/shop:1"
	client, created := buildFakeSucceeding(t, source, target, validDigest)

	if _, err := NewBuilder(client).Build(ctx, source, target, false, controlplane.SourceCredential{}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	ttl := (*created)[0].Spec.TTLSecondsAfterFinished
	if ttl == nil {
		t.Fatal("build Job has no ttlSecondsAfterFinished; failed Jobs would accumulate (issue #280)")
	}
	if *ttl != buildJobTTLSeconds {
		t.Errorf("ttlSecondsAfterFinished = %d, want %d (3 days)", *ttl, buildJobTTLSeconds)
	}
}

// TestBuildFailsFastWhenNoHeadroom asserts that when the capacity prober reports no schedulable room
// for a build, the build is refused with an actionable error and NO Job is created — rather than a
// Job that hangs Pending (issue #274).
func TestBuildFailsFastWhenNoHeadroom(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	var created int
	client.PrependReactor("create", "jobs", func(clienttesting.Action) (bool, runtime.Object, error) {
		created++
		return false, nil, nil
	})

	b := NewBuilder(client).WithCapacityProber(stubCapacity{state: noHeadroom()})
	_, err := b.Build(ctx, controlplane.SourceRef{Repo: "https://github.com/acme/shop", Ref: "v1"}, "reg/acme/shop:1", false, controlplane.SourceCredential{})
	if err == nil {
		t.Fatal("Build should fail fast when no node has room for the build")
	}
	if !strings.Contains(err.Error(), "cannot be scheduled") {
		t.Errorf("error = %q, want it to say the build cannot be scheduled", err)
	}
	// The message must be actionable: name the shortfall and tell the user to add capacity.
	if !strings.Contains(err.Error(), "Add a node") {
		t.Errorf("error = %q, want an actionable 'add a node' remedy", err)
	}
	if created != 0 {
		t.Errorf("created %d jobs, want 0 — nothing should be created when the build cannot schedule", created)
	}
}

// TestBuildProceedsWithHeadroom asserts the pre-flight is a gate only when the build cannot fit: with
// a prober reporting ample room, the build runs to completion as normal (issue #274).
func TestBuildProceedsWithHeadroom(t *testing.T) {
	ctx := context.Background()
	source := controlplane.SourceRef{Repo: "https://github.com/acme/shop", Ref: "v1"}
	const target = "reg.burrow.svc/acme/shop:1"
	client, created := buildFakeSucceeding(t, source, target, validDigest)

	b := NewBuilder(client).WithCapacityProber(stubCapacity{state: ampleHeadroom()})
	digest, err := b.Build(ctx, source, target, false, controlplane.SourceCredential{})
	if err != nil {
		t.Fatalf("Build with ample headroom should proceed: %v", err)
	}
	if digest != validDigest {
		t.Errorf("digest = %q, want %q", digest, validDigest)
	}
	if len(*created) != 1 {
		t.Errorf("created %d jobs, want 1", len(*created))
	}
}

// TestBuildCapacityReadErrorDoesNotBlock asserts a capacity read failure does NOT break the build:
// the pre-flight is best-effort, so a misconfigured capacity read must not stop builds (issue #274).
func TestBuildCapacityReadErrorDoesNotBlock(t *testing.T) {
	ctx := context.Background()
	source := controlplane.SourceRef{Repo: "https://github.com/acme/shop", Ref: "v1"}
	const target = "reg.burrow.svc/acme/shop:1"
	client, created := buildFakeSucceeding(t, source, target, validDigest)

	b := NewBuilder(client).WithCapacityProber(stubCapacity{err: errors.New("forbidden: cannot list nodes")})
	if _, err := b.Build(ctx, source, target, false, controlplane.SourceCredential{}); err != nil {
		t.Fatalf("a capacity read error must not block the build: %v", err)
	}
	if len(*created) != 1 {
		t.Errorf("created %d jobs, want 1 (the build proceeds despite the capacity read error)", len(*created))
	}
}

// TestBuildInsecurePush asserts that an insecure build (the plain-HTTP in-cluster registry, ADR-0054
// §5) carries the TARGET_INSECURE=true hint the buildScript reads to push with --tls-verify=false.
func TestBuildInsecurePush(t *testing.T) {
	ctx := context.Background()
	source := controlplane.SourceRef{Repo: "https://github.com/acme/shop", Ref: "v1.2.3"}
	const target = "burrow-registry.burrow.svc.cluster.local:5000/shop:build"
	client, created := buildFakeSucceeding(t, source, target, validDigest)

	b := NewBuilder(client)
	if _, err := b.Build(ctx, source, target, true, controlplane.SourceCredential{}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(*created) != 1 {
		t.Fatalf("created %d jobs, want 1", len(*created))
	}
	build := (*created)[0].Spec.Template.Spec.Containers[0]
	if got := envValue(build.Env, "TARGET_INSECURE"); got != "true" {
		t.Errorf("build env TARGET_INSECURE = %q, want %q for the plain-HTTP in-cluster push", got, "true")
	}
}

// TestBuildReadsDigestAndReaps asserts a successful build returns the digest and reaps its Job.
func TestBuildReadsDigestAndReaps(t *testing.T) {
	ctx := context.Background()
	source := controlplane.SourceRef{Repo: "https://github.com/acme/shop", Ref: "abc123"}
	const target = "reg.burrow.svc/acme/shop:2"
	client, _ := buildFakeSucceeding(t, source, target, validDigest)

	var deleted bool
	client.PrependReactor("delete", "jobs", func(a clienttesting.Action) (bool, runtime.Object, error) {
		deleted = true
		return true, nil, nil
	})

	b := NewBuilder(client)
	digest, err := b.Build(ctx, source, target, false, controlplane.SourceCredential{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if digest != validDigest {
		t.Errorf("digest = %q, want %q", digest, validDigest)
	}
	if !deleted {
		t.Error("a successful build did not reap its Job")
	}
}

// TestBuildJobFailure asserts a Failed build Job becomes a structured error and no digest.
func TestBuildJobFailure(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	client.PrependReactor("get", "jobs", func(a clienttesting.Action) (bool, runtime.Object, error) {
		name := a.(clienttesting.GetAction).GetName()
		return true, &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: buildNamespace},
			Status:     batchv1.JobStatus{Failed: 1},
		}, nil
	})

	b := NewBuilder(client)
	digest, err := b.Build(ctx, controlplane.SourceRef{Repo: "https://github.com/acme/shop", Ref: "v1"}, "reg/acme/shop:1", false, controlplane.SourceCredential{})
	if err == nil {
		t.Fatal("Build should return an error when the Job fails")
	}
	if digest != "" {
		t.Errorf("digest = %q, want empty on failure", digest)
	}
}

// TestBuildSuccessNoDigest asserts a Job that reports success but wrote no digest is a build failure
// rather than a deploy pinned to nothing.
func TestBuildSuccessNoDigest(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	client.PrependReactor("get", "jobs", func(a clienttesting.Action) (bool, runtime.Object, error) {
		name := a.(clienttesting.GetAction).GetName()
		return true, &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: buildNamespace},
			Status:     batchv1.JobStatus{Succeeded: 1},
		}, nil
	})
	// No pod seeded, so there is no termination-log digest to read.

	b := NewBuilder(client)
	if _, err := b.Build(ctx, controlplane.SourceRef{Repo: "https://github.com/acme/shop", Ref: "v1"}, "reg/acme/shop:1", false, controlplane.SourceCredential{}); err == nil {
		t.Fatal("Build should error when a succeeded Job produced no digest")
	}
}

// TestBuildContextCancel asserts a still-running build honors context cancellation instead of
// blocking to the timeout.
func TestBuildContextCancel(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("get", "jobs", func(a clienttesting.Action) (bool, runtime.Object, error) {
		name := a.(clienttesting.GetAction).GetName()
		// Never terminal: the Job is still running, so the wait loop must fall through to the select.
		return true, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: buildNamespace}}, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled: the first loop turn must return ctx.Err()

	b := NewBuilder(client)
	if _, err := b.Build(ctx, controlplane.SourceRef{Repo: "https://github.com/acme/shop", Ref: "v1"}, "reg/acme/shop:1", false, controlplane.SourceCredential{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Build err = %v, want context.Canceled", err)
	}
}

// TestBuildValidatesInputBeforeAnyJob asserts a malformed source or empty target is rejected as
// ErrInvalid before any Job is created.
func TestBuildValidatesInputBeforeAnyJob(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		source controlplane.SourceRef
		target string
	}{
		{"empty repo", controlplane.SourceRef{Ref: "v1"}, "reg/app:1"},
		{"empty ref", controlplane.SourceRef{Repo: "https://github.com/acme/shop"}, "reg/app:1"},
		{"empty target", controlplane.SourceRef{Repo: "https://github.com/acme/shop", Ref: "v1"}, "  "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			b := NewBuilder(client)
			_, err := b.Build(ctx, tc.source, tc.target, false, controlplane.SourceCredential{})
			if !errors.Is(err, controlplane.ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
			jobs, _ := client.BatchV1().Jobs(buildNamespace).List(ctx, metav1.ListOptions{})
			if len(jobs.Items) != 0 {
				t.Errorf("created %d jobs for invalid input, want 0", len(jobs.Items))
			}
		})
	}
}

// TestParseDigest covers the termination-log digest parsing: a well-formed sha256 is accepted
// (trimming a trailing newline), and anything else is rejected as unknown.
func TestParseDigest(t *testing.T) {
	if got := parseDigest(validDigest + "\n"); got != validDigest {
		t.Errorf("parseDigest(valid) = %q, want %q", got, validDigest)
	}
	for _, bad := range []string{"", "not-a-digest", "sha256:short", "sha256:" + strings.Repeat("z", 64), "Successfully built abc"} {
		if got := parseDigest(bad); got != "" {
			t.Errorf("parseDigest(%q) = %q, want empty", bad, got)
		}
	}
}

// TestBuildJobNameDeterministic asserts an identical source+target maps to the same Job name (so a
// re-run is idempotent) and a different one to a different name.
func TestBuildJobNameDeterministic(t *testing.T) {
	src := controlplane.SourceRef{Repo: "https://github.com/acme/shop", Ref: "v1"}
	a := buildJobName(src, "reg/app:1")
	if b := buildJobName(src, "reg/app:1"); a != b {
		t.Errorf("same input gave different names %q vs %q", a, b)
	}
	if c := buildJobName(src, "reg/app:2"); a == c {
		t.Errorf("different target gave the same name %q", a)
	}
	if !strings.HasPrefix(a, "burrow-build-") {
		t.Errorf("name %q lacks the burrow-build- prefix", a)
	}
}

// TestBuilderImageForVersion asserts the version pin: a stamped release version maps to the builder
// image published under the same tag (reproducible), while an unstamped dev build ("" or "v0.0.0")
// maps to "" so the caller keeps the floating :latest default.
func TestBuilderImageForVersion(t *testing.T) {
	for _, unstamped := range []string{"", "v0.0.0"} {
		if got := BuilderImageForVersion(unstamped); got != "" {
			t.Errorf("BuilderImageForVersion(%q) = %q, want empty (keep :latest default)", unstamped, got)
		}
	}
	if got, want := BuilderImageForVersion("v0.13.0"), "ghcr.io/burrow-cloud/burrow-builder:v0.13.0"; got != want {
		t.Errorf("BuilderImageForVersion(v0.13.0) = %q, want %q", got, want)
	}
}

// envValue returns the value of the named env var, or "" if absent.
func envValue(env []corev1.EnvVar, name string) string {
	for _, e := range env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

// hasCapability reports whether caps contains want.
func hasCapability(caps []corev1.Capability, want corev1.Capability) bool {
	for _, c := range caps {
		if c == want {
			return true
		}
	}
	return false
}

// assertNoSourceBytes is a guard for the load-bearing invariant that no source code crosses into the
// Job (ADR-0004, ADR-0053 §3): the Job carries only the git ref, the repo URL, and the target
// reference — the source is cloned inside the cluster. There is no ConfigMap/inline volume of source
// and no command payload beyond the fixed clone/build scripts.
func assertNoSourceBytes(t *testing.T, job *batchv1.Job) {
	t.Helper()
	// The credential Secret (ADR-0057) is the ONE non-emptyDir volume a build may carry: it holds a
	// provider token, never source. Everything else must be an emptyDir scratch — no ConfigMap or
	// Secret injecting source bytes into the build.
	credVolumes := map[string]bool{"git-creds": true, "registry-auth": true}
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.ConfigMap != nil {
			t.Errorf("Job volume %q injects data into the build; source must be cloned in-cluster, not carried", v.Name)
		}
		if v.Secret != nil {
			if !credVolumes[v.Name] {
				t.Errorf("Job volume %q is an unexpected Secret; only the source-provider credential may be mounted", v.Name)
			}
			continue
		}
		if v.EmptyDir == nil {
			t.Errorf("Job volume %q is not an emptyDir; a build carries no source payload", v.Name)
		}
	}
}

// TestBuildWithSourceCredential asserts the in-cluster build consumes a source-provider credential
// by MOUNTING it, never by passing it (ADR-0057 §4): a Secret in the build namespace holds the token
// (as a git url.insteadOf rewrite and a docker config.json), the clone points GIT_CONFIG_GLOBAL at
// it, buildah points REGISTRY_AUTH_FILE at it, the Secret is owned by the Job so it is
// garbage-collected with it — and the token appears NOWHERE in the Job spec.
func TestBuildWithSourceCredential(t *testing.T) {
	ctx := context.Background()
	source := controlplane.SourceRef{Repo: "https://github.com/acme/private", Ref: "v1.2.3"}
	const target = "ghcr.io/acme/private:1.2.3"
	const token = "ghp_super_secret_build_token"
	client, created := buildFakeSucceeding(t, source, target, validDigest)

	cred := controlplane.SourceCredential{Provider: controlplane.ProviderGitHub, Token: token}
	if _, err := NewBuilder(client).Build(ctx, source, target, false, cred); err != nil {
		t.Fatalf("Build: %v", err)
	}
	job := (*created)[0]
	secretName := credSecretName(job.Name)

	// The credential Secret was created in the build namespace with both materializations of the token.
	secret, err := client.CoreV1().Secrets(buildNamespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("credential secret %q not created: %v", secretName, err)
	}
	gitcfg := string(secret.Data[gitConfigFile])
	if !strings.Contains(gitcfg, token) {
		t.Errorf("gitconfig does not carry the token for the private clone:\n%s", gitcfg)
	}
	if !strings.Contains(gitcfg, "insteadOf") || !strings.Contains(gitcfg, "github.com") {
		t.Errorf("gitconfig is not a github url.insteadOf rewrite:\n%s", gitcfg)
	}
	dockercfg := string(secret.Data[registryAuthFile])
	wantAuth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	if !strings.Contains(dockercfg, wantAuth) {
		t.Errorf("docker config.json does not carry the base64 registry auth:\n%s", dockercfg)
	}
	if !strings.Contains(dockercfg, "ghcr.io") {
		t.Errorf("docker config.json does not target the provider registry ghcr.io:\n%s", dockercfg)
	}

	// The Secret is owned by the Job so Kubernetes garbage-collects it when the Job is reaped.
	if len(secret.OwnerReferences) != 1 || secret.OwnerReferences[0].Kind != "Job" || secret.OwnerReferences[0].Name != job.Name {
		t.Errorf("credential secret ownerReferences = %+v, want a single Job owner %q", secret.OwnerReferences, job.Name)
	}

	// The clone mounts the gitconfig and points GIT_CONFIG_GLOBAL at it.
	clone := job.Spec.Template.Spec.InitContainers[0]
	if got := envValue(clone.Env, "GIT_CONFIG_GLOBAL"); got != gitCredsPath+"/"+gitConfigFile {
		t.Errorf("clone GIT_CONFIG_GLOBAL = %q, want the mounted gitconfig", got)
	}
	if !mountsVolume(clone.VolumeMounts, "git-creds") {
		t.Error("clone does not mount the git-creds volume")
	}
	// The build mounts the docker config and points REGISTRY_AUTH_FILE at it.
	build := job.Spec.Template.Spec.Containers[0]
	if got := envValue(build.Env, "REGISTRY_AUTH_FILE"); got != registryAuthPath+"/"+registryAuthFile {
		t.Errorf("build REGISTRY_AUTH_FILE = %q, want the mounted docker config", got)
	}
	if !mountsVolume(build.VolumeMounts, "registry-auth") {
		t.Error("build does not mount the registry-auth volume")
	}

	// The invariant that matters: the token is NOT in the Job spec anywhere — not an env value, not a
	// command, not a volume. It lives only in the separate Secret object (asserted above).
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal job: %v", err)
	}
	if strings.Contains(string(raw), token) {
		t.Error("the source token leaked into the build Job spec; it must live only in the mounted Secret")
	}
	// The credential mounts are the only Secret volumes; no source bytes are carried.
	assertNoSourceBytes(t, job)
}

// TestBuildWithoutCredentialCreatesNoSecret asserts the public-source path is unchanged: with the
// zero credential, no credential Secret is created and no credential volume is mounted (ADR-0057).
func TestBuildWithoutCredentialCreatesNoSecret(t *testing.T) {
	ctx := context.Background()
	source := controlplane.SourceRef{Repo: "https://github.com/acme/public", Ref: "v1"}
	const target = "ghcr.io/acme/public:1"
	client, created := buildFakeSucceeding(t, source, target, validDigest)

	if _, err := NewBuilder(client).Build(ctx, source, target, false, controlplane.SourceCredential{}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	job := (*created)[0]
	if _, err := client.CoreV1().Secrets(buildNamespace).Get(ctx, credSecretName(job.Name), metav1.GetOptions{}); err == nil {
		t.Error("a public build created a credential Secret; it must not")
	}
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Secret != nil {
			t.Errorf("public build mounts a Secret volume %q; want none", v.Name)
		}
	}
}

// TestBuildNoPodMutatorLeavesDefault guards the backward-compatible default of the ADR-0053 §6
// executor extension point (WithBuildPodMutator): with no mutator wired, the build Job is byte-for-byte
// the OSS default — the build container stays privileged and the pod carries no runtimeClassName. This
// is the safety net against an accidental behavior change from adding the hook.
func TestBuildNoPodMutatorLeavesDefault(t *testing.T) {
	ctx := context.Background()
	source := controlplane.SourceRef{Repo: "https://github.com/acme/shop", Ref: "v1"}
	const target = "reg.burrow.svc/acme/shop:1"
	client, created := buildFakeSucceeding(t, source, target, validDigest)

	if _, err := NewBuilder(client).Build(ctx, source, target, false, controlplane.SourceCredential{}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	pod := (*created)[0].Spec.Template.Spec
	if pod.RuntimeClassName != nil {
		t.Errorf("pod runtimeClassName = %q, want nil (OSS runs no RuntimeClass, ADR-0059)", *pod.RuntimeClassName)
	}
	if pod.ActiveDeadlineSeconds != nil {
		t.Errorf("pod activeDeadlineSeconds = %d, want nil (OSS sets no build deadline)", *pod.ActiveDeadlineSeconds)
	}
	build := pod.Containers[0].SecurityContext
	if build.Privileged == nil || !*build.Privileged {
		t.Error("build container is not privileged; the default (no mutator) must leave OSS behavior unchanged (ADR-0059)")
	}
}

// TestBuildPodMutatorApplied asserts the ADR-0053 §6 executor extension point is honored: a mutator
// standing in for the managed product's hardened executor (cloud ADR-0003) can set a gVisor
// RuntimeClass, override the build container's security context to a non-privileged restricted one,
// and set a hard build deadline — and all of it reaches the created Job. This is enabling API for the
// managed product; OSS itself wires no mutator.
func TestBuildPodMutatorApplied(t *testing.T) {
	ctx := context.Background()
	source := controlplane.SourceRef{Repo: "https://github.com/acme/shop", Ref: "v1"}
	const target = "reg.burrow.svc/acme/shop:1"
	const runtimeClass = "gvisor"
	const deadline int64 = 900
	client, created := buildFakeSucceeding(t, source, target, validDigest)

	mutator := func(spec *corev1.PodSpec) {
		rc := runtimeClass
		d := deadline
		spec.RuntimeClassName = &rc
		spec.ActiveDeadlineSeconds = &d
		for i := range spec.Containers {
			if spec.Containers[i].Name == buildContainerName {
				spec.Containers[i].SecurityContext = &corev1.SecurityContext{
					Privileged:               boolPtr(false),
					AllowPrivilegeEscalation: boolPtr(false),
					ReadOnlyRootFilesystem:   boolPtr(true),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				}
			}
		}
	}

	b := NewBuilder(client).WithBuildPodMutator(mutator)
	if _, err := b.Build(ctx, source, target, false, controlplane.SourceCredential{}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	pod := (*created)[0].Spec.Template.Spec
	if pod.RuntimeClassName == nil || *pod.RuntimeClassName != runtimeClass {
		t.Errorf("pod runtimeClassName = %v, want %q (the mutator's gVisor class)", pod.RuntimeClassName, runtimeClass)
	}
	if pod.ActiveDeadlineSeconds == nil || *pod.ActiveDeadlineSeconds != deadline {
		t.Errorf("pod activeDeadlineSeconds = %v, want %d (the mutator's build deadline)", pod.ActiveDeadlineSeconds, deadline)
	}
	build := pod.Containers[0].SecurityContext
	if build.Privileged == nil || *build.Privileged {
		t.Error("mutator's non-privileged build container was not applied; the hook must override the security context")
	}
	if build.ReadOnlyRootFilesystem == nil || !*build.ReadOnlyRootFilesystem {
		t.Error("mutator's read-only root filesystem was not applied")
	}
	if build.AllowPrivilegeEscalation == nil || *build.AllowPrivilegeEscalation {
		t.Error("mutator's no-privilege-escalation was not applied")
	}
}

// TestBuildNamespaceConfigurable asserts WithBuildNamespace overrides where the build Job and its
// credential Secret are created, while the default (no override) still lands in the dedicated
// burrow-builds namespace. This parameterizes what is otherwise a constant for downstream callers
// (the managed product's per-tenant build namespaces, cloud ADR-0003) without changing OSS behavior.
func TestBuildNamespaceConfigurable(t *testing.T) {
	ctx := context.Background()
	source := controlplane.SourceRef{Repo: "https://github.com/acme/private", Ref: "v1.2.3"}
	const target = "ghcr.io/acme/private:1.2.3"
	const ns = "t-abc-build"
	cred := controlplane.SourceCredential{Provider: controlplane.ProviderGitHub, Token: "ghp_token"}

	// A fake whose build Job is observed Succeeded in the OVERRIDE namespace, with a terminated pod
	// there carrying the digest so the read-back finds it.
	client := fake.NewSimpleClientset()
	created := &[]*batchv1.Job{}
	client.PrependReactor("create", "jobs", func(a clienttesting.Action) (bool, runtime.Object, error) {
		*created = append(*created, a.(clienttesting.CreateAction).GetObject().(*batchv1.Job).DeepCopy())
		return false, nil, nil
	})
	client.PrependReactor("get", "jobs", func(a clienttesting.Action) (bool, runtime.Object, error) {
		name := a.(clienttesting.GetAction).GetName()
		return true, &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Status:     batchv1.JobStatus{Succeeded: 1},
		}, nil
	})
	jobName := buildJobName(source, target)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: jobName + "-abc", Namespace: ns,
			Labels: map[string]string{nameLabel: jobName},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  buildContainerName,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: validDigest + "\n"}},
			}},
		},
	}
	if _, err := client.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed pod: %v", err)
	}

	if _, err := NewBuilder(client).WithBuildNamespace(ns).Build(ctx, source, target, false, cred); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(*created) != 1 {
		t.Fatalf("created %d jobs, want 1", len(*created))
	}
	if got := (*created)[0].Namespace; got != ns {
		t.Errorf("job namespace = %q, want the override %q", got, ns)
	}
	// The credential Secret lands in the SAME override namespace, not the default.
	if _, err := client.CoreV1().Secrets(ns).Get(ctx, credSecretName(jobName), metav1.GetOptions{}); err != nil {
		t.Errorf("credential secret not created in the override namespace %q: %v", ns, err)
	}
	if _, err := client.CoreV1().Secrets(buildNamespace).Get(ctx, credSecretName(jobName), metav1.GetOptions{}); err == nil {
		t.Errorf("credential secret leaked into the default namespace %q", buildNamespace)
	}

	// The default (no WithBuildNamespace) still lands in burrow-builds.
	defSource := controlplane.SourceRef{Repo: "https://github.com/acme/shop", Ref: "v1"}
	const defTarget = "reg.burrow.svc/acme/shop:1"
	defClient, defCreated := buildFakeSucceeding(t, defSource, defTarget, validDigest)
	if _, err := NewBuilder(defClient).Build(ctx, defSource, defTarget, false, controlplane.SourceCredential{}); err != nil {
		t.Fatalf("default Build: %v", err)
	}
	if got := (*defCreated)[0].Namespace; got != buildNamespace {
		t.Errorf("default job namespace = %q, want %q", got, buildNamespace)
	}
}

// mountsVolume reports whether mounts includes one named name.
func mountsVolume(mounts []corev1.VolumeMount, name string) bool {
	for _, m := range mounts {
		if m.Name == name {
			return true
		}
	}
	return false
}
