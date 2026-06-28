// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"fmt"
	"sort"

	"github.com/spf13/cobra"
)

// newEnvCmd groups an app's non-secret environment configuration (ADR-0028). The store is the
// single source of truth for an app's env, managed independently of deploy: set/unset mutate it
// and (by default) roll the running workload so the change takes effect; list prints it back.
func newEnvCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "env",
		Short: "Manage an app's non-secret environment configuration",
		Long: "env manages an app's non-secret environment configuration — the single source of\n" +
			"truth for the app's env, sourced into the workload at deploy time. Setting or\n" +
			"unsetting a variable rolls the running app so it picks the change up; pass\n" +
			"--no-restart to defer the change to the next deploy. For secrets, use a Secret, not\n" +
			"env (env values are non-secret config).",
	}
	cmd.AddCommand(newEnvSetCmd(), newEnvListCmd(), newEnvUnsetCmd())
	return cmd
}

func newEnvSetCmd() *cobra.Command {
	o := &commonOpts{}
	var noRestart bool
	cmd := &cobra.Command{
		Use:   "set <app> KEY=VALUE",
		Short: "Set (upsert) an environment variable for an app",
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
				if err := c.SetEnv(ctx, app, k, v, noRestart); err != nil {
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
	cmd.Flags().BoolVar(&noRestart, "no-restart", false, "persist the change without rolling the running workload; it lands on the next deploy")
	return cmd
}

func newEnvUnsetCmd() *cobra.Command {
	o := &commonOpts{}
	var noRestart bool
	cmd := &cobra.Command{
		Use:   "unset <app> KEY",
		Short: "Remove an environment variable from an app",
		Args:  exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			app, key := args[0], args[1]
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			if err := c.UnsetEnv(ctx, app, key, noRestart); err != nil {
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
	cmd.Flags().BoolVar(&noRestart, "no-restart", false, "persist the change without rolling the running workload; it lands on the next deploy")
	return cmd
}

func newEnvListCmd() *cobra.Command {
	o := &commonOpts{}
	cmd := &cobra.Command{
		Use:   "list <app>",
		Short: "List an app's environment variables",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			env, err := c.Env(ctx, args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if o.json {
				return emit(out, true, env, "")
			}
			keys := make([]string, 0, len(env))
			for k := range env {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Fprintf(out, "%s=%s\n", k, env[k])
			}
			return nil
		},
	}
	bindCommon(cmd.Flags(), o)
	return cmd
}
