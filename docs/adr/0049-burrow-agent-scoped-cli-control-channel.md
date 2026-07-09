# ADR-0049: `burrow-agent` — a scoped CLI as the agent's control channel

## Status

🟡 Proposed

## TL;DR

Retire the MCP server as the agent's primary interface and give the coding agent its own
dedicated binary, **`burrow-agent`**: a capability-reduced, JSON-first command-line surface that
the agent invokes directly, authenticating to the control plane with a scoped control-plane token
and holding no cluster credentials. The primary win is **composability** — the agent can pipe,
grep, and `jq` a CLI's output (`burrow-agent logs web --json | jq … | head`), which a fixed
MCP result blob fundamentally cannot offer — plus a one-binary install, a structurally
capability-reduced surface, and one place to maintain the operate-verbs instead of two.

Supersedes [ADR-0005](0005-mcp-server-holds-no-cluster-credentials.md): its
credential-boundary principle is preserved and migrated to `burrow-agent`. Builds on
[ADR-0038](0038-scoped-agent-credential.md) (the scoped agent credential the CLI carries) and
echoes [ADR-0007](0007-explicit-deploy-by-image-reference.md) (the scoped CLI is the spine;
MCP, if kept, is an optional layer). Does **not** supersede
[ADR-0004](0004-code-never-over-mcp.md) (the code-never-over-MCP invariant survives,
transport-independent) or [ADR-0002](0002-four-layer-architecture.md) (the thin client layer
simply becomes the scoped CLI). Supersedes ADR-0005.

## Context

Burrow's agent-facing surface is an MCP server ([ADR-0002](0002-four-layer-architecture.md),
[ADR-0003](0003-agent-neutral-mcp-control-surface.md)): the agent issues MCP tool calls,
`burrow-mcp` translates them into control-plane calls, and each call returns a single structured
result blob. That shape has served the deploy-and-operate core, but it carries costs that grow as
the surface matures:

1. **MCP is call-response with a fixed result.** The agent receives one blob per tool call and
   cannot compose it — it cannot pipe a result into `grep`, slice it with `jq`, page it with
   `head`, or chain one tool's output into another's input without round-tripping every
   intermediate through its own context. Composability is the native idiom of a coding agent's
   environment (a shell), and MCP fundamentally cannot offer it. A CLI with `--json` output the
   agent composes directly — `burrow-agent logs web --json | jq '.[] | select(.level=="error")' | head`
   — is a real capability the tool-call channel structurally lacks. This is the primary motivation.

2. **An MCP server is a second thing to install and configure.** The user stands up `burrow-mcp`,
   wires it into the agent's MCP configuration, and keeps it current. A single CLI on the agent's
   PATH removes that setup step entirely — one binary, no server to run, relevant directly to the
   getting-started path.

3. **A persistent perception that "MCP pollutes my context."** Schema-on-demand already mitigates
   the tool-schema footprint in some clients, so this is partly perception rather than a hard cost
   — but a CLI the agent invokes on demand sidesteps the perception altogether: nothing is resident
   until the agent runs a command.

4. **Two surfaces drift.** Burrow already ships a human CLI (`burrow`) alongside the MCP tools, and
   keeping an MCP surface and a CLI surface at feature parity is a standing tax — every new
   operate-verb must be built, documented, and tested twice, and the two can silently diverge.

The scoped agent credential ([ADR-0038](0038-scoped-agent-credential.md)) already established that
the agent reaches burrowd through a narrow, burrowd-only credential and holds no real cluster
power; the guardrails bind because every path goes through burrowd
([ADR-0021](0021-guardrails-require-control-plane-only-agent-access.md)). What is missing is the
right shape for the agent's own control channel.

## Decision

**Replace the MCP server with a dedicated, scoped command-line binary, `burrow-agent`, as the
agent's primary interface to Burrow.** The agent invokes `burrow-agent` directly; it is
composable, JSON-first, structurally reduced to the safe operate-verbs, and credential-free
beyond a scoped control-plane token.

### 1. Two distinct binaries, not a subcommand carve-out

- **`burrow`** is the human admin CLI: install, bootstrap, connect, guard, environments — the full
  surface, driven by a human at a terminal with their admin kubeconfig.
- **`burrow-agent`** is the agent's binary: a **capability-reduced** surface that *intrinsically
  lacks* the dangerous admin verbs. It has no `install`, no `bootstrap`, no `cluster` setup, no
  `guard set`, no registry/provider credential writes — those verbs are simply not compiled into
  it. It carries the operate-verbs (deploy, status, logs, rollback, scale, run, and their
  read-only siblings), outputs JSON first so the agent can compose it, and authenticates to the
  control plane with a **scoped control-plane token** tied to the scoped agent credential
  ([ADR-0038](0038-scoped-agent-credential.md)). Like the MCP server before it, it holds **no
  cluster credentials**.

### 2. Defense in depth: three independent layers

The safety of pointing an agent at production rests on three layers, each of which holds even if
the others fail:

- **(a) The binary lacks the dangerous verb.** `burrow-agent` cannot express `install` or
  `guard set` because the command does not exist in it. This is a *structural* reduction, not a
  runtime check.
- **(b) The scoped token's RBAC lacks the permission.** Even a command that reached the control
  plane is bounded by the narrow grants the scoped credential carries
  ([ADR-0038](0038-scoped-agent-credential.md)).
- **(c) The control-plane guardrails gate the rest, server-side.** Deploy, delete, publish, and the
  other risky operations are held or denied by policy in burrowd
  ([ADR-0006](0006-guardrails-in-the-control-plane.md),
  [ADR-0020](0020-guardrails-as-configurable-policy.md)), regardless of which client called.

This is a **stronger boundary than today's "deny the `burrow` CLI via a settings rule."** A deny
list depends on a runtime honoring it at call time; a binary that structurally lacks the verb does
not. The reduction moves from a runtime convention to the shape of the artifact itself.

### 3. Why two binary names, not a `burrow agent …` subcommand

The agent's environment restricts what the agent may run by matching against the **executable**.
Agent runtimes generally match permissions on the command being invoked, and a deny rule takes
precedence over an allow rule — so allowing a subcommand while denying its parent binary is fragile
where it works at all, and it is **not portable across runtimes**: different agent runtimes
(Cursor, Codex, and others) lean toward executable-level allow/deny and do not share one
subcommand-matching semantics. Two distinct binary names are the portable
lowest-common-denominator: every runtime that can allow or deny a command can allow `burrow-agent`
and deny `burrow`. The design commits to the portable argument — a separate executable — rather
than to any one runtime's precise rule syntax.

### 4. `burrow connect <tool>` wires the agent's permissions

The current `burrow mcp <tool> install` command — which installs the MCP server and writes a
`burrow` deny rule into the agent's configuration — is superseded by a **`burrow connect <tool>`**
(and `burrow connect <tool> install`) command whose sole job is to write the agent's permission
rules: **allow `burrow-agent`, deny `burrow`**, and optionally deny `kubectl` and other cluster
CLIs. It keeps the existing preview → idempotent-apply → backup UX: show the user the rules it will
write, apply them idempotently, and back up what it replaced. (This ADR records the command's job;
it does not implement it.)

### 5. Capability discovery without MCP tool schemas

MCP handed the agent a machine-readable tool list; a CLI must answer the same "what can I do here?"
question by its own means. `burrow-agent` provides discovery through **rich `--help` and
self-description** on every command and subcommand, and a **concise instructions surface** — the
bare `burrow-agent` invocation with no arguments prints an orientation, and `burrow connect` can
install an instructions document into the agent's context, analogous to the `instructions` the MCP
server exposes today. The agent discovers the surface by reading help and instructions, not by
loading a schema.

### 6. Relationship to the load-bearing invariants

- **Supersedes [ADR-0005](0005-mcp-server-holds-no-cluster-credentials.md).** The principle — the
  agent-adjacent layer holds no cluster credentials; the control plane is the sole credentialed
  layer and the security boundary — is *preserved and migrated*. `burrow-agent` holds no cluster
  credentials, only a scoped control-plane token, and burrowd remains the one layer that talks to
  Kubernetes. What changes is the name of the thin client, not the boundary.
- **Does not supersede [ADR-0004](0004-code-never-over-mcp.md).** Its decision is invariant #1 and
  is **transport-independent**: code moves through the container registry and never through the
  agent's control channel, whether that channel is MCP or a CLI. A `deploy` names an image
  reference already on the belt; `burrow-agent` sends the reference and small metadata, never
  bytes. The invariant survives verbatim in substance.
- **Does not supersede [ADR-0002](0002-four-layer-architecture.md).** The four layers stand; layer
  2 — the thin, credential-free client — is simply the scoped CLI (`burrow-agent`) rather than an
  MCP server. The control plane is still the product and the only Kubernetes-credentialed layer.
- **Echoes [ADR-0007](0007-explicit-deploy-by-image-reference.md).** The scoped CLI is the spine of
  the agent's control channel, where the guardrails run and the structured result is produced. MCP,
  if kept at all, is an optional layer beside it — never the other way round.

### 7. MCP disposition

- **Stop shipping the `burrow-mcp` binary as of the next release.** The release build drops it.
- **Keep `mcp/` and `cmd/burrow-mcp/` in the tree for now.** The code stays retrievable and can be
  removed outright later; git history preserves it regardless.
- **Documentation and positioning stop recommending MCP.** The getting-started path, the README,
  and support guidance point at `burrow-agent`.
- **When a user asks for MCP, the honest answer is that the composable CLI is the better path.** A
  short, reusable "why the CLI beats MCP here" list for the docs and support:
  - *Composability* — pipe, `grep`, `jq`, and chain output; a fixed result blob cannot.
  - *One-binary install* — nothing to run or wire up; one command on PATH.
  - *Structural capability-reduction* — the agent's binary lacks the dangerous verbs by
    construction, a stronger boundary than a runtime deny list.
  - *No parity burden* — one surface for the operate-verbs, so nothing drifts.

## Consequences

- **The agent gains composition.** Piping, filtering, and chaining `--json` output is the day-one
  capability that the tool-call channel could not provide; it is the reason for the change.
- **One fewer thing to install.** The getting-started path drops the MCP-server setup step: install
  one CLI, run `burrow connect <tool>`, and the agent is wired.
- **The agent-layer boundary gets structurally stronger.** Capability-reduction moves from a
  runtime deny list to the shape of the binary, backed by scoped RBAC and server-side guardrails.
- **Loses MCP-ecosystem positioning and discoverability.** Burrow no longer advertises as an MCP
  server in that ecosystem's directories. The counter is broader reach, not narrower: "works with
  any agent that can run a command" covers more runtimes than "MCP-capable agents."
- **The human-confirm UX shifts.** Where an MCP client rendered the approval prompt, a held
  operation now surfaces through Burrow's own `--confirm` flow, relayed to the human through chat.
  This is a presentation change only — the guardrails are enforced server-side regardless of client
  ([ADR-0006](0006-guardrails-in-the-control-plane.md)).
- **Some governance-minded orgs prefer MCP for agent-layer tool-call observability.** The counter is
  that Burrow's server-side audit log ([ADR-0027](0027-audit-log.md)) records every guarded
  operation and its disposition behind the boundary — a stronger record than client-side tool-call
  logging, and one that does not depend on the client.
- **A follow-up must generalize the invariant-#1 wording.** CLAUDE.md and the four-layer narrative
  phrase the code-artifact invariant as "over MCP"; with the agent's channel now a CLI, that wording
  should read "over the agent control channel" (the substance is unchanged — code still moves only
  through the registry). This ADR names that reconciliation as a consequence; it does not make the
  edit.
- **New surface follows:** the `burrow-agent` binary and its capability-reduced command set, the
  `burrow connect <tool>` wiring command that replaces `burrow mcp <tool> install`, the JSON-first
  output mode across the operate-verbs, and the instructions surface for discovery. (Named here as a
  decision; built with its slice.)

## Rejected alternatives

- **Keep MCP as the primary interface.** Rejected: the tool-call channel cannot offer composition —
  the primary win — and it keeps the extra install step and the two-surface parity tax. MCP's
  ecosystem positioning does not outweigh a capability the agent's environment makes native.
- **Maintain both MCP and a CLI at feature parity.** Rejected: the parity burden is exactly the tax
  this decision removes, and shipping two co-equal surfaces sends a split message about which is the
  real interface. If MCP is kept at all it is an optional layer, not a co-equal spine
  ([ADR-0007](0007-explicit-deploy-by-image-reference.md)).
- **A `burrow agent …` subcommand carve-out instead of a separate binary.** Rejected: agent
  runtimes match permissions on the executable and let a deny take precedence over an allow, so
  allowing a subcommand while denying its parent is fragile, and the subcommand-matching semantics
  are not portable across runtimes. Two binary names are the portable lowest common denominator.
- **Give the agent the full `burrow` binary and rely only on a deny list.** Rejected: a deny list is
  a runtime convention that must be honored at call time, weaker than a binary that structurally
  lacks the dangerous verbs. Capability-reduction belongs in the shape of the artifact, not in a
  rule the runtime might not enforce.
