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
EX_BIN="$EX_ROOT/.bin"                   # the built burrow + burrow-agent binaries (gitignored)
RUN_ENV="$EX_ROOT/.run-env"              # setup -> verify handoff (gitignored)

# One disposable cluster is shared across every example so you can run several without
# rebuilding. Override the name (or bring your own) with these env vars.
CLUSTER="${BURROW_EXAMPLES_CLUSTER:-burrow-examples}"

# Populated by ex_ensure_cluster; also restored by ex_load_env in verify.sh.
BURROW=""
BURROW_AGENT=""
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

# ex_build_binaries compiles both CLIs into $EX_BIN: `burrow`, the human admin CLI the setup
# and verify scripts drive, and `burrow-agent`, the scoped control channel the agent itself runs
# (ADR-0049). They are separate binaries so the agent can be allowed one and denied the other.
ex_build_binaries() {
  mkdir -p "$EX_BIN"
  BURROW="$EX_BIN/burrow"
  BURROW_AGENT="$EX_BIN/burrow-agent"
  echo "=== build the burrow and burrow-agent CLIs ==="
  ( cd "$REPO_ROOT" && go build -o "$BURROW" ./cmd/burrow && go build -o "$BURROW_AGENT" ./cmd/burrow-agent )
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
BURROW_AGENT="$BURROW_AGENT"
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

# ex_wire_agent writes a project-local .claude/settings.json into the given workspace dir so an
# agent launched there is wired to burrow-agent the way `burrow agent claude install` wires a real
# user (ADR-0049 §4) — allow the scoped binary, deny the human admin CLI — without touching the
# reader's own ~/.claude. The env block puts $EX_BIN on PATH so plain `burrow-agent` resolves (the
# allow rule matches the command name, so the binary must be reachable by that name) and points
# KUBECONFIG at this example's cluster, which is how burrow-agent finds the control plane.
# Absolute paths so it works from any cwd.
ex_wire_agent() {
  local ws="$1"
  mkdir -p "$ws/.claude"
  jq -n \
    --arg path "$EX_BIN:$PATH" \
    --arg kcfg "$KCFG" \
    '{permissions:{allow:["Bash(burrow-agent *)","Bash(burrow-agent)"],deny:["Bash(burrow *)"]},
      env:{PATH:$path,KUBECONFIG:$kcfg}}' \
    > "$ws/.claude/settings.json"
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
