//go:build docker

package main

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// TestHoldsAtRest_TheInstrumentIsSelective proves the soak's holds-at-rest check
// and its PAIR count against real rows before a sim pair leans on them.
//
// HARD is the claim columns plus CONFIRMED slot, mouth and occupancy
// reservations; a PENDING reservation is the soft hold a waiting order keeps and
// never counts. So five waiting orders are seeded, one per kind of hard hold,
// and all five are reported. Four orders must NOT be: one that has reached
// `dispatched` (the hold is what the fleet call is for), a compound leg (born
// holding its part of the parent's hand, excepted until that moves), a terminal
// order (a leak, which checkTerminalOrderReservations owns), and a waiting order
// whose only reservation is pending. A check that flagged all nine would say
// nothing.
func TestHoldsAtRest_TheInstrumentIsSelective(t *testing.T) {
	t.Parallel()
	testdb.DisableWedgeSweep(t, "pins the holds-at-rest detector: its rows are the violations it must find")
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)

	order := func(status protocol.Status, parent *int64) int64 {
		o := testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.Status = status
			o.ParentOrderID = parent
		})
		return o.ID
	}
	holdBin := func(orderID int64, label string) {
		b := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, label)
		_, err := db.Exec(`UPDATE bins SET claimed_by = $1 WHERE id = $2`, orderID, b.ID)
		testutil.MustNoErr(t, err, "seed a bin claim")
	}
	nodeRes := func(orderID int64, kind, state string, nodeID int64) {
		_, err := db.Exec(`INSERT INTO reservations (order_id, resource_kind, state, node_id, expires_at)
			VALUES ($1, $2, $3, $4, $5)`, orderID, kind, state, nodeID, time.Now().Add(time.Hour))
		testutil.MustNoErr(t, err, "seed a "+state+" "+kind+" reservation")
	}

	// (C1: the waiting holders rest in `queued`; `sourcing` is no longer a status.)
	waitingBin := order(protocol.StatusQueued, nil)
	holdBin(waitingBin, "HAR-SOAK-WAIT-BIN")
	waitingSlot := order(protocol.StatusQueued, nil)
	_, err := db.Exec(`UPDATE nodes SET claimed_by = $1 WHERE id = $2`, waitingSlot, sd.StorageNode.ID)
	testutil.MustNoErr(t, err, "seed a slot claim")
	waitingSlotRes := order(protocol.StatusQueued, nil)
	nodeRes(waitingSlotRes, "slot", "confirmed", sd.StorageNode.ID)
	waitingMouth := order(protocol.StatusQueued, nil)
	nodeRes(waitingMouth, "mouth", "confirmed", sd.StorageNode.ID)
	waitingOcc := order(protocol.StatusPending, nil)
	nodeRes(waitingOcc, "occupancy", "confirmed", sd.StorageNode.ID)

	holdBin(order(protocol.StatusDispatched, nil), "HAR-SOAK-DISPATCHED-BIN")
	parent := order(protocol.StatusReshuffling, nil)
	holdBin(order(protocol.StatusPending, &parent), "HAR-SOAK-LEG-BIN")
	holdBin(order(protocol.StatusConfirmed, nil), "HAR-SOAK-TERMINAL-BIN")
	nodeRes(order(protocol.StatusQueued, nil), "slot", "pending", sd.LineNode.ID)

	want := map[int64]string{
		waitingBin:     "is queued holding 1 bin(s), 0 slot(s), 0 confirmed slot reservation(s), 0 confirmed mouth(s), 0 confirmed occupancy row(s)",
		waitingSlot:    "is queued holding 0 bin(s), 1 slot(s), 0 confirmed slot reservation(s), 0 confirmed mouth(s), 0 confirmed occupancy row(s)",
		waitingSlotRes: "is queued holding 0 bin(s), 0 slot(s), 1 confirmed slot reservation(s), 0 confirmed mouth(s), 0 confirmed occupancy row(s)",
		waitingMouth:   "is queued holding 0 bin(s), 0 slot(s), 0 confirmed slot reservation(s), 1 confirmed mouth(s), 0 confirmed occupancy row(s)",
		waitingOcc:     "is pending holding 0 bin(s), 0 slot(s), 0 confirmed slot reservation(s), 0 confirmed mouth(s), 1 confirmed occupancy row(s)",
	}
	v := checkHoldsAtRest(db)
	if len(v) != len(want) {
		t.Fatalf("want exactly the %d waiting holders reported, got %d:\n%s", len(want), len(v), strings.Join(v, "\n"))
	}
	for id, frag := range want {
		if !hasLine(v, "HOLDS AT REST: order "+strconv.FormatInt(id, 10)+" "+frag) {
			t.Errorf("order %d not reported as %q:\n%s", id, frag, strings.Join(v, "\n"))
		}
	}
	if p := pairMeasures(db, &unrun{}, checkInvariants(db), nil); p.holdsAtRest != len(want) {
		t.Errorf("PAIR holds-at-rest = %d, want %d", p.holdsAtRest, len(want))
	}
}

// TestPair_HistoryWedgesAgeFromTheLastHistoryRow proves the hist-wedges column
// sees what the updated_at-aged stall checker cannot: an order re-parked every
// scanner pass (updated_at fresh) whose last HISTORY row is old against the sim
// clock, which is the newest history row in the table.
func TestPair_HistoryWedgesAgeFromTheLastHistoryRow(t *testing.T) {
	t.Parallel()
	testdb.DisableWedgeSweep(t, "seeds history rows at chosen ages")
	db := testdb.Open(t)

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	seed := func(status protocol.Status, lastAgo time.Duration) {
		o := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = status })
		_, err := db.Exec(`DELETE FROM order_history WHERE order_id = $1`, o.ID)
		testutil.MustNoErr(t, err, "clear history")
		_, err = db.Exec(`INSERT INTO order_history (order_id, status, created_at) VALUES ($1, $2, $3)`,
			o.ID, string(status), now.Add(-lastAgo))
		testutil.MustNoErr(t, err, "seed history")
		// updated_at is fresh, as a re-parking scanner pass leaves it.
		_, err = db.Exec(`UPDATE orders SET updated_at = now() WHERE id = $1`, o.ID)
		testutil.MustNoErr(t, err, "freshen updated_at")
	}
	seed(protocol.StatusQueued, 40*time.Minute)    // parked past its 15-min budget: counted
	seed(protocol.StatusPending, 5*time.Minute)    // parked, within budget: not counted
	seed(protocol.StatusInTransit, 25*time.Minute) // in flight past its 20-min budget: counted
	seed(protocol.StatusConfirmed, 90*time.Minute) // terminal: in no population
	seed(protocol.StatusDispatched, 0)             // the newest row: sets the sim clock

	n, oldest, err := historyWedges(db)
	testutil.MustNoErr(t, err, "history wedges")
	if n != 2 || oldest < 39.9 || oldest > 40.1 {
		t.Fatalf("hist-wedges = %d (oldest %.1f min), want 2 (oldest 40.0): the queued order at 40 min and "+
			"the in-transit order at 25 min", n, oldest)
	}
	if p := pairMeasures(db, &unrun{}, checkInvariants(db), nil); p.wedges != 0 {
		t.Errorf("updated_at-aged wedges = %d, want 0 — every row was freshened, which is the blindness "+
			"hist-wedges exists to cover", p.wedges)
	}
}
