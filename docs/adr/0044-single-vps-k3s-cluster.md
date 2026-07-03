# ADR-0044: Provision a single-VPS k3s cluster

## Status

Proposed (open — the install mechanism is under investigation; do not treat as accepted)

## TL;DR

Let Burrow stand up a complete cluster on a single cheap VPS running k3s, so a self-hoster gets
a live Burrow target for roughly the price of the VPS (about $5/mo on Hetzner) instead of ~$24/mo
on a managed cloud (a managed control plane plus a billable cloud load balancer). k3s's built-in
**servicelb** makes the node's own IP a free LoadBalancer public IP (validated by e2e #193), so no
cloud load balancer is needed: deploy, expose, point DNS at the node IP, and get TLS via HTTP-01
on the node's :80. **Open question, the reason this stays Proposed:** whether Burrow installs by
**SSHing from the laptop** (`burrow cluster create --ssh root@<ip>`) or by running an **installer
directly on the VPS** (`curl … | sh`, or a `burrow` binary run on the box). Depends on the
[ADR-0043](0043-public-reachability-is-a-loadbalancer.md) servicelb detection fix; single node
means no HA. Relates to [ADR-0038](0038-scoped-agent-credential.md),
[ADR-0034](0034-agent-native-onboarding.md).

## Context

Burrow today requires a pre-existing Kubernetes cluster. The reference target (a managed
DigitalOcean cluster plus a cloud load balancer) runs about $24/mo. A large, vocal segment of
self-hosters wants a cheaper path: a single ~$5 VPS. That segment is disproportionately active on
forums, so serving it well is worth it for adoption, not just revenue.

k3s is the natural fit: a single binary that ships servicelb, traefik, and local-path storage. The
servicelb piece is the unlock, on a single node, a `type: LoadBalancer` Service is assigned the
node's own IP for free (proven in e2e #193, where a LoadBalancer got a real address on k3s with no
cloud provider). So a single VPS is a complete Burrow target with no cloud load balancer: the node
IP is the public IP DNS points at, and HTTP-01 works because the node serves :80.

The missing piece is getting from "just a VPS" to "k3s + Burrow installed," in as close to one
step as is safe.

## Decision (proposed)

Burrow gains a way to provision a **single-node k3s cluster** on a VPS and install itself into it
in one flow, using the upstream k3s installer. Single-node by default and growable (add k3s agents
later). The public IP is the node's IP via servicelb; no cloud load balancer.

**The open question to investigate (why this is Proposed):**

- **Option A, SSH from the laptop.** `burrow cluster create --ssh root@<ip>` runs on the laptop,
  SSHes into the VPS, installs k3s and Burrow, and fetches the kubeconfig (rewritten to the public
  IP) back to the laptop.
  - *Pros:* one command from where the user already works; the kubeconfig lands automatically, so
    they can operate immediately.
  - *Cons:* Burrow needs an SSH client with host-key and key handling, becomes an SSH orchestrator
    (scope creep toward config management), and acts as root on the user's box over the wire.
- **Option B, install on the VPS.** The user SSHes into their box (as they already do) and runs an
  installer there (`curl https://get.burrow.dev | sh`, or a `burrow` binary), which installs k3s
  and Burrow locally and mints the scoped agent credential
  ([ADR-0038](0038-scoped-agent-credential.md)) to copy to the laptop.
  - *Pros:* no SSH client in Burrow, no key handling; matches how self-hosters already work; reuses
    the scoped-credential model for laptop access.
  - *Cons:* two steps (SSH in, run the installer) plus copying the scoped kubeconfig to the laptop.

Current lean (not decided): **Option B as the primary** (simpler, safer, self-hoster-native, and
the kubeconfig-to-laptop step is already the scoped-credential flow), with Option A as an optional
laptop-first convenience added later. To be validated before this ADR is accepted.

## Consequences

- Expands Burrow's scope from "operate your cluster" to "provision it on a VPS." A new install
  surface, and for Option A an SSH-as-root trust surface, both to design carefully.
- Single node means no high availability (a node failure takes the site down). Fine for the cheap
  tier, but stated plainly.
- Requires the servicelb detection fix from ADR-0043, or Burrow will report LoadBalancer
  unsupported on the k3s node and mis-steer the user. The VPS is the concrete driver for that fix.
- The VPS must expose :80/:443 (public traffic and HTTP-01) and :6443 (the API server the laptop
  reaches burrowd through), plus :22 for Option A. The flow should check and guide.
- Whether to vendor the k3s install logic or lean on an existing tool (k3sup, MIT) is a build
  detail settled after the mechanism question.

## Rejected alternatives

- **Require a managed cluster.** Excludes the cheap self-hoster tier this ADR exists to serve.
- **A full multi-node / Terraform provisioner.** Too heavy for the goal; start single-node and grow
  by adding k3s agents.
- **Only document an external tool (k3sup) with no integration.** Fine as an interim, but the
  one-command story is the differentiator worth owning.
