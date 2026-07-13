# ADR-0053: In-cluster build from a source reference (optional, off the explicit spine)

## Status

✅ Accepted

## TL;DR

Burrow stays **client-build-first** — build in CI or locally, push, deploy by image
reference. This ADR adds an **optional** alternative for users who do not want a CI
dependency: Burrow builds their image **inside their own cluster** from a git reference. The
trigger is an **explicit** call (`burrow app build <app> --source <git-ref>` and a `burrow-agent build` verb), mirroring [ADR-0007](0007-explicit-deploy-by-image-reference.md); the control plane
reaches **outward** to clone and build, so it needs no inbound endpoint. **Code never crosses
the control channel** ([ADR-0004](0004-code-never-over-mcp.md)) — only the git ref crosses; the
builder clones the source inside the cluster. The build runs as a Kubernetes Job producing an
image, whose **reference then flows into the existing guarded deploy path** — same rollout,
record, rollback handle, audit entry. An **optional in-cluster registry** is the zero-config
default push target, removing the external-registry-account friction. An optional
**passive build-on-push** mode reuses the outbound-only **poll** shape of
[ADR-0052](0052-pull-based-passive-deploy.md), so a private, NAT'd cluster needs no inbound
setup. Layers on [ADR-0004](0004-code-never-over-mcp.md) and
[ADR-0007](0007-explicit-deploy-by-image-reference.md); reuses the pull-based shape of
[ADR-0052](0052-pull-based-passive-deploy.md). Supersedes nothing.

## Context

Deploying by image reference ([ADR-0007](0007-explicit-deploy-by-image-reference.md)) assumes
the image already exists in a registry the cluster can pull from
([ADR-0004](0004-code-never-over-mcp.md)). Getting it there is the developer's job, and Burrow
supports two build paths ([ADR-0008](0008-two-build-paths.md)): the self-host developer builds
and pushes (client-side), or a managed platform builds server-side. For the solo developer who
is Burrow's first user, the client-side path still carries two frictions that this ADR removes:

1. **A CI dependency for build-on-push.** To turn "I pushed a commit" into "a new image
   exists," the developer wires a CI system (GitHub Actions or similar) that builds and pushes.
   That is one more account, one more config file, and one more system to keep working — for a
   user who just wants their code to run on their own cluster.

2. **An external registry account and pull tokens.** Even with a local build, the image has to
   land in a registry the cluster can pull from, which means an external registry account
   (GHCR, DigitalOcean, Docker Hub) and a generated pull credential wired as an imagePullSecret
   ([ADR-0017](0017-private-registry-authentication.md)). [ADR-0046](0046-registry-onboarding.md)
   already works to reduce this; an in-cluster registry removes the external account entirely.

Building inside the cluster addresses both — but building is code execution, and Burrow's whole
architecture is built to keep code off the control channel and keep the explicit call the spine.
So the in-cluster build must be added **without** violating those invariants: it stays optional,
stays explicit, and reuses the existing guarded deploy path rather than becoming a second,
unguarded way in.

## Decision

### 1. An optional in-cluster build path, never the deploy spine

Burrow adds an **optional** path where the control plane builds the user's image inside their
own cluster from a git reference. It does **not** change the default: Burrow remains
client-build-first, and deploy remains **by image reference**
([ADR-0007](0007-explicit-deploy-by-image-reference.md)). In-cluster build is a convenience for
users who do not want a CI dependency; it is never the foundation the system rests on. Everything
downstream of a successful build is the existing deploy path, unchanged.

### 2. The trigger is an explicit call; the control plane reaches outward

Build is triggered by an **explicit** call, exactly as deploy is
([ADR-0007](0007-explicit-deploy-by-image-reference.md)): `burrow app build <app> --source <git-ref>`
on the human admin CLI (under the `app` subcommand, beside `burrow app deploy`), and a
`burrow-agent build` verb the agent invokes
([ADR-0049](0049-burrow-agent-scoped-cli-control-channel.md)). The control plane reaches
**outward** to clone the git reference and run the build; it needs **no inbound or public
endpoint** to be driven. This is the spine for build in the same sense that the explicit call is
the spine for deploy — the one place the guardrails, the structured feedback, and (once the image
exists) the rollback handle attach.

### 3. Code never crosses the control channel — only the git ref does

The invariant of [ADR-0004](0004-code-never-over-mcp.md) holds unchanged. The only thing that
crosses the control channel is **metadata**: the git reference (a repository URL plus a commit or
tag). The **builder** clones the actual source from git **inside the cluster**; the agent never
carries a tarball, a diff, or any source bytes. *The control channel is the remote control; git is
where the source comes from, and the registry is still the conveyor belt for the built image.*

### 4. The build runs as a Kubernetes Job, and its output rejoins the guarded deploy path

The build runs as a **Kubernetes Job in the user's own namespace**, using a standard builder. The
builders are **Cloud Native Buildpacks** and **buildah**: Buildpacks is the friendly
default for the **no-Dockerfile** case (it detects the language and produces an image with no
build recipe to write), and **buildah** is used when a **Dockerfile is present** (a daemonless,
unprivileged in-cluster image build from the user's own Dockerfile). Kaniko, once the obvious
choice here, is archived upstream and is not used; BuildKit is the alternative to buildah when
aggressive layer caching is wanted. On success the Job produces an image, and
the **resulting image reference flows into the existing guarded deploy path**
([ADR-0006](0006-guardrails-in-the-control-plane.md),
[ADR-0007](0007-explicit-deploy-by-image-reference.md)): the same rollout, the same deploy record
with its `Supersedes` chain (the rollback handle), the same audit entry
([ADR-0027](0027-audit-log.md)). **Build is a front-end that ends where deploy begins.** It does
not replace deploy, and it produces nothing downstream can tell apart from an
externally-built image except in its recorded provenance.

### 5. Where the built image goes: an optional in-cluster registry as the zero-config default

The built image needs a registry the cluster can pull from. The default push target for self-host
is an **optional lightweight in-cluster container registry** — **Zot** (the OCI-native
`project-zot` registry), chosen for its minimal footprint, with the CNCF **`distribution`**
registry (formerly Docker Distribution) as a heavier alternative
— reachable by an **in-cluster service name** and wired by `burrow install`. On k3s this means
configuring the containerd registry (a `registries.yaml` mirror / registry config) so the kubelet
resolves the in-cluster name and pulls from it. This removes the need to create an external
registry account and generate pull tokens for the in-cluster build case. **External registries
(GHCR, DigitalOcean, Docker Hub) remain fully supported**; the in-cluster registry is the
zero-config default, not the only option. Deploy stays **strictly by image reference**
([ADR-0007](0007-explicit-deploy-by-image-reference.md)) — the in-cluster registry is simply a
registry that happens to be local.

### 6. The builder sits behind a minimal `Builder` seam

The builder is a **seam** — a `Builder` interface with a real adapter and a fake, the same
pattern as every other Burrow dependency that touches the cluster, the registry, the clock, or the
database (CLAUDE.md). The interface is deliberately **minimal**: it takes a source reference and a
target image reference and returns a resulting image **digest** or an **error**. Nothing more.
Isolation and sandboxing are expressed **inside an implementation**, not as interface knobs. This
is deliberate: the separate commercial multi-tenant product must be able to supply a hardened,
sandboxed executor behind the same seam without the OSS interface having to anticipate its needs.
The OSS interface is not over-coupled to the cloud product — it describes "turn this source into
this image," and each side implements the trust model it needs.

### 7. Trust model: this OSS path is single-tenant, so no sandbox is required

The OSS build path is **single-tenant**. The user owns the cluster **and** the source, so
build-time arbitrary code execution is a risk **only to themselves** — the same trust posture as
running `docker build` on their own laptop. No sandbox is required in OSS; the build runs in the
user's own namespace under restricted PodSecurity as defense in depth (§ Consequences), not as an
isolation boundary against an adversary. The **multi-tenant** case — where the build runs
**untrusted strangers' source** and must itself be sandboxed like the workload — is explicitly
**out of scope here** and belongs to the commercial product's own ADR, which swaps the executor
behind the § 6 seam.

### 8. Optional passive build-on-push is a poll, not a webhook

For a self-hosted user who wants **build-on-git-push**, the passive mode is a **poll** of the git
reference (pull), reusing the **outbound-only** shape of
[ADR-0052](0052-pull-based-passive-deploy.md): burrowd periodically checks the ref for a new
commit and, when one appears, fires the explicit build-then-guarded-deploy path. This requires
**no domain, no ingress, and no webhook**. A **webhook receiver** (push) is possible **only** if
the user has exposed their control-plane API through the `--with-ingress` public-HTTPS stack
(#233) with a domain; that is the **advanced** path, not the spine. Pull is the default precisely
because Burrow's ICP runs **private, NAT'd, or firewalled** clusters that cannot accept an inbound
webhook — identical reasoning to [ADR-0052](0052-pull-based-passive-deploy.md), where the same
reachability wall made pull the primary and push the optional public-cluster path.

## Consequences

- **No CI dependency for build-on-push.** A self-hosted user can go from a git commit to a
  running image without wiring an external CI system — the control plane clones and builds inside
  their own cluster on the outbound-only poll.
- **No external-registry token friction.** The optional in-cluster registry is the zero-config
  default push target, so the in-cluster build case needs no external registry account and no
  generated pull token; external registries stay fully supported.
- **All invariants hold.** Code never crosses the control channel
  ([ADR-0004](0004-code-never-over-mcp.md)); the trigger is explicit
  ([ADR-0007](0007-explicit-deploy-by-image-reference.md)); the built image rejoins the guarded
  deploy path so guardrails ([ADR-0006](0006-guardrails-in-the-control-plane.md)), the deploy
  record, rollback, and the audit log ([ADR-0027](0027-audit-log.md)) are never bypassed.
- **Reuses existing machinery.** The build ends where deploy begins (§ 4), and the passive mode
  reuses the [ADR-0052](0052-pull-based-passive-deploy.md) poller shape rather than inventing a
  new inbound surface. The `Builder` seam lets the commercial product layer sandboxing without an
  OSS change (§ 6).
- **In-cluster build consumes cluster resources.** A build is CPU- and memory-hungry; the build
  Job must carry **resource caps** so a build cannot starve running workloads on a small node.
- **An in-cluster registry needs storage and garbage collection.** The in-cluster registry needs a
  persistent volume and a GC policy so accumulated build layers do not fill the disk.
- **Building is still code execution.** Even though it is trusted (§ 7), the build Job runs in the
  user's namespace under **restricted PodSecurity** (non-root, dropped capabilities) as defense in
  depth — not because the OSS path faces an adversary, but so the single-tenant posture is a
  deliberate floor rather than an accident.

## Rejected alternatives

- **Build over the control channel.** Rejected: it would carry source bytes over the control path,
  violating [ADR-0004](0004-code-never-over-mcp.md). Only the git reference crosses; the builder
  clones the source from git inside the cluster (§ 3).
- **External-registry-only (no in-cluster registry).** Rejected as the sole option: it keeps the
  account-and-token friction this ADR removes (§ 5). External registries remain supported, but the
  in-cluster registry is the zero-config default.
- **Webhook-push as the build trigger.** Rejected as the spine: it needs an inbound, publicly
  reachable endpoint that a NAT'd cluster cannot accept — the same rejection as
  [ADR-0052](0052-pull-based-passive-deploy.md). Pull is the default; the webhook is available
  only on the advanced public-HTTPS path (§ 8).
- **Making build the deploy spine.** Rejected: deploy stays by image reference
  ([ADR-0007](0007-explicit-deploy-by-image-reference.md)) and build is an optional front-end that
  ends where deploy begins. The explicit deploy call remains canonical.
- **A heavyweight in-cluster CI system.** Rejected as out of scope: this is **one Job producing one
  image**, not a pipeline engine with stages, caches, and a job graph. A build orchestrator is a
  different product.
