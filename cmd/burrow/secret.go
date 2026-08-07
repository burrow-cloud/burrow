// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// newSecretCmd groups an app's secret environment configuration (ADR-0028, ADR-0029). Secret
// values live only in a per-app Kubernetes Secret in the app namespace, sourced into the workload
// at runtime; they are never inlined into the Deployment, written to the control plane's database,
// or carried over the agent control channel (ADR-0004). The whole group goes through burrowd's
// authenticated
// control-plane API:
//
//   - `secret set` carries a VALUE. The value travels over the authenticated, TLS-protected
//     control-plane API to burrowd, which writes it to the per-app Secret (ADR-0029). It is never
//     logged, never stored in the database, and still never crosses the agent control channel —
//     `burrow-agent` carries no secret-set command, so the agent cannot set a value.
//   - `secret list` (KEYS only) and `secret unset` (by KEY) carry no value, so they are on the
//     agent surface as `burrow-agent secret` and `burrow-agent secret unset`.
func newSecretCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Manage an app's secret environment configuration",
		Long: "secret manages an app's secret environment — database URLs, API keys — sourced into\n" +
			"the workload at runtime from a per-app Kubernetes Secret. `secret list` shows the KEYS,\n" +
			"never a value, and a value is never typed as a command argument: `secret set` prompts\n" +
			"for it, or reads it from a pipe.",
	}
	cmd.AddCommand(newSecretSetCmd(), newSecretListCmd(), newSecretUnsetCmd())
	return cmd
}

// newSecretSetCmd sends a secret value to burrowd over the authenticated control-plane API, which
// writes it into the per-app Secret (ADR-0029). The value never crosses the agent control channel,
// is never logged, and is never stored in the database (ADR-0004). It is also never an ARGUMENT: it
// is typed at a hidden prompt or piped in (readSecretValue). By default burrowd rolls the running
// app so it picks the new value up (envFrom is read only at pod start); --no-restart defers that to
// the next deploy.
func newSecretSetCmd() *cobra.Command {
	o := &commonOpts{}
	var noRestart, stdin bool
	cmd := &cobra.Command{
		Use:   "set <app> KEY",
		Short: "Set (upsert) a secret environment variable for an app",
		Long: "set stores one secret value for an app. The value is never an argument: type it at the\n" +
			"hidden prompt, or pipe it in with --stdin for a script. Anything in argv lands in your\n" +
			"shell history and in the process table, where it stays long after the command is done.\n\n" +
			"NEVER paste a secret value into an agent prompt — it is retained in the conversation and\n" +
			"re-sent on every later tool call. Run this yourself; an agent can confirm the key landed\n" +
			"with `burrow app secret list <app>`.\n\n" +
			"The value goes to burrowd over the authenticated control-plane API (TLS), which writes it\n" +
			"into the app's per-app Kubernetes Secret. It is never logged and never stored in the\n" +
			"database.",
		Example: "  burrow app secret set web STRIPE_SECRET_KEY\n" +
			"  printf '%s' \"$STRIPE_SECRET_KEY\" | burrow app secret set web STRIPE_SECRET_KEY --stdin\n" +
			"  burrow app secret set web GITHUB_APP_KEY --stdin < key.pem",
		Args: exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			app, key := args[0], args[1]
			value, err := readSecretValue(cmd, key, stdin)
			if err != nil {
				return err
			}
			c, env, err := o.resolveAndConnect(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			if err := c.SetSecret(ctx, app, env, key, value, noRestart); err != nil {
				return err
			}
			human := fmt.Sprintf("set secret %s on %s", key, app)
			if noRestart {
				human += " (not restarted; lands on next deploy)"
			}
			return o.emitChange(cmd.OutOrStdout(), map[string]string{"app": app, "key": key}, human)
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().BoolVar(&noRestart, "no-restart", false, "persist the value without rolling the running workload; it lands on the next deploy")
	cmd.Flags().BoolVar(&stdin, "stdin", false, "read the value from a pipe or a redirect instead of prompting for it (for scripts and CI)")
	return cmd
}

// readSecretValue reads the secret value for key. There is only one positional argument after the
// app — the KEY — because the value has no argv form at all (issue #425).
//
// argv is the wrong channel for a secret and there is no way to make it a right one: an argument is
// written to the shell's history file, which almost nobody audits, and it is visible in `ps` to
// every other user on the machine for as long as the process runs. `KEY=VALUE` also had to guess
// where the key ended, and a DSN or a base64 payload contains `=` routinely. Both problems go away
// when the value is never an argument.
//
// So it is read instead, through readToken — the helper `registry login` and `provider add` already
// use, so a secret is supplied one way across the whole CLI: hidden input at a terminal, the pipe in
// a script. That is also the only form a multi-line credential (a PEM private key) can take.
//
// The one thing it must never do is block on a read nobody asked for. A terminal is prompted at; a
// non-interactive invocation reads standard input only when --stdin says so, and otherwise fails
// saying which flag to pass. So a CI job that forgot the value ends with a message rather than a
// hung build.
func readSecretValue(cmd *cobra.Command, key string, stdin bool) (string, error) {
	// The KEY half is echoed back and the value half deliberately is not. Repeating it would copy the
	// thing this refusal exists to contain into stderr, and from there into a CI log, which is the
	// same mistake in a different file.
	if k, _, ok := strings.Cut(key, "="); ok {
		return "", fmt.Errorf("a secret value is not passed as an argument: it would land in your shell "+
			"history and in the process table. Run `burrow app secret set <app> %s` and type the value at "+
			"the prompt, or pipe it in with --stdin", k)
	}
	if !stdin && !stdinIsTerminal(cmd.InOrStdin()) {
		return "", fmt.Errorf("no terminal to prompt for the value of %s: pipe it in with --stdin "+
			"(printf '%%s' \"$VALUE\" | burrow app secret set <app> %s --stdin)", key, key)
	}
	value, err := readToken(cmd.InOrStdin(), cmd.OutOrStdout(), fmt.Sprintf("Value for %s: ", key))
	if err != nil {
		return "", err
	}
	if value == "" {
		return "", fmt.Errorf("no value supplied for %s", key)
	}
	return value, nil
}

func newSecretListCmd() *cobra.Command {
	o := &commonOpts{}
	cmd := &cobra.Command{
		Use:   "list <app>",
		Short: "List an app's secret keys (never the values)",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, env, err := o.resolveAndConnectRead(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			keys, err := c.Secrets(ctx, args[0], env)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if o.json {
				return emit(out, true, map[string][]string{"keys": keys}, "")
			}
			if len(keys) == 0 {
				fmt.Fprintf(out, "No secrets set for %s. Set one with `burrow app secret set %s KEY`.\n", args[0], args[0])
				return nil
			}
			for _, k := range keys {
				fmt.Fprintln(out, k)
			}
			return nil
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
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
			c, env, err := o.resolveAndConnect(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			if err := c.UnsetSecret(ctx, app, env, key, noRestart); err != nil {
				return err
			}
			human := fmt.Sprintf("unset secret %s on %s", key, app)
			if noRestart {
				human += " (not restarted; lands on next deploy)"
			}
			return o.emitChange(cmd.OutOrStdout(), map[string]string{"app": app, "key": key}, human)
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().BoolVar(&noRestart, "no-restart", false, "persist the removal without rolling the running workload; it lands on the next deploy")
	return cmd
}
