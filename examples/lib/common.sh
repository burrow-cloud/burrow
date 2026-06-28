#!/usr/bin/env bash
# Shared helpers for the Burrow examples. Each example is a self-contained "operate a real
# cluster with your agent" scenario: a setup script plants a broken state, you point your
# agent CLI at Burrow and let it diagnose and fix, and a verify script grades the result.
#
# This file is SOURCED by the per-example setup.sh / verify.sh — it defines variables and
# functions, it does not run anything on its own. The heavy lifting (stand up a disposable
# k3d cluster, build the binaries, install Burrow) lives here so each example only has to
# express its own scenario.
#
# State is passed from setup.sh to verify.sh through a small env file ($RUN_ENV) so the two
# separate invocations — with your interactive agent session in between — share the same
# cluster, kubeconfig, and binaries.

# Resolve paths from THIS file's location so they are correct regardless of the caller's cwd.
_COMMON_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
EX_ROOT=$(cd "$_COMMON_DIR/.." && pwd)   # the examples/ directory
REPO_ROOT=$(cd "$EX_ROOT/.." && pwd)     # the repo root
EX_BIN="$EX_ROOT/.bin"                   # built CLI + MCP binaries (gitignored)
RUN_ENV="$EX_ROOT/.run-env"              # setup -> verify handoff (gitignored)

# One disposable cluster is shared across every example so you can run several without
# rebuilding. Override the name (or bring your own) with these env vars.
CLUSTER="${BURROW_EXAMPLES_CLUSTER:-burrow-examples}"

# Populated by ex_ensure_cluster; also restored by ex_load_env in verify.sh.
BURROW=""
BURROW_MCP=""
KCFG=""

# ex_require_bins fails early, listing everything missing, before any slow work. Note this is
# the tooling the SETUP needs — not your agent: setup and verify never spend API tokens, only
# your own interactive agent session (your `claude`/agent CLI, on your own plan) does.
ex_require_bins() {
  local missing=
  local bin
  for bin in k3d kubectl ko docker go jq; do
    command -v "$bin" >/dev/null 2>&1 || missing="$missing $bin"
  done
  if [ -n "$missing" ]; then
    echo "FAIL: missing prerequisites:$missing" >&2
    echo "Install them and re-run. ko is 'brew install ko' (https://ko.build)." >&2
    exit 1
  fi
}

# ex_build_binaries compiles the burrow CLI and the burrow-mcp server into $EX_BIN. The MCP
# binary is what your agent launches (via the generated .mcp.json) to reach the control plane.
ex_build_binaries() {
  mkdir -p "$EX_BIN"
  BURROW="$EX_BIN/burrow"
  BURROW_MCP="$EX_BIN/burrow-mcp"
  echo "=== build the burrow CLI and burrow-mcp ==="
  ( cd "$REPO_ROOT" && go build -o "$BURROW" ./cmd/burrow && go build -o "$BURROW_MCP" ./cmd/burrow-mcp )
}

# ex_ensure_cluster makes a Burrow-serving k3d cluster exist, reusing it if it already does so
# repeated examples are fast. It sets $KCFG to a stable kubeconfig path (persists across the
# setup -> agent -> verify gap, unlike a mktemp file).
ex_ensure_cluster() {
  if k3d cluster list "$CLUSTER" >/dev/null 2>&1; then
    echo "=== reusing existing k3d cluster '$CLUSTER' ==="
  else
    echo "=== create k3d cluster '$CLUSTER' ==="
    k3d cluster create "$CLUSTER"
  fi
  # `k3d kubeconfig write` returns a fixed on-disk path (not a temp file), so the same
  # kubeconfig is valid for the agent session and the later verify run.
  KCFG=$(k3d kubeconfig write "$CLUSTER")

  # Install Burrow only if it is not already serving on this cluster (idempotent reuse).
  if "$BURROW" app list --kubeconfig "$KCFG" >/dev/null 2>&1; then
    echo "=== Burrow already installed on '$CLUSTER' — reusing ==="
    return
  fi
  echo "=== build + import the burrowd image (ko) ==="
  # ko builds the Go binary on the host and loads it into the local docker daemon; capture the
  # exact image ref it prints and use it for the import and the install (capstone pattern).
  local image
  image=$(cd "$REPO_ROOT" && KO_DOCKER_REPO=ko.local ko build ./cmd/burrowd)
  k3d image import "$image" -c "$CLUSTER"
  echo "=== burrow install (waits for the control plane to be ready) ==="
  "$BURROW" install --burrowd-image "$image" --kubeconfig "$KCFG"
}

# ex_save_env records the run state so verify.sh (a separate invocation) can find the same
# cluster, kubeconfig, and binaries.
ex_save_env() {
  cat > "$RUN_ENV" <<EOF
CLUSTER="$CLUSTER"
KCFG="$KCFG"
BURROW="$BURROW"
BURROW_MCP="$BURROW_MCP"
EOF
}

# ex_load_env restores what ex_save_env wrote; verify.sh calls this instead of rebuilding.
ex_load_env() {
  if [ ! -f "$RUN_ENV" ]; then
    echo "FAIL: $RUN_ENV not found — run this example's setup.sh first." >&2
    exit 1
  fi
  # shellcheck disable=SC1090
  . "$RUN_ENV"
}

# ex_write_mcp_config writes a .mcp.json into the given workspace dir so an agent launched
# there auto-discovers the Burrow MCP server. The server key MUST be "burrow" so the tool ids
# are mcp__burrow__<tool>; burrow-mcp reaches the in-cluster control plane via BURROW_KUBECONFIG
# over the API-server proxy (no port-forward). Absolute paths so it works from any cwd.
ex_write_mcp_config() {
  local ws="$1"
  jq -n \
    --arg cmd "$BURROW_MCP" \
    --arg kcfg "$KCFG" \
    '{mcpServers:{burrow:{command:$cmd,args:[],env:{BURROW_KUBECONFIG:$kcfg}}}}' \
    > "$ws/.mcp.json"
}

# ex_wait_available polls `burrow app status <app>` until the workload reports available, or
# fails after ~timeout seconds. formatStatus prints "...replicas ready, available" only when
# the workload is actually available, so that exact substring is the readiness signal.
ex_wait_available() {
  local app="$1" tries="${2:-30}"
  local i
  for i in $(seq 1 "$tries"); do
    if "$BURROW" app status "$app" --kubeconfig "$KCFG" 2>/dev/null | grep -q "ready, available"; then
      return 0
    fi
    sleep 4
  done
  return 1
}

# ex_teardown deletes the shared cluster and the run state. Examples never call this for you —
# you decide when you are done. Run `bash examples/lib/teardown.sh` (or k3d cluster delete).
ex_teardown() {
  echo "=== deleting cluster '$CLUSTER' ==="
  k3d cluster delete "$CLUSTER" >/dev/null 2>&1 || true
  rm -f "$RUN_ENV"
}
