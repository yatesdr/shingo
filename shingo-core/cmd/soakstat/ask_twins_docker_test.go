//go:build docker

package main

import (
	"fmt"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// TestAskTwins_AnOrderBeingAskedForCountsInFlight: soakstat's own spellings of
// "parked" and "in flight", read against real rows. An order Core is asking the
// fleet for is in flight whatever its status says, as the old in-create
// `dispatched` row is: the stall kinds' rendered SQL puts it under "in flight",
// the order counts count it in flight and not queued, and the stranded report's
// releaser reads it as live work bringing its resource. A `queued` row at rest
// is parked, queued, and not a bringer.
func TestAskTwins_AnOrderBeingAskedForCountsInFlight(t *testing.T) {
	t.Parallel()
	testdb.ExpectAskLeftOpen(t, "the pin writes an ask onto a row to ask soakstat's readers about it")
	db := testdb.Open(t)
	seed := func(status protocol.Status, asked string) int64 {
		id := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = status }).ID
		_, err := db.Exec(`UPDATE orders SET asked_vendor_order_id = $2 WHERE id = $1`, id, asked)
		testutil.MustNoErr(t, err, "write the ask")
		return id
	}
	before := seed(protocol.StatusDispatched, "")
	asking := seed(protocol.StatusQueued, "sg-twin-ask")
	atRest := seed(protocol.StatusQueued, "")

	kindOf := map[int64][]string{}
	for _, p := range stallPopulations {
		rows, err := db.Query(fmt.Sprintf(`SELECT id FROM orders WHERE %s`, p.clause()))
		testutil.MustNoErr(t, err, "the "+p.label+" clause")
		for rows.Next() {
			var id int64
			testutil.MustNoErr(t, rows.Scan(&id), "scan")
			kindOf[id] = append(kindOf[id], p.label)
		}
		testutil.MustNoErr(t, rows.Close(), "close")
	}
	for id, want := range map[int64]string{before: "in flight", asking: "in flight", atRest: "parked"} {
		if got := kindOf[id]; len(got) != 1 || got[0] != want {
			t.Errorf("order %d falls in stall kinds %v, want [%s]", id, got, want)
		}
	}

	c := orderCounts(db, &unrun{})
	if c.queued.n != 1 || c.inFlight.n != 2 {
		t.Errorf("order counts: queued %s, in flight %s; want 1 (the row at rest) and 2 (the old in-create order and the asked one)",
			c.queued, c.inFlight)
	}

	bringer := map[int64]bool{}
	for _, id := range []int64{before, asking, atRest} {
		var status string
		testutil.MustNoErr(t, db.QueryRow(`SELECT status FROM orders WHERE id=$1`, id).Scan(&status), "status")
		var asked string
		testutil.MustNoErr(t, db.QueryRow(`SELECT asked_vendor_order_id FROM orders WHERE id=$1`, id).Scan(&asked), "ask")
		re := &runEnd{byID: map[int64]*activeOrder{}}
		h := &activeOrder{id: id, status: status, asked: asked, dst: "TWIN-SRC", carries: "TWIN"}
		re.byID[id], re.ids = h, []int64{id}
		waiter := &activeOrder{id: 0, typ: string(protocol.OrderTypeRetrieve), src: "TWIN-SRC", payload: "TWIN"}
		_, live := re.bringer(nearProgress{})(waiter)
		bringer[id] = live
	}
	if !bringer[before] || !bringer[asking] || bringer[atRest] {
		t.Errorf("the stranded releaser reads bringers %v; want the old in-create order and the asked one live, the row at rest not", bringer)
	}
}
