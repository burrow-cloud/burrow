// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/burrow-cloud/burrow/controlplane"
)

var _ controlplane.Builder = (*BuildAdapter)(nil)

const (
	// defaultGitImage is the image the clone init container runs. It only needs `git`; a minimal
	// git image keeps the pull small. Phase 3's install wiring (ADR-0053 §5) may override it.
	defaultGitImage = "alpine/git:2.45.2"
	// defaultBuildImage is the image the build container runs. It bundles BOTH builders the ADR
	// names (ADR-0053 §4) — buildah (for the Dockerfile case) and the Cloud Native Buildpacks
	// lifecycle (for the no-Dockerfile case) — so a single Job can choose between them at runtime,
	// after the source is cloned, without the control plane ever inspecting the source (§3). Phase 3
	// wires its install and can override it via WithBuildImage; it is a constant here so the adapter
	// and its unit tests are self-contained.
	defaultBuildImage = "ghcr.io/burrow-cloud/burrow-builder:latest"

	// workspacePath is the shared emptyDir the init container clones into and the build container
	// reads. It is the only place source bytes ever live — inside the cluster, never on the control
	// plane (ADR-0004, ADR-0053 §3).
	workspacePath = "/workspace"
	// buildHomePath backs $HOME for the rootless build so buildah's container storage and the CNB
	// lifecycle's scratch land on a writable emptyDir, letting the container root filesystem stay
	// read-only (defense in depth, ADR-0053 §7).
	buildHomePath = "/home/build"
	// buildTmpPath backs /tmp for the same reason.
	buildTmpPath = "/tmp"

	// buildContainerName / cloneContainerName are fixed names (not derived from any app or ref) so
	// the digest read-back never depends on caller input.
	buildContainerName = "build"
	cloneContainerName = "clone"

	// buildJobTimeout caps how long burrowd waits for an in-cluster build to finish. A build is
	// slower than a run or a pg_dump — a cold buildpacks build pulls a builder and a runtime — so it
	// gets a longer ceiling.
	buildJobTimeout = 30 * time.Minute
	// buildJobPoll is the interval between Job-status reads while waiting.
	buildJobPoll = 3 * time.Second

	// buildUID/buildGID are the non-root user/group the build runs as. fsGroup is set to the same
	// GID so the shared emptyDir is group-writable and the clone and build steps can both write it.
	buildUID int64 = 1000
	buildGID int64 = 1000
)

// cloneScript clones the git reference INTO the cluster (ADR-0053 §3). The repository URL and ref
// arrive as environment variables (REPO, REF), never interpolated into the script, so a crafted
// value cannot inject shell — the values are data, not code. A shallow fetch of the exact ref keeps
// the clone small and works for a commit SHA, a tag, or a branch.
const cloneScript = `set -eu
git init -q ` + workspacePath + `
git -C ` + workspacePath + ` remote add origin "$REPO"
git -C ` + workspacePath + ` fetch --depth 1 origin "$REF"
git -C ` + workspacePath + ` checkout -q FETCH_HEAD`

// buildScript chooses the builder AFTER the clone, from the cloned tree — the control plane never
// inspects the source to decide (ADR-0053 §3/§4). A Dockerfile means buildah (a daemonless,
// rootless build of the user's own recipe); its absence means the Cloud Native Buildpacks lifecycle
// (which detects the language and needs no recipe). Either way the image is pushed to $TARGET_IMAGE
// and its content digest is written to the pod's termination-log, where the adapter reads it back
// without mounting anything (the same channel RunBackupJob uses for the dump size).
//
// $TARGET_INSECURE marks the push target as a plain-HTTP registry (the in-cluster registry, ADR-0054
// §5): the buildah push then passes --tls-verify=false, which both skips certificate verification and
// lets containers/image fall back to plain HTTP, so no extra transport hint is needed. It applies ONLY
// to the push to $TARGET_IMAGE — the `bud` base-image pull keeps TLS defaults, so pulling a base image
// from an external registry stays verified. The Cloud Native Buildpacks lifecycle has no equivalent
// insecure-push handling wired yet, so a no-Dockerfile build to a plain-HTTP registry fails fast with
// an actionable message rather than an obscure TLS error (documented follow-up, ADR-0054 §5).
const buildScript = `set -eu
PUSH_TLS_FLAGS=""
if [ "${TARGET_INSECURE:-}" = "true" ]; then
  PUSH_TLS_FLAGS="--tls-verify=false"
fi
if [ -f ` + workspacePath + `/Dockerfile ]; then
  # Dockerfile present: buildah builds the user's own recipe (ADR-0053 §4).
  buildah --storage-driver vfs bud -t "$TARGET_IMAGE" ` + workspacePath + `
  buildah --storage-driver vfs push $PUSH_TLS_FLAGS --digestfile /tmp/digest "$TARGET_IMAGE" "docker://$TARGET_IMAGE"
  cat /tmp/digest > /dev/termination-log
else
  # No Dockerfile: the Cloud Native Buildpacks lifecycle detects and builds (ADR-0053 §4).
  if [ "${TARGET_INSECURE:-}" = "true" ]; then
    echo "the no-Dockerfile Cloud Native Buildpacks path cannot yet push to the plain-HTTP in-cluster registry; add a Dockerfile, or push to an external registry with an explicit target (buildpacks insecure push is a follow-up, ADR-0054 §5)" >&2
    exit 1
  fi
  /cnb/lifecycle/creator -app=` + workspacePath + ` "$TARGET_IMAGE"
  grep -o 'sha256:[0-9a-f]\{64\}' /layers/report.toml | head -n1 > /dev/termination-log
fi`

// BuildAdapter is the production controlplane.Builder: it runs an in-cluster build as a Kubernetes
// Job in the app's own namespace (ADR-0053 §4). It clones the git reference inside the cluster,
// builds with buildah or Cloud Native Buildpacks, pushes to the target registry reference, and
// returns the resulting image digest — the immutable identity the resulting guarded deploy pins
// (ADR-0053 §4). Isolation lives INSIDE this implementation, not on the seam (ADR-0053 §6): the OSS
// path is single-tenant (§7), so the build runs under restricted PodSecurity as defense in depth,
// not as an adversary boundary — a hardened, sandboxed executor is the commercial product's job
// behind the same seam.
//
// It lives under controlplane/ (not controlplane/internal) so cmd/burrowd and the managed module
// can wire it; it is licensed Apache-2.0.
type BuildAdapter struct {
	client     kubernetes.Interface
	namespace  string
	gitImage   string
	buildImage string
}

// NewBuilder returns a BuildAdapter over the given clientset and namespace (defaulting to
// "default"). Tests inject a fake clientset; production injects a real one (see
// NewBuilderFromConfig). The namespace is the app's own namespace: a build runs beside the workload
// it will become, never in the control-plane namespace (ADR-0053 §4).
func NewBuilder(client kubernetes.Interface, namespace string) *BuildAdapter {
	if namespace == "" {
		namespace = "default"
	}
	return &BuildAdapter{client: client, namespace: namespace, gitImage: defaultGitImage, buildImage: defaultBuildImage}
}

// NewBuilderFromConfig builds a BuildAdapter from a REST config and namespace — the production
// wiring path, mirroring NewFromConfig for the Kubernetes seam.
func NewBuilderFromConfig(cfg *rest.Config, namespace string) (*BuildAdapter, error) {
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kube: building clientset: %w", err)
	}
	return NewBuilder(client, namespace), nil
}

// WithBuildImage overrides the build image (the buildah + Buildpacks bundle). An empty value leaves
// the default. Returns the adapter for chaining.
func (b *BuildAdapter) WithBuildImage(image string) *BuildAdapter {
	if image != "" {
		b.buildImage = image
	}
	return b
}

// WithGitImage overrides the clone init-container image. An empty value leaves the default. Returns
// the adapter for chaining.
func (b *BuildAdapter) WithGitImage(image string) *BuildAdapter {
	if image != "" {
		b.gitImage = image
	}
	return b
}

// Build runs the in-cluster build to completion and returns the pushed image's content digest
// (ADR-0053 §4). Only the git reference and the target reference cross into the builder; the source
// is cloned inside the cluster, so no code travels over the control channel (ADR-0004, ADR-0053 §3).
// A clone, build, or push failure is returned as a structured error and nothing is pushed; the
// caller does NOT touch the deploy path on error (ADR-0053 §4). It blocks until the Job succeeds or
// fails, or the build timeout elapses.
func (b *BuildAdapter) Build(ctx context.Context, source controlplane.SourceRef, targetImage string, insecure bool) (string, error) {
	if err := source.Validate(); err != nil {
		return "", fmt.Errorf("kube: build: %w: %w", controlplane.ErrInvalid, err)
	}
	if strings.TrimSpace(targetImage) == "" {
		return "", fmt.Errorf("kube: build: target image reference is empty: %w", controlplane.ErrInvalid)
	}

	name := buildJobName(source, targetImage)
	job := b.buildJob(name, source, targetImage, insecure)
	jobs := b.client.BatchV1().Jobs(b.namespace)
	if _, err := jobs.Create(ctx, job, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", fmt.Errorf("kube: creating build job %q: %w", name, err)
	}

	deadline := time.Now().Add(buildJobTimeout)
	for {
		j, err := jobs.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", fmt.Errorf("kube: reading build job %q: %w", name, err)
		}
		if j.Status.Failed > 0 {
			// Leave the Job (and its pod logs) for diagnosis; do not reap a failure.
			return "", fmt.Errorf("kube: build job %q failed", name)
		}
		if j.Status.Succeeded > 0 {
			digest := b.jobTerminationDigest(ctx, name)
			if digest == "" {
				// The Job reported success but wrote no digest — treat it as a build failure rather
				// than pinning a deploy to nothing. Leave the Job for diagnosis.
				return "", fmt.Errorf("kube: build job %q reported success but produced no image digest", name)
			}
			// Reap on success: delete the Job and its pods so builds do not accumulate.
			policy := metav1.DeletePropagationBackground
			_ = jobs.Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &policy})
			return digest, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("kube: build job %q did not complete within %s", name, buildJobTimeout)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(buildJobPoll):
		}
	}
}

// buildJobName derives a deterministic Job name from the source and target so an identical build is
// idempotent (a re-run reuses the name) without the adapter needing an injected ID or clock. The
// hash keeps the name short and DNS-safe regardless of how long the repo URL or target reference is.
func buildJobName(source controlplane.SourceRef, targetImage string) string {
	sum := sha256.Sum256([]byte(source.Repo + "\n" + source.Ref + "\n" + targetImage))
	return "burrow-build-" + hex.EncodeToString(sum[:])[:12]
}

// buildJob builds the one-shot build Job (ADR-0053 §4): an init container clones the git ref into a
// shared emptyDir, then the build container detects the Dockerfile, builds with buildah or
// Buildpacks, pushes to targetImage, and writes the digest to its termination-log. Every write path
// (the workspace, $HOME for container storage, /tmp) is a writable emptyDir so the container root
// filesystem can stay read-only (ADR-0053 §7). BackoffLimit 0 with RestartPolicy Never makes a
// single attempt whose outcome is the result — no retry masking a failure.
func (b *BuildAdapter) buildJob(name string, source controlplane.SourceRef, targetImage string, insecure bool) *batchv1.Job {
	labels := map[string]string{nameLabel: name, managedByLabel: managedByValue}
	var backoff int32

	// The repo URL and ref are passed as env, never interpolated into a script, so they are data
	// and cannot inject shell (ADR-0053 §3, §7). Only these two values and the target reference
	// cross into the builder — never source bytes.
	cloneEnv := []corev1.EnvVar{
		{Name: "REPO", Value: source.Repo},
		{Name: "REF", Value: source.Ref},
		// The workspace is a root-owned emptyDir but the clone runs non-root (buildUID), so git's
		// ownership check rejects it ("dubious ownership"). Mark the workspace safe via git's
		// environment-based config, which needs no writable HOME and no script interpolation.
		{Name: "GIT_CONFIG_COUNT", Value: "1"},
		{Name: "GIT_CONFIG_KEY_0", Value: "safe.directory"},
		{Name: "GIT_CONFIG_VALUE_0", Value: workspacePath},
	}
	buildEnv := []corev1.EnvVar{
		{Name: "TARGET_IMAGE", Value: targetImage},
		// $HOME on a writable emptyDir so rootless buildah's storage and the CNB lifecycle scratch
		// have somewhere to write while the container root filesystem stays read-only.
		{Name: "HOME", Value: buildHomePath},
		// vfs storage needs no host mounts or privileges — the rootless, unprivileged path.
		{Name: "STORAGE_DRIVER", Value: "vfs"},
		// chroot isolation is the rootless, daemonless buildah mode (no privileged container).
		{Name: "BUILDAH_ISOLATION", Value: "chroot"},
	}
	if insecure {
		// The push target is the plain-HTTP in-cluster registry (ADR-0054 §5): the buildScript reads
		// this and pushes with --tls-verify=false. Set only when true so an external push stays over
		// TLS by default.
		buildEnv = append(buildEnv, corev1.EnvVar{Name: "TARGET_INSECURE", Value: "true"})
	}

	workspace := corev1.VolumeMount{Name: "workspace", MountPath: workspacePath}
	volumes := []corev1.Volume{
		{Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "home", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: b.namespace, Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:   corev1.RestartPolicyNever,
					SecurityContext: buildPodSecurityContext(),
					InitContainers: []corev1.Container{{
						Name:            cloneContainerName,
						Image:           b.gitImage,
						Command:         []string{"sh", "-c", cloneScript},
						Env:             cloneEnv,
						VolumeMounts:    []corev1.VolumeMount{workspace},
						SecurityContext: buildContainerSecurityContext(),
						Resources:       buildResources(),
					}},
					Containers: []corev1.Container{{
						Name:    buildContainerName,
						Image:   b.buildImage,
						Command: []string{"sh", "-c", buildScript},
						Env:     buildEnv,
						VolumeMounts: []corev1.VolumeMount{
							workspace,
							{Name: "home", MountPath: buildHomePath},
							{Name: "tmp", MountPath: buildTmpPath},
						},
						SecurityContext: buildContainerSecurityContext(),
						Resources:       buildResources(),
					}},
					Volumes: volumes,
				},
			},
		},
	}
}

// buildPodSecurityContext is the pod-level restricted PodSecurity floor for a build (ADR-0053 §7):
// non-root with a fixed unprivileged UID/GID, an fsGroup so the shared emptyDir is group-writable by
// both the clone and build steps, and the RuntimeDefault seccomp profile. This is defense in depth,
// not an adversary boundary — the OSS path is single-tenant and the user owns the source.
func buildPodSecurityContext() *corev1.PodSecurityContext {
	uid, gid := buildUID, buildGID
	return &corev1.PodSecurityContext{
		RunAsNonRoot:   boolPtr(true),
		RunAsUser:      &uid,
		RunAsGroup:     &gid,
		FSGroup:        &gid,
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// buildContainerSecurityContext is the container-level restricted floor: no privilege escalation,
// all Linux capabilities dropped, and a read-only root filesystem (every write path is a writable
// emptyDir, ADR-0053 §7).
func buildContainerSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: boolPtr(false),
		ReadOnlyRootFilesystem:   boolPtr(true),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

// buildResources caps the build's CPU and memory so an in-cluster build cannot starve running
// workloads on a small node (ADR-0053 Consequences). The requests keep it schedulable; the limits
// are the ceiling that protects the node.
func buildResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("250m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2"),
			corev1.ResourceMemory: resource.MustParse("2Gi"),
		},
	}
}

// jobTerminationDigest reads the image digest the build container wrote to /dev/termination-log from
// the terminated container's state message (the same channel RunBackupJob uses for the dump size).
// Best-effort: any miss or a message that is not a sha256 digest yields "" (digest unknown), which
// the caller turns into a build failure rather than pinning a deploy to nothing.
func (b *BuildAdapter) jobTerminationDigest(ctx context.Context, jobName string) string {
	pods, err := b.client.CoreV1().Pods(b.namespace).List(ctx, metav1.ListOptions{LabelSelector: nameLabel + "=" + jobName})
	if err != nil || len(pods.Items) == 0 {
		return ""
	}
	for _, pod := range pods.Items {
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.State.Terminated == nil {
				continue
			}
			if d := parseDigest(cs.State.Terminated.Message); d != "" {
				return d
			}
		}
	}
	return ""
}

// parseDigest extracts a sha256 content digest from a termination-log message, tolerating trailing
// whitespace or a newline. It returns "" when the message is not a well-formed sha256 digest.
func parseDigest(msg string) string {
	msg = strings.TrimSpace(msg)
	if !strings.HasPrefix(msg, "sha256:") {
		return ""
	}
	hexPart := strings.TrimPrefix(msg, "sha256:")
	if len(hexPart) != 64 {
		return ""
	}
	if _, err := hex.DecodeString(hexPart); err != nil {
		return ""
	}
	return msg
}
