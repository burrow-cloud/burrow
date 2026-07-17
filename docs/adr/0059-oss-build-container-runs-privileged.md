# ADR-0059: The OSS build container runs privileged (supersedes ADR-0056)

## Status

✅ Accepted

## TL;DR

[ADR-0056](0056-build-security-context-for-the-oss-builder.md) proposed running the OSS in-cluster
build container with a **narrow** relaxation of [ADR-0053](0053-in-cluster-build-from-source.md) §7 —
`seccompProfile: Unconfined` plus the `SETUID`/`SETGID` capabilities and `allowPrivilegeEscalation:
true`, keeping the read-only root filesystem and dropping every other capability — and explicitly
rejected "make it privileged." **Validated on a live managed cluster (DigitalOcean DOKS /
containerd), that narrow set does not complete a build.** It clears the `unshare(CLONE_NEWUSER)` wall
ADR-0056 targeted, but not the next one: buildah's layer extraction (`chrootarchive`) remounts the
container **root mount** private, and a managed CRI **locks** that mount, so the remount is denied
even inside buildah's own user namespace, even with `CAP_SYS_ADMIN`, and even with a writable root
filesystem. The only context that completes a build there is a **privileged** build container. This
ADR therefore supersedes ADR-0056's *mechanism*: the OSS build container runs `privileged: true` with
`seccompProfile: Unconfined` and a writable root filesystem. **ADR-0056's trust argument is unchanged
and is what still makes this acceptable** — the OSS build path is single-tenant, the user owns the
source, so §7 was defense in depth, not an adversary boundary; isolation for the untrusted-source
(multi-tenant) case stays behind the `Builder` seam ([ADR-0053](0053-in-cluster-build-from-source.md)
§6) in a gVisor/microVM executor (cloud ADR-0003), which never runs a privileged pod on a shared node.

Supersedes [ADR-0056](0056-build-security-context-for-the-oss-builder.md) (its narrow-relaxation
mechanism only; its trust posture and its §6-seam isolation hook carry forward unchanged). Refines
[ADR-0053](0053-in-cluster-build-from-source.md) §7 for the OSS builder.

## Context

ADR-0056 refined [ADR-0053](0053-in-cluster-build-from-source.md) §7 to make a rootless container
builder run, and was careful to relax the PodSecurity floor by the *minimal* set it believed the
builder needed — Unconfined seccomp for `unshare(CLONE_NEWUSER)`, `SETUID`/`SETGID` for the uid-map
helpers, `allowPrivilegeEscalation: true` for the setuid helpers — keeping the read-only root
filesystem and rejecting a privileged container as "an all-or-nothing" overreach. It also said, in as
many words, that "rootless-builder-in-Kubernetes security contexts are notoriously fiddly; expect a
round or two of iteration," and that the exact set must be **pinned on a real cluster**. This ADR is
that pinning.

Validating the in-cluster build end to end on a live single-node **DOKS** cluster, each step of the
ADR-0056 context was exercised against the actual builder image and a real Dockerfile source. The
narrow relaxation was **necessary but not sufficient**:

- With ADR-0056's set, buildah gets **past** `unshare(CLONE_NEWUSER)` — the seccomp/cap/no-new-privs
  relaxation does its job.
- It then fails applying the base image's first layer:
  `ApplyLayer ... remount /, flags: 0x44000: permission denied`. `chrootarchive` — the helper buildah
  uses to untar a layer under a chroot — remounts `/` recursively private (`MS_REC|MS_PRIVATE`) before
  pivoting. On a managed CRI the container's root mount is created **locked** (the kubelet/containerd
  set `MNT_LOCKED`-style flags on it), and changing propagation on a locked mount is refused.

Testing the failure in a throwaway pod, mirroring the build container's context exactly, isolated the
cause conclusively:

- **`readOnlyRootFilesystem: false`** — no change; the remount is denied on a *writable* root too.
- **Adding `CAP_SYS_ADMIN`** — no change; a namespaced `CAP_SYS_ADMIN` cannot change propagation on a
  mount the parent namespace locked. `unshare -Urm --map-root-user` reproduces the identical
  `cannot change root filesystem propagation: Permission denied` outside buildah entirely.
- **`privileged: true`** — the build completes: base image pulled, `RUN` executed, image committed and
  tagged.

The wall is structural, not a missing capability: building a container image means laying down layers
under an isolating mount, and a managed CRI does not let an unprivileged (even userns-root) process
remount the locked container root. ADR-0056's preference-ordering — "Unconfined seccomp over
`CAP_SYS_ADMIN` over privileged," narrowest-first — is sound in principle but bottoms out here: on
managed Kubernetes there is no rung below privileged that runs the build.

## Decision

### 1. The OSS build container runs privileged

The build container's security context is `privileged: true`, with `seccompProfile: Unconfined`,
`allowPrivilegeEscalation: true`, and a **writable** root filesystem (`readOnlyRootFilesystem: false`
— buildah must be able to remount it). This replaces ADR-0056's `SETUID`/`SETGID` + drop-ALL +
read-only-root set, which does not complete a build on a managed CRI. Privileged supplies, in one
setting that the runtime actually honors, the unlocked mount and full capability set buildah's layer
extraction needs; the narrower rungs (Unconfined-only, `CAP_SYS_ADMIN`) were validated **not** to.

The relaxation applies **only to the build container**. The clone init container keeps the full §7
floor unchanged — a shallow `git fetch` needs none of it, so `allowPrivilegeEscalation: false`,
read-only root, and drop-ALL stay. The pod still runs as a non-root UID/GID at the pod level.

### 2. The single-tenant trust posture is what makes this acceptable — unchanged from ADR-0056

Nothing in the threat model changed; only the mechanism did. The OSS build path is **single-tenant,
and the user owns the source it builds**. Build-time arbitrary code execution is a risk **only to the
user themselves** — the same posture as running `docker build` on their own laptop, which is itself a
privileged daemon. §7's restricted PodSecurity was **defense in depth, not an adversary boundary**
([ADR-0053](0053-in-cluster-build-from-source.md) §7, [ADR-0056](0056-build-security-context-for-the-oss-builder.md)
§2); running the build privileged lowers a belt-and-braces floor for trusted code, it does not breach
a boundary the OSS threat model relies on. The trade is made **deliberately and in the open** here.

### 3. Isolation for the untrusted-source case stays behind the `Builder` seam — unchanged from ADR-0056

The case that needs a real adversary boundary — building **untrusted strangers' source** in the
commercial multi-tenant product — is out of scope for the OSS path and is **not** addressed by this
PodSecurity context. Its isolation hook is the `Builder` seam
([ADR-0053](0053-in-cluster-build-from-source.md) §6): the commercial product swaps in a hardened,
**gVisor-sandboxed** (or microVM) executor behind the same interface (cloud ADR-0003). That executor
**never runs a privileged pod on a shared node** — it isolates at the gVisor/microVM layer. Making the
OSS container privileged changes nothing for the multi-tenant story, because that story was never
carried by the OSS build container's PodSecurity.

### 4. The residual posture that is kept

Privileged is broad, so the bounding lives elsewhere and is stated plainly: the build runs in a
**dedicated `burrow-builds` namespace** (issue #278), isolated from both the app namespace and the
control-plane namespace, so a build cannot reach a running app's Secrets or burrowd's credentials and
database; the pod runs as a **non-root UID**; only source bytes the user themselves authored are ever
built; the build's **resource caps** ([ADR-0053](0053-in-cluster-build-from-source.md) Consequences)
keep it from starving workloads; and the privileged container is **transient** — one Job per build,
TTL-reaped (issue #280). The isolation that matters for the OSS single-tenant model is *namespace and
lifetime*, not the build container's own capability set.

## Consequences

- **The in-cluster Dockerfile build actually runs on managed Kubernetes.** Validated end to end on a
  live DOKS cluster: clone → buildah build → push to the in-cluster registry → guarded deploy, with
  the app reaching ready — the whole point of [ADR-0053](0053-in-cluster-build-from-source.md).
- **The build container is privileged — more than ADR-0056 intended, and far more than an app pod.**
  On a cluster enforcing Pod Security Admission, the `burrow-builds` namespace needs the `privileged`
  level (a label the install applies to that namespace only). This is a documented install
  consequence, scoped to the build namespace, never the app or control-plane namespace.
- **The build adapter also configures buildah's storage explicitly.** Independent of the security
  context, the adapter points buildah at a private `storage.conf` (vfs graphroot/runroot under `$HOME`,
  created `0700`), a private `XDG_RUNTIME_DIR`, and a writable `TMPDIR` — otherwise buildah falls back
  to `/var/tmp/storage-run-$UID`, which it refuses as group-writable under the pod's `fsGroup`. Pinned
  in the adapter's comments against this ADR.
- **The multi-tenant boundary is unaffected.** Untrusted-source isolation was never carried by this
  PodSecurity context (§3); moving to privileged changes nothing for the commercial product.
- **No change to the client-build-first default.** This touches only the optional in-cluster build
  path; the default remains build-in-CI-or-locally and deploy by image reference
  ([ADR-0007](0007-explicit-deploy-by-image-reference.md)).

## Rejected alternatives

- **Keep ADR-0056's narrow set (Unconfined + `SETUID`/`SETGID`, read-only root).** Rejected: validated
  not to complete a build on a managed CRI — buildah's layer remount of the locked container root is
  denied. It clears only the first wall (`unshare`), not the second (`remount /`). Shipping it would be
  ADR-0056's own "honesty about a non-working feature is worse than a bounded relaxation," in reverse.
- **Add `CAP_SYS_ADMIN` instead of going privileged.** Rejected: validated insufficient. A namespaced
  `CAP_SYS_ADMIN` cannot change propagation on a mount the parent namespace locked; the remount is
  refused with it present. It grants breadth without unlocking the one operation the build needs.
- **Writable root filesystem alone (no privilege).** Rejected: validated insufficient; the remount is
  denied on a writable root too, because the block is the mount **lock**, not the read-only flag.
- **Kubernetes user namespaces (`hostUsers: false`) so the pod gets a real userns from the kubelet.**
  Not adopted now: it is the principled long-term path to an unprivileged in-cluster build, but it is
  a newer feature with uneven availability across managed providers (and unproven on the reference DOKS
  target at time of writing). Left as a future refinement that could narrow §1 back down if and when it
  is dependable everywhere Burrow runs; until then, privileged is the option that actually works on the
  reference target.
- **Tighten this PodSecurity context to serve the multi-tenant case.** Rejected as a category error,
  exactly as in [ADR-0056](0056-build-security-context-for-the-oss-builder.md) §3: the untrusted-source
  boundary belongs to the commercial product's gVisor/microVM executor behind the
  [ADR-0053](0053-in-cluster-build-from-source.md) §6 `Builder` seam (cloud ADR-0003), not to this
  floor.
