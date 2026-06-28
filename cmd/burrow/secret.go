// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/burrow-cloud/burrow/connect"
)

// restartedAtAnnotation forces a rolling update when a secret value changes under an existing key
// (which does not otherwise mutate the pod template). It mirrors controlplane.RestartedAtAnnotation,
// duplicated here because the CLI (Apache-2.0) does not import the control-plane packages (FSL).
const restartedAtAnnotation = "burrow.cloud/restarted-at"

// appSecretName is the per-app Secret holding an app's secret env, in the app namespace (ADR-0028).
// It mirrors controlplane.AppSecretName; the scheme is duplicated across the license boundary so
// both the CLI (which writes the Secret with the kubeconfig) and burrowd compute the same name.
func appSecretName(app string) string { return "burrow-app-" + app + "-secrets" }

// newSecretCmd groups an app's secret environment configuration (ADR-0028). The security model
// splits by whether a value is involved (ADR-0004):
//
//   - `secret set` carries a VALUE, so it never touches MCP or burrowd: the CLI writes the value
//     straight into the per-app Kubernetes Secret with the developer's kubeconfig, exactly like
//     `provider add` / `registry login`. The value never reaches the control plane.
//   - `secret list` (KEYS only) and `secret unset` (by KEY) carry no value, so they go through
//     burrowd over the API and are also MCP tools.
func newSecretCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Manage an app's secret environment configuration",
		Long: "secret manages an app's secret environment — database URLs, API keys — sourced into\n" +
			"the workload at runtime from a per-app Kubernetes Secret. Secret VALUES never travel\n" +
			"over MCP or the control-plane API and are never written to the control plane's database\n" +
			"(ADR-0004): `secret set` writes the value directly to the cluster with your kubeconfig.\n" +
			"`secret list` shows only the KEYS, never the values.\n\n" +
			"NEVER paste a secret value into an agent prompt — anything in the prompt is retained in\n" +
			"the conversation and re-sent on later tool calls. Run `secret set` yourself; the agent\n" +
			"can confirm the key is present with `secret list`.",
	}
	cmd.AddCommand(newSecretSetCmd(), newSecretListCmd(), newSecretUnsetCmd())
	return cmd
}

// newSecretSetCmd writes a secret value directly into the per-app Secret with the developer's
// kubeconfig — never through burrowd (the value never crosses the API or MCP; ADR-0004). After
// writing it bumps the Deployment's pod-template restart annotation (unless --no-restart) so the
// running app rolls and picks the new value up; envFrom is read only at pod start.
func newSecretSetCmd() *cobra.Command {
	var namespace, appNamespace, kubeconfig string
	var noRestart bool
	cmd := &cobra.Command{
		Use:   "set <app> KEY=VALUE",
		Short: "Set (upsert) a secret environment variable for an app",
		Long: "set writes a secret value directly into the app's Kubernetes Secret with your\n" +
			"kubeconfig. The value never travels over MCP or the control-plane API and is never\n" +
			"stored in the control plane's database (ADR-0004).\n\n" +
			"NEVER paste a secret value into an agent prompt — it is retained in the conversation\n" +
			"and re-sent on every later tool call. Run this command yourself at your terminal; the\n" +
			"agent can confirm the key landed with `burrow app secret list <app>`.\n\n" +
			"By default the running app is rolled so it picks the value up; pass --no-restart to\n" +
			"defer it to the next deploy.",
		Example: "  burrow app secret set web STRIPE_SECRET_KEY=sk_live_…\n" +
			"  burrow app secret set web DATABASE_URL=postgres://…",
		Args: exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			app := args[0]
			var kv kvFlag
			if err := kv.Set(args[1]); err != nil {
				return err
			}
			cs, err := clientset(kubeconfig)
			if err != nil {
				return err
			}
			appNS := appNamespace
			if appNS == "" {
				if appNS, err = appNamespaceOf(ctx, cs, namespace); err != nil {
					return err
				}
			}
			out := cmd.OutOrStdout()
			for k, v := range kv.m {
				if err := setAppSecret(ctx, cs, appNS, app, k, v); err != nil {
					return err
				}
				human := fmt.Sprintf("set secret %s on %s (namespace %s)", k, app, appNS)
				if !noRestart {
					rolled, err := restartWorkload(ctx, cs, appNS, app)
					if err != nil {
						return err
					}
					if !rolled {
						human += " (not yet deployed; lands on first deploy)"
					}
				} else {
					human += " (not restarted; lands on next deploy)"
				}
				fmt.Fprintln(out, human)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&namespace, "namespace", connect.DefaultNamespace, "control-plane namespace Burrow is installed in")
	cmd.Flags().StringVar(&appNamespace, "app-namespace", "", "namespace apps deploy into (default: discovered from the install)")
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "path to kubeconfig (default: ambient)")
	cmd.Flags().BoolVar(&noRestart, "no-restart", false, "persist the value without rolling the running workload; it lands on the next deploy")
	return cmd
}

func newSecretListCmd() *cobra.Command {
	o := &commonOpts{}
	cmd := &cobra.Command{
		Use:   "list <app>",
		Short: "List an app's secret keys (never the values)",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			keys, err := c.Secrets(ctx, args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if o.json {
				return emit(out, true, map[string][]string{"keys": keys}, "")
			}
			if len(keys) == 0 {
				fmt.Fprintf(out, "No secrets set for %s. Set one with `burrow app secret set %s KEY=VALUE`.\n", args[0], args[0])
				return nil
			}
			for _, k := range keys {
				fmt.Fprintln(out, k)
			}
			return nil
		},
	}
	bindCommon(cmd.Flags(), o)
	return cmd
}

func newSecretUnsetCmd() *cobra.Command {
	o := &commonOpts{}
	var noRestart bool
	cmd := &cobra.Command{
		Use:   "unset <app> KEY",
		Short: "Remove a secret from an app",
		Args:  exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			app, key := args[0], args[1]
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			if err := c.UnsetSecret(ctx, app, key, noRestart); err != nil {
				return err
			}
			human := fmt.Sprintf("unset secret %s on %s", key, app)
			if noRestart {
				human += " (not restarted; lands on next deploy)"
			}
			return emit(cmd.OutOrStdout(), o.json, map[string]string{"app": app, "key": key}, human)
		},
	}
	bindCommon(cmd.Flags(), o)
	cmd.Flags().BoolVar(&noRestart, "no-restart", false, "persist the removal without rolling the running workload; it lands on the next deploy")
	return cmd
}

// setAppSecret upserts key=value into the app's per-app Secret in the app namespace, creating the
// Secret if absent. It acts with the developer's kubeconfig; the value never crosses the API or MCP
// (ADR-0004). It retries on conflict since a concurrent set/unset can race the resourceVersion.
func setAppSecret(ctx context.Context, cs kubernetes.Interface, namespace, app, key, value string) error {
	secrets := cs.CoreV1().Secrets(namespace)
	name := appSecretName(app)
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		existing, err := secrets.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			_, err = secrets.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: namespace,
					Labels:    map[string]string{"app.kubernetes.io/name": app, "app.kubernetes.io/managed-by": "burrow"},
				},
				Type: corev1.SecretTypeOpaque,
				Data: map[string][]byte{key: []byte(value)},
			}, metav1.CreateOptions{})
			return err
		}
		if err != nil {
			return err
		}
		if existing.Data == nil {
			existing.Data = map[string][]byte{}
		}
		existing.Data[key] = []byte(value)
		_, err = secrets.Update(ctx, existing, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return fmt.Errorf("writing secret %s/%s: %w", namespace, name, err)
	}
	return nil
}

// restartWorkload bumps the app Deployment's pod-template restart annotation to now, rolling the
// running app so it picks up the changed Secret (envFrom is read only at pod start). It returns
// (false, nil) when the Deployment does not exist yet — the Secret persists and the next deploy
// injects it via envFrom, which is not an error. Kept fully kubeconfig-side, consistent with
// `secret set` being a kubeconfig operation.
func restartWorkload(ctx context.Context, cs kubernetes.Interface, namespace, app string) (bool, error) {
	patch := []byte(fmt.Sprintf(
		`{"spec":{"template":{"metadata":{"annotations":{%q:%q}}}}}`,
		restartedAtAnnotation, time.Now().UTC().Format(time.RFC3339Nano),
	))
	_, err := cs.AppsV1().Deployments(namespace).Patch(ctx, app, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("rolling deployment %s/%s: %w", namespace, app, err)
	}
	return true, nil
}
