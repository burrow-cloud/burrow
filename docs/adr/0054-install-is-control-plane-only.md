# ADR-0054: `install` provisions only the control plane; additive components are standalone `cluster` commands

## Status

✅ Accepted

## TL;DR

`burrow install` and `burrow cluster bootstrap` provision **only the control plane**. Every
additive cluster component — ingress/TLS, the optional in-cluster registry, and future ones — is a
**standalone, opt-in `burrow cluster <component>` command** with its own `status` / `install` /
`uninstall`, not a `--with-*` convenience flag on install or bootstrap. This ADR adds
`burrow cluster registry` for the in-cluster registry and removes the shipped `--with-registry` and
`--with-ingress` flags.

The in-cluster registry is a **single cluster-agnostic mechanism**: Zot behind the cluster ingress,
with TLS issued by the existing Let's Encrypt HTTP-01 `letsencrypt` ClusterIssuer. The in-cluster
build pushes to an **internal ClusterIP Service** over plain HTTP, and nodes pull the image through
the **ingress over TLS** with a generated pull credential — **no node or containerd editing**, so it
behaves the same on k3s and a managed cluster like DOKS. It therefore **depends on the ingress
stack** and needs a hostname; a cluster with no domain cannot use it today (documented limitation).

Extends the CLI command taxonomy ([ADR-0024](0024-cli-command-taxonomy.md)) and the CLI onboarding
and organization decision ([ADR-0037](0037-cli-onboarding-and-organization.md)); governs how the
optional in-cluster registry of ([ADR-0053](0053-in-cluster-build-from-source.md)) is provisioned.
Supersedes nothing.

## Context

`burrow install` deploys the control plane; `burrow cluster bootstrap` turns a bare VPS into a
single-node cluster and deploys the control plane onto it ([ADR-0044](0044-single-vps-k3s-cluster.md)).
Two additive components then appeared that a cluster may or may not want: the ingress/TLS stack
([ADR-0018](0018-reaching-an-app-at-a-url.md), [ADR-0034](0034-agent-native-onboarding.md)) and the
optional in-cluster image registry ([ADR-0053](0053-in-cluster-build-from-source.md)).

The ingress/TLS stack already had a standalone home: `burrow cluster ingress install`, a one-time
operator setup run with the kubeconfig. But convenience flags then crept onto the provisioning
commands — `burrow install --with-registry`, and `burrow cluster bootstrap --with-ingress` and
`--with-registry` — each folding an additive component into the install/bootstrap run.

Those flags are a design mistake worth correcting before more accrete. They make "what does install
do?" depend on which flags were passed, so the answer drifts per invocation. They bury a component
behind a flag on an unrelated command, where it is undiscoverable: a user looking for the in-cluster
registry finds nothing named "registry" to run, and cannot inspect or remove what a one-shot flag
installed. And they scale badly — every new additive component would want its own `--with-*` flag on
both install and bootstrap, multiplying the surface. The in-cluster registry, in particular, needs
its own inspect-and-remove lifecycle (it costs a PersistentVolume and wires the node's containerd),
which a one-way install flag cannot express.

## Decision

### 1. `install` and `bootstrap` provision only the control plane

`burrow install` and `burrow cluster bootstrap` provision the control plane and nothing else. They
never install ingress, the in-cluster registry, or any other additive component. Their output states
this and points at the standalone commands for the additive pieces.

### 2. Every additive component is a standalone, opt-in `burrow cluster <component>` command

Each additive cluster component is its own noun under `burrow cluster`, with a consistent shape:

- bare `burrow cluster <component>` reports whether the component is installed (read-only status);
- `burrow cluster <component> install` provisions it;
- `burrow cluster <component> uninstall` removes it (implementing what is safe, and documenting any
  residue it cannot cleanly reverse).

`burrow cluster ingress` (with `install`) is the existing example. This ADR adds
`burrow cluster registry` for the optional in-cluster registry of
([ADR-0053](0053-in-cluster-build-from-source.md)): its `install` deploys the lightweight registry
behind the cluster ingress and wires it as burrowd's zero-config default build push target (§5); its
`uninstall` reverses those; the bare command reports status. It is a kubeconfig-side operator setup,
not an agent operation, and does not route through burrowd's guarded API — the same posture as
`burrow cluster ingress install`.

The in-cluster registry is deliberately named `burrow cluster registry`, distinct from
`burrow config registry`, which manages pull credentials for **external** registries
([ADR-0017](0017-private-registry-authentication.md)). The two use the standard "registry"
vocabulary and cross-reference each other in help: one manages the registry that runs *in* the
cluster, the other the credentials to pull from registries *outside* it.

### 3. No `--with-*` convenience flags on install or bootstrap

Install and bootstrap carry no `--with-<component>` flags. The removed `--with-registry` (on
`burrow install` and `burrow cluster bootstrap`) and `--with-ingress` (on `burrow cluster bootstrap`)
are gone; the standalone commands are the only way to add these components.

### 4. An additive component may depend on another, and verifies it

A standalone component whose install needs another component present verifies that dependency and
fails with an error naming the command that provides it, rather than half-installing. The in-cluster
registry (§5) depends on the ingress stack for its public TLS endpoint:
`burrow cluster registry install` checks that the ingress-nginx controller, cert-manager, and the
`letsencrypt` ClusterIssuer are present and, if any is missing, refuses and points at
`burrow cluster ingress install`. This keeps each component standalone and opt-in while making the
dependency explicit and actionable.

### 5. The in-cluster registry is Kubernetes-native: internal Service push, public ingress pull

The optional in-cluster registry ([ADR-0053](0053-in-cluster-build-from-source.md) §5) runs as a
single mechanism that is identical on every cluster type — a plain Kubernetes-native service, with no
node-specific or containerd editing:

- **Zot** runs as a Deployment with a PersistentVolumeClaim in the control-plane namespace.
- An **internal ClusterIP Service** is the push endpoint. The in-cluster build pushes to it directly
  over plain HTTP; it needs no external auth because it is only reachable in-cluster. burrowd's
  `BURROW_BUILD_REGISTRY` points at this Service's cluster-DNS name.
- An **Ingress vhost** at `--host` is the pull endpoint. It is annotated to use the existing
  `letsencrypt` HTTP-01 ClusterIssuer, so the certificate is issued through the same ingress the rest
  of the cluster uses — no DNS-01 solver and no second issuer. `nginx.ingress.kubernetes.io/proxy-
  body-size: "0"` is set so large image layers push and pull. The public endpoint is protected with
  ingress-layer nginx basic auth backed by a generated credential, so the *internal* push path stays
  credential-free while the *external* pull path requires auth.
- A dedicated **imagePullSecret** carrying that credential is installed in the app namespace (the
  same [ADR-0017](0017-private-registry-authentication.md) pull-secret path `burrow config registry`
  uses), so app Pods pull the public host.
- **Push endpoint and pull reference differ, but resolve to the same stored image.** The build pushes
  to the internal Service endpoint, but the built image is deployed by the **public** reference
  `<host>/<app>:<tag>@<digest>` so nodes pull it through the ingress. Because a registry's stored
  repository path is independent of the endpoint host used to reach it, the internal push and the
  public pull share the same repository path and digest and therefore address the same bytes.
  burrowd carries both: `BURROW_BUILD_REGISTRY` (internal push endpoint) and
  `BURROW_BUILD_PUBLIC_REGISTRY` (public pull host).
- **The internal push is plain HTTP.** The in-cluster build pushes to the internal Service over plain
  HTTP: the engine is the single place that knows the in-cluster endpoint is insecure, and it marks
  the push so the build recipe pushes with buildah `--tls-verify=false` (which also allows the plain-
  HTTP fallback). This applies only to the push to the in-cluster registry — an explicit external
  target is always pushed over TLS, and the base-image pull is always verified.

**Known limitation — buildpacks push to the in-cluster registry.** The insecure push to the plain-HTTP
in-cluster registry is wired for the **Dockerfile / buildah** path only. The no-Dockerfile **Cloud
Native Buildpacks** path has no insecure-push handling wired, so a buildpacks build with no explicit
target fails fast with an actionable message; it works against an external (TLS) registry, and pushing
to the in-cluster registry from the buildpacks path is a follow-up.

**Known limitation — no-domain clusters.** The ingress-and-TLS pull path requires a hostname. A
cluster with no domain cannot use the in-cluster registry today; `install` fails cleanly and says so.
A future no-domain fallback can wire the node's container runtime to trust the in-cluster registry
directly via the runtime-agnostic containerd `certs.d` mechanism (not k3s-specific files); it is out
of scope here.

## Consequences

- **`install` has one meaning.** What `burrow install` and `burrow cluster bootstrap` do no longer
  varies with flags — they provision the control plane, full stop, which is simpler to document,
  reason about, and keep honest.
- **Additive components are discoverable and inspectable.** Each is a named noun with a status and an
  uninstall, so a user can list, add, inspect, and remove it, rather than divine what a one-shot flag
  left behind.
- **The pattern scales.** A new additive component is a new `burrow cluster <component>` with the same
  three verbs, adding no flag to install or bootstrap.
- **Two steps instead of one for a full public setup.** A user who wants a VPS that serves a public
  HTTPS site now runs `burrow cluster bootstrap` and then `burrow cluster ingress install` (and
  `burrow cluster registry install` if they want the in-cluster registry), rather than one flagged
  bootstrap. The extra step is the cost of the clarity above, and each step is independently
  inspectable and reversible.
- **The in-cluster registry gains a real lifecycle.** Because it is a standalone command, it can be
  inspected and uninstalled, which a one-way install flag never offered.

## Rejected alternatives

- **`--with-registry` / `--with-ingress` convenience flags on install and bootstrap** (the shipped
  design, now removed). Rejected: one-shot flags blur what `install` means (its behavior varies per
  invocation), hide additive components behind a flag on an unrelated command where they are
  undiscoverable and un-removable, and scale poorly — each new component would want its own flag on
  both commands. A standalone command per component is discoverable, inspectable, reversible, and
  uniform.
- **The in-cluster registry as a NodePort Service mirrored into the node's containerd via k3s
  `registries.yaml`** (the earlier design, now dropped). Rejected: editing
  `/etc/rancher/k3s/registries.yaml` on the node is k3s-specific — it does not work on a managed
  cluster like DOKS — and it requires running the install on the node itself, splitting the registry's
  behavior by cluster type instead of keeping one mechanism that works everywhere
  ([ADR-0046](0046-registry-onboarding.md)). Routing pulls through the cluster ingress with TLS (§5)
  is one mechanism that works identically everywhere and reuses the ingress the cluster already runs,
  at the cost of requiring a hostname (the documented no-domain limitation).
- **A single `burrow cluster addons` umbrella command.** Rejected as premature and vaguer than named
  nouns: `burrow cluster ingress` and `burrow cluster registry` say exactly what they manage, matching
  the noun-grouped taxonomy ([ADR-0024](0024-cli-command-taxonomy.md)); a generic "addons" surface
  would re-introduce the discoverability problem it aims to solve.
- **Folding the components into the agent-driven control-plane API.** Rejected: installing a
  cluster-wide controller or a registry with a PersistentVolume and node containerd wiring is
  privileged operator setup done with the kubeconfig, not an agent operation — the same boundary that
  keeps `burrow cluster ingress install` off the guarded API.
