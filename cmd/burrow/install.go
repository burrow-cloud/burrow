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
	"text/tabwriter"
	"text/template"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/burrow-cloud/burrow/connect"
	"github.com/burrow-cloud/burrow/controlplane/kube"
	"github.com/burrow-cloud/burrow/localconfig"
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

// appRoleManifest is the shared app-namespace Role/RoleBinding template (ADR-0035): it defines the
// "appNamespaceRole" named template that both install (the default app namespace) and `burrow env
// add` (each per-environment namespace) render, so burrowd's app-namespace grant cannot drift
// between the two paths.
//
//go:embed manifests/approle.yaml.tmpl
var appRoleManifest string

// installTemplate parses the shared appNamespaceRole define first so the install body can invoke it.
var installTemplate = template.Must(template.Must(template.New("install").Parse(appRoleManifest)).Parse(installManifests))

// installOptions are the values rendered into the install manifests. Namespace holds the
// control plane (burrowd, Postgres); AppNamespace is where deployed apps go — separate, so
// app workloads aren't mixed in with the control-plane infrastructure. ServiceAccount is burrowd's
// ServiceAccount name, threaded into the shared app-namespace Role (defaults to "burrowd").
type installOptions struct {
	Namespace      string
	AppNamespace   string
	AddonNamespace string
	ServiceAccount string
	Image          string
	Token          string
	DBPassword     string
	Port           int
}

// installArgs are the resolved inputs to an install run: the target kube context (the required
// positional, empty for the no-argument listing path), the namespaces, image, and flags.
type installArgs struct {
	kubeContext  string
	environment  string
	namespace    string
	appNamespace string
	image        string
	kubeconfig   string
	dryRun       bool
	wait         bool
	verbose      bool
}

// clientsetFn builds the readiness/probe clientset for a kube context. It is a package var so a
// test can substitute a fake clientset for install's pre-apply checks, readiness wait, and
// capability probe without a real cluster.
var clientsetFn = func(kubeconfig, kubeContext string) (kubernetes.Interface, error) {
	return clientsetForContext(kubeconfig, kubeContext)
}

// listContexts loads the kubeconfig contexts. It is a package var so a test can substitute a
// fixed set (and the missing-kubeconfig error) without depending on the machine's real kubeconfig.
var listContexts = connect.Contexts

func newInstallCmd() *cobra.Command {
	a := installArgs{}
	cmd := &cobra.Command{
		Use:   "install [context]",
		Short: "Install the Burrow control plane into a cluster",
		Long: "install deploys the Burrow control plane into the kube context you name.\n\n" +
			"The context is a required argument: install targets exactly that cluster and never the\n" +
			"ambient current context implicitly, so it cannot install into prod by accident. Run\n" +
			"`burrow install` with no argument to list your kubeconfig contexts.\n\n" +
			"On success it names the environment (a generated name, or --environment) and records it\n" +
			"as your current environment in ~/.burrow/config.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				a.kubeContext = args[0]
			}
			return runInstall(cmd.Context(), a, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().StringVar(&a.environment, "environment", "", "name for this environment (default: a generated adjective-animal name)")
	cmd.Flags().StringVar(&a.namespace, "namespace", connect.DefaultNamespace, "namespace to install the control plane into")
	cmd.Flags().StringVar(&a.appNamespace, "app-namespace", connect.DefaultAppNamespace, "namespace to deploy applications into")
	cmd.Flags().StringVar(&a.image, "burrowd-image", defaultBurrowdImage(), "burrowd container image to deploy (must be pullable by the cluster)")
	cmd.Flags().StringVar(&a.kubeconfig, "kubeconfig", "", "path to kubeconfig (default: ambient)")
	cmd.Flags().BoolVar(&a.dryRun, "dry-run", false, "print the manifests instead of applying them")
	cmd.Flags().BoolVar(&a.wait, "wait", true, "wait for the control plane to become ready")
	cmd.Flags().BoolVar(&a.verbose, "verbose", false, "show every resource kubectl applies instead of a summary")
	return cmd
}

func runInstall(ctx context.Context, a installArgs, stdout, stderr io.Writer) error {
	if a.image == "" {
		return errNoBurrowdImage()
	}

	// render builds the manifests (minting fresh secrets) on demand: dry-run prints them without
	// touching a cluster, and the real path applies them once a target context is resolved.
	render := func() (string, error) {
		token, err := randHex(16)
		if err != nil {
			return "", err
		}
		dbPassword, err := randHex(12)
		if err != nil {
			return "", err
		}
		return renderManifests(installOptions{
			Namespace:      a.namespace,
			AppNamespace:   a.appNamespace,
			AddonNamespace: connect.DefaultAddonNamespace,
			Image:          a.image,
			Token:          token,
			DBPassword:     dbPassword,
			Port:           connect.DefaultPort,
		})
	}

	// dry-run prints the manifests without contacting a cluster and without needing a context.
	if a.dryRun {
		manifests, err := render()
		if err != nil {
			return err
		}
		fmt.Fprint(stdout, manifests)
		return nil
	}

	// Resolve the install target explicitly (ADR-0037). Burrow operates a cluster you point it at,
	// so a missing or empty kubeconfig is a clear stop, not a raw library error.
	contexts, err := listContexts(a.kubeconfig)
	if err != nil || len(contexts) == 0 {
		return errNoCluster()
	}
	// No context given: list the contexts (marking the current one) and instruct re-running with
	// one. Non-interactive and never installs into a guessed target.
	if a.kubeContext == "" {
		writeInstallContextHint(stdout, contexts)
		return nil
	}
	if !contextExists(contexts, a.kubeContext) {
		return fmt.Errorf("context %q is not in your kubeconfig; available: %s\nrun `burrow install <context>` with one of these",
			a.kubeContext, contextNames(contexts))
	}

	manifests, err := render()
	if err != nil {
		return err
	}

	cs, err := clientsetFn(a.kubeconfig, a.kubeContext)
	if err != nil {
		return err
	}
	if installed, err := alreadyInstalled(ctx, cs, a.namespace); err != nil {
		return err
	} else if installed {
		return fmt.Errorf("Burrow is already installed in namespace %q; run `burrow upgrade` to update it "+
			"(re-running install would mint new secrets and break the existing control plane)", a.namespace)
	}

	if err := applyFn(ctx, a.kubeconfig, a.kubeContext, manifests, a.verbose, stdout, stderr); err != nil {
		return err
	}

	if a.wait {
		if err := waitForReady(ctx, a.kubeconfig, a.kubeContext, a.namespace, stdout); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "\nBurrow is installed and ready in namespace %q.\n", a.namespace)
	} else {
		fmt.Fprintf(stdout, "\nBurrow installed into namespace %q (not waiting for readiness).\n", a.namespace)
	}

	// Installing tells you what your cluster can do (ADR-0034): probe the cluster's capabilities
	// kubeconfig-side and print a one-line summary. The probe is read-only and best-effort — a
	// failure here never fails a successful install, since the agent reads capabilities live anyway.
	printCapabilitySummary(ctx, cs, stdout)

	// Name and record the environment (ADR-0036/0037): write a local handle pinned as current, so
	// first-run detection flips and `burrow env list` shows it without connecting.
	if err := recordEnvironment(a, stdout); err != nil {
		return err
	}

	if a.wait {
		fmt.Fprint(stdout, "Deploy an app:\n  burrow app deploy <app> --image <ref>\n")
	}
	return nil
}

// recordEnvironment writes the just-installed environment into the local config as a handle and
// pins it as the current environment (ADR-0036/0037). The name is the explicit --environment or a
// generated adjective-animal name. It prints the confirmation and the rename hint.
func recordEnvironment(a installArgs, stdout io.Writer) error {
	name := a.environment
	if name == "" {
		name = friendlyName()
	}
	cfg, err := localconfig.Load()
	if err != nil {
		return err
	}
	if err := cfg.Add(localconfig.Environment{
		Name:                  name,
		Context:               a.kubeContext,
		ControlPlaneNamespace: a.namespace,
		AppNamespace:          a.appNamespace,
		// Cluster-per-environment: the whole cluster is the environment, so commands send burrowd
		// no env name and get the default app namespace and the global guardrails (ADR-0036). A
		// namespace-per-environment env carries its registered name instead (see `burrow env add`).
		Env: "",
	}); err != nil {
		return err
	}
	cfg.Current = name
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "\nInstalled. Environment %q is now your current environment.\n", name)
	fmt.Fprintf(stdout, "Rename it any time:  burrow env rename %s <new-name>\n", name)
	return nil
}

// errNoCluster is the clear stop when there is no kubeconfig (or it holds no contexts): Burrow
// operates a cluster you point it at, so it explains how to point it rather than surfacing a raw
// library error.
func errNoCluster() error {
	return fmt.Errorf("no kubeconfig found, so there is no cluster to install into. Burrow operates a " +
		"cluster you point it at: set $KUBECONFIG or create ~/.kube/config, then run `burrow install <context>`")
}

// writeInstallContextHint lists the kubeconfig contexts (marking the current one) and instructs the
// user to re-run install with one. It does not install and does not prompt (ADR-0037).
func writeInstallContextHint(w io.Writer, contexts []connect.Context) {
	fmt.Fprintln(w, "Pick the cluster to install Burrow into. Your kubeconfig contexts:")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CURRENT\tNAME\tCLUSTER")
	for _, c := range contexts {
		marker := ""
		if c.Current {
			marker = "*"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", marker, c.Name, c.Cluster)
	}
	_ = tw.Flush()
	fmt.Fprintln(w, "\nThen run `burrow install <context>` with one of these.")
}

// contextExists reports whether name is one of the kubeconfig contexts.
func contextExists(contexts []connect.Context, name string) bool {
	for _, c := range contexts {
		if c.Name == name {
			return true
		}
	}
	return false
}

// contextNames returns the context names joined for an error message.
func contextNames(contexts []connect.Context) string {
	names := make([]string, 0, len(contexts))
	for _, c := range contexts {
		names = append(names, c.Name)
	}
	return strings.Join(names, ", ")
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
func waitForReady(ctx context.Context, kubeconfig, kubeContext, namespace string, out io.Writer) error {
	cs, err := clientsetFn(kubeconfig, kubeContext)
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

// kubectlApply pipes the manifests to `kubectl apply -f -`, targeting kubeContext when set so the
// manifests land in the named cluster (empty means the kubeconfig's current context). By default it
// prints a one-line summary of how many resources changed; with verbose it streams kubectl's
// per-resource output.
func kubectlApply(ctx context.Context, kubeconfig, kubeContext, manifests string, verbose bool, stdout, stderr io.Writer) error {
	return applyAndSummarize(ctx, applyArgs(kubeconfig, kubeContext, "-"), manifests, verbose, stdout, stderr)
}

func applyArgs(kubeconfig, kubeContext, source string) []string {
	args := []string{"apply", "-f", source}
	if kubeContext != "" {
		args = append([]string{"--context", kubeContext}, args...)
	}
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
	if o.ServiceAccount == "" {
		o.ServiceAccount = "burrowd"
	}
	var sb strings.Builder
	if err := installTemplate.Execute(&sb, o); err != nil {
		return "", fmt.Errorf("rendering manifests: %w", err)
	}
	return sb.String(), nil
}
