// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/burrow-cloud/burrow/client"
	"github.com/burrow-cloud/burrow/internal/agentsurface"
	"github.com/burrow-cloud/burrow/localconfig"
)

// rootLong orients the agent to burrow-agent as a whole: what it is, that output is JSON, and how it
// authenticates. It is the discovery surface (ADR-0049 §5) — the bare invocation prints it.
const rootLong = `burrow-agent is your control channel to Burrow: it reports the state of the user's applications
on their Kubernetes cluster so you can survey and diagnose, and it carries the operate-verbs so you
can act — the compute verbs (deploy, build, rollback, scale, autoscale, run), the routing verbs (expose,
unexpose, domain add/remove), the add-on operations (addon install/attach/backup), the config
writes (config set/unset), secret unset, and the guarded destructive delete.

Every command prints its result as indented JSON, so you can pipe, grep, and jq it
(e.g. burrow-agent logs web | jq '.lines[] | select(.message | test("error"))').

When something is wrong and you do not yet know what, burrow-agent failures is the place to start:
it reports what broke across every object Burrow manages, with a first_seen, a last_seen and a
count per (object, reason). Burrow reports what it observed and never claims a cause — reading
twenty rows and concluding "the node pool was tainted at 02:14" is your half of the work. Read the
"coverage" field before concluding a cluster is healthy: its "gaps" are stretches in which nothing
was observing, so an empty list over a gap is not evidence that nothing broke.

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
this binary at all. Run burrow-agent guard to see both kinds of limit at once: the guardrail
dispositions, and the capabilities absent from this binary with what each one is and who can run
it. Relay that to the human rather than reporting an unknown command or working around it.
Run -h on any command to see what it does and the flags it takes.`

// newRootCmd builds the burrow-agent command tree: the read-only operate-verbs and the mutating
// compute verbs (deploy, build, rollback, scale, autoscale, run). The dangerous ADMIN verbs are structurally
// absent — never registered here — so this binary cannot express them (ADR-0049 §2a).
func newRootCmd() *cobra.Command {
	cobra.EnableCommandSorting = false
	root := &cobra.Command{
		Use:           "burrow-agent",
		Short:         "The coding agent's control channel to Burrow",
		Long:          rootLong,
		SilenceUsage:  true,
		SilenceErrors: true,
		// `burrow-agent --version` reports the version this binary sends in the ADR-0039 handshake, so
		// a stranded agent (and `burrow version`, which reads it) can see the skew rather than infer it
		// from a refusal. It is deliberately a FLAG, not a subcommand: the agent surface is a closed
		// allow-list (agent_surface_guard_test.go) and reporting your own version is not a capability.
		// The template emits JSON like every other output this binary produces (ADR-0049 §1).
		Version: agentVersion(),
	}
	root.SetVersionTemplate("{\"version\": \"{{.Version}}\"}\n")
	root.AddCommand(
		newAppsCmd(),
		newStatusCmd(),
		newHistoryCmd(),
		newNextTagCmd(),
		newLogsCmd(),
		newConfigCmd(),
		newHealthCmd(),
		newSecretCmd(),
		newReachabilityCmd(),
		newClusterCmd(),
		newAddonsCmd(),
		newBackupsCmd(),
		newLogsQueryCmd(),
		newMetricsQueryCmd(),
		newGuardCmd(),
		newAuditCmd(),
		newFailuresCmd(),
		newProvidersCmd(),
		newEnvironmentsCmd(),
		// The mutating compute operate-verbs (ADR-0049 Phase 2a). Each funnels through the confirm
		// flow in mutate.go and prints an outcome envelope.
		newDeployCmd(),
		newBuildCmd(),
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

// newHistoryCmd reports an app's deploy timeline: the releases recorded for it, newest first — what
// versions it has been rolled to, when, and whether each landed (the release status conveys success
// or failure). It is READ-ONLY: it reads the deploy records the control plane already writes and
// changes nothing, so it funnels through withClient (never the mutate path) and prints the release
// array as JSON, like the other read verbs (ADR-0049, ADR-0052 §6 — the agent observes). ADR-0052 §5
// will enrich each release with its deploy provenance (auto-update vs. manual, the level); this
// surfaces it automatically once the record carries it, with no change here.
func newHistoryCmd() *cobra.Command {
	o := &connOpts{}
	cmd := &cobra.Command{
		Use:   "history <app>",
		Short: "Report an app's deploy timeline: the versions it has been rolled to, when, and whether each landed",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.withClient(cmd, func(ctx context.Context, c *client.Client, env string) (any, error) {
				return c.History(ctx, args[0], env)
			})
		},
	}
	bindConn(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	return cmd
}

// newNextTagCmd suggests the app's next semver release tags from its current running tag (ADR-0052 §8).
// It turns "please use semver" into concrete numbers the agent applies to its build: the current tag
// plus the next patch/minor/major. It is READ-ONLY guidance — it reads the running tag the control
// plane already knows and computes nothing on the cluster — so it funnels through withClient and
// prints JSON like the other read verbs. A missing release or a non-semver current tag degrades to a
// note rather than erroring (ADR-0040), so the agent always gets a usable answer.
func newNextTagCmd() *cobra.Command {
	o := &connOpts{}
	cmd := &cobra.Command{
		Use:   "next-tag <app>",
		Short: "Suggest the next semver release tag (patch/minor/major) from the app's current running tag",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.withClient(cmd, func(ctx context.Context, c *client.Client, env string) (any, error) {
				return c.NextTag(ctx, args[0], env)
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
	cmd.AddCommand(newCapacityCmd())
	return cmd
}

// newCapacityCmd is `burrow-agent cluster capacity`: the scheduling capacity/headroom surface
// (issue #275) so the agent can answer "is the cluster at capacity, do I need to scale?" and
// pre-flight a resource-hungry operation. It reports, per node and cluster-total, allocatable /
// committed (sum of pod requests) / free CPU and memory, the top consumers, and a verdict on
// whether a typical in-cluster build fits — all from the Kubernetes API alone, no metrics-server.
// Read-only; JSON-first so the agent can compose the result.
func newCapacityCmd() *cobra.Command {
	o := &connOpts{}
	cmd := &cobra.Command{
		Use:   "capacity",
		Short: "Report scheduling headroom: per-node allocatable vs committed, top consumers, and whether a build fits (no metrics-server needed)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.withClient(cmd, func(ctx context.Context, c *client.Client, _ string) (any, error) {
				return c.Capacity(ctx)
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
			// A listing is a read, so an unnamed environment spans them all rather than being
			// refused; each row says which environment its dump came from (ADR-0067 §1).
			return o.withClient(cmd, func(ctx context.Context, c *client.Client, env string) (any, error) {
				return c.Backups(ctx, args[0], app, env)
			})
		},
	}
	bindConn(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
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

// newGuardCmd is the agent's read-only view of everything it cannot do, in one answer
// ([ADR-0065](../../docs/adr/0065-what-belongs-on-the-agent-surface.md) §7). It reports two
// different kinds of limit and keeps them apart:
//
//   - "guardrails" — the control plane's dispositions. A `deny` here is a legible refusal the
//     agent can anticipate and relay, and an operator can relax it with `burrow guard set`.
//   - "absent_capabilities" — verbs that are not compiled into this binary at all, each with what
//     it is, why it is held back, and who can perform it instead.
//
// The second group exists because an absent verb is otherwise a DEAD END: `unknown command`, with
// no account of what the capability was or who has it. ADR-0065 §5 is blunt about where dead ends
// lead — an agent that hits one may get creative and route around the control channel entirely,
// reaching for `kubectl` or a shell, which is the failure ADR-0021 says Burrow cannot close from
// the inside. Absent AND legible is a refusal the agent can hand to a human, and that is what
// makes tier 1 tolerable rather than merely safe.
//
// Reading this enumerates the surface, which ADR-0065 §7 accepts outright: the CLI is open source
// and `--help` already reveals it, so nothing is withheld and no access control guards the read.
//
// The command stays READ-ONLY. It has no subcommands and writes nothing: `guard set` is the
// operator's lever, run with the admin kubeconfig, and its absence from this binary is what makes
// every disposition above trustworthy rather than advisory.
func newGuardCmd() *cobra.Command {
	o := &connOpts{}
	cmd := &cobra.Command{
		Use:   "guard",
		Short: "List the control-plane guardrails, their dispositions, and the capabilities absent from this binary",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.withClient(cmd, func(ctx context.Context, c *client.Client, env string) (any, error) {
				gs, err := c.Guardrails(ctx, env)
				if err != nil {
					return nil, err
				}
				// Derived from the command tree this binary actually registers, so a verb dropped
				// from the binary becomes legible here with no second edit.
				return agentsurface.NewGuardReport(gs, absentCapabilities(cmd.Root())), nil
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

// newFailuresCmd is the failure ledger's read surface on the agent channel (ADR-0074 §8). It is here
// and not only on the human CLI because ADR-0074 §5 makes the agent the surface's most important
// consumer: Burrow's half is to report every failure it observed, completely and in a shape
// something else can reason over; the agent's half is to read twenty rows and conclude "the node
// pool was tainted at 02:14; remove the taint or add the toleration". Synthesising a cause from
// partial evidence is what a language model is good at and a control plane is not, and Burrow
// deliberately does not run one.
//
// So this prints ROWS, NEVER GROUPS. The `burrow failures` listing groups by shared reason because a
// person reading thirty red lines during an incident needs the cascade to read as one event; that is
// a human-facing heuristic, and an agent inheriting it would be correlating on someone else's terms
// instead of its own. Every answer carries the observation coverage behind it, so a gap in the
// ledger cannot be mistaken for an hour in which nothing broke.
func newFailuresCmd() *cobra.Command {
	o := &connOpts{}
	var kind, name, env, reason string
	var since time.Duration
	var all bool
	var limit int
	cmd := &cobra.Command{
		Use:   "failures",
		Short: "Report what is broken across everything Burrow manages, with the observation coverage behind the answer",
		Long: "failures reports the control plane's record of what broke across every object it manages —\n" +
			"apps, add-ons, backups, and exposures — as rows, oldest first. One row per (object, reason),\n" +
			"each with a first_seen, a last_seen, a resolved_at, and how many observations found it\n" +
			"present.\n\n" +
			"Burrow reports what it observed and never claims a cause. Rows sharing a reason and a window\n" +
			"are a correlation you can reason over — a taint, a database outage — not a diagnosis Burrow\n" +
			"asserts. Forming the cause and the fix from them is your half of the work.\n\n" +
			"The `coverage` field says whether the answer can be read at face value: its `gaps` are\n" +
			"stretches in which nothing was observing, so an empty `failures` list over a gap is not\n" +
			"evidence that nothing broke. Check it before concluding a cluster is healthy.\n\n" +
			"By default it reports failures that are still active. --since looks back over a window and\n" +
			"includes ones that have since recovered; --all is the whole retained history.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.withClient(cmd, func(ctx context.Context, c *client.Client, _ string) (any, error) {
				return c.Failures(ctx, client.FailureQuery{
					Kind: kind, Name: name, Env: env, Reason: reason, Since: since, All: all, Limit: limit,
				})
			})
		},
	}
	bindConn(cmd.Flags(), o)
	cmd.Flags().StringVar(&kind, "kind", "", "filter to one kind of object ("+strings.Join(client.FailureKinds(), ", ")+")")
	cmd.Flags().StringVar(&name, "name", "", "filter to one object by name")
	cmd.Flags().StringVar(&env, "env", "", "filter to one environment")
	cmd.Flags().StringVar(&reason, "reason", "", "filter to one reason (e.g. Unschedulable, CrashLoopBackOff)")
	cmd.Flags().DurationVar(&since, "since", 0, "look back over this window, including failures that have since recovered (e.g. 24h)")
	cmd.Flags().BoolVar(&all, "all", false, "include failures that have since recovered, over the whole retained history")
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
