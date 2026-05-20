#!/bin/bash
set -euo pipefail

# db-migration.sh — move a shingo-edge SQLite DB into the FHS layout
# (/var/lib/shingo-edge/shingoedge.db).
#
# Usage:  db-migration.sh <path-to-old-shingoedge.db>
#
# Copies (not moves) the old DB so the original stays as a rollback
# safety net. Verifies WAL checkpoint, SQLite integrity, and row
# counts on both sides before declaring success.

if [ $# -lt 1 ] || [ -z "$1" ]; then
    echo "Usage: $0 <path-to-old-shingoedge.db>" >&2
    exit 1
fi

OLD_DB="$1"
NEW_DIR=/var/lib/shingo-edge
NEW_DB="$NEW_DIR/shingoedge.db"
COUNTS_FILE=/tmp/db-migration-counts.txt
NEW_COUNTS_FILE=/tmp/db-migration-counts-new.txt

if [ ! -f "$OLD_DB" ]; then
    echo "ERROR: old DB not found at $OLD_DB" >&2
    exit 1
fi

echo "=== Step 1: Confirm edge is stopped ==="
# Match the compiled binary by exact process name, OR `go run` invocations
# whose first non-flag argument resolves to .../cmd/shingoedge. Pattern is
# deliberately narrow so this script's own cmdline (which contains the .db
# path) doesn't false-match.
if pgrep -x shingoedge > /dev/null 2>&1 \
   || pgrep -f 'go run [^ ]*cmd/shingoedge' > /dev/null 2>&1; then
    echo "ERROR: edge process still running. Stop it before running this script."
    pgrep -xa shingoedge || true
    pgrep -fa 'go run [^ ]*cmd/shingoedge' || true
    exit 1
fi

echo "=== Step 2: WAL checkpoint old DB ==="
sqlite3 "$OLD_DB" "PRAGMA wal_checkpoint(TRUNCATE);" || { echo "checkpoint failed"; exit 1; }

echo "=== Step 3: Integrity check old DB ==="
INTEGRITY=$(sqlite3 "$OLD_DB" "PRAGMA integrity_check;")
if [ "$INTEGRITY" != "ok" ]; then
    echo "ERROR: old DB integrity check failed: $INTEGRITY"
    exit 1
fi
echo "old DB integrity: $INTEGRITY"

echo "=== Step 4: Snapshot row counts of old DB ==="
sqlite3 "$OLD_DB" "
SELECT 'orders', count(*) FROM orders
UNION ALL SELECT 'outbox', count(*) FROM outbox
UNION ALL SELECT 'bins', count(*) FROM bins
UNION ALL SELECT 'process_nodes', count(*) FROM process_nodes
UNION ALL SELECT 'payload_catalog', count(*) FROM payload_catalog;
" > "$COUNTS_FILE"
echo "row counts snapshot saved to $COUNTS_FILE:"
cat "$COUNTS_FILE"

echo "=== Step 5: Create destination directory ==="
sudo mkdir -p "$NEW_DIR"
sudo chown shingo:shingo "$NEW_DIR"
sudo chmod 755 "$NEW_DIR"

echo "=== Step 6: Copy DB files ==="
cp -v "$OLD_DB" "$NEW_DB"
[ -f "${OLD_DB}-wal" ] && cp -v "${OLD_DB}-wal" "${NEW_DB}-wal"
[ -f "${OLD_DB}-shm" ] && cp -v "${OLD_DB}-shm" "${NEW_DB}-shm"

echo "=== Step 7: Integrity check new DB ==="
NEW_INTEGRITY=$(sqlite3 "$NEW_DB" "PRAGMA integrity_check;")
if [ "$NEW_INTEGRITY" != "ok" ]; then
    echo "ERROR: new DB integrity check failed: $NEW_INTEGRITY"
    exit 1
fi
echo "new DB integrity: $NEW_INTEGRITY"

echo "=== Step 8: Verify row counts match ==="
sqlite3 "$NEW_DB" "
SELECT 'orders', count(*) FROM orders
UNION ALL SELECT 'outbox', count(*) FROM outbox
UNION ALL SELECT 'bins', count(*) FROM bins
UNION ALL SELECT 'process_nodes', count(*) FROM process_nodes
UNION ALL SELECT 'payload_catalog', count(*) FROM payload_catalog;
" > "$NEW_COUNTS_FILE"
if diff -q "$COUNTS_FILE" "$NEW_COUNTS_FILE" > /dev/null; then
    echo "Row counts match. Migration verified."
else
    echo "ERROR: row counts differ between old and new DB."
    echo "--- old ---"
    cat "$COUNTS_FILE"
    echo "--- new ---"
    cat "$NEW_COUNTS_FILE"
    exit 1
fi

echo "=== DB MIGRATION COMPLETE ==="
echo "Old DB still in place at $OLD_DB (rollback safety net)"
echo "New DB at $NEW_DB ready to use"
