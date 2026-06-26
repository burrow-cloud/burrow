// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
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
	parent.AddCommand(newProviderAddCmd(), newProviderListCmd())
	return parent
}

func newProviderAddCmd() *cobra.Command {
	o := &commonOpts{}
	var name, secretKey string
	var tokenStdin bool
	cmd := &cobra.Command{
		Use:   "add <type>",
		Short: "Register a provider credential (e.g. digitalocean, cloudflare)",
		Long: "add registers a provider of the given type and stores its API token. The token is\n" +
			"read from standard input (--token-stdin) so it never lands in your shell history or\n" +
			"the process table, written into the burrow-credentials Secret with your kubeconfig,\n" +
			"and recorded in the control-plane registry. Pass --name to register more than one\n" +
			"provider of the same type.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			providerType := args[0]
			if !tokenStdin {
				return errors.New("--token-stdin is required (the API token is read from standard input)")
			}
			token, err := readSecretStdin(cmd.InOrStdin())
			if err != nil {
				return err
			}
			if token == "" {
				return errors.New("no token read from standard input")
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
	cmd.Flags().BoolVar(&tokenStdin, "token-stdin", false, "read the provider API token from standard input")
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
				fmt.Fprintln(out, "no providers configured")
				return nil
			}
			for _, p := range providers {
				fmt.Fprintf(out, "%s\t%s\t%s\n", p.Name, p.Type, strings.Join(p.Capabilities, ","))
			}
			return nil
		},
	}
	bindCommon(cmd.Flags(), o)
	return cmd
}

// readSecretStdin reads a secret value from r and trims surrounding whitespace, so a token
// piped in (with or without a trailing newline) is read cleanly.
func readSecretStdin(r io.Reader) (string, error) {
	b, err := io.ReadAll(r)
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
