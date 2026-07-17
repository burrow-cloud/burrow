# syntax=docker/dockerfile:1

# burrow-builder is the image the in-cluster build Job's *build* container runs
# (controlplane/kube/build.go; ADR-0053 §4). The clone step runs in a separate alpine/git
# init container, so the build container's job is to turn a cloned source tree into a pushed
# image. This first image bundles the Dockerfile build path: rootless buildah. The
# no-Dockerfile Cloud Native Buildpacks path (the buildScript's `/cnb/lifecycle/creator`
# branch) is a documented follow-up — see the NOTE below — so a no-Dockerfile build fails
# fast with a missing-lifecycle error rather than silently doing the wrong thing.
#
# It is built to run under the relaxed-but-bounded security context ADR-0056 pins for a
# rootless container builder: a non-root UID (1000), a read-only root filesystem (every
# write path is a writable emptyDir the Job mounts: /workspace, $HOME, /tmp), Unconfined
# seccomp plus the SETUID/SETGID capabilities and allowPrivilegeEscalation the Pod supplies,
# and vfs storage so no host mounts or privileges are needed. ADR-0056 is Proposed and calls
# for pinning the exact rootless set on a real cluster; this image follows its recommended
# configuration.

# quay.io/buildah/stable is the upstream rootless-buildah image: it already carries buildah,
# a vfs-capable storage.conf, and the /etc/subuid + /etc/subgid ranges rootless buildah's
# newuidmap/newgidmap helpers need. Pinned to a digest-free stable tag for readability; the
# publishing workflow tags the built image, so reproducibility comes from that tag, not this
# base ref.
FROM quay.io/buildah/stable:v1.43.1

# git on PATH for the build container. The clone itself runs in the alpine/git init container
# (ADR-0053 §4), but a user Dockerfile may fetch dependencies over git during `buildah bud`,
# so git belongs in the builder too.
RUN dnf install -y git \
 && dnf clean all \
 && rm -rf /var/cache/dnf

# Key the subordinate uid/gid ranges to the exact username of UID 1000 (the buildUID/buildGID
# the Job runs as, controlplane/kube/build.go), so rootless buildah's newuidmap/newgidmap
# resolve regardless of how the base image named that user.
RUN u="$(id -un 1000)" \
 && printf '%s:100000:65536\n' "$u" > /etc/subuid \
 && printf '%s:100000:65536\n' "$u" > /etc/subgid

# vfs needs no host mounts or privileges; chroot isolation is the rootless, daemonless build
# mode. The build Job sets the same values in the container env (build.go), so these are the
# defaults for anyone running the image directly, not a divergent configuration.
ENV STORAGE_DRIVER=vfs \
    BUILDAH_ISOLATION=chroot

# Run as the non-root build user by default; the Pod also pins RunAsUser 1000 (ADR-0056 §4).
USER 1000

# NOTE (follow-up, ADR-0053 §4): the no-Dockerfile Cloud Native Buildpacks path invokes
# `/cnb/lifecycle/creator` (build.go buildScript). Shipping the CNB lifecycle, buildpacks,
# and a run image is a larger, separate piece of work; until it lands, this image supports
# the Dockerfile (buildah) path only and a no-Dockerfile build reports a clear failure.
