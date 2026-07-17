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
func newUpgradeCmd() *cobra.Command {
	var namespace, image, kubeconfig string
	var dryRun, wait, verbose bool
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade the in-cluster control plane in place (preserves state)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUpgrade(cmd.Context(), namespace, image, kubeconfig, dryRun, wait, verbose, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().StringVar(&namespace, "namespace", connect.DefaultNamespace, "namespace the control plane is installed in")
	cmd.Flags().StringVar(&image, "burrowd-image", defaultBurrowdImage(), "burrowd image to upgrade to (default: this CLI's pinned release)")
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "path to kubeconfig (default: ambient)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the manifests instead of applying them")
	cmd.Flags().BoolVar(&wait, "wait", true, "wait for the control plane to become ready")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "show every resource burrow applies instead of a summary")
	return cmd
}

func runUpgrade(ctx context.Context, namespace, image, kubeconfig string, dryRun, wait, verbose bool, stdout, stderr io.Writer) error {
	if image == "" {
		return errNoBurrowdImage()
	}
	cs, err := clientset(kubeconfig)
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
	if err := applyFn(ctx, kubeconfig, "", manifests, verbose, stdout, stderr); err != nil {
		return err
	}
	if !wait {
		fmt.Fprintf(stdout, "\nBurrow upgrade applied in namespace %q (not waiting for readiness).\n", namespace)
		backfillAgentCredential(ctx, kubeconfig, namespace, stdout)
		return nil
	}
	if err := waitForReady(ctx, kubeconfig, "", namespace, stdout); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "\n%s Burrow is upgraded and ready in namespace %q.\n", okMark(stdout), namespace)
	backfillAgentCredential(ctx, kubeconfig, namespace, stdout)
	return nil
}

// backfillAgentCredential provisions the local scoped agent kubeconfig for the operator's own
// environment after an upgrade (ADR-0038 §4). The install manifests an upgrade re-applies mint the
// agent ServiceAccount/Role/Secret cluster-side even for a pre-Phase-1 install; this is the matching
// local-side backfill, so a control plane installed before the scoped credential existed gains the
// local kubeconfig without a fresh install. It is best-effort: any failure warns and returns, never
// failing the upgrade. It backfills only a handle already registered for the upgraded cluster's
// context; if none is registered locally there is nothing to backfill (the operator can `burrow
// install` to register and join).
func backfillAgentCredential(ctx context.Context, kubeconfig, namespace string, stdout io.Writer) {
	ctxName, err := connect.TargetContextName(kubeconfig, "")
	if err != nil {
		fmt.Fprintf(stdout, "\n%scould not resolve the current kube context to backfill the agent credential: %v\n", warning(stdout), err)
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
// database password, and the app namespace — from the cluster, and returns render options
// that keep them while swapping in the new image. Re-minting the secrets would invalidate the
// running token and orphan the existing Postgres volume, so they are read, not regenerated.
func upgradeOptions(ctx context.Context, cs kubernetes.Interface, namespace, image string) (installOptions, error) {
	token, err := secretValue(ctx, cs, namespace, connect.DefaultTokenSecret, connect.DefaultTokenKey)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return installOptions{}, fmt.Errorf("Burrow is not installed in namespace %q; run `burrow install` first", namespace)
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
	return installOptions{
		Namespace:      namespace,
		AppNamespace:   appNamespace,
		AddonNamespace: addonNamespace,
		BuildNamespace: connect.DefaultBuildNamespace,
		Image:          image,
		Token:          token,
		DBPassword:     dbPassword,
		Port:           connect.DefaultPort,
	}, nil
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

// clientset builds a Kubernetes clientset from the kubeconfig (or in-cluster config), using
// the same config resolution as connect, targeting the kubeconfig's current context.
func clientset(kubeconfig string) (*kubernetes.Clientset, error) {
	return clientsetForContext(kubeconfig, "")
}

// clientsetForContext is clientset with an explicit kubeconfig context override: when kubeContext
// is non-empty it selects that context's cluster instead of the current one (ADR-0035), so a
// command can probe a specific environment's control plane.
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
