#!/usr/bin/env bash
# Create a disposable k3d cluster, export BURROW_TEST_KUBECONFIG pointing at it, run the
# given command, then delete the cluster.
#
# HEAVY: this starts Docker containers (a k3s cluster). Requires k3d and a running
# Docker. Run it deliberately, not as part of the routine `task check` gate.
#
# Usage: scripts/with-k3d.sh go test ./controlplane/kube/ -run TestIntegration -v
set -euo pipefail

command -v k3d >/dev/null 2>&1 || { echo "k3d not found — install k3d" >&2; exit 1; }

CLUSTER="${K3D_CLUSTER:-burrow-test}"

cleanup() { k3d cluster delete "$CLUSTER" >/dev/null 2>&1 || true; }
trap cleanup EXIT

k3d cluster create "$CLUSTER" --wait --timeout 120s >/dev/null 2>&1
KCFG=$(k3d kubeconfig write "$CLUSTER")
export BURROW_TEST_KUBECONFIG="$KCFG"
"$@"
