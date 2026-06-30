// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/burrow-cloud/burrow/connect"
)

// probeTimeout caps the best-effort control-plane probe so `burrow version` returns promptly even
// when the targeted cluster is unreachable.
const probeTimeout = 5 * time.Second

// newVersionCmd reports this CLI's version and, best effort, the version of the control plane
// installed in the cluster — read from the burrowd Deployment's image, so it works even if
// burrowd is unhealthy and needs no API token.
func newVersionCmd() *cobra.Command {
	var kubeconfig, kubeContext, namespace string
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the CLI version and the installed control-plane version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			// The CLI line prints first and always; control-plane connectivity never blocks it.
			fmt.Fprintf(out, "burrow (CLI):  %s\n", cliVersion())

			// Name the targeted context so the control-plane line is legible in both the success and
			// failure cases. Best effort: a missing or unreadable kubeconfig leaves it empty, which
			// the failure path below still reports cleanly.
			ctxName, _ := connect.TargetContextName(kubeconfig, kubeContext)

			ctx, cancel := context.WithTimeout(cmd.Context(), probeTimeout)
			defer cancel()
			cs, err := clientsetForContext(kubeconfig, kubeContext)
			if err != nil {
				fmt.Fprintln(out, controlPlaneLine("", err, ctxName, namespace))
				return nil
			}
			img, err := burrowdImage(ctx, cs, namespace)
			fmt.Fprintln(out, controlPlaneLine(img, err, ctxName, namespace))
			return nil
		},
	}
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "path to kubeconfig (default: ambient)")
	cmd.Flags().StringVar(&kubeContext, "context", "", "kubeconfig context to target (default: current context)")
	cmd.Flags().StringVar(&namespace, "namespace", connect.DefaultNamespace, "namespace the control plane is installed in")
	return cmd
}

// controlPlaneLine renders the "control plane:" line from a probe result: the burrowd image and
// the error from reading its Deployment, plus the targeted context and namespace. It is pure so
// the success, not-installed, and unreachable renderings are unit-tested without a cluster.
func controlPlaneLine(img string, err error, ctxName, namespace string) string {
	switch {
	case err == nil:
		return fmt.Sprintf("control plane: %s (context %q, namespace %q)", imageTag(img), ctxName, namespace)
	case apierrors.IsNotFound(err):
		return fmt.Sprintf("control plane: not installed (context %q, namespace %q)", ctxName, namespace)
	default:
		return fmt.Sprintf("control plane: unreachable via context %q (%s)", ctxName, probeReason(err))
	}
}

// probeReason reduces a connectivity error to a concise reason, dropping the dialed URL that the
// Kubernetes client prepends. It names the common failures explicitly (timeout, DNS, refused) and
// otherwise strips the `Get "<url>": ` prefix so the URL noise stays out of the version line.
func probeReason(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf("timed out after %s", probeTimeout)
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "no such host"
	}
	return trimDialPrefix(err.Error())
}

// trimDialPrefix removes a leading `Get "<url>": ` (or any `<verb> "<url>": `) that the Kubernetes
// REST client puts on transport errors, leaving just the underlying reason.
func trimDialPrefix(s string) string {
	if quote := strings.Index(s, ` "`); quote >= 0 {
		if sep := strings.Index(s[quote:], `": `); sep >= 0 {
			return s[quote+sep+len(`": `):]
		}
	}
	return s
}

// version is the CLI release version, stamped at build time with
// `-ldflags "-X main.version=<tag>"` (goreleaser injects the release tag on a tagged build).
// It is empty for a local `go build`, a `go install …@version`, or a test binary, in which
// case cliVersion falls back to the build info — keeping the Go pseudo-version for a local
// source build rather than overwriting it with a stale constant.
var version string

// cliVersion returns this CLI's release version: the ldflags-stamped tag for a release build,
// otherwise the main-module version from the build info — set when it is installed with
// `go install …@version` or a Go pseudo-version for a local source build — or "dev" for an
// unversioned build.
func cliVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

// imageTag returns just the version tag of an image reference (the part after the last colon,
// ignoring any registry-host port and stripping a digest), e.g.
// "ghcr.io/burrow-cloud/burrowd:v0.2.1" -> "v0.2.1". An untagged image is returned unchanged.
func imageTag(image string) string {
	if at := strings.Index(image, "@"); at >= 0 {
		image = image[:at] // drop a digest: name@sha256:...
	}
	if colon := strings.LastIndex(image, ":"); colon > strings.LastIndex(image, "/") {
		return image[colon+1:]
	}
	return image
}

// burrowdImage returns the image of the installed burrowd Deployment, or an error — an
// IsNotFound error when no control plane is installed in the namespace.
func burrowdImage(ctx context.Context, cs kubernetes.Interface, namespace string) (string, error) {
	d, err := cs.AppsV1().Deployments(namespace).Get(ctx, "burrowd", metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Name == "burrowd" {
			return c.Image, nil
		}
	}
	return "", fmt.Errorf("the burrowd deployment in %s has no burrowd container", namespace)
}
