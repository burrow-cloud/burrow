# ADR-0045: The OSS/enterprise boundary — a single-tenant core the managed product extends

## Status

✅ Accepted

## TL;DR

This open-source repo is a **single-tenant control-plane engine** (burrowd, plus its thin CLI and
MCP clients). The managed/enterprise product — SSO, multi-tenancy, teams, billing, a dashboard —
is a **separate private Go module that imports the OSS control plane's public API and wraps it,
never a fork.** Three seams make that work and must be preserved as the OSS core grows: (1) the
**module boundary** (`controlplane`/`operator` live outside top-level `internal/` so a private
module can import them), (2) the **principal seam** ([ADR-0038](0038-scoped-agent-credential.md))
where an authenticated identity is resolved (constant `"shared-agent"` in OSS; SSO plugs in here),
and (3) a **pluggable control-plane transport/auth in the CLI** (self-host reaches burrowd over the
kubeconfig → API-server proxy; enterprise uses an SSO token to the managed endpoint). SSO
integrates at the principal seam, and the agent acts on behalf of the logged-in user. Builds on
[ADR-0002](0002-four-layer-architecture.md), [ADR-0005](0005-mcp-server-holds-no-cluster-credentials.md),
[ADR-0038](0038-scoped-agent-credential.md), and [ADR-0009](0009-honest-status.md) (the managed
cloud is a separate product). Supersedes nothing.

## Context

CLAUDE.md and [ADR-0009](0009-honest-status.md) already declare that the multi-tenant managed cloud
(billing, teams, dashboard, SSO) is a separate product that does not live in this repo. But that
declaration has not been pinned to the specific *seams* that let a private module extend the OSS
core without forking it. As v0.9+ adds features (VPS provisioning, the unified publish, database
provisioning), it is easy to accidentally break those seams — hardcode identity at a call site,
bury the control-plane client behind a concrete transport, or leak tenancy into the engine — and
only discover it when the private repo cannot cleanly wrap the core.

Some of the seams already exist: the module boundary (`controlplane`/`operator` outside
`internal/`, stated in `controlplane/doc.go`) and the principal resolver
([ADR-0038](0038-scoped-agent-credential.md), a package-var substitution point in
`controlplane/audit.go`, constant `"shared-agent"` today). This ADR names the full set and the CLI
integration so they are preserved deliberately, and states the one refactor the OSS core owes to
keep the boundary clean.

## Decision

1. **The OSS repo is a single-tenant engine.** burrowd is a single-tenant control plane; the CLI
   and MCP server are its thin clients. No auth provider, multi-tenancy, org/team model, billing,
   or dashboard lives in this repo. The engine holds cluster credentials and enforces guardrails
   ([ADR-0002](0002-four-layer-architecture.md), [ADR-0006](0006-guardrails-in-the-control-plane.md));
   it does not know about users or tenants.

2. **The managed product is a private module that imports the OSS public API.** It wraps burrowd
   with SSO (OIDC/SAML), per-principal authorization (teams, roles), multi-tenancy (organizations,
   many clusters), billing, and a dashboard. It **imports** `controlplane`'s public API — kept
   outside `internal/` for exactly this — rather than forking the engine.

3. **SSO integrates at the principal seam.** The acting identity flows through the ADR-0038
   principal resolver, a single substitution point (constant `"shared-agent"` in OSS). The managed
   auth layer validates the SSO token and resolves the real user principal; the audit log records
   the true user and a per-principal authorization layer (enterprise-only) enforces who may do
   what. The agent acts **on behalf of** the logged-in user, so its actions are attributed and
   authorized as that user rather than as an anonymous shared agent.

4. **The CLI's control-plane transport and auth is a seam.** Self-host reaches burrowd over the
   kubeconfig → Kubernetes API-server proxy path (the scoped credential, unchanged). Enterprise
   adds a `burrow login` (an OIDC device/browser flow) that stores a token and talks to the managed
   endpoint over authenticated HTTPS. The control-plane *client* is an interface so the **same CLI
   serves both**; the login/OIDC plumbing lives in the private module. Preferred model: **pluggable
   auth in one CLI**. The alternative — extracting the CLI command tree into an importable package
   so a private CLI can extend it — stays available if a heavier enterprise CLI is ever wanted.

## Consequences

- **Preserve the module boundary:** keep `controlplane`/`operator`'s public API stable and outside
  `internal/`; that import surface is the seam.
- **Preserve the principal seam:** never hardcode the acting identity at a call site; always route
  it through the resolver so the managed product substitutes SSO identity without touching the
  engine.
- **The one refactor this ADR calls for in OSS:** make the CLI's control-plane client transport/auth
  an explicit interface (self-host kubeconfig-proxy as the default implementation), so the
  enterprise SSO transport slots in without a fork. Do this before the CLI accretes more
  transport-coupled call sites.
- **Keep tenancy/auth/billing out of the engine.** They belong in the private module; leaking them
  into OSS bloats the single-tenant core and blurs the open/commercial line.
- **Honest status holds:** the OSS repo never ships or advertises the managed features
  ([ADR-0009](0009-honest-status.md)); an ADR ahead of the private code is fine.
- The scoped-agent-credential model ([ADR-0038](0038-scoped-agent-credential.md)) generalizes:
  `"shared-agent"` is the degenerate single-user case of the SSO principal.

## Rejected alternatives

- **Fork the OSS core for the enterprise product.** Guarantees drift and double maintenance; the
  import-the-public-API boundary is the whole point of keeping `controlplane` out of `internal/`.
- **Build SSO and multi-tenancy into the OSS control plane behind flags.** Bloats the single-tenant
  engine, couples it to an auth model, and blurs the open/commercial boundary. Keep it in the
  private module.
- **A separate enterprise CLI that reimplements the commands.** Fork risk and divergence; pluggable
  auth in one CLI keeps a single command surface for both self-host and enterprise.
