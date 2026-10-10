//go:build docker

package main

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// TestDwell_AnOpenDwellIsAgedOnTheSimClock: history is stamped on the sim
// clock, which a sped-up sim runs ahead of the server's. A dig leg standing at
// its dwell for 60 sim-seconds when the newest history row was written reads
// 60 s, not the server's NOW() minus a timestamp from the sim's future (the
// demo run of 2026-10-08 averaged -2219 s on one such leg).
func TestDwell_AnOpenDwellIsAgedOnTheSimClock(t *testing.T) {
	t.Parallel()
	testdb.DisableWedgeSweep(t, "seeds history rows at chosen times")
	db := testdb.Open(t)

	simNow := time.Now().UTC().Add(48 * time.Hour) // the sim is ahead of the server
	parent := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = protocol.StatusReshuffling })
	leg := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.Status, o.ParentOrderID, o.DeliveryNode = protocol.StatusStaged, &parent.ID, ""
	})
	for _, id := range []int64{parent.ID, leg.ID} {
		_, err := db.Exec(`DELETE FROM order_history WHERE order_id = $1`, id)
		testutil.MustNoErr(t, err, "clear history")
	}
	_, err := db.Exec(`INSERT INTO order_history (order_id, status, created_at) VALUES ($1, $2, $3)`,
		leg.ID, string(protocol.StatusStaged), simNow.Add(-60*time.Second))
	testutil.MustNoErr(t, err, "the leg reaches its dwell")
	_, err = db.Exec(`INSERT INTO order_history (order_id, status, created_at) VALUES ($1, $2, $3)`,
		parent.ID, string(protocol.StatusReshuffling), simNow)
	testutil.MustNoErr(t, err, "the newest row sets the sim clock")

	s := dwellDuration(db, &unrun{})
	if s.n != 1 || s.open != 1 {
		t.Fatalf("dwell = %d leg(s), %d open; want the one open leg", s.n, s.open)
	}
	if s.avgS < 59.5 || s.avgS > 60.5 || s.maxS < 59.5 || s.maxS > 60.5 {
		t.Fatalf("dwell avg %.1f s max %.1f s, want 60 s on the sim clock", s.avgS, s.maxS)
	}
}
