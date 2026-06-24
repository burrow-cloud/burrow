#!/usr/bin/env bash
# Capstone end-to-end test: build the burrowd image, install Burrow into a k3d cluster,
# and deploy an app through the CLI — auto-connecting via the developer's kubeconfig and
# the Kubernetes API-server proxy (no port-forward). This exercises the entire stack the
# way a user would.
#
# Expects a running k3d cluster named $K3D_CLUSTER, plus kubectl, docker, and go.
set -euo pipefail

CLUSTER="${K3D_CLUSTER:-burrow-ci}"
KCFG=$(k3d kubeconfig write "$CLUSTER")
export KUBECONFIG="$KCFG"

WORK=$(mktemp -d)
BURROW="$WORK/burrow"
echo "=== build the burrow CLI ==="
go build -o "$BURROW" ./cmd/burrow

echo "=== build + import the burrowd image ==="
docker build -t burrowd:e2e .
k3d image import burrowd:e2e -c "$CLUSTER"

echo "=== burrow install (waits for the control plane to be ready) ==="
"$BURROW" install --burrowd-image burrowd:e2e --kubeconfig "$KCFG"

echo "=== burrow deploy (auto-connect: kubeconfig + API-server proxy, no port-forward) ==="
"$BURROW" deploy web --image nginx:alpine --kubeconfig "$KCFG"

echo "=== wait for the app to become available ==="
ok=
for _ in $(seq 1 45); do
  if "$BURROW" status web --kubeconfig "$KCFG" | grep -q "ready, available"; then
    ok=1
    break
  fi
  sleep 4
done

echo "--- final status ---"
"$BURROW" status web --kubeconfig "$KCFG"
if [ -z "$ok" ]; then
  echo "FAIL: app never became available"
  kubectl get pods -A
  exit 1
fi

echo "=== rollback path: deploy a second image, then roll back ==="
"$BURROW" deploy web --image nginx:1.27-alpine --kubeconfig "$KCFG"
"$BURROW" rollback web --kubeconfig "$KCFG" | grep -q "nginx:alpine" || { echo "FAIL: rollback did not restore nginx:alpine"; exit 1; }

echo "=== CAPSTONE E2E PASSED: install -> deploy -> status -> rollback, all via the CLI over the proxy ==="
