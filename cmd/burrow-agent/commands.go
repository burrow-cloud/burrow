// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"time"

	"github.com/spf13/cobra"

	"github.com/burrow-cloud/burrow/client"
	"github.com/burrow-cloud/burrow/localconfig"
)

// rootLong orients the agent to burrow-agent as a whole: what it is, that output is JSON, and how it
// authenticates. It is the discovery surface (ADR-0049 §5) — the bare invocation prints it.
const rootLong = `burrow-agent is your control channel to Burrow: it reports the state of the user's applications
on their Kubernetes cluster so you can survey and diagnose, and it carries the operate-verbs so you
can act — the compute verbs (deploy, rollback, scale, autoscale, run), the routing verbs (expose,
unexpose, domain add/remove), the add-on operations (addon install/remove/attach/backup), the config
writes (config set/unset), secret unset, and the guarded destructive delete.

Every command prints its result as indented JSON, so you can pipe, grep, and jq it
(e.g. burrow-agent logs web | jq '.lines[] | select(.message | test("error"))').

A mutating verb prints a structured outcome envelope with a top-level "outcome" field:
  executed              — the operation ran; "result" carries its result.
  held_for_confirmation — a guardrail holds it; "code" and "message" say what needs approval.
                          Relay it to the human and, ONLY once they approve, re-run with --confirm.
                          Never self-confirm.
  denied                — a guardrail refused it outright; no --confirm will help.
  error                 — an actual failure (launch, transport, a not-found app).
Exit code: executed 0, error 1, held_for_confirmation 2, denied 3.

It authenticates to the control plane with a scoped, burrowd-only credential and holds no cluster
credentials — the control plane behind it holds those and enforces the guardrails. It builds and
pushes no images: deploy names an image reference already on a registry the cluster can pull from,
never code. A destructive verb like delete is still available, but it is guarded — held for the
human's confirmation, never self-confirmed. The dangerous ADMIN verbs (install, bootstrap, cluster
setup, guard set, credential writes, and — deliberately — setting a secret VALUE) are not part of
this binary at all. Run -h on any command to see what it does and the flags it takes.`

// newRootCmd builds the burrow-agent command tree: the read-only operate-verbs and the mutating
// compute verbs (deploy, rollback, scale, autoscale, run). The dangerous ADMIN verbs are structurally
// absent — never registered here — so this binary cannot express them (ADR-0049 §2a).
func newRootCmd() *cobra.Command {
	cobra.EnableCommandSorting = false
	root := &cobra.Command{
		Use:           "burrow-agent",
		Short:         "The coding agent's control channel to Burrow",
		Long:          rootLong,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(
		newAppsCmd(),
		newStatusCmd(),
		newLogsCmd(),
		newConfigCmd(),
		newSecretCmd(),
		newReachabilityCmd(),
		newClusterCmd(),
		newAddonsCmd(),
		newBackupsCmd(),
		newLogsQueryCmd(),
		newMetricsQueryCmd(),
		newGuardCmd(),
		newAuditCmd(),
		newProvidersCmd(),
		newEnvironmentsCmd(),
		// The mutating compute operate-verbs (ADR-0049 Phase 2a). Each funnels through the confirm
		// flow in mutate.go and prints an outcome envelope.
		newDeployCmd(),
		newRollbackCmd(),
		newScaleCmd(),
		newAutoscaleCmd(),
		newRunCmd(),
		// The remaining agent-exposed mutating verbs (ADR-0049 Phase 2b): routing, add-on, and the
		// guarded destructive delete. Each funnels through the same confirm flow in mutate.go. (config
		// set/unset and secret unset are attached as subcommands of the config/secret list verbs above.)
		newExposeCmd(),
		newUnexposeCmd(),
		newDomainCmd(),
		newAddonCmd(),
		newDeleteCmd(),
	)
	return root
}

// withClient resolves a control-plane client and the target environment, runs fn, and prints its
// result as JSON. Every client-backed verb funnels through it so wiring stays uniform.
func (o *connOpts) withClient(cmd *cobra.Command, fn func(ctx context.Context, c *client.Client, env string) (any, error)) error {
	ctx := cmd.Context()
	c, env, err := o.resolve(ctx, cmd.ErrOrStderr())
	if err != nil {
		return err
	}
	v, err := fn(ctx, c, env)
	if err != nil {
		return err
	}
	return emitJSON(cmd.OutOrStdout(), v)
}

func newAppsCmd() *cobra.Command {
	o := &connOpts{}
	cmd := &cobra.Command{
		Use:   "apps",
		Short: "List the applications Burrow manages and each one's running state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.withClient(cmd, func(ctx context.Context, c *client.Client, env string) (any, error) {
				return c.Apps(ctx, env)
			})
		},
	}
	bindConn(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	return cmd
}

func newStatusCmd() *cobra.Command {
	o := &connOpts{}
	cmd := &cobra.Command{
		Use:   "status <app>",
		Short: "Report an application's most recent release and live workload state",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.withClient(cmd, func(ctx context.Context, c *client.Client, env string) (any, error) {
				return c.Status(ctx, args[0], env)
			})
		},
	}
	bindConn(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	return cmd
}

func newLogsCmd() *cobra.Command {
	o := &connOpts{}
	var tail int
	cmd := &cobra.Command{
		Use:   "logs <app>",
		Short: "Return recent log lines for an application's workload",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.withClient(cmd, func(ctx context.Context, c *client.Client, env string) (any, error) {
				return c.Logs(ctx, args[0], env, tail)
			})
		},
	}
	bindConn(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().IntVar(&tail, "tail", 0, "maximum number of recent log lines to return (0 = server default)")
	return cmd
}

func newConfigCmd() *cobra.Command {
	o := &connOpts{}
	cmd := &cobra.Command{
		Use:   "config <app>",
		Short: "List an application's non-secret config vars (set/unset are subcommands)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.withClient(cmd, func(ctx context.Context, c *client.Client, env string) (any, error) {
				return c.Config(ctx, args[0], env)
			})
		},
	}
	bindConn(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	// The mutating config writes (ADR-0049 Phase 2b) hang off the list verb as subcommands, so
	// `config web` lists and `config set web K=V` writes. They funnel through the confirm flow in
	// mutate.go like every mutating verb.
	cmd.AddCommand(newConfigSetCmd(), newConfigUnsetCmd())
	return cmd
}

func newSecretCmd() *cobra.Command {
	o := &connOpts{}
	cmd := &cobra.Command{
		Use:   "secret <app>",
		Short: "List the KEYS of an application's secrets (never the values); unset is a subcommand",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.withClient(cmd, func(ctx context.Context, c *client.Client, env string) (any, error) {
				keys, err := c.Secrets(ctx, args[0], env)
				if err != nil {
					return nil, err
				}
				return map[string][]string{"keys": keys}, nil
			})
		},
	}
	bindConn(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	// `secret unset` (removing a key carries no value) hangs off the list verb as a subcommand. There
	// is deliberately NO `secret set`: a secret VALUE never routes through the agent channel (ADR-0029),
	// so the agent binary cannot express it — the human sets secrets with the `burrow` CLI.
	cmd.AddCommand(newSecretUnsetCmd())
	return cmd
}

func newReachabilityCmd() *cobra.Command {
	o := &connOpts{}
	var wait bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "reachability <app>",
		Short: "Report whether an application is reachable at its hostname, link by link",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.withClient(cmd, func(ctx context.Context, c *client.Client, env string) (any, error) {
				if wait {
					return c.WaitReachable(ctx, args[0], env, timeout, nil)
				}
				return c.Reachability(ctx, args[0], env)
			})
		},
	}
	bindConn(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().BoolVar(&wait, "wait", false, "poll until the app is live (reachable) or the timeout elapses")
	cmd.Flags().DurationVar(&timeout, "timeout", 3*time.Minute, "how long to poll in --wait mode")
	return cmd
}

func newClusterCmd() *cobra.Command {
	o := &connOpts{}
	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Report the cluster's capabilities (ingress, storage, load balancer, TLS, DNS)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.withClient(cmd, func(ctx context.Context, c *client.Client, _ string) (any, error) {
				return c.Cluster(ctx)
			})
		},
	}
	bindConn(cmd.Flags(), o)
	return cmd
}

func newAddonsCmd() *cobra.Command {
	o := &connOpts{}
	cmd := &cobra.Command{
		Use:   "addons",
		Short: "List the backing-service add-ons installed on the cluster",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.withClient(cmd, func(ctx context.Context, c *client.Client, _ string) (any, error) {
				return c.Addons(ctx)
			})
		},
	}
	bindConn(cmd.Flags(), o)
	return cmd
}

func newBackupsCmd() *cobra.Command {
	o := &connOpts{}
	cmd := &cobra.Command{
		Use:   "backups <addon> [app]",
		Short: "List recorded database backups for an add-on, optionally restricted to one app",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			app := ""
			if len(args) == 2 {
				app = args[1]
			}
			return o.withClient(cmd, func(ctx context.Context, c *client.Client, _ string) (any, error) {
				return c.Backups(ctx, args[0], app)
			})
		},
	}
	bindConn(cmd.Flags(), o)
	return cmd
}

func newLogsQueryCmd() *cobra.Command {
	o := &connOpts{}
	var limit int
	var backend string
	cmd := &cobra.Command{
		Use:   "logs-query [query]",
		Short: "Query the cluster's aggregated logs add-on (LogsQL); empty query matches everything",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			query := ""
			if len(args) == 1 {
				query = args[0]
			}
			return o.withClient(cmd, func(ctx context.Context, c *client.Client, _ string) (any, error) {
				return c.QueryLogs(ctx, query, limit, backend)
			})
		},
	}
	bindConn(cmd.Flags(), o)
	cmd.Flags().IntVar(&limit, "limit", 0, "maximum number of records to return (0 = server default)")
	cmd.Flags().StringVar(&backend, "backend", "", "target a specific logs add-on when more than one serves the logs capability")
	return cmd
}

func newMetricsQueryCmd() *cobra.Command {
	o := &connOpts{}
	var backend string
	cmd := &cobra.Command{
		Use:   "metrics-query <query>",
		Short: "Run an instant PromQL query against the connected metrics add-on",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.withClient(cmd, func(ctx context.Context, c *client.Client, _ string) (any, error) {
				return c.QueryMetrics(ctx, args[0], backend)
			})
		},
	}
	bindConn(cmd.Flags(), o)
	cmd.Flags().StringVar(&backend, "backend", "", "target a specific metrics add-on when more than one serves the metrics capability")
	return cmd
}

func newGuardCmd() *cobra.Command {
	o := &connOpts{}
	cmd := &cobra.Command{
		Use:   "guard",
		Short: "List the control-plane guardrails and their current dispositions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.withClient(cmd, func(ctx context.Context, c *client.Client, env string) (any, error) {
				return c.Guardrails(ctx, env)
			})
		},
	}
	bindConn(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	return cmd
}

func newAuditCmd() *cobra.Command {
	o := &connOpts{}
	var app, operation, outcome string
	var limit int
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Review the control plane's append-only audit log of guarded, mutating operations",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.withClient(cmd, func(ctx context.Context, c *client.Client, _ string) (any, error) {
				return c.Audit(ctx, client.AuditFilter{App: app, Operation: operation, Outcome: outcome, Limit: limit})
			})
		},
	}
	bindConn(cmd.Flags(), o)
	cmd.Flags().StringVar(&app, "app", "", "filter to one app/host/add-on target")
	cmd.Flags().StringVar(&operation, "operation", "", "filter to one operation (e.g. deploy, rollback, app_delete)")
	cmd.Flags().StringVar(&outcome, "outcome", "", "filter to one outcome (e.g. executed, held, denied, failed)")
	cmd.Flags().IntVar(&limit, "limit", 0, "maximum number of rows to return (0 = server default)")
	return cmd
}

func newProvidersCmd() *cobra.Command {
	o := &connOpts{}
	cmd := &cobra.Command{
		Use:   "providers",
		Short: "List the configured cloud providers and the capabilities each serves",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.withClient(cmd, func(ctx context.Context, c *client.Client, _ string) (any, error) {
				return c.Providers(ctx)
			})
		},
	}
	bindConn(cmd.Flags(), o)
	return cmd
}

// environmentsResult is the JSON shape of the environments command: the local handles plus the
// current selection, read purely from the local config with no cluster contact.
type environmentsResult struct {
	Environments []localconfig.Environment `json:"environments"`
	Current      string                    `json:"current"`
	Mode         string                    `json:"mode"`
	Context      string                    `json:"context"`
	Namespace    string                    `json:"namespace"`
}

func newEnvironmentsCmd() *cobra.Command {
	o := &connOpts{}
	cmd := &cobra.Command{
		Use:   "environments",
		Short: "List your local environment handles and the current selection (reads no cluster)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := localconfig.Load()
			if err != nil {
				return err
			}
			resolved, err := localconfig.Resolve(cfg, o.kubeconfig)
			if err != nil {
				return err
			}
			return emitJSON(cmd.OutOrStdout(), environmentsResult{
				Environments: cfg.Environments,
				Current:      resolved.Name,
				Mode:         string(resolved.Mode),
				Context:      resolved.Context,
				Namespace:    resolved.Namespace,
			})
		},
	}
	bindConn(cmd.Flags(), o)
	return cmd
}
