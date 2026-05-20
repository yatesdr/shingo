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

# Count rows across every user table in the given DB. Output is sorted
# "<table>|<count>" lines, suitable for diff'ing between old and new.
snapshot_counts() {
    local db="$1"
    sqlite3 "$db" "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name;" \
    | while IFS= read -r tbl; do
        [ -z "$tbl" ] && continue
        local n
        n=$(sqlite3 "$db" "SELECT count(*) FROM \"$tbl\";")
        printf '%s|%s\n' "$tbl" "$n"
    done
}

echo "=== Step 4: Snapshot row counts of old DB ==="
snapshot_counts "$OLD_DB" > "$COUNTS_FILE"
if [ ! -s "$COUNTS_FILE" ]; then
    echo "ERROR: old DB has no user tables — refusing to migrate an empty DB"
    exit 1
fi
echo "row counts snapshot saved to $COUNTS_FILE:"
cat "$COUNTS_FILE"

echo "=== Step 5: Create destination directory ==="
sudo mkdir -p "$NEW_DIR"
sudo chown shingo:shingo "$NEW_DIR"
sudo chmod 755 "$NEW_DIR"

echo "=== Step 6: Copy DB files ==="
# Step 2 already ran wal_checkpoint(TRUNCATE), so the old -wal is empty and
# -shm is regenerable. Only the .db itself carries data; copying the WAL
# siblings would just create more root-owned files to chown.
sudo cp -v "$OLD_DB" "$NEW_DB"
sudo chown shingo:shingo "$NEW_DB"
sudo chmod 644 "$NEW_DB"

echo "=== Step 7: Integrity check new DB ==="
NEW_INTEGRITY=$(sqlite3 "$NEW_DB" "PRAGMA integrity_check;")
if [ "$NEW_INTEGRITY" != "ok" ]; then
    echo "ERROR: new DB integrity check failed: $NEW_INTEGRITY"
    exit 1
fi
echo "new DB integrity: $NEW_INTEGRITY"

echo "=== Step 8: Verify row counts match ==="
snapshot_counts "$NEW_DB" > "$NEW_COUNTS_FILE"
if diff -q "$COUNTS_FILE" "$NEW_COUNTS_FILE" > /dev/null; then
    echo "Row counts match across $(wc -l < "$COUNTS_FILE") tables. Migration verified."
else
    echo "ERROR: row counts differ between old and new DB."
    echo "--- old ---"
    cat "$COUNTS_FILE"
    echo "--- new ---"
    cat "$NEW_COUNTS_FILE"
    echo "--- diff ---"
    diff "$COUNTS_FILE" "$NEW_COUNTS_FILE" || true
    exit 1
fi

echo "=== DB MIGRATION COMPLETE ==="
echo "Old DB still in place at $OLD_DB (rollback safety net)"
echo "New DB at $NEW_DB ready to use"
