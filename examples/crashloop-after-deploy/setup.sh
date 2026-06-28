#!/usr/bin/env bash
# Plant the scenario: deploy a healthy 'checkout', then ship a broken release over it so the
# app is now crash-looping in CrashLoopBackOff — exactly the state a user finds after a bad
# deploy. Stand up (or reuse) a disposable k3d cluster with Burrow installed, leave the app
# broken, and write the agent's MCP config into ./workspace. Then you take over: launch your
# agent in ./workspace and let it diagnose and fix it through Burrow.
#
# Setup spends NO API tokens — only your interactive agent session does. See ../README.md.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
. "$SCRIPT_DIR/../lib/common.sh"

ex_require_bins
ex_build_binaries
ex_ensure_cluster
ex_save_env

APP=checkout

echo "=== deploy a HEALTHY release of '$APP' (nginx:alpine) and wait for it to come up ==="
# The good release the app should be running. It must actually become available so the prior
# release in Burrow's history is genuinely healthy — that is what makes a rollback the fix.
"$BURROW" app deploy "$APP" --image nginx:alpine --kubeconfig "$KCFG"
if ! ex_wait_available "$APP" 45; then
  echo "FAIL: the healthy baseline release never became available — cannot plant the scenario." >&2
  "$BURROW" app status "$APP" --kubeconfig "$KCFG" || true
  exit 1
fi
echo "--- baseline is healthy ---"
"$BURROW" app status "$APP" --kubeconfig "$KCFG"

echo "=== ship a BROKEN release over it (this is the bug) ==="
# A new release that crash-loops on boot with an unambiguous root-cause line. busybox echoes
# the failure then exits non-zero, so the pod CrashLoopBackOffs and re-logs the line — visible
# through `burrow logs checkout`. This supersedes the healthy nginx release; Burrow still holds
# that prior release, so a rollback restores health.
"$BURROW" app deploy "$APP" --image busybox:1.36 --kubeconfig "$KCFG" -- \
  sh -c 'echo "FATAL: checkout: config migration v2 failed: unknown column \"region\" — aborting startup"; sleep 2; exit 1'

echo "=== write the agent's MCP config into ./workspace ==="
ex_write_mcp_config "$SCRIPT_DIR/workspace"

cat <<EOF

============================================================================
Scenario planted: '$APP' is crash-looping after a bad deploy.

Now play the operator. In a NEW terminal:

    cd $SCRIPT_DIR/workspace
    claude          # or your agent CLI — it auto-loads .mcp.json (the burrow MCP server)

Hand it the ticket in that directory (workspace/TICKET.md) and let it drive Burrow to
diagnose and fix the app. Do NOT read this directory's README.md — it is the answer key.

When the agent says it is done, grade it:

    bash $SCRIPT_DIR/verify.sh

Tear everything down when finished:

    bash $EX_ROOT/lib/teardown.sh
============================================================================
EOF
