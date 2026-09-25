package store

import (
	"fmt"

	"shingoedge/store/schema"
)

// migrations_lineside_piles.go — one pile identity, (node, payload, state).
//
// A lineside pile used to carry the style it was captured under and the pair
// it sat on, and Core keyed its mirror on that style while the Edge merged
// across it, which is how the two drifted apart (FINDINGS-lineside-bucket-
// drift-2026-09-24 §2). A pile is now (node, payload) in one of two states:
// active until the node's cutover, stranded after it. Core's copy is fed by
// levels, so the delta bookkeeping for piles goes with the columns.
//
// NO ROLLBACK to the previous build after this runs: its columns are gone.

// rebuildLinesidePiles moves node_lineside_bucket to the (node, payload,
// state) shape: style_id and pair_key go, `inactive` becomes `stranded`, and
// the inactive rows of one (node, payload), which the old shape kept one per
// style, fold into one stranded row by summing qty (latest updated_at kept).
// An active row is already unique per (node, payload) under the old partial
// index and comes across as it is, id included, so the Clear endpoint's row
// id still names it. A group that sums to nothing is dropped: a level of 0 is
// the row being gone.
//
// SQLite cannot drop an indexed column, hence the rebuild; rebuildTable holds
// the legacy_alter_table rule. Runs only while style_id is still there.
func (db *DB) rebuildLinesidePiles() error {
	has, err := schema.TableHasColumn(db.DB, "node_lineside_bucket", "style_id")
	if err != nil {
		return fmt.Errorf("lineside piles: read columns: %w", err)
	}
	if !has {
		return nil
	}
	// The payload column's two older names, renamed later in migrate() for
	// the untouched table. Only a database that old still has them; the error
	// on every other one is the no-op.
	db.Exec("ALTER TABLE node_lineside_bucket RENAME COLUMN part_number TO payload_code")
	db.Exec("ALTER TABLE node_lineside_bucket RENAME COLUMN cat_id TO payload_code")
	return db.rebuildTable("node_lineside_bucket", `
ALTER TABLE node_lineside_bucket RENAME TO node_lineside_bucket_legacy;
CREATE TABLE node_lineside_bucket (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    node_id      INTEGER NOT NULL REFERENCES process_nodes(id) ON DELETE CASCADE,
    payload_code TEXT NOT NULL,
    qty          INTEGER NOT NULL DEFAULT 0,
    state        TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'stranded')),
    created_at   TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at   TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE (node_id, payload_code, state)
);
INSERT INTO node_lineside_bucket (id, node_id, payload_code, qty, state, created_at, updated_at)
SELECT MAX(id), node_id, payload_code, SUM(qty),
       CASE WHEN state = 'active' THEN 'active' ELSE 'stranded' END AS pile_state,
       MIN(created_at), MAX(updated_at)
FROM node_lineside_bucket_legacy
GROUP BY node_id, payload_code, pile_state
HAVING SUM(qty) > 0;
DROP TABLE node_lineside_bucket_legacy;
`)
}

// purgeLinesideBucketDeltas removes what the delta-fed pile mirror left behind:
// the bucket-scope seq rows (a pile level allocates under 'bucket_level' and
// carries no net) and every outbox row of the retired delta subject that never
// went out, unsent or dead-lettered. A new Core has no handler for that
// subject, and the boot resend of every pile's level supersedes what it
// carried. The subject is spelled as a literal because its constant is gone.
//
// Idempotent and cheap at every open: nothing writes either any more.
func (db *DB) purgeLinesideBucketDeltas() error {
	if _, err := db.Exec(`DELETE FROM inventory_delta_seq WHERE scope_kind = 'bucket'`); err != nil {
		return fmt.Errorf("lineside piles: purge bucket seq rows: %w", err)
	}
	if _, err := db.Exec(`DELETE FROM outbox
		WHERE msg_type = 'inventory.lineside_bucket_delta' AND sent_at IS NULL`); err != nil {
		return fmt.Errorf("lineside piles: purge bucket delta outbox rows: %w", err)
	}
	return nil
}
