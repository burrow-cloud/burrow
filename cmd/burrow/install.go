// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"fmt"
	"io"
	"os/exec"
	"runtime/debug"
	"sort"
	"strings"
	"text/template"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/burrow-cloud/burrow/connect"
	"github.com/burrow-cloud/burrow/controlplane/kube"
)

// defaultBurrowdImage is the control-plane image `install`/`upgrade` deploy by default: the
// burrowd release matching this CLI's own build, derived from the module version so the CLI and
// the control plane move in lockstep with no hand-maintained version in the code. It returns ""
// when no published image matches this build (see burrowdTag), in which case install/upgrade
// require an explicit --burrowd-image. Override with --burrowd-image to run a specific build (the
// e2e builds one locally and imports it into k3d).
func defaultBurrowdImage() string {
	tag := burrowdTag()
	if tag == "" {
		return ""
	}
	return "ghcr.io/burrow-cloud/burrowd:" + tag
}

// burrowdTag resolves the published burrowd release tag matching this CLI build, or "" if none
// exists. It reads the build's module version and interprets it with the standard module/semver
// semantics rather than a hand-maintained constant:
//   - a real release version (vX.Y.Z, or a prerelease tag like vX.Y.Z-rc1) is an actual published
//     tag, used as-is;
//   - a Go pseudo-version — what Go 1.24+ stamps into a local `go build` past a tag — resolves to
//     the release it sits on top of via the pseudo-version base, e.g.
//     v0.3.1-0.<ts>-<commit> -> v0.3.0 (the newest published image);
//   - "(devel)", an empty version, or a tag-less pseudo-version (v0.0.0-<ts>-<commit>, no prior
//     release) have no matching published image and resolve to "".
//
// The version `burrow version` reports for the CLI is separate and may be a pseudo-version.
func burrowdTag() string {
	return burrowdTagFor(mainModuleVersion())
}

// burrowdTagFor is burrowdTag's pure core, taking the module version explicitly so it is unit
// testable without a build-info dependency.
func burrowdTagFor(v string) string {
	// Drop build metadata (e.g. the "+dirty" Go appends for an uncommitted tree). It is not part
	// of any release tag, and "+" is not even a valid image-tag character, so a "v0.3.0+dirty"
	// tag would fail to pull.
	if i := strings.IndexByte(v, '+'); i >= 0 {
		v = v[:i]
	}
	if !semver.IsValid(v) {
		return "" // "(devel)" or empty: not a version, no published image
	}
	if module.IsPseudoVersion(v) {
		// The base is the tag the commit was built on top of — Go increments the patch and
		// encodes it, so PseudoVersionBase("vX.Y.(Z+1)-0.<ts>-<commit>") is "vX.Y.Z", the last
		// release. An empty base means there was no prior tag (v0.0.0-<ts>-<commit>).
		base, err := module.PseudoVersionBase(v)
		if err != nil || base == "" {
			return ""
		}
		return base
	}
	return semver.Canonical(v)
}

// mainModuleVersion returns this build's main-module version from the build info: a release tag
// when installed via `go install …@version`, a Go pseudo-version for a local source build past a
// tag, or "(devel)"/"" when unavailable.
func mainModuleVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		return bi.Main.Version
	}
	return ""
}

// errNoBurrowdImage is returned by install/upgrade when no --burrowd-image was given and this
// CLI build has no matching published image to default to — an unreleased source build with no
// release tag underneath. A released CLI always derives its image, so this only surfaces for a
// from-scratch build, where deploying the right control plane means building it too.
func errNoBurrowdImage() error {
	return fmt.Errorf("this build of the burrow CLI (%s) has no matching published burrowd image, "+
		"so there is no default to install; pass --burrowd-image (e.g. build one with `ko build "+
		"./cmd/burrowd` and import it into the cluster), or use a released CLI", cliVersion())
}

// installManifests is the control-plane install manifest template, embedded from
// manifests/install.yaml.tmpl (like the migrations are embedded in controlplane/postgres).
//
//go:embed manifests/install.yaml.tmpl
var installManifests string

var installTemplate = template.Must(template.New("install").Parse(installManifests))

// installOptions are the values rendered into the install manifests. Namespace holds the
// control plane (burrowd, Postgres); AppNamespace is where deployed apps go — separate, so
// app workloads aren't mixed in with the control-plane infrastructure.
type installOptions struct {
	Namespace      string
	AppNamespace   string
	AddonNamespace string
	Image          string
	Token          string
	DBPassword     string
	Port           int
}

func newInstallCmd() *cobra.Command {
	var namespace, appNamespace, image, kubeconfig string
	var dryRun, wait, verbose bool
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install the Burrow control plane into your cluster",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInstall(cmd.Context(), namespace, appNamespace, image, kubeconfig, dryRun, wait, verbose, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().StringVar(&namespace, "namespace", connect.DefaultNamespace, "namespace to install the control plane into")
	cmd.Flags().StringVar(&appNamespace, "app-namespace", connect.DefaultAppNamespace, "namespace to deploy applications into")
	cmd.Flags().StringVar(&image, "burrowd-image", defaultBurrowdImage(), "burrowd container image to deploy (must be pullable by the cluster)")
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "path to kubeconfig (default: ambient)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the manifests instead of applying them")
	cmd.Flags().BoolVar(&wait, "wait", true, "wait for the control plane to become ready")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "show every resource kubectl applies instead of a summary")
	return cmd
}

func runInstall(ctx context.Context, namespace, appNamespace, image, kubeconfig string, dryRun, wait, verbose bool, stdout, stderr io.Writer) error {
	if image == "" {
		return errNoBurrowdImage()
	}
	token, err := randHex(16)
	if err != nil {
		return err
	}
	dbPassword, err := randHex(12)
	if err != nil {
		return err
	}

	manifests, err := renderManifests(installOptions{
		Namespace:      namespace,
		AppNamespace:   appNamespace,
		AddonNamespace: connect.DefaultAddonNamespace,
		Image:          image,
		Token:          token,
		DBPassword:     dbPassword,
		Port:           connect.DefaultPort,
	})
	if err != nil {
		return err
	}

	if dryRun {
		fmt.Fprint(stdout, manifests)
		return nil
	}

	cs, err := clientset(kubeconfig)
	if err != nil {
		return err
	}
	if installed, err := alreadyInstalled(ctx, cs, namespace); err != nil {
		return err
	} else if installed {
		return fmt.Errorf("Burrow is already installed in namespace %q; run `burrow upgrade` to update it "+
			"(re-running install would mint new secrets and break the existing control plane)", namespace)
	}

	if err := kubectlApply(ctx, kubeconfig, manifests, verbose, stdout, stderr); err != nil {
		return err
	}

	if wait {
		if err := waitForReady(ctx, kubeconfig, namespace, stdout); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "\nBurrow is installed and ready in namespace %q.\n", namespace)
	} else {
		fmt.Fprintf(stdout, "\nBurrow installed into namespace %q (not waiting for readiness).\n", namespace)
	}

	// Installing tells you what your cluster can do (ADR-0034): probe the cluster's capabilities
	// kubeconfig-side and print a one-line summary. The probe is read-only and best-effort — a
	// failure here never fails a successful install, since the agent reads capabilities live anyway.
	printCapabilitySummary(ctx, cs, stdout)

	if wait {
		fmt.Fprint(stdout, "Deploy an app:\n  burrow app deploy <app> --image <ref>\n")
	}
	return nil
}

// printCapabilitySummary probes the cluster's capabilities with the kubeconfig client and prints a
// one-line summary (ADR-0034). It is best-effort: a probe failure prints nothing and is not fatal.
func printCapabilitySummary(ctx context.Context, cs kubernetes.Interface, stdout io.Writer) {
	caps, err := kube.DetectCapabilities(ctx, cs)
	if err != nil {
		return
	}
	fmt.Fprintf(stdout, "Detected: %s\n", capabilitySummary(toClientCaps(caps)))
}

// waitForReady blocks until the in-cluster Postgres and burrowd are ready, printing
// progress. burrowd only becomes ready after it has reached Postgres and applied its
// migrations, so this confirms the whole control plane is up.
func waitForReady(ctx context.Context, kubeconfig, namespace string, out io.Writer) error {
	cs, err := clientset(kubeconfig)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "\nWaiting for Burrow to become ready...")
	if err := waitForDeployment(ctx, cs, namespace, "postgres", "database", out, 3*time.Minute); err != nil {
		return err
	}
	return waitForDeployment(ctx, cs, namespace, "burrowd", "control plane", out, 3*time.Minute)
}

func waitForDeployment(ctx context.Context, cs kubernetes.Interface, namespace, name, label string, out io.Writer, timeout time.Duration) error {
	fmt.Fprintf(out, "  %s ...", label)
	deadline := time.Now().Add(timeout)
	for {
		d, err := cs.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			desired := int32(1)
			if d.Spec.Replicas != nil {
				desired = *d.Spec.Replicas
			}
			if desired > 0 && d.Status.ObservedGeneration >= d.Generation && d.Status.ReadyReplicas >= desired {
				fmt.Fprintln(out, " ready")
				return nil
			}
		}
		if time.Now().After(deadline) {
			fmt.Fprintln(out, " timed out")
			return fmt.Errorf("%s did not become ready within %s", label, timeout)
		}
		time.Sleep(2 * time.Second)
	}
}

// kubectlApply pipes the manifests to `kubectl apply -f -`. By default it prints a one-line
// summary of how many resources changed; with verbose it streams kubectl's per-resource output.
func kubectlApply(ctx context.Context, kubeconfig, manifests string, verbose bool, stdout, stderr io.Writer) error {
	return applyAndSummarize(ctx, applyArgs(kubeconfig, "-"), manifests, verbose, stdout, stderr)
}

func applyArgs(kubeconfig, source string) []string {
	args := []string{"apply", "-f", source}
	if kubeconfig != "" {
		args = append([]string{"--kubeconfig", kubeconfig}, args...)
	}
	return args
}

// applyAndSummarize runs `kubectl apply` (optionally feeding manifests on stdin) and reports
// the result the way a non-Kubernetes user wants by default: a single line of what changed,
// rather than a wall of per-resource output. With verbose, or on failure (so the error is
// debuggable), it shows kubectl's full output. kubectl's stderr always passes through.
func applyAndSummarize(ctx context.Context, args []string, stdin string, verbose bool, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = stderr
	err := cmd.Run()
	if err != nil {
		fmt.Fprint(stdout, out.String())
		return fmt.Errorf("kubectl apply: %w", err)
	}
	if verbose {
		fmt.Fprint(stdout, out.String())
		return nil
	}
	summarizeApply(out.String(), stdout)
	return nil
}

// summarizeApply prints a one-line summary of kubectl apply output, counting resources by the
// trailing action word kubectl prints (e.g. "created", "configured", "unchanged").
func summarizeApply(out string, w io.Writer) {
	counts := map[string]int{}
	total := 0
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		counts[strings.ToLower(fields[len(fields)-1])]++
		total++
	}
	if total == 0 {
		return
	}
	var parts []string
	for _, action := range []string{"created", "configured", "unchanged", "deleted", "pruned"} {
		if n := counts[action]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, action))
			delete(counts, action)
		}
	}
	others := make([]string, 0, len(counts))
	for action := range counts {
		others = append(others, action)
	}
	sort.Strings(others)
	for _, action := range others {
		parts = append(parts, fmt.Sprintf("%d %s", counts[action], action))
	}
	fmt.Fprintf(w, "Applied %d resource(s): %s.\n", total, strings.Join(parts, ", "))
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating random secret: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func renderManifests(o installOptions) (string, error) {
	var sb strings.Builder
	if err := installTemplate.Execute(&sb, o); err != nil {
		return "", fmt.Errorf("rendering manifests: %w", err)
	}
	return sb.String(), nil
}
