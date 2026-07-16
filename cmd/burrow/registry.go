// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
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
		Long: "registry configures the credentials the cluster uses to pull images from an EXTERNAL\n" +
			"private registry (GHCR, Docker Hub, GitLab, ...) — it stores a pull Secret with your\n" +
			"kubeconfig. It does not run a registry; to install and manage the OPTIONAL in-cluster\n" +
			"registry that runs IN your cluster (the zero-config push target for the in-cluster build),\n" +
			"use `burrow cluster registry` instead.",
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
	var passwordStdin bool
	login := &cobra.Command{
		Use:   "login <host>",
		Short: "Store a credential for a private registry (prompts for the token)",
		Long: "login stores the credentials the cluster uses to pull images from a private\n" +
			"registry. Omit the flags and it prompts: the username as normal input, the\n" +
			"password or token with the input hidden, so the token never lands in your shell\n" +
			"history or the process table. For a private GitHub registry use a dedicated,\n" +
			"long-lived Personal Access Token with the read:packages scope. Pass -u to skip\n" +
			"the username prompt; -p supplies the token on the command line (insecure, kept\n" +
			"for non-interactive use); --password-stdin reads the token from standard input\n" +
			"for scripts and CI (echo \"$TOKEN\" | burrow config registry login ghcr.io -u me\n" +
			"--password-stdin).",
		Example: "  burrow config registry login ghcr.io -u me\n" +
			"  echo \"$TOKEN\" | burrow config registry login ghcr.io -u me --password-stdin",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			host := args[0]
			username, password, err := resolveLoginCredentials(cmd, host, username, password, passwordStdin)
			if err != nil {
				return err
			}
			cs, appNS, err := resolve(ctx)
			if err != nil {
				return err
			}
			if err := registryLogin(ctx, cs, appNS, host, username, password); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "configured registry %q for your apps\n", host)
			return nil
		},
	}
	login.Flags().StringVarP(&username, "username", "u", "", "registry username (prompted when omitted)")
	login.Flags().StringVarP(&password, "password", "p", "", "registry password or token on the command line (insecure; prefer the prompt or --password-stdin)")
	login.Flags().BoolVar(&passwordStdin, "password-stdin", false, "read the password or token from standard input")

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

// stdinIsTerminal reports whether the command's standard input is an interactive terminal. It is
// a package var so tests can force the non-interactive path deterministically without a real TTY.
var stdinIsTerminal = func(in io.Reader) bool {
	f, ok := in.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// resolveLoginCredentials determines the username and password/token for a registry login from
// the flags and, when the flags are omitted on an interactive terminal, secure prompts. The
// resolution order is: for the password, --password-stdin, then -p, then a hidden interactive
// prompt, then an error; for the username, -u, then a visible interactive prompt, then an error.
// It follows docker login: -p on a terminal earns a warning because it leaks the token into shell
// history and the process table, and a non-interactive shell with no supplied password is an
// error that names the non-interactive path.
func resolveLoginCredentials(cmd *cobra.Command, host, username, password string, passwordStdin bool) (string, string, error) {
	if passwordStdin && password != "" {
		return "", "", errors.New("--password/-p and --password-stdin are mutually exclusive")
	}
	in := cmd.InOrStdin()
	errOut := cmd.ErrOrStderr()
	interactive := stdinIsTerminal(in)

	// Print the provider-aware "where to get a token" hint once, right before the prompts, and
	// only when a missing credential means we are actually going to prompt for it.
	promptUsername := username == "" && interactive
	promptPassword := !passwordStdin && password == "" && interactive
	if promptUsername || promptPassword {
		fmt.Fprintln(errOut, registryTokenHint(host))
	}

	// Username: -u, else an interactive prompt, else an error.
	if username == "" {
		if !interactive {
			return "", "", errors.New("no username provided: pass -u/--username, or run the command in an interactive terminal to be prompted")
		}
		u, err := readLine(in, errOut, "Username: ")
		if err != nil {
			return "", "", err
		}
		username = u
	}

	// Password: --password-stdin, else -p, else an interactive hidden prompt, else an error.
	switch {
	case passwordStdin:
		b, err := io.ReadAll(in)
		if err != nil {
			return "", "", fmt.Errorf("reading password from standard input: %w", err)
		}
		password = strings.TrimRight(string(b), "\r\n")
	case password != "":
		if interactive {
			fmt.Fprintln(errOut, "warning: using --password/-p on the command line is insecure; prefer the interactive prompt or --password-stdin")
		}
	case interactive:
		p, err := readHidden(in, errOut, "Password (read:packages token): ")
		if err != nil {
			return "", "", err
		}
		password = p
	default:
		return "", "", errors.New("no password provided: pass --password-stdin (echo \"$TOKEN\" | burrow config registry login <host> -u <user> --password-stdin), or run the command in an interactive terminal to be prompted")
	}

	if username == "" {
		return "", "", errors.New("a username is required")
	}
	if password == "" {
		return "", "", errors.New("a password or token is required")
	}
	return username, password, nil
}

// registryTokenHint returns a one-line, provider-aware pointer to where the user creates a pull
// token for the given registry host. Modern terminals linkify a full URL, so the user can click
// straight through to the right page. Unknown hosts get a generic line with no URL. This mapping
// is the single source of truth for provider token guidance; the MCP layer stays generic.
func registryTokenHint(host string) string {
	switch {
	case host == "ghcr.io" || strings.HasSuffix(host, ".ghcr.io"):
		return "Create a read:packages token: https://github.com/settings/tokens/new?scopes=read:packages"
	case host == "registry.gitlab.com":
		return "Create a personal access token with the read_registry scope: https://gitlab.com/-/user_settings/personal_access_tokens"
	case host == "docker.io" || host == "registry-1.docker.io" || host == "index.docker.io":
		return "Create an access token: https://app.docker.com/settings/personal-access-tokens"
	default:
		return "Create an access token with pull (read) access in your registry's account settings."
	}
}

// readLine prints prompt to out and reads one visible line from in, trimming surrounding
// whitespace. It is used for non-secret input such as the registry username.
func readLine(in io.Reader, out io.Writer, prompt string) (string, error) {
	fmt.Fprint(out, prompt)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("reading input: %w", err)
	}
	return strings.TrimSpace(line), nil
}

// readHidden prints prompt to out and reads one line from in without echoing it, so a token never
// shows on screen. It requires in to be a terminal (an *os.File whose fd is a tty); callers gate
// it on stdinIsTerminal. A trailing newline is written after the read because the hidden input
// does not echo the Enter key.
func readHidden(in io.Reader, out io.Writer, prompt string) (string, error) {
	f, ok := in.(*os.File)
	if !ok {
		return "", errors.New("cannot read a hidden password from a non-terminal")
	}
	fmt.Fprint(out, prompt)
	b, err := term.ReadPassword(int(f.Fd()))
	fmt.Fprintln(out) // terminate the line the hidden input was typed on
	if err != nil {
		return "", fmt.Errorf("reading password: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
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
