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

## Shipped since v0.1

- **Private registry auth** — `burrow registry login/logout/list` provisions a
  `dockerconfigjson` pull Secret with the developer's kubeconfig and attaches it to the app
  namespace's default ServiceAccount; the credential never crosses MCP
  ([ADR-0017](adr/0017-private-registry-authentication.md)). It also made the
  **setup-vs-operation boundary** explicit: `install`/`upgrade`/`registry` act with the
  kubeconfig; `deploy`/`status`/`logs`/`rollback`/`scale` go through burrowd.
- **CLI on Cobra** — the command surface moved to Cobra
  ([ADR-0019](adr/0019-cli-framework-cobra.md)), so the v0.2 commands are built on it.

## Shipped since v0.1 (continued)

- **Guardrails as configurable policy** — the compiled-in, deny-or-allow guardrails are now
  `allow | confirm | deny` policy stored in the control plane and read live by burrowd
  ([ADR-0020](adr/0020-guardrails-as-configurable-policy.md)). `burrow guard list` is
  read-only (and an MCP tool); `burrow guard set` is CLI-only — the agent cannot change its
  own guardrails. The DNS and exposure gates plug in as policy rather than new hardcodes.
  Operators must keep the control plane the agent's only cluster path for the guardrails to
  bind ([ADR-0021](adr/0021-guardrails-require-control-plane-only-agent-access.md),
  [docs/HARDENING.md](HARDENING.md)).

## Now: v0.2 — reach a deployed app at a URL (ingress, TLS, DNS)

**Goal:** an agent can make a deployed app reachable at a real hostname over HTTPS, on the
user's own cluster — the missing half of "deploy and operate" (today a deployed app is only
reachable by port-forward). Reachability is a chain (controller → Service/Ingress → TLS →
DNS), and the design is built around making that chain **introspectable** so the agent can
reason about which link is broken and act on the gaps it owns. The full design — including
the human-setup vs. agent-operation split — is **[ADR-0018](adr/0018-reaching-an-app-at-a-url.md)
(Accepted)**.

The shape (per ADR-0018):

- **A reachability surface** (read-only CLI + MCP tool, folded into `status`): the state of
  each link — controller present + external address, Service/Ingress, TLS cert, DNS — with a
  next action tagged agent-fixable or human-setup.
- **Ingress + cert-manager** via a dedicated setup command (not folded into `burrow install`)
  that **detects an existing controller** and installs one only if absent.
- **`expose` / `unexpose`** — guarded operations through burrowd that create the
  Service + Ingress (RBAC grows to services/ingresses, no credential access).
- **DNS automation** (DigitalOcean first): a provider seam, provider credentials held **only**
  in the control plane and injected into burrowd (not RBAC-read), and `domain add/remove`
  operations with scoped read/write/delete guardrails
  ([ADR-0006](adr/0006-guardrails-in-the-control-plane.md)).

**Build order (all in v0.2 scope):** (1) `expose`/`unexpose` + the reachability surface;
(2) TLS via cert-manager; (3) the DNS-provider seam + `dns configure` + `domain` operations.
Each stage is a thin slice that ends green. Stage 1 is underway: `expose`/`unexpose` are
wired end to end (Kubernetes adapter → engine → API → client → CLI → MCP) behind the new
`expose_public` guardrail (confirm by default), with RBAC for services/ingresses. The
**reachability surface** is next, then TLS, then DNS.

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
