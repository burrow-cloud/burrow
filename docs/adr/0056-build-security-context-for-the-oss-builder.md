# ADR-0056: The OSS build container's security context — relax restricted PodSecurity for the rootless builder (refines ADR-0053 §7)

## Status

♻️ Superseded by [ADR-0059](0059-oss-build-container-runs-privileged.md)

## TL;DR

The in-cluster build's restricted PodSecurity floor ([ADR-0053](0053-in-cluster-build-from-source.md)
§7) cannot run a rootless container builder: buildah fails to create the user namespace it needs
(`unshare(CLONE_NEWUSER): Operation not permitted`). This ADR refines §7 for the OSS builder: the
build container's security context is **relaxed to the minimal set the rootless builder needs** to
run — recommended as `seccompProfile: Unconfined` plus the `SETUID`/`SETGID` capabilities and
`allowPrivilegeEscalation: true`, while keeping the non-root UID, the read-only root filesystem,
every other capability dropped, and the resource caps. This is acceptable because the OSS path is
**single-tenant and the user owns the source**, so §7 was defense in depth, not an adversary
boundary — §7 says exactly this. The isolation hook for the **untrusted-source** case already
exists: the `Builder` seam ([ADR-0053](0053-in-cluster-build-from-source.md) §6) lets the
commercial multi-tenant product supply a hardened, gVisor-sandboxed executor (cloud ADR-0003), so
isolation there moves to gVisor/microVM, not PodSecurity.

Refines [ADR-0053](0053-in-cluster-build-from-source.md) §7 for the OSS builder; preserves its §6
`Builder` seam as the isolation hook; interacts with the commercial product's own build-isolation
decision (cloud ADR-0003). Supersedes nothing.

## Context

[ADR-0053](0053-in-cluster-build-from-source.md) builds the user's image inside their own cluster
as a Kubernetes Job (§4) and runs that Job under a **restricted PodSecurity** floor (§7): non-root
with a fixed unprivileged UID/GID, the **RuntimeDefault** seccomp profile, `allowPrivilegeEscalation:
false`, **all** Linux capabilities dropped, and a read-only root filesystem. §7 is explicit that this
is **defense in depth**, not an isolation boundary against an adversary — the OSS path is
single-tenant, and build-time code execution is a risk only to the user who owns both the cluster and
the source.

That floor is incompatible with a rootless container builder. **Building a container image inside a
container fundamentally requires either root-in-the-container or a user namespace** — there is no
third way to lay down image layers with the right ownership. The rootless builder chosen for the
Dockerfile case, **buildah**, needs a user namespace, and the §7 floor blocks it on two fronts
(observed during in-cluster-build validation, issue #282):

- **The `unshare(CLONE_NEWUSER)` is denied.** The RuntimeDefault seccomp profile only permits the
  namespace-creating `clone`/`unshare` when the caller holds `CAP_SYS_ADMIN`; with all capabilities
  dropped it is refused outright — the literal `unshare(CLONE_NEWUSER): Operation not permitted`.
- **The setuid uid-map helpers are blocked.** `allowPrivilegeEscalation: false` sets `no_new_privs`,
  which neuters the setuid `newuidmap`/`newgidmap` helpers rootless buildah uses to write its uid/gid
  maps. `BUILDAH_ISOLATION=chroot` already avoids a user namespace for the build's own `RUN` steps,
  but not for buildah's rootless *setup*, which still maps ids.

The one builder that avoided a user namespace, **Kaniko**, is archived upstream and was already
rejected in [ADR-0053](0053-in-cluster-build-from-source.md) §4. So the choice is not "find a builder
that runs under §7 unchanged" — none does — but "how much of §7 to relax, and on what grounds."

## Decision

### 1. The OSS build container relaxes its security context to the minimal set the rootless builder needs

The build container's security context is relaxed from the §7 floor by **exactly the set the rootless
builder requires to create its user namespace and write its uid/gid maps, and no more**. The exact set
is **pinned during implementation and validated on a real cluster** (rootless-builder-in-Kubernetes
security contexts are notoriously fiddly; expect a round or two of iteration), but the recommended
target — the narrowest relaxation that is known to work — is:

- `seccompProfile: Unconfined` on the build container, so the `unshare(CLONE_NEWUSER)` is permitted.
  This is preferred over adding `CAP_SYS_ADMIN` (which the RuntimeDefault profile would also accept):
  unconfining seccomp opens only syscalls, whereas `CAP_SYS_ADMIN` is the single broadest capability
  in Linux ("the new root"). The narrower relaxation wins.
- Add `SETUID` and `SETGID` capabilities (still dropping all others), so the builder can set up its
  id maps.
- `allowPrivilegeEscalation: true`, so the setuid `newuidmap`/`newgidmap` helpers can run.

Everything §7 hardening that is **not** load-bearing for the builder is **kept** (§4 below): the
non-root UID/GID, the read-only root filesystem (every write path is already a writable emptyDir), all
capabilities except the two named above dropped, and the build's resource caps. The relaxation applies
only to the **build** container; the clone init container keeps the full §7 floor, since a shallow
`git fetch` needs none of it.

### 2. The single-tenant trust posture is what makes this acceptable

This relaxation is defensible for the OSS path for the reason §7 already states: the OSS build path is
**single-tenant, and the user owns the source**. Build-time arbitrary code execution is a risk **only
to the user themselves** — the same posture as running `docker build` on their own laptop, which runs
with far more privilege than even the relaxed context here. §7's restricted PodSecurity was **defense
in depth, not an adversary boundary**; relaxing it to run the builder therefore lowers a
belt-and-braces floor, it does not breach a security boundary the OSS threat model depends on. The
decision is made **deliberately and in the open** here rather than left as an accidental byproduct of
making the build work.

### 3. Isolation for the untrusted-source case stays behind the `Builder` seam, not PodSecurity

The case that **does** need an adversary boundary — a build running **untrusted strangers' source**
in the commercial multi-tenant product — is explicitly out of scope for the OSS path
([ADR-0053](0053-in-cluster-build-from-source.md) §7) and is **not** addressed by tightening this
PodSecurity context. Its isolation hook already exists: the minimal `Builder` seam
([ADR-0053](0053-in-cluster-build-from-source.md) §6) lets the commercial product swap in a hardened,
**gVisor-sandboxed** (or microVM) executor behind the same interface (cloud ADR-0003). Isolation for
untrusted source lives **inside that implementation**, at the gVisor/microVM layer — not in the OSS
build container's PodSecurity. This ADR **refines §7's PodSecurity floor for the OSS builder only**;
it leaves the seam, and the multi-tenant story it enables, untouched.

### 4. The residual hardening that is kept

The relaxation is bounded. The OSS build container still runs with a **non-root UID/GID**, a
**read-only root filesystem** (writes go only to the workspace, `$HOME`, and `/tmp` emptyDirs), **all
capabilities dropped except** the minimal `SETUID`/`SETGID` the builder needs, and the **resource
caps** that keep a build from starving workloads on a small node
([ADR-0053](0053-in-cluster-build-from-source.md) Consequences). The only ceilings lowered are the
three the rootless builder cannot run without. The single-tenant posture is thus a **deliberate
floor**, not an all-or-nothing "make it privileged."

## Consequences

- **The in-cluster Dockerfile build runs.** With the relaxed context, rootless buildah can create its
  user namespace and complete a build inside the user's own cluster — the whole point of
  [ADR-0053](0053-in-cluster-build-from-source.md).
- **The build container is less hardened than a workload pod.** It runs with `Unconfined` seccomp and
  two extra capabilities. On a cluster enforcing the Pod Security Admission `restricted` profile, the
  build namespace needs the `baseline` (or a targeted exception) level — a documented install
  consequence, not a surprise. The relaxation is scoped to the build container, not the app.
- **The exact set is validated, not assumed.** The recommended set (§1) is the starting point;
  implementation pins the minimal working set on a real cluster and records it in the build adapter's
  comments against this ADR. If a single-uid mapping (no `/etc/subuid` range) proves robust enough for
  real Dockerfiles, it could drop `allowPrivilegeEscalation` by writing the uid map directly with
  `CAP_SETUID` — but single-uid mapping breaks builds that `chown` to arbitrary uids, so it is not the
  default recommendation.
- **The multi-tenant boundary is unaffected.** Because untrusted-source isolation was never carried by
  this PodSecurity context (§3), relaxing it changes nothing for the commercial product, which
  isolates builds at the gVisor/microVM layer behind the `Builder` seam.
- **No change to the client-build-first default.** This touches only the optional in-cluster build
  path; the default remains build-in-CI-or-locally and deploy by image reference
  ([ADR-0007](0007-explicit-deploy-by-image-reference.md)).

## Rejected alternatives

- **Keep §7 as-is.** Rejected: the build simply cannot run. RuntimeDefault seccomp with all
  capabilities dropped denies `unshare(CLONE_NEWUSER)`, and `no_new_privs` blocks the setuid uid-map
  helpers — there is no configuration of a rootless container builder that runs under the unmodified
  §7 floor. Honesty about a non-working feature is worse than a documented, bounded relaxation.
- **Switch builders to escape the tradeoff.** Rejected: it does not exist. **Kaniko**, the one builder
  that avoided a user namespace, is archived upstream and already rejected
  ([ADR-0053](0053-in-cluster-build-from-source.md) §4). **BuildKit rootless** — the layer-caching
  alternative to buildah — needs the **same** user namespace and hits the identical `unshare` wall, so
  it does not escape the tradeoff; it only moves it. Building a container in a container needs
  root-in-container or a userns, full stop.
- **Run the builder as root-in-the-container** (drop `RunAsNonRoot`, set UID 0) instead of the narrow
  cap/seccomp relaxation. Rejected in favor of §1: root-in-container gives up the non-root UID, a
  meaningful residual floor that the recommended set keeps, for no reduction in the actual capability
  the builder needs. Keeping the build unprivileged-but-for-two-caps is a better single-tenant floor
  than a root container.
- **Grant `CAP_SYS_ADMIN` instead of unconfining seccomp.** Rejected: `CAP_SYS_ADMIN` is the single
  broadest Linux capability and would let the RuntimeDefault profile permit the `unshare`, but it
  grants far more than the build needs. `seccompProfile: Unconfined` plus `SETUID`/`SETGID` is the
  narrower relaxation and is preferred (§1).
- **Tighten this PodSecurity context to serve the multi-tenant case.** Rejected as a category error:
  the untrusted-source boundary is not, and was never, carried by the OSS build container's
  PodSecurity (§3). It belongs to the commercial product's gVisor/microVM executor behind the
  [ADR-0053](0053-in-cluster-build-from-source.md) §6 `Builder` seam and to its own ADR (cloud
  ADR-0003), not to this floor.
