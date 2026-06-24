#!/usr/bin/env bash
# Boot an ephemeral local Postgres, export BURROW_TEST_DATABASE_URL pointing at it, run
# the given command, then tear everything down. Lets the Postgres integration tests run
# locally without a standing database.
#
# Usage: scripts/with-test-postgres.sh go test ./controlplane/postgres/... -v
set -euo pipefail

find_pgbin() {
  for d in /usr/local/opt/postgresql@18/bin /opt/homebrew/opt/postgresql@18/bin \
           /usr/local/opt/postgresql/bin /opt/homebrew/opt/postgresql/bin; do
    if [ -x "$d/initdb" ]; then echo "$d"; return 0; fi
  done
  if command -v initdb >/dev/null 2>&1; then dirname "$(command -v initdb)"; return 0; fi
  return 1
}

PGBIN=$(find_pgbin) || { echo "no local Postgres (initdb) found — install postgresql" >&2; exit 1; }

WORK=$(mktemp -d)
DATADIR="$WORK/data"
PORT="${PGPORT:-55432}"

cleanup() {
  "$PGBIN/pg_ctl" -D "$DATADIR" stop -m fast >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

"$PGBIN/initdb" -D "$DATADIR" -U postgres --auth-host=trust >/dev/null 2>&1
# A long temp path can exceed the 103-byte Unix-socket limit, so listen on TCP only.
"$PGBIN/pg_ctl" -D "$DATADIR" -l "$WORK/pg.log" \
  -o "-p $PORT -c listen_addresses=127.0.0.1 -c unix_socket_directories=''" -w start >/dev/null
"$PGBIN/createdb" -h 127.0.0.1 -p "$PORT" -U postgres burrow_test

export BURROW_TEST_DATABASE_URL="postgres://postgres@127.0.0.1:$PORT/burrow_test?sslmode=disable"
"$@"
