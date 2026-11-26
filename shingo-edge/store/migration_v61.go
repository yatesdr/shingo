package store

import (
	"database/sql"

	"shingo/protocol/migrate"
)

// Migration 61: orders.cancel_intent, the cancel this station asked Core for
// and Core has not ended (domain.CancelIntent, JSON; '' = none). A cancel of an
// order whose status is Core's word writes it with its order.cancel, and Core's
// word ends the row and clears it. Every existing row gets '': no cancel asked.
//
// It was numbered v60 on the branch that wrote it, in the chain this tree
// retired; above main's v60 it is v61. Guarded by the column, so a file that
// branch's build migrated, which records 60 and already has the column, gets
// main's v60 re-run by v60's Verify and v61 recorded with nothing added. Both
// starts are pinned by TestV61_AFileAtMainsV60ReachesV61WithItsRows
// (internal/schemadump). There is no down step.

// v61HasColumn counts orders.cancel_intent: 1 once v61 has run.
const v61HasColumn = `SELECT COUNT(*) FROM pragma_table_info('orders') WHERE name = 'cancel_intent'`

func v61Up(tx *sql.Tx) error {
	var n int
	if err := tx.QueryRow(v61HasColumn).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := tx.Exec(`ALTER TABLE orders ADD COLUMN cancel_intent TEXT NOT NULL DEFAULT ''`)
	return err
}

// v61Verify holds while orders has its cancel_intent column.
func v61Verify(q migrate.Querier) bool {
	var n int
	return q.QueryRow(v61HasColumn).Scan(&n) == nil && n == 1
}
