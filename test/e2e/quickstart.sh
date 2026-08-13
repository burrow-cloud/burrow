#!/usr/bin/env bash
# Quickstart end-to-end test: pins the "fast path" of docs/QUICKSTART.md so the doc cannot rot.
# It walks exactly the sequence a stranger follows on their laptop — stand up k3d, install
# Burrow, build and import the examples/hello image, deploy it, confirm it is running, then try
# to delete it and assert the guardrail REFUSES the delete instead of executing it (the payoff of
# the whole walkthrough), and that relaxing the guardrail is what makes the delete reachable.
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

echo "=== burrow cluster install (waits for the control plane to be ready) ==="
"$BURROW" cluster install "$CTX" --burrowd-image "$BURROWD_IMAGE" --kubeconfig "$KCFG"

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
# THE PAYOFF: app delete is REFUSED, not executed — and --confirm does not open it.
# app.delete is denied by default (ADR-0065 section 3): deleting an app destroys its release
# history, so there is nothing to roll back to and a confirmation prompt protects only an
# attentive reader. The CLI surfaces the refusal as a non-zero exit and a message naming
# app.delete; the app must still be there afterward, WITH or WITHOUT --confirm. This is the exact
# "Burrow refuses a destructive op" moment the doc ends on — assert it refuses, not deletes.
# =============================================================================
for flag in "" "--confirm"; do
  label=${flag:-"NO --confirm"}
  echo "=== burrow app delete hello ($label) must be REFUSED, not executed ==="
  set +e
  # shellcheck disable=SC2086 # $flag is deliberately word-split: empty means no flag at all.
  delete_out=$("$BURROW" app delete hello $flag --kubeconfig "$KCFG" 2>&1)
  delete_rc=$?
  set -e
  printf '%s\n' "$delete_out"
  if [ "$delete_rc" -eq 0 ]; then
    echo "FAIL: 'burrow app delete hello $flag' succeeded — the guardrail did not refuse it"
    exit 1
  fi
  grep -q "app.delete" <<<"$delete_out" \
    || { echo "FAIL: the refused delete did not name the app.delete guardrail"; exit 1; }
  grep -q "denied" <<<"$delete_out" \
    || { echo "FAIL: the refused delete did not report a denial"; exit 1; }
  # A tier-2 deny is a floor, not a fixed setting: the refusal must point at scoping it to one
  # environment, so an operator's first move is `guard set --env`, not a global relax.
  grep -q -- "guard set --env" <<<"$delete_out" \
    || { echo "FAIL: the refusal did not point at per-environment scoping"; exit 1; }

  echo "=== assert hello still exists (the delete was refused, not performed) ==="
  "$BURROW" app status hello --kubeconfig "$KCFG" | grep -q "ready, available" \
    || { echo "FAIL: hello is gone — the delete executed instead of being refused"; exit 1; }
done

# Changing the policy takes a credential of your own (ADR-0099): burrowd refuses a policy write
# from an agent credential, and from the install's shared token, which has no kind at all — reading
# "no kind" as a person would leave the policy writable on exactly the installs that have only an
# agent to hold. So the operator signs in first, which is one command and is what a person does on
# their own machine. Everything above this line runs on the shared token, unchanged: deploying,
# reading status, and being refused a delete are not policy writes.
echo "=== burrow auth login (a policy write takes the operator's own credential) ==="
"$BURROW" auth login --context "$CTX" --kubeconfig "$KCFG" --name quickstart \
  || { echo "FAIL: 'burrow auth login' did not succeed"; exit 1; }

# The deny is the operator's starting point, not a wall: relaxing the guardrail is what makes the
# verb reachable, and only this CLI can do it (`burrow-agent` has no `guard set`, and burrowd
# refuses one from an agent credential however it is sent). The doc describes this lever rather
# than pulling it — it leaves the app in place for the agent path — so this is the one step the
# test takes beyond the walkthrough, and it is the half of the guardrail story a deny-only
# assertion would leave unproven.
echo "=== burrow guard set app.delete confirm, then the delete goes through with --confirm ==="
"$BURROW" guard set app.delete confirm --kubeconfig "$KCFG" \
  || { echo "FAIL: 'burrow guard set app.delete confirm' did not succeed"; exit 1; }
"$BURROW" app delete hello --confirm --kubeconfig "$KCFG" \
  || { echo "FAIL: the delete was still refused after the guardrail was relaxed to confirm"; exit 1; }
if "$BURROW" app status hello --kubeconfig "$KCFG" 2>/dev/null | grep -q "ready, available"; then
  echo "FAIL: hello survived a confirmed delete after the guardrail was relaxed"
  exit 1
fi

echo "=== QUICKSTART E2E PASSED: install -> build+import hello -> deploy -> status running -> delete DENIED -> relaxed -> deleted ==="
