#!/usr/bin/env bash
# Create a disposable k3d cluster, install the CloudNativePG operator into it, export
# BURROW_TEST_KUBECONFIG pointing at it, run the given command, then delete the cluster.
#
# The operator is part of the harness rather than of one test because the Postgres add-on IS a
# CloudNativePG `Cluster` (ADR-0066 §1) — there is no second mechanism, so a cluster without the
# operator cannot run the Postgres integration tests at all and they skip on it.
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

# The pin the code applies (controlplane/kube/placement_cnpg.go CNPGVersion), read from the source so
# the harness and the product cannot install different releases.
CNPG_VERSION=$(sed -n 's/^const CNPGVersion = "\(.*\)"$/\1/p' "$(dirname "$0")/../controlplane/kube/placement_cnpg.go")
if [ -n "$CNPG_VERSION" ] && command -v kubectl >/dev/null 2>&1; then
  kubectl --kubeconfig "$KCFG" apply --server-side -f \
    "https://github.com/cloudnative-pg/cloudnative-pg/releases/download/v${CNPG_VERSION}/cnpg-${CNPG_VERSION}.yaml" >/dev/null
  kubectl --kubeconfig "$KCFG" -n cnpg-system rollout status deploy/cnpg-controller-manager --timeout=180s >/dev/null
fi

"$@"
