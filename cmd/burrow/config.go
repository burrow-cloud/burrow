// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"fmt"
	"sort"

	"github.com/spf13/cobra"
)

// newAppConfigCmd groups an app's non-secret config vars (ADR-0028) — configuration set as
// environment variables. The store is the single source of truth for an app's config, managed
// independently of deploy: set/unset mutate it and (by default) roll the running workload so the
// change takes effect; list prints it back.
func newAppConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage an app's config vars (non-secret configuration)",
		Long: "config manages an app's config vars: non-secret configuration set as environment\n" +
			"variables, the single source of truth for the app's config, sourced into the workload\n" +
			"at deploy time. Setting or unsetting a config var rolls the running app so it picks the\n" +
			"change up; pass --no-restart to defer the change to the next deploy. For secrets, use a\n" +
			"Secret, not config (config vars are non-secret).",
	}
	cmd.AddCommand(newAppConfigSetCmd(), newAppConfigListCmd(), newAppConfigUnsetCmd())
	return cmd
}

func newAppConfigSetCmd() *cobra.Command {
	o := &commonOpts{}
	var noRestart bool
	cmd := &cobra.Command{
		Use:   "set <app> KEY=VALUE",
		Short: "Set (upsert) a config var for an app",
		Args:  exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			app := args[0]
			var kv kvFlag
			if err := kv.Set(args[1]); err != nil {
				return err
			}
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			for k, v := range kv.m {
				if err := c.SetConfig(ctx, app, o.env, k, v, noRestart); err != nil {
					return err
				}
				human := fmt.Sprintf("set %s on %s", k, app)
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
	cmd.Flags().BoolVar(&noRestart, "no-restart", false, "persist the change without rolling the running workload; it lands on the next deploy")
	return cmd
}

func newAppConfigUnsetCmd() *cobra.Command {
	o := &commonOpts{}
	var noRestart bool
	cmd := &cobra.Command{
		Use:   "unset <app> KEY",
		Short: "Remove a config var from an app",
		Args:  exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			app, key := args[0], args[1]
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			if err := c.UnsetConfig(ctx, app, o.env, key, noRestart); err != nil {
				return err
			}
			human := fmt.Sprintf("unset %s on %s", key, app)
			if noRestart {
				human += " (not restarted; lands on next deploy)"
			}
			return emit(cmd.OutOrStdout(), o.json, map[string]string{"app": app, "key": key}, human)
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().BoolVar(&noRestart, "no-restart", false, "persist the change without rolling the running workload; it lands on the next deploy")
	return cmd
}

func newAppConfigListCmd() *cobra.Command {
	o := &commonOpts{}
	cmd := &cobra.Command{
		Use:   "list <app>",
		Short: "List an app's config vars",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			cfg, err := c.Config(ctx, args[0], o.env)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if o.json {
				return emit(out, true, cfg, "")
			}
			keys := make([]string, 0, len(cfg))
			for k := range cfg {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Fprintf(out, "%s=%s\n", k, cfg[k])
			}
			return nil
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	return cmd
}
