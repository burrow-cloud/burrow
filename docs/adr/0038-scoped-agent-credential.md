# ADR-0038: Burrow mints a scoped agent credential at install; the human keeps the admin kubeconfig

## Status

Accepted. Extends [ADR-0005](0005-mcp-server-holds-no-cluster-credentials.md),
[ADR-0014](0014-self-host-connectivity-via-kubeconfig.md), and
[ADR-0021](0021-guardrails-require-control-plane-only-agent-access.md); interacts with
[ADR-0027](0027-audit-log.md) (a new `principal` field) and
[ADR-0036](0036-environment-selection.md) (the local config stores the credential and the
control-plane namespace). Supersedes nothing.

## Context

Today the client — the CLI and the `burrow-mcp` MCP server — reaches the in-cluster control
plane (burrowd) using the developer's **ambient kubeconfig** through the Kubernetes API-server
proxy ([ADR-0014](0014-self-host-connectivity-via-kubeconfig.md)). The MCP server holds no
credentials of its own ([ADR-0005](0005-mcp-server-holds-no-cluster-credentials.md)), and
secret *values* never cross MCP ([ADR-0029](0029-secrets-through-the-control-plane.md),
[ADR-0030](0030-credentials-through-the-control-plane.md)) — both hold. But the ambient
kubeconfig is real cluster access, as broad as whatever it grants, and in practice that is
often cluster-admin. The agent runs in the same environment and has a shell, so it effectively
holds that access.

The guardrails live in burrowd ([ADR-0006](0006-guardrails-in-the-control-plane.md)) and only
*bind* if every path to the cluster goes through burrowd
([ADR-0021](0021-guardrails-require-control-plane-only-agent-access.md)). The kubeconfig is
therefore the real trust boundary. [docs/HARDENING.md](../HARDENING.md) tells operators to
isolate the agent so its only reachable credential is Burrow's, but Burrow does not yet provide
the tooling to mint such a scoped credential. Shell-denies — denying the agent `burrow` and
`kubectl` (the #154 hardening) — are defense in depth, not a boundary: they depend on the
agent honoring its permission configuration, not on what the credential can actually do.

## Decision

**Burrow mints a scoped, burrowd-only credential for the agent at `burrow install`, and the
human keeps their own admin kubeconfig for privileged and governance work.** The kubeconfig
stops being the agent's lever; the guardrails become binding by construction.

### 1. Mint a scoped agent credential at `burrow install`

`install` is already a privileged, kubeconfig-side setup operation. It gains a step that:

- creates a `burrow-agent` **ServiceAccount** plus a narrow **Role/RoleBinding** in the
  control-plane namespace granting **only** what the client needs to reach burrowd — proxy
  access to the burrowd Service and `get` on the `burrowd-api-token` Secret — and nothing else:
  no pods, no other secrets, no other namespaces, no cluster-scoped read;
- mints a ServiceAccount bearer token, reads the cluster CA, and writes a **self-contained
  scoped kubeconfig** into the Burrow local config directory (`~/.burrow/`), **not** into the
  user's `~/.kube/config`.

`burrow-mcp` and the operate-path CLI commands use that scoped kubeconfig. The consequence is
that even a shelled-out `kubectl` pointed at it is denied everything except reaching burrowd —
so the guardrails become binding by construction, not merely by shell-denies.

### 2. Two credentials, two roles

- The **human** keeps their own admin kubeconfig for privileged setup and governance
  operations: `install`, `upgrade`, `guard set`, `env add`, and registry/provider credentials.
- The **agent** gets only the scoped, burrowd-only credential.

Least privilege for the agent; full control retained by the human.

### 3. Credential mechanism: a ServiceAccount bearer token

The credential is a ServiceAccount bearer token (a JWT). For the first version a **long-lived
token** — a `kubernetes.io/service-account-token` Secret — is acceptable, because the RBAC
narrowness is the real control and the token is revocable by deleting the ServiceAccount or its
Secret. A bound, short-lived token (the TokenRequest API, which needs refresh) or a client
certificate (via the CSR API) are noted as future tightenings. The scoped kubeconfig carries
the API server URL, the cluster CA, and the token.

### 4. Idempotent, multi-user `install` (join)

`install <context>` must be safe to re-run and to run as a **second user** on an
already-installed cluster. On a populated cluster it must **not** re-mint burrowd's secrets —
today it errors "run upgrade" precisely to avoid breaking the control plane. Instead it skips
the cluster apply and performs the **local join**: reading the existing scoped agent credential
and the recorded namespace (with the joining user's own kubeconfig access) and writing that
user's `~/.burrow/config`. This join path is shared with `burrow env scan`. Edge case: a
joining user needs read access to the agent-SA token Secret to self-serve; otherwise the
operator hands over the scoped kubeconfig out of band.

### 5. Shared agent ServiceAccount now; per-user ServiceAccounts are a deliberate, additive future step

For now all users' agents share the one `burrow-agent` ServiceAccount. Per-user ServiceAccounts
are inseparable from single-sign-on, because audit attribution requires tying an action to a
real human identity — so they are deferred to the SSO/auth work. To make that later step
**additive rather than a rewrite**, the design threads a **principal** (actor) concept through
the system now, as a constant:

- credential provisioning sits behind a **seam** that returns the shared ServiceAccount today
  and can mint a per-user ServiceAccount keyed on an SSO identity later;
- burrowd resolves a **caller identity** through a seam that is a constant ("shared agent")
  today and a `TokenReview`-to-SSO mapping later;
- the audit record ([ADR-0027](0027-audit-log.md)) gains a **`principal`** field now,
  populated with the shared-agent constant, so per-user SSO attribution later fills in a value
  instead of migrating the meaning of past rows.

### 6. Namespace discovery

The control-plane namespace is recorded in the local config
([ADR-0036](0036-environment-selection.md), `ControlPlaneNamespace`) at install, so the CLI and
MCP resolve it from there rather than searching the cluster. Label-based discovery — the
install-time `app.kubernetes.io/managed-by: burrow` label on the namespace — remains an
admin-path fallback that requires cluster-wide read, which the scoped credential deliberately
lacks and does not need.

## Consequences

- `install` gains ServiceAccount + RBAC creation (privileged, kubeconfig-side, consistent with
  install already being privileged) and writes the scoped kubeconfig plus the namespace into
  the local config.
- The agent's reachable capability becomes exactly "the guardrailed control plane," so the
  "safe enough for prod" posture holds under scrutiny rather than resting on shell-denies.
- [docs/HARDENING.md](../HARDENING.md) and the `burrow-mcp` help text should be updated to state
  that the kubeconfig is the trust boundary and that Burrow now provisions a scoped agent
  credential. (Those doc and help changes land with the implementation, not here.)
- The principal seam and the audit `principal` field are seeded now so per-user SSO attribution
  is additive later.
- Relationship: extends [ADR-0005](0005-mcp-server-holds-no-cluster-credentials.md),
  [ADR-0014](0014-self-host-connectivity-via-kubeconfig.md), and
  [ADR-0021](0021-guardrails-require-control-plane-only-agent-access.md); interacts with
  [ADR-0027](0027-audit-log.md) and [ADR-0036](0036-environment-selection.md). It supersedes no
  ADR.

## Rejected alternatives

- **Keep using the ambient admin kubeconfig (status quo).** Simplest, but the agent inherits
  full cluster access, so the guardrails are only as strong as shell-denies — which
  [docs/HARDENING.md](../HARDENING.md) itself calls defense-in-depth, not a boundary.
- **Per-user ServiceAccounts now.** More isolation and per-user revocation, but a per-user
  ServiceAccount has no real value without an identity model to attribute it to; deferring it to
  the SSO/auth work avoids building an attribution scheme twice. The principal seam keeps the
  door open.
- **Expose burrowd via an ingress with a shared API token instead of the kube proxy.** Removes
  the kubeconfig dependency, but broadens the control plane's network exposure and loses the
  "if `kubectl` works, Burrow works" simplicity of the API-server proxy
  ([ADR-0014](0014-self-host-connectivity-via-kubeconfig.md)).
