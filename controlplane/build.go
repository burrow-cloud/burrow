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
	if req.TargetImage == "" {
		return BuildResult{}, fmt.Errorf("build %s: target image reference is empty: %w", req.App, ErrInvalid)
	}
	if e.builder == nil {
		return BuildResult{}, fmt.Errorf("build %s: in-cluster build is not configured: %w", req.App, ErrNotImplemented)
	}

	// Build inside the cluster. A builder error is terminal for the build: surface it and do NOT touch
	// the deploy path — nothing is rolled out, no release is recorded (ADR-0053 §4).
	digest, err := e.builder.Build(ctx, req.Source, req.TargetImage)
	if err != nil {
		return BuildResult{}, fmt.Errorf("build %s: %w", req.App, err)
	}
	if strings.TrimSpace(digest) == "" {
		return BuildResult{}, fmt.Errorf("build %s: builder returned an empty digest: %w", req.App, ErrInvalid)
	}

	// The built image rejoins the existing guarded deploy path (ADR-0053 §4). Deploy the digest-pinned
	// reference (repo:tag@sha256:...) so the release pins the exact bytes just built while keeping the
	// tag for semver classification. This is the SAME unexported deploy the explicit call uses — same
	// guardrails, rollout, deploy record, rollback chain, and audit entry — stamped manual because an
	// in-cluster build is an explicit, human- or agent-triggered call (ADR-0053 §2).
	image := pinDigest(req.TargetImage, digest)
	dep, err := e.deploy(ctx, DeployRequest{App: req.App, Env: req.Env, Image: image, Confirm: req.Confirm}, manualProvenance())
	if err != nil {
		return BuildResult{}, fmt.Errorf("build %s: %w", req.App, err)
	}
	return BuildResult{Digest: digest, Deploy: dep}, nil
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
