# ADR-0030: Burrow-owned credentials flow through the control-plane API, never MCP

## Status

Accepted. Extends [ADR-0029](0029-secrets-through-the-control-plane.md) (app secret values
traverse the control-plane API, never MCP) to Burrow's **own** vendor and connected-backend
credentials. **Supersedes the kubeconfig-direct credential-write in
[ADR-0023](0023-provider-credentials.md)** (the registry-of-tokens model itself stands).
[ADR-0017](0017-private-registry-authentication.md) (the registry pull secret) follows the same
principle and is a deferred follow-on. Reaffirms [ADR-0004](0004-code-never-over-mcp.md) and
[ADR-0002](0002-four-layer-architecture.md) / [ADR-0005](0005-mcp-server-holds-no-cluster-credentials.md).

## Context

[ADR-0023](0023-provider-credentials.md) keeps Burrow's vendor credentials — DNS provider tokens
and **connected-backend** auth (a Loki / Prometheus bearer token from `addon connect --auth`) — in
one scoped `burrow-credentials` Secret, with the registry recording only the *key*. Those values
are written **kubeconfig-direct** by the CLI (`provider add`, `addon connect --auth`): the value
never touches burrowd.

That is the same model [ADR-0028](0028-app-config-and-secrets.md) used for app secrets, and it has
the same limitation: it does not generalize to the **web UI** (no kubeconfig — just an
authenticated session to burrowd) or the **managed product**. A user entering a DNS token or a
connected-backend bearer token in the UI sends it to burrowd, which must write `burrow-credentials`.
[ADR-0029](0029-secrets-through-the-control-plane.md) already decided this for app secrets and noted
that provider and connected-backend credentials would follow. This ADR does that.

It also answers "how do we manage secrets for add-ons?": Burrow **operates** add-ons with safe
defaults, so they get no `app secret`-style store. The only user-provided add-on secret is the
**connect-time auth token**, and it becomes the second customer of this credential path
(alongside provider tokens). Add-on *generated* credentials (a future Postgres password) are
burrowd-generated and written the same way; the user never types them.

## Decision

Burrow-owned credential **values** (provider tokens, connected-backend bearer tokens, and
burrowd-generated add-on credentials) may traverse **burrowd's authenticated control-plane API**
(the CLI today; the web UI and managed product later), which writes them into the
`burrow-credentials` Secret. They are **still never carried over MCP**.

- **`provider add` and `addon connect --auth`** send the value to burrowd, which writes the Secret
  and records the registry entry (key only). The CLI drops its kubeconfig-direct write.
- **The agent never sets a credential value** — no MCP tool carries one; the agent references and
  lists *keys*. [ADR-0004](0004-code-never-over-mcp.md) is intact.
- **RBAC:** burrowd's `burrow-credentials` grant broadens from `get` to **`get, update`**, still
  **`resourceNames`-scoped to the single `burrow-credentials` Secret** in the control-plane
  namespace. No `ClusterRole`; nothing beyond that one named Secret. (This is *tighter* than the
  app-namespace secrets grant, which RBAC cannot name-scope.)
- **Guards (load-bearing, as [ADR-0029](0029-secrets-through-the-control-plane.md)):** the value is
  never logged (the access log records method + path + status; the path carries no value), never in
  an error message, never in the API response, never in any non-credential record, and never over
  MCP.

### Scope

This covers the `burrow-credentials` Secret (provider + connected-backend tokens) and the future
add-on generated credentials written into it or into the add-on namespace. The **registry pull
secret** ([ADR-0017](0017-private-registry-authentication.md)) — a `dockerconfigjson` in the app
namespace — follows the same principle but is a separate secret type in a different namespace and is
deferred to a follow-on.

## Consequences

- The web UI and the managed product can manage vendor and connected-backend credentials — their
  purpose — through one control-plane-owned path. The CLI is simpler (no kubeconfig/clientset write,
  no Secret-rollback dance on a rejected token).
- burrowd now sees credential values **in transit**. Acceptable: it is the trust boundary, the grant
  is `resourceNames`-scoped to one Secret, and a compromise of burrowd already implies read of that
  Secret — so the value's exposure is not meaningfully widened.
- The same no-log / no-response / no-MCP guards as [ADR-0029](0029-secrets-through-the-control-plane.md)
  are the real risk surface and are tested, not assumed.
- "Add-on secrets" need no new store: the connect-time token rides this path, installed-add-on config
  stays catalog-default (a future `addon config` if users ask), and generated creds are
  burrowd-managed.

## Rejected alternatives

- **Keep kubeconfig-direct (ADR-0023 status quo).** Cannot support the web UI or managed product, and
  leaves two credential-writing paths.
- **A separate `addon secret` / `addon config` store mirroring `app secret`.** Add-ons are
  Burrow-operated, not user code; their credentials are a single connect-time token (this path) or
  Burrow-generated (Burrow-managed). A parallel store would invert the safe-defaults model.
- **An external vault now.** Over-built for the open core; burrowd is already the trust boundary. A
  vault/KMS integration can layer on later without changing this decision.
