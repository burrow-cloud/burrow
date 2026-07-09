# CLAUDE.md

Burrow is an agent-native cloud platform: it lets an AI coding agent deploy and operate
real applications on a Kubernetes cluster by driving Burrow through the `burrow-agent` CLI,
its scoped control channel ([ADR-0049](docs/adr/0049-burrow-agent-scoped-cli-control-channel.md)).
The agent says "deploy this," "roll it back," "show me the logs," "scale it," and Burrow does
it safely on the user's own cluster.

**Positioning:** the first user is a solo developer or small agency who already has a
Kubernetes cluster (e.g. on DigitalOcean), installs Burrow into it, points their agent at
it, and operates their infrastructure by talking to the agent. **Compute first:** the v0.1
job is deploying someone's code and running it. Databases, domains, autoscaling, and cost
controls come later ([docs/ROADMAP.md](docs/ROADMAP.md)).

This repository is **open source**: the single-tenant control plane, the `burrow-agent`
control channel, and the `burrow` CLI, packaged so a developer can self-host the whole thing. The multi-tenant managed
cloud (billing, teams, dashboard, SSO) is a separate product and does not live
here.

> **Status: pre-implementation.** No feature code has shipped. The foundations (module,
> layout, docs, ADRs) are in place; the v0.1 scope is proposed in
> [docs/PLAN.md](docs/PLAN.md) and awaits maintainer approval. Keep all documentation
> honest about this — features are goals until they ship
> ([ADR-0009](docs/adr/0009-honest-status.md)).

## Critical invariants — never violate these

These are the load-bearing design decisions. Code or docs that break them are wrong even if
tests pass. Each has an ADR.

1. **Code never travels over the agent control channel.** The channel carries tool calls and
   small metadata (an image reference, env vars, a command). The built image moves through a
   **container registry**, never the control channel. *The control channel is the remote
   control; the registry is the conveyor belt.* See
   [ADR-0004](docs/adr/0004-code-never-over-mcp.md); its "over MCP" wording is generalized to
   any control channel (MCP or CLI) by
   [ADR-0049](docs/adr/0049-burrow-agent-scoped-cli-control-channel.md) — the substance is
   unchanged.
2. **The agent control channel holds no cluster credentials; the control plane does.** The
   security boundary is the control plane, not the thin `burrow-agent` client. See
   [ADR-0005](docs/adr/0005-mcp-server-holds-no-cluster-credentials.md), whose principle
   [ADR-0049](docs/adr/0049-burrow-agent-scoped-cli-control-channel.md) migrates from the MCP
   server to `burrow-agent`.
3. **Guardrails live in the control plane**, between agent and cluster. Dangerous
   operations are gated or refused there, and every operation returns a structured result
   the agent can reason over. See [ADR-0006](docs/adr/0006-guardrails-in-the-control-plane.md).
4. **Deploy is an explicit call by image reference.** Passive image-tag watching
   (GitOps auto-deploy) may exist as an optional mode later but is never the spine —
   the explicit call is where the guardrails, the structured feedback, and the rollback
   handle live. See [ADR-0007](docs/adr/0007-explicit-deploy-by-image-reference.md).
5. **The control plane is the product.** It is the only layer that holds cluster
   credentials and the only layer that talks to Kubernetes. The `burrow-agent` control
   channel and the `burrow` CLI are thin clients of its API. See
   [ADR-0002](docs/adr/0002-four-layer-architecture.md).
6. **Honest status.** Everything in the docs is a goal until it ships. Never describe
   unbuilt behavior as done. See [ADR-0009](docs/adr/0009-honest-status.md).

## Go conventions

- Standard Go style: `gofmt`, `go vet`, idiomatic naming. Exported identifiers get doc
  comments.
- Errors are wrapped with context (`fmt.Errorf("...: %w", err)`) and checked, not ignored.
- **No global mutable state; pass dependencies explicitly** so they can be faked in tests.
- **Anything that touches Kubernetes, the container registry, or the database goes
  behind an interface** (a seam) that tests can substitute. Control-plane logic stays pure
  and seam-isolated. Determinism comes from injected dependencies, not from a global
  simulator.
- **Prefer the standard library; keep the dependency graph small.** Every dependency must
  justify itself. The Kubernetes client, an MCP library, and a Postgres driver are expected
  costs; speculative dependencies are not.
- The stack: **Go** for the control plane, the `burrow-agent` control channel, and the CLI. **Kubernetes** as the
  target. **Postgres** (self-hosted, running in the cluster — ADR-0012) for the control
  plane's own state, accessed with **pgx via `database/sql`** and schema-managed by
  **embedded goose migrations** with single-minor-step upgrades (ADR-0013).
  **HTMX** if and when a self-host dashboard is needed. **DigitalOcean** as the reference
  cluster target.

## Build, test, and lint

Targets live in `Taskfile.yml` ([Task](https://taskfile.dev)). They split into **light**
(pure Go, safe anytime) and **heavy** (start a real Postgres or a k3d/Docker cluster):

```sh
task check        # LIGHT pre-commit gate: fmt, vet, build, unit tests, SPDX
task test         # LIGHT: unit tests (integration tests skip without their env vars)
task test:race    # LIGHT: tests under the race detector

task test:pg      # HEAVY: Postgres integration vs an ephemeral local Postgres
task test:k3d     # HEAVY (Docker): Kubernetes integration vs a disposable k3d cluster
task test:integration  # HEAVY: both of the above
```

**`task check` must pass before any commit** — it is all light (no Docker). The heavy
integration targets are run deliberately, not on every commit; their helper scripts
(`scripts/with-test-postgres.sh`, `scripts/with-k3d.sh`) stand up and tear down the
service themselves. CI runs the same checks plus both integration suites
(`.github/workflows/ci.yml`) on GitHub's runners, so the heavy paths are always exercised
remotely even if you only run `task check` locally.

**Testing posture — this is where Burrow DIFFERS from the sibling project Hamster: there is
no global simulation harness. The full rationale is [ADR-0010](docs/adr/0010-testing-strategy.md).**
Burrow delegates consensus, durability, and leader election to Kubernetes (etcd) and
Postgres; it does not implement them, so Hamster's simulated disk/network/crash harness has
no correctness surface to test here. Three layers instead:

- **Seam-isolated unit tests against fakes.** Core logic is pure and seam-isolated:
  Kubernetes, the registry, the **clock**, and the database all behind injected interfaces.
  **No ambient time or I/O in core logic** (no `time.Now()`, no direct network/filesystem) —
  deterministic by construction.
- **Targeted deterministic fault injection** for the deploy state machine and the operator
  reconcile loops — the only parts with distributed-systems-shaped bugs (ordering, retries,
  API conflicts, partial failure, stale watches): a fake k8s client driven through
  adversarial event orderings and injected API errors under a seeded schedule, plus
  controller-runtime envtest.
- **Ephemeral-cluster integration tests** run the real `deploy`, `rollback`, `logs`, and
  `scale` paths against a local **kind or k3d** cluster, covering the real adapters fakes
  cannot. (Wiring lands with the v0.1 slice — see [docs/PLAN.md](docs/PLAN.md).)

## Code layout

All code is licensed **Apache-2.0** ([ADR-0033](docs/adr/0033-relicense-to-apache.md),
[LICENSING.md](LICENSING.md)); every `.go` file carries an `SPDX-License-Identifier: Apache-2.0`
header. The control plane and operator are kept **out of the top-level `internal/`** so a
separate private module (the managed product) can import their public API — a module boundary,
not a license boundary.

**Client surface:**

- [`cmd/burrow`](cmd/burrow/) — the human **admin CLI**. Installs Burrow into a cluster, builds
  and pushes images (client-side build path), wires an agent (`burrow agent <tool> install`), and
  calls the control-plane API directly.
- [`cmd/burrow-agent`](cmd/burrow-agent/) — the **agent control channel** ([ADR-0049](docs/adr/0049-burrow-agent-scoped-cli-control-channel.md)):
  a thin, agent-neutral, credential-free, capability-reduced CLI the agent invokes directly. It
  carries the operate-verbs, outputs JSON first so the agent can compose the result, and holds no
  cluster credentials.
- [`mcp`](mcp/) — the **MCP server** package (binary [`cmd/burrow-mcp`](cmd/burrow-mcp/)): the agent's
  former surface, **retired** by [ADR-0049](docs/adr/0049-burrow-agent-scoped-cli-control-channel.md) and
  no longer shipped in releases. Kept in-tree for now (ADR-0049 §7); not the recommended path.
- [`internal`](internal/) — module-private **shared helpers** only.

**The product:**

- [`controlplane`](controlplane/) — the **control plane** (the product): public API
  (interfaces, the App/Release/Policy domain types, the constructor). Holds cluster
  credentials, runs deploy orchestration, rollout/rollback, logs/status, scaling, the
  guardrails, and the deploy record. Guts in [`controlplane/internal`](controlplane/internal/);
  binary [`cmd/burrowd`](cmd/burrowd/).
- [`operator`](operator/) — the Kubernetes **operator**: CRD types and reconciler entry, guts
  in [`operator/internal`](operator/internal/).

Everything that touches Kubernetes, the registry, the clock, or the database lives
behind a seam (interface) with a real adapter and a fake. The packages above currently hold
only doc-stubs establishing the license boundary and layout; the implementation arrives with
the v0.1 slice ([docs/PLAN.md](docs/PLAN.md)).

## Where the design lives

- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) — the system design narrative: the four
  layers, the load-bearing invariants, the request paths (deploy, status, logs, scale,
  rollback), components, and state.
- [`docs/ROADMAP.md`](docs/ROADMAP.md) — the v0.x → v1.0 milestones.
- [`docs/PLAN.md`](docs/PLAN.md) — the current execution plan: what is being worked now and
  next, and the explicit v0.1 scope and out-of-scope list. The front line only, pruned as
  work lands.
- [`docs/adr/`](docs/adr/) — Architecture Decision Records. One decision per file, with the
  reasoning and the rejected alternatives. Start at the [index](docs/adr/README.md).

## Development workflow

- The plan lives in the repo at two altitudes: high-level milestones per version in
  [docs/ROADMAP.md](docs/ROADMAP.md), and the current execution plan — what is being worked
  now and next — in [docs/PLAN.md](docs/PLAN.md). PLAN.md is the front line only and is
  pruned as work lands (no growing TODO/kanban graveyard — a completed item's record
  survives in git history, its now-green test, and the shipped ADR/doc).
- **Work in phases: one branch and one pull request per phase.** Each phase ends green
  (`go build`, `go vet`, `gofmt`, `go test ./...`, and the SPDX check all pass) before its PR
  is opened against `main`. Keep pull requests small and focused: one issue, one concern.
- **Do the coding work in a sub-agent, not the main thread.** For any non-trivial change,
  delegate the whole loop to a sub-agent: it explores, implements, writes tests, runs
  `task check`, opens the PR, then **watches the PR through the merge queue and re-kicks it when
  the heavy k3d/Postgres suite flakes** (the transient "apiserver not ready"), and reports back a
  **concise** result (what changed and why, test outcome, PR number, merge status). The main
  thread **reviews that result** — pulling the diff only when needed — and does not hand-edit
  files or run its own background polling loops to watch PRs: those loops are unreliable (the main
  loop misses the merge) and burn context. Reserve direct main-thread edits for tiny, mechanical
  fixes. This keeps the main context lean and review deliberate.
- **Semver from v0.1 toward v1.0.**
- **Sign every commit with `git commit -s`** (Developer Certificate of Origin, required on
  all commits for provenance — [ADR-0001](docs/adr/0001-license-and-dco.md)).
- **Do not add AI/agent attribution to commits or PRs.** No `Co-Authored-By: Claude`
  trailer, no "Generated with Claude Code" line, no `Claude-Session` trailer — commit
  messages and PR descriptions carry only their own content and the DCO sign-off.
- **Write commit messages and PR descriptions in the maintainer's outward voice.** They are
  the author's own record to the project, not a reply to the maintainer. Describe the change
  and its rationale impersonally — never address the reader in the second person or reference
  "your" decision/point/request (e.g. "Built around your point that…", "Records your call",
  "Closes the gap you hit"). State the thing itself ("Adopt Cobra because…", "Fix the 401 by…").
  The same outward voice applies to the README and other repo prose.
- **All code is Apache-2.0** ([LICENSING.md](LICENSING.md),
  [ADR-0033](docs/adr/0033-relicense-to-apache.md)). Each new `.go` file gets an
  `SPDX-License-Identifier: Apache-2.0` header; the CI check (`scripts/check-spdx.sh`) enforces
  it. The code in this repo is open source; the managed cloud and enterprise tier are a
  separate product that does not live here ([ADR-0009](docs/adr/0009-honest-status.md)).
- **Outside code is not merged under the DCO alone** — the maintainer keeps sole copyright
  for commercial licensing, so outside-code PRs are declined or CLA-gated; issues and
  discussions are the open way to contribute ([CONTRIBUTING.md](CONTRIBUTING.md)).
- **Accepted ADRs are immutable.** Once an ADR is Accepted, do not edit its body. A changed
  or reversed decision is a *new* ADR that names exactly what it supersedes; the only edit
  permitted to the superseded one is its Status line (`Superseded by ADR-00NN`). Fixing a
  typo or dead link is fine; adding reasoning is not.
- **ADRs record decisions, not implementation status.** "Accepted" means the decision is
  made, not that the code exists yet — an ADR ahead of the code is normal. Never write
  "implemented / not yet implemented" into an ADR. Track decided-but-unbuilt work in
  [docs/ROADMAP.md](docs/ROADMAP.md) and [docs/PLAN.md](docs/PLAN.md), or as a skipped or
  failing test that asserts the ADR's behavior and names it.
- Keep docs and code in step in the same pull request. A design doc may be *corrected* when
  it states something factually wrong about the system, but progress tracking stays out of
  both ADRs and design-doc prose. Docs that contradict the code are worse than no docs.
- Keep the [README](README.md) version-status table current as releases move: the version
  under active work is `🚧 in progress`, shipped ones are ✅ (linked to their release),
  later ones `planned`. It is the user-facing status surface and must never lag the code
  ([ADR-0009](docs/adr/0009-honest-status.md)).

## Naming

Use **standard vocabulary** for system components in code, docs, CLI, and logs: cluster,
deployment, rollout, image, registry, control plane, agent control channel (`burrow-agent`),
agent. Burrow is the brand, not an operational vocabulary — **do not invent themed names for
system internals.**
