#!/usr/bin/env bash
# Enforce the per-directory license boundary (ADR-0001, LICENSING.md): every Go file
# must carry an SPDX-License-Identifier header matching the license for its directory,
# above a Copyright line.
#
#   controlplane/ , operator/ , cmd/burrowd/   -> FSL-1.1-ALv2   (the product)
#   everything else (cmd/burrow, cmd/burrow-mcp, mcp/, internal/) -> Apache-2.0 (client surface)
#
# Run from the repo root. Exits non-zero on any violation.
set -euo pipefail

fail=0

expected_license() {
  case "$1" in
    controlplane/* | operator/* | cmd/burrowd/*) echo "FSL-1.1-ALv2" ;;
    *) echo "Apache-2.0" ;;
  esac
}

# All tracked-or-untracked .go files, excluding generated/vendored trees.
while IFS= read -r -d '' file; do
  rel="${file#./}"
  want="$(expected_license "$rel")"
  header="$(head -n 5 "$file")"

  if ! grep -q "// SPDX-License-Identifier: ${want}" <<<"$header"; then
    echo "MISSING/WRONG SPDX: ${rel} (expected ${want})"
    fail=1
  fi
  if ! grep -q "// Copyright .* Nicholas Phillips" <<<"$header"; then
    echo "MISSING Copyright line: ${rel}"
    fail=1
  fi
done < <(find . -name '*.go' -not -path './vendor/*' -print0)

if [[ "$fail" -ne 0 ]]; then
  echo "SPDX check failed." >&2
  exit 1
fi
echo "SPDX check passed."
