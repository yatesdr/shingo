package testdb

import (
	"testing"

	"shingocore/store"
)

// AssertQueuedCarriesACause fails the test for any order_history row into
// `queued` with no code, other than an order's birth row (C1.1.5; RULINGS-C1-P0
// F5: no `queued` write without a code, anywhere).
//
// `queued` is the one place an order rests short of the fleet, and the row is
// where it says why. A transition into it with no code is a wait nobody can
// explain. The lifecycle refuses one; this sweep runs at the end of every docker
// test, so every test that walks an order through a wait is a witness that the
// refusal holds on its path.
//
// THE BIRTH ROW IS EXEMPT. A fixture may create an order already `queued`
// (testdb.CreateOrder's default) to put it in the scanner's set; that row is an
// INSERT, not a write the ladder makes, and orders.Create stamps it with the
// order's birth status.
func AssertQueuedCarriesACause(t testing.TB, db *store.DB) {
	t.Helper()
	rows, err := db.DB.Query(`
		SELECT h.order_id, h.id
		  FROM order_history h
		 WHERE h.status = 'queued' AND COALESCE(h.code, '') = ''
		   AND h.id > (SELECT MIN(b.id) FROM order_history b WHERE b.order_id = h.order_id)
		 ORDER BY h.id`)
	if err != nil {
		t.Fatalf("AssertQueuedCarriesACause: scan order_history: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var orderID, rowID int64
		if err := rows.Scan(&orderID, &rowID); err != nil {
			t.Fatalf("AssertQueuedCarriesACause: scan row: %v", err)
		}
		t.Errorf("QUEUED WITHOUT A CAUSE: order %d went to `queued` with no code (order_history row %d). "+
			"A wait says why on its row; find the write that parked it without one.", orderID, rowID)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("AssertQueuedCarriesACause: order_history: %v", err)
	}
}
