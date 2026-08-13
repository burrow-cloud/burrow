// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"runtime/debug"
	"strings"
	"text/tabwriter"
	"text/template"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/burrow-cloud/burrow/connect"
	"github.com/burrow-cloud/burrow/controlplane"
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
	Namespace           string
	AppNamespace        string
	AddonNamespace      string
	BuildNamespace      string
	ServiceAccount      string
	AgentServiceAccount string
	Image               string
	Token               string
	DBPassword          string
	Port                int
	// Database is which shape the control plane's own database is installed in (ADR-0086 §2):
	// "cnpg", a CloudNativePG `Cluster`, or "plain", a single Deployment. It is chosen once, at
	// install; an upgrade reads back the shape already running rather than re-deciding, so a routine
	// upgrade never changes the database underneath an install.
	Database string
	// InstallID identifies this install (ADR-0084 §5). It is rendered into a ConfigMap in the
	// control-plane namespace and into burrowd's environment, and it is the one rendered value that
	// is NOT a secret: it authorises nothing, and a second person joining the install reads it. A
	// fresh install mints one; an upgrade reads the existing one back and re-renders it, because a
	// regenerated id would make every target already pointed here report a mismatch.
	InstallID string
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
	// minimal and noMetricsServer opt out of the detected lightweight baseline install/bootstrap
	// ensures on top of the control plane (ADR-0054 §1). minimal skips every baseline add-on;
	// noMetricsServer skips just metrics-server. metrics-server is the only baseline today, so the
	// two currently coincide; minimal is the forward-looking "control plane only" switch.
	minimal         bool
	noMetricsServer bool
	// database is the --database value: which shape the control plane's own database is installed
	// in (ADR-0086 §2). Empty means the default, CloudNativePG.
	database string
	// clusterOnly runs the install for its cluster-side effects only (deploy burrowd, and mint the
	// scoped burrow-agent credential via the manifests), skipping the laptop-oriented local
	// bookkeeping: it records no ~/.burrow environment handle and prints no "connect your agent"
	// guidance. `burrow cluster bootstrap` sets it when it deploys burrowd on the VPS (ADR-0044); the
	// bootstrap prints the join-token block instead. Normal `burrow cluster install` leaves it false.
	clusterOnly bool
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

// installExamples is the Examples block for `install`, shared by the command's `-h` help and the
// no-argument context listing so the two never drift.
const installExamples = "  # Install Burrow into a context with the defaults\n" +
	"  burrow cluster install do-nyc1-cluster\n\n" +
	"  # Install into a different app namespace\n" +
	"  burrow cluster install do-nyc1-cluster --app-namespace my-apps\n\n" +
	"  # Preview the manifests without applying them\n" +
	"  burrow cluster install do-nyc1-cluster --dry-run"

// newInstallAliasCmd is the deprecated top-level `burrow install` (ADR-0060): install now lives
// under the cluster-lifecycle surface as `burrow cluster install`, but the old spelling keeps
// working so existing muscle memory and scripts do not break. It delegates to the same constructor
// and marks itself Deprecated, which both prints a one-line migration hint on use and hides it from
// the main help (Cobra excludes Deprecated commands from the command listing).
func newInstallAliasCmd() *cobra.Command {
	cmd := newInstallCmd()
	cmd.Deprecated = "use \"burrow cluster install\"."
	return cmd
}

func newInstallCmd() *cobra.Command {
	a := installArgs{}
	cmd := &cobra.Command{
		Use:   "install <context>",
		Short: "Install the Burrow control plane into a cluster",
		Long: "Install the Burrow control plane into the kube context you name.\n\n" +
			"install provisions ONLY the control plane. Additive cluster components are separate,\n" +
			"opt-in commands you run when you want them:\n" +
			"  burrow cluster ingress install    # ingress-nginx, cert-manager, a Let's Encrypt issuer\n" +
			"  burrow cluster registry install   # the optional in-cluster image registry\n\n" +
			"The context is required: install targets exactly that cluster and never the ambient\n" +
			"current context implicitly, so it cannot install into prod by accident. Run\n" +
			"`burrow cluster install` with no argument to list your contexts.\n\n" +
			"The control plane's own database runs on CloudNativePG, the same way every Postgres add-on\n" +
			"does, so it can fail over and be backed up. Install applies the operator and waits for it,\n" +
			"which creates cluster-scoped CustomResourceDefinitions and needs cluster-admin. A cluster\n" +
			"that will not accept those installs with --database plain, which runs the database as a\n" +
			"single Deployment with no backups and no failover. The choice is made here and only here.\n\n" +
			"On success it names the environment (a generated name, or --environment) and records it\n" +
			"as your current environment.",
		Example: installExamples,
		Args:    cobra.MaximumNArgs(1),
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
	cmd.Flags().BoolVar(&a.verbose, "verbose", false, "show every resource burrow applies instead of a summary")
	cmd.Flags().StringVar(&a.database, "database", databaseCNPG,
		"how the control plane's own database runs: cnpg (a CloudNativePG cluster, with failover and backups) or plain (a single Deployment, with neither)")
	cmd.Flags().BoolVar(&a.minimal, "minimal", false, "install only the control plane, skipping the detected lightweight baseline (metrics-server)")
	cmd.Flags().BoolVar(&a.noMetricsServer, "no-metrics-server", false, "do not auto-ensure the metrics-server baseline (needed for HPA autoscaling and `kubectl top`; add it later with `burrow cluster metrics install`)")
	return cmd
}

func runInstall(ctx context.Context, a installArgs, stdout, stderr io.Writer) error {
	if a.image == "" {
		return errNoBurrowdImage()
	}
	database, err := validateDatabase(a.database)
	if err != nil {
		return err
	}

	// render builds the manifests (minting fresh secrets and a fresh install id) on demand: dry-run
	// prints them without touching a cluster, and the real path applies them once a target context is
	// resolved. It returns the install id as well as the manifests, because the id is the one minted
	// value the local side also has to record — on the target, so a later command can tell whether it
	// arrived at this install or at some other one wearing the same context name (ADR-0084 §5).
	render := func() (manifests, installID string, err error) {
		token, err := randHex(16)
		if err != nil {
			return "", "", err
		}
		dbPassword, err := randHex(12)
		if err != nil {
			return "", "", err
		}
		// Sixteen random bytes, the same size as the API token. The id is not a secret, so its size is
		// about collision rather than guessing: two installs must never mint the same id, including
		// across the rebuild-under-the-same-name case this whole mechanism exists to catch.
		installID, err = randHex(16)
		if err != nil {
			return "", "", err
		}
		out, err := renderManifests(installOptions{
			Namespace:      a.namespace,
			AppNamespace:   a.appNamespace,
			AddonNamespace: connect.DefaultAddonNamespace,
			BuildNamespace: connect.DefaultBuildNamespace,
			Image:          a.image,
			Token:          token,
			DBPassword:     dbPassword,
			InstallID:      installID,
			Port:           connect.DefaultPort,
			Database:       database,
		})
		if err != nil {
			return "", "", err
		}
		return out, installID, nil
	}

	// dry-run prints the manifests without contacting a cluster and without needing a context.
	if a.dryRun {
		manifests, _, err := render()
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
		writeInstallContextHint(ctx, stdout, a.kubeconfig, a.namespace, contexts)
		return nil
	}
	if !contextExists(contexts, a.kubeContext) {
		return fmt.Errorf("context %q is not in your kubeconfig; available: %s\nrun `burrow cluster install <context>` with one of these",
			a.kubeContext, contextNames(contexts))
	}

	manifests, installID, err := render()
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
		// Cluster-only re-run (bootstrap on an already-provisioned VPS): the cluster side is done and
		// there is no local config to write, so report and return rather than performing the local join.
		if a.clusterOnly {
			fmt.Fprintf(stdout, "\nBurrow is already installed in namespace %q.\n", a.namespace)
			return nil
		}
		// A populated control plane must not be re-minted (that would break the running install), so
		// install performs the local JOIN instead of erroring: it reads the existing scoped agent
		// credential and writes only this user's local config, making no cluster changes. This is the
		// second-user and re-run path (ADR-0038 §4).
		return joinExistingInstall(ctx, a, cs, stdout)
	}

	// What this install will do about its database, said before anything is applied (ADR-0086 §5) —
	// including, for the default, that it creates cluster-scoped CustomResourceDefinitions and takes
	// longer. The operator is installed straight afterwards, from the SAME read the plan was printed
	// from, so the plan cannot announce one thing and the install do another.
	if err := installControlPlaneDatabaseOperator(ctx, a, database, cs, stdout, stderr); err != nil {
		return err
	}

	if err := applyFn(ctx, a.kubeconfig, a.kubeContext, manifests, a.verbose, stdout, stderr); err != nil {
		return err
	}

	if a.wait {
		if err := waitForReady(ctx, a.kubeconfig, a.kubeContext, a.namespace, database, stdout); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "\nBurrow is installed and ready in namespace %q.\n", a.namespace)
	} else {
		fmt.Fprintf(stdout, "\nBurrow installed into namespace %q (not waiting for readiness).\n", a.namespace)
	}
	writeDefaultEnvironmentNotice(stdout, a.appNamespace)

	// Installing tells you what your cluster can do (ADR-0034): probe the cluster's capabilities
	// kubeconfig-side and print a one-line summary. The probe is read-only and best-effort — a
	// failure here never fails a successful install, since the agent reads capabilities live anyway.
	printCapabilitySummary(ctx, cs, stdout)

	// Auto-ensure the lightweight, detected baseline on top of the control plane (ADR-0054 §1):
	// metrics-server, so `app autoscale` (HPA), `kubectl top`, and utilization reporting behave the
	// same on every cluster. It detects a vendor copy (k3s/GKE/AKS) and leaves it alone, installs the
	// baseline where absent (EKS/DOKS/kind), and is opt-out via --minimal / --no-metrics-server. It is
	// best-effort: a baseline hiccup must not fail an already-installed control plane.
	// force is false here and only reachable from `burrow cluster metrics install --force`: an
	// install must never replace a registration it did not make without being asked to (ADR-0096 §3).
	// A registered-but-not-serving Metrics API has already been reported in full, and the generic
	// warning below is suppressed for it — its closing advice is to re-run install, which on that
	// cluster reaches the same refusal a second time.
	if err := ensureMetricsServer(ctx, a.kubeconfig, a.kubeContext, cs, a.minimal || a.noMetricsServer, false, a.verbose, stdout, stderr); err != nil &&
		!errors.Is(err, errMetricsAPINotServing) {
		fmt.Fprintf(stdout, "\n%scould not ensure the metrics-server baseline: %v\n"+
			"The control plane is installed; ensure it later with a metrics-server manifest, or re-run install.\n", warning(stdout), err)
	}

	// Cluster-only (bootstrap on the VPS): the cluster-side effects are done — burrowd deployed and
	// the scoped burrow-agent credential minted by the manifests. Skip the laptop-oriented local
	// bookkeeping (recording a ~/.burrow handle) and the "connect your agent" guidance; the caller
	// prints the join-token block for the laptop instead (ADR-0044).
	if a.clusterOnly {
		return nil
	}

	// Name and record the environment (ADR-0036/0037): write a local handle pinned as current, so
	// first-run detection flips and `burrow env list` shows it without connecting. This also mints
	// the scoped agent credential (ADR-0038) and records its kubeconfig path on the handle.
	if err := recordEnvironment(ctx, a, cs, stdout); err != nil {
		return err
	}

	// Record which install this is on the target pointed at this context (ADR-0084 §5), so a later
	// command can tell whether it arrived here or at a different Burrow standing behind the same
	// context name.
	recordInstallID(a.kubeContext, installID, stdout)

	if a.wait {
		fmt.Fprintf(stdout, "%s %s", okMark(stdout), postInstallGuidance)
	}
	return nil
}

// writeDefaultEnvironmentNotice states what install just created: one environment, called `prod`,
// mapped to the app namespace, carrying production-grade guardrail defaults (ADR-0067 §2–§3).
//
// ADR-0067 asks for this to be said plainly, and the reason is the case it gets wrong. Someone whose
// only cluster is genuinely a sandbox now has an environment called `prod` and will find the
// defaults stricter than they want. That is the intended direction of error — over-strict on a
// sandbox is an annoyance a `guard set` fixes; under-strict on real production is not recoverable —
// but being told is the difference between a considered default and a surprise.
//
// It leads with what the environment DOES ("apps deploy into") and names the namespace, because a
// few lines further on `recordEnvironment` prints a second thing also called an environment: the
// LOCAL handle for this cluster in ~/.burrow/config (ADR-0036). Two different things share the word,
// so this one is described by its effect rather than by the word alone.
func writeDefaultEnvironmentNotice(w io.Writer, appNamespace string) {
	fmt.Fprintf(w, "\nApps deploy into the environment %q (namespace %q).\n", controlplane.DefaultEnvironment, appNamespace)
	fmt.Fprintf(w, "It is called %q because a single environment is production, and it carries production-grade\n"+
		"guardrails: deleting an app or a DNS record is denied until you relax it. Changing the policy\n"+
		"takes a credential of your own, so sign in first and then loosen one:\n"+
		"  burrow auth login --context <cluster>\n"+
		"  burrow guard set app.delete confirm\n"+
		"Add a second environment when you want one:  burrow env add staging\n",
		controlplane.DefaultEnvironment)
}

// postInstallGuidance is the tail a successful `install --wait` prints: Burrow is operated by an AI
// agent through the scoped `burrow-agent` CLI, not by CLI deploys, so it points the user at wiring
// their agent rather than at a `burrow app deploy` command. No em-dashes: it is user-facing CLI output.
const postInstallGuidance = "Burrow is ready. Wire your AI agent to operate it:\n" +
	"  burrow agent claude install\n\n" +
	"Then open your agent and ask it to deploy your app.\n"

// recordEnvironment writes the just-installed environment into the local config as a handle and
// pins it as the current environment (ADR-0036/0037). The name is the explicit --environment or a
// generated adjective-animal name. It mints the scoped agent credential (ADR-0038) and records its
// kubeconfig path on the handle, then prints the confirmation and the rename hint.
func recordEnvironment(ctx context.Context, a installArgs, cs kubernetes.Interface, stdout io.Writer) error {
	name := a.environment
	if name == "" {
		name = friendlyName()
	}

	// Mint the scoped, burrowd-only agent credential and write its kubeconfig under ~/.burrow/
	// (ADR-0038). No consumer reads it yet (that is phase 2), so a mint hiccup — e.g. a slow token
	// controller — must not fail an otherwise-installed control plane: warn and record the handle
	// without the credential, which a re-run can provision.
	agentKubeconfig, agentContext, err := mintAgentCredentialFn(ctx, a, name, cs, stdout)
	if err != nil {
		fmt.Fprintf(stdout, "\n%scould not mint the scoped agent credential: %v\n"+
			"The control plane is installed; re-run `burrow cluster install` to provision it.\n", warning(stdout), err)
	}

	if err := addAndPinEnvironment(a, name, agentKubeconfig, agentContext); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "\nInstalled. Environment %q is now your current environment.\n", name)
	fmt.Fprintf(stdout, "Rename it any time:  burrow env rename %s <new-name>\n", name)
	noteRegisteredTarget(a.kubeContext, stdout)
	return nil
}

// installHandle builds the environment-handle template for an install/join target from installArgs.
// Cluster-per-environment: the whole cluster is the environment, so commands send burrowd no env
// name and get the default app namespace and the global guardrails (ADR-0036); a
// namespace-per-environment env carries its registered name instead (see `burrow env add`). The
// scoped agent credential is filled in by saveJoinedEnvironment.
func installHandle(a installArgs, name string) localconfig.Environment {
	return localconfig.Environment{
		Name:                  name,
		Context:               a.kubeContext,
		ControlPlaneNamespace: a.namespace,
		AppNamespace:          a.appNamespace,
		Env:                   "",
	}
}

// addAndPinEnvironment registers the environment handle for a fresh install target and pins it as
// current (ADR-0036/0037), carrying the scoped agent credential (ADR-0038). A fresh install always
// registers a new handle, so this is the add-and-pin case of saveJoinedEnvironment.
func addAndPinEnvironment(a installArgs, name, agentKubeconfig, agentContext string) error {
	cfg, err := localconfig.Load()
	if err != nil {
		return err
	}
	return saveJoinedEnvironment(cfg, name, false, installHandle(a, name), agentKubeconfig, agentContext)
}

// joinEnvironmentName resolves the handle name for a join and whether an existing handle for the
// kube context is being updated: reuse an existing handle's name, else the explicit name, else a
// generated adjective-animal name. The name is resolved before the credential is landed because it
// names the local kubeconfig file. Shared by install's join-existing path and `burrow join`
// (ADR-0044) so both name a joined environment identically.
func joinEnvironmentName(cfg *localconfig.Config, kubeContext, explicit string) (name string, updateExisting bool) {
	if existing, ok := cfg.LookupByContext(kubeContext); ok {
		return existing.Name, true
	}
	if explicit != "" {
		return explicit, false
	}
	return friendlyName(), false
}

// saveJoinedEnvironment records a joined environment's scoped credential into cfg, pins it as
// current, registers the cluster as a TARGET, and Saves (ADR-0038 §4, ADR-0078 §1). It updates an
// existing handle's credential in place (updateExisting) — keeping the join idempotent — or
// registers the new handle built by the caller. Shared by a fresh install, install's join-existing
// path, and `burrow join` (ADR-0044) so all three record a handle the same way.
//
// The target is the half that was missing. `burrow auth login` wrote targets and nothing else did,
// so a person whose only Burrow was one they installed themselves had none — and signing in to the
// managed product then left them with a single target, every command re-pointed at it, and `burrow
// auth switch` listing nothing to go back to (cloud#201). An install is the act that says a cluster
// runs Burrow, which is exactly what a target records, so it is the right place to record one.
//
// It REGISTERS rather than selects, and the difference is the point: whichever target is active
// stays active. Repointing somebody who deliberately chose something else is the same silent
// redirection cloud#201 is about, aimed the other way. What changes is that there is now something
// to switch to, and noteRegisteredTarget says so when the active target is not this cluster.
func saveJoinedEnvironment(cfg *localconfig.Config, name string, updateExisting bool, handle localconfig.Environment, agentKubeconfig, agentContext string) error {
	if updateExisting {
		cfg.SetAgentCredential(name, agentKubeconfig, agentContext)
	} else {
		handle.AgentKubeconfig = agentKubeconfig
		handle.AgentContext = agentContext
		if err := cfg.Add(handle); err != nil {
			return err
		}
	}
	cfg.Current = name
	// Every caller resolves a context before it gets here — install refuses without one and a join
	// token carries a validated one — so the guard is for the shape of the code rather than a state
	// anything reaches: a nameless target would fail validation, and failing the local bookkeeping of
	// a control plane that is already installed and working is the one outcome worth ruling out.
	if handle.Context != "" {
		if _, err := cfg.RegisterTarget(localconfig.KubernetesTarget(handle.Context)); err != nil {
			return err
		}
	}
	return cfg.Save()
}

// noteRegisteredTarget says that commands are STILL GOING SOMEWHERE ELSE, when an install has just
// happened and the active target names a different place.
//
// That is the whole message, and it is the one case worth a line. The install registers a target and
// deliberately does not select one, so somebody signed in to the managed product who installs Burrow
// into a cluster of their own has just done a large, successful, cluster-changing thing whose effect
// on their next command is: none. Discovering that from the next `burrow app list` is precisely the
// shape of cloud#201, only in reverse.
//
// It is silent otherwise. With this cluster already selected there is nothing to report, and with
// nothing selected commands resolve exactly as they did a moment ago — through the environment handle
// the install just pinned, which is this cluster — so a line about targets would be raising a
// distinction that has no consequence yet. `burrow auth status` lists the registered target for
// anybody who wants to see it.
//
// It reads the config back rather than being handed one, for the same reason recordInstallID does:
// it runs after the install has been reported, so the state it describes is the state on disk, and
// a bookkeeping read that fails is worth nothing more than silence behind a control plane that is
// installed and working.
func noteRegisteredTarget(kubeContext string, out io.Writer) {
	if kubeContext == "" {
		return
	}
	cfg, err := localconfig.Load()
	if err != nil {
		return
	}
	target, selected, err := cfg.ActiveTarget()
	if err != nil || !selected {
		return
	}
	if target.Kind == localconfig.TargetKindKubernetes && target.Context == kubeContext {
		return
	}
	fmt.Fprintf(out, "\nThis cluster is registered as the target %q, but it is not the active one: your commands\n"+
		"still go to %s. Point them at this cluster with:  burrow auth switch %s\n",
		kubeContext, target.Describe(), kubeContext)
}

// joinExistingInstall performs the local join of an already-installed cluster (ADR-0038 §4): it reads
// the existing scoped agent credential with this user's own kubeconfig access and writes only their
// local config — no cluster resources are minted or changed. This is the second-user path and the
// re-run path. It is idempotent: a handle already registered for this context is updated in place
// with the (possibly refreshed) credential rather than duplicated. A join that cannot read the agent
// credential surfaces readAgentToken's actionable error.
func joinExistingInstall(ctx context.Context, a installArgs, cs kubernetes.Interface, stdout io.Writer) error {
	cfg, err := localconfig.Load()
	if err != nil {
		return err
	}

	name, updateExisting := joinEnvironmentName(cfg, a.kubeContext, a.environment)

	agentKubeconfig, agentContext, err := joinAgentCredentialFn(ctx, a.kubeconfig, a.kubeContext, a.namespace, name)
	if err != nil {
		return err
	}

	if err := saveJoinedEnvironment(cfg, name, updateExisting, installHandle(a, name), agentKubeconfig, agentContext); err != nil {
		return err
	}

	// The joining person learns which install this is from the cluster itself, which is why the id
	// lives in a ConfigMap rather than a Secret (ADR-0084 §5): reading it needs no privileged access,
	// because it grants none. An install that predates ids has no ConfigMap, and records nothing
	// quietly; a read that actually FAILED is said out loud, because the join otherwise succeeds and
	// the person would have no way to know their target was left unchecked. Neither fails the join —
	// the local config is written either way.
	if id, err := readInstallID(ctx, cs, a.namespace); err != nil {
		fmt.Fprintf(stdout, "\n%scould not read which install this is, so your target will not be checked against it: %v\n"+
			"Re-run `burrow cluster install %s` once that is resolved.\n", warning(stdout), err, a.kubeContext)
	} else {
		recordInstallID(a.kubeContext, id, stdout)
	}

	fmt.Fprintf(stdout, "\nJoined the existing Burrow install in namespace %q.\n", a.namespace)
	fmt.Fprintln(stdout, "This wrote only your local config (~/.burrow); no cluster changes were made.")
	fmt.Fprintf(stdout, "Environment %q is now your current environment.\n", name)
	fmt.Fprintf(stdout, "Rename it any time:  burrow env rename %s <new-name>\n", name)
	noteRegisteredTarget(a.kubeContext, stdout)
	return nil
}

// recordInstallID writes the id of the install now running behind a kube context onto every local
// record that names that context — the target the `burrow` CLI resolves through and the environment
// handle `burrow-agent` resolves through (ADR-0084 §5). The context name is how a command gets to a
// cluster; the id is how it knows it arrived at this Burrow rather than at another one standing
// behind a name that was reused.
//
// It is best-effort and quiet on both of the ways it can do nothing:
//
//   - Nothing is registered for this context. Installing does not require having run `burrow auth
//     login` first, so this is an ordinary state, not a failure. The id is recorded on the cluster
//     regardless, and a target or handle pointed here later picks it up.
//   - The local config cannot be loaded or saved. The control plane is installed and working at this
//     point; a bookkeeping problem is worth saying out loud but must not fail the install behind it.
//
// It prints nothing on success. The id is machinery, not a thing anyone needs to read or keep.
func recordInstallID(kubeContext, installID string, stdout io.Writer) {
	if kubeContext == "" || installID == "" {
		return
	}
	cfg, err := localconfig.Load()
	if err != nil {
		fmt.Fprintf(stdout, "\n%scould not load the local config to record which install this is: %v\n", warning(stdout), err)
		return
	}
	if !cfg.SetInstallID(kubeContext, installID) {
		return
	}
	if err := cfg.Save(); err != nil {
		fmt.Fprintf(stdout, "\n%scould not record which install this is locally: %v\n", warning(stdout), err)
	}
}

// readInstallID reads an install's own id out of the ConfigMap it records it in (ADR-0084 §5). It
// separates "this install has no id" from "the id could not be read", because the two look identical
// at the call site and mean opposite things.
//
// An absent ConfigMap, or one carrying no id, returns "" and no error: the control plane predates
// install ids, which is an ordinary state needing no comment — the next upgrade mints one. Anything
// else is a real failure and is returned: an RBAC denial or an API-server outage means the id is
// unknown rather than absent, and silently recording nothing there would leave somebody wondering
// why their target is never checked.
func readInstallID(ctx context.Context, cs kubernetes.Interface, namespace string) (string, error) {
	cm, err := cs.CoreV1().ConfigMaps(namespace).Get(ctx, connect.DefaultInstallConfigMap, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading configmap %s/%s: %w", namespace, connect.DefaultInstallConfigMap, err)
	}
	return cm.Data[connect.DefaultInstallIDKey], nil
}

// errNoCluster is the clear stop when there is no kubeconfig (or it holds no contexts): Burrow
// operates a cluster you point it at, so it explains how to point it rather than surfacing a raw
// library error.
func errNoCluster() error {
	return fmt.Errorf("no kubeconfig found, so there is no cluster to install into. Burrow operates a " +
		"cluster you point it at: set $KUBECONFIG or create ~/.kube/config, then run `burrow cluster install <context>`")
}

// writeInstallContextHint lists the kubeconfig contexts (marking the current one) and, for each,
// probes whether Burrow is already installed, so a user picking a cluster to install into can see at
// a glance which contexts already run Burrow and which are free. It instructs re-running install
// with a context that has none; it does not install and does not prompt (ADR-0037). Probing is
// sequential and bounded per context by connect.ProbeTimeout, matching `burrow env list --discover`.
func writeInstallContextHint(ctx context.Context, w io.Writer, kubeconfig, namespace string, contexts []connect.Context) {
	fmt.Fprint(w, "Install the Burrow control plane into your cluster.\n\n")
	fmt.Fprintf(w, "The control plane installs into a namespace (default %q); your apps deploy into the\n"+
		"app namespace (default %q).\n\n", connect.DefaultNamespace, connect.DefaultAppNamespace)
	fmt.Fprint(w, "Choose a context to install into. Detected Kubernetes contexts:\n\n")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CURRENT\tCONTEXT\tCLUSTER\tBURROWD")
	for _, c := range contexts {
		marker := ""
		if c.Current {
			marker = "*"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", marker, c.Name, c.Cluster, installStatusFor(ctx, kubeconfig, c.Name, namespace))
	}
	_ = tw.Flush()
	// Close with the same Examples block `install -h` shows and a single Usage line at the bottom,
	// matching the kubectl-style help layout.
	fmt.Fprintf(w, "\nExamples:\n%s\n\nUsage:\n  burrow cluster install <context> [flags]\n", installExamples)
}

// installStatusFor probes one context for an installed burrowd and renders its BURROWD cell:
// "installed (<tag>)", "not installed", or "unreachable (<reason>)". It reuses the discovery probe
// seam and classifyProbe so the install listing and `burrow env list --discover` cannot diverge.
func installStatusFor(ctx context.Context, kubeconfig, kubeContext, namespace string) string {
	probeCtx, cancel := context.WithTimeout(ctx, connect.ProbeTimeout)
	img, perr := probeContextFn(probeCtx, kubeconfig, kubeContext, namespace)
	cancel()
	status, version, _ := classifyProbe(img, perr)
	switch status {
	case "installed":
		return fmt.Sprintf("installed (%s)", version)
	case "unreachable":
		return fmt.Sprintf("unreachable (%s)", version)
	default:
		return status
	}
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

// installControlPlaneDatabaseOperator prints the database plan and, for the CloudNativePG default,
// puts the operator on the cluster before the manifests that name its `Cluster` are applied
// (ADR-0086 §1). It shares the operator install with `burrow cluster postgres install`
// (ensureCloudNativePG), so there is one set of skip rules and one wait rather than two.
//
// A `plain` install prints its plan and installs no operator: the flag exists for a cluster that
// will not have one, so reaching for it there would defeat the choice.
func installControlPlaneDatabaseOperator(ctx context.Context, a installArgs, database string,
	cs kubernetes.Interface, stdout, stderr io.Writer) error {
	if database == databasePlain {
		writeInstallDatabasePlan(stdout, a.verbose, database, cloudNativePGState{})
		return nil
	}

	found, err := detectCloudNativePGFn(ctx, cs)
	if err != nil {
		return err
	}
	state := cloudNativePGState{ready: found.Ready, version: found.Version}
	writeInstallDatabasePlan(stdout, a.verbose, database, state)

	fmt.Fprintln(stdout, "\nInstalling:")
	r := installReporter{w: stdout, verbose: a.verbose}
	if err := ensureCloudNativePG(ctx, a.kubeconfig, a.kubeContext, cs, found, r, a.wait, a.verbose, stdout, stderr); err != nil {
		return errCloudNativePGRequired(a.kubeContext, err)
	}

	// The `Cluster` in the manifests below cannot be applied until the API server is serving the
	// definition that describes it, and a CustomResourceDefinition is established a moment after it
	// is created rather than the instant the apply returns. This wait is NOT the readiness wait
	// --wait governs: it is a precondition of the next apply, so it runs either way. Without it a
	// `--wait=false` install fails on "no matches for kind Cluster" a second after installing the
	// very thing that provides it.
	if err := waitForCloudNativePGAPI(ctx, cs, cnpgAPIWait); err != nil {
		return errCloudNativePGRequired(a.kubeContext, err)
	}
	return nil
}

// waitForReady blocks until the control plane's database and burrowd are ready, printing
// progress. burrowd only becomes ready after it has reached the database and applied its
// migrations, so this confirms the whole control plane is up.
//
// The database is waited for in whichever shape it was installed (ADR-0086 §2). The two are not
// interchangeable readiness checks: a `plain` database is a Deployment that has rolled out, and a
// CloudNativePG one is a `Cluster` with an instance serving — a Deployment wait against a CNPG
// install would sit until it timed out on an object that will never exist.
func waitForReady(ctx context.Context, kubeconfig, kubeContext, namespace, database string, out io.Writer) error {
	cs, err := clientsetFn(kubeconfig, kubeContext)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "\nWaiting for Burrow to become ready...")
	if database == databasePlain {
		if err := waitForDeployment(ctx, cs, namespace, controlPlaneClusterName, "database", out, 3*time.Minute); err != nil {
			return err
		}
	} else {
		ri, err := controlPlaneClusterFn(kubeconfig, kubeContext, namespace)
		if err != nil {
			return err
		}
		if err := waitForControlPlaneCluster(ctx, ri, namespace, out, controlPlaneClusterWait); err != nil {
			return err
		}
	}
	return waitForDeployment(ctx, cs, namespace, "burrowd", "control plane", out, 3*time.Minute)
}

func waitForDeployment(ctx context.Context, cs kubernetes.Interface, namespace, name, label string, out io.Writer, timeout time.Duration) error {
	fmt.Fprintf(out, "  %s ...", label)
	deadline := time.Now().Add(timeout)
	for {
		d, err := cs.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			if deploymentRolledOut(d) {
				fmt.Fprintln(out, " "+okMark(out))
				return nil
			}
		}
		if time.Now().After(deadline) {
			fmt.Fprintln(out, " "+failMark(out)+" timed out")
			return fmt.Errorf("%s did not become ready within %s", label, timeout)
		}
		time.Sleep(2 * time.Second)
	}
}

// deploymentRolledOut reports whether the Deployment's newest revision is fully rolled out,
// using the same completion test as `kubectl rollout status` (Kubernetes
// deploymentutil.DeploymentComplete). Status.ReadyReplicas alone is insufficient: it counts
// ready pods across BOTH the old and new ReplicaSets, so during a rolling update the old pod
// keeps it satisfied while the new pod is still ContainerCreating — greenlighting the old
// revision. Requiring UpdatedReplicas/AvailableReplicas to reach desired and Replicas to equal
// UpdatedReplicas confirms the new revision is the only one left and available.
func deploymentRolledOut(d *appsv1.Deployment) bool {
	desired := int32(1)
	if d.Spec.Replicas != nil {
		desired = *d.Spec.Replicas
	}
	return desired > 0 &&
		d.Status.ObservedGeneration >= d.Generation &&
		d.Status.UpdatedReplicas >= desired &&
		d.Status.Replicas == d.Status.UpdatedReplicas &&
		d.Status.AvailableReplicas >= desired
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating random secret: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// mintAgentCredentialFn is the seam recordEnvironment uses to mint and write the scoped agent
// credential (ADR-0038). It is a package var so install's other tests can substitute a no-op that
// records a fixed path without a real cluster or token controller; the real path is
// mintAgentCredential.
var mintAgentCredentialFn = mintAgentCredential

// mintAgentCredential builds the REST config for the install target, mints the scoped burrowd-only
// kubeconfig from the agent ServiceAccount's long-lived token Secret, and writes it under ~/.burrow/
// named for the environment. It returns the written path and the context name inside it.
func mintAgentCredential(ctx context.Context, a installArgs, envName string, cs kubernetes.Interface, _ io.Writer) (kubeconfigPath, kubeContext string, err error) {
	restCfg, err := connect.RESTConfig(a.kubeconfig, a.kubeContext)
	if err != nil {
		return "", "", err
	}
	data, err := mintAgentKubeconfig(ctx, cs, restCfg, a.namespace, agentTokenSecretName)
	if err != nil {
		return "", "", err
	}
	path, err := writeAgentKubeconfig(envName, data)
	if err != nil {
		return "", "", err
	}
	return path, agentKubeContextName, nil
}

func renderManifests(o installOptions) (string, error) {
	if o.ServiceAccount == "" {
		o.ServiceAccount = "burrowd"
	}
	if o.Database == "" {
		o.Database = databaseCNPG
	}
	if o.AgentServiceAccount == "" {
		o.AgentServiceAccount = agentServiceAccountFn(defaultPrincipal)
	}
	var sb strings.Builder
	if err := installTemplate.Execute(&sb, o); err != nil {
		return "", fmt.Errorf("rendering manifests: %w", err)
	}
	return sb.String(), nil
}
