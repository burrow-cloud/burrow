# ADR-0044: Provision a single-VPS k3s cluster

## Status

✅ Accepted

## TL;DR

Burrow can stand up a complete cluster on a single cheap VPS running k3s, so a self-hoster gets a
live target for roughly the price of the VPS (about $5/mo) instead of ~$24/mo on a managed cloud (a
managed control plane plus a billable cloud load balancer). k3s's built-in **servicelb** makes the
node's own IP a free LoadBalancer public IP (proven in e2e #193; detection landed in #201). The
install is **Option B, an installer run on the VPS**: the user SSHes in once and runs
`curl … | sh`, which installs k3s (with `--tls-san <public-ip>`), deploys burrowd, and prints a
`burrow join <token>`. The user runs that token on their laptop (after `brew install burrow`), and
it lands **both** the admin and the scoped agent credential locally, so after the one-time SSH
bootstrap **every operation, governance and agent alike, runs from the laptop**, exactly like a
managed cluster. Burrow never SSHes anywhere (Option A rejected), and takes no dependency on k3sup.
Single node means no HA. Builds on [ADR-0038](0038-scoped-agent-credential.md) (the join path) and
[ADR-0043](0043-public-reachability-is-a-loadbalancer.md) (servicelb detection); relates to
[ADR-0034](0034-agent-native-onboarding.md), [ADR-0005](0005-mcp-server-holds-no-cluster-credentials.md).
Supersedes nothing.

## Context

Burrow today requires a pre-existing Kubernetes cluster. The reference target (a managed
DigitalOcean cluster plus a cloud load balancer) runs about $24/mo. A large, vocal segment of
self-hosters wants a cheaper path: a single ~$5 VPS. That segment is disproportionately active on
forums, so serving it well is worth it for adoption.

k3s is the fit: a single binary that ships servicelb, traefik, and local-path storage. servicelb is
the unlock, on a single node a `type: LoadBalancer` Service is assigned the node's own IP for free
(proven in e2e #193; Burrow's capability detection recognizes it as of #201). So a single VPS is a
complete Burrow target with no cloud load balancer: the node IP is the public IP DNS points at, and
HTTP-01 works because the node serves :80. The missing piece is getting from "just a VPS" to "k3s +
Burrow installed" in as close to one step as is safe, while preserving Burrow's model where the
agent operates from the user's laptop.

## Decision

Burrow provisions a **single-node k3s cluster** on a VPS via an **installer run on the VPS**
(Option B), single-node by default and growable (add k3s agents later). The full flow:

1. **The user provisions a VPS** with SSH access and opens `:6443` (the API server, reached from
   the laptop), `:80`, and `:443` (public traffic) in the provider's firewall.
2. **On the VPS, one command:** `curl -sfL https://get.burrow.dev | sh`. The bootstrap installs k3s
   with `--tls-san <public-ip>` and `--node-external-ip <public-ip>` (so the API-server certificate
   is valid for the public IP the laptop connects to) and `--write-kubeconfig-mode 0644`, deploys
   burrowd into it, and prints a single **`burrow join <token>`** line. The token carries the API
   URL (`https://<public-ip>:6443`), the cluster CA, and an **admin** credential.
3. **The user copies the printed `burrow join <token>`** to their laptop. It is a token, not a file
   copy, and it is admin-grade, so it is handled like a kubeconfig (never pasted into agent chat).
4. **On the laptop:** `brew install burrow` (the CLI and `burrow-mcp` client), then
   `burrow join <token>`. This records the admin kubeconfig for governance and mints/records the
   scoped agent credential in `~/.burrow/agents` (exactly what `burrow install` does against a
   managed cluster). From here the agent operates via the scoped credential and the human governs
   via admin, all from the laptop.

**The SSH is a one-time bootstrap only.** After step 2 the user never SSHes back in: governance
(`guard set`, `upgrade`) and agent operations all run from the laptop, the same experience as a
managed cluster. The admin credential reaching the laptop is the same posture as a managed cluster
(where the admin kubeconfig already lives on the laptop); keeping admin off the laptop was
considered and rejected because it would force SSH for every governance op.

**Burrow never SSHes.** The privileged channel is the user's own SSH session, which they already
establish and host-key-verify with their own tooling. The kubeconfig public-IP rewrite that k3sup
performs is a trivial string replacement Burrow reimplements if needed; Burrow takes **no dependency
on k3sup**.

## Consequences

- Expands Burrow's scope from "operate your cluster" to "provision it on a VPS," via an installer
  that runs on the box (the user's own SSH session), not an SSH orchestrator Burrow drives. This
  keeps Burrow out of the SSH-as-root, host-key, and key-handling business
  ([ADR-0005](0005-mcp-server-holds-no-cluster-credentials.md)).
- After a single SSH bootstrap, all operations run from the laptop, matching the managed-cluster
  experience. The `burrow join <token>` reuses the ADR-0038 join path and lands both credentials.
- Single node means no high availability; a node failure takes the site down. Fine for the cheap
  tier, stated plainly.
- The `--tls-san <public-ip>` / open-`:6443` requirement is the single most common failure mode (the
  laptop's TLS to the API server fails without it); the bootstrap sets it and the flow guides the
  firewall opening.
- Depends on the servicelb detection from ADR-0043 (landed in #201), or Burrow would report
  LoadBalancer unsupported on the k3s node.
- The bootstrap token is admin-grade; the flow and docs treat it accordingly.

## Rejected alternatives

- **Option A, SSH from the laptop** (`burrow cluster create --ssh`). Rejected as the spine: it would
  graft an SSH client, remote root execution, host-key trust, and key handling onto a CLI whose
  design minimizes what Burrow holds and touches. It buys one fewer manual step at the cost of a
  permanent SSH-as-root surface. It may return later as an optional laptop-first front end that
  reimplements the trivial kubeconfig rewrite and calls the same on-VPS bootstrap and join, but it
  is not the boundary.
- **Depend on k3sup.** Its only useful part is a ~3-line kubeconfig rewrite; a binary dependency is
  not warranted, and reimplementing it keeps the dependency graph small.
- **Keep the admin credential off the laptop.** A hygiene bonus, but it forces SSH for every
  governance op, which contradicts the requirement that all ops run from the laptop after setup, and
  it is the same posture as a managed cluster anyway.
- **Require a managed cluster / a multi-node or Terraform provisioner.** The first excludes the
  cheap tier this ADR serves; the second is too heavy. Start single-node and grow by adding agents.
