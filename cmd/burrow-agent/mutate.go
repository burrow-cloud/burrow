// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/burrow-cloud/burrow/client"
	"github.com/burrow-cloud/burrow/controlplane"
)

// The four outcomes a mutating verb resolves to. They are the stable top-level "outcome" value the
// agent branches on, distinct so a held operation is never mistaken for a failure and a denial is
// never mistaken for a hold (ADR-0020, ADR-0049).
const (
	outcomeExecuted = "executed"              // the operation ran; Result carries its result.
	outcomeHeld     = "held_for_confirmation" // a guardrail holds it for the human's approval.
	outcomeDenied   = "denied"                // a guardrail refused it outright; no confirm helps.
	outcomeError    = "error"                 // an actual failure (launch, transport, not-found).
)

// The process exit code each outcome maps to. executed is success; the others are distinct non-zero
// codes so a script notices the operation did not run, while the JSON envelope is still printed to
// stdout regardless (ADR-0049 §2c).
const (
	exitCodeExecuted = 0
	exitCodeError    = 1
	exitCodeHeld     = 2
	exitCodeDenied   = 3
)

// outcome is the structured envelope every mutating verb prints as JSON. The top-level Outcome field
// makes the four cases unambiguous; the agent reads it, decides, and relays to the human. On a held
// operation ConfirmRequired signals that re-running with --confirm proceeds — a signal the agent acts
// on ONLY after the human approves, never on its own (ADR-0020).
type outcome struct {
	Outcome   string `json:"outcome"`
	Operation string `json:"operation"`
	// Result is the operation's own result (a DeployResult, RunResult, and so on), present only when
	// the operation executed.
	Result any `json:"result,omitempty"`
	// Code is the machine-readable guardrail that tripped, present on held and denied outcomes.
	Code string `json:"code,omitempty"`
	// Message is a human-readable explanation of a held, denied, or error outcome — exactly what the
	// agent relays to the human.
	Message string `json:"message,omitempty"`
	// ConfirmRequired is true only on a held outcome: re-running the command with --confirm proceeds
	// once the human approves. burrow-agent never sets --confirm itself.
	ConfirmRequired bool `json:"confirm_required,omitempty"`
	// Hint spells out the confirm action in words, for the human the agent relays it to.
	Hint string `json:"hint,omitempty"`
}

// exitError carries a process exit code without a message to print. A mutating verb has already
// printed its outcome envelope to stdout, so main only needs the code — it must not print a second,
// human-shaped error line that would muddy the JSON contract.
type exitError struct{ code int }

func (e *exitError) Error() string { return "burrow-agent: exit " + strconv.Itoa(e.code) }

// classify turns an operation error into the right non-executed outcome. It reuses the control plane's
// own classification rather than reinventing it: the API surfaces a held operation as an *APIError with
// NeedsConfirmation set (the disposition-confirm hold, ADR-0020), and a guardrail denial as an
// *APIError whose Code is a known guardrail (controlplane.KnownGuardrail). Everything else — a
// not-found app, an ambiguous environment, a transport failure — is a plain error the agent surfaces
// and stops on.
func classify(operation string, err error) outcome {
	var api *client.APIError
	if errors.As(err, &api) {
		switch {
		case api.NeedsConfirmation:
			return outcome{
				Outcome:         outcomeHeld,
				Operation:       operation,
				Code:            api.Code,
				Message:         api.Message,
				ConfirmRequired: true,
				Hint:            "relay this to the human; re-run with --confirm ONLY after they approve. Never self-confirm.",
			}
		case controlplane.KnownGuardrail(controlplane.GuardrailCode(api.Code)):
			return outcome{
				Outcome:   outcomeDenied,
				Operation: operation,
				Code:      api.Code,
				Message:   api.Message,
			}
		}
	}
	return outcome{Outcome: outcomeError, Operation: operation, Message: err.Error()}
}

// emitOutcome prints oc as JSON and returns the error that carries its process exit code. An executed
// outcome returns nil (exit 0); the others return an *exitError so main exits distinctly without
// printing a second error line. A failure to write the JSON is returned raw — there is no envelope to
// fall back on.
func emitOutcome(w io.Writer, oc outcome) error {
	if err := emitJSON(w, oc); err != nil {
		return err
	}
	switch oc.Outcome {
	case outcomeExecuted:
		return nil
	case outcomeHeld:
		return &exitError{code: exitCodeHeld}
	case outcomeDenied:
		return &exitError{code: exitCodeDenied}
	default:
		return &exitError{code: exitCodeError}
	}
}

// mutate is the confirm-flow spine every mutating verb funnels through. It resolves the scoped client
// and target environment, runs fn, and prints exactly one outcome envelope: executed with the result,
// or a held/denied/error envelope classified from the error. It never retries and never sets confirm
// on its own — confirm reaches the control plane only via the caller's --confirm flag (ADR-0049 §2c).
func (o *connOpts) mutate(cmd *cobra.Command, operation string, fn func(ctx context.Context, c *client.Client, env string) (any, error)) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	c, env, err := o.resolve(ctx, cmd.ErrOrStderr())
	if err != nil {
		return emitOutcome(out, classify(operation, err))
	}
	res, err := fn(ctx, c, env)
	if err != nil {
		return emitOutcome(out, classify(operation, err))
	}
	return emitOutcome(out, outcome{Outcome: outcomeExecuted, Operation: operation, Result: res})
}

// newDeployCmd deploys an app by image reference plus small metadata (ADR-0004, ADR-0007). burrow-agent
// never builds or pushes an image: it sends the reference and metadata, never code — the client-side
// build path stays on the human `burrow` CLI.
func newDeployCmd() *cobra.Command {
	o := &connOpts{}
	var image string
	var replicas, metricsPort int
	var confirm bool
	cmd := &cobra.Command{
		Use:   "deploy <app> --image <ref> [-- command args...]",
		Short: "Deploy an app by image reference (never builds or pushes; the reference must already be on a registry)",
		Long: "Deploy an app to the cluster by container image reference. burrow-agent NEVER builds or\n" +
			"pushes images (ADR-0004): build and push the image with your own tooling (e.g. docker build)\n" +
			"to a registry the cluster can pull from, tagging it with an incrementing semantic version and\n" +
			"never reusing a tag, then pass that reference with --image.\n\n" +
			"To run something other than the image's default entrypoint, pass the command after a --\n" +
			"separator, like kubectl run:\n" +
			"  burrow-agent deploy worker --image myrepo/app:1.2.3 -- ./worker --queue emails\n\n" +
			"Config and secrets are a separate, app-global store sourced at deploy time — not passed here.\n" +
			"Deploying no longer resets the replica count: omit --replicas to preserve the current scale.\n\n" +
			"Deploy is allowed by default; an operator may set the app.deploy guardrail to hold it for\n" +
			"confirmation (e.g. in prod) or deny it. When held, the outcome says so — relay it and re-run\n" +
			"with --confirm ONLY after the human approves.",
		// Exactly one positional (the app name) before any --; everything after -- overrides the command.
		Args: func(cmd *cobra.Command, args []string) error {
			n := len(args)
			if d := cmd.ArgsLenAtDash(); d >= 0 {
				n = d
			}
			if n != 1 {
				return fmt.Errorf("expected exactly one app name, got %d", n)
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if image == "" {
				return errors.New("--image is required")
			}
			app := args[0]
			var command []string
			if d := cmd.ArgsLenAtDash(); d >= 0 {
				command = args[d:]
			}
			return o.mutate(cmd, "deploy", func(ctx context.Context, c *client.Client, env string) (any, error) {
				return c.Deploy(ctx, app, client.DeployRequest{
					Env:         env,
					Image:       image,
					Command:     command,
					MetricsPort: int32(metricsPort),
					Replicas:    int32(replicas),
					Confirm:     confirm,
				})
			})
		},
	}
	bindConn(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().StringVar(&image, "image", "", "container image reference to deploy (required)")
	cmd.Flags().IntVar(&replicas, "replicas", 0, "desired replicas (0 = keep current; a new app defaults to 1; ignored while autoscaling is enabled)")
	cmd.Flags().IntVar(&metricsPort, "metrics-port", 0, "annotate the pod so the metrics add-on scrapes /metrics on this port")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm a deploy a guardrail holds for confirmation (supply only after the human approves)")
	return cmd
}

// newRollbackCmd rolls an app back to its previous release.
func newRollbackCmd() *cobra.Command {
	o := &connOpts{}
	var confirm bool
	cmd := &cobra.Command{
		Use:   "rollback <app>",
		Short: "Roll an app back to its previous release",
		Long: "Roll an application back to its previously running release by redeploying that release's\n" +
			"image reference. Rollback is a recovery action, allowed by default; an operator may set the\n" +
			"app.rollback guardrail to hold it for confirmation. When held, the outcome says so — relay it\n" +
			"and re-run with --confirm ONLY after the human approves.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.mutate(cmd, "rollback", func(ctx context.Context, c *client.Client, env string) (any, error) {
				return c.Rollback(ctx, args[0], env, confirm)
			})
		},
	}
	bindConn(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm a rollback a guardrail holds for confirmation (supply only after the human approves)")
	return cmd
}

// newScaleCmd sets an app's replica count, subject to the replica-ceiling and scale-to-zero guardrails.
func newScaleCmd() *cobra.Command {
	o := &connOpts{}
	var confirm bool
	cmd := &cobra.Command{
		Use:   "scale <app> <replicas>",
		Short: "Set an app's replica count",
		Long: "Set an application's replica count. A guardrail may refuse it (above the replica ceiling) or\n" +
			"hold it for confirmation (scaling to zero). When held, the outcome says so — relay it and\n" +
			"re-run with --confirm ONLY after the human approves.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			n, err := strconv.Atoi(args[1])
			if err != nil {
				return fmt.Errorf("replicas must be a number, got %q", args[1])
			}
			return o.mutate(cmd, "scale", func(ctx context.Context, c *client.Client, env string) (any, error) {
				return c.Scale(ctx, args[0], env, int32(n), confirm)
			})
		},
	}
	bindConn(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm a scale a guardrail holds for confirmation (supply only after the human approves)")
	return cmd
}

// newAutoscaleCmd configures autoscaling for an app, or turns it off.
func newAutoscaleCmd() *cobra.Command {
	o := &connOpts{}
	var (
		min, max, cpu, memory int32
		confirm               bool
	)
	cmd := &cobra.Command{
		Use:   "autoscale <app> [off]",
		Short: "Configure autoscaling for an app, or turn it off",
		Long: "Configure a HorizontalPodAutoscaler on the app's Deployment so it scales between --min and\n" +
			"--max replicas to hold a target CPU (and optional memory) utilization. The max is bounded by\n" +
			"the replica-ceiling guardrail. Autoscaling needs metrics-server; without it the autoscaler is\n" +
			"set but will not scale until it is installed (the result carries a warning).\n\n" +
			"Run \"burrow-agent autoscale <app> off\" to remove autoscaling (idempotent).\n\n" +
			"A guardrail may hold this for confirmation or deny it. When held, the outcome says so — relay\n" +
			"it and re-run with --confirm ONLY after the human approves.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			app := args[0]
			off := len(args) == 2
			if off && args[1] != "off" {
				return fmt.Errorf("second argument must be \"off\" to turn autoscaling off, got %q", args[1])
			}
			return o.mutate(cmd, "autoscale", func(ctx context.Context, c *client.Client, env string) (any, error) {
				if off {
					if err := c.DisableAutoscale(ctx, app, env, confirm); err != nil {
						return nil, err
					}
					return map[string]string{"app": app, "autoscaling": "off"}, nil
				}
				return c.Autoscale(ctx, app, client.AutoscaleRequest{Env: env, Min: min, Max: max, CPU: cpu, Memory: memory, Confirm: confirm})
			})
		},
	}
	bindConn(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().Int32Var(&min, "min", 1, "minimum replicas")
	cmd.Flags().Int32Var(&max, "max", 10, "maximum replicas (bounded by the replica-ceiling guardrail)")
	cmd.Flags().Int32Var(&cpu, "cpu", 80, "target average CPU utilization percent")
	cmd.Flags().Int32Var(&memory, "memory", 0, "target average memory utilization percent (0 leaves it unset)")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm an autoscale a guardrail holds for confirmation (supply only after the human approves)")
	return cmd
}

// newRunCmd runs a one-off command in the app's own current image and environment (ADR-0048). The
// executed result is the RunResult; a non-zero exit code is a normal executed outcome the agent reasons
// over, not an error.
func newRunCmd() *cobra.Command {
	o := &connOpts{}
	var confirm bool
	var ttl time.Duration
	cmd := &cobra.Command{
		Use:   "run <app> -- command args...",
		Short: "Run a one-off command in an app's own image and environment",
		Long: "Run a one-off command inside an app's own current image, in its namespace, with its config\n" +
			"and secrets injected exactly as the running app sees them. Use it for the tasks that belong in\n" +
			"the app's runtime: database migrations, seed and fixture loads, data backfills, a maintenance\n" +
			"script.\n\n" +
			"Pass the command after a -- separator, like kubectl run:\n" +
			"  burrow-agent run web -- npm run migrate\n\n" +
			"The run is synchronous: Burrow launches the command, waits for it to finish, and reports the\n" +
			"exit code and the command's combined stdout+stderr output. A non-zero exit code is a NORMAL\n" +
			"executed outcome to reason over, not a failure — the envelope's outcome is still \"executed\".\n\n" +
			"Gated by the app.run guardrail (confirm by default), which gates WHETHER the command runs, not\n" +
			"what it does: this is a command runner, not a SQL firewall, so a command can still make\n" +
			"destructive changes. When held, the outcome says so — relay it and re-run with --confirm ONLY\n" +
			"after the human approves.",
		// Exactly one positional (the app name) before any --; everything after -- is the command.
		Args: func(cmd *cobra.Command, args []string) error {
			n := len(args)
			if d := cmd.ArgsLenAtDash(); d >= 0 {
				n = d
			}
			if n != 1 {
				return fmt.Errorf("expected exactly one app name before --, got %d", n)
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			app := args[0]
			var command []string
			if d := cmd.ArgsLenAtDash(); d >= 0 {
				command = args[d:]
			}
			if len(command) == 0 {
				return errors.New("a command is required after --, e.g. `burrow-agent run web -- npm run migrate`")
			}
			req := client.RunRequest{Command: command, Confirm: confirm}
			// An omitted --ttl leaves TTLSeconds nil so the server applies its default (1h); a supplied
			// duration (including 0, delete immediately) is sent as seconds. A negative is rejected here.
			if cmd.Flags().Changed("ttl") {
				if ttl < 0 {
					return fmt.Errorf("--ttl must not be negative, got %s", ttl)
				}
				secs := int32(ttl.Seconds())
				req.TTLSeconds = &secs
			}
			return o.mutate(cmd, "run", func(ctx context.Context, c *client.Client, env string) (any, error) {
				req.Env = env
				return c.Run(ctx, app, req)
			})
		},
	}
	bindConn(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().DurationVar(&ttl, "ttl", 0, "how long to keep the finished Job before it is garbage-collected (e.g. 30m; 0 = delete immediately; omit to keep the default of 1h)")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm a run a guardrail holds for confirmation (supply only after the human approves)")
	return cmd
}
