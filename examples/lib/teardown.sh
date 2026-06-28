#!/usr/bin/env bash
# Delete the shared examples cluster and forget the run state. Run this when you are done
# with the examples; nothing tears the cluster down for you (so a half-finished diagnosis
# survives a closed terminal).
set -euo pipefail
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"
ex_teardown
echo "done."
