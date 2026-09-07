//go:build docker

package dispatch

import (
	"testing"
	"time"

	"shingo/protocol/clock"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// TestLaneEntry_MotionlessWitnessStopsBlocking is the acceptance 2026-09-06
// wedge made into a test: order 379 parked at a lane mouth for fourteen
// sim-hours behind order 376 — queued, never dispatched, never going to
// dispatch (its own wait hung on a dead consumer) — while the evaluator
// correctly re-answered "park" every sweep because a never-dispatched witness
// kept counting as "certainly still coming" forever.
//
// Three legs:
//
//	aged witness     — no status change past the bound  → the shallow store ADMITS
//	fresh witness    — queued seconds ago               → still PARKS (Tier 2 holds)
//	flapping witness — a recent real transition         → still PARKS (liveness is
//	                                                     motion, and a flapping
//	                                                     order is moving)
//
// The fresh leg is the existing Tier-2 behavior restated here so the fixture's
// both directions live in one place; the flap leg pins that a real status
// transition restarts the clock — while SetQueueDetail's in-place cause-stamps
// (which do NOT touch created_at) would not, a distinction the production read
// relies on and this fixture keeps honest by writing rows the way transitions
// do.
func TestLaneEntry_MotionlessWitnessStopsBlocking(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	_, laneID, s0 := gatedLane(t, db, "TEMOT", "")
	laneNode, err := db.GetNode(laneID)
	if err != nil {
		t.Fatalf("get lane: %v", err)
	}
	slots, err := db.ListLaneSlots(laneID)
	if err != nil {
		t.Fatalf("list slots: %v", err)
	}
	var s1 *nodes.Node
	for _, s := range slots {
		dptRaw, err := db.GetSlotDepth(s.ID)
		if dpt := testutil.Must(t, dptRaw, err, "db.GetSlotDepth(s.ID)"); dpt == 1 {
			s1 = s
		}
	}
	if s1 == nil {
		t.Fatal("fixture should have a depth-1 slot")
	}

	// ── leg 1: the AGED witness ────────────────────────────────────────────
	// The witness is a real queued deeper store (CreateOrder's insert writes
	// its birth history row); the backdate then ages the newest row past the
	// bound, which is what fourteen motionless sim-hours looked like.
	shallow := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.DeliveryNode = s0.Name
		o.Status = "queued"
	})
	aged := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.DeliveryNode = s1.Name
		o.Status = "queued"
	})
	backdateNewestRow(t, db, aged.ID, motionlessWitnessBound+time.Minute)

	v, err := d.laneEntryCause(laneNode, shallow, s0)
	if err != nil {
		t.Fatalf("aged witness: %v", err)
	}
	if !v.Admitted() {
		t.Fatalf("aged witness: cause=%q — a never-dispatched witness motionless past %s must stop blocking; "+
			"this is the 14-sim-hour 379/376 wedge", v.Cause(), motionlessWitnessBound)
	}

	// ── leg 2: the FRESH witness still parks ───────────────────────────────
	fresh := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.DeliveryNode = s1.Name
		o.Status = "queued"
	})
	v, err = d.laneEntryCause(laneNode, shallow, s0)
	if err != nil || v.Admitted() || v.Cause() != CauseLaneDeeperPending {
		t.Fatalf("fresh witness %d: admitted=%v cause=%q err=%v — Tier 2 must still park behind an order "+
			"that queued seconds ago", fresh.ID, v.Admitted(), v.Cause(), err)
	}

	// ── leg 3: the FLAPPING witness still parks ────────────────────────────
	// The aged witness transitions (queued → sourcing → queued): a real
	// status change writes fresh rows, the clock restarts, and the witness is
	// live again no matter how old its earlier rows are.
	for _, status := range []string{"sourcing", "queued"} {
		if _, err := db.Exec(`UPDATE orders SET status=$1 WHERE id=$2`, status, aged.ID); err != nil {
			t.Fatalf("flap to %s: %v", status, err)
		}
		if _, err := db.Exec(`INSERT INTO order_history (order_id, status, detail, created_at)
			VALUES ($1, $2, 'fixture flap', $3)`, aged.ID, status, clock.Now().UTC()); err != nil {
			t.Fatalf("flap history %s: %v", status, err)
		}
	}
	v, err = d.laneEntryCause(laneNode, shallow, s0)
	if err != nil || v.Admitted() || v.Cause() != CauseLaneDeeperPending {
		t.Fatalf("flapping witness: admitted=%v cause=%q err=%v — a witness that just transitioned is moving, "+
			"and must still block", v.Admitted(), v.Cause(), err)
	}
}

// backdateNewestRow ages the newest order_history row for orderID to age ago.
//
// UPDATE-not-INSERT, and against the NEWEST row, both on purpose: the
// motionless read is DISTINCT ON (order_id) … ORDER BY order_id, id DESC, so
// id ordering wins over created_at — an INSERTed old-dated row loses to the
// real transition row the fixture just wrote and the backdate silently
// no-ops. The markStaged-style raw UPDATE against the newest row is the only
// spelling that manufactures a motionless witness out of a live fixture.
func backdateNewestRow(t *testing.T, db *store.DB, orderID int64, age time.Duration) {
	t.Helper()
	if _, err := db.Exec(`UPDATE order_history SET created_at = $1
		WHERE id = (SELECT id FROM order_history WHERE order_id = $2
		            ORDER BY id DESC LIMIT 1)`,
		clock.Now().UTC().Add(-age), orderID); err != nil {
		t.Fatalf("backdate newest history row for %d: %v", orderID, err)
	}
}
