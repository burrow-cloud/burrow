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
			"the workload at runtime from a per-app Kubernetes Secret. `secret list` shows only the\n" +
			"KEYS, never a value. See `burrow app secret set --help` for how a value is supplied.",
	}
	cmd.AddCommand(newSecretSetCmd(), newSecretListCmd(), newSecretUnsetCmd())
	return cmd
}

// newSecretSetCmd sends a secret value to burrowd over the authenticated control-plane API, which
// writes it into the per-app Secret (ADR-0029). The value never crosses the agent control channel,
// is never logged, and
// is never stored in the database (ADR-0004). By default burrowd rolls the running app so it picks
// the new value up (envFrom is read only at pod start); --no-restart defers that to the next deploy.
func newSecretSetCmd() *cobra.Command {
	o := &commonOpts{}
	var noRestart, stdin bool
	cmd := &cobra.Command{
		Use:   "set <app> KEY=VALUE",
		Short: "Set (upsert) a secret environment variable for an app",
		Long: "set sends a secret value to burrowd over the authenticated control-plane API (TLS),\n" +
			"and burrowd writes it into the app's per-app Kubernetes Secret.\n\n" +
			"--stdin takes the KEY alone and reads the value from standard input: hidden input at a\n" +
			"terminal, the pipe in a script. Prefer it. A value typed as an argument lands in your\n" +
			"shell history and in the process table, where it stays long after the command is done,\n" +
			"and a multi-line value (a PEM key) can only be supplied this way.\n\n" +
			"NEVER paste a secret value into an agent prompt — it is retained in the conversation\n" +
			"and re-sent on every later tool call. Run this command yourself at your terminal; the\n" +
			"agent can confirm the key landed with `burrow app secret list <app>`.\n\n" +
			"By default the running app is rolled so it picks the value up; pass --no-restart to\n" +
			"defer it to the next deploy.",
		Example: "  burrow app secret set web STRIPE_SECRET_KEY --stdin\n" +
			"  cat key.pem | burrow app secret set web GITHUB_APP_KEY --stdin\n" +
			"  burrow app secret set web LOG_LEVEL=debug",
		Args: exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			app := args[0]
			key, value, err := secretKeyValue(cmd, args[1], stdin)
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
	cmd.Flags().BoolVar(&stdin, "stdin", false, "read the value from standard input (hidden at a terminal), taking the argument as the KEY alone")
	return cmd
}

// secretKeyValue resolves the KEY and its value from the second positional argument.
//
// Without --stdin the argument is KEY=VALUE, split on the FIRST `=` so a value that contains one —
// a DSN, a base64 payload — survives intact. With --stdin the argument is the KEY and the value is
// read from standard input, which is the form that keeps the value out of shell history and out of
// the process table where any other user on the machine can read it (issue #425). Reading is
// delegated to readToken, the same helper `registry login` and `provider add` use, so a value is
// supplied one way across the CLI: hidden input at a terminal, the pipe in a script.
//
// Neither form ever blocks on a read the caller did not ask for: without --stdin nothing is read at
// all, so a non-interactive invocation that forgot the value fails on the argument instead of
// hanging.
func secretKeyValue(cmd *cobra.Command, arg string, stdin bool) (key, value string, err error) {
	if !stdin {
		k, v, ok := strings.Cut(arg, "=")
		if !ok || k == "" {
			return "", "", fmt.Errorf("expected KEY=VALUE, got %q; or pass the KEY alone with --stdin "+
				"and keep the value out of your shell history", arg)
		}
		return k, v, nil
	}
	if strings.Contains(arg, "=") {
		return "", "", fmt.Errorf("with --stdin the argument is the KEY alone (got %q); the value is read from standard input", arg)
	}
	value, err = readToken(cmd.InOrStdin(), cmd.OutOrStdout(), fmt.Sprintf("Value for %s: ", arg))
	if err != nil {
		return "", "", err
	}
	if value == "" {
		return "", "", fmt.Errorf("no value for %s arrived on standard input", arg)
	}
	return arg, value, nil
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
				fmt.Fprintf(out, "No secrets set for %s. Set one with `burrow app secret set %s KEY --stdin`.\n", args[0], args[0])
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
