// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	batchv1client "k8s.io/client-go/kubernetes/typed/batch/v1"
	"k8s.io/client-go/rest"

	"github.com/burrow-cloud/burrow/controlplane"
)

var _ controlplane.Builder = (*BuildAdapter)(nil)

const (
	// defaultGitImage is the image the clone init container runs. It only needs `git`; a minimal
	// git image keeps the pull small. Phase 3's install wiring (ADR-0053 §5) may override it.
	defaultGitImage = "alpine/git:2.45.2"
	// builderImageRepo is the repository the build container image is published to. The floating
	// :latest tag is the default; a released burrowd pins it to its own stamped version instead (see
	// BuilderImageForVersion) so a build is reproducible.
	builderImageRepo = "ghcr.io/burrow-cloud/burrow-builder"
	// defaultBuildImage is the image the build container runs. It bundles BOTH builders the ADR
	// names (ADR-0053 §4) — buildah (for the Dockerfile case) and the Cloud Native Buildpacks
	// lifecycle (for the no-Dockerfile case) — so a single Job can choose between them at runtime,
	// after the source is cloned, without the control plane ever inspecting the source (§3). Phase 3
	// wires its install and can override it via WithBuildImage; it is a constant here so the adapter
	// and its unit tests are self-contained.
	defaultBuildImage = builderImageRepo + ":latest"

	// workspacePath is the shared emptyDir the init container clones into and the build container
	// reads. It is the only place source bytes ever live — inside the cluster, never on the control
	// plane (ADR-0004, ADR-0053 §3).
	workspacePath = "/workspace"

	// gitCredsPath is where the source-provider credential's gitconfig is mounted into the clone init
	// container (ADR-0057). The clone points GIT_CONFIG_GLOBAL at the file, whose url.insteadOf rewrite
	// injects the token for the provider's git host — so the token authenticates the fetch of a private
	// repo WITHOUT ever appearing as a Job env var, a command-line argument, or a --source value.
	gitCredsPath = "/git-creds"
	// gitConfigFile is the gitconfig filename inside gitCredsPath.
	gitConfigFile = "gitconfig"
	// registryAuthPath is where the source-provider credential's docker config.json is mounted into the
	// build container (ADR-0057 §4). buildah reads $REGISTRY_AUTH_FILE from here to authenticate the
	// push and any private base-image pull against the provider's registry (ghcr.io, registry.gitlab.com).
	registryAuthPath = "/registry-auth"
	// registryAuthFile is the docker config.json filename inside registryAuthPath.
	registryAuthFile = "config.json"
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

	// buildNamespace is the dedicated namespace the in-cluster build Job runs in — isolated from BOTH
	// the app namespace (where running workloads and their Secrets live) and the control-plane
	// namespace (where burrowd, Postgres, and the cluster credentials live). The build executes the
	// user's own source build steps (Dockerfile directives, dependency-install scripts), so it must not
	// share a namespace with running apps: a build in the app namespace could reach another app's
	// Secret. A dedicated namespace scopes the build's RBAC and Secret reach to nothing but the build
	// itself (issue #278, ADR-0053 §7), and is the natural seam for the commercial product's sandboxed
	// executor (cloud ADR-0003). It is deliberately NOT the control-plane namespace — running build
	// code there would let it reach burrowd's ServiceAccount, Secrets, and database, weaker isolation.
	// Any imagePullSecret / registry-auth Secret a build needs is created HERE, never in the app
	// namespace.
	buildNamespace = "burrow-builds"

	// buildJobTTLSeconds is how long a finished build Job (succeeded OR failed) lingers before
	// Kubernetes' TTL-after-finished controller reaps it and its pods. The maintainer's chosen
	// retention is a few days; three days keeps a recent failure inspectable without leaking Jobs
	// forever. It is the UNIFORM backstop that fixes failed-Job accumulation (issue #280) — a success
	// is still reaped immediately for a clean cluster, and the TTL covers the failures the wait loop
	// deliberately leaves behind for diagnosis.
	buildJobTTLSeconds int32 = 3 * 24 * 60 * 60 // 259200s = 3 days
)

// BuilderImageForVersion returns the pinned builder image reference for a stamped release
// version, so a released burrowd pulls the builder image published under the SAME release tag
// (reproducible) rather than the floating :latest. For an unstamped dev build (version "" or
// "v0.0.0") it returns "" — the caller then leaves the :latest default (or an explicit
// BURROW_BUILD_IMAGE override) in place.
func BuilderImageForVersion(version string) string {
	if version == "" || version == "v0.0.0" {
		return ""
	}
	return builderImageRepo + ":" + version
}

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
# Rootless buildah keeps its container storage (graphroot) and its runtime state (runroot) on the
# writable $HOME emptyDir so the container root filesystem stays read-only (ADR-0053 §7). The builder
# image's default storage.conf points runroot at /var/tmp/storage-run-$UID, which buildah validates at
# startup and refuses because it is not writable by the current user ALONE (the pod's fsGroup leaves
# the mounted dirs group-writable). So point buildah at a private storage.conf whose graphroot/runroot
# live under $HOME, created explicitly and locked to 0700 — this is applied at config-load time, before
# any --root/--runroot flag, so it overrides the image default cleanly.
STORE="$HOME/.local/share/containers/storage"
RUNROOT="$HOME/.local/share/containers/runroot"
mkdir -p "$STORE" "$RUNROOT" "$XDG_RUNTIME_DIR"
chmod 700 "$HOME/.local/share/containers" "$STORE" "$RUNROOT" "$XDG_RUNTIME_DIR"
export CONTAINERS_STORAGE_CONF="$HOME/storage.conf"
printf '[storage]\ndriver = "vfs"\ngraphroot = "%s"\nrunroot = "%s"\n' "$STORE" "$RUNROOT" > "$CONTAINERS_STORAGE_CONF"
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
// Job in the dedicated burrow-builds namespace (issue #278, ADR-0053 §4). It clones the git reference inside the cluster,
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
	// capacity pre-flights scheduling headroom before a build Job is created so a build that cannot
	// fit fails fast with an actionable message instead of hanging Pending (issue #274). It is
	// OPTIONAL: nil means no pre-flight (the build proceeds), and it is wired in production via
	// WithCapacityProber. Reusing the CapacityProber seam keeps the build check and the capacity
	// report (issue #275) on the same headroom math.
	capacity controlplane.CapacityProber
}

// NewBuilder returns a BuildAdapter over the given clientset. The build always runs in the dedicated
// burrow-builds namespace (issue #278), isolated from both the app and control-plane namespaces —
// the caller no longer chooses it. Tests inject a fake clientset; production injects a real one (see
// NewBuilderFromConfig).
func NewBuilder(client kubernetes.Interface) *BuildAdapter {
	return &BuildAdapter{client: client, namespace: buildNamespace, gitImage: defaultGitImage, buildImage: defaultBuildImage}
}

// NewBuilderFromConfig builds a BuildAdapter from a REST config — the production wiring path,
// mirroring NewFromConfig for the Kubernetes seam.
func NewBuilderFromConfig(cfg *rest.Config) (*BuildAdapter, error) {
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kube: building clientset: %w", err)
	}
	return NewBuilder(client), nil
}

// WithCapacityProber enables the pre-build scheduling-headroom check (issue #274): before creating a
// build Job, the adapter reads the cluster's capacity through the prober and refuses with an
// actionable error when no node has room for the build's request. A nil prober (the default) leaves
// the check off and the build proceeds. Returns the adapter for chaining.
func (b *BuildAdapter) WithCapacityProber(p controlplane.CapacityProber) *BuildAdapter {
	b.capacity = p
	return b
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
func (b *BuildAdapter) Build(ctx context.Context, source controlplane.SourceRef, targetImage string, insecure bool, cred controlplane.SourceCredential) (string, error) {
	if err := source.Validate(); err != nil {
		return "", fmt.Errorf("kube: build: %w: %w", controlplane.ErrInvalid, err)
	}
	if strings.TrimSpace(targetImage) == "" {
		return "", fmt.Errorf("kube: build: target image reference is empty: %w", controlplane.ErrInvalid)
	}

	// Fail fast when the build cannot schedule (issue #274). A build pod requests a quarter CPU /
	// 512Mi (buildResources); on a fully-committed small node — common on the cheap self-host ICP,
	// where platform overhead alone can exhaust a 1-vCPU/2-GB node — the Job would otherwise sit
	// Pending forever behind an obscure FailedScheduling event. Pre-flight the same scheduling-headroom
	// math the capacity surface uses (issue #275) and refuse with the plain-language verdict instead.
	// The check is best-effort: it runs only when a prober is wired, and a read error does NOT block
	// the build (a misconfigured capacity read must not break builds) — only a definitive "no node has
	// room" verdict stops it before any Job is created.
	if b.capacity != nil {
		if state, err := b.capacity.ReadResourceState(ctx); err == nil {
			if fits, verdict := controlplane.BuildFitsState(state); !fits {
				return "", fmt.Errorf("kube: in-cluster build cannot be scheduled: %s", verdict)
			}
		}
	}

	// The build runs in the dedicated burrow-builds namespace (issue #278), which `burrow install`
	// provisions kubeconfig-side along with burrowd's Role there. burrowd holds only namespaced Roles
	// and cannot create namespaces or cluster RBAC itself (least privilege) — the same reason
	// `burrow env add` creates per-environment namespaces kubeconfig-side rather than at runtime.
	name := buildJobName(source, targetImage)
	job := b.buildJob(name, source, targetImage, insecure, cred)
	jobs := b.client.BatchV1().Jobs(b.namespace)
	created, err := jobs.Create(ctx, job, metav1.CreateOptions{})
	switch {
	case apierrors.IsAlreadyExists(err):
		// A Job with this deterministic name already exists for this exact source+target. Its status
		// decides whether the re-run reuses it or retries — read using the SAME interpretation the wait
		// loop below applies (Status.Failed / Status.Succeeded):
		//
		//   - FAILED: reusing it would return the previous build's stale failure on every re-run until the
		//     TTL controller reaps it ~3 days later (issue #280), never actually retrying (issue #298).
		//     Delete it and recreate a fresh Job so the re-run rebuilds. Least-surprising default.
		//   - SUCCEEDED: reuse the result (a good build is cheap and idempotent — do not rebuild).
		//   - Still ACTIVE: reuse it (an in-flight build for the same ref — do not start a duplicate).
		//
		// Reuse keeps the existing Job's UID, so a credential Secret stays owned by it and is
		// garbage-collected when the Job is reaped.
		existing, gerr := jobs.Get(ctx, name, metav1.GetOptions{})
		if gerr != nil {
			return "", fmt.Errorf("kube: reading existing build job %q: %w", name, gerr)
		}
		if existing.Status.Failed > 0 {
			if created, err = b.replaceFailedJob(ctx, jobs, name, job); err != nil {
				return "", err
			}
		} else {
			created = existing
		}
	case err != nil:
		return "", fmt.Errorf("kube: creating build job %q: %w", name, err)
	}

	// When a source-provider credential was resolved, materialize it into a Secret in the build
	// namespace that the Job mounts (ADR-0057 §4). It is owned by the Job, so it is garbage-collected
	// when the Job is reaped (on success, or by the TTL controller on failure) — the token never
	// outlives the build. The token reaches Kubernetes only here, written straight into the Secret; it
	// is never a Job env var, a command line, or an API response.
	if !cred.IsZero() {
		if err := b.ensureBuildCredentials(ctx, credSecretName(name), created, cred); err != nil {
			return "", err
		}
	}

	deadline := time.Now().Add(buildJobTimeout)
	for {
		j, err := jobs.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", fmt.Errorf("kube: reading build job %q: %w", name, err)
		}
		if j.Status.Failed > 0 {
			// Leave the failed Job (and its pod logs) for diagnosis; the TTL controller reaps it after
			// buildJobTTLSeconds so failures no longer accumulate indefinitely (issue #280).
			return "", fmt.Errorf("kube: build job %q failed", name)
		}
		if j.Status.Succeeded > 0 {
			digest := b.jobTerminationDigest(ctx, name)
			if digest == "" {
				// The Job reported success but wrote no digest — treat it as a build failure rather
				// than pinning a deploy to nothing. Leave the Job for diagnosis.
				return "", fmt.Errorf("kube: build job %q reported success but produced no image digest", name)
			}
			// Reap on success immediately (a clean cluster: a good build has nothing to diagnose) —
			// the TTL is only the backstop for the failures left behind above (issue #280).
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

// replaceFailedJob deletes the failed build Job left behind by a previous re-run and creates a fresh
// one in its place, so an idempotent re-run of the same source+ref actually retries the build instead
// of returning the previous failure (issue #298). It mirrors the success-reap path's background
// propagation: the Job owner is removed immediately (dependent pods and the owned credential Secret
// are garbage-collected asynchronously), so the recreate does not collide with the old Job. A delete
// that races the TTL controller and finds the Job already gone (NotFound) is fine — the recreate
// still proceeds. If the recreate itself still races an AlreadyExists (another re-run recreated it
// first), it is surfaced as a clear transient error the caller can retry.
func (b *BuildAdapter) replaceFailedJob(ctx context.Context, jobs batchv1client.JobInterface, name string, job *batchv1.Job) (*batchv1.Job, error) {
	policy := metav1.DeletePropagationBackground
	if err := jobs.Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &policy}); err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("kube: replacing failed build job %q: %w", name, err)
	}
	created, err := jobs.Create(ctx, job, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("kube: recreating build job %q after failure: the failed Job is still being deleted; retry the build: %w", name, err)
	}
	if err != nil {
		return nil, fmt.Errorf("kube: recreating build job %q after failure: %w", name, err)
	}
	return created, nil
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
func (b *BuildAdapter) buildJob(name string, source controlplane.SourceRef, targetImage string, insecure bool, cred controlplane.SourceCredential) *batchv1.Job {
	labels := map[string]string{nameLabel: name, managedByLabel: managedByValue}
	var backoff int32
	ttl := buildJobTTLSeconds

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
		// $HOME on a writable emptyDir so buildah's container storage and the CNB lifecycle scratch
		// have somewhere to write. The buildScript keeps buildah's graphroot and runroot under here.
		{Name: "HOME", Value: buildHomePath},
		// vfs storage needs no overlay/host mounts — the driver the buildScript configures via a
		// private storage.conf under $HOME.
		{Name: "STORAGE_DRIVER", Value: "vfs"},
		// buildah/containers derive the rootless runtime dir from XDG_RUNTIME_DIR; point it at a
		// private dir under $HOME (the buildScript creates it 0700) so buildah does not fall back to
		// /var/tmp/storage-run-$UID, which it refuses as group-writable under the pod's fsGroup.
		{Name: "XDG_RUNTIME_DIR", Value: buildHomePath + "/run"},
		// containers/image stages layer downloads under TMPDIR; point it at the writable /tmp emptyDir
		// so it does not try to use /var/tmp on the container root filesystem.
		{Name: "TMPDIR", Value: buildTmpPath},
	}
	if insecure {
		// The push target is the plain-HTTP in-cluster registry (ADR-0054 §5): the buildScript reads
		// this and pushes with --tls-verify=false. Set only when true so an external push stays over
		// TLS by default.
		buildEnv = append(buildEnv, corev1.EnvVar{Name: "TARGET_INSECURE", Value: "true"})
	}

	workspace := corev1.VolumeMount{Name: "workspace", MountPath: workspacePath}
	cloneMounts := []corev1.VolumeMount{workspace}
	buildMounts := []corev1.VolumeMount{
		workspace,
		{Name: "home", MountPath: buildHomePath},
		{Name: "tmp", MountPath: buildTmpPath},
	}
	volumes := []corev1.Volume{
		{Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "home", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}

	// A source-provider credential (ADR-0057) is consumed by MOUNTING, never by passing: the clone
	// reads its gitconfig (url.insteadOf token rewrite) via GIT_CONFIG_GLOBAL, and buildah reads its
	// docker config.json via REGISTRY_AUTH_FILE. The token itself lives only in the mounted Secret's
	// data — it is never one of these env values, so it never appears in the Job spec or a command line.
	if !cred.IsZero() {
		secretName := credSecretName(name)
		volumes = append(volumes,
			corev1.Volume{Name: "git-creds", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: secretName,
				Items:      []corev1.KeyToPath{{Key: gitConfigFile, Path: gitConfigFile}},
			}}},
			corev1.Volume{Name: "registry-auth", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: secretName,
				Items:      []corev1.KeyToPath{{Key: registryAuthFile, Path: registryAuthFile}},
			}}},
		)
		cloneMounts = append(cloneMounts, corev1.VolumeMount{Name: "git-creds", MountPath: gitCredsPath, ReadOnly: true})
		cloneEnv = append(cloneEnv, corev1.EnvVar{Name: "GIT_CONFIG_GLOBAL", Value: gitCredsPath + "/" + gitConfigFile})
		buildMounts = append(buildMounts, corev1.VolumeMount{Name: "registry-auth", MountPath: registryAuthPath, ReadOnly: true})
		buildEnv = append(buildEnv, corev1.EnvVar{Name: "REGISTRY_AUTH_FILE", Value: registryAuthPath + "/" + registryAuthFile})
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: b.namespace, Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff,
			// The TTL controller reaps this Job and its pods buildJobTTLSeconds after it finishes,
			// covering both successes and failures uniformly (issue #280).
			TTLSecondsAfterFinished: &ttl,
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
						VolumeMounts:    cloneMounts,
						SecurityContext: cloneContainerSecurityContext(),
						Resources:       buildResources(),
					}},
					Containers: []corev1.Container{{
						Name:            buildContainerName,
						Image:           b.buildImage,
						Command:         []string{"sh", "-c", buildScript},
						Env:             buildEnv,
						VolumeMounts:    buildMounts,
						SecurityContext: builderContainerSecurityContext(),
						Resources:       buildResources(),
					}},
					Volumes: volumes,
				},
			},
		},
	}
}

// credSecretName is the deterministic name of the source-provider credential Secret for a build Job.
// It is derived from the Job name so a re-run reuses it, and it is owned by the Job so it is
// garbage-collected when the Job is reaped (ADR-0057 §4).
func credSecretName(jobName string) string { return jobName + "-creds" }

// ensureBuildCredentials writes the source-provider token into a Secret in the build namespace that
// the Job mounts (ADR-0057 §4). The Secret holds two materializations of the ONE token: a gitconfig
// whose url.insteadOf rewrite authenticates the private clone, and a docker config.json that
// authenticates buildah's push/pull to the provider's registry. It is owned by the build Job so
// Kubernetes garbage-collects it when the Job is reaped. The token is written straight into the
// Secret data and is never logged or placed in an error — a write failure names the Secret only.
func (b *BuildAdapter) ensureBuildCredentials(ctx context.Context, secretName string, owner *batchv1.Job, cred controlplane.SourceCredential) error {
	gitcfg, err := gitCredentialConfig(cred)
	if err != nil {
		return fmt.Errorf("kube: building git credentials for build %q: %w", owner.Name, err)
	}
	dockercfg, err := registryAuthConfig(cred)
	if err != nil {
		return fmt.Errorf("kube: building registry credentials for build %q: %w", owner.Name, err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: b.namespace,
			Labels:    map[string]string{nameLabel: owner.Name, managedByLabel: managedByValue},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1",
				Kind:       "Job",
				Name:       owner.Name,
				UID:        owner.UID,
			}},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			gitConfigFile:    []byte(gitcfg),
			registryAuthFile: []byte(dockercfg),
		},
	}
	if _, err := b.client.CoreV1().Secrets(b.namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		// The error names the Secret only — never the token value.
		return fmt.Errorf("kube: writing build credentials secret %s/%s: %w", b.namespace, secretName, err)
	}
	return nil
}

// gitCredentialConfig renders a gitconfig whose url.<authed>.insteadOf rewrite injects the provider
// token into every clone of the provider's git host — so `git fetch` authenticates a private repo
// without the token ever being a command-line argument. The token rides in the userinfo of the
// rewritten URL, URL-encoded so any token character is carried safely.
func gitCredentialConfig(cred controlplane.SourceCredential) (string, error) {
	host := cred.Provider.GitHost()
	if host == "" {
		return "", fmt.Errorf("provider %q is not a source provider", cred.Provider)
	}
	authed := (&url.URL{Scheme: "https", User: url.UserPassword(cred.Provider.GitUser(), cred.Token), Host: host, Path: "/"}).String()
	base := "https://" + host + "/"
	return fmt.Sprintf("[url %q]\n\tinsteadOf = %s\n", authed, base), nil
}

// registryAuthConfig renders a docker config.json authenticating the provider's registry host with
// the same token, for buildah's $REGISTRY_AUTH_FILE (ADR-0057 §4). One provider token covers both the
// git clone and the registry (§1), so the build's push and any private base-image pull authenticate
// with it too.
func registryAuthConfig(cred controlplane.SourceCredential) (string, error) {
	host := cred.Provider.RegistryHost()
	if host == "" {
		return "", fmt.Errorf("provider %q has no registry host", cred.Provider)
	}
	auth := base64.StdEncoding.EncodeToString([]byte(cred.Provider.GitUser() + ":" + cred.Token))
	cfg := struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}{Auths: map[string]struct {
		Auth string `json:"auth"`
	}{host: {Auth: auth}}}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	return string(raw), nil
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

// cloneContainerSecurityContext is the full restricted PodSecurity floor (ADR-0053 §7), kept as-is
// for the clone init container: no privilege escalation, all Linux capabilities dropped, and a
// read-only root filesystem (its one write path, the workspace, is a writable emptyDir). A shallow
// git fetch needs none of the relaxation the builder does, so it keeps the tightest floor.
func cloneContainerSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: boolPtr(false),
		ReadOnlyRootFilesystem:   boolPtr(true),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

// builderContainerSecurityContext runs the build container privileged. WHY the full relaxation:
// building a container image requires a user+mount namespace whose root mount can be remounted
// private, and buildah's layer extraction (chrootarchive) does exactly that. On a managed CRI like
// DOKS/containerd the container root mount is LOCKED, so that remount is denied even inside buildah's
// own user namespace and even with CAP_SYS_ADMIN or a writable root filesystem — validated on a live
// DOKS cluster, where nothing short of privileged completes a build. ADR-0056's narrower relaxation
// (Unconfined seccomp + SETUID/SETGID + AllowPrivilegeEscalation) cleared unshare(CLONE_NEWUSER) but
// not the locked-mount remount, so it is insufficient on managed Kubernetes; this supersedes it.
//
// This is acceptable ONLY because the OSS build path is single-tenant and the user owns the source it
// builds — the build is trusted code, so §7's PodSecurity was defense in depth, not an adversary
// boundary. Isolation for the untrusted-source (multi-tenant) case is NOT a PodSecurity context: it
// lives in the commercial product's hardened, gVisor/microVM executor behind the Builder seam
// (ADR-0056 §3, ADR-0053 §6), which never runs a privileged pod on a shared node.
func builderContainerSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		Privileged:               boolPtr(true),
		AllowPrivilegeEscalation: boolPtr(true),
		ReadOnlyRootFilesystem:   boolPtr(false),
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
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
