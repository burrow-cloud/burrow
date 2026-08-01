# ADR-0080: A rollback is not blocked by its own hook

## Status

✅ Accepted

## TL;DR

A failed `pre-rollback` hook aborts the rollback. That is right when the hook failed because the
schema could not be stepped back, and wrong when it failed because the hook could not run at all —
and today the only way past it deletes the hook.

- **The abort stays.** Letting older code serve against a half-reverted schema is the outcome the
  ordering exists to prevent.
- **`--skip-hooks` is added to `rollback`**, and to nothing else. It is non-destructive: the hook
  stays configured.
- **It is operator-only.** The agent cannot skip a safety step; it reports that a human can.
- **Skipping is recorded**, so "we rolled back around a broken hook" is a fact somebody can find
  later rather than something that happened quietly.

Extends [ADR-0072](0072-deploy-and-run-lifecycle-hooks.md) §8, which decided when `pre-rollback` runs
and was silent on what happens when it fails. Applies [ADR-0065](0065-what-belongs-on-the-agent-surface.md)'s
criterion to the override itself. Supersedes nothing.

## Context

[ADR-0072](0072-deploy-and-run-lifecycle-hooks.md) §3 says a failed `pre-deploy` hook aborts the
deploy. §8 decides that `pre-rollback` runs before traffic moves back, from the image being rolled
back *away from*, so the schema is stepped back before the older code serves — and says nothing about
what happens if it fails. The implementation made it symmetric with §3 and aborts the rollback
(`6c1bd07`), which was the right reading and is not what this record changes.

**The problem is that two different failures arrive as the same abort.**

The hook exits non-zero because the migration revert failed. Aborting is correct: the schema is not
where the older code expects it, and rolling back anyway is precisely the bad outcome.

The hook fails because it could not run. The image will not pull, the Job is unschedulable, a taint
landed on the node pool, the command has a typo, the ten-minute bound elapsed on something wedged.
The schema is fine. The rollback is blocked by something that has nothing to do with it.

The second case arrives at the worst possible moment. **Rollback is the incident escape hatch** — the
thing reached for when a deploy has gone wrong and the fastest correct action is to undo it. A path
that must not have an extra step now has one.

And the step it has is lossy. The only way past today is
`burrow app hook unset <app> --on pre-rollback`, which **deletes the hook**. So recovering costs an
operator their configuration, under pressure, and restoring it afterwards depends on remembering to.
A lossy escape hatch used in an incident is one that gets left in the lossy state, and the next
rollback then runs with no schema protection at all — the failure this arrangement was built to
prevent, arrived at by way of its own escape.

## Decision

### 1. The abort stays

A failed `pre-rollback` hook continues to abort the rollback. Nothing about the default changes.

The reasoning is unchanged and worth restating because the rest of this record only makes sense on
top of it: the hook runs before traffic moves back so the schema is stepped back before the older
code serves it, and a rollback that proceeds past a failed revert puts the previous release against a
schema it does not understand. **Failing closed is correct**, and an operator who has not thought
about it gets the safe behaviour.

### 2. `rollback` gains `--skip-hooks`, and nothing else does

The override exists on `rollback` alone. Not on `deploy`, not as a global.

**Because the urgency is what justifies it.** A deploy can wait: if a `pre-deploy` hook is broken, the
right response is to fix the hook and deploy afterwards, and nothing is on fire while that happens.
A rollback is reached for when something *is* on fire, and the cost of waiting is measured in the
outage it was meant to end. The same flag on `deploy` would be a way to routinely skip migrations,
which is a different feature with none of this reasoning behind it.

**It is non-destructive.** The hook stays configured; one invocation ignores it. That is the whole
point — the existing escape works by deletion, and this one leaves nothing to restore.

### 3. It is operator-only, and the agent says so

`--skip-hooks` is absent from `burrow-agent` (ADR-0065 tier 1) and reported by `guard` as a
capability the agent does not have.

Deciding to skip a safety step during an incident is a judgement about *this situation* — whether the
schema is actually fine, whether the hook's failure is unrelated, what the blast radius of being
wrong is. That is exactly the kind of judgement [ADR-0065](0065-what-belongs-on-the-agent-surface.md)
keeps off the agent surface, and the blast radius of getting it wrong is an application serving
against a schema nobody verified.

**But the agent must be able to say what to run.** ADR-0065 §7 is explicit that an absent capability
which is also *legible* is a refusal the agent can relay, and that a dead end is what pushes an agent
to route around the control channel entirely. So a rollback that aborts on a hook failure returns a
refusal naming the flag and that a human runs it — which is the agent doing its most useful thing in
an incident: telling the person what their options are.

### 4. Skipping is recorded, and the record says which hook

A skipped hook is written to the audit log with the operation: which hook was skipped, on which app
and environment, by whom.

**"We rolled back around a broken hook" is a fact that matters afterwards.** It explains why the
schema is in the state it is in, it is the first thing worth knowing when the next person asks why
production looks odd, and it is exactly the kind of thing nobody writes down at three in the morning.
The audit log already records what Burrow was asked to do and what it decided
([ADR-0069](0069-what-the-audit-log-records.md)); a deliberate bypass of a safety step is squarely
that.

The command's own output says it too, plainly, at the moment it happens. An operator who reaches for
the flag under pressure should not have to infer from silence that it worked.

## Consequences

**The escape hatch stops being lossy**, which was the actual defect. An operator recovers from a
broken hook without destroying configuration, and the next rollback has its schema protection intact.

**A safety step becomes skippable, and that is a real reduction.** Somebody will skip a hook whose
failure *was* the migration revert, and serve old code against a half-reverted schema. §1 keeps the
default safe and §3 keeps the decision with a human, but neither prevents a wrong call made in a
hurry. That is the trade: a bypass that is deliberate, recorded and non-destructive, against one that
is improvised, invisible, and takes the hook with it.

**The two failure modes still look the same at the moment of decision.** Nothing here tells an
operator whether the hook failed because the revert failed or because the Job would not schedule —
they have to read the error and judge. The `HookError` already carries the phase, the command, the
exit code and the bounded output, which is the material for that judgement; making the distinction
*legible* is worth doing and is not decided here.

**One more flag on the incident path.** Every flag on a command reached for in an emergency is a thing
to remember while stressed. This one is justified by the alternative being worse, not by being free.

## Rejected alternatives

**Making a failed `pre-rollback` non-blocking.** The simplest fix: report the failure and roll back
anyway. It removes the need for a flag entirely — and it removes the protection with it, in every
case including the one where the revert genuinely failed. §8 puts the hook before traffic moves for a
reason, and a hook that cannot stop anything is a log line.

**Keeping `hook unset` as the only escape.** The status quo. It works, and it works by deleting the
thing that protects the next rollback, at the moment somebody is least able to notice they have. The
lossiness is the defect; a mechanism that requires destroying configuration to proceed will be found
in the destroyed state later.

**A `--force` flag that skips everything.** Broader and more familiar. It bundles skipping a hook with
whatever else a future `--force` accumulates, which is how flags of that name end up meaning "ignore
all safety" — and this decision is specifically about one safety step whose failure may be unrelated
to the risk it guards.

**Letting the agent skip hooks when it can show the failure was infrastructural.** Tempting, because
the agent often *can* tell an unschedulable Job from a failed migration, and it is faster than waking
someone. It puts the agent in the position of judging when a safety step does not apply, which is the
authority ADR-0065 withholds — and the case where its judgement is wrong is the case where the
schema was genuinely half-reverted.

**A confirmation prompt instead of a flag.** Interactive, forces a pause, reads well. It fails the
case that matters: a rollback run from a script, a runbook, or a CI job has no one to answer the
prompt, and the operator reaching for a rollback at three in the morning may be doing so through
exactly such a path.
