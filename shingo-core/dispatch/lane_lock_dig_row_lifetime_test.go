//go:build docker

package dispatch

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// TestDigRow_SurvivesChildPickup pins the lifetime of Hold A — the reshuffle's
// claim on the lane — against the per-block early release that currently eats it.
//
// ── What happens today ────────────────────────────────────────────────────
//
// LaneLock.TryLock writes a durable mouth row, mode='dig', owner = the compound
// PARENT. Then the first unbury child picks its bin up out of a lane slot, and:
//
//	EventBinEnteredTransit{child, laneSlot}
//	  → HandleTransitForLaneGate(child, slot)
//	  → laneOwnerFor(child) resolves to the PARENT — deliberately, so "a child's
//	    block progress releases the parent-owned hold"
//	  → releaseOrderLaneFor(parent, slot), which filters on NOTHING except "is
//	    this node in a lane": no mode, no enforcement mode, no order type
//	  → reservations.ReleaseLane, whose SQL has no mode predicate either:
//	    DELETE FROM reservations WHERE order_id=$1 AND resource_kind='mouth' AND node_id=$2
//
// So the dig row is deleted at the FIRST pickup of a multi-leg reshuffle, and
// every remaining leg runs with no durable claim on the lane at all.
//
// ── Why nobody noticed ────────────────────────────────────────────────────
//
// The in-memory map is still the grant authority, and the group resolver skips
// dig-locked lanes from memory (five sites), so no competing order ever binds to
// the lane. The row is not load-bearing for exclusion today — it is a mirror.
// And the tripwire that would have reported the divergence, LaneLock.
// CheckDivergence, has no production caller: its only two call sites are in
// lane_lock_mirror_test.go. The row vanishes, memory keeps working, nothing logs.
//
// That is exactly why this test exists rather than a field measurement. The
// mirror is about to become the authority; a hold that silently dies at leg one
// is survivable only while nothing depends on it.
//
// The early release itself is CORRECT for plain orders — an outbound hold should
// drop when the bin leaves the lane, an inbound hold when the bin lands. It is
// wrong only for a dig, whose claim must span every leg. So the fix is a mode
// exemption, not a change to the handoff.
func TestDigRow_SurvivesChildPickup(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	lane := mirrorLane(t, db, "LANE-DIGLIFE", 3)

	slots, err := db.ListLaneSlots(lane)
	if err != nil || len(slots) == 0 {
		t.Fatalf("list lane slots: %v (got %d)", err, len(slots))
	}
	pickupSlot := slots[0] // the mouth slot the first blocker is lifted out of

	parent := testdb.CreateOrder(t, db)
	child := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.ParentOrderID = &parent.ID
	})

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	ll := NewLaneLockWithDB(db.DB)
	if !ll.TryLock(lane, parent.ID) {
		t.Fatal("TryLock on a free lane must succeed")
	}
	if got := digRowCount(t, db, lane); got != 1 {
		t.Fatalf("dig rows after TryLock = %d, want 1", got)
	}

	// The first unbury leg picks its blocker up out of the lane. The dig is not
	// over — the blocker is still in the robot's arms, the target is still buried,
	// and every remaining leg still needs the lane.
	d.HandleTransitForLaneGate(child.ID, pickupSlot.ID)

	if got := digRowCount(t, db, lane); got != 1 {
		t.Fatalf("dig rows after the first child's pickup = %d, want 1.\n"+
			"The reshuffle's claim on lane %d was deleted at leg one: the child's transit routed through "+
			"laneOwnerFor to the PARENT, and releaseOrderLaneFor drops any mouth row on that lane regardless "+
			"of mode. Hold A must span the whole reshuffle — every later leg is now running unclaimed.", got, lane)
	}
	if d := ll.CheckDivergence(); d != 0 {
		t.Fatalf("divergence after the first child's pickup = %d, want 0 — memory still holds the lane "+
			"but the row is gone, which is precisely the mismatch CheckDivergence exists to report and "+
			"which nothing in production is currently asking it about", d)
	}
}

// TestPlainOrderRow_StillReleasedAtPickup is the control that keeps the fix
// narrow. The per-block early handoff is right for a plain order: an outbound
// hold exists so nothing else enters while the bin is being taken OUT, and once
// the bin has left the lane the hold has done its job. Exempting digs must not
// turn that off.
func TestPlainOrderRow_StillReleasedAtPickup(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	lane := mirrorLane(t, db, "LANE-PLAINLIFE", 3)

	slots, err := db.ListLaneSlots(lane)
	if err != nil || len(slots) == 0 {
		t.Fatalf("list lane slots: %v (got %d)", err, len(slots))
	}

	o := testdb.CreateOrder(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	// An OUTBOUND hold: the shape a plain retrieve takes while lifting a bin out.
	if err := reservations.AcquireLanes(db.DB, o.ID, reservations.ModeOutbound, "test", lane); err != nil {
		t.Fatalf("AcquireLanes: %v", err)
	}
	if got := mouthRowCount(t, db, lane); got != 1 {
		t.Fatalf("mouth rows after acquire = %d, want 1", got)
	}

	d.HandleTransitForLaneGate(o.ID, slots[0].ID)

	if got := mouthRowCount(t, db, lane); got != 0 {
		t.Fatalf("mouth rows after a PLAIN order's pickup = %d, want 0 — the early handoff must keep "+
			"working for plain orders; only the dig mode is exempt", got)
	}
}

// mouthRowCount counts ALL active mouth rows on a lane, any mode — the dig-blind
// counterpart to digRowCount.
func mouthRowCount(t *testing.T, db *store.DB, laneID int64) int {
	t.Helper()
	rows, err := reservations.ActiveMouthRows(db.DB, laneID)
	if err != nil {
		t.Fatalf("ActiveMouthRows: %v", err)
	}
	return len(rows)
}
