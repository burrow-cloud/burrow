// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newSecretCmd groups an app's secret environment configuration (ADR-0028, ADR-0029). Secret
// values live only in a per-app Kubernetes Secret in the app namespace, sourced into the workload
// at runtime; they are never inlined into the Deployment, written to the control plane's database,
// or carried over MCP (ADR-0004). The whole group goes through burrowd's authenticated
// control-plane API:
//
//   - `secret set` carries a VALUE. The value travels over the authenticated, TLS-protected
//     control-plane API to burrowd, which writes it to the per-app Secret (ADR-0029). It is never
//     logged, never stored in the database, and still never crosses MCP — there is no secret-set
//     MCP tool, so the agent cannot set a value.
//   - `secret list` (KEYS only) and `secret unset` (by KEY) carry no value and are also MCP tools.
func newSecretCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Manage an app's secret environment configuration",
		Long: "secret manages an app's secret environment — database URLs, API keys — sourced into\n" +
			"the workload at runtime from a per-app Kubernetes Secret. `secret set` sends the value\n" +
			"over burrowd's authenticated control-plane API (TLS), and burrowd writes it to the\n" +
			"Secret; the value is never carried over MCP, never logged, and never written to the\n" +
			"control plane's database (ADR-0029/0004). `secret list` shows only the KEYS.\n\n" +
			"NEVER paste a secret value into an agent prompt — anything in the prompt is retained in\n" +
			"the conversation and re-sent on later tool calls. Run `secret set` yourself; the agent\n" +
			"can confirm the key is present with `secret list`.",
	}
	cmd.AddCommand(newSecretSetCmd(), newSecretListCmd(), newSecretUnsetCmd())
	return cmd
}

// newSecretSetCmd sends a secret value to burrowd over the authenticated control-plane API, which
// writes it into the per-app Secret (ADR-0029). The value never crosses MCP, is never logged, and
// is never stored in the database (ADR-0004). By default burrowd rolls the running app so it picks
// the new value up (envFrom is read only at pod start); --no-restart defers that to the next deploy.
func newSecretSetCmd() *cobra.Command {
	o := &commonOpts{}
	var noRestart bool
	cmd := &cobra.Command{
		Use:   "set <app> KEY=VALUE",
		Short: "Set (upsert) a secret environment variable for an app",
		Long: "set sends a secret value to burrowd over the authenticated control-plane API (TLS),\n" +
			"and burrowd writes it into the app's per-app Kubernetes Secret. The value never travels\n" +
			"over MCP, is never logged, and is never stored in the control plane's database\n" +
			"(ADR-0029/0004).\n\n" +
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
			c, env, err := o.resolveAndConnect(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			for k, v := range kv.m {
				if err := c.SetSecret(ctx, app, env, k, v, noRestart); err != nil {
					return err
				}
				human := fmt.Sprintf("set secret %s on %s", k, app)
				if noRestart {
					human += " (not restarted; lands on next deploy)"
				}
				if err := emit(cmd.OutOrStdout(), o.json, map[string]string{"app": app, "key": k}, human); err != nil {
					return err
				}
			}
			return nil
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
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
			c, env, err := o.resolveAndConnect(ctx, cmd.ErrOrStderr())
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
			return emit(cmd.OutOrStdout(), o.json, map[string]string{"app": app, "key": key}, human)
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().BoolVar(&noRestart, "no-restart", false, "persist the removal without rolling the running workload; it lands on the next deploy")
	return cmd
}
