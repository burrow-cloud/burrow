// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/burrow-cloud/burrow/connect"
)

// registrySecretName is the single dockerconfigjson Secret that holds the developer's
// registry credentials in the app namespace. It is attached to the namespace's default
// ServiceAccount so app Pods inherit it (ADR-0017).
const registrySecretName = "burrow-registry"

// registryClientset builds the Kubernetes clientset the registry subcommands act with. It is a
// package var so tests can substitute a fake, mirroring clientsetFn in install.go; it defaults to
// the real kubeconfig-driven clientset.
var registryClientset = func(kubeconfig string) (kubernetes.Interface, error) {
	return clientset(kubeconfig)
}

// dockerConfig is the on-disk/in-Secret shape of a dockerconfigjson credential file.
type dockerConfig struct {
	Auths map[string]dockerAuth `json:"auths"`
}

type dockerAuth struct {
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Auth     string `json:"auth,omitempty"`
}

// newRegistryCmd manages the cluster's registry pull credentials. It is a setup command: it
// acts with the developer's kubeconfig to provision a Kubernetes pull Secret, distinct from
// the agent-driven operations that flow through burrowd (ADR-0017). The credential never
// travels over MCP and burrowd never handles it. The login/logout/list subcommands share the
// namespace flags and resolve the app namespace from the install.
func newRegistryCmd() *cobra.Command {
	var namespace, appNamespace, kubeconfig string
	parent := &cobra.Command{
		Use:   "registry",
		Short: "Configure credentials for a private image registry (login/logout/list)",
	}
	parent.PersistentFlags().StringVar(&namespace, "namespace", connect.DefaultNamespace, "control-plane namespace Burrow is installed in")
	parent.PersistentFlags().StringVar(&appNamespace, "app-namespace", "", "namespace apps deploy into (default: discovered from the install)")
	parent.PersistentFlags().StringVar(&kubeconfig, "kubeconfig", "", "path to kubeconfig (default: ambient)")

	// resolve builds a clientset and determines the app namespace (from the flag or the
	// install) for whichever subcommand runs.
	resolve := func(ctx context.Context) (kubernetes.Interface, string, error) {
		cs, err := registryClientset(kubeconfig)
		if err != nil {
			return nil, "", err
		}
		appNS := appNamespace
		if appNS == "" {
			appNS, err = appNamespaceOf(ctx, cs, namespace)
			if err != nil {
				return nil, "", err
			}
		}
		return cs, appNS, nil
	}

	var username, password string
	login := &cobra.Command{
		Use:   "login <host>",
		Short: "Store a credential for a private registry",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			host := args[0]
			cs, appNS, err := resolve(ctx)
			if err != nil {
				return err
			}
			if username == "" || password == "" {
				return errors.New("a username and password/token are required (use -u/-p)")
			}
			if err := registryLogin(ctx, cs, appNS, host, username, password); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "configured registry %q for your apps\n", host)
			return nil
		},
	}
	login.Flags().StringVarP(&username, "username", "u", "", "registry username")
	login.Flags().StringVarP(&password, "password", "p", "", "registry password or token")

	logout := &cobra.Command{
		Use:   "logout <host>",
		Short: "Remove a registry's stored credential",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			cs, appNS, err := resolve(ctx)
			if err != nil {
				return err
			}
			if err := registryLogout(ctx, cs, appNS, args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed registry %q\n", args[0])
			return nil
		},
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "List configured registries",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			cs, appNS, err := resolve(ctx)
			if err != nil {
				return err
			}
			hosts, err := registryList(ctx, cs, appNS)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(hosts) == 0 {
				fmt.Fprintln(out, "no image registries configured")
				return nil
			}
			for _, h := range hosts {
				fmt.Fprintln(out, h)
			}
			return nil
		},
	}

	parent.AddCommand(login, logout, list)
	return parent
}

// registryLogin upserts the host's credential into the burrow-registry Secret and ensures the
// app namespace's default ServiceAccount references it, so app Pods pull with it.
func registryLogin(ctx context.Context, cs kubernetes.Interface, namespace, host, username, password string) error {
	secrets := cs.CoreV1().Secrets(namespace)
	cfg := dockerConfig{Auths: map[string]dockerAuth{}}

	existing, err := secrets.Get(ctx, registrySecretName, metav1.GetOptions{})
	create := apierrors.IsNotFound(err)
	switch {
	case create:
	case err != nil:
		return fmt.Errorf("reading registry secret: %w", err)
	default:
		if raw, ok := existing.Data[corev1.DockerConfigJsonKey]; ok && len(raw) > 0 {
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return fmt.Errorf("parsing existing registry secret: %w", err)
			}
			if cfg.Auths == nil {
				cfg.Auths = map[string]dockerAuth{}
			}
		}
	}

	cfg.Auths[host] = dockerAuth{
		Username: username,
		Password: password,
		Auth:     base64.StdEncoding.EncodeToString([]byte(username + ":" + password)),
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encoding registry credentials: %w", err)
	}

	if create {
		_, err = secrets.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: registrySecretName, Namespace: namespace},
			Type:       corev1.SecretTypeDockerConfigJson,
			Data:       map[string][]byte{corev1.DockerConfigJsonKey: raw},
		}, metav1.CreateOptions{})
	} else {
		existing.Type = corev1.SecretTypeDockerConfigJson
		if existing.Data == nil {
			existing.Data = map[string][]byte{}
		}
		existing.Data[corev1.DockerConfigJsonKey] = raw
		_, err = secrets.Update(ctx, existing, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("writing registry secret: %w", err)
	}
	return setPullSecretOnDefaultSA(ctx, cs, namespace, true)
}

// registryLogout removes one host's credential. When it was the last one, the Secret is
// deleted and detached from the default ServiceAccount.
func registryLogout(ctx context.Context, cs kubernetes.Interface, namespace, host string) error {
	secrets := cs.CoreV1().Secrets(namespace)
	existing, err := secrets.Get(ctx, registrySecretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("no registries are configured in namespace %q", namespace)
	}
	if err != nil {
		return fmt.Errorf("reading registry secret: %w", err)
	}

	var cfg dockerConfig
	if raw, ok := existing.Data[corev1.DockerConfigJsonKey]; ok && len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return fmt.Errorf("parsing registry secret: %w", err)
		}
	}
	if _, ok := cfg.Auths[host]; !ok {
		return fmt.Errorf("registry %q is not configured in namespace %q", host, namespace)
	}
	delete(cfg.Auths, host)

	if len(cfg.Auths) == 0 {
		if err := secrets.Delete(ctx, registrySecretName, metav1.DeleteOptions{}); err != nil {
			return fmt.Errorf("deleting registry secret: %w", err)
		}
		return setPullSecretOnDefaultSA(ctx, cs, namespace, false)
	}

	raw, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encoding registry credentials: %w", err)
	}
	existing.Data[corev1.DockerConfigJsonKey] = raw
	if _, err := secrets.Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("writing registry secret: %w", err)
	}
	return nil
}

// registryList returns the configured registry hosts, sorted.
func registryList(ctx context.Context, cs kubernetes.Interface, namespace string) ([]string, error) {
	s, err := cs.CoreV1().Secrets(namespace).Get(ctx, registrySecretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading registry secret: %w", err)
	}
	var cfg dockerConfig
	if raw, ok := s.Data[corev1.DockerConfigJsonKey]; ok && len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("parsing registry secret: %w", err)
		}
	}
	hosts := make([]string, 0, len(cfg.Auths))
	for h := range cfg.Auths {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	return hosts, nil
}

// setPullSecretOnDefaultSA adds (present=true) or removes (present=false) the burrow-registry
// pull secret from the namespace's default ServiceAccount, so app Pods inherit (or stop
// inheriting) it. It is idempotent.
func setPullSecretOnDefaultSA(ctx context.Context, cs kubernetes.Interface, namespace string, present bool) error {
	sas := cs.CoreV1().ServiceAccounts(namespace)
	sa, err := sas.Get(ctx, "default", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading the default service account in %s: %w", namespace, err)
	}
	has := -1
	for i, ref := range sa.ImagePullSecrets {
		if ref.Name == registrySecretName {
			has = i
			break
		}
	}
	switch {
	case present && has >= 0:
		return nil
	case present:
		sa.ImagePullSecrets = append(sa.ImagePullSecrets, corev1.LocalObjectReference{Name: registrySecretName})
	case !present && has < 0:
		return nil
	default:
		sa.ImagePullSecrets = append(sa.ImagePullSecrets[:has], sa.ImagePullSecrets[has+1:]...)
	}
	if _, err := sas.Update(ctx, sa, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("updating the default service account in %s: %w", namespace, err)
	}
	return nil
}
