#!/usr/bin/env bash
# Build and push the burrowd control-plane image for a development cluster, stamped with a
# development version so the upgrade gate (ADR-0013) and the client/server skew gate (ADR-0039)
# accept a from-source build instead of reading v0.0.0 as a downgrade.
#
# burrowd is NOT stamped via ldflags — .ko.yaml deliberately carries none — so, exactly like the
# release workflow, this sed-edits `var version` in cmd/burrowd/main.go before `ko build`. The
# edit is reverted on exit, on success OR failure, so the working tree is never left dirty.
#
# HEAVY (Docker): requires ko and a registry the cluster can pull from.
#
#   KO_DOCKER_REPO=ghcr.io/you/burrowd scripts/dev-image.sh v0.13.0-dev
#
# KO_DOCKER_REPO is read from the environment (ko's own convention). KO_PLATFORM is optional
# (e.g. linux/amd64, linux/arm64); when unset ko builds for its default platform.
set -euo pipefail

DEV_VERSION="${1:?usage: dev-image.sh <dev-version> (e.g. v0.13.0-dev)}"
: "${KO_DOCKER_REPO:?set KO_DOCKER_REPO to a registry the cluster can pull from (e.g. ghcr.io/you/burrowd)}"

main_go="cmd/burrowd/main.go"

# Revert the version stamp on any exit so the tree is never left dirty.
restore() { git checkout -- "$main_go"; }
trap restore EXIT

# Portable in-place edit (works with both BSD and GNU sed); the .bak is discarded.
sed -i.bak "s/^var version = .*/var version = \"${DEV_VERSION}\"/" "$main_go"
rm -f "${main_go}.bak"

platform_arg=()
if [[ -n "${KO_PLATFORM:-}" ]]; then
  platform_arg=(--platform="${KO_PLATFORM}")
fi

ko build --bare "${platform_arg[@]}" --tags "${DEV_VERSION}" ./cmd/burrowd
