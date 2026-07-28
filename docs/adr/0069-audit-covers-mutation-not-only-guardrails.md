# ADR-0069: The audit log records mutation, not only guarded operations

## Status

✅ Accepted

## TL;DR

The audit log is written by `recordDecision(ctx, op, target, args, code GuardrailCode, guardErr)`.
Its signature says the whole thing: **an operation is audited because it passed through a
guardrail.** Anything without a guardrail code is unaudited by construction.

So the audit log covers app lifecycle — deploy, rollback, scale, expose, autoscale, delete, add-on
install/remove/detach/restore — and covers **none of** these:

```
SetGuardrail    (guard set)      NOT audited
SetSecret / UnsetSecret          NOT audited
SetConfig                        NOT audited
AddEnvironment                   NOT audited
```

That is an inversion. The unaudited set is the **privileged** set — the operations a human runs with
the admin kubeconfig, that change what everything else is allowed to do. Changing an app's replica
count leaves a permanent record; changing the policy that decides whether an agent may delete apps
leaves none.

It also undercuts a decision made elsewhere. [ADR-0065](0065-what-belongs-on-the-agent-surface.md)
makes two capabilities safe by denying them *by default*, and states that the tier holds only because
`guard set` is operator-only. If someone relaxes those denials, there is no record that it happened,
when, or by whom.

This decouples the two: **an operation is audited because it mutates, not because it is guarded** —
and §6 extends it to the reads that *disclose* something, chiefly `logs`, which is the one read that
can return a credential an application printed.

Refines [ADR-0027](0027-audit-log.md), which scoped the log to "agent operations and guardrail
decisions" — a scope that was right for its purpose and is too narrow for what the log is now relied
on for. Supports [ADR-0065](0065-what-belongs-on-the-agent-surface.md) and
[ADR-0068](0068-operational-limits-are-configuration.md), both of which put safety properties behind
operator-only settings. Supersedes nothing.

## Context

### What exists today

- **`audit_log`** has columns `ts, operation, target, args, guardrail_code, disposition, outcome,
  result, caller` — a shape built around a guardrail decision.
- **Two writers**, both in `controlplane/audit.go`: `recordDecision`, which takes a `GuardrailCode`
  and records the disposition, and `recordExecution`, which records the outcome afterwards.
- **Every call site passes a guardrail code.** The audited operations are exactly:
  `app.deploy`, `app.rollback`, `app.delete`, `app.expose_public`, `app.autoscale`,
  `addon.install`, `addon.remove`, `addon.detach`, `addon.restore`.
- **Everything else is silent.** `SetGuardrail`, `SetSecret`, `UnsetSecret`, `SetConfig` and
  `AddEnvironment` are Engine methods with no `recordDecision`/`recordExecution` call. Provider
  registration, `install` and `upgrade` are outside the Engine entirely and equally unrecorded.
- **`caller` exists but is coarse.** The migration comment says so plainly: reserved so identity can
  be enriched later without a migration of meaning.

### What breaks

**The privileged operations are the unlogged ones.** A deploy — routine, reversible, agent-driven,
and already gated — is recorded. A guardrail policy change — rare, powerful, human-only, and the
thing that decides what the agent may do — is not. If an operator asks "who allowed this?", the log
answers for the deploy and is silent about the permission that let it happen.

**Two safety decisions now rest on an unlogged setting.** ADR-0065 denies `app.delete` and
`dns.delete` by default and expects operators to relax them per environment. ADR-0068 puts
operational limits behind operator-only configuration. Both are correct only while somebody knows
what the current settings are and how they got that way. Today the first half is inspectable
(`guard list`) and the second half does not exist.

**Secret and config changes are invisible.** Not their *values* — those must never be logged
([ADR-0029](0029-secrets-through-the-control-plane.md)) — but the fact that a key was set or unset,
on which app, and by whom. An app that starts failing after a config change has no record that the
change happened.

### What this record resolves

Which operations the audit log covers, and on what basis — replacing "the ones that happen to have a
guardrail" with a stated rule.

## Decision

### 1. Mutation is the criterion, not guarding

**Any operation that changes control-plane or cluster state is audited**, whether or not a guardrail
evaluated it. A guardrail code and disposition remain *columns* — populated when one applied, empty
when none did — rather than the reason a row exists.

The two are genuinely independent concerns: a guardrail decides whether something may happen, an
audit records that it did. Coupling them meant an operation could only be accountable if it was also
gated, which is backwards for the operations that need accounting most.

### 2. The operations added

- **Guardrail policy** — `guard set`. The highest-value addition: it changes what every subsequent
  operation is permitted to do, and it is the setting ADR-0065's tiers depend on.
- **Configuration** — [ADR-0068](0068-operational-limits-are-configuration.md)'s cluster and
  environment limits, for the same reason.
- **App config and secrets** — `config set/unset`, `secret set/unset`. **Keys and app names only,
  never values** (§3).
- **Environments** — create and remove. Creating one applies a namespace and grants burrowd RBAC in
  it; that is a privilege change and belongs in the record.
- **Provider registration** — which providers were configured and when. Not the tokens.
- **Install and upgrade** — the operations that create every other privilege. These sit outside the
  Engine today and are the most awkward to reach, which is a reason to be deliberate about them
  rather than a reason to omit them.

### 3. Secret values never enter the log, and this constrains §2

ADR-0029 keeps secret values off every path but the Kubernetes Secret, and that is unchanged. An
audit row for `secret set` records the app and the **key**; the value does not reach the row, the
`args` JSON, or the operation string.

This is stated as a decision rather than assumed because §2 adds the first audited operations whose
arguments are secret-adjacent, and an `args` blob is exactly where a value gets logged by accident.

### 4. The record says who, as well as what

`caller` is populated for every row §2 adds, distinguishing at minimum a **human operator** from an
**agent**. "The policy changed" is a much weaker statement than "the policy was changed by the
operator CLI".

The column exists and its comment already anticipates enrichment; a fuller principal model is later
work and this record does not design one.

### 5. Failures are audited too

An attempted mutation that fails is recorded with its outcome. A refused `guard set` and a failed
`secret set` are both things an operator investigating an incident needs to see, and a log that
records only successes is a log that hides the interesting half.

### 6. Reads are audited when they disclose data, not merely when they happen

Auditing every read is the wrong shape: reads outnumber mutations by orders of magnitude, an agent
polls `status` and `apps` continuously, and a log in which almost every row is a listing is harder to
use for the question it exists to answer. Retention would become urgent immediately, and the signal
this record adds would be buried under the noise.

**The line is disclosure.** A read is audited when it returns data belonging to the thing being read,
rather than Burrow's own account of its state:

| Audited | Not audited |
| --- | --- |
| `logs` — an app's output, which may contain anything it printed | `apps`, `status`, `history` |
| log and metric **queries** | `reachability` |
| `secret` listing — key names are information even though values never appear | `guard` — the policy is not secret |
| reading the **audit log itself** | `addons`, `backups` listings |

`logs` is the clearest case and the reason this section exists: an application prints whatever it
prints, so its log is the one read that can return a credential. "Who read production's logs" is a
question worth being able to answer, and it is not answerable today.

Reading the audit log is included deliberately. A record of who inspected the record is what stops
the log being quietly consulted, and it costs one row per read of a rarely-read surface.

**A read row is not a decision row.** There is no guardrail, no disposition, and nothing to hold — it
records that data was disclosed, to whom, and when. Rows carry the target and the caller, never the
disclosed content: an audited `logs` read must not embed log lines, which would put in the audit log
precisely the secret the read might have exposed.

## Consequences

- **The log becomes answerable for "who changed the rules"**, which is the question it could not
  answer and the one that matters when something has gone wrong.
- **Volume grows.** The mutations added are rare by nature; §6's reads are not — `logs` in particular
  is polled during any investigation, so it is the row type that will dominate. Retention therefore
  becomes a real question with this record rather than eventually, and this record does not decide
  it.
- **`audit_log`'s shape stops being guardrail-centric.** `guardrail_code` and `disposition` become
  optional in meaning as well as in SQL, and anything reading the table that assumed a code is
  present needs to tolerate its absence.
- **Every future mutating operation inherits an obligation.** Adding one now means adding an audit
  call, and forgetting is silent — the same failure that produced this record. A test asserting that
  Engine methods which mutate also audit would convert the rule into a check; this record recommends
  it without specifying its shape.
- **`install` and `upgrade` are awkward.** They run before or outside the Engine, sometimes against a
  control plane that is not yet serving, so recording them may mean writing directly to the store or
  deferring the row. That is real work and it is the piece most likely to be dropped.
- **An audit log that covers privileged operations invites being trusted as a compliance artifact.**
  It is append-only in intent, but it lives in the same database as everything else and an operator
  with admin access can edit it. That limit should be documented rather than discovered.

## Rejected alternatives

- **Audit only `guard set`, and leave the rest.** The smallest change that fixes the ADR-0065
  dependency, and defensible on those grounds alone. Rejected because the same argument applies
  unchanged to configuration, environments and secret keys, and because leaving the criterion
  unstated is what allowed the gap to open — the next unaudited privileged operation would arrive by
  the same route.
- **Keep coupling audit to guardrails, and give the missing operations guardrail codes.** Superficially
  tidy, and it needs no change to the audit path. Rejected because it would mean inventing
  dispositions for operations that have no meaningful `allow`/`confirm`/`deny` — the same conflation
  [ADR-0068](0068-operational-limits-are-configuration.md) removes for the replica ceiling. An
  operation should not need to be gateable to be recordable.
- **Log secret and config changes with their values**, for a complete record. Rejected outright:
  ADR-0029 exists precisely to keep values off every path but the Secret, and an audit log is
  long-lived, widely readable, and exported.
- **Write audit rows from the API layer rather than the Engine**, catching everything uniformly.
  Attractive, and it would catch operations the Engine never sees. Rejected because the API layer
  knows the request, not the outcome — it cannot record whether the mutation succeeded, which is
  most of the value — and because operations that bypass the API would still be missed.
