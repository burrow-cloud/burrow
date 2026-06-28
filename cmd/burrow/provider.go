// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/burrow-cloud/burrow/client"
)

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

// newProviderCmd manages cloud-provider credentials. `provider add` is a setup command: the token
// travels over burrowd's authenticated control-plane API (TLS), which validates it and writes it
// into the burrow-credentials Secret (ADR-0030). burrowd holds the token; it never travels over MCP
// and the agent never holds it.
func newProviderCmd() *cobra.Command {
	parent := &cobra.Command{
		Use:   "provider",
		Short: "Configure cloud-provider credentials (add/list)",
		Long: "provider registers the cloud-provider credentials Burrow uses on your behalf —\n" +
			"e.g. a DigitalOcean or Cloudflare API token for DNS. The token travels over burrowd's\n" +
			"authenticated control-plane API (TLS), which validates it and stores it in a Kubernetes\n" +
			"Secret in the control-plane namespace; it never travels over MCP and the agent never\n" +
			"holds it.",
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
			"(echo \"$TOKEN\" | burrow config provider add cloudflare). The token travels over\n" +
			"burrowd's authenticated control-plane API (TLS), which validates it against the vendor\n" +
			"and writes it into the burrow-credentials Secret (ADR-0030); it never travels over MCP\n" +
			"and is never logged. Pass --name to register more than one provider of the same type.\n\n" +
			"Supported types: " + providerTypesHint() + " (see `burrow config provider types`).",
		Example: "  burrow config provider add cloudflare\n" +
			"  burrow config provider add digitalocean --name do-dns",
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

			// Send the token to burrowd over its authenticated control-plane API (TLS). burrowd
			// validates it against the vendor, writes it into the burrow-credentials Secret, and
			// records the registry entry — a rejected token writes nothing (ADR-0030). The token
			// travels only in the request body; it never crosses MCP and is never logged.
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			p, err := c.AddProvider(ctx, client.AddProviderRequest{Name: providerName, Type: providerType, SecretKey: key, Token: token})
			if err != nil {
				return err
			}

			human := fmt.Sprintf("registered provider %q (type %s, capabilities %s)\n"+
				"token stored in burrow-credentials under key %q",
				p.Name, p.Type, strings.Join(p.Capabilities, ", "), p.SecretKey)
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
				fmt.Fprintf(out, "No providers configured. Add one with `burrow config provider add <type>`.\n"+
					"Supported types: %s (see `burrow config provider types`).\n", providerTypesHint())
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
