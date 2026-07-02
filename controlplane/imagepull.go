// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"fmt"
	"strings"
)

// The helpers here turn a blocking pod-pull condition into the structured, actionable Issue a
// Kubernetes adapter attaches to a WorkloadStatus (ADR-0006). They live in the core package,
// dependency-free, so both the real adapter and the fake build the same message — the real
// adapter reports a genuine ImagePullBackOff, and a test injects the raw reason and gets an
// identical, host-naming Issue without a cluster.

// Blocking image-pull waiting reasons a container reports when the cluster cannot fetch the
// image. These are the only reasons Burrow surfaces as an Issue: a private registry with no
// pull credentials is the common, human-fixable cause (ADR-0017). Transient reasons like
// ContainerCreating or PodInitializing are deliberately excluded — they resolve on their own.
const (
	// ReasonImagePullBackOff is the kubelet's back-off state after repeated pull failures.
	ReasonImagePullBackOff = "ImagePullBackOff"
	// ReasonErrImagePull is the kubelet's first pull failure, before it backs off.
	ReasonErrImagePull = "ErrImagePull"
)

// IsImagePullReason reports whether reason is a blocking image-pull failure Burrow surfaces as
// an actionable Issue. A pod waiting for any other reason (still creating, initializing, …) is
// not reported, so Status never flags a transient state as a problem.
func IsImagePullReason(reason string) bool {
	return reason == ReasonImagePullBackOff || reason == ReasonErrImagePull
}

// ImagePullIssue builds the actionable Issue message for a workload whose pod cannot pull its
// image: it names the image, the registry host the credentials are missing for, and the exact
// `burrow config registry login` command the user runs to fix it. The credential step is human- and
// CLI-only and never crosses MCP (ADR-0017), so the message points at the user's terminal.
func ImagePullIssue(image, reason string) string {
	host := RegistryHost(image)
	return fmt.Sprintf("cannot pull image %q (%s): the cluster has no credentials for registry %q. If it is private, ask the user to run: burrow config registry login %s", image, reason, host, host)
}

// RegistryHost returns the registry host of an image reference following the Docker convention:
// the first "/"-separated component is the host when it looks like one (it contains a "." or a
// ":", or is "localhost"); otherwise the reference is an implicit Docker Hub name and the host
// is "docker.io". Examples: "ghcr.io/org/app:1" -> "ghcr.io", "library/nginx" -> "docker.io",
// "registry.example.com:5000/app:1" -> "registry.example.com:5000".
func RegistryHost(image string) string {
	const dockerHub = "docker.io"
	first, rest, ok := strings.Cut(image, "/")
	if !ok {
		// No slash: a bare Docker Hub name like "nginx" or "nginx:1".
		return dockerHub
	}
	if strings.ContainsAny(first, ".:") || first == "localhost" {
		return first
	}
	// The first component is a Docker Hub namespace (e.g. "library"), not a host.
	_ = rest
	return dockerHub
}
