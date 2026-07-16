// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"fmt"
	"strings"
)

// Build builds an app's image from a git source reference inside the user's own cluster and, on
// success, hands the resulting image into the existing guarded deploy path (ADR-0053). It is the
// optional in-cluster build path, never the deploy spine: Burrow stays client-build-first and deploy
// stays by image reference (ADR-0007), so a build is a front-end that ends where deploy begins.
//
// Only metadata crosses into the builder — the git reference and the target image reference; the
// builder clones the actual source from git inside the cluster, so no code travels over the control
// channel (ADR-0004, ADR-0053 §3). On a successful build the resulting digest-pinned reference flows
// into the same rollout, deploy record (the rollback handle), and audit entry an explicit deploy runs
// (ADR-0053 §4), so guardrails and the deploy history are never bypassed. On a builder error nothing
// is deployed: the error is surfaced structurally and the deploy path is not touched.
func (e *Engine) Build(ctx context.Context, req BuildRequest) (BuildResult, error) {
	if err := (App{Name: req.App}).Validate(); err != nil {
		return BuildResult{}, fmt.Errorf("build: %w: %w", ErrInvalid, err)
	}
	if err := req.Source.Validate(); err != nil {
		return BuildResult{}, fmt.Errorf("build %s: %w: %w", req.App, ErrInvalid, err)
	}
	// pushTarget is where the builder pushes the image; deployBase is the reference the resulting
	// deploy pins. For a caller-supplied target the two are identical (external registries stay fully
	// supported). For the default in-cluster path they DIFFER (ADR-0054): the builder pushes to the
	// internal cluster-DNS Service endpoint (in-cluster, plain HTTP, credential-free), but the deploy
	// must reference the image by the PUBLIC registry host so the kubelet pulls it through the ingress
	// over a trusted cert. Both share the same repository path and digest, so the internal push and the
	// public pull resolve to the same stored image — a registry's stored repo path is independent of
	// the endpoint host used to reach it.
	pushTarget, deployBase := req.TargetImage, req.TargetImage
	if req.TargetImage == "" {
		// No explicit target: default to the optional in-cluster registry when one is wired — the
		// zero-config default push target (ADR-0053 §5, ADR-0054). The image happens to live in a
		// local registry; deploy still pins it strictly by reference (ADR-0007). When no in-cluster
		// registry is configured, a build with no target is an error, since there is nowhere to push.
		if e.buildRegistry == "" {
			return BuildResult{}, fmt.Errorf("build %s: target image reference is empty and no in-cluster registry is configured to default to: %w", req.App, ErrInvalid)
		}
		pushTarget = defaultBuildTarget(e.buildRegistry, req.App)
		// Reference the public host for the deploy so the node pulls through the ingress. Fall back to
		// the internal push target only when no public host is wired (an in-cluster registry installed
		// without a public ingress), preserving the earlier single-endpoint behavior.
		deployBase = pushTarget
		if e.buildPublicRegistry != "" {
			deployBase = defaultBuildTarget(e.buildPublicRegistry, req.App)
		}
	}
	if e.builder == nil {
		return BuildResult{}, fmt.Errorf("build %s: in-cluster build is not configured: %w", req.App, ErrNotImplemented)
	}

	// Build inside the cluster, pushing to pushTarget. A builder error is terminal for the build:
	// surface it and do NOT touch the deploy path — nothing is rolled out, no release is recorded
	// (ADR-0053 §4).
	digest, err := e.builder.Build(ctx, req.Source, pushTarget)
	if err != nil {
		return BuildResult{}, fmt.Errorf("build %s: %w", req.App, err)
	}
	if strings.TrimSpace(digest) == "" {
		return BuildResult{}, fmt.Errorf("build %s: builder returned an empty digest: %w", req.App, ErrInvalid)
	}

	// The built image rejoins the existing guarded deploy path (ADR-0053 §4). Deploy the digest-pinned
	// PUBLIC reference (repo:tag@sha256:...) so the release pins the exact bytes just built — reachable
	// by the node through the ingress — while keeping the tag for semver classification. This is the
	// SAME unexported deploy the explicit call uses — same guardrails, rollout, deploy record, rollback
	// chain, and audit entry — stamped manual because an in-cluster build is an explicit, human- or
	// agent-triggered call (ADR-0053 §2).
	image := pinDigest(deployBase, digest)
	dep, err := e.deploy(ctx, DeployRequest{App: req.App, Env: req.Env, Image: image, Confirm: req.Confirm}, manualProvenance())
	if err != nil {
		return BuildResult{}, fmt.Errorf("build %s: %w", req.App, err)
	}
	return BuildResult{Digest: digest, Deploy: dep}, nil
}

// defaultBuildTargetTag is the tag a build's default in-cluster target carries. The deploy pins the
// exact bytes by digest (see pinDigest), so the tag is only a human-readable handle, not the
// identity; a fixed, non-semver tag keeps it out of the ADR-0052 semver auto-update path — an
// in-cluster build is an explicit call, not something to also auto-redeploy on tag movement.
const defaultBuildTargetTag = "build"

// defaultBuildTarget composes a default build reference from a registry host, the app as the
// repository, and a fixed build tag — e.g. "burrow-registry.burrow.svc.cluster.local:5000/web:build"
// (ADR-0053 §5). It is used for BOTH the internal push endpoint and the public pull host (ADR-0054):
// because the repository path (app + tag) is identical, the internally pushed image and the publicly
// pulled reference resolve to the same stored image.
func defaultBuildTarget(registry, app string) string {
	return registry + "/" + app + ":" + defaultBuildTargetTag
}

// pinDigest returns the digest-pinned form of a target image reference (e.g.
// "ghcr.io/u/app:1.2.3" + "sha256:abc" -> "ghcr.io/u/app:1.2.3@sha256:abc"), so a build deploys the
// exact bytes it produced rather than a mutable tag. Any digest already on the reference is replaced.
// A tag, when present, is preserved, so the resulting reference still classifies for semver
// auto-update (ADR-0052).
func pinDigest(targetImage, digest string) string {
	if i := strings.IndexByte(targetImage, '@'); i >= 0 {
		targetImage = targetImage[:i]
	}
	return targetImage + "@" + digest
}
