# ADR-0048: One-off command runner (`burrow_run`)

## Status

✅ Accepted

## TL;DR

Add `burrow_run`: an agent-invoked, synchronous, guarded, one-off command runner. It runs a
caller-provided command from the app's **own current image** (the currently-deployed release) as a
short-lived Kubernetes Job in the app's namespace, with the app's config env vars and its per-app
Secret injected via `envFrom` — so `DATABASE_URL` and every secret appear exactly as the running app
sees them. It waits for completion, captures stdout/stderr and the exit code, returns them as a
structured result, and reaps the Job (leaving it on failure for diagnosis). It is the primitive
behind migrations, seeds, backfills, one-off scripts, and console-style tasks, surfaced as the MCP
tool `burrow_run` and the parity CLI `burrow app run <app> -- <cmd>`.

The explicit call is the spine, mirroring [ADR-0007](0007-explicit-deploy-by-image-reference.md)
(deploy is an explicit call, not an implicit trigger). It reuses the
[ADR-0032](0032-postgres-backups.md) backup-Job machinery and the
[ADR-0028](0028-app-config-and-secrets.md) per-app-Secret `envFrom` injection, is gated by a new
`app.run` guardrail under [ADR-0006](0006-guardrails-in-the-control-plane.md) /
[ADR-0020](0020-guardrails-as-configurable-policy.md), and is recorded in the audit log
([ADR-0027](0027-audit-log.md)). Its data-safety story is honest per
[ADR-0009](0009-honest-status.md): the guardrail gates the operation, not the command's contents.
Supersedes nothing.

## Context

An operator's app is more than a long-running process. Real applications need one-off tasks run
*in* their runtime: database migrations, seed and fixture loads, data backfills, a maintenance
script, a REPL-style diagnostic. Today Burrow can deploy an app, roll it back, scale it, read its
logs, and give it a database and secrets — but it has no way to run a single command inside the
app's environment. The agent's only recourse is out-of-band `kubectl exec`, which routes around the
control plane, holds no guardrail, leaves no audit record, and defeats the scoped-credential
boundary ([ADR-0038](0038-scoped-agent-credential.md)) that makes the agent safe to point at prod.

The need is concrete and about to be dogfooded: the Burrow website's blog moves its posts from
markdown into Postgres, which is a migration that must run against the app's real database with the
app's real `DATABASE_URL`. That command must execute in the exact image the app ships — same
runtime, same dependencies, same env, same secrets — or it is testing something other than what
runs in production.

Three facts shape the design:

1. **The pieces already exist, unassembled.** [ADR-0032](0032-postgres-backups.md) established the
   pattern for a short-lived task on the cluster: burrowd builds a Kubernetes Job, launches it,
   polls it to completion, records the outcome, and reaps it. The deploy path already resolves an
   app's current image and injects its config env vars and per-app Secret via `envFrom`
   ([ADR-0028](0028-app-config-and-secrets.md),
   [ADR-0029](0029-secrets-through-the-control-plane.md)). A one-off command runner is those two
   capabilities combined, plus one genuinely new step.

2. **A one-off command is naturally request/response.** It is launched, it runs, it finishes with an
   exit code and some output. Burrow has no streaming primitive today, and a short command does not
   need one — the shape is synchronous: launch, wait, return.

3. **Running arbitrary commands in prod is exactly the kind of operation guardrails exist for.** A
   migration can drop a table. The control plane is the place that decision is gated
   ([ADR-0006](0006-guardrails-in-the-control-plane.md)), and per-environment policy already lets an
   operator be free in staging and gated in prod ([ADR-0035](0035-environments.md),
   [ADR-0020](0020-guardrails-as-configurable-policy.md)).

## Decision

Add **`burrow_run`**, an explicit, agent-invoked, synchronous, guarded one-off command runner, and
its parity CLI `burrow app run <app> -- <cmd>`.

### 1. Explicit one-shot, sequenced by the agent — not an auto-on-deploy hook

`burrow_run` is an **explicit call**, invoked by the agent when it wants to run something. It is not
a declarative pre-deploy or release-command hook that fires automatically on every deploy. This is
the [ADR-0007](0007-explicit-deploy-by-image-reference.md) stance applied to command execution: the
explicit call is the spine, where the guardrail runs and the structured result is produced; an
implicit trigger is never the spine.

The explicit call is what lets the agent **sequence and react**. The agent runs the command, reads
its result, and decides what to do next: run a migration, and only if it succeeds, deploy; or deploy
first, then run. It can express the expand/contract migration shape — run the additive step, deploy
the code, run the cleanup step (run → deploy → run) — that a pre-deploy-only hook structurally
cannot. And it can stop on failure instead of blindly proceeding. A declarative "release command"
that runs on every deploy could be a **future optional layer on top of this primitive** — never a
replacement for the explicit call, exactly the status
[ADR-0007](0007-explicit-deploy-by-image-reference.md) gives GitOps auto-deploy.

### 2. The app's own current image, in the app's own environment

`burrow_run` runs the command from the app's **currently-deployed release's image**, resolved
through the same current-image lookup the deploy and rollback paths use — not an arbitrary,
caller-named image. The whole value is executing in the exact runtime, dependency set, and
filesystem the app ships with.

The Job runs in the app's namespace, and its container's environment is composed the same way the
app's own workload is: the app's config env vars and its per-app Secret injected via `envFrom`
([ADR-0028](0028-app-config-and-secrets.md)). So `DATABASE_URL` and every secret resolve exactly as
the running app sees them, with no separate wiring and no secret value crossing MCP
([ADR-0029](0029-secrets-through-the-control-plane.md)). The caller supplies only the command to run
(and its arguments); the environment comes from the app.

### 3. Synchronous: launch → wait → capture → return

The operation is request/response. burrowd builds the Job, launches it, and waits for it to finish,
then returns a **structured result carrying stdout, stderr, and the exit code**. A non-zero exit is
a normal structured outcome the agent reasons over, not a transport error. There is **no streaming**
— Burrow has no streaming primitive today, and a one-off command does not need one; a long-running
job that wants progress can revisit this later.

A **10-minute timeout** bounds the wait. Because the result — stdout, stderr, and the exit code — is
captured into the structured response **before** burrowd returns, the persisted Job and its pod are
never the source of the agent's answer; they are only an out-of-band window for a human to inspect a
failure by hand (`kubectl describe`, events). Cleanup is therefore uniform across both terminal
states, driven by TTL rather than an imperative reap (§7). This is a deliberate divergence from the
[ADR-0032](0032-postgres-backups.md) backup pattern, which leaves a failed Job in place indefinitely:
a backup Job's outcome must be read back from the Job, whereas `burrow_run`'s outcome is already in
hand, so a TTL that garbage-collects both success and failure is safe here and avoids accumulating
finished Jobs.

### 4. A new `app.run` guardrail

Command execution is gated by a new guardrail code, **`app.run`**, in the configurable-policy model
([ADR-0020](0020-guardrails-as-configurable-policy.md)). Like the other `app.*` codes it is
**per-environment scopable** ([ADR-0035](0035-environments.md)): its default disposition is
`confirm`, and `deny` or `confirm` is the recommended posture on prod. It follows the standard
held → `confirm=true` flow — a held operation returns a structured confirmation-required result
naming the command, and the agent surfaces it to the human and re-invokes only on their explicit
approval. **The agent never self-confirms** ([ADR-0020](0020-guardrails-as-configurable-policy.md)).
The acting command is recorded in the audit log ([ADR-0027](0027-audit-log.md)), through the same
per-operation allowlist that keeps secret values out of the record.

### 5. Honest limitation: `app.run` gates the operation, not its contents

**`burrow_run` is a command runner, not a SQL firewall.** The `app.run` guardrail gates *whether a
command may run*, per environment and per invocation. It does **not** inspect what the command does.
`burrow_run` runs the command opaquely: a migration or script invoked through it may contain any
destructive change — `DROP TABLE`, `TRUNCATE`, `ALTER … DROP COLUMN`, `DELETE` — and the guardrail
will not detect or hold on that content. Statement-level, content-aware data guardrails (hold a
`DROP`, allow an additive change) require parsing the SQL, which is a separate, Postgres-specific
tool (`burrow_run_sql`) explicitly **deferred to a future ADR** (see Rejected alternatives).

So this ADR does **not** claim "guardrailed migrations, solved" — that would violate
[ADR-0009](0009-honest-status.md). Data-safety for `burrow_run` comes from three honest layers, none
of which is content inspection:

- **Environment-scoped gating.** `deny`/`confirm` on prod means a command in the environment that
  matters cannot run silently ([ADR-0035](0035-environments.md),
  [ADR-0020](0020-guardrails-as-configurable-policy.md)).
- **Confirm-per-invocation with the command in plain view.** The command is echoed in the confirm
  prompt the human approves and recorded in the audit log
  ([ADR-0027](0027-audit-log.md)) — the human sees the actual command before it runs.
- **Recover, not prevent.** Real safety for destructive schema change is a proper migration tool's
  own versioning plus Burrow's Postgres backups ([ADR-0032](0032-postgres-backups.md)): back up →
  run → verify, with restore available if the command did the wrong thing.

### 6. Reuse, not reinvention

`burrow_run` is assembled from machinery that already exists. It reuses the
[ADR-0032](0032-postgres-backups.md) Job lifecycle behind the kube seam — build a Job, launch it,
poll it to completion — and the deploy path's current-image resolution and per-app-Secret `envFrom`
injection ([ADR-0028](0028-app-config-and-secrets.md)). The one genuinely new capability is
**capturing a finished Job pod's stdout/stderr and exit code**: pod lookup already exists for
backups, and log retrieval already exists for `logs`; `burrow_run` combines them and adds the exit
code. No new dependency and no new credential boundary — the Job runs under the same namespace-scoped
grants the add-on Jobs use, and the agent reaches it only through the guarded control plane
([ADR-0038](0038-scoped-agent-credential.md)).

### 7. Job cleanup (TTL)

The Job is garbage-collected by Kubernetes' native `ttlSecondsAfterFinished`, which applies uniformly
to **both** terminal states — `Complete` and `Failed` — so a finished Job and its pod are removed a
set time after they finish, with no imperative reap and no indefinite accumulation of failed Jobs.
The TTL **defaults to 1 hour**: long enough to inspect a failure by hand, short enough that finished
Jobs do not pile up. It is **configurable per invocation** through a `ttl` duration parameter on the
tool and CLI, including **`ttl=0`** to delete the Job immediately once the output is captured. Because
the result is already in the structured response before the Job is cleaned up (§3), a short or zero
TTL never costs the agent its answer — it only shortens the human forensics window.

## Consequences

- **Agent-driven one-off tasks become first-class:** migrations, seeds, backfills, one-off scripts,
  and console-style diagnostics run inside the app's real runtime, through the guarded control plane
  instead of an out-of-band `kubectl exec`. The first customer is the Burrow website's blog
  markdown→Postgres migration (dogfood).
- **The per-environment guardrail model extends to command execution.** `app.run` joins the `app.*`
  policy codes, free in staging and gated in prod, with no new gating mechanism
  ([ADR-0020](0020-guardrails-as-configurable-policy.md), [ADR-0035](0035-environments.md)).
- **A new audit operation.** Each run — held, denied, or executed — is recorded with the command
  through the redacting allowlist ([ADR-0027](0027-audit-log.md)).
- **New surface:** the `burrow_run` MCP tool and the `burrow app run <app> -- <cmd>` CLI (both taking
  a per-call `ttl` parameter), the `app.run` guardrail entry and its seeded default, the
  `ttlSecondsAfterFinished`-based Job cleanup, and the finished-Job stdout/stderr/exit-code capture
  behind the kube seam (faked in unit tests, exercised in a k3d e2e).
- **The opaque-command limitation is a stated, honest boundary,** not a hidden gap: `burrow_run`
  gates the operation, not its contents. Content-aware SQL guardrails are deferred and named, so no
  reader mistakes command gating for statement-level data protection
  ([ADR-0009](0009-honest-status.md)).

## Rejected alternatives

- **`burrow_run_sql(app, sql)` — a narrow, Postgres-only SQL tool instead of a general runner.**
  This is the only option that could do statement-level guardrails: parse the SQL, hold a `DROP`,
  allow an additive change. But it requires a SQL parser and reinvents migration versioning,
  idempotency, and rollback through the agent loop — the things a real migration tool already does
  well. It also solves only the database case, not the seed/backfill/script cases a general runner
  covers. **Deferred, not discarded:** it is the natural home for the future content-aware data
  guardrail, a separate slice layered beside `burrow_run`, not a replacement for it.
- **A declarative pre-deploy release-command hook (Heroku-style auto-run on every deploy).**
  Off-thesis: it puts an implicit trigger on the spine, exactly what
  [ADR-0007](0007-explicit-deploy-by-image-reference.md) rejects for deploy. It cannot express
  expand/contract (run → deploy → run) and cannot react to the command's output to stop on failure —
  both of which the explicit call gives for free. It may return later as an **optional layer over**
  `burrow_run`, never as the primitive.
- **Streaming output.** No streaming primitive exists in Burrow, and a one-off command is naturally
  request/response. Synchronous capture is the honest shape; revisit if long-running jobs need
  progress.
- **An arbitrary-image runner (run any image, not the app's own).** Rejected: the entire point is
  the app's exact runtime, dependencies, config, and secrets. A general "run this image" tool is a
  broader, different primitive with a different security profile; it is not what migrations, seeds,
  and backfills need.
