// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// A lock makes destruction take two deliberate acts (cloud ADR-0060).
//
// A lock is STATE ON A THING — one app, or one add-on instance — and not a policy about a caller.
// Locked, the operations that cannot be undone refuse: deleting the app, removing the add-on
// instance, and detaching from an add-on with --delete-data. Everything else proceeds untouched:
// deploys, rollbacks, scaling, restarts, attaching, config changes and ordinary detaches all undo
// by doing them again, and a lock that interrupted them would be switched off and left off, which
// is how a safety mechanism becomes decoration.
//
// # It is not a guardrail, and the difference is the point
//
// A guardrail asks WHO is calling and answers accordingly (ADR-0006, ADR-0097): it binds the agent
// and can leave a human alone. A lock asks nothing about the caller. It refuses the person who set
// it, from their own terminal, with their own admin kubeconfig — because its purpose is to
// interrupt a MOMENT rather than to bound an authority, and the failure it exists for is a person
// running the right command against the wrong target.
//
// It is therefore NOT A SECURITY CONTROL and nothing here should describe it as one. Anyone with
// write access to the namespace deletes the underlying objects with kubectl and never touches
// Burrow. What a lock buys is that the destructive path THROUGH BURROW takes an act whose only
// purpose is to permit destruction, at a different moment from the destruction itself.
//
// # Neither verb is on the agent surface
//
// `burrow lock` and `burrow unlock` are operator commands. burrow-agent carries neither, and the
// capability catalogue (internal/agentsurface) reports both as absent so an agent can say what
// stands in the way and who removes it. A mechanism whose value is that a second deliberate human
// act is required is worth nothing if one caller can perform both acts in one loop.
//
// The agent can SEE a lock: it rides on `status` and on the app and add-on listings, and a refusal
// NAMES it (LockedError). That is the difference between an agent reporting "this is locked, a
// person unlocks it with this command" and an agent retrying against a refusal that never changes.

// LockSubject names what kind of thing a lock is on. An app and an add-on instance are locked
// independently and are both in scope on purpose: an app holds a workload a deploy can recreate,
// while the instance holds the data (cloud ADR-0060 §1).
type LockSubject string

const (
	// LockSubjectApp is a lock on one app in one environment.
	LockSubjectApp LockSubject = "app"
	// LockSubjectAddonInstance is a lock on one add-on instance, addressed by the label an operator
	// types (ADR-0091 §1) in the environment it serves.
	LockSubjectAddonInstance LockSubject = "addon_instance"
)

// Lock is one lock: what is locked, where, and when it was set. LockedAt comes from the injected
// clock, never ambient time (ADR-0010).
type Lock struct {
	Subject     LockSubject `json:"subject"`
	Name        string      `json:"name"`
	Environment string      `json:"environment"`
	LockedAt    time.Time   `json:"locked_at"`
}

// LockResult is what `lock` and `unlock` report. It states the resulting STATE rather than the
// change, so a caller that locks something already locked reads the same answer as one that locked
// it now — both are "this is locked", which is what the operator asked for.
//
// Changed says whether this call is what changed it, so a person who expected to be the one to
// unlock something can tell that somebody else already had.
type LockResult struct {
	Subject     LockSubject `json:"subject"`
	Name        string      `json:"name"`
	Environment string      `json:"environment"`
	Locked      bool        `json:"locked"`
	// LockedAt is when the lock was set, zero when the subject is not locked.
	LockedAt time.Time `json:"locked_at,omitempty"`
	Changed  bool      `json:"changed"`
}

// LockedError is the refusal a destructive operation returns for a locked subject. It is a distinct
// type rather than a sentinel because the answer has parts a caller acts on: WHAT is locked, WHERE,
// and the exact command that removes the lock.
//
// It is deliberately not a GuardrailError. A guardrail refusal means "policy says this caller may
// not"; re-issuing it with confirm=true, or as a different caller, can succeed. This one means "the
// thing itself is locked", it is the same answer for every caller, and nothing about the request
// changes it — only a separate command against the subject does. Collapsing the two would tell an
// agent that has learned the confirm flow that it can proceed on its own say-so.
type LockedError struct {
	Subject     LockSubject
	Name        string
	Environment string
	// Operation is what was refused, phrased as the act: "deleting this app", "removing this add-on
	// instance". It is in the message so the refusal says what did not happen.
	Operation string
	// Unlock is the exact command a person runs to remove the lock.
	Unlock string
}

// Error renders the refusal.
//
// THE WORDING IS PART OF THE MECHANISM. It has to do two things that a naive "operation refused"
// cannot. It must name the unlock command, because the whole design is that destruction takes a
// second deliberate act and a refusal that does not say what that act is turns the mechanism into a
// support question. And it must not read like a permissions error: a caller told "denied" concludes
// they need more access, asks somebody for it, and learns nothing — where the true answer is that
// the thing is locked, that the lock holds for everybody including them, and that removing it is one
// command they can run themselves.
func (e *LockedError) Error() string {
	return fmt.Sprintf("%s %q in environment %s is locked, so %s is refused. "+
		"The lock is on the %s itself and holds for every caller, including whoever set it. "+
		"Remove it with `%s`, then run this again.",
		e.subjectNoun(), e.Name, e.Environment, e.Operation, e.subjectNoun(), e.Unlock)
}

// subjectNoun is how the refusal names what is locked, in the words an operator uses for it.
func (e *LockedError) subjectNoun() string {
	if e.Subject == LockSubjectAddonInstance {
		return "add-on instance"
	}
	return "app"
}

// AsLocked reports whether err is (or wraps) a lock refusal, and returns it. The API layer uses it
// to answer with the `locked` code, which is what lets a client — and through it an agent — tell a
// lock apart from a guardrail refusal without parsing prose.
func AsLocked(err error) (*LockedError, bool) {
	var l *LockedError
	if errors.As(err, &l) {
		return l, true
	}
	return nil, false
}

// LockApp locks one app in one environment, so deleting it refuses until somebody unlocks it. It is
// idempotent: locking an app that is already locked reports it locked and unchanged, and does not
// move the timestamp — the answer to "since when has this been protected" should not be reset by a
// second person confirming the protection.
//
// THE APP HAS TO EXIST. A lock on a name nobody deploys protects nothing while reading exactly like
// protection, and a typo is the same class of mistake this mechanism exists to interrupt.
func (e *Engine) LockApp(ctx context.Context, app, env string) (LockResult, error) {
	return e.setAppLock(ctx, app, env, true)
}

// UnlockApp removes an app's lock. It is THE deliberate act the mechanism is built around, and it
// takes no confirmation flag: `--confirm` catches a command nobody read, and this command has no
// purpose other than to permit destruction, so a caller typing it has already stated the intent a
// confirmation would ask for. Stacking one on top would make the pair ceremony, and ceremony is what
// gets switched off.
//
// It is audited, and it is the line worth reading (cloud ADR-0060 §6): an unlock with a deletion
// after it is somebody doing what they meant to, while an unlock with nothing after it is a lock
// removed and never restored.
func (e *Engine) UnlockApp(ctx context.Context, app, env string) (LockResult, error) {
	return e.setAppLock(ctx, app, env, false)
}

// setAppLock is the shared body of LockApp and UnlockApp.
func (e *Engine) setAppLock(ctx context.Context, app, env string, locked bool) (LockResult, error) {
	verb := "unlock app"
	if locked {
		verb = "lock app"
	}
	if err := (App{Name: app}).Validate(); err != nil {
		return LockResult{}, fmt.Errorf("%s: %w: %w", verb, ErrInvalid, err)
	}
	name, ns, err := e.resolveMutatingEnvironment(ctx, env)
	if err != nil {
		return LockResult{}, fmt.Errorf("%s %s: %w", verb, app, err)
	}
	exists, err := e.appExists(ctx, e.k8s.WithNamespace(ns), app, name)
	if err != nil {
		return LockResult{}, fmt.Errorf("%s %s: %w", verb, app, err)
	}
	if !exists {
		return LockResult{}, fmt.Errorf("%s %s: unknown app: %w", verb, app, ErrNotFound)
	}
	return e.writeLock(ctx, LockSubjectApp, name, app, locked)
}

// LockAddonInstance locks one add-on instance, so removing it — and detaching an app from it with
// --delete-data — refuses until somebody unlocks it. The instance is resolved the way a removal
// resolves it (ADR-0091 §2): a registry name, else a label in the named environment, so the argument
// that locks an instance is the same argument that would have destroyed it.
func (e *Engine) LockAddonInstance(ctx context.Context, instance, env string) (LockResult, error) {
	return e.setInstanceLock(ctx, instance, env, true)
}

// UnlockAddonInstance removes an add-on instance's lock. Like UnlockApp it takes no confirmation.
func (e *Engine) UnlockAddonInstance(ctx context.Context, instance, env string) (LockResult, error) {
	return e.setInstanceLock(ctx, instance, env, false)
}

// setInstanceLock is the shared body of LockAddonInstance and UnlockAddonInstance.
func (e *Engine) setInstanceLock(ctx context.Context, instance, env string, locked bool) (LockResult, error) {
	verb := "unlock addon"
	if locked {
		verb = "lock addon"
	}
	info, err := e.removalTarget(ctx, instance, env)
	if err != nil {
		return LockResult{}, fmt.Errorf("%s %s: %w", verb, instance, err)
	}
	// The lock is keyed by the instance's LABEL, not by its cluster name, for the same reason a
	// guardrail disposition is (ADR-0091 §4): the label is the half an operator reads and types, and
	// for every instance that existed before labels the two are the same string anyway.
	return e.writeLock(ctx, LockSubjectAddonInstance, envName(info.Environment), instanceLabel(info), locked)
}

// writeLock performs the store write for both subjects and records the audit row. Both acts are
// recorded; neither carries a guardrail decision row, because neither verb has a guardrail — a lock
// is not policy, and there is no disposition anything could be set to.
func (e *Engine) writeLock(ctx context.Context, subject LockSubject, env, name string, locked bool) (LockResult, error) {
	op, what := auditOpUnlock, "unlock"
	if locked {
		op, what = auditOpLock, "lock"
	}
	args := map[string]string{"subject": string(subject), "env": env}
	existing, err := e.db.Lock(ctx, subject, env, name)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return LockResult{}, fmt.Errorf("%s %s: reading the lock: %w", what, name, err)
	}
	was := err == nil
	res := LockResult{Subject: subject, Name: name, Environment: env, Locked: locked, Changed: was != locked}
	if locked {
		res.LockedAt = existing.LockedAt
	}
	if !res.Changed {
		// Nothing to write. The audit row is still worth having for a lock — somebody asserted the
		// protection — but there is no state change to record, so it says so through the args.
		args["changed"] = "false"
		e.recordExecution(ctx, op, name, args, nil)
		return res, nil
	}
	args["changed"] = "true"
	if locked {
		lock := Lock{Subject: subject, Name: name, Environment: env, LockedAt: e.clock.Now()}
		if err := e.db.SetLock(ctx, lock); err != nil {
			e.recordExecution(ctx, op, name, args, err)
			return LockResult{}, fmt.Errorf("lock %s: %w", name, err)
		}
		res.LockedAt = lock.LockedAt
	} else if err := e.db.DeleteLock(ctx, subject, env, name); err != nil {
		e.recordExecution(ctx, op, name, args, err)
		return LockResult{}, fmt.Errorf("unlock %s: %w", name, err)
	}
	e.recordExecution(ctx, op, name, args, nil)
	return res, nil
}

// refuseIfLocked returns a LockedError when the named subject is locked, and nil otherwise. It is
// called on the destructive paths BEFORE the guardrail is evaluated, so a locked subject answers
// "locked" rather than holding for a confirmation that would not have helped: the caller cannot
// satisfy this refusal by re-issuing the same command with --confirm, and telling them to try is
// worse than telling them nothing.
//
// A store that cannot answer is a refusal, not a pass. "I could not read whether this is locked" and
// "this is not locked" are different facts, and reading the first as the second destroys the one
// thing somebody asked Burrow to protect.
func (e *Engine) refuseIfLocked(ctx context.Context, subject LockSubject, env, name, operation, unlock string) error {
	_, err := e.db.Lock(ctx, subject, env, name)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil
	case err != nil:
		return fmt.Errorf("reading whether %q is locked: %w", name, err)
	}
	return &LockedError{Subject: subject, Name: name, Environment: env, Operation: operation, Unlock: unlock}
}

// unlockCommand renders the exact operator command that removes a lock, named in every refusal. It
// is composed here rather than written out at each call site so the four destructive paths cannot
// hand a caller four spellings of one command — and so the string is testable, which a message
// built inline is not.
//
// It always names the environment. `--env prod` is redundant on a single-environment install and
// costs a person nothing; leaving it out costs the person with several environments the one detail
// that decides whether they unlock production or staging.
func unlockCommand(subject LockSubject, name, env string) string {
	if subject == LockSubjectAddonInstance {
		return fmt.Sprintf("burrow unlock addon %s --env %s", name, env)
	}
	return fmt.Sprintf("burrow unlock %s --env %s", name, env)
}

// lockKey is how a set of locks is keyed in memory: by environment AND name. The environment is
// part of the key rather than assumed from the caller's scope because one listing spans several
// environments (the add-ons listing does), and a set keyed by name alone would report an instance in
// staging as locked because production's instance of the same label is.
func lockKey(env, name string) string { return env + "\x00" + name }

// lockedNames reads the locks of one subject kind as a set keyed by lockKey, for the listings that
// report lock state beside everything else they show (cloud ADR-0060 §5). An empty env reads every
// environment's.
//
// It is BEST-EFFORT by contract: a store that will not answer yields no locks rather than an error,
// because a listing is a read that must keep working when one of its enrichments does not, and a
// listing that failed entirely would be a worse answer than one that omits a flag. The destructive
// path never uses this — it uses refuseIfLocked, which fails closed.
func (e *Engine) lockedNames(ctx context.Context, subject LockSubject, env string) map[string]bool {
	locks, err := e.db.Locks(ctx, subject, env)
	if err != nil {
		return nil
	}
	set := make(map[string]bool, len(locks))
	for _, l := range locks {
		set[lockKey(envName(l.Environment), l.Name)] = true
	}
	return set
}
