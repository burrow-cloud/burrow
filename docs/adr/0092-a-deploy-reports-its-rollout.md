# ADR-0092: A deploy reports its rollout, not its submission

## Status

✅ Accepted

## TL;DR

Deploy answered when the API server took the Deployment. Now it waits and says what happened.

- Rollout observation is no longer only the hook's. Every deploy makes it, once, shared with the
  `post-deploy` hook and the dependency check.
- Three outcomes, three reports: replicas ready — the line it always printed; bound ran out, still
  progressing — say so, name the bound; will not progress — name the pod's own reason.
- A rollout that did not become ready exits non-zero and never prints `deployed` or `superseded`.
  Superseded is the word that stops someone looking at the old pod.
- The report carries the diagnosis `burrow app status` already had, plus — for a pod that is up and
  failing its readiness check, where nothing else classifies it — what that pod was printing. That
  text never reaches a hook's environment.
- `--wait=false` still answers at submission, and then reports the outcome as unknown, not as good.
- Release row keeps `status` (what Burrow applied, what rollback walks back from) and gains `rollout`
  beside it (what the cluster did with it).

Extends [ADR-0072](0072-deploy-and-run-lifecycle-hooks.md) §4-§5, whose bounded wait this promotes
from a hook feature to the deploy's own report. Speaks [ADR-0074](0074-burrow-observes-what-it-manages.md)
§2's closed reason vocabulary and coins nothing. Applies [ADR-0009](0009-honest-status.md)'s
discipline to what a command claims it did. Supersedes nothing.

## Context

`burrow app deploy` answered as soon as the cluster accepted the write. It printed that a release was
deployed, that the previous one was superseded, and exited 0 — at the moment the API server took the
Deployment object, before a single pod of the new release had started.

Rolling `burrowd-cloud` from `v0.1.52` to `v0.1.60` on a live install produced exactly that
(issue #546). The new pod could not list the `Tenant` resource, so it failed its readiness probe and
Kubernetes kept the old ReplicaSet serving — correct behaviour, and five minutes later the old pod
was still answering every request while the new one sat `0/1 Running`. The deploy had already
reported success.

The output was not merely optimistic; it was specific and wrong twice. It named a release as
`deployed` and it named the previous release as `superseded`. The second is the more damaging,
because it is the sentence that stops a reader going to look at the old pod.

Three things make this worse than a cosmetic message:

- **`app deploy` is on the agent surface.** A human eventually sees a pod list. An agent has the
  sentence and nothing else, and every step it takes afterwards reasons from an image that is not
  serving.
- **A silent partial rollout is indistinguishable from a good one.** At one replica this is a deploy
  that never happened; at several it is a rollout wedged part-way, with some traffic on each image.
  The report said the same thing either way.
- **The explanation was already in reach.** `burrow app status` ranks an app's pods down to the
  blocking condition that names the fix, and the failure ledger records it cluster-wide. The deploy
  simply never looked.

The wait itself was not missing. [ADR-0072](0072-deploy-and-run-lifecycle-hooks.md) §4-§5 already
built a bounded rollout observation with a verdict, a reason from
[ADR-0074](0074-burrow-observes-what-it-manages.md) §2's closed set, and an operator-set bound
(`deploy.settle_timeout`, five minutes by default, thirty at most). It was built to tell a
`post-deploy` hook how the rollout went, and it was deliberately lazy: an app with no hook and no
derived dependency to check made no observation at all, so a deploy that nobody had configured to be
told about never looked at its own rollout. The machinery was right; the deploy was not one of its
consumers.

## Decision

### 1. Every deploy waits for its rollout

The deploy takes the rollout observation itself, immediately after the workload is applied and before
it records or reports anything. It is the same single observation
[ADR-0072](0072-deploy-and-run-lifecycle-hooks.md) §4's `post-deploy` hook and
[ADR-0076](0076-health-checks-readiness-only-and-dependencies-at-deploy-time.md) §4's dependency check share, so a deploy still
spends the settle bound at most once however many parties want to know how it went, and the report,
the check, and the hook cannot say different things about one rollout.

The bound is `deploy.settle_timeout` — the knob an operator already has, not a second one. It is
generous by construction: the wait ends early on any blocking condition, so the full bound is only
ever spent by a rollout that is genuinely still going, and an app that legitimately takes longer than
five minutes to pull and start is an operator setting the bound higher rather than a deploy that
fails. The report names that setting when the bound is what ended the wait.

A workload with no desired replicas has nothing to roll out, so it settles rather than waiting out
the bound. "No replicas became ready" is the correct outcome for an app that is asleep, not a
failure.

### 2. The report describes the outcome, and so does the exit code

A deploy reports which of three things happened, and reports each differently:

1. **The new replicas became ready inside the bound.** The line the CLI has always printed, which is
   now a line that was checked.
2. **The bound ran out with the rollout still progressing.** The report says that, names the setting
   that raises the bound, says Kubernetes is still rolling it out, and exits non-zero.
3. **The rollout will not progress** — a crash loop, an image that will not pull, a pod no node can
   run, an exceeded progress deadline. The report names the reason, from
   [ADR-0074](0074-burrow-observes-what-it-manages.md) §2's closed set, and exits non-zero.

Neither negative report uses the words `deployed` or `superseded`. Both say plainly that the previous
release may still be serving and that nothing was rolled back — Burrow does not roll back by itself
([ADR-0072](0072-deploy-and-run-lifecycle-hooks.md) §6) — and both point at `burrow app status` and
`burrow app logs`.

A negative outcome **carries the diagnosis, not only the verdict**. The result carries the live
status surface's own explanation of the same wedged workload: the registry that could not be reached,
the taint no node tolerates, the container's exit code and a tail of its last log. There is no second
inspection and no second vocabulary — it is the answer `burrow app status` would give for that pod at
that moment, returned by the deploy so a failed deploy explains itself without a second call.

One case has no such explanation, and it is the case that produced this record: a pod that is up,
Running, and failing its readiness check reports no blocking condition, so nothing classifies it and
the wait can say only that it waited. For that pod the observation says it is running and *not
ready*, and captures a bounded tail of what it was printing — which in the live failure was the
forbidden `tenants` list, and would have pointed straight at the cause.

That capture is the application's own output, so it goes to the caller and nowhere else. The values a
`post-deploy` hook is told are copied into a Job's environment, where they persist in a Kubernetes
object readable by anyone with access to the namespace ([ADR-0074](0074-burrow-observes-what-it-manages.md)
§9); the hook is told the reason and the replica counts, exactly as before. The captured output
travels back over the API to the caller that is waiting on the answer, is rendered once, and is
discarded — the contract `burrow app status` already reads under.

The exit code carries the same verdict as the prose. `--json` keeps its stdout contract and carries
the observation structurally, so an agent branches on a field rather than on a sentence.

### 3. Waiting is the default; not waiting is an explicit choice that reports an unknown outcome

A caller may still have the answer at submission, with `burrow app deploy --wait=false`. What it gets
back then is an **unknown** outcome: the report says the rollout was not waited for and that whether
the new pods are serving is not known. It does not fall back to the old sentence, which asserted an
outcome nobody had observed.

The default is to wait, and the request's zero value is that default, so a caller that predates the
option — an older CLI, a script posting the old wire shape — gets the wait. Declining to wait does
not skip a wait something else needs: an app with a `post-deploy` hook or a derived dependency still
settles, because those features are defined in terms of the rollout's outcome.

The opt-out is on the human CLI and not on `burrow-agent`, for the reason
[ADR-0080](0080-a-rollback-is-not-blocked-by-its-own-hook.md) §3 keeps `--skip-hooks` off it: an
agent's whole problem here is being told an operation worked when it did not, and a flag that
restores that is not one the agent should be able to reach for.

### 4. The record keeps its status and gains what was observed

A release row records **both** facts, in two fields, because one field cannot hold them:

- `status` stays what it has always been: the registry's record of which release Burrow applied, and
  which one a rollback returns to. A wedged release is still `deployed`.
- `rollout` records what the deploy observed — settled, unsettled with the reason, or nothing at all
  where the deploy did not wait.

Demoting a wedged release to `failed` was the obvious move and is the wrong one. `burrow app rollback`
takes the newest `deployed` release and re-applies the release *that* one superseded; if a wedged
release stops being `deployed`, the handle moves one release further back, and `rollback` lands an
image nobody asked for at the moment somebody is recovering from a bad deploy. Marking it `failed`
*and* leaving the previous release unsuperseded breaks the chain the same way; marking it `failed`
while superseding the previous one leaves no `deployed` release at all, and rollback stops working
entirely.

The recorded rollout is an **observation, not a live fact**, so it does not go stale: it says what the
deploy saw when it stopped waiting, which stays true however the rollout ends afterwards. What is
true now is `burrow app status`, which reads the cluster. `burrow app history` renders the two
together — a release whose rollout did not come up reads as `deployed (not ready: CrashLoopBackOff)`
— and the audit trail records the same pair, so a reviewer reading it afterwards is told whether the
image a row is about ever served.

## Consequences

- A deploy takes as long as its rollout takes, where it used to return in the time one API call
  takes. That is the point, and it is bounded: `deploy.settle_timeout`, already inside the deploy
  budget every client derives its own timeout from, so no client bound changes.
- A rollout that does not become ready now fails a script, a pipeline, or an agent step that used to
  pass. That is a behaviour change for every caller and is the intended one — the alternative is the
  step passing on an image that is not serving.
- A deploy of a legitimately slow app can be reported as not-yet-ready when it is merely slow. The
  report distinguishes that case from a wedged one and names the setting that raises the bound, and
  raising it is an operator action rather than a code change.
- The deploy makes one extra read of the status surface, and one bounded log read, only when a
  rollout did not settle. A successful deploy costs neither.
- Two fields now describe a release where one did, and a reader has to know which answers which. The
  history surface renders them together so the distinction is visible where it matters rather than
  only in the schema.
- `burrow app rollback` has the same gap this record closes for `deploy`: it applies an older image
  and reports success without observing whether that image came up. It already runs the same settle
  for its `post-deploy` hook, so the change is the same shape and is not made here — one record, one
  concern.

## Rejected alternatives

- **Fix the wording and keep answering at submission.** Softening "deployed" to "submitted" would
  stop the sentence being false, and would leave the caller with nothing to act on: the agent still
  has to make a second call to find out, and the case this record exists for is the one where it does
  not know to.
- **Report the rollout as a hint on a successful result.** Hints are non-blocking notes on a deploy
  that worked. A rollout that never became ready is not a note on a working deploy, and an exit code
  of 0 is the part that misleads a program.
- **Record a wedged release as `failed`.** It reads as the honest choice and breaks the recovery path:
  rollback selects by `status`, so the word has to keep naming the release Burrow last applied. See
  §4.
- **Add a release status for "applied but not serving".** A fifth status has the same problem from the
  other side — every consumer that branches on `status` would have to learn it, and rollback would
  have to accept it as a rollback baseline anyway, which is to say it would have to mean `deployed`.
- **Make the wait opt-in.** It would leave the default exactly as it is, which is the defect, and the
  people who most need the wait are the ones who do not know to ask for it.
- **Coin a reason for "running but not ready".** [ADR-0074](0074-burrow-observes-what-it-manages.md) §2's set
  is closed, and adding a member changes what the status surface, the app listing, and the failure
  ledger all report. That may well be the right change; it is a decision about the failure vocabulary
  rather than about what a deploy reports, and it does not belong in this record.
- **Have Burrow roll back a rollout that did not settle.** Rejected by
  [ADR-0072](0072-deploy-and-run-lifecycle-hooks.md) §6 and not reopened here: the remedy for a failed
  deploy is a judgement about blast radius and data. Burrow reports; the operator, or the hook,
  decides.
