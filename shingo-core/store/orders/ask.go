package orders

import (
	"database/sql"
	"errors"
	"fmt"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingocore/store/internal/helpers"
)

// ── THE ASK: orders.asked_vendor_order_id ──────────────────────────────────
//
// The vendor order id Core is asking the fleet to create for an order. The
// order's hand opens it as its first row lock (TakeAskTx), in the transaction
// that takes the order's holds, and writes no status: the order keeps the one
// it rests in. The fleet's answer closes it:
//
//   - yes: FleetYesTx writes `dispatched` with the vendor id in one statement;
//   - no: ClearAskTx, in the transaction that demotes the order's holds;
//   - the order ended during the create: the job is cancelled, and the ask
//     closes once the fleet took the cancel (ClearAsk), or the job moves to the
//     ended row for the terminate chase (ChaseEndedAsk).
//
// While it is open:
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

// TakeAskTx is the hand's first row lock: the compare-and-swap that opens the
// ask on a row still in `from` with no ask open. It writes nothing else — no
// status, history, version or tell: the station is not told an ask. False
// means the order moved, or another caller is asking for it; the caller rolls
// back and yields.
func TakeAskTx(tx *sql.Tx, id int64, from protocol.Status, askedVendorOrderID string) (bool, error) {
	res, err := tx.Exec(`UPDATE orders SET asked_vendor_order_id=$3
		WHERE id=$1 AND status=$2 AND asked_vendor_order_id=''`, id, string(from), askedVendorOrderID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// FleetYesTx is the fleet's yes, on the caller's transaction: in one statement
// guarded on the status the hand found and on the ask, the order goes
// `dispatched` naming the job (vendor_order_id, CREATED), with the robot the
// create pinned or none and the record of the blocks the create sent (actions,
// block id -> action, into block_actions: replaced, not merged, since every
// block of the job named here was sent now), the ask and the wait cleared and
// the version bumped; then its history row. It returns the row as written
// (Told), for the station's tell. False, with nothing written, means the row no
// longer holds that ask in that status: the order ended while the fleet was
// creating its job. One of the writers of block_actions, with ClaimAppend.
func FleetYesTx(tx *sql.Tx, id int64, from protocol.Status, askedVendorOrderID, robotID string, actions map[string]string, detail, code, actor string, refJSON any) (Told, bool, error) {
	to := protocol.StatusDispatched
	raw, err := blockActionsJSON(actions)
	if err != nil {
		return Told{}, false, err
	}
	told, err := ScanTold(tx.QueryRow(`UPDATE orders SET status=$1, vendor_order_id=$2, vendor_state='CREATED',
		robot_id=$3, block_actions=$7::jsonb, asked_vendor_order_id='', error_detail='', updated_at=$4,
		version=version+1`+waitEndsSQL(string(to))+`
		WHERE id=$5 AND status=$6 AND asked_vendor_order_id=$2`+ToldReturning,
		string(to), askedVendorOrderID, robotID, clock.Now().UTC(), id, string(from), raw))
	if errors.Is(err, sql.ErrNoRows) {
		return Told{}, false, nil
	}
	if err != nil {
		return Told{}, false, err
	}
	if _, err := tx.Exec(
		`INSERT INTO order_history (order_id, status, detail, code, actor, ref, created_at)
		 VALUES ($1, $2, $3, NULLIF($4,''), NULLIF($5,''), $6, $7)`,
		id, string(to), detail, code, actor, refJSON, clock.Now().UTC()); err != nil {
		return Told{}, false, err
	}
	return told, true, nil
}

// ClearAskTx closes, on the caller's transaction, an ask the fleet answered
// without a job Core keeps. Only the ask named is cleared, so a late clear
// cannot erase a newer ask. False means the row no longer carries that ask.
func ClearAskTx(tx *sql.Tx, id int64, askedVendorOrderID string) (bool, error) {
	res, err := tx.Exec(`UPDATE orders SET asked_vendor_order_id='' WHERE id=$1 AND asked_vendor_order_id=$2`,
		id, askedVendorOrderID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ClearAsk is ClearAskTx on a statement of its own: the ask of an order that
// ended during its create, once the fleet has taken the cancel of its job.
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

// OpenAsk is an ask open on an order row: the order, the status it rests in,
// its requester (station, or the Core actor that asked), and the job Core
// asked the fleet for.
type OpenAsk struct {
	OrderID   int64
	Status    protocol.Status
	Requester string
	Asked     string
}

// ListOpenAsks is every order row with an ask open, live or ended, by id. At
// boot each one is a create a previous Core did not live to answer
// (engine.ReconciliationService.TakeBootAsks).
func ListOpenAsks(db *sql.DB) ([]OpenAsk, error) {
	rows, err := db.Query(`SELECT id, status, ` + helpers.RequesterSQL("") + `, asked_vendor_order_id
		FROM orders WHERE asked_vendor_order_id <> '' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OpenAsk
	for rows.Next() {
		var a OpenAsk
		var status string
		if err := rows.Scan(&a.OrderID, &status, &a.Requester, &a.Asked); err != nil {
			return nil, err
		}
		a.Status = protocol.Status(status)
		out = append(out, a)
	}
	return out, rows.Err()
}
