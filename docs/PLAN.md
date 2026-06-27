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

- **Private registry auth** — `burrow config registry login/logout/list` provisions a
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

## Shipped: v0.2 — reach a deployed app at a URL (ingress, TLS, DNS) ✅

Released as **v0.2.0**. An agent can make a deployed app reachable at a real hostname over
HTTPS on the user's own cluster — the missing half of "deploy and operate." Reachability is a
chain (controller → Service/Ingress → TLS → DNS), built to be **introspectable** so the agent
can reason about which link is broken and act on the gaps it owns. The full design — including
the human-setup vs. agent-operation split — is **[ADR-0018](adr/0018-reaching-an-app-at-a-url.md)
(Accepted)**.

## Next (v0.4) — agent-provisioned building blocks

The differentiator: the agent stands up and operates a whole stack on the user's cluster, not
just an app. The user asks for a capability; the agent writes the integration code; **Burrow
provisions a vetted, self-hostable, permissively-licensed (Apache / MIT / BSD) backing service
with sane defaults and operates it behind the guardrails**, then hands the agent the connection
details. The model — a curated catalog plus a registry of installed instances, reusing the
provider-registry / guardrail / credential-Secret patterns — is
[ADR-0025](adr/0025-building-block-addons.md). First slices: a **cache**
([ValKey](https://valkey.io), BSD-3) and **metrics**
([VictoriaMetrics](https://victoriametrics.com) / Prometheus, Apache-2.0), then
observability-driven answers ("how is my app doing?" / "why is it slow?") over the logs and
metrics the agent set up, plus **`app delete`** (with a delete guardrail). Log aggregation only
if Kubernetes' built-in logs prove insufficient, and with a permissively-licensed store
(VictoriaLogs, not AGPL Loki). See [ROADMAP.md](ROADMAP.md).

**Deferred until requested:** server-side build from a git reference
([ADR-0008](adr/0008-two-build-paths.md)) — client-side build plus deploy-by-image-reference
covers the common case today. Smaller TLS/DNS follow-ons (a DNS-01 issuer, folding the
provider's record into reachability) ride along when a building-block slice needs them.

Shipped in **v0.3**: the CLI regrouped by task (`app`/`config`/`system`, `expose`→`publish` —
[ADR-0024](adr/0024-cli-command-taxonomy.md)) with `app list`; the Cloudflare adapter verifying
account-scoped (`cfat_`) tokens by listing zones; the app Ingress bound to the ingress-nginx
class so it gets an address; reachability resolving via public DNS so a freshly added record is
seen (the chain converges for an agent); and a burrowd request log.

Shipped in **v0.2.1** (patch): quieter `install`/`upgrade` output with `--verbose`, helpful
CLI argument errors, ko-built images (no Dockerfile) with a warm CI build cache, a read-only
`burrow_providers` MCP tool, and `domain add/remove` auto-selecting the sole configured DNS
provider so `--provider` is optional.

<!-- v0.2 build detail below is retained for now; prune as the next front line forms. -->

The shape (per ADR-0018):

- **A reachability surface** (read-only CLI + MCP tool, folded into `status`): the state of
  each link — controller present + external address, Service/Ingress, TLS cert, DNS — with a
  next action tagged agent-fixable or human-setup.
- **Ingress + cert-manager** via a dedicated setup command (not folded into `burrow install`)
  that **detects an existing controller** and installs one only if absent.
- **`expose` / `unexpose`** — guarded operations through burrowd that create the
  Service + Ingress (RBAC grows to services/ingresses, no credential access).
- **DNS automation** (DigitalOcean / Cloudflare): a **provider registry** — vendor tokens in
  one `burrow-credentials` Secret with the structure in the database, read by burrowd through
  a `resourceNames`-scoped `get` so adding or rotating a token needs no restart
  ([ADR-0023](adr/0023-provider-credentials.md)) — a DNS-provider seam over it, and
  `domain add/remove` operations with scoped read/write/delete guardrails
  ([ADR-0006](adr/0006-guardrails-in-the-control-plane.md)).

**Build order (all in v0.2 scope):** (1) `expose`/`unexpose` + the reachability surface;
(2) TLS via cert-manager; (3) the provider registry + DNS-provider seam + `domain` operations.
Each stage is a thin slice that ends green. Stage 1 is landing: `expose`/`unexpose` are
wired end to end behind the `expose_public` guardrail (confirm by default), and the
**reachability surface** (`burrow app reachability`, `burrow_reachability`) reports the chain —
deployed → ready → exposed → external address → DNS — as a one-line plain summary plus the
full structured chain (ADR-0022's layered model). It reads the external address from the
app's own Ingress status, so it needs **no new RBAC**. `expose --tls` now requests an HTTPS
certificate: the Ingress carries the `cert-manager.io/cluster-issuer` annotation + a TLS
stanza, and cert-manager (once installed) fills the cert; reachability reports whether TLS is
configured. Stage 3 has landed: the **provider registry** (`burrow config provider add/list`) records
a vendor credential — writing the token into the one `burrow-credentials` Secret with the
developer's kubeconfig and the non-secret registry (type, capabilities, Secret key) in the
database; `burrow install` creates the empty Secret and burrowd's only secrets grant, a
`resourceNames`-scoped `get` on it ([ADR-0023](adr/0023-provider-credentials.md)). On top of
it the **DNS-provider seam** (DigitalOcean + Cloudflare adapters over `net/http` + a fake)
verifies a token on `provider add` and now manages records, and **`domain add/remove`**
(`burrow app domain …`, `burrow_domain_add` / `burrow_domain_remove`) point a host at the
cluster's external address through a configured provider — guarded by the `dns_write` /
`dns_delete` guardrails (confirm by default). burrowd reads the token at call time and is the
only thing that talks to the vendor. **`burrow system ingress install`** closes the manual gap: a
setup command that detects an existing ingress-nginx / cert-manager and installs only what is
missing (pinned upstream manifests via the developer's kubeconfig), then waits for cert-manager
and creates a Let's Encrypt ClusterIssuer (HTTP-01, named `letsencrypt` to match
`expose --tls`). With that, the v0.2 reachability chain is end to end, and `burrow app domain add
--app <app>` reads the controller-assigned address from the app's ingress so neither the human
nor the agent copies an IP by hand. Remaining polish: a DNS-01 issuer solver (issue before the
host is public, using the provider token), and folding the provider's record into the
**reachability** surface ("the provider holds the record").

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
