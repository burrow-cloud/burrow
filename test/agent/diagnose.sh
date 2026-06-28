#!/usr/bin/env bash
# Local, manually-run agent diagnosis test: prove a headless Claude agent can find the
# root cause of a failing app from Burrow's aggregated logs, end to end. The flow stands up
# a k3d cluster, installs Burrow, installs the logs add-on (VictoriaLogs store + Fluent Bit
# collector), deploys a deliberately-failing app that logs an unambiguous root cause,
# confirms the failure line is queryable back through Burrow, then runs `claude -p` pointed
# at the burrow MCP server and asserts the agent BOTH queries the logs AND names the root
# cause.
#
# This COSTS ANTHROPIC API TOKENS, so it is run by hand, never in CI. See README.md.
#
# Requires: claude (the Claude Code CLI) + ANTHROPIC_API_KEY, k3d, kubectl, ko
# (https://ko.build — `brew install ko`), docker, jq, go. Mirrors the proven setup in
# test/e2e/install-and-deploy.sh (the capstone): k3d cluster, ko-built burrowd image,
# `burrow install` over the API-server proxy.
#
# Run:   ANTHROPIC_API_KEY=... bash test/agent/diagnose.sh
# Debug: KEEP=1 ANTHROPIC_API_KEY=... bash test/agent/diagnose.sh   (keeps the cluster)
set -euo pipefail

# --- 1. prereq checks (fail early, list everything missing) -------------------------------
missing=
for bin in claude k3d kubectl ko docker jq go; do
  if ! command -v "$bin" >/dev/null 2>&1; then
    missing="$missing $bin"
  fi
done
if [ -z "${ANTHROPIC_API_KEY:-}" ]; then
  missing="$missing ANTHROPIC_API_KEY(env)"
fi
if [ -n "$missing" ]; then
  echo "FAIL: missing prerequisites:$missing" >&2
  echo "Install the tools and export ANTHROPIC_API_KEY, then re-run. See test/agent/README.md." >&2
  exit 1
fi

# Resolve the repo root from this script's location so absolute paths in the MCP config are
# correct regardless of the caller's working directory.
SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "$SCRIPT_DIR/../.." && pwd)
cd "$REPO_ROOT"

CLUSTER="${K3D_CLUSTER:-burrow-agent-diag}"
WORK=$(mktemp -d)
KCFG="$WORK/kubeconfig"
AGENT_OUT="$WORK/agent-out.jsonl"
MCP_CFG="$WORK/mcp-config.json"

# --- 2. cluster + cleanup trap ------------------------------------------------------------
# Dump cluster state on any failure (the install/poll waits otherwise exit before any
# diagnostics), mirroring the capstone's dump_diagnostics.
dump_diagnostics() {
  echo "=== DIAGNOSTICS: a step failed, dumping cluster state ==="
  kubectl --kubeconfig "$KCFG" get pods -A -o wide || true
  echo "--- burrow-apps namespace events ---"
  kubectl --kubeconfig "$KCFG" -n burrow-apps get events --sort-by=.lastTimestamp | tail -n 30 || true
  echo "--- checkout app pods + logs ---"
  kubectl --kubeconfig "$KCFG" -n burrow-apps get pods -l app.kubernetes.io/name=checkout -o wide || true
  kubectl --kubeconfig "$KCFG" -n burrow-apps logs -l app.kubernetes.io/name=checkout --tail=40 || true
  echo "--- burrowd describe + logs ---"
  kubectl --kubeconfig "$KCFG" -n burrow describe deploy/burrowd || true
  kubectl --kubeconfig "$KCFG" -n burrow logs deploy/burrowd --tail=80 || true
  echo "--- burrow-addons namespace (logs add-on lives here) ---"
  kubectl --kubeconfig "$KCFG" -n burrow-addons get all || true
  kubectl --kubeconfig "$KCFG" -n burrow-addons logs deploy/burrow-logs --tail=40 || true
  kubectl --kubeconfig "$KCFG" -n burrow-addons logs ds/burrow-logs-collector --tail=40 || true
}

cleanup() {
  status=$?
  if [ "$status" -ne 0 ]; then
    dump_diagnostics || true
  fi
  if [ "${KEEP:-0}" = "1" ]; then
    echo "KEEP=1 set — leaving cluster '$CLUSTER' and work dir '$WORK' in place for debugging."
    echo "  kubeconfig: $KCFG"
    echo "  agent output: $AGENT_OUT"
  else
    echo "=== cleanup: deleting cluster '$CLUSTER' ==="
    k3d cluster delete "$CLUSTER" >/dev/null 2>&1 || true
    rm -rf "$WORK" || true
  fi
  exit "$status"
}
trap cleanup EXIT

echo "=== create k3d cluster '$CLUSTER' ==="
# Delete any stale cluster of the same name first so a re-run starts clean.
k3d cluster delete "$CLUSTER" >/dev/null 2>&1 || true
k3d cluster create "$CLUSTER"
k3d kubeconfig get "$CLUSTER" > "$KCFG"

echo "=== build the burrow CLI and the burrow-mcp binary ==="
BURROW="$REPO_ROOT/bin/burrow"
BURROW_MCP="$REPO_ROOT/bin/burrow-mcp"
go build -o "$BURROW" ./cmd/burrow
go build -o "$BURROW_MCP" ./cmd/burrow-mcp

echo "=== build + import the burrowd image (ko) ==="
# ko builds the Go binary on the host and loads it into the local docker daemon; capture the
# exact image ref ko prints and use it for the import and the install (capstone pattern).
BURROWD_IMAGE=$(KO_DOCKER_REPO=ko.local ko build ./cmd/burrowd)
k3d image import "$BURROWD_IMAGE" -c "$CLUSTER"

# --- 3. install Burrow + wait for readiness -----------------------------------------------
echo "=== burrow install (waits for the control plane to be ready) ==="
"$BURROW" install --burrowd-image "$BURROWD_IMAGE" --kubeconfig "$KCFG"

# --- 4. install the logs add-on -----------------------------------------------------------
echo "=== addon install logs (VictoriaLogs store + Fluent Bit collector) ==="
# --confirm bypasses the addon_install guardrail (confirm-by-default) so the run is
# non-interactive.
"$BURROW" addon install logs --confirm --kubeconfig "$KCFG"

echo "=== wait for the logs store to become ready ==="
kubectl --kubeconfig "$KCFG" -n burrow-addons rollout status deploy/burrow-logs --timeout=120s

# --- 5. deploy the deliberately-failing app -----------------------------------------------
echo "=== deploy a deliberately-failing 'checkout' app THROUGH Burrow (CrashLoopBackOff, logs root cause) ==="
# Deploy via `burrow app deploy` with a -- command override, so checkout is a real
# Burrow-managed workload the agent discovers via burrow_apps/burrow_status (not just logs).
# busybox echoes an unambiguous root cause then exits non-zero, so the pod CrashLoopBackOffs
# and keeps re-logging the line — a steady stream for Fluent Bit to ship. The deploy returns
# once the release is recorded; it does not wait for readiness (the app never becomes ready,
# by design). Root-cause keywords asserted later: "database" AND ("connection refused" OR "5432").
"$BURROW" app deploy checkout --image busybox:1.36 --kubeconfig "$KCFG" -- \
  sh -c 'echo "FATAL: checkout cannot connect to database at db.default.svc.cluster.local:5432: connection refused"; sleep 2; exit 1'

# --- 6. wait until the failure line is queryable through Burrow ---------------------------
echo "=== wait until the FATAL line is queryable via 'burrow addon logs' (bounded ~90s) ==="
# Confirm the collector shipped the line into VictoriaLogs BEFORE asking the agent —
# otherwise the agent test would be meaningless (no logs to find). Bounded poll (~90s) to
# cover Fluent Bit's tail+flush latency plus the CrashLoop backoff between re-logs.
found=
last_out=
for _ in $(seq 1 18); do
  last_out=$("$BURROW" addon logs 'FATAL' --kubeconfig "$KCFG" 2>&1 || true)
  if printf '%s\n' "$last_out" | grep -q "FATAL: checkout cannot connect to database"; then
    found=1
    break
  fi
  sleep 5
done
if [ -z "$found" ]; then
  echo "FAIL: the FATAL root-cause line never appeared via 'burrow addon logs' — the agent test"
  echo "would be meaningless without queryable logs."
  echo "--- last query output ---"
  printf '%s\n' "$last_out"
  exit 1 # the EXIT trap dumps diagnostics
fi
echo "--- root-cause line round-tripped through the logs pipeline ---"
printf '%s\n' "$last_out" | grep "FATAL: checkout cannot connect to database" | head -n 3

# --- 7. write the MCP config for claude ---------------------------------------------------
echo "=== write the MCP config (server key 'burrow' -> tool ids mcp__burrow__<tool>) ==="
# burrow-mcp auto-connects to the in-cluster control plane through the API-server proxy using
# BURROW_KUBECONFIG (the same kubeconfig the CLI uses). The server key MUST be "burrow" so
# tool ids are mcp__burrow__<toolName>. Absolute paths so claude can launch it from any cwd.
jq -n \
  --arg cmd "$BURROW_MCP" \
  --arg kcfg "$KCFG" \
  '{mcpServers:{burrow:{command:$cmd,args:[],env:{BURROW_KUBECONFIG:$kcfg}}}}' \
  > "$MCP_CFG"

# --- 8. run the headless agent ------------------------------------------------------------
echo "=== run the headless Claude agent against the burrow MCP server ==="
# Flags confirmed via `claude --help`:
#   -p/--print                 non-interactive: print response and exit
#   --mcp-config <file>        load MCP servers from a JSON file
#   --allowedTools <tools...>  space/comma-separated allowlist; whitelisting exactly the three
#                              read-only burrow tools means the agent never hits an interactive
#                              permission prompt (so the run cannot hang on approval).
#   --output-format stream-json + --verbose  newline-delimited JSON event stream (required
#                              together for the streamed events the jq assertions parse).
# timeout bounds a hang so it fails instead of blocking forever.
PROMPT="The 'checkout' app in my cluster is failing. Use the Burrow tools to inspect it and its logs, then tell me the single root cause."

set +e
timeout 180 claude -p "$PROMPT" \
  --mcp-config "$MCP_CFG" \
  --allowedTools "mcp__burrow__burrow_apps" "mcp__burrow__burrow_status" "mcp__burrow__burrow_logs_query" \
  --output-format stream-json \
  --verbose \
  > "$AGENT_OUT" 2>&1
agent_rc=$?
set -e
if [ "$agent_rc" -ne 0 ]; then
  echo "FAIL: claude exited non-zero ($agent_rc) — output tail:"
  tail -n 40 "$AGENT_OUT"
  exit 1 # the EXIT trap dumps diagnostics
fi

# --- 9. assertions against the stream-json output -----------------------------------------
# stream-json is newline-delimited JSON events. The shapes relied on (confirmed via
# `claude --help`: --output-format stream-json + --verbose; standard Claude Code stream
# schema):
#   - assistant turn:  {"type":"assistant","message":{"content":[ {"type":"tool_use",
#                       "name":"mcp__burrow__burrow_logs_query", ...}, {"type":"text",
#                       "text":"..."} ]}, ...}
#   - terminal result: {"type":"result","subtype":"success","result":"<final answer text>", ...}
# `jq -s` slurps the whole stream into an array; the recursive descent `..|.name?` finds every
# nested tool_use name regardless of exact wrapping.

echo "=== assert (a): the agent USED the logs-query tool ==="
if jq -s -r '.[]|.. | .name? // empty' "$AGENT_OUT" 2>/dev/null | grep -q 'burrow_logs_query'; then
  echo "PASS: agent issued a mcp__burrow__burrow_logs_query tool call."
else
  echo "FAIL: no burrow_logs_query tool_use found in the agent stream."
  echo "--- tool names seen in the stream ---"
  jq -s -r '.[]|.. | .name? // empty' "$AGENT_OUT" 2>/dev/null | sort -u || true
  echo "--- agent output tail ---"
  tail -n 40 "$AGENT_OUT"
  exit 1 # the EXIT trap dumps diagnostics
fi

echo "=== extract the agent's final answer ==="
# Prefer the terminal result event's .result string; fall back to concatenating assistant
# text blocks if no result event is present.
ANSWER=$(jq -s -r '[.[]|select(.type=="result")|.result] | last // empty' "$AGENT_OUT" 2>/dev/null)
if [ -z "$ANSWER" ]; then
  ANSWER=$(jq -s -r '[.[]|select(.type=="assistant")|.message.content[]?|select(.type=="text")|.text] | join("\n")' "$AGENT_OUT" 2>/dev/null)
fi
echo "----------------------------------------------------------------------"
echo "AGENT FINAL ANSWER:"
printf '%s\n' "$ANSWER"
echo "----------------------------------------------------------------------"

echo "=== assert (b): the answer names the root cause ==="
# Case-insensitive: must mention "database" AND ("connection refused" OR "5432"). The exact
# wording is LLM-variable, so we check for the root-cause CONCEPTS, not a fixed string.
answer_lc=$(printf '%s' "$ANSWER" | tr '[:upper:]' '[:lower:]')
has_db=0
has_detail=0
case "$answer_lc" in *database*) has_db=1 ;; esac
case "$answer_lc" in *"connection refused"*) has_detail=1 ;; esac
case "$answer_lc" in *5432*) has_detail=1 ;; esac

if [ "$has_db" = "1" ] && [ "$has_detail" = "1" ]; then
  echo "PASS: answer names the root cause (database + connection refused/5432)."
else
  echo "FAIL: answer did not name the root cause."
  echo "  database mentioned: $has_db ; (connection refused OR 5432) mentioned: $has_detail"
  echo "--- agent output tail ---"
  tail -n 40 "$AGENT_OUT"
  exit 1 # the EXIT trap dumps diagnostics
fi

echo "=== AGENT DIAGNOSIS PASSED ==="
echo "The headless agent queried Burrow's aggregated logs and correctly named the root cause:"
printf '%s\n' "$ANSWER"
