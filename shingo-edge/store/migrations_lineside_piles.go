package store

import (
	"fmt"
	"log"

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
	if err := db.rebuildTable("node_lineside_bucket", `
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
`); err != nil {
		return err
	}
	return db.strandUnclaimedPiles()
}

// strandUnclaimedPiles strands, once, every active pile whose part the
// process's active style does not claim at that node. It runs only inside the
// rebuild above, so only on the open that retires the old shape.
//
// Under the cutover rule such a pile has already crossed a cutover: its part
// is not what the node runs now. The old Edge left it active, because it
// deactivated piles only at a release, and Core's old count hid it with a
// claims lookup. Left active under the new rule, it would count as on-hand
// until the process next cut over, suppressing that part's replenishment
// (Springfield, 2026-09-25: 419 of a part at a seat running another, which
// turned a below-threshold read into a hold). Stranding it here makes the
// re-seeded mirror count what the old rule counted.
//
// The test is the old Core rule's, so nothing else moves: strand only when the
// node's process has an active style that claims the node, and no such claim
// names the part, either as its payload or among its allowed payloads; a
// retired claim is not one the style runs, so it counts for neither. A pile
// at a node with no active-style claim stays active, as the old rule counted
// it. Each one is logged as a count anomaly, like a strand at a cutover.
func (db *DB) strandUnclaimedPiles() error {
	for _, col := range [][2]string{
		{"processes", "active_style_id"},
		{"style_node_claims", "payload_code"},
		{"style_node_claims", "allowed_payload_codes"},
		{"style_node_claims", "retired_at"},
	} {
		has, err := schema.TableHasColumn(db.DB, col[0], col[1])
		if err != nil {
			return fmt.Errorf("lineside piles: read %s columns: %w", col[0], err)
		}
		if !has {
			// A database from before claims carried payloads has no rule
			// to apply; its piles stay as they came across.
			return nil
		}
	}

	piles, err := db.unclaimedActivePiles()
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("lineside piles: strand unclaimed: begin: %w", err)
	}
	defer tx.Rollback()
	for _, p := range piles {
		if _, err := tx.Exec(`INSERT INTO node_lineside_bucket (node_id, payload_code, qty, state)
			VALUES (?, ?, ?, 'stranded')
			ON CONFLICT (node_id, payload_code, state)
			DO UPDATE SET qty = qty + excluded.qty, updated_at = datetime('now')`,
			p.nodeID, p.payloadCode, p.qty); err != nil {
			return fmt.Errorf("lineside piles: strand unclaimed pile %d: %w", p.id, err)
		}
		if _, err := tx.Exec(`DELETE FROM node_lineside_bucket WHERE id = ?`, p.id); err != nil {
			return fmt.Errorf("lineside piles: strand unclaimed pile %d: delete active: %w", p.id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("lineside piles: strand unclaimed: commit: %w", err)
	}
	for _, p := range piles {
		log.Printf("lineside: count anomaly at cutover (migration): node=%d core_node=%s payload=%s qty=%d stranded — "+
			"its part is not claimed by the process's active style at this node", p.nodeID, p.coreNode, p.payloadCode, p.qty)
	}
	return nil
}

type unclaimedPile struct {
	id, nodeID  int64
	coreNode    string
	payloadCode string
	qty         int
}

// unclaimedActivePiles reads the active piles strandUnclaimedPiles strands.
// allowed_payload_codes is a JSON array, or the empty string for none.
func (db *DB) unclaimedActivePiles() ([]unclaimedPile, error) {
	rows, err := db.Query(`SELECT b.id, b.node_id, COALESCE(pn.core_node_name, ''), b.payload_code, b.qty
		FROM node_lineside_bucket b
		JOIN process_nodes pn ON pn.id = b.node_id
		JOIN processes p ON p.id = pn.process_id
		WHERE b.state = 'active' AND p.active_style_id IS NOT NULL
		  AND EXISTS (SELECT 1 FROM style_node_claims sc
		      WHERE sc.style_id = p.active_style_id AND sc.core_node_name = pn.core_node_name
		        AND sc.retired_at IS NULL)
		  AND NOT EXISTS (SELECT 1 FROM style_node_claims sc
		      WHERE sc.style_id = p.active_style_id AND sc.core_node_name = pn.core_node_name
		        AND sc.retired_at IS NULL
		        AND (sc.payload_code = b.payload_code
		          OR EXISTS (SELECT 1 FROM json_each(CASE WHEN json_valid(sc.allowed_payload_codes)
		                        THEN sc.allowed_payload_codes ELSE '[]' END) j
		                     WHERE j.value = b.payload_code)))
		ORDER BY b.id`)
	if err != nil {
		return nil, fmt.Errorf("lineside piles: strand unclaimed: read: %w", err)
	}
	defer rows.Close()
	var out []unclaimedPile
	for rows.Next() {
		var p unclaimedPile
		if err := rows.Scan(&p.id, &p.nodeID, &p.coreNode, &p.payloadCode, &p.qty); err != nil {
			return nil, fmt.Errorf("lineside piles: strand unclaimed: scan: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
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
