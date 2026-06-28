# ADR-0029: Secrets traverse the control-plane API, never MCP

## Status

Accepted. **Supersedes the secret-transport decision in [ADR-0028](0028-app-config-and-secrets.md)**
— which required `app secret set` to write the value kubeconfig-direct and never cross the
control-plane API. The rest of ADR-0028 stands (the env/secret store, keys-only `secret list`,
secret values living only in a per-app Kubernetes Secret, never inlined into the Deployment or
written to Postgres). Reaffirms [ADR-0004](0004-code-never-over-mcp.md) (code and secrets never
over MCP) and [ADR-0002](0002-four-layer-architecture.md) / [ADR-0005](0005-mcp-server-holds-no-cluster-credentials.md)
(the control plane is the trust boundary; the MCP server holds no credentials).

## Context

[ADR-0028](0028-app-config-and-secrets.md) decided that a secret value is written **kubeconfig-direct**
— the CLI uses the developer's kubeconfig to write the per-app Kubernetes Secret, and the value
never passes through burrowd — to minimize burrowd's exposure to values in transit.

That does not generalize to where Burrow is going:

- **A web UI.** The planned HTMX/JSON dashboard has no kubeconfig; it has an authenticated session
  to **burrowd**. A user typing a secret into that UI sends it to burrowd's API, which must write it
  to Kubernetes.
- **The managed product.** The multi-tenant control plane receives secrets through its API for the
  same reason — there is no developer kubeconfig in that path.

Re-examining the trade: burrowd is the **designated trust boundary** (ADR-0002/0005). It already
holds the API token, the database password, and provider credentials, and it operates the app
namespace (full workload CRUD). Its RBAC is entirely **namespaced Roles** — there is no
`ClusterRole`, nothing cluster-wide — scoped to the burrow, app, and addon namespaces. Routing a
secret value *through* burrowd in transit stays within that existing scope; it does not widen
burrowd's reach. The kubeconfig-only rule was over-rotated on minimizing in-transit exposure, at
the cost of the UI and managed paths and a second, special-cased secret-writing path.

The agent boundary is unchanged and is the line that actually matters: the **MCP** server is the
thin, credential-free agent surface, and a secret must never cross it.

## Decision

A secret **value may traverse burrowd's authenticated control-plane API** (the CLI today; the web
UI and the managed product later), which writes it into the per-app Kubernetes Secret in the app
namespace. It is **still never carried over MCP**.

- **`app secret set`** becomes a normal control-plane API call — `POST /v1/apps/{app}/secrets`
  with `{key, value}` — and burrowd writes the Secret. The CLI drops its kubeconfig-direct write.
  One secret-writing path, owned by the control plane.
- **The agent still cannot set a secret value.** There is no `burrow_secret_set` MCP tool (a test
  fails if one ever appears); the agent references secret *keys* and asks the human, who sets the
  value through the CLI or UI. `secret list` (keys only) and `secret unset` (by key) remain
  MCP-callable — they carry no value. [ADR-0004](0004-code-never-over-mcp.md) is intact.
- **RBAC is unchanged from [ADR-0028](0028-app-config-and-secrets.md):** `secrets: get, list,
  create, update`, **namespace-scoped to the app namespace** (no `ClusterRole`). burrowd writes the
  value with the same grant it already holds to manage that namespace's workloads.

### Guards (load-bearing)

The value passes through burrowd, so two properties must hold or we leak it:

- **Never logged, never audited.** burrowd's request log skips the secret-set body, and the audit
  log records the **key name only** (the keys-only redaction from [ADR-0027](0027-audit-log.md)).
  No code path writes a secret value to a log, the audit table, or Postgres.
- **Only over the authenticated, TLS-protected API** — the kubeconfig API-server proxy today, HTTPS
  for the UI and managed — and never over MCP.

### Complementary mitigation: a dedicated default app namespace

The default app namespace moves from `default` to a dedicated **`burrow-apps`**, so burrowd's
namespace-scoped secrets grant does not land on the cluster's shared `default` namespace by
default. (An operator may still choose `--app-namespace default` explicitly.)

### Scope

This principle — **credentials through the control-plane API, never MCP** — will also apply to
provider and registry credentials (kubeconfig-direct today) when the UI and managed product need to
manage them. Out of scope here; this ADR covers app secrets.

## Consequences

- The web UI and the managed product can manage secrets — their whole purpose — through one
  unified, control-plane-owned path. The CLI is simpler (no kubeconfig/clientset special case, no
  app-namespace discovery for `secret set`).
- burrowd now sees secret values **in transit**. Acceptable: it is the trust boundary, its access
  is namespace-scoped, and a compromise of burrowd already implies app-namespace Secret access — so
  the value's exposure is bounded to the app namespace, never cluster-wide, and is not meaningfully
  widened by routing it through.
- We give up the strict "burrowd never sees a value" property from ADR-0028. It did not survive
  contact with the UI/managed requirements and bought little, since burrowd already needs
  app-namespace Secret access to do its job.
- The no-log / no-audit guards become the real risk surface and are called out explicitly so they
  are tested and reviewed, not assumed.

## Rejected alternatives

- **Keep kubeconfig-only (ADR-0028 status quo).** Cannot support the web UI or the managed product,
  and leaves two secret-writing paths to maintain.
- **A separate secrets service / external vault now.** Over-built for the open core; burrowd is
  already the trust boundary. A future vault/KMS integration can layer on without changing this
  decision.
- **Inline secret values into the Deployment, or store them in Postgres.** Rejected by
  [ADR-0028](0028-app-config-and-secrets.md) and still rejected — values live only in the
  Kubernetes Secret.
