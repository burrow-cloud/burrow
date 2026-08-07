// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/burrow-cloud/burrow/client"
	"github.com/burrow-cloud/burrow/controlplane"
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
	cmd.AddCommand(newSecretSetCmd(), newSecretListCmd(), newSecretUnsetCmd(),
		newSecretMountCmd(), newSecretUnmountCmd(), newSecretMountsCmd())
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

// mountLong is the shared orientation for the three mount verbs (ADR-0089). It says what a mount
// changes and, just as importantly, what it does not: the value does not move, and mounting a key
// does not take it out of the environment.
const mountLong = "A mount changes only how a secret is DELIVERED. The value does not move: it stays in the same\n" +
	"per-app Kubernetes Secret, written the same way, and mounting a key does not remove it from the\n" +
	"app's environment.\n\n" +
	"Files land in a directory Burrow owns — /run/secrets by default — and only the keys you mount\n" +
	"are in it. The directory is on the app as BURROW_SECRETS_DIR, never the value, so an app that\n" +
	"takes a path from the environment (GOOGLE_APPLICATION_CREDENTIALS, KUBECONFIG) is pointed at a\n" +
	"file with `burrow app config set`.\n\n" +
	"A mount is app configuration rather than part of a release: it survives a rollback, so rolling\n" +
	"back to a release cut before the mount existed does not take the credential with it."

// newSecretMountCmd projects one secret key into a file (ADR-0089 §1). It names a KEY and never
// carries a value, so it is on the agent surface as `burrow-agent secret mount` (§7).
func newSecretMountCmd() *cobra.Command {
	o := &commonOpts{}
	var filename, dir string
	cmd := &cobra.Command{
		Use:   "mount <app> KEY",
		Short: "Project a secret key into a file the app can read",
		Long: "Project one of an app's secret keys into a file, for a credential that is better read from\n" +
			"disk than from the environment — a kubeconfig, a PEM private key, a service-account JSON.\n" +
			"An environment variable is readable at /proc/<pid>/environ, is inherited by every child\n" +
			"process, and lands in a crash dump; a file is read by whoever opens it.\n\n" +
			"The key must already be set. Mounting one that is not would give you an app that starts,\n" +
			"finds no file, and fails at the moment it needs the credential.\n\n" +
			"--dir moves the directory for the WHOLE APP, and there is deliberately no per-key path:\n" +
			"a per-key path can shadow a file in the app's own image, and it stops Kubernetes updating\n" +
			"the file in place when the value is rotated.\n\n" +
			"This re-applies the running workload, so the app rolls with the file in place.\n\n" + mountLong,
		Example: "  burrow app secret mount web GOOGLE_CREDENTIALS\n" +
			"  burrow app secret mount web TLS_KEY --filename tls.key\n" +
			"  burrow app secret mount api KUBECONFIG --dir /etc/app/secrets",
		Args: exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, env, err := o.resolveAndConnect(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			res, err := c.MountSecret(ctx, args[0], env, args[1], filename, dir)
			if err != nil {
				return err
			}
			return o.emitChange(cmd.OutOrStdout(), res, mountsHuman(args[0], res))
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().StringVar(&filename, "filename", "", "the `name` the key lands under (default: the key itself)")
	cmd.Flags().StringVar(&dir, "dir", "", "the `directory` this app's mounted keys land in (default: /run/secrets) — per app, never per key")
	return cmd
}

// newSecretUnmountCmd stops projecting one key as a file. It removes a FILE, never a value.
func newSecretUnmountCmd() *cobra.Command {
	o := &commonOpts{}
	cmd := &cobra.Command{
		Use:   "unmount <app> KEY",
		Short: "Stop projecting a secret key into a file",
		Long: "Stop projecting a secret key into a file. The value is untouched: the key stays set and\n" +
			"stays in the app's environment, so this cannot lose a credential.\n\n" +
			"This re-applies the running workload, so the app rolls without the file.\n\n" + mountLong,
		Args: exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, env, err := o.resolveAndConnect(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			res, err := c.UnmountSecret(ctx, args[0], env, args[1])
			if err != nil {
				return err
			}
			return o.emitChange(cmd.OutOrStdout(), res, mountsHuman(args[0], res))
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	return cmd
}

// newSecretMountsCmd lists which of an app's keys are projected as files, and where.
func newSecretMountsCmd() *cobra.Command {
	o := &commonOpts{}
	cmd := &cobra.Command{
		Use:   "mounts <app>",
		Short: "List the secret keys an app reads as files, and where they land",
		Long:  "List which of an app's secret keys are projected into files, and the path each lands at.\n\n" + mountLong,
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, env, err := o.resolveAndConnectRead(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			res, err := c.SecretMounts(ctx, args[0], env)
			if err != nil {
				return err
			}
			return emit(cmd.OutOrStdout(), o.json, res, mountsHuman(args[0], res))
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	return cmd
}

// mountsHuman renders a projection for a person: the path each mounted key lands at, and the
// variable that carries the directory. It prints key names, which is all the result holds.
func mountsHuman(app string, res client.SecretMounts) string {
	if len(res.Mounts) == 0 {
		return fmt.Sprintf("No secret is mounted as a file on %s. Mount one with `burrow app secret mount %s KEY`.", app, app)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s reads %d secret(s) as files, mode 0400, read-only:\n", app, len(res.Mounts))
	for _, m := range res.Mounts {
		fmt.Fprintf(&b, "  %s\t%s\n", m.Key, m.Path)
	}
	fmt.Fprintf(&b, "%s=%s is set on the app.", controlplane.SecretsDirEnvVar, res.Dir)
	return b.String()
}
