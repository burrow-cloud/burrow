# ADR-0020: Guardrails as inspectable, configurable policy with a confirm disposition

## Status

Proposed. This refines [ADR-0006](0006-guardrails-in-the-control-plane.md) (guardrails live
in the control plane) by making the policy inspectable and configurable and adding a
`confirm` disposition. Recorded for review before the code lands; exact default thresholds
are tuned as it ships, not pinned here.

## Context

Today the guardrails are compiled-in and binary. `Policy{MaxReplicas, AllowScaleToZero}`
holds two knobs, `checkReplicas` either refuses an operation (a `GuardrailError`) or allows
it, and the defaults (`DefaultPolicy`: ceiling 50, scale-to-zero off) are constants — not
inspectable by the agent and not changeable without a rebuild. Two pressures break this:

- **v0.2 adds guardrails that aren't simply "deny."** DNS writes, DNS *deletes*, and public
  exposure ([ADR-0018](0018-reaching-an-app-at-a-url.md)) want a middle ground between
  silently allowing and hard-refusing — a *confirm* step — and they want to be configurable
  per install. Hardcoding a third, fourth, and fifth bespoke gate repeats the pattern this
  ADR replaces.
- **The agent should know the rules before it acts.** ADR-0006 promises structured results;
  an agent that can read "`domain remove` will require confirmation" *before* calling it
  plans better than one that discovers a refusal after the fact.

## Decision

Generalize guardrails into an **inspectable, configurable policy** evaluated in the control
plane, with three dispositions and a clear split over who may change it.

### Policy model

A policy is a set of entries keyed by a stable **action** (e.g. `scale.to_zero`,
`scale.ceiling`, `deploy.over_newer`, and later `dns.write`, `dns.delete`, `expose.public`).
Each entry has a **disposition** — `allow` | `confirm` | `deny` — and, for parameterized
guardrails, a **threshold** (e.g. the replica ceiling), where the disposition applies when
the request crosses the threshold. The current `Policy{MaxReplicas, AllowScaleToZero}`
becomes two entries: `scale.ceiling` (`deny`, threshold 50) and `scale.to_zero` (`confirm` —
realizing the "gated, not silent" intent the current code only half-implements by denying).

Policy lives in the control plane's Postgres ([ADR-0012](0012-in-cluster-postgres.md)),
seeded from conservative defaults via a migration ([ADR-0013](0013-database-migrations-and-upgrade-policy.md))
and evaluated by burrowd. Evaluation returns one of three structured outcomes, not a bare
allow/deny: **proceed**, **confirmation required** (with the code and a human reason), or
**denied** (with the code and reason).

### The confirm disposition and who it binds

- **`deny`** is absolute: the operation is refused for everyone (the current `GuardrailError`).
- **`confirm`** returns a structured *confirmation-required* result — not an error. The caller
  proceeds only by re-invoking with an explicit confirmation: the CLI passes `--confirm`; an
  MCP tool passes `confirm: true`. For the **human CLI** this is a hard gate (a person decides
  to add the flag). For the **agent** it is a *cooperative* gate: the tool contract requires
  the agent to surface the confirmation to the user and re-invoke only on their say-so —
  honest about being cooperative, not cryptographically enforced.
- **`allow`** proceeds silently.

For a gate that must hold even against an over-eager or misbehaving agent, the action is left
`deny`; the **human's lever to relax it is `guard set`** (below), which is an out-of-band
human action. So "deny + the human flips it when they mean to permit it" is the hard gate,
and `confirm` is the everyday cooperative gate.

### Surface: the agent can see policy; only the human can change it

- **`burrow guard list`** — read-only; shows every action, its disposition, and any threshold.
  **Exposed over MCP** (read-only) so the agent knows the rules before acting.
- **`burrow guard set <action> <allow|confirm|deny> [--limit n]`** — changes a policy entry.
  **CLI-only; never an MCP tool.** The agent must not be able to change its own guardrails, so
  the MCP server simply does not expose policy mutation — only the human, through the CLI,
  adjusts policy. This is the [ADR-0003](0003-agent-neutral-mcp-control-surface.md) principle
  in action: the MCP server chooses which operations to expose, and self-guardrail-editing is
  not one of them.

`guard set` writes policy through burrowd's API (the policy lives in Postgres, which only
burrowd touches); the protection is not that the endpoint doesn't exist but that the MCP
server never surfaces it as a tool.

## Consequences

- **`controlplane` migration:** `Policy`/`checkReplicas` generalize from two fixed knobs to a
  keyed, disposition-carrying policy with an evaluation that returns proceed / confirm / deny.
  `GuardrailError` (deny) stays; a sibling **confirmation-required** outcome is added.
  Defaults preserve today's behavior except scale-to-zero moves from `deny` to `confirm`,
  which is the originally-intended "gated" behavior.
- **Operation protocol gains confirmation:** mutating CLI commands take `--confirm`; mutating
  MCP tools take a `confirm` argument. A confirmation-required outcome is a normal structured
  result the agent reasons over, not an error.
- **v0.2 guardrails plug in as policy** rather than new hardcoded gates: `dns.write`,
  `dns.delete` (default `confirm`/`deny`), `expose.public` (default `confirm`) become entries.
- **The setup-vs-operation split holds** ([ADR-0017](0017-private-registry-authentication.md)):
  `guard set` is a human action; guard *evaluation* happens inside agent operations; `guard
  list` is readable by both.
- **Honest about enforcement strength** ([ADR-0009](0009-honest-status.md)): `confirm` is a
  cooperative gate for the agent and a hard gate for the CLI; `deny` is absolute. The docs say
  so plainly rather than implying `confirm` restrains a hostile agent.

## Rejected alternatives

- **Keep guardrails compiled-in, just add more booleans.** Rejected: it does not scale to the
  v0.2 gates, gives the agent no way to read the rules, and offers operators no per-install
  control.
- **Expose `guard set` over MCP too.** Rejected outright: it lets the agent disable its own
  guardrails, defeating the purpose. Policy mutation is human-only.
- **Make `confirm` a cryptographic, agent-proof gate.** Rejected for v0.x as overengineered and
  flow-breaking; `deny` already provides the hard gate, and the human's `guard set` is the
  out-of-band control. `confirm` is honestly a cooperative gate for agents.
- **Store policy in a Kubernetes ConfigMap instead of Postgres.** Rejected: the control plane
  already owns its state in Postgres with migrations and a single writer (burrowd); splitting
  policy into a ConfigMap adds a second source of truth and a second RBAC surface.
