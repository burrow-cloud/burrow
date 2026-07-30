# ADR-0075: A green PR is a tested PR — the merge queue goes, the integration job moves

## Status

✅ Accepted

## TL;DR

`main` is governed by a GitHub merge queue. With two committers who work on non-conflicting PRs,
serializing merges buys close to nothing — and it costs a five-minute floor on every merge, an
eviction failure mode that has consumed real time, and a class of failure that is **invisible from
the pull request**: a queue run can fail and evict a PR while the PR itself still reads
`AWAITING_CHECKS`.

So the queue should go. But it cannot simply be deleted, and the reason is the whole point of this
record.

**The queue is doing two jobs, and only one of them is worthless here.** The `kube-integration` job —
the k3d suite, the only place Burrow is exercised against a real API server — is gated
`github.event_name == 'merge_group'`. **The queue is the only place it ever runs.** Delete the queue
and the integration suite stops running before merge entirely, silently, which is precisely the
failure class this project has spent its recent effort eliminating.

There is a second, quieter dependency. `strict_required_status_checks_policy` is **false**, so a PR
is never required to be current with `main`. The queue is therefore also the only thing that tests
the **merged** result rather than the branch in isolation.

This record removes the queue and **moves `kube-integration` to pull-request checks**, so that a
green PR becomes an actual promise instead of a partial one. It accepts, explicitly, that the
integration suite then runs on every code push rather than once per merge, and that a PR green
against a stale base can break `main` — with two committers and a fix-forward norm, that is a real
regression in guarantee and an acceptable one.

**This became affordable only recently.** Until [#360](https://github.com/burrow-cloud/burrow/pull/360)
the docs-only path filter never worked — `predicate-quantifier` defaulted to `some`, so `'**'`
matched everything and `code` was unconditionally true. Moving the k3d job to PRs before that fix
would have run ten minutes of Kubernetes on every typo.

Applies [ADR-0009](0009-honest-status.md)'s discipline to CI: a check that reports green without
having run is the same dishonesty as a doc describing unbuilt behaviour as done. Supersedes nothing.

## Context

### What exists today

The `protect-main` ruleset on `refs/heads/main` carries `deletion`, `non_fast_forward`,
`pull_request`, `required_linear_history`, `required_signatures`, `required_status_checks` (one
context, `PR checks`) and `merge_queue`. The queue is configured `merge_method: SQUASH`,
`grouping_strategy: ALLGREEN`, `min_entries_to_merge_wait_minutes: 5`.

Verification happens in three tiers, each hiding failures from the one below:

| tier | catches | misses |
| --- | --- | --- |
| `task check` | fmt, vet, build, light unit tests | store tests — they **skip** without `BURROW_TEST_DATABASE_URL` |
| PR checks | store tests, via a Postgres service container | **the k3d integration suite** |
| merge queue | everything | — |

`kube-integration` is gated `needs.changes.outputs.code == 'true' && github.event_name ==
'merge_group'`, with a comment explaining the intent: the heavy job runs once, against the merged
result, rather than twice.

`pr-checks` is an aggregate job that passes when every other job passed **or was legitimately
skipped**, and it is the single required status check. A skipped `kube-integration` on a PR is
therefore indistinguishable, at the gate, from a passing one.

### What breaks

**A green PR is not a promise.** On a pull request the integration suite reports `SKIPPED`, the
aggregate gate treats that as fine, and the PR goes green having never touched a Kubernetes API
server. That is defensible as long as the queue runs it afterwards — but it means the signal
developers and agents read most often is the one that means least, and nothing on the PR says so.

**Queue failures are invisible from the pull request.** A PR sitting at `AWAITING_CHECKS` looks
identical whether the queue is running normally or failing and evicting. Finding out requires
listing runs on `gh-readonly-queue/main/pr-N-*`. This is not a hypothetical inconvenience: it is
written down in the project's own operational notes because it cost hours.

**The queue evicts rather than retries.** A transient k3d failure — the integration job exiting 75,
which it self-describes as *"infra/setup failure, most likely transient: safe to rerun"* — drops the
PR out of the queue permanently. Recovering needs a re-enqueue, and reliably noticing needs a
watcher process per PR. That is real machinery built to service the queue rather than the code.

**Every merge pays a five-minute floor**, from `min_entries_to_merge_wait_minutes`, whatever the
change is.

**And the serialization it exists to provide is not needed.** Two committers, working deliberately on
non-conflicting PRs. The concurrency problem a merge queue solves — several PRs each green alone and
red together — is not the problem this repository has.

### What this record resolves

Whether the merge queue stays, where the integration suite runs, and what a green pull request is
allowed to mean.

## Decision

### 1. The merge queue is removed

The `merge_queue` rule comes off the `protect-main` ruleset. Everything else in that ruleset stays —
`required_signatures`, `required_linear_history`, `pull_request`, `required_status_checks`,
`deletion`, `non_fast_forward`. This removes a merge mechanism, not a protection.

Squash stays the merge method, set at the repository level, so history stays linear.

### 2. `kube-integration` moves to pull-request checks, in the same change

**Not afterwards, and not as a follow-up issue.** The job's `merge_group` gate is the only thing that
makes it run at all, so a change that removes the queue without moving the job deletes the
integration suite from the pipeline while leaving every check green. The two edits are one change or
the repository is worse off than before.

After this, `code == 'true'` is the whole gate: the suite runs on every pull request that touches
code, and docs-only changes skip it as the filter now correctly arranges.

### 3. Branches are not required to be up to date, and `main` is fixed forward

`strict_required_status_checks_policy` stays **false**.

Requiring branches to be current would mean every merge forces a rebase and a full re-run of the
suite, serialized behind whatever landed first. That is a merge queue, hand-rolled, with manual steps
and worse ergonomics — it would reintroduce the cost this record removes while calling it something
else.

The accepted consequence is stated plainly in §"Consequences": a PR green against a stale base can
break `main`. At two committers, with a fix-forward norm already in practice, that is the cheaper
risk. It is the one thing here worth revisiting, and §5 says when.

### 4. `pr-checks` stays the single required check

The aggregate gate is unchanged and remains the only required context. It keeps doing what it does:
passing when every job passed or was legitimately skipped, failing when any failed or was cancelled.
What changes is that "legitimately skipped" no longer covers the integration suite on a code change —
after §2 there is no `merge_group`-only job for it to excuse.

### 5. The trigger to reconsider is concurrency, not team size

A merge queue earns its keep when merges are genuinely concurrent. Two humans on separate PRs are
not that. **Several agent branches racing to land are.** This project already came close: three cloud
issues were nearly built in parallel and were deliberately sequenced instead, because they would have
collided.

So the condition is written down rather than left to memory: **if parallel agent-authored branches
become the normal shape of work, reconsider.** Not "if a third developer joins" — the number of
people is not the variable that matters.

## Consequences

- **A green PR becomes a tested PR**, which it is not today. This is the point, and it is ADR-0009's
  principle applied to CI: a check that reports green without having run is the same dishonesty as a
  doc that describes unbuilt behaviour as done.
- **The integration suite runs on every code push rather than once per merge.** Three pushes to a PR
  pay three times, where the queue paid once. This is the real cost and it is not small — roughly ten
  minutes per run. It is affordable because the docs-only filter now genuinely skips (#360), because
  Actions minutes are free on a public repository, and because the alternative is a signal nobody
  should trust.
- **A stale PR can break `main`.** Two PRs green on the same base, textually non-conflicting,
  semantically incompatible, will merge and break the branch. Today the queue catches that. This
  record trades that guarantee for everything else in it, and the honest framing is that `main`
  becomes *fixed forward* rather than *kept green by construction*.
- **A failing check becomes visible where people look.** No more reading `gh-readonly-queue` runs to
  discover why a PR has not merged.
- **The merge-watcher subagent becomes unnecessary** for its main purpose. Re-kicking evictions was
  machinery that existed to service the queue; with no queue there is nothing to be evicted from. A
  flaky integration run now fails the PR visibly and is re-run from the PR, which is worse
  ergonomically and better epistemically.
- **Merges get roughly five minutes faster**, from dropping the queue's minimum wait alone.
- **k3d flakes now block a PR instead of evicting it.** The same flake, surfaced differently: a red
  check someone must re-run rather than a silent removal someone must notice. That is a downgrade in
  automation and an upgrade in legibility, and given how much of this repository's recent work has
  been about making failures visible, it is the right side of the trade.

## Rejected alternatives

- **Keep the merge queue as it is.** No work, and it does provide a real guarantee — every merge
  tested against the actual merged result. Rejected because the guarantee is paid for by the wrong
  people: the cost lands on every merge and on the operational surface (invisible failures, evictions,
  a watcher process), while the benefit only materialises in the rare case of two semantically
  incompatible PRs that two careful committers were not going to write anyway.
- **Remove the queue and leave `kube-integration` gated on `merge_group`.** The obvious minimal
  change, and it is a trap worth naming explicitly rather than only avoiding: the job's condition
  would never be true again, so the integration suite would silently stop running while every check
  stayed green. It is exactly the shape of the always-true path filter this repository just spent a
  PR fixing, and it is why §2 insists the two edits are one change.
- **Remove the queue and enable `strict` required status checks** so branches must be current.
  Preserves the merged-result guarantee. Rejected in §3: it is a merge queue rebuilt by hand, forcing
  a rebase and a full re-run per merge, serialized, with the automation removed and the cost kept.
- **Run `kube-integration` on a schedule against `main`** instead of per PR. Cheap, and it would
  catch a broken `main` within the day. Rejected because it catches breakage *after* it lands, which
  makes `main` a place bugs are discovered rather than a place they are prevented — and because
  attributing a nightly failure to one of the day's merges is work that a per-PR run does for free.
- **Keep the queue only for PRs that touch code**, docs bypassing it. Reduces the cost without losing
  the guarantee where it matters. Rejected as the worst of both: two merge paths to reason about, the
  invisible-failure problem retained for exactly the changes most likely to hit it, and a conditional
  merge mechanism is harder to explain than either option alone.
- **Reduce `min_entries_to_merge_wait_minutes` to zero and keep everything else.** Addresses the
  most annoying symptom cheaply. Rejected because the five-minute floor is the least of the costs —
  the invisible failures and the eviction machinery are the substance, and neither is tuning.
