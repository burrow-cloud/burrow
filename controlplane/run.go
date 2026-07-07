// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"fmt"
	"strings"
)

// defaultRunTTLSeconds is the finished-Job TTL a run applies when the request names none: one hour
// (ADR-0048 §7) — long enough to inspect a failure by hand, short enough that finished Jobs do not
// pile up. A request may override it, including 0 to delete the Job as soon as the output is captured.
const defaultRunTTLSeconds int32 = 3600

// Run executes a caller-provided one-off command inside the app's OWN current image and environment
// as a short-lived Kubernetes Job, waits for it to finish, and returns the captured output and exit
// code as a structured result (ADR-0048). It is the primitive behind migrations, seeds, backfills,
// and console-style tasks.
//
// The command runs in the app's currently-deployed release image — resolved the same way deploy and
// rollback resolve it — with the app's config env and per-app Secret injected via envFrom, so the
// command sees exactly the runtime, dependencies, config, and secrets the running app sees
// (ADR-0048 §2). No secret value crosses the API or MCP: the caller supplies only the command
// (ADR-0029). The operation is synchronous — launch, wait, capture, return (ADR-0048 §3): a non-zero
// exit is a normal structured outcome the agent reasons over, not a transport error.
//
// It is gated by the app.run guardrail (confirm by default), which gates WHETHER the command runs,
// not what it does — the opaque-command limitation is honest and stated (ADR-0048 §5). A held run
// returns a *GuardrailError needing confirmation; the agent surfaces the exact command to the human
// and re-invokes with Confirm only on explicit approval — it never self-confirms (ADR-0020). Every
// run — held, denied, or executed — is recorded in the audit log with the command (ADR-0027).
func (e *Engine) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if err := (App{Name: req.App}).Validate(); err != nil {
		return RunResult{}, fmt.Errorf("run: %w: %w", ErrInvalid, err)
	}
	if len(req.Command) == 0 {
		return RunResult{}, fmt.Errorf("run %s: command is empty: %w", req.App, ErrInvalid)
	}
	ttl := defaultRunTTLSeconds
	if req.TTLSeconds != nil {
		if *req.TTLSeconds < 0 {
			return RunResult{}, fmt.Errorf("run %s: ttl %d is negative: %w", req.App, *req.TTLSeconds, ErrInvalid)
		}
		ttl = *req.TTLSeconds
	}
	// Resolve the target environment to its namespace up front so an unknown or ambiguous environment
	// fails fast, before the guardrail decision or any cluster write (ADR-0047).
	ns, err := e.resolveMutatingNamespace(ctx, req.Env)
	if err != nil {
		return RunResult{}, fmt.Errorf("run %s: %w", req.App, err)
	}
	k := e.k8s.WithNamespace(ns)

	// The command runs in the app's currently-deployed image (ADR-0048 §2): resolve it the same way
	// deploy and rollback do. An app with nothing deployed has no image to run a command in.
	releases, err := e.db.Releases(ctx, req.App)
	if err != nil {
		return RunResult{}, fmt.Errorf("run %s: reading release history: %w", req.App, err)
	}
	cur, ok := lastDeployed(releases)
	if !ok {
		return RunResult{}, fmt.Errorf("run %s: no deployed release to run a command in — deploy it first: %w", req.App, ErrNotFound)
	}

	// Env is the app-global config store, sourced into the Job exactly as a deploy sources it
	// (ADR-0028) so the command sees the same non-secret config; the per-app Secret is injected via
	// envFrom in the kube seam, so no secret value passes through here.
	env, err := e.db.AppEnv(ctx, req.App)
	if err != nil {
		return RunResult{}, fmt.Errorf("run %s: reading env: %w", req.App, err)
	}

	pol, err := e.db.Policy(ctx)
	if err != nil {
		return RunResult{}, fmt.Errorf("run %s: loading guardrail policy: %w", req.App, err)
	}
	command := strings.Join(req.Command, " ")
	// The redacted audit args carry the command, image, and environment — never a secret value. The
	// command is the salient fact a reviewer reads, and the confirm prompt echoes it (ADR-0048 §5).
	args := map[string]string{"command": command, "image": cur.Image, "env": envName(req.Env)}
	if err := e.recordDecision(ctx, auditOpRun, req.App, args, GuardrailAppRun,
		pol.evaluateGuardrail(req.Env, "run", GuardrailAppRun, req.Confirm,
			fmt.Sprintf("running %q in %s (%s)", command, req.App, cur.Image))); err != nil {
		return RunResult{}, err
	}
	// The execution-row args carry the env KEY NAMES only — never values (ADR-0027).
	args["env_keys"] = auditKeys(env)

	res, err := k.RunJob(ctx, RunSpec{App: req.App, ID: e.ids.NewID(), Image: cur.Image, Command: req.Command, Env: env, TTLSeconds: ttl})
	if err != nil {
		e.recordExecution(ctx, auditOpRun, req.App, args, err)
		return RunResult{}, fmt.Errorf("run %s: %w", req.App, err)
	}
	res.App = req.App
	e.recordExecution(ctx, auditOpRun, req.App, args, nil)
	return res, nil
}
