// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Lifecycle hooks are named for THE MOMENT THEY RUN (ADR-0072 §1). `pre-deploy` runs before a
// deploy's image reaches the cluster; `pre-rollback` runs before a rollback's older image does.
// The name answers the only question a reader has — when does my command run — which is why this
// is not called a release command: in every other tool a release is the artifact that records what
// shipped, and borrowing the word would ask the reader to learn a second meaning while answering
// nothing (§1).
//
// A hook is CONFIGURATION, held per app and environment beside the app's config (ADR-0028), because
// the path that needs it has no caller to supply one: auto-deploy exists precisely so nobody has to
// be there (§1). Burrow does not understand migrations — it runs a command, and versioning, ordering
// and idempotency belong to the user's own tool (§10).

// HookPhase names when a hook runs. The value is the phase's name exactly as an operator types it
// (`--on pre-deploy`), so the configured value and the documented moment are one string.
type HookPhase string

const (
	// HookPreDeploy runs from the image BEING DEPLOYED, before traffic moves, on every deploy path —
	// an explicit `burrow app deploy`, a build that ends in a deploy, and an unattended auto-deploy
	// alike (ADR-0072 §2). Its failure aborts the deploy (§3).
	HookPreDeploy HookPhase = "pre-deploy"
	// HookPreRollback runs from the image being rolled back FROM, before traffic moves back
	// (ADR-0072 §8). Unset means nothing runs, which is the safe default: a team practising
	// expand/contract migrates forward only and would be harmed by anything running on a rollback.
	HookPreRollback HookPhase = "pre-rollback"
)

// hookPhasePostDeploy is the third phase ADR-0072 names (§4). It is NOT accepted yet: the phases that
// run before an image moves are built, and the phase that reports how a settled rollout went is
// separate work. Storing a `post-deploy` command Burrow never runs would be a setting that silently
// does nothing, which is worse than a refusal that says so (ADR-0009).
const hookPhasePostDeploy HookPhase = "post-deploy"

// hookPhases is the closed set of phases a hook may be set on, in the order a listing shows them:
// the order they would fire in around an app's life, not alphabetical.
var hookPhases = []HookPhase{HookPreDeploy, HookPreRollback}

// HookPhases returns every phase a hook may be set on, so a caller can name the set without
// reaching into the catalogue.
func HookPhases() []HookPhase {
	return append([]HookPhase(nil), hookPhases...)
}

// KnownHookPhase reports whether p names a phase hooks may be set on today.
func KnownHookPhase(p HookPhase) bool {
	for _, known := range hookPhases {
		if known == p {
			return true
		}
	}
	return false
}

// validateHookPhase rejects a phase no hook may be set on. It answers `post-deploy` specifically
// rather than lumping it in with a typo: it is a real phase of the record this implements, and a
// reader who typed it deserves to know it is not wired rather than to conclude they misspelled it.
func validateHookPhase(p HookPhase) error {
	if KnownHookPhase(p) {
		return nil
	}
	if p == hookPhasePostDeploy {
		return fmt.Errorf("phase %q is not available yet; setting it would store a command Burrow never runs: %w", p, ErrInvalid)
	}
	names := make([]string, 0, len(hookPhases))
	for _, known := range hookPhases {
		names = append(names, string(known))
	}
	return fmt.Errorf("unknown phase %q (want %s): %w", p, strings.Join(names, " or "), ErrInvalid)
}

// Hook is one configured lifecycle command: the phase it fires at and the command it runs, for one
// app in one environment. Command is an argv, so a command's argument boundaries survive storage
// rather than depending on a shell to re-split them.
type Hook struct {
	App         string    `json:"app"`
	Environment string    `json:"environment"`
	Phase       HookPhase `json:"phase"`
	Command     []string  `json:"command"`
}

// hookJobTTLSeconds is how long a finished hook Job lingers before Kubernetes' TTL-after-finished
// controller reaps it: one hour, the same window `burrow app run` applies (ADR-0048 §7). ADR-0072 §3
// requires the Job of a FAILED hook to be left for diagnosis, and the TTL is fixed when the Job is
// created — before the outcome is known — so both outcomes take the window the failure needs.
const hookJobTTLSeconds int32 = 3600

// hookOutputLimit bounds how much of a failed hook's captured output rides in the error. The output
// is the user's own command's, returned to the caller who asked for the deploy exactly as
// `burrow app run` returns it; the bound is there so one runaway command cannot turn an error
// message into a megabyte. The audit row carries NONE of it (see Summary).
const hookOutputLimit = 8 << 10

// HookError reports that a lifecycle hook did not succeed, and therefore that the operation it ran
// ahead of did not happen (ADR-0072 §3). It is a structured outcome, not a system failure: the
// request was understood, the guardrail allowed it, and the user's own command said no. Callers
// distinguish it with AsHook.
//
// It carries the command's captured output so the failure is diagnosable from the response rather
// than from a hunt through the cluster — the point of §3's "reported as the deploy's failure, with
// the command's output". Summary is the same failure WITHOUT the output, and it is what reaches the
// audit log: a stored row must carry the phase, the command, and the exit code, and never whatever
// the command happened to print.
type HookError struct {
	App     string
	Env     string
	Phase   HookPhase
	Command []string
	// Image is the image the hook ran from: the one being deployed for a pre-deploy hook, the one
	// being left for a pre-rollback hook (ADR-0072 §2, §8).
	Image string
	// ExitCode is the command's exit code when it ran and failed. Zero when the command never
	// produced one (Cause is set instead).
	ExitCode int
	// Output is the command's captured combined output, bounded by hookOutputLimit.
	Output string
	// TimedOut reports the hook did not finish inside the run window and Burrow stopped waiting.
	TimedOut bool
	// Cause is the launch, poll, or timeout failure when the hook never ran to an exit code. Nil
	// when the command ran and exited non-zero, which is the ordinary failure.
	Cause error
}

// Summary is the failure without the command's output: the phase, the command, the image, and why
// it failed. It is what the audit log and any log line record, so a row can never carry a value the
// user's command printed.
func (e *HookError) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s hook failed for %s in %s: %q (image %s)", e.Phase, e.App, e.Env, strings.Join(e.Command, " "), e.Image)
	switch {
	case e.TimedOut:
		b.WriteString(": the command did not finish inside the run window")
	case e.Cause != nil:
		fmt.Fprintf(&b, ": %v", e.Cause)
	default:
		fmt.Fprintf(&b, ": exit code %d", e.ExitCode)
	}
	return b.String()
}

// Error is the summary plus the command's captured output and what Burrow did about it — the whole
// answer the caller who asked for the deploy needs.
func (e *HookError) Error() string {
	var b strings.Builder
	b.WriteString(e.Summary())
	fmt.Fprintf(&b, ". The %s did not happen and the running version is untouched. The Job is left for diagnosis.", e.operation())
	if e.Output != "" {
		b.WriteString("\noutput (combined stdout+stderr):\n")
		b.WriteString(e.Output)
	}
	return b.String()
}

// operation names what the hook's failure aborted, so the message says what did not happen rather
// than only what failed.
func (e *HookError) operation() string {
	if e.Phase == HookPreRollback {
		return "rollback"
	}
	return "deploy"
}

// Unwrap exposes the launch or timeout failure so errors.Is still reaches a sentinel underneath it.
func (e *HookError) Unwrap() error { return e.Cause }

// AsHook reports whether err is (or wraps) a HookError and returns it, mirroring AsGuardrail so a
// front end (the HTTP API, and through it the `burrow` CLI and `burrow-agent`) can surface the
// structured failure without parsing prose.
func AsHook(err error) (*HookError, bool) {
	var h *HookError
	if errors.As(err, &h) {
		return h, true
	}
	return nil, false
}

// auditableHookError reduces a hook failure to the error the AUDIT ROW records: the summary, which
// names the phase, the command, the image and the exit code and carries none of the command's
// output. Any other error passes through unchanged.
func auditableHookError(err error) error {
	if h, ok := AsHook(err); ok {
		return errors.New(h.Summary())
	}
	return err
}

// Hooks returns the lifecycle hooks configured for app in env, in phase order (ADR-0072 §1). A
// phase with no hook is absent from the result rather than present and empty: unset means no hook
// and today's behaviour exactly.
func (e *Engine) Hooks(ctx context.Context, app, env string) ([]Hook, error) {
	if err := (App{Name: app}).Validate(); err != nil {
		return nil, fmt.Errorf("hooks: %w: %w", ErrInvalid, err)
	}
	if _, err := e.resolveNamespace(ctx, env); err != nil {
		return nil, fmt.Errorf("hooks %s: %w", app, err)
	}
	hooks, err := e.db.AppHooks(ctx, app, envName(env))
	if err != nil {
		return nil, fmt.Errorf("hooks %s: %w", app, err)
	}
	order := make(map[HookPhase]int, len(hookPhases))
	for i, p := range hookPhases {
		order[p] = i
	}
	sort.SliceStable(hooks, func(i, j int) bool { return order[hooks[i].Phase] < order[hooks[j].Phase] })
	return hooks, nil
}

// SetHook configures the command app runs at phase in env, replacing any command already set there
// (ADR-0072 §1). It is one mechanism with the phase NAMED rather than a command per phase, so a
// fourth phase is a new value and not a new verb.
//
// The blast radius is worth stating where it is set: a pre-deploy hook is configuration set once and
// forgotten, and a command that starts failing blocks every deploy of that app until someone notices
// (ADR-0072's consequences). That is the correct failure direction and it is still a new way for an
// app to become undeployable.
func (e *Engine) SetHook(ctx context.Context, app, env string, phase HookPhase, command []string) (Hook, error) {
	if err := (App{Name: app}).Validate(); err != nil {
		return Hook{}, fmt.Errorf("set hook: %w: %w", ErrInvalid, err)
	}
	if err := validateHookPhase(phase); err != nil {
		return Hook{}, fmt.Errorf("set hook %s: %w", app, err)
	}
	if err := validateHookCommand(command); err != nil {
		return Hook{}, fmt.Errorf("set %s hook %s: %w", phase, app, err)
	}
	if _, err := e.resolveMutatingNamespace(ctx, env); err != nil {
		return Hook{}, fmt.Errorf("set %s hook %s: %w", phase, app, err)
	}
	if err := e.db.SetAppHook(ctx, app, envName(env), phase, command); err != nil {
		return Hook{}, fmt.Errorf("set %s hook %s: %w", phase, app, err)
	}
	return Hook{App: app, Environment: envName(env), Phase: phase, Command: command}, nil
}

// UnsetHook removes app's hook at phase in env. Unsetting a phase with no hook is a no-op, not an
// error — the state it asks for is the state that already holds — and afterwards that phase runs
// nothing, which is today's behaviour exactly (ADR-0072 §1).
func (e *Engine) UnsetHook(ctx context.Context, app, env string, phase HookPhase) error {
	if err := (App{Name: app}).Validate(); err != nil {
		return fmt.Errorf("unset hook: %w: %w", ErrInvalid, err)
	}
	if err := validateHookPhase(phase); err != nil {
		return fmt.Errorf("unset hook %s: %w", app, err)
	}
	if _, err := e.resolveMutatingNamespace(ctx, env); err != nil {
		return fmt.Errorf("unset %s hook %s: %w", phase, app, err)
	}
	if err := e.db.UnsetAppHook(ctx, app, envName(env), phase); err != nil {
		return fmt.Errorf("unset %s hook %s: %w", phase, app, err)
	}
	return nil
}

// validateHookCommand rejects a command that could not run. A hook is an argv, not a shell line: an
// empty argv has nothing to execute, and an empty first element names no program.
func validateHookCommand(command []string) error {
	if len(command) == 0 {
		return fmt.Errorf("command is empty: %w", ErrInvalid)
	}
	if strings.TrimSpace(command[0]) == "" {
		return fmt.Errorf("command names no program: %w", ErrInvalid)
	}
	return nil
}

// runHook runs app's hook for phase, if one is set, from image — the image being deployed for a
// pre-deploy hook and the image being left for a pre-rollback one (ADR-0072 §2, §8). It returns nil
// when no hook is set (today's behaviour exactly) and when the command succeeded, and a *HookError
// when it did not, which the caller turns into an aborted deploy or rollback (§3).
//
// The command runs as a one-shot Job in the app's namespace, from the app's own image, with the
// app's config env and per-app Secret injected exactly as `burrow app run` does (ADR-0048 §2) — so a
// migration sees DATABASE_URL and every other secret as the running app sees them. No secret VALUE
// passes through here: cfg is the non-secret config store, and the Secret is attached by the kube
// seam via envFrom.
//
// Hooks for one app and environment are SERIALIZED (§9). Two pushes in quick succession must not run
// two migration Jobs against one database, so a deploy waits for the previous one's hook rather than
// racing it. The wait respects ctx: a caller that gives up stops waiting rather than queueing behind
// a hook nobody is watching.
func (e *Engine) runHook(ctx context.Context, k Kubernetes, phase HookPhase, app, env, image string, cfg map[string]string) error {
	command, err := e.db.AppHook(ctx, app, envName(env), phase)
	if err != nil {
		return fmt.Errorf("reading the %s hook: %w", phase, err)
	}
	if len(command) == 0 {
		return nil // unset means no hook and today's behaviour exactly (ADR-0072 §1)
	}
	release, err := e.hookLock.acquire(ctx, app, envName(env))
	if err != nil {
		return fmt.Errorf("waiting for the previous %s hook of %s in %s: %w", phase, app, envName(env), err)
	}
	defer release()

	// The audit args carry the phase, the command, the image, the environment and the env KEY NAMES —
	// never a value (ADR-0027). The command is the salient fact a reviewer reads.
	args := map[string]string{
		"phase":    string(phase),
		"command":  strings.Join(command, " "),
		"image":    image,
		"env":      envName(env),
		"env_keys": auditKeys(cfg),
	}
	// The Job's name derives from this ID, so leading with the phase makes a hook Job identifiable as
	// one in `kubectl get jobs` without a second lookup.
	res, runErr := k.RunJob(ctx, RunSpec{
		App:        app,
		ID:         string(phase) + "-" + e.ids.NewID(),
		Image:      image,
		Command:    command,
		Env:        cfg,
		TTLSeconds: hookJobTTLSeconds,
	})
	if runErr == nil && res.ExitCode == 0 {
		e.recordExecution(ctx, auditOpHook, app, args, nil)
		return nil
	}
	he := &HookError{App: app, Env: envName(env), Phase: phase, Command: command, Image: image}
	if runErr != nil {
		// A launch, poll, or timeout failure: the command never produced an exit code. RunJob reports a
		// deadline it stopped waiting on with TimedOut, which reads differently from a pod that could
		// not start, so keep the two apart rather than calling both "failed".
		he.Cause, he.TimedOut = runErr, res.TimedOut
	} else {
		he.ExitCode = res.ExitCode
		he.Output = boundedOutput(res.Stdout + res.Stderr)
	}
	e.recordExecution(ctx, auditOpHook, app, args, errors.New(he.Summary()))
	return he
}

// boundedOutput trims a captured output to hookOutputLimit, keeping the TAIL: a failing command's
// last lines are where its error is, and the head is usually its banner.
func boundedOutput(out string) string {
	if len(out) <= hookOutputLimit {
		return out
	}
	return "[earlier output truncated]\n" + out[len(out)-hookOutputLimit:]
}

// hookLock serializes the hooks of one (app, environment) pair (ADR-0072 §9). Two pushes in quick
// succession must not run two migration Jobs against one database, and the same lock is what a
// database promotion or a maintenance operation would take, which is why it is keyed per app and
// environment rather than per deploy.
//
// It is an IN-PROCESS lock, and that is honest rather than accidental: burrowd runs as a single
// replica (its install manifest sets `replicas: 1`), so one process holds every deploy path. A
// multi-writer control plane would need this to become a Postgres advisory lock, and the seam for
// that is this one type.
type hookLock struct {
	mu      sync.Mutex
	holders map[string]*hookHolder
}

// hookHolder is one key's lock: a one-slot channel, so a waiter can select on ctx instead of
// blocking forever, plus the count of goroutines interested in it, so a key that nobody is using is
// dropped rather than accumulating one entry per app that ever deployed.
type hookHolder struct {
	ch   chan struct{}
	refs int
}

func newHookLock() *hookLock { return &hookLock{holders: make(map[string]*hookHolder)} }

// acquire blocks until nothing else holds the lock for (app, env), then returns the release. It
// returns ctx's error if the caller gives up first, having taken nothing.
func (l *hookLock) acquire(ctx context.Context, app, env string) (func(), error) {
	key := app + "\x00" + env
	l.mu.Lock()
	h, ok := l.holders[key]
	if !ok {
		h = &hookHolder{ch: make(chan struct{}, 1)}
		l.holders[key] = h
	}
	h.refs++
	l.mu.Unlock()

	drop := func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		h.refs--
		if h.refs == 0 {
			delete(l.holders, key)
		}
	}
	select {
	case h.ch <- struct{}{}:
		return func() { <-h.ch; drop() }, nil
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	}
}
