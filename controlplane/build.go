// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"fmt"
	"net/url"
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
	// insecure marks the push as targeting the in-cluster registry, which serves plain HTTP in-cluster
	// (ADR-0054 §5). This is the single place that knows the in-cluster endpoint is plain HTTP: only
	// the default path below sets it; an explicit external target is always pushed over TLS.
	insecure := false
	if req.TargetImage == "" {
		// No explicit target: default to the optional in-cluster registry when one is wired — the
		// zero-config default push target (ADR-0053 §5, ADR-0054). The image happens to live in a
		// local registry; deploy still pins it strictly by reference (ADR-0007). When no in-cluster
		// registry is configured, a build with no target is an error, since there is nowhere to push.
		if e.buildRegistry == "" {
			return BuildResult{}, fmt.Errorf("build %s: target image reference is empty and no in-cluster registry is configured to default to: %w", req.App, ErrInvalid)
		}
		pushTarget = defaultBuildTarget(e.buildRegistry, req.App)
		insecure = true
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

	// Resolve the source-provider credential for the clone URL's host, if one is configured (ADR-0057).
	// A private repo needs it; a public repo resolves to the zero credential and the build clones
	// anonymously as before. The token is read from the guarded credential store and handed to the
	// builder in memory — it never enters this method's log lines, the BuildResult, or an error.
	cred, err := e.resolveSourceCredential(ctx, req.Source.Repo)
	if err != nil {
		return BuildResult{}, fmt.Errorf("build %s: %w", req.App, err)
	}

	// Build inside the cluster, pushing to pushTarget. A builder error is terminal for the build:
	// surface it and do NOT touch the deploy path — nothing is rolled out, no release is recorded
	// (ADR-0053 §4).
	digest, err := e.builder.Build(ctx, req.Source, pushTarget, insecure, cred)
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

// resolveSourceCredential returns the source-provider credential that authenticates a clone of repo,
// or the zero SourceCredential when the repo's host has no known source provider, no such provider is
// configured, or the repo is on a host Burrow does not front (a public clone, credential-free). It
// maps the clone URL's host to a source provider (ADR-0057 §1), finds the matching registered
// provider, and reads its token from the guarded credential store — the SAME store `provider add`
// wrote it to (ADR-0030). The token is returned to the caller in memory and is NEVER placed in an
// error: a read failure names the provider and the key, never the value.
func (e *Engine) resolveSourceCredential(ctx context.Context, repo string) (SourceCredential, error) {
	host := gitHost(repo)
	if host == "" {
		return SourceCredential{}, nil
	}
	providerType, ok := SourceProviderForHost(host)
	if !ok {
		// The clone host is not a source provider Burrow fronts (a self-hosted git, a public mirror):
		// nothing to authenticate with — clone anonymously, as before.
		return SourceCredential{}, nil
	}
	providers, err := e.db.Providers(ctx)
	if err != nil {
		return SourceCredential{}, fmt.Errorf("resolving the %s source credential: %w", providerType, err)
	}
	var p *Provider
	for i := range providers {
		if providers[i].Type == providerType && providers[i].Serves(CapabilitySource) {
			p = &providers[i]
			break
		}
	}
	if p == nil {
		// No source provider configured for this host: the repo must be public. Clone anonymously; a
		// private repo then fails at the fetch with an actionable message (issue #279).
		return SourceCredential{}, nil
	}
	token, err := e.credentials.Token(ctx, p.SecretKey)
	if err != nil {
		return SourceCredential{}, fmt.Errorf("reading the %s source token (key %q): %w", providerType, p.SecretKey, err)
	}
	return SourceCredential{Provider: providerType, Token: token}, nil
}

// gitHost extracts the host from an HTTPS git clone URL (e.g. "https://github.com/u/app" ->
// "github.com"). It returns "" for a non-HTTP(S) URL (an SSH `git@…` remote, a local path): the OSS
// path authenticates HTTPS clones with a token (ADR-0057), and an SSH deploy key is a separate,
// git-only credential form Burrow does not yet wire.
func gitHost(repo string) string {
	u, err := url.Parse(strings.TrimSpace(repo))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return ""
	}
	return u.Hostname()
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
