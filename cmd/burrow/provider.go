// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

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
	{"github", []string{"source"}},
	{"gitlab", []string{"source"}},
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
		Short: "Configure provider credentials (add/list)",
		Long: "provider registers the provider credentials Burrow uses on your behalf — a\n" +
			"DigitalOcean or Cloudflare API token for DNS, or a GitHub/GitLab token whose one\n" +
			"value the in-cluster build uses to clone a PRIVATE git source AND to authenticate\n" +
			"the provider's image registry. The token travels over burrowd's\n" +
			"authenticated control-plane API (TLS), which stores it in a Kubernetes Secret in the\n" +
			"control-plane namespace; it never travels over MCP and the agent never holds it.",
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
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "TYPE\tSUPPORTS")
			for _, p := range providerCatalog {
				fmt.Fprintf(tw, "%s\t%s\n", p.Type, strings.Join(p.Capabilities, ", "))
			}
			return tw.Flush()
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
			"burrowd's authenticated control-plane API (TLS), which writes it into the\n" +
			"burrow-credentials Secret; a DNS provider's token is validated against the vendor\n" +
			"first. It never travels over MCP and is never logged. For a private build source use\n" +
			"a github or gitlab token — one token clones the private repo and authenticates its\n" +
			"registry; a fine-grained token scoped to the repos you build (plus\n" +
			"read:packages where the registry is shared) keeps the blast radius small. Pass --name\n" +
			"to register more than one provider of the same type.\n\n" +
			"Supported types: " + providerTypesHint() + " (see `burrow config provider types`).",
		Example: "  burrow config provider add cloudflare\n" +
			"  burrow config provider add digitalocean --name do-dns\n" +
			"  echo \"$GH_PAT\" | burrow config provider add github",
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
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tTYPE\tCAPABILITIES")
			for _, p := range providers {
				fmt.Fprintf(tw, "%s\t%s\t%s\n", p.Name, p.Type, strings.Join(p.Capabilities, ","))
			}
			return tw.Flush()
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
