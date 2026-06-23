# ADR-0010: Testing strategy — seam-isolated units, ephemeral-cluster integration, targeted fault injection; no global simulation harness

## Status

Accepted.

## Context

Burrow's sibling project, Hamster, is built around a **global deterministic simulation
harness**: a single event queue with virtual time, a seeded PRNG, a faulty network, and a
crash-faithful disk, under which the whole store runs against adversarial failure schedules.
That harness is justified there because Hamster *implements its own* consensus (Raft),
durability (erasure coding), and leader election — the catastrophic-correctness surface
where a subtle ordering or crash bug silently loses or corrupts data. Simulating time,
network, and disk is the only way to test code that owns those properties.

Burrow is a different kind of system. It does **not** implement consensus, durability, or
leader election. It **delegates** them: Kubernetes (backed by etcd) owns cluster state and
leader election; Postgres (Neon) owns the control plane's own durable state. Burrow's job is
to *orchestrate* those external systems safely — to drive deploys, rollouts, rollbacks,
logs, and scaling through the Kubernetes API and to record the deploy history in Postgres.

The catastrophic-correctness surface that justifies Hamster's harness therefore **does not
exist inside Burrow**. Porting that harness would mean building a simulated etcd/Kubernetes
and a simulated Postgres — reimplementing the correctness surface of the very systems Burrow
chose not to own — to test code that does not contain the bugs the harness is designed to
find. The distributed-systems-shaped bugs Burrow *can* have are real but narrower: event
ordering, retries, Kubernetes API conflicts, partial failure, and stale watches in the
deploy state machine and the operator's reconcile loops.

This ADR records why Burrow's testing diverges from Hamster's, so the divergence is explicit
and reviewable rather than an unexamined omission.

## Decision

Burrow's testing has three layers, and explicitly excludes a global simulation harness.

1. **Seam-isolated unit tests against fakes.** Core control-plane logic stays **pure and
   seam-isolated**: Kubernetes, the container registry, the clock, and the database all sit
   behind injected interfaces. **No ambient time or I/O in core logic** — no
   `time.Now()`, no direct network or filesystem access; the clock and every external system
   are parameters. Core logic is unit-tested against in-memory fakes of those seams, which is
   deterministic by construction.

2. **Ephemeral-cluster integration tests.** The real `deploy`, `rollback`, `logs`, and
   `scale` paths run against an **ephemeral local Kubernetes cluster (kind or k3d)** in CI.
   These cover the real Kubernetes client adapter and the real API behaviors that a fake
   cannot.

3. **Targeted deterministic fault-injection tests** for the only parts with
   distributed-systems-shaped bugs — the **deploy state machine** and the **operator
   reconcile loops**. A fake Kubernetes client is driven through **adversarial event
   orderings and injected API errors under a seeded schedule** (conflicts, not-found, stale
   watches, partial failures, retries), and the operator's reconcilers are additionally
   tested under controller-runtime **envtest** (a real API server + etcd, no kubelet).

Determinism comes from **injected dependencies and seeded schedules**, not from a global
event simulator.

## Consequences

- The seam discipline is a hard rule, not a nicety: anything that touches Kubernetes, the
  registry, the clock, or the database goes behind an interface, so units stay fast,
  deterministic, and fake-driven. This is mirrored in [CLAUDE.md](../CLAUDE.md) and
  [docs/PLAN.md](../PLAN.md).
- CI runs three tiers: fast seam unit tests everywhere; fault-injection tests around the
  state machine and reconcilers; and ephemeral-cluster (kind/k3d) plus envtest integration
  for the real adapters.
- The fault-injection layer is where Burrow's equivalent of "adversarial schedules" lives —
  scoped to the orchestration logic Burrow owns, not to simulated storage or network.
- There is **no** simulated disk, simulated network, or crash-faithful storage layer, and no
  global virtual-time event queue. A contributor expecting Hamster's harness will not find
  one, by design — this ADR is the reason.
- If Burrow ever takes on self-implemented consensus or durability (not anticipated — it
  delegates both), this decision would be revisited in a new ADR.

## Rejected alternatives

- **Port Hamster's global deterministic simulation harness.** Rejected: Burrow's correctness
  lives in orchestrating external systems (Kubernetes/etcd, Postgres) that it cannot
  faithfully simulate without reimplementing them, not in self-implemented
  consensus/durability. The harness would test a correctness surface Burrow does not own and
  miss the orchestration bugs it does have.
- **Integration tests only (skip the fakes).** Rejected: ephemeral-cluster tests are slower
  and coarser; they cannot exhaustively drive the adversarial orderings and injected errors
  that the state machine and reconcilers must survive. Fast seam units plus targeted fault
  injection cover that space; integration tests confirm the real adapters.
- **Fakes only (skip the real cluster).** Rejected: fakes cannot reproduce real Kubernetes
  API semantics, admission, and watch behavior; the real deploy/rollback/logs/scale paths
  must run against a real API server (kind/k3d, envtest) before they can be trusted.
