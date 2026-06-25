// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

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

// dockerConfig is the on-disk/in-Secret shape of a dockerconfigjson credential file.
type dockerConfig struct {
	Auths map[string]dockerAuth `json:"auths"`
}

type dockerAuth struct {
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Auth     string `json:"auth,omitempty"`
}

// cmdRegistry manages the cluster's registry pull credentials. It is a setup command: it acts
// with the developer's kubeconfig to provision a Kubernetes pull Secret, distinct from the
// agent-driven operations that flow through burrowd (ADR-0017). The credential never travels
// over MCP and burrowd never handles it.
func cmdRegistry(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: burrow registry <login|logout|list> [flags]")
	}
	sub, rest := args[0], args[1:]

	fs := flag.NewFlagSet("registry "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	namespace := fs.String("namespace", connect.DefaultNamespace, "control-plane namespace Burrow is installed in")
	appNamespace := fs.String("app-namespace", "", "namespace apps deploy into (default: discovered from the install)")
	kubeconfig := fs.String("kubeconfig", "", "path to kubeconfig (default: ambient)")
	var username, password string
	var fromDocker bool
	if sub == "login" {
		fs.StringVar(&username, "u", "", "registry username")
		fs.StringVar(&username, "username", "", "registry username")
		fs.StringVar(&password, "p", "", "registry password or token")
		fs.StringVar(&password, "password", "", "registry password or token")
		fs.BoolVar(&fromDocker, "from-docker-config", false, "read the credential for <host> from ~/.docker/config.json")
	}
	pos, flagArgs := splitArgs(rest)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}

	cs, err := clientset(*kubeconfig)
	if err != nil {
		return err
	}
	appNS := *appNamespace
	if appNS == "" {
		appNS, err = appNamespaceOf(ctx, cs, *namespace)
		if err != nil {
			return err
		}
	}

	switch sub {
	case "login":
		host := arg(pos, 0)
		if host == "" {
			return errors.New("usage: burrow registry login <host> [-u user -p token | --from-docker-config]")
		}
		if fromDocker {
			username, password, err = dockerConfigAuth(host)
			if err != nil {
				return err
			}
		}
		if username == "" || password == "" {
			return errors.New("a username and password/token are required (use -u/-p or --from-docker-config)")
		}
		if err := registryLogin(ctx, cs, appNS, host, username, password); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "configured registry %q for apps in namespace %q\n", host, appNS)
		return nil
	case "logout":
		host := arg(pos, 0)
		if host == "" {
			return errors.New("usage: burrow registry logout <host>")
		}
		if err := registryLogout(ctx, cs, appNS, host); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "removed registry %q from namespace %q\n", host, appNS)
		return nil
	case "list":
		hosts, err := registryList(ctx, cs, appNS)
		if err != nil {
			return err
		}
		if len(hosts) == 0 {
			fmt.Fprintf(stdout, "no registries configured in namespace %q\n", appNS)
			return nil
		}
		for _, h := range hosts {
			fmt.Fprintln(stdout, h)
		}
		return nil
	default:
		return fmt.Errorf("unknown registry subcommand %q (want login, logout, or list)", sub)
	}
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

// dockerConfigAuth reads the credential for host from the developer's Docker config, so
// `--from-docker-config` can reuse a `docker login` the user already did.
func dockerConfigAuth(host string) (username, password string, err error) {
	path := os.Getenv("DOCKER_CONFIG")
	if path != "" {
		path = filepath.Join(path, "config.json")
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "", fmt.Errorf("locating docker config: %w", err)
		}
		path = filepath.Join(home, ".docker", "config.json")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("reading docker config %s: %w", path, err)
	}
	var cfg dockerConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", "", fmt.Errorf("parsing docker config %s: %w", path, err)
	}
	a, ok := cfg.Auths[host]
	if !ok {
		return "", "", fmt.Errorf("no credentials for %q in %s; run `docker login %s` first", host, path, host)
	}
	if a.Username != "" && a.Password != "" {
		return a.Username, a.Password, nil
	}
	if a.Auth != "" {
		decoded, err := base64.StdEncoding.DecodeString(a.Auth)
		if err != nil {
			return "", "", fmt.Errorf("decoding docker auth for %q: %w", host, err)
		}
		if i := strings.IndexByte(string(decoded), ':'); i >= 0 {
			return string(decoded)[:i], string(decoded)[i+1:], nil
		}
	}
	return "", "", fmt.Errorf("docker config has no usable credential for %q", host)
}
