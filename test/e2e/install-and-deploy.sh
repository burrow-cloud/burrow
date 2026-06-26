#!/usr/bin/env bash
# Capstone end-to-end test: build the burrowd image, install Burrow into a k3d cluster,
# and deploy an app through the CLI — auto-connecting via the developer's kubeconfig and
# the Kubernetes API-server proxy (no port-forward). This exercises the entire stack the
# way a user would.
#
# Expects a running k3d cluster named $K3D_CLUSTER, plus kubectl, docker, go, and ko
# (https://ko.build — `brew install ko`), which builds the burrowd image.
set -euo pipefail

CLUSTER="${K3D_CLUSTER:-burrow-ci}"
KCFG=$(k3d kubeconfig write "$CLUSTER")
export KUBECONFIG="$KCFG"

# On any failure (including the install readiness wait, which otherwise exits before any
# diagnostics), dump cluster state so a flake is debuggable — pod status plus the control
# plane's describe and logs (including a crashed container's previous logs).
dump_diagnostics() {
  echo "=== DIAGNOSTICS: a step failed, dumping cluster state ==="
  kubectl get pods -A -o wide || true
  echo "--- burrow namespace events ---"
  kubectl -n burrow get events --sort-by=.lastTimestamp | tail -n 30 || true
  echo "--- postgres logs ---"
  kubectl -n burrow logs deploy/postgres --tail=40 || true
  echo "--- burrowd describe ---"
  kubectl -n burrow describe deploy/burrowd || true
  echo "--- burrowd logs (current) ---"
  kubectl -n burrow logs deploy/burrowd --tail=80 || true
  echo "--- burrowd logs (previous, if it restarted) ---"
  kubectl -n burrow logs deploy/burrowd --previous --tail=80 || true
}
trap dump_diagnostics ERR

WORK=$(mktemp -d)
BURROW="$WORK/burrow"
echo "=== build the burrow CLI ==="
go build -o "$BURROW" ./cmd/burrow

echo "=== build + import the burrowd image (ko) ==="
# ko builds the Go binary on the host and assembles a minimal image — no Dockerfile, and
# crucially it reuses the host Go build cache (already warm from the `go test` runs above),
# instead of recompiling client-go from scratch inside a `docker build`.
ko build --local --base-import-paths --tags e2e --push=false ./cmd/burrowd
BURROWD_IMAGE=ko.local/burrowd:e2e
k3d image import "$BURROWD_IMAGE" -c "$CLUSTER"

echo "=== burrow install (waits for the control plane to be ready) ==="
"$BURROW" install --burrowd-image "$BURROWD_IMAGE" --kubeconfig "$KCFG"

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
  exit 1 # the ERR trap dumps diagnostics
fi

echo "=== rollback path: deploy a second image, then roll back ==="
"$BURROW" deploy web --image nginx:1.27-alpine --kubeconfig "$KCFG"
"$BURROW" rollback web --kubeconfig "$KCFG" | grep -q "nginx:alpine" || { echo "FAIL: rollback did not restore nginx:alpine"; exit 1; }

echo "=== CAPSTONE E2E PASSED: install -> deploy -> status -> rollback, all via the CLI over the proxy ==="
