# ADR-0030: Provider and backend credentials traverse the control-plane API, never MCP

## Status

Accepted. Extends [ADR-0029](0029-secrets-through-the-control-plane.md) (secrets traverse the
control-plane API, never MCP) to the **provider and backend credentials** ADR-0029 left in scope:
the vendor tokens in the `burrow-credentials` Secret. **Supersedes the credential-transport rule in
[ADR-0023](0023-provider-credentials.md)** — which required `provider add` to write the
token kubeconfig-direct so the token never reached burrowd. The rest of ADR-0023 stands (the single
`burrow-credentials` Secret, one key per provider; the non-secret registry in Postgres; the token
read at call time so a rotation needs no restart). Reaffirms [ADR-0004](0004-code-never-over-mcp.md)
(code and credentials never over MCP) and
[ADR-0002](0002-four-layer-architecture.md) / [ADR-0005](0005-mcp-server-holds-no-cluster-credentials.md)
(the control plane is the trust boundary; the MCP server holds no credentials).

## Context

[ADR-0023](0023-provider-credentials.md) decided that a provider token is written
**kubeconfig-direct** — the CLI uses the developer's kubeconfig to write `burrow-credentials`, and
the token never passes through burrowd, which only ever *reads* it — to minimize burrowd's exposure
to values in transit.

[ADR-0029](0029-secrets-through-the-control-plane.md) re-examined exactly that trade for app secrets
and routed the value through burrowd's authenticated control-plane API instead, because the
kubeconfig-only rule did not survive contact with where Burrow is going:

- **A web UI.** The planned dashboard has no kubeconfig; it has an authenticated session to
  **burrowd**. A user pasting a DigitalOcean or Cloudflare token into that UI sends it to burrowd's
  API, which must write it to `burrow-credentials`.
- **The managed product.** The multi-tenant control plane receives credentials through its API for
  the same reason — there is no developer kubeconfig in that path.

The same reasoning applies to provider and backend credentials, and the same boundary still holds:
burrowd is the **designated trust boundary** (ADR-0002/0005). It already reads `burrow-credentials`
at call time; routing the value *through* it in transit stays within the existing, name-scoped reach
and removes a second, special-cased credential-writing path. The **MCP** server remains the thin,
credential-free agent surface, and a credential must never cross it.

## Decision

A provider or backend bearer-token **value may traverse burrowd's authenticated control-plane API**
(the CLI today; the web UI and the managed product later), which validates it and writes it into the
single `burrow-credentials` Secret. It is **still never carried over MCP**.

- **`provider add`** sends the token VALUE in its `POST /v1/providers` body. burrowd **validates the
  token first** — it builds the vendor adapter with the token and makes the existing cheap
  authenticated call — and only on success writes it into `burrow-credentials` and records the
  registry entry. A rejected token returns an error and writes **nothing** (no write-then-rollback;
  nothing is written until validation passes). The CLI drops its kubeconfig-direct write.
- **`addon connect --auth`** carries the bearer-token VALUE in its connect-request body. burrowd
  writes it into `burrow-credentials` under the key and records the registry entry. The CLI drops
  its kubeconfig-direct write.
- **The agent still cannot configure a credential.** `burrow_providers` is read-only; there is no
  MCP tool that adds a provider or carries a token, and no MCP tool input accepts a `token` (a test
  fails if one ever appears). The agent connects only *unauthenticated* backends or references an
  *already-configured* credential by key. Authenticated connect and provider add are human/CLI
  operations. [ADR-0004](0004-code-never-over-mcp.md) is intact.
- **RBAC broadens minimally:** burrowd's name-scoped `burrowd-credentials` Role grants `get` **and
  now `update`** on exactly the `burrow-credentials` Secret — still no `ClusterRole`, still not
  `list`, not `watch`, not `create`, and no other Secret. The scope (that one object) is unchanged;
  only the verb set grows by `update` so burrowd can write the value it received.

### Guards (load-bearing)

The value passes through burrowd, so these properties must hold or we leak it. The token value
flows ONLY: CLI → POST body → API decode → engine → `Credentials.SetToken` → the `burrow-credentials`
Secret.

- **Never logged.** burrowd's access log records method, path, and status only — the path carries no
  value. No code path writes a token to a log.
- **Never in an error.** Validation and write errors are wrapped with the provider/backend NAME and
  the key only — never the value.
- **Never in a response.** The recorded `Provider` / `AddonInfo` carries the Secret key, never the
  token; the endpoints do not echo the value back.
- **Never in Postgres.** The registry stores the non-secret entry (type, capabilities, key); the
  value lives only in the `burrow-credentials` Secret.
- **Only over the authenticated, TLS-protected API** — the kubeconfig API-server proxy today, HTTPS
  for the UI and managed — and **never over MCP.**

## Consequences

- The web UI and the managed product can configure providers and authenticated backends — their
  whole purpose — through one unified, control-plane-owned path. The CLI is simpler: no
  kubeconfig/clientset special case and no write-then-validate-then-rollback dance, since validation
  precedes the single write.
- burrowd now sees a provider token **in transit**. Acceptable: it is the trust boundary, its access
  to `burrow-credentials` is name-scoped, and it already reads that Secret to do its job — routing
  the value through does not meaningfully widen its reach.
- We give up the "burrowd never writes `burrow-credentials`" property from ADR-0023. It did not
  survive the UI/managed requirements and bought little, since burrowd already needed to read the
  Secret.
- The no-log / no-error / no-response / no-DB guards become the real risk surface and are called out
  explicitly so they are tested and reviewed, not assumed.

## Rejected alternatives

- **Keep kubeconfig-only (ADR-0023 status quo).** Cannot support the web UI or the managed product,
  and leaves two credential-writing paths to maintain.
- **Write-then-validate-then-rollback (the prior CLI dance).** Replaced by validate-then-write in the
  engine: nothing is written until the token authenticates, so there is no rejected token to roll
  back.
- **A separate secrets service / external vault now.** Over-built for the open core; burrowd is
  already the trust boundary. A future vault/KMS integration can layer on without changing this
  decision.
