# ADR-0011: Kubernetes integration — client-go behind the seam, workload-typed resources

## Status

Accepted.

## Context

The control plane is the only layer that talks to Kubernetes (ADR-0002,
[ADR-0005](0005-mcp-server-holds-no-cluster-credentials.md)), through the
`controlplane.Kubernetes` seam (ADR-0010). Two concrete questions about that seam and its
real adapter need settling before the adapter is built:

1. **How does the adapter talk to the cluster?** Options: the official Go SDK
   (`k8s.io/client-go`); the higher-level controller framework
   (`sigs.k8s.io/controller-runtime`, which wraps client-go); raw REST against the API
   server; or shelling out to the `kubectl` CLI.
2. **What Kubernetes resources does a deploy create?** A stateless app is a `Deployment`;
   a stateful one (stable identity, persistent volumes, ordered rollout) is a
   `StatefulSet`; batch work is a `Job`/`CronJob`. v0.1 is compute-first and deploys
   stateless app code, but databases and other stateful workloads are on the roadmap.

The seam as first written (Phase 2) named its type and methods after `Deployment`
(`DeploymentSpec`, `ApplyDeployment`, …). Because StatefulSets are a known, roadmapped
need rather than a speculative one, baking `Deployment` into the seam vocabulary would
force a rename later.

## Decision

### Talk to the cluster with client-go (and controller-runtime for the operator)

The real `controlplane.Kubernetes` adapter uses **`k8s.io/client-go`**, the official
programmatic SDK. The **operator** (`operator/`) uses
**`sigs.k8s.io/controller-runtime`** for its reconcile loops. Burrow never shells out to
the `kubectl` CLI.

These dependencies live **only behind the seam**: only `controlplane/internal` (the
adapter) and `operator/` import client-go / controller-runtime. The deploy engine and all
core logic depend on the `controlplane.Kubernetes` interface, never on client-go, so core
logic stays deterministic and fake-tested (ADR-0010), and client-go's large dependency
tree stays out of the Apache-licensed client surface (`mcp/`, `cmd/burrow`). client-go is
a deliberate, justified exception to the "small dependency graph" rule (CLAUDE.md), like
the Postgres driver and an MCP library — there is no sane alternative to the API server's
own client.

### The seam speaks "workloads," not "deployments"; v0.1 uses Deployment only

The `controlplane.Kubernetes` seam is generalized to a workload vocabulary now, so adding
new kinds later is additive rather than a rename:

- `WorkloadKind` names the Kubernetes resource a workload maps to (`Deployment`,
  `StatefulSet`, …); the empty value means `Deployment`.
- `WorkloadSpec` (with a `Kind` field) replaces `DeploymentSpec`; `WorkloadStatus`
  replaces `DeploymentStatus`; the methods are `ApplyWorkload`, `WorkloadStatus`,
  `ScaleWorkload`, `DeleteWorkload`, plus `Logs`.

**v0.1 uses `Kind: Deployment` exclusively.** The engine sets it; `DeployRequest` does not
yet expose a workload type, because v0.1 is stateless compute only (databases, and the
StatefulSet path, are out of scope — see [PLAN.md](../PLAN.md)). When stateful workloads
arrive (with database provisioning), the workload type becomes a first-class concept that
the engine maps to the right `WorkloadKind` — a change that is additive to this seam, and
that resolves the kind from the workload type rather than letting the agent choose raw
Kubernetes resources.

## Consequences

- The Phase 5 adapter is a client-go implementation of `controlplane.Kubernetes`; the
  Phase 2 seam and the Phase 3 engine already speak the workload vocabulary, so no rename
  is needed when StatefulSets land.
- The first `go.sum` and a meaningful jump in module-graph size arrive with the client-go
  adapter; this is expected and contained behind the seam.
- Integration tests (ADR-0010) exercise the real adapter against an ephemeral kind/k3d
  cluster; the operator additionally uses controller-runtime envtest.
- Adding a `WorkloadKind` is additive: a new constant, the engine's mapping from workload
  type to kind, and the adapter's handling of that kind — no change to existing records or
  the seam's shape.

## Rejected alternatives

- **Shell out to `kubectl`.** Rejected: fragile string output, no typed errors, an extra
  runtime dependency on a CLI binary, and worse testability than a programmatic client.
- **Raw REST against the API server.** Rejected: reimplements what client-go already does
  (auth, discovery, encoding, retries) with more bugs and no benefit.
- **Import client-go into core logic.** Rejected: it would couple the deterministic,
  fake-tested engine to a real cluster client and pull client-go into the Apache client
  surface. The seam exists precisely to keep it out.
- **Keep the seam `Deployment`-shaped and generalize later (Option A).** Rejected for this
  project: StatefulSets are a roadmapped certainty, so the rename is a known future cost;
  paying the small generalization now (a `Kind` field and workload-named methods) avoids
  it. (Had stateful support been speculative, YAGNI would favor staying Deployment-shaped.)
