package store

import (
	"database/sql"
	"fmt"
	"testing"

	"shingo/protocol/testutil"
)

// runBackfillOnce seeds routing sets from the live claims as every database at
// v59 got them: the statements of the retired chain's v25, kept as a test
// seed for the tests that read what they leave (TestRoutingNodes_*, and the
// retired-claim pin).
func runBackfillOnce(t *testing.T, db *DB) {
	t.Helper()
	tx, err := db.Begin()
	testutil.MustNoErr(t, err, "begin")
	defer tx.Rollback()
	testutil.MustNoErr(t, backfillRoutingSetsOnce(tx), "seed the routing sets")
	testutil.MustNoErr(t, tx.Commit(), "commit")
}

func backfillRoutingSetsOnce(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT p.id FROM processes p
		WHERE EXISTS (SELECT 1 FROM style_node_claims c JOIN styles s ON s.id = c.style_id
			WHERE s.process_id = p.id AND s.deleted_at IS NULL AND c.retired_at IS NULL)
		AND NOT EXISTS (SELECT 1 FROM process_routing_nodes r WHERE r.process_id = p.id)`)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	legs := []struct{ field, role string }{
		{"inbound_source", "source"},
		{"outbound_destination", "destination"},
		{"changeover_evac_destination", "destination"},
		{"inbound_staging", "staging"},
		{"outbound_staging", "staging"},
	}
	// The tool change's own clearance destination is read only where the column
	// still stands. v47 drops it, and the style_node_claims rebuild an aged
	// database crosses before this step no longer carries it, so on such a
	// database there is nothing of it to backfill.
	var evacDest int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('style_node_claims')
		WHERE name = 'changeover_evac_destination'`).Scan(&evacDest); err != nil {
		return err
	}
	for _, id := range ids {
		for _, l := range legs {
			if l.field == "changeover_evac_destination" && evacDest == 0 {
				continue
			}
			if _, err := tx.Exec(`INSERT OR IGNORE INTO process_routing_nodes
				(process_id, core_node_name, role, origin, enabled)
				SELECT DISTINCT s.process_id, TRIM(c.`+l.field+`), ?, ?, 1
				FROM style_node_claims c JOIN styles s ON s.id = c.style_id
				WHERE s.process_id = ? AND s.deleted_at IS NULL AND c.retired_at IS NULL AND TRIM(c.`+l.field+`) != ''`,
				l.role, "backfill", id); err != nil {
				return fmt.Errorf("routing set backfill: process %d %s: %w", id, l.field, err)
			}
		}
		if _, err := tx.Exec(`DELETE FROM process_routing_nodes WHERE process_id = ?1 AND core_node_name IN (
			SELECT core_node_name FROM process_nodes WHERE process_id = ?1 AND deleted_at IS NULL)`, id); err != nil {
			return fmt.Errorf("routing set backfill: process %d drop positions: %w", id, err)
		}
	}
	return nil
}
