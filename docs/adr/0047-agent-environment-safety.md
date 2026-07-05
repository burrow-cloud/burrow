# ADR-0047: Agent environment safety — explicit, sticky environment targeting

## Status

✅ Accepted

## TL;DR

The agent's environment target is **explicit** and **sticky**. A mutating operation with no named
environment is refused with a structured, alternatives-listing error whenever more than one
environment is **registered** — at either environment axis (the MCP local handles and burrowd's own
registry), judged by registration alone, never silently sent to whatever kube context or default
namespace happens to be current. A single registered environment is used without ceremony. When a
target's control plane is unreachable or a call errors, the result names the other environments (and,
where cheap, their reachability) so the human can redirect — but the operation stops there: nothing
auto-switches. This turns a class of wrong-environment deploys, and dead-cluster wandering, into an
actionable stop.

Builds on [ADR-0035](0035-environments.md) (environments) and
[ADR-0036](0036-environment-selection.md) (environment selection: local handles, the pin/follow
model); realizes [ADR-0006](0006-guardrails-in-the-control-plane.md) (every operation returns a
structured result the agent can reason over) and extends [ADR-0039](0039-cli-control-plane-version-skew.md)'s
actionable-error posture to the environment-routing seam. The acting environment already lands in the
audit trail ([ADR-0027](0027-audit-log.md)). Supersedes nothing.

## Context

Burrow lets one agent operate many environments ([ADR-0035](0035-environments.md)): local handles in
`~/.burrow/config` map a name to a cluster (a kube context) and a burrowd-registered environment. The
CLI resolves operations through an active environment — a pinned handle or the followed kube context
([ADR-0036](0036-environment-selection.md)). The MCP server exposes the same environments: every
per-app tool takes an optional `env` argument (a handle name) and a low-level `context` override, and
`burrow_environments` lists the handles.

But the agent's default target is **implicit**. When a mutating tool is called with no `env` and no
`context`, the MCP selector routes to the current kube context — whatever the ambient kubeconfig
points at — with no confirmation and no forcing function. The selector's own contract says the agent
"targets explicitly and never rides the human's pin or ambient context," yet the empty-argument path
does exactly ride the ambient context. So a bare instruction like "ship it" routes to whichever
cluster is current, not necessarily the one the human means.

This produced a concrete failure. A developer running two environments — `prod` (their website) and a
throwaway single-VPS cluster — shut the VPS down, then asked an agent to ship a release. The agent
called the deploy and status tools with no `env`, which routed to the now-dead VPS's scoped
credential; the control plane was unreachable; and the agent retried the same dead target rather than
recognizing that `prod` existed and was reachable. It never consulted `burrow_environments`, so it
never learned that `prod` was the real website. Two failure modes compounded: an implicit target (the
wrong cluster by default) and the temptation to wander (a transient or unreachable error inviting a
switch to a different environment).

The immediate mitigation was guidance: the MCP server instructions now tell the agent to name the
environment explicitly, confirm it when several are registered, and never switch environments to work
around a failure. But guidance is soft — it protects only a compliant agent, and the routing itself
still permits an implicit target and gives an unreachable failure no way to name the alternatives.
This ADR makes the safety a property of the system, not of the agent's goodwill.

## Decision

The agent's environment target is explicit and sticky, enforced where the routing lives.

### 1. No implicit target for a mutating operation when the target is ambiguous

A mutating tool — deploy, rollback, scale, autoscale, delete, expose/unexpose, config and secret
writes, add-on and domain mutations — that does not name a target is **refused** when more than one
environment is **registered**, with a structured error that lists the environments (name,
cluster/context, namespace) and instructs the agent to name one. The check is on *registration*, not
reachability: ambiguity is a static fact about how many environments exist, resolved without a network
probe. Reachability enters only on an actual failure (§4).

The guard applies at **both** environment axes ([ADR-0035](0035-environments.md)), because either can
be ambiguous:

- **Cluster-per-env, in the MCP selector.** When more than one local handle is registered in
  `~/.burrow/config` and a mutating call names neither `env` nor `context`, it is refused rather than
  routed to the ambient kube context. This is the axis the motivating incident lived on.
- **Namespace-per-env, in burrowd.** When more than one environment is registered in burrowd's
  registry — the implicit `default` environment plus any named one — and a mutating request arrives
  with no environment, burrowd refuses rather than defaulting to `default`. So registering even a
  single named environment (staging) alongside the default makes a bare mutation name its target.

Either way the result is a first-class, machine-readable refusal
([ADR-0006](0006-guardrails-in-the-control-plane.md)), not a low-level failure — the agent reads it and
re-issues the call with an explicit environment. Neither the MCP server nor burrowd falls back to an
implicit target for a mutating call.

### 2. A single environment is used without ceremony

When exactly one environment is registered at a given axis — one local handle, or only burrowd's
`default` environment — there is no ambiguity and no harm, so the operation proceeds against it with no
forcing function. Friction is added only where it prevents a wrong-environment mutation, so the common
single-environment self-hoster is unaffected.

### 3. Read-only survey stays frictionless but legible

Read-only tools — status, apps, logs, metrics, reachability, cluster, guard, environments — may
default to the current context so the agent can survey before it acts, but every result echoes the
environment it read. The agent always sees which environment a fact came from, so a survey never
silently conflates two.

### 4. The target is sticky; failures name alternatives but never switch

When the target environment's control plane is unreachable or a call errors, the result is structured
and, when other environments are registered, names them (and, where cheap, their reachability) so the
human can redirect. But the operation **stops there**. Neither burrowd, the MCP server, nor the
guidance ever retries a failed operation against a different environment or auto-selects a reachable
one: a transient error on `prod` must never become a deploy elsewhere. Changing the target environment
is always a deliberate, human-directed act.

### 5. The routing intent is made true in code

The selector's stated contract — the agent targets explicitly and does not ride an implicit target —
is realized rather than aspirational. The ambient-context default is scoped to the read-only survey
path (§3) and the unambiguous single-environment case (§2); the mutating path requires an explicit
target when the target is ambiguous (§1). The code and its comment agree.

## Consequences

- A multi-environment user's first mutating call in a task must name the environment — or the agent
  must, prompted by the structured refusal. That is the intended cost: it is exactly the moment a
  wrong-environment deploy would otherwise happen silently.
- Single-environment users — the common self-hoster — see no change.
- An unreachable or dead cluster becomes an actionable stop that names the reachable alternatives,
  instead of an opaque timeout the agent retries or works around.
- The environment a mutating operation acts in is already echoed and recorded ([ADR-0036](0036-environment-selection.md),
  [ADR-0027](0027-audit-log.md)); combined with the [ADR-0039](0039-cli-control-plane-version-skew.md)
  client-version record, the trail now answers who acted, with which client, in which environment.
- The forcing function lives in the MCP/control-plane seam, so it protects any agent, not only one
  that follows the instructions. The shipped instructions and the code now say the same thing.

## Rejected alternatives

- **The agent inherits the CLI's active environment (the pin).** Coupling the agent to the human's
  CLI selection is the implicit-target problem in another form, and it would not have helped here — the
  pin also pointed at the dead VPS. The agent must target explicitly; the pin is the human's CLI state,
  not the agent's.
- **Auto-failover to a reachable environment.** Tempting on an unreachable target, but catastrophic: a
  transient error on `prod` would become a deploy to staging, or the reverse. The operation must stop,
  not relocate. Naming the alternatives for a human to choose is the safe half; acting on them is the
  unsafe half.
- **Guidance only — the shipped instructions as the whole fix.** Necessary but not sufficient:
  instructions protect only a compliant agent, and the routing still permits an implicit target. A
  code-level forcing function is what makes the safety hold regardless of the agent.
- **Always require an explicit environment, even when only one exists.** Needless friction where there
  is no ambiguity and no possible harm; it taxes the common single-environment case to no benefit.
- **A confirm-style guardrail on every mutating op ([ADR-0020](0020-guardrails-as-configurable-policy.md)).**
  The guardrail model gates an operation's *risk*; this is about resolving the operation's *target*
  before risk is even assessed. A missing target is not a held operation — it is an unanswered
  question, better returned as a structured "which environment?" than as a confirm prompt.
