// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/burrow-cloud/burrow/connect"
)

// latestReleaseURL is the unauthenticated GitHub API endpoint that returns this repository's
// latest published release. The repository is public, so it reads without a token.
const latestReleaseURL = "https://api.github.com/repos/burrow-cloud/burrow/releases/latest"

// latestReleaseTimeout bounds the latest-release check so `burrow version` never hangs on a slow
// or unreachable network: the check is best-effort and skipped silently on any failure.
const latestReleaseTimeout = 3 * time.Second

// fetchLatestRelease returns the tag of the latest published release (e.g. "v0.7.2"), or an error.
// It is a package var so tests can fake it with no network. It is best-effort by contract: any
// failure (offline, timeout, non-200, rate-limited, malformed body) is returned so the caller can
// skip the release check silently rather than fail `burrow version`.
var fetchLatestRelease = func(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, latestReleaseTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latestReleaseURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github releases API returned %s", resp.Status)
	}
	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	return body.TagName, nil
}

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

			ctx, cancel := context.WithTimeout(cmd.Context(), connect.ProbeTimeout)
			defer cancel()

			// cpVer is the control plane's release version, read from the burrowd image tag on a
			// successful probe and left empty otherwise (not installed or unreachable). It feeds the
			// latest-release comparison below.
			var cpVer string
			cs, err := clientsetForContext(kubeconfig, kubeContext)
			if err != nil {
				fmt.Fprintln(out, controlPlaneLine("", err, ctxName, namespace))
			} else if img, imgErr := burrowdImage(ctx, cs, namespace); imgErr != nil {
				fmt.Fprintln(out, controlPlaneLine(img, imgErr, ctxName, namespace))
			} else {
				fmt.Fprintln(out, controlPlaneLine(img, nil, ctxName, namespace))
				cpVer = imageTag(img)
			}

			// Best effort: compare against the latest published release and flag an outdated CLI or
			// control plane. Any failure (offline, timeout, rate-limited) is skipped silently, so
			// `burrow version` still works with no network and never hangs.
			if latest, lerr := fetchLatestRelease(cmd.Context()); lerr == nil && latest != "" {
				fmt.Fprintf(out, "latest release: %s\n", latest)
				for _, hint := range upgradeHints(cliVersion(), cpVer, latest) {
					fmt.Fprintln(out, hint)
				}
			}
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
		return fmt.Sprintf("control plane: unreachable via context %q (%s)", ctxName, connect.FailureReason(err))
	}
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

// upgradeHints compares the CLI and control-plane versions against the latest published release and
// returns the applicable upgrade hint lines. It is pure so the comparison is unit-testable without a
// cluster or network:
//   - a control plane on a valid release older than latest gets the `burrow upgrade` hint;
//   - a CLI on a valid, non-pseudo release older than latest gets the `brew upgrade` hint (a local
//     dev/pseudo build is exempt, since there is nothing to brew-upgrade);
//   - when neither is behind, a single reassurance that this is the latest release.
//
// It assumes latest is a non-empty tag (the caller only calls it when the release check succeeded).
func upgradeHints(cliVer, cpVer, latest string) []string {
	var hints []string
	if semver.IsValid(cpVer) && semver.Compare(cpVer, latest) < 0 {
		hints = append(hints, fmt.Sprintf("Your control plane is behind. Run `burrow upgrade` to update it to %s.", latest))
	}
	if semver.IsValid(cliVer) && !module.IsPseudoVersion(cliVer) && semver.Compare(cliVer, latest) < 0 {
		hints = append(hints, fmt.Sprintf("A newer burrow (%s) is available. Run `brew upgrade burrow`.", latest))
	}
	if len(hints) == 0 {
		hints = append(hints, "You are on the latest release.")
	}
	return hints
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
