# Burrow Plan — current execution plan

> **This file is the front line only.** It holds what is being worked now and next, in
> priority order, and is pruned as work lands — no growing TODO graveyard. Coarse
> milestones live in [ROADMAP.md](ROADMAP.md); a completed item's record survives in git
> history, its now-green test, and the shipped ADR/doc.

## Shipped: v0.1 — the thin vertical slice ✅

An agent operates a real application on the user's own Kubernetes cluster, end to end,
safely — proven against the reference DigitalOcean cluster. `burrow install` lands the
control plane and an in-cluster Postgres; the CLI and MCP server reach it through the
Kubernetes API-server proxy using the developer's kubeconfig
([ADR-0014](adr/0014-self-host-connectivity-via-kubeconfig.md),
[ADR-0015](adr/0015-token-header-only-x-burrow-token.md)); an agent connected over MCP can
`deploy` by image reference, then `status`, `logs`, `rollback`, and `scale`, every mutating
call passing through the control-plane guardrails
([ADR-0006](adr/0006-guardrails-in-the-control-plane.md)). `burrow upgrade` rolls the
control plane forward in place ([ADR-0016](adr/0016-cli-distribution-and-upgrade-lifecycle.md)).
The detail lives in git history, the now-green tests (unit + k3d integration + the capstone
e2e), and the ADRs.

## Now: private registry auth (landing first)

Real apps live in private registries, so pulling a private image is table-stakes and lands
ahead of the URL work. `burrow registry login/logout/list` provisions a `dockerconfigjson`
pull Secret with the developer's kubeconfig and attaches it to the app namespace's default
ServiceAccount, so app Pods inherit it — the control plane, burrowd's RBAC, and the deploy
path are untouched, and the credential never crosses MCP
([ADR-0017](adr/0017-private-registry-authentication.md)). This also makes explicit the
**setup-vs-operation boundary**: `install`, `upgrade`, and `registry` act with the
kubeconfig; `deploy`/`status`/`logs`/`rollback`/`scale` go through burrowd.

## Next: v0.2 — reach a deployed app at a URL (ingress, domains, TLS)

**Goal:** an agent can make a deployed app reachable at a real hostname over HTTPS, on the
user's own cluster — the missing half of "deploy and operate" (today a deployed app is only
reachable by port-forward). The capability is security-sensitive (DNS write access, public
exposure), so it is **designed before it is built**.

The three pieces, in the order they unlock value:

1. **HTTP routing.** An in-cluster ingress controller (e.g. ingress-nginx) plus an MCP/CLI
   surface to expose a deployed app at a hostname — `expose <app> --host <name>` — creating
   the Service/Ingress through the same guarded control-plane path as deploy. Reachable over
   HTTP once DNS points at the cluster.
2. **TLS.** Automatic certificates (ACME via cert-manager) so the hostname serves HTTPS with
   no manual cert handling.
3. **DNS automation.** A DNS-provider seam (DigitalOcean first, matching the reference
   target; others behind the seam) so the agent can point a domain at the cluster. DNS write
   access is the sharp edge — it gets scoped guardrails (read vs. write vs. delete) and, like
   the cluster credentials, its provider credentials live **only** in the control plane
   ([ADR-0005](adr/0005-mcp-server-holds-no-cluster-credentials.md)).

**Next step (before code): a design ADR** for the ingress/domains/TLS approach — the
controller and cert-manager choices, the Ingress resource model behind the Kubernetes seam
([ADR-0011](adr/0011-kubernetes-integration.md)), the new tool surface, the DNS-provider
seam, and the DNS guardrail policy. Then the thin first slice (piece 1) lands behind it.

### Out of scope for v0.2 (explicit)

Kept out to keep the slice thin and the docs honest ([ADR-0009](adr/0009-honest-status.md)):

- **Server-side build from a git reference** ([ADR-0008](adr/0008-two-build-paths.md)) and
  **richer / configurable guardrail policy** — both real near-term candidates
  ([ROADMAP.md](ROADMAP.md)), but sequenced after the URL story unless reprioritized.
- **Database provisioning, autoscaling, cost controls, multi-tenancy, GitOps auto-deploy,
  and a self-host dashboard** — later milestones; see [ROADMAP.md](ROADMAP.md).

## Testing posture (unchanged)

Burrow **differs from Hamster** — there is no global simulation harness
([ADR-0010](adr/0010-testing-strategy.md)): seam-isolated unit tests against fakes (k8s, the
registry, the clock, the database, and now the DNS provider behind injected interfaces);
targeted deterministic fault injection for the reconcile/deploy paths; and ephemeral-cluster
(k3d) integration plus the capstone e2e for the real adapters.

## Status of the blocking decisions

- **License: settled.** [ADR-0001](adr/0001-license-and-dco.md) is **Accepted** — Apache-2.0
  client surface, FSL-1.1-ALv2 control plane and operator, sole ownership with CLA-gated
  outside code.
