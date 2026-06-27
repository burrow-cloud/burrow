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
  echo "--- burrow-addons namespace (logs add-on lives here) ---"
  kubectl -n burrow-addons get all || true
  echo "--- burrow-logs store logs ---"
  kubectl -n burrow-addons logs deploy/burrow-logs --tail=40 || true
  echo "--- fluent-bit collector logs ---"
  kubectl -n burrow-addons logs ds/burrow-logs-collector --tail=40 || true
}
trap dump_diagnostics ERR

WORK=$(mktemp -d)
BURROW="$WORK/burrow"
echo "=== build the burrow CLI ==="
go build -o "$BURROW" ./cmd/burrow

echo "=== build + import the burrowd image (ko) ==="
# ko builds the Go binary on the host (reusing the build cache warm from the test runs above,
# instead of recompiling client-go inside a docker build) and loads it into the local docker
# daemon. KO_DOCKER_REPO=ko.local routes the load to the daemon; capture the exact image ref
# ko prints to stdout and use it for the import and the install.
BURROWD_IMAGE=$(KO_DOCKER_REPO=ko.local ko build ./cmd/burrowd)
k3d image import "$BURROWD_IMAGE" -c "$CLUSTER"

echo "=== burrow install (waits for the control plane to be ready) ==="
"$BURROW" install --burrowd-image "$BURROWD_IMAGE" --kubeconfig "$KCFG"

echo "=== burrow app deploy (auto-connect: kubeconfig + API-server proxy, no port-forward) ==="
"$BURROW" app deploy web --image nginx:alpine --kubeconfig "$KCFG"

echo "=== wait for the app to become available ==="
ok=
for _ in $(seq 1 45); do
  if "$BURROW" app status web --kubeconfig "$KCFG" | grep -q "ready, available"; then
    ok=1
    break
  fi
  sleep 4
done

echo "--- final status ---"
"$BURROW" app status web --kubeconfig "$KCFG"
if [ -z "$ok" ]; then
  echo "FAIL: app never became available"
  exit 1 # the ERR trap dumps diagnostics
fi

echo "=== rollback path: deploy a second image, then roll back ==="
"$BURROW" app deploy web --image nginx:1.27-alpine --kubeconfig "$KCFG"
"$BURROW" app rollback web --kubeconfig "$KCFG" | grep -q "nginx:alpine" || { echo "FAIL: rollback did not restore nginx:alpine"; exit 1; }

# =============================================================================
# ADDON: logs pipeline
# Exercise the REAL production logs path end-to-end, reusing the already-installed
# control plane, $BURROW, and $KCFG above (no re-install):
#   burrow CLI -> control-plane API -> in-cluster burrowd -> Fluent Bit collector
#   -> VictoriaLogs store -> query back via `burrow addon logs`.
# The query MUST go through the in-cluster burrowd (the test host cannot resolve
# in-cluster Service DNS), so this is the only faithful way to verify the round trip.
# =============================================================================

echo "=== addon install logs (VictoriaLogs store + Fluent Bit collector) ==="
# --confirm bypasses the addon_install guardrail (confirm-by-default) so the run is
# non-interactive; the flag maps to the engine's confirm bool.
"$BURROW" addon install logs --confirm --kubeconfig "$KCFG"

echo "=== wait for the logs store to become ready ==="
# Add-ons live in the burrow-addons namespace; the store Deployment is named burrow-logs.
# rollout status blocks until the store is available (it is the readiness signal `addon
# list` reports as Ready); on timeout it exits non-zero and the ERR trap dumps state.
kubectl --kubeconfig "$KCFG" -n burrow-addons rollout status deploy/burrow-logs --timeout=120s

echo "--- installed add-ons ---"
"$BURROW" addon list --kubeconfig "$KCFG"

echo "=== deploy a logger fixture that continuously emits a unique marker ==="
# `burrow app deploy` has no command/args flag, so the looped-echo fixture is applied
# directly with kubectl into the app namespace (default). It emits an error-shaped line
# carrying the BURROW_E2E_LOGLINE marker every 2s; Fluent Bit tails the node's container
# logs and ships them into VictoriaLogs.
kubectl --kubeconfig "$KCFG" apply -f - <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: burrow-e2e-logger
  namespace: default
spec:
  replicas: 1
  selector:
    matchLabels:
      app: burrow-e2e-logger
  template:
    metadata:
      labels:
        app: burrow-e2e-logger
    spec:
      containers:
        - name: logger
          image: busybox:1.36
          command:
            - sh
            - -c
            - 'i=0; while true; do echo "BURROW_E2E_LOGLINE level=error iteration=$i app failed to connect"; i=$((i+1)); sleep 2; done'
YAML
kubectl --kubeconfig "$KCFG" -n default rollout status deploy/burrow-e2e-logger --timeout=60s

echo "=== query the marker back through burrow addon logs (bounded poll) ==="
# Bounded poll (~90s) to cover Fluent Bit's tail+flush latency into VictoriaLogs. The
# query is the marker itself; assert the round-tripped output contains it.
found=
last_out=
for _ in $(seq 1 18); do
  last_out=$("$BURROW" addon logs 'BURROW_E2E_LOGLINE' --kubeconfig "$KCFG" 2>&1 || true)
  if printf '%s\n' "$last_out" | grep -q "BURROW_E2E_LOGLINE"; then
    found=1
    break
  fi
  sleep 5
done

if [ -z "$found" ]; then
  echo "FAIL: marker BURROW_E2E_LOGLINE never appeared via 'burrow addon logs'"
  echo "--- last query output ---"
  printf '%s\n' "$last_out"
  exit 1 # the ERR trap dumps diagnostics
fi
echo "--- marker round-tripped through the logs pipeline ---"
printf '%s\n' "$last_out" | grep "BURROW_E2E_LOGLINE" | head -n 3

echo "=== tidy up the logs add-on and the logger fixture (best-effort) ==="
# Cleanup is non-fatal — the cluster is deleted after the run regardless.
"$BURROW" addon remove burrow-logs --confirm --kubeconfig "$KCFG" || true
kubectl --kubeconfig "$KCFG" -n default delete deploy/burrow-e2e-logger --ignore-not-found || true

echo "=== CAPSTONE E2E PASSED: install -> deploy -> status -> rollback -> logs pipeline, all via the CLI over the proxy ==="
