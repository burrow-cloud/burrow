// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/burrow-cloud/burrow/connect"
	"github.com/burrow-cloud/burrow/localconfig"
)

// dbSecretName holds the control-plane database credentials (rendered by the install
// manifests). Its password is preserved across upgrades so the existing data volume stays
// readable.
const dbSecretName = "burrowd-db"

// cmdUpgrade rolls the in-cluster control plane forward in place. It reuses the existing
// install's secrets and app namespace and re-renders the manifests with a new burrowd image,
// so only the burrowd Deployment rolls — Postgres and its data are untouched. The image
// defaults to this CLI's pinned release, so `burrow upgrade` after a CLI update is the whole
// control-plane upgrade. Migrations ride burrowd's startup behind the single-minor-step gate
// (ADR-0013). See ADR-0016.
// newUpgradeAliasCmd is the deprecated top-level `burrow upgrade` (ADR-0060): upgrade now lives
// under the cluster-lifecycle surface as `burrow cluster upgrade`, but the old spelling keeps
// working so existing muscle memory and scripts do not break. It delegates to the same constructor
// and marks itself Deprecated, which both prints a one-line migration hint on use and hides it from
// the main help (Cobra excludes Deprecated commands from the command listing).
func newUpgradeAliasCmd() *cobra.Command {
	cmd := newUpgradeCmd()
	cmd.Deprecated = "use \"burrow cluster upgrade\"."
	return cmd
}

func newUpgradeCmd() *cobra.Command {
	var namespace, image, kubeconfig, kubeContext string
	var dryRun, wait, verbose bool
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade the in-cluster control plane in place (preserves state)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Upgrade acts on a kubeconfig context rather than the active target (ADR-0078 §3), so
			// say which context that is whenever the target names another one. This is the sharpest
			// case of the four: without the note, an upgrade rolls whatever the kubeconfig last
			// pointed at forward and says only which NAMESPACE it touched.
			noteLifecycleContext(kubeconfig, kubeContext, cmd.ErrOrStderr())
			return runUpgrade(cmd.Context(), namespace, image, kubeconfig, kubeContext, dryRun, wait, verbose, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().StringVar(&namespace, "namespace", connect.DefaultNamespace, "namespace the control plane is installed in")
	cmd.Flags().StringVar(&image, "burrowd-image", defaultBurrowdImage(), "burrowd image to upgrade to (default: this CLI's pinned release)")
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "path to kubeconfig (default: ambient)")
	bindLifecycleContext(cmd.Flags(), &kubeContext)
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the manifests instead of applying them")
	cmd.Flags().BoolVar(&wait, "wait", true, "wait for the control plane to become ready")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "show every resource burrow applies instead of a summary")
	return cmd
}

func runUpgrade(ctx context.Context, namespace, image, kubeconfig, kubeContext string, dryRun, wait, verbose bool, stdout, stderr io.Writer) error {
	if image == "" {
		return errNoBurrowdImage()
	}
	// Through the seam install already owns, so an upgrade can be driven end to end against a fake
	// cluster. An empty kubeContext resolves the kubeconfig's current context, which is what an
	// upgrade with no --context acts on.
	cs, err := clientsetFn(kubeconfig, kubeContext)
	if err != nil {
		return err
	}
	opts, err := upgradeOptions(ctx, cs, namespace, image)
	if err != nil {
		return err
	}

	manifests, err := renderManifests(opts)
	if err != nil {
		return err
	}
	if dryRun {
		fmt.Fprint(stdout, manifests)
		return nil
	}

	fmt.Fprintf(stdout, "Upgrading Burrow in namespace %q to image %q...\n", namespace, image)
	if err := applyFn(ctx, kubeconfig, kubeContext, manifests, verbose, stdout, stderr); err != nil {
		return err
	}
	if !wait {
		fmt.Fprintf(stdout, "\nBurrow upgrade applied in namespace %q (not waiting for readiness).\n", namespace)
		backfillAgentCredential(ctx, kubeconfig, kubeContext, namespace, stdout)
		recordUpgradedInstallID(kubeconfig, kubeContext, opts.InstallID, stdout)
		return nil
	}
	if err := waitForReady(ctx, kubeconfig, kubeContext, namespace, opts.Database, stdout); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "\n%s Burrow is upgraded and ready in namespace %q.\n", okMark(stdout), namespace)
	backfillAgentCredential(ctx, kubeconfig, kubeContext, namespace, stdout)
	recordUpgradedInstallID(kubeconfig, kubeContext, opts.InstallID, stdout)
	return nil
}

// recordUpgradedInstallID records the upgraded install's id on the target pointed at the upgraded
// cluster (ADR-0084 §5). It is the local-side counterpart of upgradeOptions minting an id for an
// install that predates them: without it, an install that gains an id at upgrade would have one the
// cluster knows and no target ever learns, so the check would only ever protect clusters installed
// after this shipped. For an install that already had an id this re-records the same value and
// changes nothing.
//
// It is silent about a kube context it cannot resolve, because backfillAgentCredential — which runs
// immediately before it, on the same context, for the same reason — has already said so. Beyond
// that it is exactly as best-effort as the backfill: the control plane is upgraded and running, and
// a local bookkeeping problem must not turn that into a failure.
func recordUpgradedInstallID(kubeconfig, kubeContext, installID string, stdout io.Writer) {
	ctxName, err := connect.TargetContextName(kubeconfig, kubeContext)
	if err != nil {
		return
	}
	recordInstallID(ctxName, installID, stdout)
}

// backfillAgentCredential provisions the local scoped agent kubeconfig for the operator's own
// environment after an upgrade (ADR-0038 §4). The install manifests an upgrade re-applies mint the
// agent ServiceAccount/Role/Secret cluster-side even for a pre-Phase-1 install; this is the matching
// local-side backfill, so a control plane installed before the scoped credential existed gains the
// local kubeconfig without a fresh install. It is best-effort: any failure warns and returns, never
// failing the upgrade. It backfills only a handle already registered for the upgraded cluster's
// context; if none is registered locally there is nothing to backfill (the operator can `burrow
// install` to register and join). A handle that already carries the scoped credential is left
// untouched — an upgrade preserves the credential, so the backfill runs (and reports) exactly
// once, on the upgrade that first adds it, and stays silent on every routine upgrade after.
func backfillAgentCredential(ctx context.Context, kubeconfig, kubeContext, namespace string, stdout io.Writer) {
	ctxName, err := connect.TargetContextName(kubeconfig, kubeContext)
	if err != nil {
		fmt.Fprintf(stdout, "\n%scould not resolve the kube context to backfill the agent credential: %v\n", warning(stdout), err)
		return
	}
	cfg, err := localconfig.Load()
	if err != nil {
		fmt.Fprintf(stdout, "\n%scould not load the local config to backfill the agent credential: %v\n", warning(stdout), err)
		return
	}
	env, ok := cfg.LookupByContext(ctxName)
	if !ok {
		// No local handle for this cluster, so nothing to backfill onto.
		return
	}
	if env.AgentKubeconfig != "" {
		// The handle already carries the scoped credential, which an upgrade preserves: it keeps
		// the API token and does not rotate the agent credential, so an existing credential stays
		// valid. Backfilling exists only to add the credential to a handle that predates it, so
		// re-joining here would be a no-op that re-prints "Backfilled…" on every routine upgrade —
		// misleading noise that reads as "something was missing and I fixed it" when nothing was.
		return
	}
	path, agentContext, err := joinAgentCredentialFn(ctx, kubeconfig, ctxName, namespace, env.Name)
	if err != nil {
		fmt.Fprintf(stdout, "\n%scould not backfill the scoped agent credential for environment %q: %v\n", warning(stdout), env.Name, err)
		return
	}
	cfg.SetAgentCredential(env.Name, path, agentContext)
	if err := cfg.Save(); err != nil {
		fmt.Fprintf(stdout, "\n%sbackfilled the agent credential but could not save the local config: %v\n", warning(stdout), err)
		return
	}
	fmt.Fprintf(stdout, "Backfilled the scoped agent credential for environment %q.\n", env.Name)
}

// upgradeOptions reads the install state that an upgrade must preserve — the API token, the
// database password, the install id, and the app namespace — from the cluster, and returns render
// options that keep them while swapping in the new image. Re-minting them would invalidate the
// running token, orphan the existing Postgres volume, and turn every target pointed at this install
// into a mismatch, so they are read, not regenerated.
func upgradeOptions(ctx context.Context, cs kubernetes.Interface, namespace, image string) (installOptions, error) {
	token, err := secretValue(ctx, cs, namespace, connect.DefaultTokenSecret, connect.DefaultTokenKey)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return installOptions{}, fmt.Errorf("Burrow is not installed in namespace %q; run `burrow cluster install` first", namespace)
		}
		return installOptions{}, err
	}
	dbPassword, err := secretValue(ctx, cs, namespace, dbSecretName, "password")
	if err != nil {
		return installOptions{}, err
	}
	appNamespace, err := appNamespaceOf(ctx, cs, namespace)
	if err != nil {
		return installOptions{}, err
	}
	addonNamespace, err := addonNamespaceOf(ctx, cs, namespace)
	if err != nil {
		return installOptions{}, err
	}
	installID, err := installIDOf(ctx, cs, namespace)
	if err != nil {
		return installOptions{}, err
	}
	database, err := databaseOf(ctx, cs, namespace)
	if err != nil {
		return installOptions{}, err
	}
	return installOptions{
		Namespace:      namespace,
		AppNamespace:   appNamespace,
		AddonNamespace: addonNamespace,
		BuildNamespace: connect.DefaultBuildNamespace,
		Image:          image,
		Token:          token,
		DBPassword:     dbPassword,
		InstallID:      installID,
		Port:           connect.DefaultPort,
		Database:       database,
	}, nil
}

// databaseOf reports which shape this install's database is running in, so an upgrade re-renders
// the one that is there (ADR-0086 §2).
//
// AN UPGRADE MUST NOT CHANGE THE DATABASE UNDERNEATH AN INSTALL. Rendering the default here would
// write a CloudNativePG `Cluster` beside a running Deployment that still holds every row: two
// databases, an empty one in front, and the install's history stranded on a volume nothing reads.
// Moving an existing install from one to the other is a dump and a restore with burrowd stopped;
// ADR-0086 leaves it to its own design and this deliberately does not attempt it.
//
// The read is the plain database's Deployment, and its ABSENCE is what says CloudNativePG: the
// object a `plain` install cannot be without is the one to look for, and it needs no access to a
// CustomResourceDefinition that a `plain` cluster may well not serve.
func databaseOf(ctx context.Context, cs kubernetes.Interface, namespace string) (string, error) {
	_, err := cs.AppsV1().Deployments(namespace).Get(ctx, controlPlaneClusterName, metav1.GetOptions{})
	switch {
	case err == nil:
		return databasePlain, nil
	case apierrors.IsNotFound(err):
		return databaseCNPG, nil
	default:
		return "", fmt.Errorf("reading the control plane's database in namespace %q: %w", namespace, err)
	}
}

// installIDOf returns the id this install already carries, or a freshly minted one when it has none
// (ADR-0084 §5).
//
// Preserving it is the whole point. An install id is what a target compares against to know it
// reached the Burrow it was pointed at, so an upgrade that minted a new one would turn every target
// already pointed here into a mismatch — a routine upgrade breaking exactly the people who were
// using the feature correctly. It gets the same read-do-not-regenerate treatment as the API token
// and the database password.
//
// An install that predates ids has no ConfigMap, and minting one here is how it acquires one: the
// upgrade is where an existing install gains the id it never had. No target is pointed at an id it
// does not yet know, so nothing can mismatch against it — the first thing to record it is whatever
// install or join happens next.
func installIDOf(ctx context.Context, cs kubernetes.Interface, namespace string) (string, error) {
	cm, err := cs.CoreV1().ConfigMaps(namespace).Get(ctx, connect.DefaultInstallConfigMap, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("reading configmap %s/%s: %w", namespace, connect.DefaultInstallConfigMap, err)
	}
	if err == nil {
		if id := cm.Data[connect.DefaultInstallIDKey]; id != "" {
			return id, nil
		}
	}
	return randHex(16)
}

// appNamespaceOf reads the app namespace from the running burrowd Deployment's
// BURROW_NAMESPACE env, so an upgrade keeps deploying apps where they already go rather than
// silently moving them. The env is required: every install renders it, so its absence means
// the deployment is not one burrow installed.
func appNamespaceOf(ctx context.Context, cs kubernetes.Interface, namespace string) (string, error) {
	v, err := burrowdEnv(ctx, cs, namespace, "BURROW_NAMESPACE")
	if err != nil {
		return "", err
	}
	if v == "" {
		return "", fmt.Errorf("the burrowd deployment in %s has no BURROW_NAMESPACE env", namespace)
	}
	return v, nil
}

// addonNamespaceOf reads the add-on namespace from the running burrowd Deployment's
// BURROW_ADDON_NAMESPACE env, so an upgrade re-renders the manifests with the same add-on
// namespace rather than an empty one (which the install template would render as a Namespace
// with no name, failing server-side apply). Installs that predate add-ons carry no such env;
// those fall back to the default add-on namespace.
func addonNamespaceOf(ctx context.Context, cs kubernetes.Interface, namespace string) (string, error) {
	v, err := burrowdEnv(ctx, cs, namespace, "BURROW_ADDON_NAMESPACE")
	if err != nil {
		return "", err
	}
	if v == "" {
		return connect.DefaultAddonNamespace, nil
	}
	return v, nil
}

// burrowdEnv reads a single env var's value from the running burrowd Deployment. It returns
// "" (no error) when the var is absent, and errors only when the Deployment itself cannot be
// read, so callers decide whether a missing var is fatal or has a default.
func burrowdEnv(ctx context.Context, cs kubernetes.Interface, namespace, name string) (string, error) {
	d, err := cs.AppsV1().Deployments(namespace).Get(ctx, "burrowd", metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("reading the burrowd deployment in %s: %w", namespace, err)
	}
	for _, c := range d.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			if e.Name == name {
				return e.Value, nil
			}
		}
	}
	return "", nil
}

// secretValue reads one key from a Secret. The raw NotFound error is preserved (wrapped) so
// callers can distinguish a missing install from other failures.
func secretValue(ctx context.Context, cs kubernetes.Interface, namespace, name, key string) (string, error) {
	s, err := cs.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("reading secret %s/%s: %w", namespace, name, err)
	}
	v, ok := s.Data[key]
	if !ok {
		return "", fmt.Errorf("secret %s/%s has no key %q", namespace, name, key)
	}
	return string(v), nil
}

// alreadyInstalled reports whether a Burrow control plane is already present in the namespace,
// detected by its API-token Secret. `burrow install` refuses to run over an existing install
// (re-minting secrets would break the running control plane); use `burrow upgrade` instead.
func alreadyInstalled(ctx context.Context, cs kubernetes.Interface, namespace string) (bool, error) {
	_, err := cs.CoreV1().Secrets(namespace).Get(ctx, connect.DefaultTokenSecret, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking for an existing install: %w", err)
	}
	return true, nil
}

// clientsetForContext builds a Kubernetes clientset from the kubeconfig (or in-cluster config),
// using the same config resolution as connect. When kubeContext is non-empty it selects that
// context's cluster; empty means the kubeconfig's current context (ADR-0035), so a command can
// probe a specific environment's control plane.
func clientsetForContext(kubeconfig, kubeContext string) (*kubernetes.Clientset, error) {
	cfg, err := connect.RESTConfig(kubeconfig, kubeContext)
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building clientset: %w", err)
	}
	return cs, nil
}
