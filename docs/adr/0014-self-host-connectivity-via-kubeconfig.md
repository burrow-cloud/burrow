# ADR-0014: Self-host connectivity via the developer's kubeconfig and the API-server proxy

## Status

Accepted. Refines [ADR-0005](0005-mcp-server-holds-no-cluster-credentials.md) for the
self-host model (it does not supersede it; ADR-0005 stands in full for the managed model).

## Context

The control plane (`burrowd`) runs as a service *inside* the cluster and exposes its HTTP
API there. The CLI and the MCP server run on the developer's machine and must reach that
in-cluster API. The Kubernetes API server is reachable (it is in the developer's
kubeconfig — e.g. `doctl kubernetes cluster kubeconfig save …` writes it), but `burrowd`'s
ClusterIP service is not directly reachable from the laptop.

The first instinct was `kubectl port-forward`, but that means a second long-lived shell —
janky. Two cleaner facts:

1. **The Kubernetes API server can proxy to in-cluster services**:
   `GET <apiserver>/api/v1/namespaces/<ns>/services/<svc>:<port>/proxy/<path>`. The
   developer's kubeconfig already authenticates to the API server, so the client reaches
   `burrowd` *through* the API server with no port-forward and no ingress. (Verified
   against a real DigitalOcean managed cluster.)
2. So "connectivity" for a self-host client is just **the kubeconfig the developer already
   has** — there is no separate Burrow login.

This brushes against ADR-0005 ("the MCP server holds no cluster credentials"): to use the
API-server proxy, the client uses the developer's kubeconfig — a cluster credential.

## Decision

In the **self-host** model, the local client tools — the CLI, and the MCP server when run
as the developer's own local subprocess — **may use the developer's ambient kubeconfig
solely for connectivity**: to reach `burrowd` through the Kubernetes API server's service
proxy, and to read the API token from the install Secret. The cluster-**operating**
credential (the `burrowd` ServiceAccount that creates Deployments, reads logs, etc.) and
the guardrails remain only in the control plane.

This preserves the *intent* of ADR-0005:

- The MCP layer still holds **no cluster-operating credential** and exposes **no raw
  cluster access** as a tool — it can only call `burrowd`'s guarded operations.
- Every operation still flows through `burrowd`'s API and guardrails
  ([ADR-0006](0006-guardrails-in-the-control-plane.md)); the kubeconfig only opens the
  tunnel, it does not bypass the boundary.
- The developer already has this kubeconfig; the client acts with the developer's own
  access, not a credential Burrow minted for the MCP layer.

**The managed / multi-tenant model keeps ADR-0005 at full strength.** There the MCP server
is network-exposed and shared, the user authenticates to the managed service (not a raw
cluster) and never receives a kubeconfig, and the managed control plane holds the cluster
credentials and enforces tenant isolation. The managed MCP server uses managed auth tokens
only — never a kubeconfig.

## Consequences

- A small Apache-licensed `connect` package reads the kubeconfig (or in-cluster config),
  resolves the API-server service-proxy base URL for `burrowd`, reads the API token from
  the install Secret, and returns a ready `client.Client`. The CLI uses it by default, so
  a developer with `kubectl` access configures nothing else. Explicit `--control-plane` /
  `--token` remain available (for an ingress, or CI).
- `connect` imports client-go (Apache) and stays out of the FSL `controlplane` packages —
  it talks to the API server, not Burrow's internals, so the license boundary holds.
- The API token is still required (`burrowd` authenticates callers); it is just read
  transparently from the cluster. Two gates protect `burrowd`: Kubernetes RBAC on the
  `services/proxy` subresource, and `burrowd`'s own token check.
- **Burrow's token travels in `X-Burrow-Token`, not `Authorization`.** The developer's
  kubeconfig commonly authenticates to the API server with a bearer token (an `exec`
  credential plugin — confirmed on the reference DigitalOcean cluster), so the API-server
  transport owns the `Authorization` header on the proxy path. The client therefore sends
  Burrow's token in `X-Burrow-Token`, which the API-server proxy forwards to `burrowd`
  unchanged; `burrowd` accepts the token from either `X-Burrow-Token` or
  `Authorization: Bearer` (the latter for the direct / ingress path). The client sends both
  headers, so one path or the other always carries it.
- Reaching `burrowd` requires `get`/`create` on `services/proxy` in its namespace —
  cluster-admin kubeconfigs have it; a scoped self-host user may need it granted.

## Rejected alternatives

- **`kubectl port-forward` in a separate shell.** Rejected: a second long-lived process to
  manage — the friction this ADR removes. (An *in-process, ephemeral* port-forward was also
  considered; the API-server proxy is simpler — no local listener, no goroutine.)
- **Put the deploy logic + kubeconfig directly in the CLI/MCP (no in-cluster control
  plane).** Rejected: it collapses the trust boundary. The agent is untrusted; the
  guardrails and cluster-operating credential must live server-side so the agent cannot
  bypass them (ADR-0002, ADR-0006). The kubeconfig is for *reaching* the control plane,
  not for *being* it.
- **Require an ingress/LoadBalancer for the control plane in v0.1.** Rejected as
  unnecessary setup and public exposure; the API-server proxy needs neither. Ingress
  remains a later option (and ties into the ingress roadmap item).
