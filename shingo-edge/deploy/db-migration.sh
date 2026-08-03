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
# Overridable so the refusal in Step 4b can actually be exercised. Nothing in
# this repo could test this script before — the destination was a hardcoded
# absolute path, so proving the guard fires meant having a real /var/lib on a
# real box. Production callers set nothing and get the same path as always.
NEW_DIR="${SHINGO_EDGE_DB_DIR:-/var/lib/shingo-edge}"
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
#
# THE EXACT DIFF IN STEP 8 IS SAFE AGAINST RETENTION, AND MUST STAY EXACT.
# The worry is obvious once retention exists anywhere on the Edge: Step 4
# counts, Step 8 counts again, and anything that deletes rows in between
# fails a migration on a healthy plant. It cannot happen, because Step 1
# refuses to run at all while a shingoedge process is alive — and
# install-edge.sh stops shingo-edge.service (waiting up to 45s, aborting if
# it will not die) and kills any stray before it calls this script. Every
# purge on the Edge is a ticker inside that process. Both snapshots
# therefore read a database nothing is writing to, and Step 8's snapshot
# reads a `cp` of the very file Step 4 read.
#
# So this is a copy-verification gate, not a liveness gate, and loosening
# it — excluding "high-churn" tables, tolerating small differences — would
# give up the only check that the copy in Step 6 was complete, in exchange
# for immunity to a race the process-stopped precondition already excludes.
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

echo "=== Step 4b: Refuse to overwrite a populated destination ==="
# THE LAST LINE OF DEFENCE, and on 2026-07-29 there was none. install-edge.sh
# routed to MIGRATION with a live FHS database present, this script copied a
# stale 2026-05-20 DB over it, and Step 8 below reported "Migration verified".
#
# Step 8 cannot catch this. It snapshots the destination AFTER the copy and
# diffs it against the source snapshot — but the destination IS a byte copy of
# the source by then, so the counts always match. It verifies that cp worked,
# which was never in doubt, and reads as if it verified that no data was lost.
#
# This check runs BEFORE the copy and compares what is actually at risk: rows
# that exist in the destination and would stop existing.
if [ -f "$NEW_DB" ]; then
    DEST_COUNTS_FILE=$(mktemp)
    snapshot_counts "$NEW_DB" > "$DEST_COUNTS_FILE" 2>/dev/null || true
    dest_total=$(awk -F'|' '{s+=$2} END {print s+0}' "$DEST_COUNTS_FILE")
    src_total=$(awk -F'|' '{s+=$2} END {print s+0}' "$COUNTS_FILE")
    echo "destination already exists: $NEW_DB ($dest_total rows across $(wc -l < "$DEST_COUNTS_FILE") tables)"
    echo "source:                     $OLD_DB ($src_total rows)"
    if [ "$dest_total" -gt 0 ]; then
        echo ""
        echo "ERROR: the destination database already holds data. Refusing to copy over it."
        echo ""
        echo "  destination: $NEW_DB   $dest_total rows   $(du -h "$NEW_DB" 2>/dev/null | cut -f1)"
        echo "  source:      $OLD_DB   $src_total rows   $(du -h "$OLD_DB" 2>/dev/null | cut -f1)"
        echo ""
        if [ "$dest_total" -gt "$src_total" ]; then
            echo "  The destination has MORE rows than the source. The source is almost"
            echo "  certainly the stale copy — this is the 2026-07-29 Springfield shape."
        fi
        echo ""
        echo "  Nothing has been modified. Back BOTH files up off-box before doing"
        echo "  anything else, then decide which one is live."
        rm -f "$DEST_COUNTS_FILE"
        exit 1
    fi
    rm -f "$DEST_COUNTS_FILE"
    echo "destination exists but is empty — safe to populate"
fi

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

echo "=== Step 8: Verify the copy is faithful to its source ==="
# WHAT THIS DOES AND DOES NOT PROVE. It compares the destination against the
# source it was just copied from, so it detects a truncated or failed cp and
# nothing else. It CANNOT tell you the right database was chosen — the
# destination is a byte copy by this point, so the counts match by construction.
#
# It used to print "Migration verified", which is the sentence that appeared on
# 2026-07-29 as a live 37.7 MB database was replaced by a 5.9 MB stale one.
# The question it sounds like it answers is answered by Step 4b, before the copy.
snapshot_counts "$NEW_DB" > "$NEW_COUNTS_FILE"
if diff -q "$COUNTS_FILE" "$NEW_COUNTS_FILE" > /dev/null; then
    echo "Copy is faithful: $(wc -l < "$COUNTS_FILE") tables, row counts identical to the source."
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
