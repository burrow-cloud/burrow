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

// The adapter reports its own stages, so the engine hands it the caller's reporter rather than
// bracketing the whole build as one opaque stage (issue #503).
var _ controlplane.ProgressBuilder = (*BuildAdapter)(nil)

// The adapter can authenticate a push to a registry the source credential does not cover, by writing
// a second entry into the same mounted docker config.json (issue #584).
var _ controlplane.PushCredentialBuilder = (*BuildAdapter)(nil)

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
	// layersPath is where the Cloud Native Buildpacks lifecycle writes the layers it builds, the
	// report.toml the digest is read back from, and its own analysis metadata. It is a writable
	// emptyDir for the same reason $HOME and /tmp are.
	//
	// It is the lifecycle's DEFAULT path, deliberately: the path is recorded into the produced
	// image as CNB_LAYERS_DIR and travels with it for the life of that image, so an unconventional
	// one is carried by every application forever in exchange for nothing. The Dockerfile path
	// never touches the directory — an unused empty one costs a buildah build nothing.
	layersPath = "/layers"
	// cnbCreator is the Cloud Native Buildpacks lifecycle binary the no-Dockerfile branch runs. It
	// runs every phase of a build in one process — detect, analyze, restore, build, export — and
	// pushes the exported image itself, so the branch needs no separate push step. The path is fixed
	// by the buildpacks specification (every builder image publishes the lifecycle at /cnb/lifecycle),
	// so it is a property of the ecosystem rather than of the burrow-builder image.
	cnbCreator = "/cnb/lifecycle/creator"

	// buildContainerName / cloneContainerName are fixed names (not derived from any app or ref) so
	// the digest read-back never depends on caller input.
	buildContainerName = "build"
	cloneContainerName = "clone"

	// buildJobTimeout caps how long burrowd waits for an in-cluster build to finish. A build is
	// slower than a run or a pg_dump — a cold buildpacks build pulls a builder and a runtime — so it
	// gets a longer ceiling. It is declared in controlplane/apiwait.go rather than here, because a
	// client's own bound has to outlast it and a bound only one side can see is how the two drift
	// apart (issue #404).
	buildJobTimeout = controlplane.BuildJobTimeout
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
)

// buildJobTTLSeconds is how long a finished build Job (succeeded OR failed) lingers before
// Kubernetes' TTL-after-finished controller reaps it and its pods. Three days keeps a recent failure
// inspectable without leaking Jobs forever. It is the UNIFORM backstop that fixes failed-Job
// accumulation (issue #280) — a success is still reaped immediately for a clean cluster, and the TTL
// covers the failures the wait loop deliberately leaves behind for diagnosis.
//
// It is cluster CONFIGURATION rather than a constant (ADR-0068 §6): how long a build's logs are
// worth keeping is an operator's decision about their own disk, and it was previously one they could
// not see, let alone change, without reading this file. `build.job_retention` is where they set it,
// and its built-in default is the three days this was.
//
// The value is applied to the Job at CREATION. A Job already finished and waiting to be reaped keeps
// the TTL it was created with, because ttlSecondsAfterFinished is a field on the object rather than
// a policy the controller re-reads — so a change takes effect for the next build, not retroactively.
func buildJobTTLSeconds(ctx context.Context, limits controlplane.ClusterConfigFunc) int32 {
	return int32(limits.ClusterDuration(ctx, controlplane.LimitBuildJobRetention).Seconds())
}

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
//
// $TARGET_INSECURE is now a property of the INSTALL rather than a constant: an in-cluster registry
// that terminates its own TLS leaves it unset, which restores verification on the buildah push and
// lets the buildpacks branch run at all. That is why the refusal below names the flag — the condition
// it reports is fixable, and on a registry with a certificate it should never be reached.
//
// The buildpacks branch tests for the lifecycle BEFORE running it, and refuses with a sentence when
// it is absent. The builder image bundles one, so this is not the expected path — but the branch
// invokes an absolute path inside an image the install can override (BURROW_BUILD_IMAGE), and an
// image without a lifecycle produced `sh: /cnb/lifecycle/creator: No such file or directory` as the
// entire explanation a user got for a failed build (issue #590). A missing builder is a legible
// refusal, in the same voice as the insecure-registry one above, whatever image is wired.
//
// The lifecycle is given `-no-color` because its output is not read on a terminal: it is captured
// from the pod's log and replayed through a CLI or a console, where the ANSI escapes it writes by
// default are noise rather than emphasis.
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
  if [ ! -x ` + cnbCreator + ` ]; then
    echo "this source has no Dockerfile, so the build needs the Cloud Native Buildpacks lifecycle, and the builder image running this build does not carry one at ` + cnbCreator + `; add a Dockerfile to the repository, upgrade to a burrowd release whose builder image bundles the lifecycle, or point the build at one with BURROW_BUILD_IMAGE" >&2
    exit 1
  fi
  if [ "${TARGET_INSECURE:-}" = "true" ]; then
    echo "the no-Dockerfile Cloud Native Buildpacks path cannot push to a plain-HTTP registry, and this build's target is one; add a Dockerfile, push to an external registry with an explicit target, or give the in-cluster registry a certificate and set BURROW_BUILD_REGISTRY_INSECURE=false (buildpacks insecure push is a follow-up, ADR-0054 §5)" >&2
    exit 1
  fi
  ` + cnbCreator + ` -app=` + workspacePath + ` -layers=` + layersPath + ` -no-color "$TARGET_IMAGE"
  grep -o 'sha256:[0-9a-f]\{64\}' ` + layersPath + `/report.toml | head -n1 > /dev/termination-log
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
	// podMutator is the ADR-0053 §6 executor extension point: an OPTIONAL hook applied to the build
	// Job's pod template spec after it is constructed and before the Job is created. It is enabling
	// API for the managed product (cloud ADR-0003), never consumed by OSS — nil (the default) leaves
	// the OSS behavior exactly as-is. Wired via WithBuildPodMutator.
	podMutator func(*corev1.PodSpec)
	// limits reads the operator-set operational configuration for the cluster-tier limits this
	// adapter applies (ADR-0068 §6): how long a finished build Job is kept, and the unschedulable
	// grace the wait loop shares with the status surface. nil (the default) resolves every limit to
	// its built-in default, which is exactly the behaviour these had as constants. Wired via
	// WithOperationalLimits.
	limits controlplane.ClusterConfigFunc
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

// WithOperationalLimits registers the source of the operator-set operational limits this adapter
// reads (ADR-0068 §6) — the build Job's retention and the unschedulable grace. It is read at the
// moment a build runs rather than captured here, so `burrow cluster config set` takes effect without
// restarting burrowd. A nil supplier (the default) resolves every limit to its built-in default.
// Returns the adapter for chaining.
func (b *BuildAdapter) WithOperationalLimits(f controlplane.ClusterConfigFunc) *BuildAdapter {
	b.limits = f
	return b
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

// WithBuildNamespace overrides the namespace the in-cluster build Job (and any credential Secret) is
// created in. The default remains the dedicated burrow-builds namespace; an empty value leaves it.
// This parameterizes what is otherwise a constant for downstream callers that run builds in a
// different namespace — the managed product's per-tenant build namespaces (cloud ADR-0003) — without
// changing OSS behavior, which never sets it. Returns the adapter for chaining.
func (b *BuildAdapter) WithBuildNamespace(ns string) *BuildAdapter {
	if ns != "" {
		b.namespace = ns
	}
	return b
}

// WithBuildPodMutator registers a hook the adapter applies to the build Job's pod template spec
// after it is constructed and before the Job is created. It is the ADR-0053 §6 seam's executor
// extension point: the managed product (cloud ADR-0003) uses it to run the build under a gVisor
// RuntimeClass with a non-privileged restricted security context, a hard activeDeadlineSeconds, and
// pod labels its egress NetworkPolicy selects — none of which OSS itself needs (OSS runs privileged,
// no RuntimeClass, per ADR-0059). A nil mutator (the default) leaves the OSS behavior exactly as-is.
// Returns the adapter for chaining.
func (b *BuildAdapter) WithBuildPodMutator(fn func(*corev1.PodSpec)) *BuildAdapter {
	b.podMutator = fn
	return b
}

// Build runs the in-cluster build to completion and returns the pushed image's content digest
// (ADR-0053 §4). Only the git reference and the target reference cross into the builder; the source
// is cloned inside the cluster, so no code travels over the control channel (ADR-0004, ADR-0053 §3).
// A clone, build, or push failure is returned as a structured error and nothing is pushed; the
// caller does NOT touch the deploy path on error (ADR-0053 §4). It blocks until the Job succeeds or
// fails, or the build timeout elapses.
func (b *BuildAdapter) Build(ctx context.Context, source controlplane.SourceRef, targetImage string, insecure bool, cred controlplane.SourceCredential) (string, error) {
	return b.build(ctx, controlplane.BuildIntent{}, source, targetImage, insecure, cred, controlplane.PushCredential{}, nil)
}

// BuildWithProgress is Build, reporting the build's stages as the Job reaches them (issue #503): the
// clone, then the build, from what the Job's pod actually shows, plus a repeat of the running stage
// often enough that the response survives a proxy's read timeout. Its result and its errors are
// Build's, exactly — reporting is beside the build, never part of it.
func (b *BuildAdapter) BuildWithProgress(ctx context.Context, source controlplane.SourceRef, targetImage string, insecure bool, cred controlplane.SourceCredential, progress func(controlplane.DeployEvent)) (string, error) {
	return b.build(ctx, controlplane.BuildIntent{}, source, targetImage, insecure, cred, controlplane.PushCredential{}, progress)
}

// BuildAttributed is BuildWithProgress, additionally recording what the build is FOR on the build Job
// itself (issue #504) — the app, the environment, and the reference its deploy pins.
//
// THE JOB IS WHERE THE INTENT BELONGS, because the Job is what survives. It outlives the request that
// created it, it outlives the goroutine waiting on it, and it outlives burrowd; the caller's call
// frame outlives none of those. Recorded here, a build that succeeds after its caller has gone is
// still finishable by whoever is running when it finishes (see StrandedBuilds). The intent is small,
// non-secret metadata on an object that was being created anyway — a label and an annotation — and a
// zero intent records nothing, which is exactly what Build and BuildWithProgress do.
//
// progress may be nil, meaning nobody asked to observe this build.
func (b *BuildAdapter) BuildAttributed(ctx context.Context, intent controlplane.BuildIntent, source controlplane.SourceRef, targetImage string, insecure bool, cred controlplane.SourceCredential, progress func(controlplane.DeployEvent)) (string, error) {
	return b.build(ctx, intent, source, targetImage, insecure, cred, controlplane.PushCredential{}, progress)
}

// BuildWithPushCredential is BuildAttributed, additionally authenticating the push to targetImage
// with a registry credential of its own (issue #584) — the case the source-provider credential cannot
// express, because that one names a provider and the provider fixes both the git host and the
// registry host.
//
// The push credential goes into the SAME mounted docker config.json the source credential's registry
// entry goes into, keyed by its own host. That is the whole mechanism: the file already exists for
// this reason, buildah already reads it through $REGISTRY_AUTH_FILE, and a config.json is a map from
// registry host to credential — so a second registry is a second entry, not a second mechanism. The
// password reaches the cluster only inside that Secret's data; it is never a Job env var, a command
// line, or anything the Job spec carries.
//
// progress may be nil, meaning nobody asked to observe this build.
func (b *BuildAdapter) BuildWithPushCredential(ctx context.Context, intent controlplane.BuildIntent, source controlplane.SourceRef, targetImage string, insecure bool, cred controlplane.SourceCredential, push controlplane.PushCredential, progress func(controlplane.DeployEvent)) (string, error) {
	return b.build(ctx, intent, source, targetImage, insecure, cred, push, progress)
}

// build is the one implementation every entry point above delegates to.
func (b *BuildAdapter) build(ctx context.Context, intent controlplane.BuildIntent, source controlplane.SourceRef, targetImage string, insecure bool, cred controlplane.SourceCredential, push controlplane.PushCredential, progress func(controlplane.DeployEvent)) (string, error) {
	if progress == nil {
		progress = func(controlplane.DeployEvent) {}
	}
	if err := source.Validate(); err != nil {
		return "", fmt.Errorf("kube: build: %w: %w", controlplane.ErrInvalid, err)
	}
	if strings.TrimSpace(targetImage) == "" {
		return "", fmt.Errorf("kube: build: target image reference is empty: %w", controlplane.ErrInvalid)
	}
	// A push credential with no registry host cannot be written into a docker config.json, whose
	// entries are keyed by host. Refuse before any Job or Secret exists; the error names the missing
	// field, never the password.
	if err := push.Validate(); err != nil {
		return "", fmt.Errorf("kube: build: %w: %w", controlplane.ErrInvalid, err)
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

	// The build runs in the dedicated burrow-builds namespace (issue #278), which `burrow cluster install`
	// provisions kubeconfig-side along with burrowd's Role there. burrowd holds only namespaced Roles
	// and cannot create namespaces or cluster RBAC itself (least privilege) — the same reason
	// `burrow env add` creates per-environment namespaces kubeconfig-side rather than at runtime.
	name := buildJobName(source, targetImage)
	job := b.buildJob(ctx, name, intent, source, targetImage, insecure, cred, push)
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
			// A reused Job carries whatever intent the run that created it recorded, which may name a
			// different environment or a different deploy reference. Re-stamp it with this run's, so a
			// build recovered later is finished for the caller that most recently asked for it — and so
			// the hold marker a previous unattended attempt left is cleared, because someone asking for
			// this build again is someone who has not given up on it.
			//
			// A FAILURE HERE DOES NOT FAIL THE BUILD. The intent is a recovery aid, not part of
			// building: the Job already carries the intent recorded when it was created, and the
			// caller in front of this call is about to drive the build to a deploy itself. Refusing to
			// build because a metadata patch was refused — an install whose Role predates the patch
			// verb is exactly that case — would be a worse bug than the one this recovers from.
			_ = b.stampBuildIntent(ctx, jobs, name, intent)
		}
	case err != nil:
		return "", fmt.Errorf("kube: creating build job %q: %w", name, err)
	}

	// When a credential was resolved — for the clone, for the push, or both — materialize it into a
	// Secret in the build namespace that the Job mounts (ADR-0057 §4, issue #584). It is owned by the
	// Job, so it is garbage-collected when the Job is reaped (on success, or by the TTL controller on
	// failure) — no secret outlives the build. Both reach Kubernetes only here, written straight into
	// the Secret; neither is ever a Job env var, a command line, or an API response.
	if !cred.IsZero() || !push.IsZero() {
		if err := b.ensureBuildCredentials(ctx, credSecretName(name), created, cred, push); err != nil {
			return "", err
		}
	}

	// The Job exists and its first container is the clone, so the report opens here — before the wait,
	// not after it. The first event is what commits the streaming response's header (issue #503), and
	// an event emitted only once the build is over is an event nobody is still connected to receive.
	reporter := newBuildReporter(progress)
	reporter.start()

	// awaitJob fails fast when the build pod cannot start rather than waiting out the thirty-minute
	// deadline (issue #352). The capacity pre-flight above catches the "no node has room" case before
	// any Job exists, but it cannot catch the rest: a clone init container whose credential Secret is
	// missing, an unreachable builder image, a taint the build pod does not tolerate. Those all leave
	// both Job counters at zero, and thirty minutes is a long time to learn nothing.
	//
	// The observer runs on the same goroutine, once per poll, and reads the pod only when it is about
	// to say something — so the report costs one extra pod read per interval and nothing else.
	j, err := awaitJobObserved(ctx, b.client, b.namespace, name, buildJobTimeout, buildJobPoll, unschedulableGrace(ctx, b.limits), func() {
		if reporter.due() {
			reporter.observe(b.buildPhase(ctx, name))
		}
	})
	if err != nil {
		// A wait that ended badly — a pod that cannot start, the deadline, a cancelled request — ends
		// the stage it was in. The error itself is unchanged and carries the diagnosis.
		reporter.enter(b.buildPhase(ctx, name))
		reporter.finish(false)
		return "", err
	}
	if j.Status.Failed > 0 {
		// Read the pod once more before closing the report: a clone that could not authenticate and a
		// Dockerfile step that exited non-zero are different failures, and the stage says which.
		reporter.enter(b.buildPhase(ctx, name))
		reporter.finish(false)
		// Leave the failed Job (and its pod logs) for diagnosis; the TTL controller reaps it after
		// buildJobTTLSeconds so failures no longer accumulate indefinitely (issue #280).
		return "", fmt.Errorf("kube: build job %q failed", name)
	}
	digest := b.jobTerminationDigest(ctx, name)
	if digest == "" {
		// The Job reported success but wrote no digest — treat it as a build failure rather
		// than pinning a deploy to nothing. Leave the Job for diagnosis. The Job ran to completion, so
		// whatever went wrong went wrong in the build.
		reporter.enter(controlplane.StageBuild)
		reporter.finish(false)
		return "", fmt.Errorf("kube: build job %q reported success but produced no image digest", name)
	}
	reporter.finish(true)
	// Reap on success immediately (a clean cluster: a good build has nothing to diagnose) —
	// the TTL is only the backstop for the failures left behind above (issue #280).
	policy := metav1.DeletePropagationBackground
	_ = jobs.Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &policy})
	return digest, nil
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
//
// ctx is taken for the operational configuration read behind buildJobTTLSeconds, not for a cluster
// call: this function constructs an object and sends nothing.
func (b *BuildAdapter) buildJob(ctx context.Context, name string, intent controlplane.BuildIntent, source controlplane.SourceRef, targetImage string, insecure bool, cred controlplane.SourceCredential, push controlplane.PushCredential) *batchv1.Job {
	labels := map[string]string{nameLabel: name, managedByLabel: managedByValue}
	var backoff int32
	ttl := buildJobTTLSeconds(ctx, b.limits)

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
		// The buildpacks lifecycle writes every layer it builds here before exporting them, so this
		// is the largest write path a no-Dockerfile build has. It is mounted for BOTH branches
		// because the branch is chosen inside the pod, after the clone, from the cloned tree — the
		// Job is built before anyone knows which builder will run (ADR-0053 §3).
		{Name: "layers", MountPath: layersPath},
	}
	volumes := []corev1.Volume{
		{Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "home", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "layers", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}

	// A credential (ADR-0057, issue #584) is consumed by MOUNTING, never by passing: the clone reads
	// its gitconfig (url.insteadOf token rewrite) via GIT_CONFIG_GLOBAL, and buildah reads its docker
	// config.json via REGISTRY_AUTH_FILE. The secret values live only in the mounted Secret's data —
	// they are never one of these env values, so they never appear in the Job spec or a command line.
	//
	// The two halves are mounted INDEPENDENTLY, because the two credentials are independent. A push
	// credential with no source credential is an ordinary case — a public repo pushed to a private
	// registry — and it must mount the registry auth WITHOUT the gitconfig: a Secret volume item that
	// names an absent key leaves the pod unable to start, which would turn a public build into a
	// permanent Pending.
	secretName := credSecretName(name)
	if !cred.IsZero() {
		volumes = append(volumes, corev1.Volume{Name: "git-creds", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: secretName,
			Items:      []corev1.KeyToPath{{Key: gitConfigFile, Path: gitConfigFile}},
		}}})
		cloneMounts = append(cloneMounts, corev1.VolumeMount{Name: "git-creds", MountPath: gitCredsPath, ReadOnly: true})
		cloneEnv = append(cloneEnv, corev1.EnvVar{Name: "GIT_CONFIG_GLOBAL", Value: gitCredsPath + "/" + gitConfigFile})
	}
	if !cred.IsZero() || !push.IsZero() {
		volumes = append(volumes, corev1.Volume{Name: "registry-auth", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: secretName,
			Items:      []corev1.KeyToPath{{Key: registryAuthFile, Path: registryAuthFile}},
		}}})
		buildMounts = append(buildMounts, corev1.VolumeMount{Name: "registry-auth", MountPath: registryAuthPath, ReadOnly: true})
		buildEnv = append(buildEnv,
			corev1.EnvVar{Name: "REGISTRY_AUTH_FILE", Value: registryAuthPath + "/" + registryAuthFile},
			// The SAME mounted file, named the way the OTHER builder looks for it: the buildpacks
			// lifecycle does not read $REGISTRY_AUTH_FILE — it resolves registry credentials through
			// the docker keychain, which reads $DOCKER_CONFIG as the DIRECTORY holding a config.json.
			// Without this a buildpacks build authenticates against nothing and its push to a private
			// registry is refused. buildah prefers $REGISTRY_AUTH_FILE, so naming the file twice
			// changes nothing about the Dockerfile path.
			corev1.EnvVar{Name: "DOCKER_CONFIG", Value: registryAuthPath},
		)
	}

	// The Job's OWN metadata carries what the build is for; the pod template's labels stay exactly as
	// they were, because they are how this adapter finds a build's pods and nothing about the deploy
	// belongs on them. A zero intent adds nothing at all.
	jobLabels := labels
	var annotations map[string]string
	if intentLabels, intentAnnotations := buildIntentMetadata(intent); len(intentLabels) > 0 {
		jobLabels = make(map[string]string, len(labels)+len(intentLabels))
		for k, v := range labels {
			jobLabels[k] = v
		}
		for k, v := range intentLabels {
			jobLabels[k] = v
		}
		annotations = intentAnnotations
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: b.namespace, Labels: jobLabels, Annotations: annotations},
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

	// Apply the ADR-0053 §6 executor extension point last, over the fully-constructed pod spec: the
	// managed product's hook swaps in its gVisor RuntimeClass, a non-privileged security context, a
	// build deadline, and its NetworkPolicy labels. OSS wires no mutator, so the pod is left exactly
	// as built above (privileged, no RuntimeClass, per ADR-0059).
	if b.podMutator != nil {
		b.podMutator(&job.Spec.Template.Spec)
	}
	return job
}

// credSecretName is the deterministic name of the credential Secret for a build Job.
// It is derived from the Job name so a re-run reuses it, and it is owned by the Job so it is
// garbage-collected when the Job is reaped (ADR-0057 §4).
func credSecretName(jobName string) string { return jobName + "-creds" }

// ensureBuildCredentials writes a build's credentials into a Secret in the build namespace that the
// Job mounts (ADR-0057 §4, issue #584). The Secret holds up to two files: a gitconfig whose
// url.insteadOf rewrite authenticates the private clone, and a docker config.json holding an entry per
// registry the build authenticates against — the source provider's, the push target's, or both. It is
// owned by the build Job so Kubernetes garbage-collects it when the Job is reaped. The secret values
// are written straight into the Secret data and are never logged or placed in an error — a write
// failure names the Secret only.
//
// A key is written only when there is something to put in it, matching what buildJob mounts: a build
// with a push credential and a public source gets a config.json and no gitconfig.
func (b *BuildAdapter) ensureBuildCredentials(ctx context.Context, secretName string, owner *batchv1.Job, cred controlplane.SourceCredential, push controlplane.PushCredential) error {
	data := map[string][]byte{}
	if !cred.IsZero() {
		gitcfg, err := gitCredentialConfig(cred)
		if err != nil {
			return fmt.Errorf("kube: building git credentials for build %q: %w", owner.Name, err)
		}
		data[gitConfigFile] = []byte(gitcfg)
	}
	dockercfg, err := registryAuthConfig(cred, push)
	if err != nil {
		return fmt.Errorf("kube: building registry credentials for build %q: %w", owner.Name, err)
	}
	data[registryAuthFile] = []byte(dockercfg)
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
		Data: data,
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

// registryAuthConfig renders the docker config.json buildah reads through $REGISTRY_AUTH_FILE
// (ADR-0057 §4), holding one entry per registry the build authenticates against.
//
// The source-provider credential contributes an entry for the provider's own registry, because one
// provider token covers both the git clone and that registry (ADR-0057 §1) — so a private base-image
// pull and a push to ghcr.io authenticate with it. The push credential contributes an entry for the
// push target's registry, which is the case the provider token cannot express at all (issue #584).
// Either may be absent; a config.json with no entries is valid and is what an anonymous build gets.
//
// A config.json is a MAP from registry host to credential, which is exactly why a second registry
// needs no second mechanism. When both name the same host the push credential wins: it was resolved
// for this specific push, while the provider entry is a side effect of who hosts the source.
func registryAuthConfig(cred controlplane.SourceCredential, push controlplane.PushCredential) (string, error) {
	type entry struct {
		Auth string `json:"auth"`
	}
	auths := map[string]entry{}
	if !cred.IsZero() {
		host := cred.Provider.RegistryHost()
		if host == "" {
			return "", fmt.Errorf("provider %q has no registry host", cred.Provider)
		}
		auths[host] = entry{Auth: basicAuth(cred.Provider.GitUser(), cred.Token)}
	}
	if !push.IsZero() {
		if err := push.Validate(); err != nil {
			return "", err
		}
		// Trimmed, because the key is what buildah looks the credential up by: a host carrying a stray
		// space is a key nothing ever matches, and the resulting push is a silent anonymous one.
		auths[strings.TrimSpace(push.Registry)] = entry{Auth: basicAuth(push.Username, push.Password)}
	}
	raw, err := json.Marshal(struct {
		Auths map[string]entry `json:"auths"`
	}{Auths: auths})
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// basicAuth encodes a username and secret the way a docker config.json entry carries them: the
// base64 of "user:secret". The secret is only ever encoded into Secret data by the caller — this
// returns a value, and never logs or wraps one.
func basicAuth(user, secret string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + secret))
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
