#!/usr/bin/env bash
# Quickstart end-to-end test: pins the "fast path" of docs/QUICKSTART.md so the doc cannot rot.
# It walks exactly the sequence a stranger follows on their laptop — stand up k3d, install
# Burrow, build and import the examples/hello image, deploy it, confirm it is running, then try
# to delete it and assert the guardrail HOLDS the delete for confirmation instead of executing
# it (the payoff of the whole walkthrough).
#
# Unlike the doc — which installs the PUBLISHED burrowd image so a stranger needs no source build
# — this test builds burrowd FROM THE TREE with ko and imports it, so it exercises the PR's code,
# not a release. The hello image is built locally in both the doc and here (docker is a prereq),
# which is the real "deploy your own code" path.
#
# Requires k3d, docker, kubectl, go, and ko (https://ko.build — `brew install ko`). It skips
# cleanly (exit 0) when docker or k3d is unavailable, like the other heavy integration tests, so
# it is safe to invoke from a light task on a machine without a Docker cluster.
#
# Cluster handling: if $K3D_CLUSTER names an already-running cluster, it is reused and left in
# place (the caller owns it — this is how CI folds the test into its existing k3d job). Otherwise
# a disposable cluster is created and torn down on exit.
set -euo pipefail

# --- skip cleanly when the heavy prerequisites are absent -------------------------------------
for bin in k3d docker kubectl go ko; do
  if ! command -v "$bin" >/dev/null 2>&1; then
    echo "SKIP: '$bin' not found — the quickstart e2e needs k3d, docker, kubectl, go, and ko." >&2
    exit 0
  fi
done

# On macOS + Docker Desktop the daemon socket is not at the default /var/run/docker.sock that ko
# and k3d probe; point them at the active docker context's socket so a maintainer can run this
# locally. Best-effort and only when DOCKER_HOST is unset — CI (Linux) uses the default socket.
if [ -z "${DOCKER_HOST:-}" ]; then
  sock=$(docker context inspect --format '{{.Endpoints.docker.Host}}' 2>/dev/null || true)
  if [ -n "$sock" ] && [ "$sock" != "unix:///var/run/docker.sock" ]; then
    export DOCKER_HOST="$sock"
  fi
fi

if ! docker info >/dev/null 2>&1; then
  echo "SKIP: the Docker daemon is not reachable — the quickstart e2e needs a running Docker." >&2
  exit 0
fi

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$REPO_ROOT"

# --- cluster: reuse a named one, or create a disposable one and delete it on exit -------------
CLUSTER="${K3D_CLUSTER:-burrow-quickstart}"
OWN_CLUSTER=0
if k3d cluster list "$CLUSTER" >/dev/null 2>&1; then
  echo "=== reusing existing k3d cluster '$CLUSTER' ==="
else
  echo "=== create disposable k3d cluster '$CLUSTER' ==="
  k3d cluster create "$CLUSTER" --wait --timeout 180s
  OWN_CLUSTER=1
fi
cleanup() {
  if [ "$OWN_CLUSTER" = 1 ]; then
    echo "=== delete disposable k3d cluster '$CLUSTER' ==="
    k3d cluster delete "$CLUSTER" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

KCFG=$(k3d kubeconfig write "$CLUSTER")
CTX=$(kubectl --kubeconfig "$KCFG" config current-context)

# Block until the k3d API server actually serves a real request (not just /readyz) — the written
# kubeconfig can point at the load-balancer port before it is forwarding, which EOFs the first
# call. Same guard the kube-integration CI job uses.
ready=
for _ in $(seq 1 45); do
  if kubectl --kubeconfig "$KCFG" get namespaces >/dev/null 2>&1; then ready=1; break; fi
  sleep 2
done
[ -n "$ready" ] || { echo "FAIL: k3d API server never served a real request" >&2; exit 1; }

# An isolated HOME keeps the test off the developer's real ~/.burrow (a pinned environment there
# would otherwise hijack the CLI's target) and ~/.claude — install records a fresh environment
# here and the CLI reads it back, exactly as a stranger's clean machine would. Capture the Go
# build and module caches from the real HOME FIRST and export them, so relocating HOME does not
# force a cold recompile/redownload (which would make this crawl in CI).
export GOCACHE="${GOCACHE:-$(go env GOCACHE)}"
export GOMODCACHE="${GOMODCACHE:-$(go env GOMODCACHE)}"
WORK=$(mktemp -d)
export HOME="$WORK/home"
mkdir -p "$HOME"
trap 'cleanup; chmod -R u+w "$WORK" 2>/dev/null || true; rm -rf "$WORK" 2>/dev/null || true' EXIT

BURROW="$WORK/burrow"
echo "=== build the burrow CLI (from the tree) ==="
go build -o "$BURROW" ./cmd/burrow

echo "=== build + import the burrowd image (ko, from the tree) ==="
# ko compiles burrowd on the host and loads it into the local docker daemon; capture the exact
# ref it prints and import that into the cluster and install it — so this tests the PR's burrowd,
# not a published release.
BURROWD_IMAGE=$(KO_DOCKER_REPO=ko.local ko build ./cmd/burrowd)
k3d image import "$BURROWD_IMAGE" -c "$CLUSTER"

echo "=== build + import the examples/hello image (the app the stranger deploys) ==="
docker build -t hello:1 examples/hello
k3d image import hello:1 -c "$CLUSTER"

echo "=== burrow install (waits for the control plane to be ready) ==="
"$BURROW" install "$CTX" --burrowd-image "$BURROWD_IMAGE" --kubeconfig "$KCFG"

echo "=== burrow app deploy hello --image hello:1 ==="
"$BURROW" app deploy hello --image hello:1 --kubeconfig "$KCFG"

echo "=== wait for hello to become available ==="
ok=
for _ in $(seq 1 45); do
  if "$BURROW" app status hello --kubeconfig "$KCFG" 2>/dev/null | grep -q "ready, available"; then
    ok=1
    break
  fi
  sleep 4
done
echo "--- status ---"
"$BURROW" app status hello --kubeconfig "$KCFG"
[ -n "$ok" ] || { echo "FAIL: hello never became available"; exit 1; }

# =============================================================================
# THE PAYOFF: app delete is HELD for confirmation, not executed.
# `burrow app delete` without --confirm trips the app.delete guardrail (confirm-by-default). The
# CLI surfaces the hold as a non-zero exit and a message naming app.delete and --confirm; the app
# must still be there afterward. This is the exact "Burrow refuses a destructive op without an
# explicit confirm" moment the doc ends on — assert it holds, not deletes.
# =============================================================================
echo "=== burrow app delete hello (NO --confirm) must be HELD, not executed ==="
set +e
delete_out=$("$BURROW" app delete hello --kubeconfig "$KCFG" 2>&1)
delete_rc=$?
set -e
printf '%s\n' "$delete_out"
if [ "$delete_rc" -eq 0 ]; then
  echo "FAIL: 'burrow app delete hello' succeeded without --confirm — the guardrail did not hold it"
  exit 1
fi
grep -qi "confirm" <<<"$delete_out" \
  || { echo "FAIL: the held delete did not mention confirmation"; exit 1; }
grep -q "app.delete" <<<"$delete_out" \
  || { echo "FAIL: the held delete did not name the app.delete guardrail"; exit 1; }

echo "=== assert hello still exists (the delete was held, not performed) ==="
"$BURROW" app status hello --kubeconfig "$KCFG" | grep -q "ready, available" \
  || { echo "FAIL: hello is gone — the delete executed instead of being held"; exit 1; }

echo "=== QUICKSTART E2E PASSED: install -> build+import hello -> deploy -> status running -> delete HELD for confirmation ==="
