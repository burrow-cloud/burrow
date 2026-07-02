# ADR-0039: Version skew between the CLI and the control plane

## Status

✅ Accepted

## TL;DR

The control plane is the version-compatibility anchor: it stays backward-compatible to clients
one minor version back and never refuses a request on version difference alone, so one person
upgrading burrowd cannot break a teammate's older CLI. A client-version header turns genuine
incompatibility — a newer client calling a feature the server lacks, or a client older than the
supported window — into an actionable "run `burrow upgrade`" error instead of an opaque failure.

Builds on [ADR-0002](0002-four-layer-architecture.md) and
[ADR-0005](0005-mcp-server-holds-no-cluster-credentials.md) (the control plane is the product;
the CLI and MCP server are thin clients), and on
[ADR-0013](0013-database-migrations-and-upgrade-policy.md) (single-minor-step upgrades);
realizes [ADR-0006](0006-guardrails-in-the-control-plane.md) (every operation returns a
structured result the agent can reason over); interacts with [ADR-0027](0027-audit-log.md) (the
acting client version belongs in the record). Supersedes nothing.

## Context

Burrow has three shipped clients of one control plane: the `burrow` CLI, the `burrow-mcp` MCP
server, and (later) the managed product. They are versioned and released together in the
Homebrew formula, but they are **installed and upgraded independently of burrowd**, which runs
in the cluster and is upgraded by a separate, privileged act (`burrow upgrade`). So their
versions drift apart in normal use, and the drift is unavoidable in the multi-user story Burrow
is built for: one developer installs Burrow and upgrades burrowd; a teammate joins the same
cluster ([ADR-0038](0038-scoped-agent-credential.md)) with whatever CLI they happen to have; a
third `brew upgrade`s their CLI but does not touch the server. Every combination of "newer/older
CLI" against "newer/older burrowd" occurs.

Two forces make this sharper than in a single-binary tool. First, a burrowd upgrade is a
**shared, one-actor event** with **many-actor blast radius**: if upgrading the server could
break other people's CLIs, then every server upgrade becomes a coordinated flag-day for the
whole team, which is exactly the friction Burrow exists to remove. Second, the `burrow-mcp`
server is a long-running subprocess that an agent session launches once; `brew upgrade` swaps
the binary on disk but the **running** MCP server keeps executing the old code until the session
restarts. So even a single user who upgrades can, mid-session, be driving an older client
(the in-flight MCP server) than the CLI in their terminal.

Today there is no policy and no mechanism. The client sends no version to burrowd; burrowd
performs no compatibility check (it reads only the auth token from the request). `burrow version`
learns the control-plane version passively, by reading the burrowd Deployment's image tag through
the kubeconfig, and prints an **advisory** hint ("your control plane is behind, run
`burrow upgrade`"). Nothing is blocked, which is the right instinct, but the gaps show at the
edges: a newer CLI that calls a brand-new operation against an older burrowd (an endpoint or
field the server does not know) fails with a low-level error rather than an actionable one, and
the compatibility contract is nowhere written down, so neither side can reason about it.

## Decision

**The control plane is the compatibility anchor. It stays backward-compatible to clients within
a one-minor window, never hard-blocks a request on version difference alone, and turns genuine
incompatibility into a structured, actionable error rather than an opaque failure.** A server
upgrade never breaks an in-window client; upgrading a client is how you gain new features, on
your own schedule.

### 1. burrowd defines the contract; the clients conform

The control plane is the only stateful, credentialed layer ([ADR-0002](0002-four-layer-architecture.md)),
so it owns the API contract and its evolution. Compatibility is always expressed relative to the
**burrowd version**: a client is "supported," "ahead," or "behind" with respect to the server,
never the reverse. The CLI and MCP server are thin conformant clients
([ADR-0005](0005-mcp-server-holds-no-cluster-credentials.md)).

### 2. Backward compatibility within a one-minor window

burrowd of minor version `N` serves clients of minor `N` and `N-1`. The API within a minor step
is **additive**: a minor may add operations, fields, and guardrail codes, but does not remove or
change the meaning of what the previous minor already relied on. A breaking change to an existing
operation is carried across a minor with a deprecation, not made in place. This window is bounded
deliberately to one minor because burrowd itself only ever moves **one minor at a time**
([ADR-0013](0013-database-migrations-and-upgrade-policy.md)): a client is never more than one minor
behind the server before its user has been prompted to upgrade, so the compatibility surface and
the migration path stay in lockstep. (The window is a floor, not a ceiling; a future ADR may widen
it once the API stabilizes past v1.0.)

### 3. Never hard-block on version difference alone

A request from an in-window client is served. Burrow does **not** refuse a request merely because
the client and server versions differ. Refusal is reserved for a request the server genuinely
cannot honor, and it is scoped to that operation, not the whole session. This is what keeps a
server upgrade from flag-daying a team: the moment burrowd moves to `N`, every teammate on `N-1`
keeps working untouched.

### 4. A version handshake that produces actionable errors

The client sends its version to burrowd on every request (an `X-Burrow-Client-Version` header,
alongside the existing `X-Burrow-Token`). burrowd uses it to make skew legible instead of opaque
([ADR-0006](0006-guardrails-in-the-control-plane.md)):

- **Client newer than the server, calling a feature the server lacks:** the server returns a
  structured error naming the gap and the fix — "this control plane (`vN-1`) does not support
  `autoscale`; ask an operator to run `burrow upgrade`" — instead of an unknown-endpoint failure.
  Shared operations still work; only the new capability is gated, and it says why.
- **Client older than the supported window:** the server returns "your `burrow` CLI (`vN-2`) is
  too old for this control plane (`vN`); run `brew upgrade burrow`," rather than misbehaving on a
  contract it no longer matches.

The acting client version also belongs in the audit record ([ADR-0027](0027-audit-log.md)),
next to the principal: who did what, with which client, against which server version.

### 5. The resulting guarantees

- **Old CLI + new server (in window):** works. The user is nudged by `burrow version`, and
  upgrades when convenient.
- **New CLI + old server:** shared operations work; a new-feature operation fails with an
  actionable "upgrade the control plane" error, never a silent or cryptic one.
- **A server upgrade never blocks another user's in-window client.** No coordinated flag-day.
- **Because the MCP server is not hot-swapped**, an agent session may briefly run an older client
  than the user's terminal CLI after a `brew upgrade`; the one-minor window absorbs this without a
  restart being mandatory for correctness, only for gaining the newer client's features.

## Consequences

- The multi-user story holds: server and client upgrade on independent schedules, and neither
  strands the other within the window.
- The API gains a discipline: within a minor, changes are additive and backward-compatible;
  anything breaking is deprecated across a minor. This is a real constraint on how the control
  plane evolves, and it is the cost of not flag-daying users.
- Skew stops being an opaque failure mode. A newer client against an older server, or a stale
  client against a fresh one, gets a first-class error an agent can act on and relay to the user,
  consistent with the structured-feedback rule.
- The client version becomes a small piece of request context (a header) and audit context,
  reused by whatever later needs to reason about who acted with what.
- `burrow version` remains the passive, token-free way to see the two versions and the advisory
  nudge; this ADR adds the active, per-operation handling that the passive view cannot give.

## Rejected alternatives

- **Hard version lock — refuse any client whose version differs from the server.** This makes
  every burrowd upgrade a synchronized team event: the instant one person upgrades the server,
  everyone else is locked out until they upgrade too. It is brittle, hostile to the multi-user
  case, and inverts the whole point of letting the agent operate infrastructure without ceremony.
- **Lockstep only — require the CLI and burrowd to be the identical version.** The strictest form
  of the above, and impossible the moment two people share a cluster.
- **Client as the compatibility anchor.** The server holds the state, the guardrails, and the
  credentials ([ADR-0002](0002-four-layer-architecture.md), [ADR-0006](0006-guardrails-in-the-control-plane.md));
  it must define the contract. A client-driven scheme would let a stale or a rogue client dictate
  terms to the authoritative layer.
- **Status quo — advisory only, no handshake.** Keeping just the passive `burrow version` hint
  leaves the new-client-against-old-server case failing opaquely and leaves the compatibility
  contract unwritten, so neither the code nor an agent can reason about it. The advisory view is
  necessary but not sufficient; this ADR keeps it and adds the missing active handling.
