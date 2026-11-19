package orders

import (
	"database/sql"
	"fmt"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingocore/store/internal/helpers"
)

// ── THE ASK: orders.asked_vendor_order_id ──────────────────────────────────
//
// The vendor order id Core is asking the fleet to create for an order. The
// order's hand sets it in the transaction that takes the order's holds; the
// fleet's answer clears it: a yes names the job in vendor_order_id in the same
// statement (UpdateVendorKeepingRobot, UpdateVendorTx), a no clears it
// (ClearAsk). While it is set:
//
//   - the order is spoken for whatever its status reads: the readers below,
//     and their Go forms in domain/order_ask.go, say so;
//   - no non-terminal status move lands on it (UpdateStatusFromWithReasonTx);
//     an ending does, and the asker then cancels the job it was given.
//
// A crash inside a create leaves the ask on the row: the one record of the id
// Core asked for.

// AcquiringSQL: Core may ask the fleet for the order now (helpers.AcquiringSQL).
// alias is the orders table alias ("" for an unaliased `orders`).
func AcquiringSQL(alias string) string { return helpers.AcquiringSQL(alias) }

// PreDispatchSQL: the order rests before the fleet, holding nothing hard for a
// job (helpers.PreDispatchSQL).
func PreDispatchSQL(alias string) string { return helpers.PreDispatchSQL(alias) }

// PastPreDispatchSQL: the order is spoken for, PreDispatchSQL's complement
// (helpers.PastPreDispatchSQL).
func PastPreDispatchSQL(alias string) string { return helpers.PastPreDispatchSQL(alias) }

// closesAskSQL is the SET fragment by which a vendor write naming the job the
// row is asking for ($1, the vendor order id written) closes the ask: the
// fleet's yes. A write naming any other job leaves the ask as it is.
const closesAskSQL = `asked_vendor_order_id = CASE WHEN asked_vendor_order_id = $1 THEN '' ELSE asked_vendor_order_id END`

// SetAskTx opens the ask on the hand's transaction: the order's row names the
// vendor order id Core is about to ask the fleet for. It writes nothing else —
// no status, history, version or tell: the station is not told an ask. False
// means the row already carries an ask (or is gone), and the caller does not
// ask.
func SetAskTx(tx *sql.Tx, id int64, askedVendorOrderID string) (bool, error) {
	res, err := tx.Exec(`UPDATE orders SET asked_vendor_order_id=$2 WHERE id=$1 AND asked_vendor_order_id=''`,
		id, askedVendorOrderID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ClearAsk closes the ask the fleet answered without a job: it refused the
// create, or it took the cancel of the job it was given. Only the ask named is
// cleared, so a late clear cannot erase a newer ask. False means the row no
// longer carries that ask.
func ClearAsk(db *sql.DB, id int64, askedVendorOrderID string) (bool, error) {
	res, err := db.Exec(`UPDATE orders SET asked_vendor_order_id='' WHERE id=$1 AND asked_vendor_order_id=$2`,
		id, askedVendorOrderID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ChaseEndedAsk is the ask of an order that ended while its create was in
// flight, when the fleet did not take the cancel of the job it created: the
// job is moved from the ask to vendor_order_id, as CREATED, in one statement,
// so it stays named and the terminate chase (engine.chaseUnterminatedOrders,
// through ListEndedWithLiveVendorState) asks the fleet about it and re-sends the
// terminate. updated_at is now, which puts the row in the chase's window. Only
// an ended row carrying that ask is written; false means there is none.
func ChaseEndedAsk(db *sql.DB, id int64, askedVendorOrderID string) (bool, error) {
	res, err := db.Exec(fmt.Sprintf(`UPDATE orders
		SET vendor_order_id=$2, vendor_state='CREATED', asked_vendor_order_id='', updated_at=$3
		WHERE id=$1 AND asked_vendor_order_id=$2 AND status IN (%s)`, protocol.TerminalStatusSQLList()),
		id, askedVendorOrderID, clock.Now().UTC())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
