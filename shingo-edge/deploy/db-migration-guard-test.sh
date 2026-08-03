#!/usr/bin/env bash
# db-migration-guard-test.sh — prove the destination guard in db-migration.sh
# still refuses to overwrite a populated Edge database.
#
# WHY THIS EXISTS. On 2026-07-29 install-edge.sh routed to MIGRATION on a box
# that already had a live database, db-migration.sh copied a stale 2026-05-20
# copy over it, and the run printed "Row counts match across 23 tables.
# Migration verified." Springfield lost a 37.7 MB production database to a
# 5.9 MB one. The verification could not have caught it: it snapshots the
# destination AFTER the copy and diffs it against the source, and by then the
# destination is a byte copy of the source, so the counts match by construction.
#
# Nothing tested any of this. The destination was a hardcoded absolute path, so
# exercising it needed a real /var/lib on a real box. db-migration.sh now reads
# SHINGO_EDGE_DB_DIR when set, which is what makes this runnable.
#
# Requires Docker. Takes about 20 seconds. Run it after touching either script.
#
#   bash shingo-edge/deploy/db-migration-guard-test.sh
#
# Exit 0 = the guard held. Exit 1 = the guard is gone, and the output tells you
# how many rows a real plant would have lost.
set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
IMAGE=alpine:3.20

echo "=== db-migration destination guard ==="
echo "repo: $REPO_ROOT"

# The container script goes in on stdin so nothing has to survive a round of
# shell quoting. MSYS_NO_PATHCONV stops Git Bash rewriting the mount path.
out=$(MSYS_NO_PATHCONV=1 docker run --rm -i -v "$REPO_ROOT":/repo:ro "$IMAGE" sh -s <<'INNER'
set -e
apk add --no-cache sqlite >/dev/null 2>&1
addgroup -S shingo 2>/dev/null || true
adduser -S -G shingo shingo 2>/dev/null || true
mkdir -p /work/live /work/legacy

seed() {
  db=$1; n=$2
  sqlite3 "$db" "CREATE TABLE orders(id INTEGER); CREATE TABLE bins(id INTEGER); CREATE TABLE claims(id INTEGER);"
  for t in orders bins claims; do
    sqlite3 "$db" "WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x<$n) INSERT INTO $t SELECT x FROM c;"
  done
}
total() {
  sqlite3 "$1" "SELECT (SELECT count(*) FROM orders)+(SELECT count(*) FROM bins)+(SELECT count(*) FROM claims);"
}

# The live FHS database (Springfield: 37.7 MB) and the stale legacy copy (5.9 MB).
seed /work/live/shingoedge.db 400
seed /work/legacy/shingoedge.db 10

before=$(total /work/live/shingoedge.db)
set +e
SHINGO_EDGE_DB_DIR=/work/live sh /repo/shingo-edge/deploy/db-migration.sh /work/legacy/shingoedge.db >/dev/null 2>&1
rc=$?
set -e
after=$(total /work/live/shingoedge.db)
echo "BEFORE=$before AFTER=$after RC=$rc"
INNER
)

echo "$out"
before=$(printf '%s' "$out" | sed -n 's/.*BEFORE=\([0-9]*\).*/\1/p')
after=$(printf '%s' "$out"  | sed -n 's/.*AFTER=\([0-9]*\).*/\1/p')
rc=$(printf '%s' "$out"     | sed -n 's/.*RC=\([0-9]*\).*/\1/p')

if [ -z "$before" ] || [ -z "$after" ]; then
    echo "FAIL — the container produced no result. Is Docker running?"
    exit 1
fi

if [ "$before" = "$after" ] && [ "$rc" != "0" ]; then
    echo "PASS — live database untouched ($before rows) and the script refused (exit $rc)."
    exit 0
fi

echo ""
echo "FAIL — the guard is gone."
echo "  live database went from $before rows to $after rows; script exited $rc."
echo "  On a plant box that is the 2026-07-29 Springfield incident: a production"
echo "  database replaced by a stale copy, with 'Migration verified' on stdout."
exit 1
