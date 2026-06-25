# ADR-0018: Reaching a deployed app at a URL — ingress, TLS, and DNS, with a reachability surface

## Status

Accepted. This is the v0.2 design, built in stages (see Consequences). Sub-policies it
defers — the exact guardrail thresholds, the precise resource annotations — are refined as
each stage ships, not pinned here.

## Context

After v0.1 a deployed app runs but is only reachable by `kubectl port-forward`. Making it
reachable at a real hostname over HTTPS, from outside the cluster, is the missing half of
"deploy and operate." Reachability is a **chain**, and any link can be missing or
misconfigured:

1. an **ingress controller** with an external address (a cloud LoadBalancer IP/hostname);
2. a **Service + Ingress** routing `host → service → the app's Pods`;
3. a **TLS certificate** for the host;
4. a **DNS record** pointing the host at the controller's external address.

The naive approach — have a tool create these resources and hope — fails the way agents
fail: when the app isn't reachable, the agent can't tell *which* link broke, so it can't
reason about a fix. And the links split across the trust boundary: installing a cluster-wide
controller and handing over a DNS-provider credential are **privileged setup** a human does;
creating an Ingress or a DNS record for an already-configured domain is an **operation** the
agent can drive through the guarded control plane. v0.1's invariants still bind: the agent
holds no cluster or provider credentials (ADR-0005), every operation is guarded and returns a
structured result (ADR-0006), and setup commands act with the developer's kubeconfig while
operations go through burrowd (ADR-0017).

## Decision

Build the reachability chain as an **introspectable** capability: Burrow reports the state of
every link so the agent can reason about reachability and act on the gaps it owns, and
defers the gaps the human owns with an actionable message.

### 1. A reachability surface (the spine)

A read-only **`reachability`** report — in the CLI (`burrow reachability <app>`, folded into
`status`) and as an **MCP tool** — returns the structured state of the chain for an app and
host:

- **ingress controller**: present? what external address (LB IP/hostname) does it have?
- **routing**: Service and Ingress created? backend endpoints ready?
- **TLS**: certificate issued / pending / failed?
- **DNS**: does `<host>` resolve (and, when a provider is configured, does the provider hold
  the record) pointing at the controller's external address?

Each link carries a status and, when unsatisfied, a **next action tagged as agent-fixable or
human-setup** — e.g. *"no ingress controller: run `burrow ingress install` (human setup)"* vs.
*"Ingress missing: I can run `expose` (agent)."* This is what lets the agent answer "why
isn't my app reachable?" instead of guessing.

### 2. Ingress + cert-manager: a Burrow-managed setup command that detects an existing stack

A dedicated **setup command** (not part of `burrow install`) — proposed `burrow ingress
install` — provisions ingress-nginx, cert-manager, and a Let's Encrypt ClusterIssuer, acting
with the developer's kubeconfig. It **detects an existing ingress controller / cert-manager
and uses it** rather than installing a second one, so the choice is not bundle-vs-BYO but
"Burrow installs one if you don't have one." Keeping it out of `burrow install` keeps the
core install minimal and lets BYO users skip it. The agent never runs this — installing a
cluster-wide controller is privileged setup; the agent *detects its absence* via the
reachability surface and tells the human to run it.

### 3. Expose: a guarded operation through burrowd

**`burrow expose <app> --host <name> [--port <n>]`** (CLI + MCP tool) creates a ClusterIP
**Service** (port 80/443 → the app's container port) and an **Ingress** (`host → service`)
through burrowd's guarded API — exposing an app to the public internet is a blast-radius
operation, so it is gated/confirmed (ADR-0006). **`unexpose`** removes them. burrowd's Role
expands to manage Services and Ingresses in the app namespace and to read the ingress
controller's Service (for its external address); it gains **no** credential access. A route
record in the control-plane database tracks what is exposed where.

### 4. TLS via cert-manager

`expose` annotates the Ingress for cert-manager to issue a certificate. With a DNS provider
configured (below), Burrow prefers the **DNS-01** challenge (issues before the host is
publicly reachable); otherwise **HTTP-01** (needs DNS already pointing at the controller).
The reachability surface reports certificate readiness so the agent can wait on or diagnose
issuance.

### 5. DNS automation: a provider seam, credentials in the control plane, scoped guardrails

Automated DNS is in scope for v0.2, DigitalOcean first:

- A human setup command — proposed `burrow dns configure digitalocean --token …` — stores the
  provider credential as a Secret **in the control-plane namespace, injected into burrowd**
  the way the database URL is (via the pod spec, not via a `secrets` RBAC grant), so the
  credential lives **only in the control plane** (ADR-0005) and never crosses MCP.
- A **DNS-provider seam** (interface + DigitalOcean adapter + fake; others later) lets burrowd
  create/update/delete records.
- **`burrow domain add <host>` / `remove <host>`** (CLI + MCP tool) are guarded operations:
  burrowd points `<host>` at the ingress controller's external address. The agent *initiates*
  these writes exactly as it initiates `deploy` — but it never holds the provider credential
  and never calls the DNS API directly; **burrowd holds the token and is the only thing that
  talks to the provider**, so every write is scoped and gated. That is the whole point of
  putting the credential in the control plane: agent-initiated DNS writes are fine *because*
  they can only happen through burrowd's guardrails. DNS writes are the sharp edge — they are
  **scoped to domains the user has delegated to Burrow**, and read/write/**delete** are
  separately gated with destructive changes confirmed (ADR-0006).

### Surface summary

- **Setup (human, kubeconfig):** `burrow ingress install`, `burrow dns configure <provider>`.
- **Operations (agent/CLI through burrowd, guarded):** `expose`, `unexpose`, `domain add`,
  `domain remove`.
- **Read-only (agent + human):** `reachability` / enriched `status`.

## Consequences

- **Build order (stages, all within v0.2 scope):** (1) `expose`/`unexpose` + the reachability
  surface for routing and the controller's external address; (2) TLS via cert-manager;
  (3) the DNS-provider seam, `burrow dns configure`, and `domain` operations with guardrails.
  Each stage is a thin slice that ends green, per the project's slice discipline.
- **The setup-vs-operation boundary (ADR-0017) now carries real weight:** `ingress install`
  and `dns configure` join `install`/`upgrade`/`registry` as human setup; `expose` and
  `domain` join the guarded operations. The reachability surface is what routes each gap to
  the right side of that line.
- **burrowd's privileges grow only within the cluster:** Services and Ingresses in the app
  namespace and a read on the controller's Service. The DNS-provider credential is injected,
  not RBAC-read — the least-privilege Role stays `secrets`-free.
- **New seams to keep core logic pure (ADR-0010):** the DNS provider (interface + DO adapter +
  fake) and a way to query the ingress controller's external address. DNS-write fault paths
  (partial updates, provider errors, propagation delay) get targeted fault-injection tests.
- **Honest status (ADR-0009):** the reachability surface reports what is actually true of the
  chain — "DNS not yet propagated," "certificate pending" — rather than claiming success on
  resource creation alone.
- **Opinionated dependencies:** ingress-nginx and cert-manager. Detection keeps them optional
  for BYO users; Gateway API and other controllers remain a later option behind the same
  expose/reachability surface.

## Rejected alternatives

- **Fold the ingress controller into `burrow install`.** Rejected per the maintainer's lean:
  it bloats the core install and forces the dependency on BYO users. A separate, detecting
  setup command is cleaner.
- **Let the agent install the controller or hold the DNS credential.** Rejected: a
  cluster-wide controller install and a DNS-provider credential are privileged setup
  (ADR-0005); the agent *diagnoses* the gap and the human closes it, then the agent operates.
- **Create resources without a reachability surface.** Rejected: it is the failure mode that
  makes agents flail — no way to tell which link is broken. Introspection is the point.
- **A LoadBalancer Service per app instead of shared ingress.** Rejected: one cloud LB per app
  is costly and slow; an ingress controller shares one LB across many hosts.
- **Manual DNS only for v0.2.** Considered (thinner, defers DNS-write risk) but rejected per
  the scope decision to include automation; manual DNS remains the supported fallback when no
  provider is configured, and the reachability surface guides it.
- **Gateway API instead of Ingress for v0.2.** Deferred: Gateway API is the likely future, but
  Ingress + ingress-nginx is more universally available and simpler for the first slice; the
  expose/reachability surface is designed to allow swapping the backend later.
