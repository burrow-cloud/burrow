# ADR-0027: Audit log — an append-only record of agent operations and guardrail decisions

## Status

Accepted. Builds on [ADR-0002](0002-four-layer-architecture.md) (the control plane is the
product and the single credential boundary), [ADR-0006](0006-guardrails-in-the-control-plane.md)
and [ADR-0020](0020-guardrails-as-configurable-policy.md) (guardrails evaluated in the control
plane, with allow / confirm / deny dispositions), [ADR-0012](0012-in-cluster-postgres.md)
(in-cluster Postgres holds control-plane state), and [ADR-0004](0004-code-never-over-mcp.md)
(code and credentials never travel over MCP — which shapes redaction). Per-principal identity is
coarse until a future authentication ADR; this ADR records what it can today and reserves room.

## Context

Burrow lets an AI agent operate a real cluster: deploy, roll back, scale, expose, manage DNS,
install add-ons, delete apps. Every mutation flows through the control plane and is guarded there
([ADR-0006](0006-guardrails-in-the-control-plane.md),
[ADR-0020](0020-guardrails-as-configurable-policy.md)). But nothing **records** what happened —
who asked, which operation, when, with what arguments, and what the guardrail decided. Once an
operation returns, the only trace is its side effect on the cluster and whatever the agent chose
to say.

Operators (and, in the managed product, admins) need accountability: a durable, queryable record
of agent actions. Three concrete pulls:

- **Debugging and incident review.** "What did the agent do at 02:00 when the site went down?"
  is currently unanswerable from Burrow itself.
- **Trust.** A central promise is that dangerous actions are gated. Showing *that they were
  gated* — "it asked before deleting; here is the confirmation" — needs a record, not a claim.
- **The decision lives in the control plane, so the record must too.** Dogfooding sharpened
  this: in a live run the only thing that paused a production rollback was the *client agent's*
  permission prompt, not Burrow — and there was no record of the decision anywhere. The
  authoritative decision is made where the credentials are; the authoritative record belongs
  there as well, not in an agent-specific, best-effort client log.

The guardrail evaluation already distinguishes exactly the outcomes a reviewer wants to see:
**allowed**, **held for confirmation**, **denied**, and — on a second, confirmed call —
**confirmation received and executed**. That maps directly onto an audit trail.

## Decision

Keep an **append-only audit log** of control-plane operations and their guardrail decisions,
stored in the control plane's Postgres ([ADR-0012](0012-in-cluster-postgres.md)) and written at
the control-plane boundary — the single choke point that already holds both the credentials and
the guardrail decision.

Each record captures:

- **when** — a timestamp from the injected clock (no ambient time, per the testing posture).
- **operation and target** — the logical operation (`deploy`, `rollback`, `scale`, `expose`,
  `dns_write`, `dns_delete`, `addon_install`, `addon_remove`, `app_delete`, …) and what it acted
  on (app, host, or add-on name).
- **arguments — redacted.** The salient parameters (image reference, replica count, host, the
  `confirm` flag), recorded through a per-operation allowlist. **Never secret values:**
  environment-variable *values*, tokens, and provider credentials are omitted or masked. The
  audit writer records keys and metadata, not contents — the same boundary
  [ADR-0004](0004-code-never-over-mcp.md) draws for MCP, applied to the record. Raw request
  bodies are never dumped.
- **decision** — the guardrail code and disposition that applied
  ([ADR-0020](0020-guardrails-as-configurable-policy.md)) and the **outcome**: `allowed`,
  `held` (confirmation required, not executed), `denied`, or `executed` (allowed, or confirmed
  and carried out). A held operation and its later confirmed execution are **two records**, so
  the trail reads "confirmation requested" → "confirmation received, executed."
- **result** — success, or the error category when an allowed operation failed to apply.
- **caller** — the authenticated caller. Today the control plane authenticates with a single API
  token, so identity is coarse: the record names that caller plus an optional client-supplied
  agent label. Which agent or which human is deferred to a dedicated authentication ADR; the
  schema reserves a column for it.

**Where it is written.** The audit write happens in the control plane around each mutating
operation, *including the guardrail-gated outcomes* — so a denied or held operation is recorded
even though it never executed. A failed write to the audit log must not silently swallow the
operation's own result, but the record is best-effort relative to the action: losing an audit row
is a logged degradation, not a reason to fail a deploy.

**How it is read.** Reads are themselves guarded control-plane operations and never expose
secrets. Surfaces, in order: a `burrow audit` CLI (list / tail / filter by app, operation, or
outcome); later a **read-only** MCP tool so the agent can review its own history; later the
managed-product admin dashboard. The agent may read the log but can never write to or alter it —
it is the operator's record *of* the agent, not the agent's scratchpad.

**Scope.** The first slice is the table, the writer wired into mutating operations and their
guardrail decisions, and `burrow audit` to read it. Deferred: richer per-principal identity (with
the auth ADR), configurable retention/pruning, the read-only MCP tool, and the dashboard.

## Consequences

- A durable, queryable accountability record — the basis for "what did the agent do," for trust
  demonstrations ("it asked first; here is the approval"), and for incident review. It
  strengthens the safe-defaults posture: guardrail decisions are now **recorded**, not only
  enforced in the moment.
- One INSERT per mutating operation: negligible cost, riding the Postgres the control plane
  already requires ([ADR-0012](0012-in-cluster-postgres.md)). No new dependency, no load on the
  user's etcd.
- **Redaction is load-bearing.** The writer must allowlist what it records and never serialize
  raw request bodies, or it would leak the very secrets [ADR-0004](0004-code-never-over-mcp.md)
  keeps off MCP. Argument capture is explicit and per-operation, which is more code than a blind
  dump — deliberately.
- Identity is coarse until authentication lands. The log is honest about this: it records the
  token and optional label it actually has, and the schema leaves room to enrich later without a
  migration of meaning.
- Append-only by contract: there is no edit or delete path through the API. Retention and pruning
  are an operator concern, handled out of band and specified later.

## Rejected alternatives

- **Kubernetes Events or the Kubernetes audit log.** Events age out and are not a durable,
  queryable record; both live in the user's etcd and carry none of Burrow's guardrail semantics.
  The control-plane decision belongs in the control-plane store, not the cluster
  ([ADR-0012](0012-in-cluster-postgres.md)).
- **Structured application logs only.** Logs are an ephemeral stream, not a schema'd, queryable
  record — the very limitation that pushed users toward the logs add-on for `app logs`. Parsing
  logs back into an audit trail is fragile and loses the guardrail outcome.
- **Trust the agent or client to log its own actions.** Not authoritative and agent-specific —
  exactly the gap dogfooding exposed, where the client (not Burrow) was the only thing that
  recorded or paused the action. The record must be written where the credentials and the
  decision are.
- **A full event-sourcing pipeline or external SIEM now.** Over-built for the single-tenant open
  core. The append-only table is the minimal durable record; exporting to an external system can
  layer on top later without changing this decision.
