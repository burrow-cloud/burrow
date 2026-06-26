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
	cmd.Flags().BoolVar(&verbose, "verbose", false, "show every resource kubectl applies instead of a summary")
	return cmd
}

func runUpgrade(ctx context.Context, namespace, image, kubeconfig string, dryRun, wait, verbose bool, stdout, stderr io.Writer) error {
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
	if err := kubectlApply(ctx, kubeconfig, manifests, verbose, stdout, stderr); err != nil {
		return err
	}
	if !wait {
		fmt.Fprintf(stdout, "\nBurrow upgrade applied in namespace %q (not waiting for readiness).\n", namespace)
		return nil
	}
	if err := waitForReady(ctx, kubeconfig, namespace, stdout); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "\nBurrow is upgraded and ready in namespace %q.\n", namespace)
	return nil
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
	return installOptions{
		Namespace:    namespace,
		AppNamespace: appNamespace,
		Image:        image,
		Token:        token,
		DBPassword:   dbPassword,
		Port:         connect.DefaultPort,
	}, nil
}

// appNamespaceOf reads the app namespace from the running burrowd Deployment's
// BURROW_NAMESPACE env, so an upgrade keeps deploying apps where they already go rather than
// silently moving them.
func appNamespaceOf(ctx context.Context, cs kubernetes.Interface, namespace string) (string, error) {
	d, err := cs.AppsV1().Deployments(namespace).Get(ctx, "burrowd", metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("reading the burrowd deployment in %s: %w", namespace, err)
	}
	for _, c := range d.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			if e.Name == "BURROW_NAMESPACE" {
				return e.Value, nil
			}
		}
	}
	return "", fmt.Errorf("the burrowd deployment in %s has no BURROW_NAMESPACE env", namespace)
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
// the same config resolution as connect.
func clientset(kubeconfig string) (*kubernetes.Clientset, error) {
	cfg, err := connect.RESTConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building clientset: %w", err)
	}
	return cs, nil
}
