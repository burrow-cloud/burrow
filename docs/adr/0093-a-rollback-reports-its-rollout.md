# ADR-0093: A rollback reports its rollout, and says what the way out is

## Status

🟡 Proposed

## TL;DR

Rollback answered at submission too. Now it waits, and a rollback that does not come up says so.

- Same wait, one observation, shared with the `post-deploy` hook. No second bound.
- Three outcomes, three reports. Neither negative one says `rolled back` or `superseded`.
- The release still serving is the **broken one** the operator was fleeing. The report names it.
- Rolling back again returns to that same release, not further back. The report says that too, and
  points at picking a release from history instead.
- The row stays `deployed` with the observation beside it. Demote it and the app has no release for
  the next rollback to walk back from.

Extends [ADR-0092](0092-a-deploy-reports-its-rollout.md), whose closing consequence names this gap,
to the operation it deliberately left alone. Speaks
[ADR-0074](0074-burrow-observes-what-it-manages.md) §2's closed reason vocabulary and coins nothing.
Supersedes nothing.

## Context

`burrow app rollback` answered as soon as the cluster accepted the write. It applied the older
image and printed one sentence — the app was rolled back to a release, and the release it replaced
was superseded — with exit 0, before a single pod of the restored image had started.

The wait was not missing. [ADR-0072](0072-deploy-and-run-lifecycle-hooks.md) §4-§5's bounded
observation is reached from the rollback path already, and
[ADR-0092](0092-a-deploy-reports-its-rollout.md) §1 made the *deploy* a consumer of it. On the
rollback path it stayed where it began: created at the very end, for the `post-deploy` hook, and only
when a hook had been configured. An app with no hook rolled back without ever looking at what it had
rolled back to.

This is [issue #546](https://github.com/burrow-cloud/burrow/issues/546) with the stakes moved. Three
things make the same defect worse here:

- **The release named as `superseded` is the one the operator is running away from.** Kubernetes
  keeps the previous ReplicaSet serving until the new pods are ready, so a rollback whose restored
  image never becomes ready leaves the *broken* release answering every request — and the report says
  it has been superseded, which is the sentence that stops anyone going to look at it.
- **The operator is mid-incident.** A deploy that does not come up leaves the known-good version
  serving; the report is wrong and the situation is safe. A rollback that does not come up leaves
  nothing fixed, and the person reading the report has already decided the current state is an
  emergency.
- **The obvious next move is wrong, and nothing says so.** `rollback` walks back from the newest
  `deployed` release and re-applies the release *that* one supersedes. After a failed rollback, the
  newest `deployed` release is the failed rollback itself — so a second `burrow app rollback` returns
  to exactly the release the first one was fleeing. The chain is correct; the operator's instinct is
  not, and the report is where that has to be said.

## Decision

### 1. Every rollback waits for its rollout

The rollback takes the rollout observation itself, immediately after the workload is applied and
before it records or reports anything. It is the same `sync.OnceValue` the `post-deploy` hook takes,
hoisted above the record rather than created after it, so a rollback spends the settle bound at most
once however many parties want to know how it went (issue #407) and the report and the hook cannot
say different things about one rollout.

The bound is `deploy.settle_timeout` — the same operator-set knob a deploy waits on
([ADR-0072](0072-deploy-and-run-lifecycle-hooks.md) §5), not a second one for a second operation.
A rollback runs no dependency check, so the hook is the only other consumer.

### 2. The report describes the outcome, and so does the exit code

A rollback reports which of three things happened, and reports each differently — the shape
[ADR-0092](0092-a-deploy-reports-its-rollout.md) §2 gives a deploy:

1. **The restored replicas became ready inside the bound.** The line rollback has always printed,
   now printed only when it is true.
2. **The bound ran out with the rollout still progressing.** The report says that, names the setting
   that raises the bound, and exits non-zero.
3. **The rollout will not progress.** The report names the reason from
   [ADR-0074](0074-burrow-observes-what-it-manages.md) §2's closed set, carries the live status
   surface's own explanation of the same workload — and, for a pod that is up and failing its
   readiness check, a bounded tail of what it was printing — and exits non-zero. That capture reaches
   the caller and never a hook's environment, where
   [ADR-0074](0074-burrow-observes-what-it-manages.md) §9 says it would persist in a Kubernetes
   object.

Neither negative report uses the words `rolled back` or `superseded`. Both carry the two facts that
belong to this operation and to no other:

- **Which release is still serving** — the one being rolled back *away from*, which on this path is
  the release the operator was escaping. Burrow observed the rollout and not the traffic, so the
  report says it *may* still be serving: at one replica it wholly is, above that it partly is.
- **That another rollback is not the way out.** It returns to that same release, for the reason §3
  keeps true. The way out is a release chosen deliberately — `burrow app history`, then a deploy of
  the image it names — and the report says so rather than leaving the reader to reach for the command
  that got them here.

`--wait=false` keeps the answer at submission and reports the outcome as **unknown**, not as good.
The request's zero value is the wait, so a caller that predates the option — an older CLI, a script
posting the route without the parameter — gets the wait; the route reads the opt-out as the literal
string `true` and nothing else, the rule [ADR-0080](0080-a-rollback-is-not-blocked-by-its-own-hook.md)
§1 gives the switches beside it. The opt-out is on the human CLI and absent from `burrow-agent`, for
the reason [ADR-0092](0092-a-deploy-reports-its-rollout.md) §3 keeps the deploy's off it.

The exit code carries the same verdict as the prose, and `--json` carries the observation
structurally so an agent branches on a field rather than on a sentence.

### 3. The rollback's own release row keeps its status, and gains what was observed

The row records both facts, in the two fields migration `00033` already added: `status` stays
`deployed`, and `rollout` records settled, unsettled with the reason, or nothing where the rollback
did not wait. The audit row records the same pair, so a reviewer is told whether the recovery a
rollback row is about ever served.

[ADR-0092](0092-a-deploy-reports-its-rollout.md) §4 gives one reason for keeping `status`: rollback
selects on the word, so demoting a row moves the handle. On this path there is a second and sharper
one. A rollback supersedes the release it replaces as part of landing; marking the rollback's own row
`failed` as well would leave the app with **no `deployed` release at all**, which is the state in
which `burrow app rollback` returns `ErrNotFound` and stops working — reached by the one person who
has just demonstrated they need it. The honest-looking record breaks the recovery path for the
operator who is already recovering.

So the failed rollback stays the rollback baseline, and the consequence is the one §2 makes the CLI
say out loud: the next rollback returns to the release this one was moving away from.

## Consequences

- A rollback takes as long as its rollout takes, bounded by `deploy.settle_timeout` — the bound
  already inside the deploy budget every client derives its timeout from, which a rollback already
  shared, so no client bound changes.
- A rollback whose restored image does not come up now fails a script, a pipeline, or an agent step
  that used to pass. That is the intended behaviour change: the alternative is an incident response
  that reports itself as complete.
- An operator who reads only the first line of a failed rollback learns the rollback did not take.
  One who reads the rest learns that repeating it will not help. That second sentence is the part
  most likely to be skipped, which is why it names the release rather than describing the rule.
- Two operations now share one report vocabulary and two renderings of it. Keeping them in one
  function was rejected below; the cost is that a change to one wording has to be considered for the
  other.
- The rollback still disables auto-deploy when it lands, whatever the rollout then did
  ([ADR-0052](0052-pull-based-image-updates.md) §5). The operator deliberately moved away from the
  newer version; a rollout that has not become ready is not a reason to let the watcher put it back.

## Rejected alternatives

- **Fold the rollback into the deploy's report.** The two outcomes are the same shape and the two
  sentences are not: a failed deploy points at the previous release as a safety net, and a failed
  rollback points at it as the problem still running. One renderer would have to branch on the
  operation at every line that differs, which is most of them.
- **Roll forward automatically when a rollback does not settle.** It is the case for automatic
  recovery at its most tempting and its most dangerous: the image it would return to is the one the
  operator just declared broken, and the `pre-rollback` hook may already have stepped the schema
  back. [ADR-0072](0072-deploy-and-run-lifecycle-hooks.md) §6 is not reopened — Burrow reports, the
  operator decides.
- **Let rollback take a target release, so the way out is another rollback.** It would answer the
  §2 warning by removing the trap, and it is a different decision:
  [ADR-0007](0007-explicit-deploy-by-image-reference.md)'s rollback is exactly one step back, and
  "return to release X" is a deploy of a chosen image, which already exists. Widening the recovery
  verb belongs in its own record.
- **Mark the failed rollback's release `failed`.** See §3: combined with superseding the release it
  replaces, it leaves no rollback baseline at all.
- **Report the failed rollback as an error rather than a result.** An error carries no structure, and
  the rollback did happen — the record is written, the older image is applied, auto-deploy is off. A
  caller needs all of that plus the verdict, which is a result with a non-zero exit code.
- **A `post-rollback` hook phase to carry the outcome.** [ADR-0072](0072-deploy-and-run-lifecycle-hooks.md)
  §4 already rejected a fourth phase for an identical answer, and this record needs the outcome in
  the *caller's* hands, which is not what a hook is.
