# ADR-0034: Agent-native onboarding — install probes the cluster, capabilities are read live, provisioning is demand-driven and cost-aware

## Status

Accepted. The first build item of the post-relicense roadmap. It folds cluster-capability
**detection into `burrow install`**, exposes those capabilities **live** to the agent over MCP, and
provisions missing substrate **only when a task needs it**, behind explicit, cost-aware consent.

## Context

Competitive research (issue-mining Coolify / Dokploy / Kubero; see
`research/competitive-icp-positioning.md`) found that the self-host-PaaS adoption ceiling is the
**install funnel**, not cluster ownership. Kubero — the closest analog, a PaaS that runs *on*
Kubernetes — has ~10× fewer stars than the VPS-based incumbents, and nearly every high-engagement
issue is an install that died on a Kubernetes prerequisite *for a user who already had a cluster*:
ingress-controller-vs-ingress confusion, DNS→public-IP / LoadBalancer wiring, cert-manager serving a
*fake* certificate, a default StorageClass that doesn't bind, and a "cluster-type picker" whose every
branch fails differently. Burrow already has the operational pieces (`reachability`, DNS automation,
`publish` + cert-manager TLS) but as separate commands that assume prerequisites.

Three constraints shape the design:

1. **It belongs in `install`.** `burrow install` already deploys burrowd, its namespace/RBAC, and
   the in-cluster Postgres using the developer's kubeconfig — so it already has full cluster
   visibility at that moment. Detection is part of installing, not a separate diagnostic verb.
2. **Don't assume ingress.** A cluster might only run cron jobs and need no domain at all. Detection
   must be **neutral capability reporting**; provisioning must be **demand-driven** — triggered by
   what the user actually asks for, never forced at install.
3. **Capabilities change out of band.** A user can `kubectl apply` an ingress controller after
   install. A profile cached once at install goes stale exactly when it matters, so the agent must
   read capabilities **live**.

## Decision

**`burrow install` probes and reports the cluster's capabilities; burrowd reads them live for the
agent; missing substrate is provisioned only on demand, kubeconfig-side, with the cost named.**

### 1. Install probes; burrowd reads capabilities live

- `burrow install` runs a capability probe as part of setup (kubeconfig) and **prints a summary** —
  e.g. "detected: nginx IngressClass, default StorageClass `do-block-storage`, provider
  DigitalOcean, no cert-manager." Installing tells you what your cluster can do.
- For the *agent*, capabilities are read **live**, not from a cached snapshot, so out-of-band changes
  are always reflected. This requires a **small read-only cluster grant** for burrowd: a `ClusterRole`
  with `get`/`list` on the cluster-scoped, non-sensitive types needed to answer "what can this
  cluster do" — **`nodes`, `storageclasses`, `ingressclasses`** — plus API-group discovery (which
  needs no RBAC, and is how cert-manager and other operators are detected, by the presence of their
  CRDs). **No secrets, no writes, no other resources.** burrowd's *write* surface stays exactly as
  locked-down as today; this is a measured, read-only exception to the otherwise-namespaced model.

### 2. A neutral capability surface over MCP (read-only)

The detected capabilities are exposed read-only — a `burrow_cluster` MCP tool and a `burrow cluster`
CLI view — reporting, per capability, *present / absent / inferred*:

- **Ingress:** an ingress controller + which `IngressClass`.
- **Public reachability:** `LoadBalancer` support (inferred from the detected cloud provider) vs.
  `NodePort` only (bare-metal / single-node).
- **TLS:** cert-manager present (via its CRDs), and whether it issues real certificates.
- **Storage:** a default `StorageClass`, and that a PVC binds.
- **DNS / provider:** a configured DNS provider ([ADR-0023](0023-provider-credentials.md)); the
  cloud provider (from node labels).

This **replaces the cluster-type picker** — Burrow observes the cluster instead of asking the user to
classify it — and, being read-only, it is the **low-trust agent entry point**: the agent can survey
a cluster and explain its state before anything is changed.

### 3. Demand-driven, cost-aware, kubeconfig-side provisioning

Installing cluster-wide substrate (nginx-ingress, cert-manager) is a **privileged write**, so it
stays on the **kubeconfig side** of the setup-vs-operation boundary — never burrowd. When a task
needs a capability the cluster lacks, the **agent recommends and explains**; the **user runs the
privileged install** with a `burrow` setup command that carries a **cost-aware confirmation**:

- A step that creates a **LoadBalancer** states that it provisions a *billable* cloud load balancer
  on the detected provider and to check current pricing — Burrow flags the **class** of cost and the
  provider, never a hardcoded price (they drift).
- Where a **free alternative** exists, the prompt offers it — **NodePort** instead of a cloud LB on a
  cost-sensitive or bare-metal cluster. PVCs are flagged the same way.
- **Nothing cluster-wide is installed without an explicit yes.**

This is the propose-then-confirm posture ([ADR-0006](0006-guardrails-in-the-control-plane.md))
applied to setup, with **cost transparency as a first-class part of the prompt** — and the privileged
action is run by the human (kubeconfig), matching how self-hosters say they will accept an agent:
it proposes and explains, you approve and execute.

### 4. Prove the first URL

When a user *does* want a public app, onboarding finishes by walking the existing `reachability`
chain (controller → Service/Ingress → TLS → DNS) end to end, so "live at `https://…`" is **verified**
and any broken link is named for the agent to drive to green.

## Consequences

- **Day-0 becomes the killer experience** — install your cluster, the agent immediately knows what it
  can do, and you reach a first URL (when you want one) without hand-configuring Kubernetes. It
  attacks the exact funnel where the only other k8s-native PaaS bleeds users, and it is agent-native
  whitespace.
- **burrowd gains one narrow read-only `ClusterRole`** (`get`/`list` on `nodes`, `storageclasses`,
  `ingressclasses`). This is the only relaxation; no cluster write, no secrets, no broad access.
- **Cost transparency becomes a product principle** — any action that creates a billable cloud
  resource says so first. Seeds the later "cost controls" theme.
- **No opinionated default** — a cron-only cluster is never nagged about ingress; provisioning is pulled
  by need, not pushed by install.
- **Slicing (separate PRs):** (1) **detection** — install-time probe + the live `burrow_cluster`
  read-only MCP tool/CLI + the read-only `ClusterRole` (the read-only foundation, lowest risk,
  immediately useful); (2) **demand-driven cost-aware provisioning** — kubeconfig-side `install`
  subcommands for nginx-ingress + cert-manager with the LB-vs-NodePort choice and cost prompts;
  (3) the **onboard flow** + first-URL verification glue. Build in that order.

## Rejected alternatives

- **A separate `burrow doctor` diagnostic command.** Rejected: detection belongs *in* `install`
  (which already has cluster visibility); a second verb is worse UX.
- **A cached capability snapshot taken only at install.** Rejected: it goes stale the moment the user
  changes the cluster out of band — the exact failure they'd hit. Live reads (with the narrow grant)
  are always current.
- **A cluster-type picker (kind / k3s / GKE / DO / …).** Rejected: it's Kubero's #1 failure mode —
  every branch fails differently and the user must self-classify a cluster they may not understand.
- **Assume every cluster wants ingress / auto-provision it at install.** Rejected: not every cluster
  needs a domain; forcing it creates billable resources nobody asked for.
- **Provision cluster-wide substrate through burrowd.** Rejected: cluster-wide *writes* need broad
  access burrowd deliberately lacks; provisioning is kubeconfig-side, run by the human
  ([ADR-0021](0021-guardrails-require-control-plane-only-agent-access.md)).
- **Auto-provision without confirmation.** Rejected: it would create billable cloud resources and
  mutate the cluster without consent — unacceptable for a control- and cost-conscious ICP.
