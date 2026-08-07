// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
)

// Lifecycle hooks are named for THE MOMENT THEY RUN (ADR-0072 §1). `pre-deploy` runs before a
// deploy's image reaches the cluster; `pre-rollback` runs before a rollback's older image does;
// `post-deploy` runs after the rollout settles, either way, and is told how it went (§4).
// The name answers the only question a reader has — when does my command run — which is why this
// is not called a release command: in every other tool a release is the artifact that records what
// shipped, and borrowing the word would ask the reader to learn a second meaning while answering
// nothing (§1).
//
// THE PRE AND POST PHASES FAIL DIFFERENTLY, AND THAT IS THE WHOLE ASYMMETRY. A failed `pre` hook
// aborts the operation it ran ahead of: nothing has reached the cluster yet, so a migration failure
// becomes a deploy that did not happen rather than one that half-happened (§3). A failed `post` hook
// aborts nothing, because the thing it reports on ALREADY HAPPENED — the image is live and the
// release is recorded before the hook is even consulted. It is reported, audited, and returned as a
// hint on an otherwise successful result. Burrow does not roll back by itself (§6).
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
	// HookPostDeploy runs AFTER THE ROLLOUT SETTLES, from the image that was deployed, and it runs
	// whether the rollout succeeded or failed (ADR-0072 §4). A post hook that fired only on success
	// could not report the case it exists for: the failure an unattended 3am push actually produces
	// is a crashloop, not a failed migration.
	//
	// It fires after a ROLLBACK too, told that the deploy it is reporting on was one. "Did this
	// settle and is it serving?" is the same question whichever direction the image moved, and a
	// separate `post-rollback` phase would be a fourth name for an identical answer.
	//
	// Its failure changes nothing. The deploy already happened, so there is nothing to abort — see
	// runPostDeployHook.
	HookPostDeploy HookPhase = "post-deploy"
	// HookPreRollback runs from the image being rolled back FROM, before traffic moves back
	// (ADR-0072 §8). Unset means nothing runs, which is the safe default: a team practising
	// expand/contract migrates forward only and would be harmed by anything running on a rollback.
	HookPreRollback HookPhase = "pre-rollback"
)

// hookPhases is the closed set of phases a hook may be set on, in the order a listing shows them:
// the order they would fire in around an app's life, not alphabetical.
var hookPhases = []HookPhase{HookPreDeploy, HookPostDeploy, HookPreRollback}

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

// validateHookPhase rejects a phase no hook may be set on.
func validateHookPhase(p HookPhase) error {
	if KnownHookPhase(p) {
		return nil
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
//
// What Burrow did about it depends on the phase, and the two cases are opposite. A `pre` hook's
// failure ABORTED the operation, so the message says the operation did not happen. A `post` hook's
// failure aborted nothing — the deploy it was reporting on was already live and recorded — so the
// message says exactly that, because a reader who saw "the deploy did not happen" after a deploy
// that plainly did would trust the surface less, not more.
//
// A `pre-rollback` failure ends with THE ONE ACTION that gets an operator moving (ADR-0080 §3). It is
// the last line rather than the first because the captured output sits between them and can run to
// eight kilobytes: the recovery instruction has to be the thing still on screen, not the thing that
// scrolled past. Nothing equivalent is printed for `pre-deploy`, and that asymmetry is the decision
// — a deploy can wait for the hook to be fixed, an outage cannot (ADR-0080 §2).
func (e *HookError) Error() string {
	var b strings.Builder
	b.WriteString(e.Summary())
	if e.Phase == HookPostDeploy {
		b.WriteString(". The deploy it was reporting on had already happened and is untouched: a post-deploy hook cannot abort something that already occurred, and Burrow does not roll back by itself. The Job is left for diagnosis.")
	} else {
		fmt.Fprintf(&b, ". The %s did not happen and the running version is untouched. The Job is left for diagnosis.", e.operation())
	}
	if e.Output != "" {
		b.WriteString("\noutput (combined stdout+stderr):\n")
		b.WriteString(e.Output)
	}
	if e.Phase == HookPreRollback {
		b.WriteString("\n")
		b.WriteString(e.rollbackRecovery())
	}
	return b.String()
}

// SkipHooksFlag is how the operator override spells itself wherever a person reads it (ADR-0080 §2):
// in the refusal a blocked rollback returns, and in the note a skipped one carries. It is a constant
// so those two cannot drift from each other — a message naming a flag that does not exist would be
// worse than no message. The operator CLI registers the same option (cobra takes the name without
// the dashes), and `guard` reports it as a capability absent from the agent binary.
const SkipHooksFlag = "--skip-hooks"

// rollbackRecovery is the sentence a blocked rollback ends with: the exact command that gets the
// operator moving, what it costs, and when NOT to reach for it (ADR-0080 §3, §4).
//
// It says three things because an incident leaves room for exactly one read. WHAT TO RUN, with the
// app and the environment already filled in, so nobody reconstructs an invocation under pressure.
// WHAT IT DOES NOT COST — the hook stays configured — because the escape that existed before this
// one worked by deleting the hook, and an operator who has met that will assume this one does too.
// And WHEN IT IS WRONG: the two failures behind a `pre-rollback` abort look identical here, so the
// message names the distinction it cannot make for them rather than implying the flag is always safe.
//
// It also states that the agent cannot supply the flag. That is not an apology — it is what turns an
// absent capability into a refusal the agent can relay to a human (ADR-0065 §7): an agent told only
// "the rollback failed" is an agent that retries, and one told which command a person runs is an
// agent doing the most useful thing available to it mid-incident.
func (e *HookError) rollbackRecovery() string {
	cmd := "burrow app rollback " + e.App
	if e.Env != "" && e.Env != DefaultEnvironment {
		cmd += " --env " + e.Env
	}
	cmd += " " + SkipHooksFlag
	return fmt.Sprintf("To roll back around this hook, a human runs `%s`: it does not run the hook, leaves it configured, and records the skip. "+
		"Reach for it when the hook could not RUN — the image would not pull, the Job would not schedule, the command is wrong — and NOT when the revert itself failed, "+
		"because past a failed revert the older code serves against a schema nobody stepped back. "+
		"%s is on the operator CLI only; burrow-agent cannot supply it (ADR-0080 §3).", cmd, SkipHooksFlag)
}

// skippedHookNote is what a rollback that skipped its `pre-rollback` hook says about it, at the
// moment it happens (ADR-0080 §4). An operator who reaches for the flag under pressure should not
// have to infer from silence that it worked, and the next person to ask why the schema looks the way
// it does should find the answer on the rollback rather than reconstruct it.
//
// It names the command that did not run, because "a hook was skipped" and "`./migrate down` did not
// run" are different facts to somebody deciding what to do next.
func skippedHookNote(app, env string, command []string) string {
	return fmt.Sprintf("the pre-rollback hook of %s in %s was SKIPPED with %s: %q did not run before the rollback. "+
		"The hook is still configured — nothing was unset — and the skip is recorded in the audit log. "+
		"If that command would have stepped the schema back, the release now serving has not had that done for it.",
		app, envName(env), SkipHooksFlag, strings.Join(command, " "))
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
// pre-deploy hook, the image being left for a pre-rollback one, the image now serving for a
// post-deploy one (ADR-0072 §2, §4, §8). It returns nil when no hook is set (today's behaviour
// exactly) and when the command succeeded, and a *HookError when it did not.
//
// WHAT THE CALLER DOES WITH THAT ERROR IS THE PHASE'S BUSINESS, NOT THIS FUNCTION'S. A pre phase
// turns it into an aborted deploy or rollback (§3); the post phase turns it into a hint, because
// what it reports on has already happened (§6).
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
//
// outcome carries what the POST phase is told beyond its own identity — how the rollout went
// (ADR-0072 §4). It is nil for the pre phases, where there is no outcome yet: a variable that was
// meaningful at some phases and not others would be worse than one that is simply absent.
//
// progress is where the hook's stage is reported (issue #480). It is emitted HERE rather than at the
// call site because this is where "is a hook even configured" is known, and a deploy must never
// print a stage for work that did not happen: the emit sits after the unset-means-no-hook return
// below, so an app with no hook reports nothing. hookStage maps the phase, so a phase with no stage
// name — `pre-rollback`, which is not on the deploy path — silently reports nothing.
func (e *Engine) runHook(ctx context.Context, k Kubernetes, phase HookPhase, app, env, image string, cfg map[string]string, outcome map[string]string, progress deployProgress) (err error) {
	command, err := e.db.AppHook(ctx, app, envName(env), phase)
	if err != nil {
		return fmt.Errorf("reading the %s hook: %w", phase, err)
	}
	if len(command) == 0 {
		return nil // unset means no hook and today's behaviour exactly (ADR-0072 §1)
	}
	// From here a hook Job really runs, so the stage is real. A phase off the deploy path reports
	// nothing at all.
	stage, reported := hookStage(phase)
	if reported {
		progress.started(stage)
		defer func() { progress.finish(stage, err == nil) }()
	}
	release, err := e.hookLock.acquire(ctx, app, envName(env))
	if err != nil {
		return fmt.Errorf("waiting for the previous %s hook of %s in %s: %w", phase, app, envName(env), err)
	}
	defer release()

	// The audit args carry the phase, the command, the image, the environment and the env KEY NAMES —
	// never a value (ADR-0027). The command is the salient fact a reviewer reads.
	//
	// A post hook's args also carry the OUTCOME and the REASON it was told. Both are Burrow's own
	// closed-vocabulary strings rather than anything a user's command or a user's application
	// produced, and they are the fact that makes the row worth reading afterwards: "a command ran"
	// and "a command ran because the rollout crash-looped" are different records.
	args := map[string]string{
		"phase":    string(phase),
		"command":  strings.Join(command, " "),
		"image":    image,
		"env":      envName(env),
		"env_keys": auditKeys(cfg),
	}
	for _, name := range []string{hookEnvOutcome, hookEnvReason, hookEnvKind} {
		if v, ok := outcome[name]; ok && v != "" {
			args[strings.ToLower(strings.TrimPrefix(name, "BURROW_"))] = v
		}
	}
	// Every hook is told WHICH MOMENT IT IS RUNNING AT and what it is running against, so one script
	// can serve several phases without having to be told twice — once in the stored argv and once in
	// the code. The post phase adds the outcome on top of it.
	told := map[string]string{
		hookEnvPhase:       string(phase),
		hookEnvApp:         app,
		hookEnvEnvironment: envName(env),
		hookEnvImage:       image,
	}
	for name, v := range outcome {
		told[name] = v
	}
	// The Job's name derives from this ID, so leading with the phase makes a hook Job identifiable as
	// one in `kubectl get jobs` without a second lookup.
	res, runErr := k.RunJob(ctx, RunSpec{
		App:        app,
		ID:         string(phase) + "-" + e.ids.NewID(),
		Image:      image,
		Command:    command,
		Env:        hookEnv(cfg, told),
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

// The environment variables a hook is told its situation through (ADR-0072 §4). They are variables
// rather than arguments because a hook is a stored argv an operator wrote once: appending arguments
// to it would change the command they configured, and a command that takes different arguments
// depending on why it ran is a command that has to be rewritten to take them.
//
// Every one of them is a value BURROW authored — a phase name, an app name, an image reference, a
// member of a closed vocabulary, or a line of replica counts. None is application output and none is
// a secret; the app's own config and Secret still arrive exactly as they do for `burrow app run`,
// through the config env and the per-app Secret's envFrom (ADR-0048 §2).
const (
	// hookEnvPhase is the phase that is running, so one script can serve several phases.
	hookEnvPhase = "BURROW_HOOK_PHASE"
	// hookEnvApp, hookEnvEnvironment and hookEnvImage are what the hook is talking about — the third
	// thing ADR-0072 §4 requires a post hook to receive, so a notification can say which app in which
	// environment on which image rather than "a deploy failed".
	hookEnvApp         = "BURROW_APP"
	hookEnvEnvironment = "BURROW_ENVIRONMENT"
	hookEnvImage       = "BURROW_IMAGE"
	// hookEnvRelease is the release the hook is reporting on, so a hook can link to it or record it.
	hookEnvRelease = "BURROW_RELEASE"
	// hookEnvKind says whether the deploy being reported on was a deploy or a rollback — §4's "it
	// fires after a rollback too, told that it was one".
	hookEnvKind = "BURROW_DEPLOY_KIND"
	// hookEnvOutcome is "succeeded" or "failed" and nothing else (RolloutOutcome.Outcome).
	hookEnvOutcome = "BURROW_DEPLOY_OUTCOME"
	// hookEnvReason is the member of ADR-0074's closed vocabulary naming why it failed, EMPTY on
	// success. It is what lets a hook branch on the cause without parsing prose: a hook that sees
	// CreateContainerConfigError knows a retry is pointless, where a message inviting interpretation
	// would have it retry.
	hookEnvReason = "BURROW_DEPLOY_REASON"
	// hookEnvDetail is one Burrow-authored line of context behind the reason — replica counts, a
	// pod's phase, a container's waiting reason. It is for a human reading a notification; nothing
	// should branch on it.
	hookEnvDetail = "BURROW_DEPLOY_DETAIL"
)

// The values hookEnvKind takes, so a hook branches on a fixed string rather than on prose.
const (
	// DeployKindDeploy is an ordinary deploy, explicit or unattended.
	DeployKindDeploy = "deploy"
	// DeployKindRollback is a rollback: the same question — did this settle and is it serving —
	// asked about an image that moved backwards (ADR-0072 §4).
	DeployKindRollback = "rollback"
)

// hookStage maps a hook phase to the deploy stage it is reported as (issue #480), and reports
// whether it has one at all. `pre-rollback` deliberately has none: it fires on the rollback path,
// which reports no progress.
func hookStage(phase HookPhase) (string, bool) {
	switch phase {
	case HookPreDeploy:
		return StagePreDeployHook, true
	case HookPostDeploy:
		return StagePostDeployHook, true
	default:
		return "", false
	}
}

// hookEnv merges the app's config with what the phase tells the hook, without mutating either.
//
// BURROW'S OWN VARIABLES WIN over an app config key of the same name. That is a deliberate choice
// with a cost: an app that happens to set BURROW_DEPLOY_OUTCOME in its config does not see its own
// value inside a hook Job. The alternative is worse — a hook told "succeeded" by a stale config
// value after a crash-looping rollout is a hook actively lying about the one thing it exists to
// report — and the `BURROW_` prefix is what makes the collision unlikely enough to accept.
// It always COPIES rather than writing into cfg: cfg is the app's config as read from the store, and
// a hook Job's environment is not a place to leave BURROW_ keys behind for the next reader of it.
func hookEnv(cfg, told map[string]string) map[string]string {
	out := make(map[string]string, len(cfg)+len(told))
	for k, v := range cfg {
		out[k] = v
	}
	for k, v := range told {
		out[k] = v
	}
	return out
}

// runPostDeployHook is ADR-0072 §4-§6: wait for the rollout to settle, then run the app's
// `post-deploy` hook and tell it how it went — whether it went well or badly, and after a rollback
// as well as after a deploy.
//
// NOTHING HAPPENS AT ALL WITH NO HOOK SET (§1). The hook is read first and the function returns
// immediately when there is none, so a deploy that nobody asked to be told about does not wait for
// its rollout, does not read the operational configuration, and returns exactly when it did before
// this phase existed. The wait is the cost of the feature and it is paid only by those who asked
// for it.
//
// THE WAIT DOES NOT HOLD THE HOOK LOCK. §9 serializes the hooks of one (app, environment) so two
// migration Jobs never run against one database; it does not ask a five-minute observation to block
// the next deploy's `pre-deploy` hook. The lock is taken by runHook, around the Job and nothing else.
//
// A FAILING HOOK IS NOT AN ERROR HERE, and this is the asymmetry with the pre phases. The deploy has
// landed: the image is live, the release is recorded, and the previous one is superseded. There is
// nothing left to abort, and Burrow does not roll back by itself (§6) — the hook is told what
// happened and decides, including by calling `burrow rollback` itself. So a failure comes back as a
// hint on a successful result, plus the audit row runHook already wrote. Returning it as the
// deploy's error would report a deploy that happened as a deploy that did not.
//
// It returns the hints to attach to the caller's result: what the rollout did, and what the hook did
// about it.
func (e *Engine) runPostDeployHook(ctx context.Context, k Kubernetes, app, env, image, releaseID, kind string, cfg map[string]string, settle rolloutSettle, progress deployProgress) []string {
	command, err := e.db.AppHook(ctx, app, envName(env), HookPostDeploy)
	if err != nil {
		slog.WarnContext(ctx, "reading the post-deploy hook failed; the deploy is unaffected",
			"app", app, "env", envName(env), "error", err)
		return nil
	}
	if len(command) == 0 {
		return nil // unset means no hook and today's behaviour exactly (ADR-0072 §1)
	}

	// The settle is asked for HERE rather than up the call stack, after the no-hook return above, so
	// an app with no hook and no derived dependency still waits for nothing (§1). It is the deploy's
	// one observation: when the dependency check already forced it, this is that same answer and no
	// second wait happens.
	outcome := settle()
	told := map[string]string{
		hookEnvRelease: releaseID,
		hookEnvKind:    kind,
		hookEnvOutcome: outcome.Outcome(),
		// Set even on success, and empty there. A hook running under `set -u` — which a careful
		// migration script does — would abort on an unset variable before it could report anything,
		// so the variable always exists and its emptiness IS the success signal.
		hookEnvReason: outcome.Reason,
		hookEnvDetail: outcome.Detail,
	}

	var hints []string
	if outcome.Failed() {
		hints = append(hints, fmt.Sprintf(
			"the rollout of %s did not settle (%s): %s. The post-deploy hook was told so and decides what to do about it; Burrow does not roll back by itself (ADR-0072 §6)",
			image, outcome.Reason, outcome.Detail))
	}
	if err := e.runHook(ctx, k, HookPostDeploy, app, env, image, cfg, told, progress); err != nil {
		// The hook failed. Its Job is left for diagnosis and its audit row is written; here it becomes
		// a hint, because the operation it reported on is done either way. The SUMMARY is used rather
		// than the full error: the summary carries the phase, the command and the exit code and none
		// of the command's captured output, and a hint travels back over the API into an agent's
		// context (ADR-0027).
		hints = append(hints, postDeployHookHint(err))
	}
	return hints
}

// postDeployHookHint renders a failed post-deploy hook for the result's hints — the summary only,
// never the command's output, and saying plainly that nothing was undone.
func postDeployHookHint(err error) string {
	detail := err.Error()
	if h, ok := AsHook(err); ok {
		detail = h.Summary()
	}
	return detail + ". Nothing was rolled back: a post-deploy hook reports on a deploy that already happened. Its Job is left for an hour so the failure can be inspected."
}

// rolloutSettle hands back THE deploy's rollout observation. Calling it more than once returns the
// observation already made rather than making a second one, so every consumer of it describes the
// same rollout.
type rolloutSettle func() RolloutOutcome

// settleOnce builds the one settle observation a deploy makes, deferred until something asks for it.
//
// TWO THINGS IN THE DEPLOY TAIL WANT IT. The deploy-time dependency check waits for the rollout so
// its exposure check reaches the release just deployed rather than the one it replaced (ADR-0076
// §4), and the `post-deploy` hook is told how the rollout went (ADR-0072 §4). They arrived as
// separate changes, each waiting for itself, so an app with both spent the settle bound TWICE — and
// on a wedged rollout, which is precisely the case a bound exists for, that is two full timeouts
// rather than one. MaxDeployWait had to declare the doubled figure, and every client budget derives
// from that (issue #407).
//
// ONE OBSERVATION IS ALSO THE MORE TRUTHFUL ONE. The wait mutates nothing (ADR-0072 §6) and neither
// does the check between them — its probe runs in a separate Job, under the Job's own name label, so
// it is not even among the pods the wait inspects — which leaves elapsed time as the only thing the
// second observation could have that the first did not. Waiting longer is not an outcome anyone
// configured: `deploy.settle_timeout` is the bound the operator sets, and asking twice quietly
// granted double it to whichever app happened to have both features on. Sharing one observation also
// removes the case where the check and the hook report differently on the same rollout.
//
// IT IS LAZY. An app with neither a derived dependency nor a `post-deploy` hook still waits for
// nothing at all: no consumer calls this, so no observation is made.
//
// THE STAGE IS REPORTED FROM INSIDE THE ONCE-FUNC, for exactly that reason (issue #480). The settle
// is the longest thing a deploy waits for and the one an operator most wants named, but it is also
// the one that often does not happen — and reporting it from the call site would announce a wait
// that laziness then skipped. Emitting here means the stage appears when, and only when, the
// observation is actually made, and once however many consumers ask.
func (e *Engine) settleOnce(ctx context.Context, k Kubernetes, app, env string, progress deployProgress) rolloutSettle {
	return sync.OnceValue(func() RolloutOutcome {
		progress.started(StageSettle)
		out := e.awaitRollout(ctx, k, app, env)
		// A rollout that did not settle is a FAILED stage and still a successful deploy: the image is
		// live and the release is recorded, and Burrow does not roll back by itself (ADR-0072 §6). The
		// mark says the wait ended badly, not that the deploy did.
		progress.finish(StageSettle, !out.Failed())
		return out
	})
}

// awaitRollout waits for app's rollout to settle, bounded by the operational limit ADR-0072 §5 puts
// the bound on rather than compiling it in. A configuration that cannot be read falls back to the
// built-in default exactly as every other limit read does: the deploy has already landed, and
// refusing to tell the hook anything because a settings table was briefly unavailable would be the
// worse failure.
//
// A seam error is not a third outcome. AwaitRollout answers every observable condition as an
// outcome, so an error from it means the call could not be made at all; that is reported as the
// deadline backstop with a detail saying so, because the hook still has to be told something and
// "Burrow could not observe the rollout" is what happened.
//
// It is reached through settleOnce, never called directly: a caller that waits for itself is how the
// deploy came to wait twice.
func (e *Engine) awaitRollout(ctx context.Context, k Kubernetes, app, env string) RolloutOutcome {
	bound, _ := e.operationalConfig(ctx).Duration(env, LimitDeploySettleTimeout)
	out, err := k.AwaitRollout(ctx, app, bound)
	if err != nil {
		slog.WarnContext(ctx, "waiting for the rollout to settle failed; the post-deploy hook is told what was observed",
			"app", app, "env", env, "error", err)
		return RolloutOutcome{
			Reason: ReasonDeadlineExceeded,
			Detail: "Burrow could not observe the rollout, so whether it settled is unknown",
		}
	}
	return out
}

// operationalConfig reads the operator-set limits, degrading to the built-in defaults when the read
// fails. It is the read-side counterpart of ClusterConfigFrom's rule, applied on a path where an
// unreadable settings table must not become a failed operation (ADR-0068 §6).
func (e *Engine) operationalConfig(ctx context.Context) OperationalConfig {
	cfg, err := e.db.OperationalConfig(ctx)
	if err != nil {
		slog.WarnContext(ctx, "reading operational configuration failed; operational limits fall back to their built-in defaults", "error", err)
		return OperationalConfig{}
	}
	return cfg
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
