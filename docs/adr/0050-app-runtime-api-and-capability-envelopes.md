# ADR-0050: App-runtime API and capability envelopes

## Status

🟡 Proposed

## TL;DR

Capture a north-star direction: expose a programmatic control-plane API that a **deployed
application itself** can call at runtime to provision infrastructure for its own needs — spin up an
ephemeral sandbox pod behind a preview subdomain, trigger a bounded worker or job — governed not by
per-call human confirmation but by a **capability envelope** an operator grants once (a namespace, a
resource ceiling, a max count, a subdomain pattern, rate limits) that the control plane enforces
while the app operates freely inside it. This generalizes *who drives the control plane* from agent
and human to **agent, human, and app as three clients of one enforced control plane** — the same
boundary, a third caller.

Builds on [ADR-0048](0048-one-off-command-runner.md) (the one-off Job machinery is the natural
narrow seed) and preserves the [ADR-0002](0002-four-layer-architecture.md) /
[ADR-0006](0006-guardrails-in-the-control-plane.md) boundary; relates to
[ADR-0038](0038-scoped-agent-credential.md) (a new scoped credential type is needed). This is a
decision to pursue the *direction*, explicitly deferred behind the compute-first core and the
getting-started path — **not** scheduled work. Supersedes nothing.

## Context

Burrow's control plane is driven today by two kinds of caller: the **agent**, operating the user's
apps through a scoped control channel, and the **human**, doing privileged setup and governance.
Both are interactive — an operation is proposed, gated, and (when risky) confirmed by a person
([ADR-0006](0006-guardrails-in-the-control-plane.md),
[ADR-0020](0020-guardrails-as-configurable-policy.md)).

A deployed application often needs infrastructure of its own, at runtime, on behalf of *its* users —
not the operator's. A product might give each of its users an ephemeral sandbox with a preview
subdomain; a SaaS might spin a bounded worker per tenant request; an app might trigger a one-off job
as part of a user-facing flow. Today the only actor that can provision on the cluster is the agent or
the human, both interactive and both the wrong shape for programmatic, per-request, high-volume
provisioning. The developer who wants this leaves Burrow to find a separate sandbox or
orchestration tool — losing the enforced boundary and the platform they already run on.

Two facts make this worth capturing now, even deferred:

1. **The narrow seed already exists.** [ADR-0048](0048-one-off-command-runner.md) built the machinery
   to launch a bounded, short-lived Kubernetes Job in an app's namespace and reap it. An app
   triggering a bounded job *within its own envelope* is that machinery with a different caller — a
   small step, not a new subsystem.
2. **The human-confirm model does not transfer to runtime volume.** Interactive confirmation is right
   for an operator's occasional risky op; it is the wrong model for an app making many provisioning
   calls per minute. A different enforcement mode is needed — one that is still enforced at the same
   boundary.

## Decision

**Pursue an app-runtime control-plane API governed by capability envelopes, as a direction — not a
committed near-term build.** The through-line: *agent, human, and app become three clients of one
enforced control plane.*

### 1. The app as a third client of the control plane

Expose a programmatic API that a deployed application can call at runtime to provision infrastructure
for its own needs — an ephemeral or sandbox pod with a preview subdomain, a bounded worker or job.
The app is a first-class caller of the control plane alongside the agent and the human. Crucially,
the boundary does not move: the app calls burrowd, and burrowd remains the only layer that talks to
Kubernetes and the only place the grant is enforced ([ADR-0002](0002-four-layer-architecture.md),
[ADR-0006](0006-guardrails-in-the-control-plane.md)). The app holds only a scoped,
envelope-bounded token and can never exceed its grant — the same thesis that makes the agent safe to
point at prod, applied to a new actor.

### 2. Start from the one-off Job seed, not full sandbox hosting

The natural first increment is narrow: an app triggering a **bounded job within its envelope**,
reusing the [ADR-0048](0048-one-off-command-runner.md) one-off Job machinery with the app as caller.
That is a small, well-understood step. Full multi-tenant sandbox hosting — many ephemeral pods, each
with its own subdomain and isolation — is the larger destination, reached incrementally from the
seed, not jumped to directly.

### 3. Capability envelopes: grant once, enforce continuously

The interactive human-confirm model does **not** transfer to high-volume runtime calls. Instead, an
operator grants a **bounded capability envelope once**, and the app operates freely inside it while
the control plane enforces the bounds on every call. An envelope bounds:

- the **namespace** the app may provision into,
- a **resource ceiling** (CPU/memory) per provisioned workload and in aggregate,
- a **maximum count** of concurrent provisioned workloads,
- a **subdomain pattern** the app's previews must fall under,
- **quotas and rate limits** on provisioning calls.

This is a **distinct enforcement mode** — pre-authorized bounded capability — from the interactive,
human-confirmed operation, but it is enforced at the *same boundary*: burrowd checks every app call
against the envelope and refuses anything outside it. The operator's one-time grant replaces
per-call confirmation; the control plane's continuous enforcement replaces the human in the loop.

### 4. Strategic framing: land-and-expand within the same user

This is **land-and-expand within the same solo-developer user**, not a pivot to a new buyer. It lets
the developer already on Burrow grow *on* the platform — building products that provision for their
own users — instead of leaving to adopt a separate tool. It contrasts sharply with single-box,
ephemeral-Docker sandbox tools: Burrow's version is a **platform primitive on real, durable,
multi-tenant, guardrailed Kubernetes**, enforced at the control-plane boundary the developer already
trusts, not a disposable local sandbox.

### 5. Fit with the thesis, not a betrayal of it

This does not weaken Burrow's core claim. The boundary is still the control plane; the app is
credential-scoped and envelope-bounded exactly as the agent is credential-scoped and guardrail-gated.
A new actor and a new enforcement mode extend the model — one enforced control plane, now with three
clients — rather than opening a second, unenforced path to the cluster.

## Consequences

- **The control-plane model generalizes cleanly.** "Who may drive the control plane" becomes agent,
  human, and app — three clients of one boundary — which is a clarifying frame for the whole system,
  not just this feature.
- **A new scoped credential type is required.** An app-runtime credential — distinct from the human
  admin kubeconfig and the scoped agent credential ([ADR-0038](0038-scoped-agent-credential.md)) —
  must be minted, bound to an envelope, and revocable. Designing it is part of the eventual work.
- **Multi-tenant workload isolation is the hard, unsolved part.** Running app-provisioned workloads
  safely on shared infrastructure means NetworkPolicy, ResourceQuota, pod security, noisy-neighbor
  containment, and crash/leak reconciliation. This ADR **names** that difficulty; it does not solve
  it.
- **Platform stickiness, not a new buyer.** The payoff is retaining and growing the existing user,
  which is why the framing stays land-and-expand rather than a market shift.
- **Explicitly deferred.** This comes **after** the compute-first core and the getting-started path
  land and earn adoption. It is a decision to pursue the *direction*, not a scheduled slice, and must
  not be read as committed work.

## Rejected alternatives

- **Tell the developer to adopt a separate sandbox tool.** Rejected as the strategic answer: it loses
  the platform stickiness (the developer leaves to grow) and, worse, loses the enforced boundary — a
  separate single-box sandbox tool has none of Burrow's control-plane guardrails or durable
  multi-tenant substrate.
- **Do it through the agent instead of a runtime API.** Rejected: the agent is the wrong actor and
  the wrong volume for programmatic, per-request provisioning. Per-user runtime provisioning happens
  at machine speed and machine frequency; routing it through the interactive agent loop is a category
  error. The app itself must be the caller, bounded by an envelope.
- **Per-call human confirmation for app provisioning.** Rejected: the interactive confirm model does
  not scale to high-volume runtime calls. The envelope — granted once, enforced continuously — is the
  right shape for a programmatic caller while keeping enforcement at the same boundary.
