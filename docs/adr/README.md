# Architecture Decision Records

This directory holds Burrow's Architecture Decision Records (ADRs): one decision per
file, numbered, each with its context, the decision, its consequences, and the
alternatives that were rejected.

## Conventions

- **One decision per file.** Files are named `NNNN-short-title.md`.
- **Accepted ADRs are immutable.** Once an ADR is Accepted, its body is not edited.
  A changed or reversed decision is a *new* ADR that names exactly what it supersedes;
  the only edit permitted to the superseded record is its Status line
  (`Superseded by ADR-00NN`). Fixing a typo or a dead link is fine; adding reasoning is
  not — new rationale belongs in a new ADR.
- **ADRs record decisions, not implementation status.** "Accepted" means the decision
  is made, not that the code exists. An ADR ahead of the code is normal. Never write
  "implemented / not yet implemented" into an ADR — track decided-but-unbuilt work in
  [docs/ROADMAP.md](../ROADMAP.md) and [docs/PLAN.md](../PLAN.md), or as a skipped/failing
  test that names the ADR.

## Status values

`Proposed` · `Accepted` · `Rejected` · `Superseded by ADR-00NN`

## Index

| ADR | Title | Status |
| --- | --- | --- |
| [0001](0001-license-and-dco.md) | License, contribution policy, and DCO | Accepted |
| [0002](0002-four-layer-architecture.md) | Four-layer architecture; the control plane is the product | Accepted |
| [0003](0003-agent-neutral-mcp-control-surface.md) | An agent-neutral MCP control surface | Accepted |
| [0004](0004-code-never-over-mcp.md) | Code never travels over MCP; the registry is the conveyor belt | Accepted |
| [0005](0005-mcp-server-holds-no-cluster-credentials.md) | The MCP server holds no cluster credentials | Accepted |
| [0006](0006-guardrails-in-the-control-plane.md) | Guardrails live in the control plane | Accepted |
| [0007](0007-explicit-deploy-by-image-reference.md) | Deploy is an explicit MCP call by image reference | Accepted |
| [0008](0008-two-build-paths.md) | Two build paths for two users | Accepted |
| [0009](0009-honest-status.md) | Honest status: docs never describe unbuilt behavior as done | Accepted |
| [0010](0010-testing-strategy.md) | Testing strategy: seam-isolated units, ephemeral-cluster integration, targeted fault injection; no global simulation harness | Accepted |
| [0011](0011-kubernetes-integration.md) | Kubernetes integration: client-go behind the seam, workload-typed resources (Deployment for v0.1) | Accepted |
| [0012](0012-in-cluster-postgres.md) | Control-plane state runs in an in-cluster Postgres (no external managed database) | Accepted |
