// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/burrow-cloud/burrow/client"
)

// credentialsSecretName is the single Secret in the control-plane namespace that holds every
// vendor token, one key per provider (ADR-0023). `burrow install` creates it empty; `provider
// add` upserts a key into it with the developer's kubeconfig, and burrowd reads it via a
// resourceNames-scoped get. The token never travels over MCP and burrowd never writes it.
const credentialsSecretName = "burrow-credentials"

// providerCatalog is the provider types this CLI knows about and the capabilities each serves,
// mirroring controlplane's known provider types — the reference behind `provider types` and the
// add command's help. The control plane validates the type authoritatively on `provider add`
// (its error names the supported types), so this is only a hint and never rejects a type itself.
var providerCatalog = []struct {
	Type         string
	Capabilities []string
}{
	{"cloudflare", []string{"dns"}},
	{"digitalocean", []string{"dns"}},
}

func supportedProviderTypes() []string {
	out := make([]string, len(providerCatalog))
	for i, p := range providerCatalog {
		out[i] = p.Type
	}
	return out
}

func providerTypesHint() string { return strings.Join(supportedProviderTypes(), ", ") }

// newProviderCmd manages cloud-provider credentials. `provider add` is a setup command: it
// writes the token into the burrow-credentials Secret with the developer's kubeconfig and
// then records the (non-secret) registry entry through burrowd — the setup-vs-operation split
// of ADR-0017. burrowd holds and reads the token; the agent never does.
func newProviderCmd() *cobra.Command {
	parent := &cobra.Command{
		Use:   "provider",
		Short: "Configure cloud-provider credentials (add/list)",
		Long: "provider registers the cloud-provider credentials Burrow uses on your behalf —\n" +
			"e.g. a DigitalOcean or Cloudflare API token for DNS. The token is stored in a\n" +
			"Kubernetes Secret in the control-plane namespace and read by the control plane; it\n" +
			"never travels over MCP and the agent never holds it.",
	}
	parent.AddCommand(newProviderTypesCmd(), newProviderAddCmd(), newProviderListCmd())
	return parent
}

// newProviderTypesCmd lists the provider types Burrow supports and the capabilities each
// serves, so a user can see what is available before configuring one. It needs no cluster.
func newProviderTypesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "types",
		Short: "List the available provider types and what each supports",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%-16s%s\n", "TYPE", "SUPPORTS")
			for _, p := range providerCatalog {
				fmt.Fprintf(out, "%-16s%s\n", p.Type, strings.Join(p.Capabilities, ", "))
			}
			return nil
		},
	}
}

func newProviderAddCmd() *cobra.Command {
	o := &commonOpts{}
	var name, secretKey string
	cmd := &cobra.Command{
		Use:   "add <type>",
		Short: "Register a provider credential (type: " + providerTypesHint() + ")",
		Long: "add registers a provider of the given type and stores its API token. You are\n" +
			"prompted for the token with the input hidden, so it never lands in your shell\n" +
			"history or the process table; for scripts, pipe it in instead\n" +
			"(echo \"$TOKEN\" | burrow provider add cloudflare). The token is written into the\n" +
			"burrow-credentials Secret with your kubeconfig and recorded in the control-plane\n" +
			"registry. Pass --name to register more than one provider of the same type.\n\n" +
			"Supported types: " + providerTypesHint() + " (see `burrow provider types`).",
		Example: "  burrow provider add cloudflare\n" +
			"  burrow provider add digitalocean --name do-dns",
		ValidArgs: supportedProviderTypes(),
		Args:      exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			providerType := args[0]
			token, err := readToken(cmd.InOrStdin(), cmd.OutOrStdout(), fmt.Sprintf("Enter the %s API token: ", providerType))
			if err != nil {
				return err
			}
			if token == "" {
				return errors.New("no token provided")
			}

			providerName := name
			if providerName == "" {
				providerName = providerType
			}
			key := secretKey
			if key == "" {
				key = providerName
			}

			// Store the token first so burrowd can verify it: provider add writes the token
			// into burrow-credentials with the developer's kubeconfig (ADR-0017), then asks
			// burrowd to validate it against the vendor and record the registry entry. burrowd
			// reads the token from the Secret — it never crosses the API. If validation fails,
			// roll the Secret back so a rejected token is not left behind.
			cs, err := clientset(o.kubeconfig)
			if err != nil {
				return err
			}
			prior, existed, err := readCredential(ctx, cs, o.namespace, key)
			if err != nil {
				return err
			}
			if err := writeCredential(ctx, cs, o.namespace, key, token); err != nil {
				return err
			}
			c, err := o.client(ctx)
			if err != nil {
				restoreCredential(ctx, cs, o.namespace, key, prior, existed)
				return err
			}
			p, err := c.AddProvider(ctx, client.AddProviderRequest{Name: providerName, Type: providerType, SecretKey: key})
			if err != nil {
				restoreCredential(ctx, cs, o.namespace, key, prior, existed)
				return err
			}

			human := fmt.Sprintf("registered provider %q (type %s, capabilities %s)\n"+
				"token stored in %s/%s under key %q",
				p.Name, p.Type, strings.Join(p.Capabilities, ", "), o.namespace, credentialsSecretName, p.SecretKey)
			return emit(cmd.OutOrStdout(), o.json, p, human)
		},
	}
	bindCommon(cmd.Flags(), o)
	cmd.Flags().StringVar(&name, "name", "", "name for this provider (default: the type)")
	cmd.Flags().StringVar(&secretKey, "secret-key", "", "key in the burrow-credentials Secret to store the token under (default: the name)")
	return cmd
}

func newProviderListCmd() *cobra.Command {
	o := &commonOpts{}
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List configured providers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			providers, err := c.Providers(ctx)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if o.json {
				return emit(out, true, providers, "")
			}
			if len(providers) == 0 {
				fmt.Fprintf(out, "No providers configured. Add one with `burrow provider add <type>`.\n"+
					"Supported types: %s (see `burrow provider types`).\n", providerTypesHint())
				return nil
			}
			fmt.Fprintf(out, "%-16s%-14s%s\n", "NAME", "TYPE", "CAPABILITIES")
			for _, p := range providers {
				fmt.Fprintf(out, "%-16s%-14s%s\n", p.Name, p.Type, strings.Join(p.Capabilities, ","))
			}
			return nil
		},
	}
	bindCommon(cmd.Flags(), o)
	return cmd
}

// readToken reads a secret token. When in is an interactive terminal it prints prompt and reads
// the token without echoing it (so it never shows on screen or in shell history); when in is
// piped or redirected (a script, `echo "$TOKEN" | …`) it reads the token from there instead.
// Surrounding whitespace is trimmed.
func readToken(in io.Reader, out io.Writer, prompt string) (string, error) {
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(out, prompt)
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(out) // terminate the line the hidden input was typed on
		if err != nil {
			return "", fmt.Errorf("reading token: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	b, err := io.ReadAll(in)
	if err != nil {
		return "", fmt.Errorf("reading token from standard input: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// writeCredential upserts the token under key in the burrow-credentials Secret in the
// control-plane namespace, creating the Secret if `burrow install` has not yet (so the
// command works against an older install). It acts with the developer's kubeconfig; burrowd
// only ever reads this Secret (ADR-0023).
func writeCredential(ctx context.Context, cs kubernetes.Interface, namespace, key, token string) error {
	secrets := cs.CoreV1().Secrets(namespace)
	existing, err := secrets.Get(ctx, credentialsSecretName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		_, err = secrets.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: credentialsSecretName, Namespace: namespace},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{key: []byte(token)},
		}, metav1.CreateOptions{})
	case err != nil:
		return fmt.Errorf("reading credentials secret: %w", err)
	default:
		if existing.Data == nil {
			existing.Data = map[string][]byte{}
		}
		existing.Data[key] = []byte(token)
		_, err = secrets.Update(ctx, existing, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("writing credentials secret: %w", err)
	}
	return nil
}

// readCredential returns the token currently stored under key and whether it was present, so a
// failed `provider add` can roll the Secret back to exactly its prior state. A missing Secret
// reads as absent, not an error.
func readCredential(ctx context.Context, cs kubernetes.Interface, namespace, key string) (string, bool, error) {
	s, err := cs.CoreV1().Secrets(namespace).Get(ctx, credentialsSecretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading credentials secret: %w", err)
	}
	v, ok := s.Data[key]
	return string(v), ok, nil
}

// restoreCredential rolls key back to prior (or removes it if it was absent), best effort —
// the caller is already returning the original failure, so a cleanup error must not mask it.
func restoreCredential(ctx context.Context, cs kubernetes.Interface, namespace, key, prior string, existed bool) {
	if existed {
		_ = writeCredential(ctx, cs, namespace, key, prior)
		return
	}
	_ = deleteCredential(ctx, cs, namespace, key)
}

// deleteCredential removes one key from the burrow-credentials Secret, leaving any other
// providers' tokens in place.
func deleteCredential(ctx context.Context, cs kubernetes.Interface, namespace, key string) error {
	secrets := cs.CoreV1().Secrets(namespace)
	s, err := secrets.Get(ctx, credentialsSecretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading credentials secret: %w", err)
	}
	if _, ok := s.Data[key]; !ok {
		return nil
	}
	delete(s.Data, key)
	if _, err := secrets.Update(ctx, s, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("updating credentials secret: %w", err)
	}
	return nil
}
