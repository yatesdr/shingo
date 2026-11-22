package store

import "log"

// What a start does to rows, once the schema is settled. It is this one step.

const (
	clearEndedActiveOrderSQL = `UPDATE process_node_runtime_states SET active_order_id = NULL
		WHERE active_order_id IN (SELECT id FROM orders WHERE status IN ('confirmed','cancelled','failed','skipped'))`
	clearEndedStagedOrderSQL = `UPDATE process_node_runtime_states SET staged_order_id = NULL
		WHERE staged_order_id IN (SELECT id FROM orders WHERE status IN ('confirmed','cancelled','failed','skipped'))`
)

// clearEndedOrderPointers clears a position's active_order_id and
// staged_order_id where they name an order that has ended. The readers take
// the pointers as they stand: the sequential backfill would read a staged
// order that has ended, and the board would draw the position replenishing.
// Open runs it on every start, before anything reads them. A failure is
// logged and does not stop the start (PIN: TestBoot_ClearsPointersAtEndedOrders).
func (db *DB) clearEndedOrderPointers() {
	for _, q := range []string{clearEndedActiveOrderSQL, clearEndedStagedOrderSQL} {
		if _, err := db.Exec(q); err != nil {
			log.Printf("store: clear the pointers at ended orders: %v", err)
		}
	}
}
