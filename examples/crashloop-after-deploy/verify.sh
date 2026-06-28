#!/usr/bin/env bash
# Grade the scenario: did the agent get 'checkout' healthy again? The pass condition is binary
# and outcome-based — the app is available — so any real fix counts (a rollback to the healthy
# release is the intended, cleanest one; see README.md). Run this AFTER your agent session.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
. "$SCRIPT_DIR/../lib/common.sh"
ex_load_env

APP=checkout

echo "=== checking whether '$APP' is healthy again (bounded poll) ==="
if ! ex_wait_available "$APP" 30; then
  echo
  echo "----------------------------------------------------------------------"
  echo "NOT FIXED: '$APP' is still not available."
  "$BURROW" app status "$APP" --kubeconfig "$KCFG" || true
  echo "--- recent app logs ---"
  "$BURROW" app logs "$APP" --tail 20 --kubeconfig "$KCFG" || true
  echo "----------------------------------------------------------------------"
  exit 1
fi

echo "--- final status ---"
status_out=$("$BURROW" app status "$APP" --kubeconfig "$KCFG")
printf '%s\n' "$status_out"

# Extra signal (not part of the pass gate): note whether the serving image is the known-good
# nginx:alpine, which is what a rollback restores. A different healthy image means the agent
# fixed it another valid way (e.g. redeployed a corrected release).
echo
if printf '%s\n' "$status_out" | grep -q "nginx:alpine"; then
  echo "Serving the known-good image (nginx:alpine) — consistent with a rollback to the prior release."
else
  echo "Healthy on a non-baseline image — the agent fixed it by redeploying rather than rolling back (also valid)."
fi

echo
echo "=== FIXED: the agent restored '$APP' to healthy. ==="
