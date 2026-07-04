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

## Format

New ADRs follow [template.md](template.md): a **Status** badge, then a one-paragraph **TL;DR**
(the decision in brief, with its relationships to other ADRs), then Context, Decision,
Consequences, and Rejected alternatives. Status badges:

✅ `Accepted` · 🟡 `Proposed` · ❌ `Rejected` · ♻️ `Superseded by ADR-00NN`

Earlier records predate this format; it applies going forward (Accepted ADRs are immutable, so
they are not reformatted).

## Index

| ADR | Title | Status |
| --- | --- | --- |
| [0001](0001-license-and-dco.md) | License, contribution policy, and DCO | Accepted (license decision superseded by ADR-0033; DCO/CLA stance stands) |
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
| [0013](0013-database-migrations-and-upgrade-policy.md) | Database migrations (embedded goose) and the single-minor-step upgrade policy | Accepted |
| [0014](0014-self-host-connectivity-via-kubeconfig.md) | Self-host connectivity via the developer's kubeconfig and the API-server proxy (refines ADR-0005) | Accepted (token-header detail corrected by ADR-0015) |
| [0015](0015-token-header-only-x-burrow-token.md) | Burrow's token is sent only in X-Burrow-Token (corrects ADR-0014) | Accepted |
| [0016](0016-cli-distribution-and-upgrade-lifecycle.md) | CLI distribution via Homebrew and the CLI-driven upgrade lifecycle | Proposed |
| [0017](0017-private-registry-authentication.md) | Private registry authentication via a developer-provisioned pull secret | Accepted |
| [0018](0018-reaching-an-app-at-a-url.md) | Reaching a deployed app at a URL — ingress, TLS, and DNS, with a reachability surface | Accepted (credential detail refined by ADR-0023) |
| [0019](0019-cli-framework-cobra.md) | Cobra for the CLI command framework; stdlib tabwriter for output | Accepted |
| [0020](0020-guardrails-as-configurable-policy.md) | Guardrails as inspectable, configurable policy with a confirm disposition | Accepted |
| [0021](0021-guardrails-require-control-plane-only-agent-access.md) | Guardrails bound the control-plane path; the operator restricts the agent's other paths | Accepted |
| [0022](0022-routing-backend-and-supported-kubernetes.md) | HTTP routing via a shared ingress (Ingress now, Gateway-ready) and supported Kubernetes versions | Accepted |
| [0023](0023-provider-credentials.md) | Provider credentials — a registry of vendor tokens in one scoped Secret | Accepted (credential transport superseded by ADR-0030) |
| [0024](0024-cli-command-taxonomy.md) | Noun-grouped CLI command taxonomy (`app` / `config` / `system`, `expose`→`publish`) | Accepted |
| [0025](0025-building-block-addons.md) | Building-block add-ons — a curated catalog of vetted, self-hostable backing services (observability first; the agent is the query layer) | Accepted |
| [0026](0026-observability-query-adapters.md) | Observability add-ons — query adapters over installed *or* existing backends (`install` vs `connect`, capabilities derived) | Accepted |
| [0027](0027-audit-log.md) | Audit log — an append-only Postgres record of agent operations and guardrail decisions (allowed / held / denied / executed) | Accepted |
| [0028](0028-app-config-and-secrets.md) | App config and secrets — a lifecycle `app env` / `app secret` store (deploy takes no env); secret values stay off MCP, out of Postgres, in a per-app Secret | Accepted (secret transport superseded by ADR-0029) |
| [0029](0029-secrets-through-the-control-plane.md) | Secrets traverse the control-plane API (CLI/UI/managed), never MCP; default app namespace → `burrow-apps` | Accepted |
| [0030](0030-credentials-through-the-control-plane.md) | Burrow-owned credentials (provider + connected-backend tokens) traverse the control-plane API, never MCP; burrowd gains name-scoped `update` on `burrow-credentials` | Accepted |
| [0031](0031-postgres-addon.md) | The Postgres add-on — one shared instance, a database and role per app; the generated `DATABASE_URL` lands in the app's per-app Secret | Accepted |
| [0032](0032-postgres-backups.md) | Postgres backups — logical `pg_dump`/`pg_restore` via Jobs to a backup PVC, recorded in the control-plane database; restore is confirm-gated | Accepted |
| [0033](0033-relicense-to-apache.md) | Relicense the whole repository to Apache-2.0; monetize via managed cloud + a proprietary enterprise tier + risk transfer | Accepted |
| [0034](0034-agent-native-onboarding.md) | Agent-native onboarding — detect cluster capabilities (no cluster-type picker), provision missing substrate on cost-aware consent, prove the first URL | Accepted |
| [0035](0035-environments.md) | Environments — context-routed clusters (cluster-per-env) then namespace-scoped environments, each with its own guardrail policy ("free in staging, gated in prod") | Accepted |
| [0036](0036-environment-selection.md) | Environment selection — one `burrow env` surface that follows the kube context, named local handles (`~/.burrow/config`), `burrow scan`; retires the `burrow context` command | Accepted |
| [0037](0037-cli-onboarding-and-organization.md) | CLI onboarding and command organization — intent-based help groups, explicit positional `install <context>` (server-side apply, no kubectl), first-run config awareness; drops `system` into `cluster` | Accepted |
| [0038](0038-scoped-agent-credential.md) | Scoped agent credential — `install` mints a `burrow-agent` ServiceAccount + narrow RBAC and writes a burrowd-only kubeconfig to `~/.burrow/`; the human keeps the admin kubeconfig; shared SA now, a `principal` seam for per-user SSO later | Accepted |
| [0039](0039-cli-control-plane-version-skew.md) | Version skew between the CLI and the control plane — burrowd is the compatibility anchor, backward-compatible one minor back, never a hard block on version difference; a client-version header turns new-client-against-old-server into an actionable "run `burrow upgrade`" error | Accepted |
| [0040](0040-burrowd-never-contacts-the-registry.md) | Burrowd never contacts the registry — remove the pre-deploy image resolve (it recorded a digest nothing used and needed a credential ADR-0017 forbids burrowd); Kubernetes resolves and pulls via the imagePullSecret, and the digest is read back from pod status | Accepted |
| [0041](0041-flatten-path-to-a-reachable-app.md) | A flatter path to a reachable app — deploy gives every app a ClusterIP Service by default (given a port), and a single publish operation chains Service, Ingress, TLS, DNS, and cert-wait with structured prerequisite feedback, so the agent does one thing for "make it reachable" | Accepted |
| [0042](0042-use-existing-ingress-controller.md) | Use the cluster's existing ingress controller — detect the running controller and bind expose plus the cert-manager HTTP-01 solver to its IngressClass instead of hardcoding nginx, installing ingress-nginx only when none exists, so k3s/k3d (traefik) and any pre-provisioned cluster work without a redundant second controller | Accepted |
| [0043](0043-public-reachability-is-a-loadbalancer.md) | Public reachability is a LoadBalancer, not NodePort — expose the ingress controller via a type=LoadBalancer Service (a real public IP, from a cloud provider, servicelb, or MetalLB), keep internal on ClusterIP, drop NodePort as a user choice, and detect LB support from whatever services LoadBalancers (not just a known cloud), proven by an e2e where servicelb assigned an IP while detection reported unsupported | Accepted |
| [0044](0044-single-vps-k3s-cluster.md) | Provision a single-VPS k3s cluster — a one-time on-VPS `curl \| sh` bootstrap installs k3s (node IP = free servicelb LoadBalancer, no cloud LB) + burrowd and prints a `burrow join <token>`; running it on the laptop lands both admin and scoped creds, so after the single SSH bootstrap all ops run from the laptop. Burrow never SSHes (Option A rejected), no k3sup dependency | Accepted |
| [0046](0046-registry-onboarding.md) | Registry onboarding — reduce the friction of getting an image into a registry, ranked by user. Primary: auto-wire the developer's existing code-provider registry (GHCR/GitLab) using the code-host token, so `docker login` + the pull secret are configured for them and no external setup remains. Second, for the no-added-cost self-hoster: an in-cluster zot registry published at `registry.<domain>` (real Let's Encrypt cert + auth) through the free servicelb ingress — native throughput, nothing to distribute, no cloud LB; an optional port-forward covers cold starts and small images. Self-signed NodePort, control-plane-mediated push, a type=LoadBalancer registry, and Harbor all rejected. Held at Proposed pending user signal that onboarding is painful | Proposed |
