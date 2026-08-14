# syntax=docker/dockerfile:1

# burrow-builder is the image the in-cluster build Job's *build* container runs
# (controlplane/kube/build.go; ADR-0053 §4). The clone step runs in a separate alpine/git
# init container, so the build container's job is to turn a cloned source tree into a pushed
# image. It carries BOTH builders ADR-0053 §4 names, because the Job picks between them at
# runtime from the cloned tree and the control plane never inspects the source to decide:
# buildah when the tree has a Dockerfile, and the Cloud Native Buildpacks lifecycle when it
# does not.
#
# It runs under the security context ADR-0059 pins for the OSS builder: a non-root UID (1000),
# Unconfined seccomp, and privileged — because on a managed CRI the container root mount is
# locked and buildah's layer extraction cannot remount it private without that. Every write
# path is still a writable emptyDir the Job mounts (/workspace, $HOME, /tmp, /layers), and
# storage is vfs, so the image needs no host mount of its own.

# WHY A BUILDER IMAGE IS THE BASE, RATHER THAN A LIFECYCLE COPIED INTO A BUILDAH IMAGE.
# A buildpacks build runs the buildpacks *inside this image's own root filesystem* and exports
# what they produce onto the run image the builder declares — so the build rootfs and the run
# image are a matched pair, published and tested together. Copying /cnb out of a builder into an
# unrelated base does run (the lifecycle reads /cnb and never inspects the image it is running
# in), but it silently unpairs them: anything a buildpack compiles rather than downloads —
# node-gyp, a Python C extension, cgo — links against the build image's C library and then has
# to load on a run image built from a different distribution. That is a failure that appears at
# runtime, in someone else's application, long after the build went green. So the buildpacks
# side chooses the base, and buildah — a self-contained tool with no such pairing — is installed
# on top of it.
#
# heroku/builder:26 is that base. It is a published, maintained CNB builder for Ubuntu 26.04
# carrying the buildpacks for .NET, Go, Java, Node.js, PHP, Python, Ruby and Scala, it declares
# its own run image (docker.io/heroku/heroku:26), and it sets CNB_USER_ID/CNB_GROUP_ID to 1000 —
# already the UID the build Job runs as, and an /etc/passwd entry the lifecycle needs to exist
# for the UID it drops to. It is chosen over Paketo's current builders on both counts: size,
# where paketobuildpacks/ubuntu-resolute-builder is ~780 MB compressed against ~473 MB here and
# ubuntu-noble-builder is ~1.5 GB (this image is pulled per build), and UID, where Paketo's
# current builders run as 1001 and would need the Job's fixed UID changed to match. Paketo's
# small builders are not the cheaper answer either: they carry only the Go and Java buildpacks,
# so the interpreted languages this path exists to serve would not detect at all.
#
# The base tag is not digest-pinned, for readability; the publishing workflow tags the built
# image, so reproducibility comes from that tag rather than from this ref.
FROM heroku/builder:26

# The base image runs as its non-root CNB user; the package installs need root.
USER 0

# buildah for the Dockerfile path, and the helpers it shells out to, which the previous
# quay.io/buildah/stable base bundled and a distribution package does not pull in on its own:
#
#   - uidmap supplies the setuid newuidmap/newgidmap helpers buildah's rootless setup uses to
#     write its uid/gid maps.
#   - netavark and aardvark-dns are the network stack buildah configures for the container each
#     RUN step executes in. Without netavark on the helper path, buildah refuses before the
#     first instruction with "could not find netavark" — a build that never starts.
#   - slirp4netns gives the rootless case a network namespace it can set up without privileges.
#
# Ubuntu 26.04 carries buildah 1.42, the same series the previous base carried, so the working
# Dockerfile path is not moved onto an older builder by the change of base. git is already in the
# base image (the clone runs in the init container, but a user Dockerfile may fetch dependencies
# over git during `buildah bud`). The buildah package pulls in the containers-common
# configuration — /etc/containers/policy.json and registries.conf — buildah needs to push at all.
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      buildah \
      uidmap \
      netavark \
      aardvark-dns \
      slirp4netns \
 && rm -rf /var/lib/apt/lists/*

# Key the subordinate uid/gid ranges to the exact username of UID 1000 (the buildUID/buildGID
# the Job runs as, controlplane/kube/build.go), so rootless buildah's newuidmap/newgidmap
# resolve regardless of how the base image named that user.
RUN u="$(id -un 1000)" \
 && printf '%s:100000:65536\n' "$u" > /etc/subuid \
 && printf '%s:100000:65536\n' "$u" > /etc/subgid

# The lifecycle writes its layers under /layers and reads platform-supplied environment from
# /platform. Both are created here, owned by the build user, so the image works when it is run
# directly and not only under the Job — which mounts a writable emptyDir over /layers (build.go),
# where the layers actually land. /layers is the lifecycle's own default and is kept: the path
# is recorded into the produced image as CNB_LAYERS_DIR, so an unconventional one would follow
# the application around for the life of the image for no gain.
RUN mkdir -p /layers /platform/env \
 && chown -R 1000:1000 /layers /platform

# vfs needs no host mounts; chroot isolation is the rootless, daemonless build mode. The build
# Job sets the same two values in the container env (build.go), so these are the defaults for
# anyone running the image directly, not a divergent configuration.
#
# CNB_PLATFORM_API is REQUIRED by the lifecycle — without it `creator` refuses to start with
# "failed to get platform API version". It belongs here, beside the lifecycle it is negotiated
# against, rather than in the build recipe: an image carrying a different lifecycle carries the
# platform API version that lifecycle supports. 0.15 is the newest this base's lifecycle
# (0.21.x) accepts.
ENV STORAGE_DRIVER=vfs \
    BUILDAH_ISOLATION=chroot \
    CNB_PLATFORM_API=0.15

# Run as the non-root build user by default; the Pod also pins RunAsUser 1000 (ADR-0059).
USER 1000

# NOTE (documented limit, ADR-0054 §5): the buildpacks path still cannot push to a plain-HTTP
# registry, so a no-Dockerfile build whose target is the optional in-cluster registry is refused
# with an explanation rather than attempted (build.go buildScript). The Dockerfile path pushes
# there with --tls-verify=false and is unaffected.
