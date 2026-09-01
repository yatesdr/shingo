//go:build docker

// Black-box (package orders_test) per the cycle note in orders_test.go.
package orders_test

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// TestCreateOrder_TheOrderAndItsBirthRowCommitTogether pins the transaction the
// birth row's invariant rests on.
//
// orders.Create writes TWO rows and takes a QueryRower, so whether they are
// atomic is the caller's choice. CreateCompoundChildren passes its own tx; every
// other door goes through db.CreateOrder, which was handing over a bare *sql.DB
// — two separate autocommits. A failure between them left either an order with
// no birth certificate (nowhere for SetQueueDetail to stamp a cause, which is
// the whole reason the row exists) or a committed order the caller was told did
// not get created.
//
// The failure is forced with a NOT VALID CHECK on the birth detail: it leaves
// existing rows alone and rejects the second insert, which is exactly the shape
// under test — order row in, history row refused.
func TestCreateOrder_TheOrderAndItsBirthRowCommitTogether(t *testing.T) {
	t.Parallel()
	d := testdb.Open(t)

	const uuid = "uuid-birth-atomic"
	_, err := d.DB.Exec(`ALTER TABLE order_history
		ADD CONSTRAINT tmp_refuse_birth_row CHECK (detail <> 'order created') NOT VALID`)
	testutil.MustNoErr(t, err, "arm the history-insert failure")

	o := newPendingOrder(uuid)
	if err := d.CreateOrder(o); err == nil {
		t.Fatal("CreateOrder reported success while its birth history row was refused")
	}

	_, err = d.DB.Exec(`ALTER TABLE order_history DROP CONSTRAINT tmp_refuse_birth_row`)
	testutil.MustNoErr(t, err, "disarm")

	var n int
	testutil.MustNoErr(t, d.DB.QueryRow(`SELECT COUNT(*) FROM orders WHERE edge_uuid=$1`, uuid).Scan(&n),
		"count the order rows")
	if n != 0 {
		t.Fatalf("%d order row(s) survived a failed create — the caller was told creation failed and "+
			"a live order is sitting in the table for the scanner to pick up", n)
	}
}

// TestCreate_WritesABirthHistoryRow pins that every order's timeline has a
// start, whatever status it is born at.
//
// Order history was written by status TRANSITIONS only, so an order whose first
// status came from the INSERT had no row saying it ever began. That is not a
// cosmetic gap: SetQueueDetail stamps a wait's cause onto the history row of the
// episode the order is resting in, and an order born into an episode that has no
// row has nowhere for its cause to land. Complex intake births at `queued`
// (dispatch/complex_intake.go) and compound children at `pending`
// (store/orders.go CreateCompoundChildren) — between them, every swap leg and
// every dig leg in the plant.
//
// The row is written by orders.Create rather than by each door, because doors
// are counted by a census and a census can be incomplete; there is exactly one
// INSERT INTO orders statement (TestCensus_OrdersTableInsertStatements pins it),
// so hanging the birth row off it makes "every order has a start" structural.
func TestCreate_WritesABirthHistoryRow(t *testing.T) {
	t.Parallel()
	d := testdb.Open(t)
	db := d.DB

	for _, born := range []protocol.Status{protocol.StatusPending, protocol.StatusQueued} {
		o := newPendingOrder("uuid-birth-" + string(born))
		o.Status = born
		testutil.MustNoErr(t, orders.Create(db, o), "create "+string(born))

		rows, err := orders.ListHistory(db, o.ID)
		testutil.MustNoErr(t, err, "list history")
		if len(rows) != 1 {
			var got []string
			for _, r := range rows {
				got = append(got, string(r.Status))
			}
			t.Fatalf("order born %q has %d history rows (%v), want exactly 1 — the birth row", born, len(rows), got)
		}
		if rows[0].Status != born {
			t.Errorf("birth row status = %q, want %q — the row must record the status the order was born at", rows[0].Status, born)
		}
	}
}
