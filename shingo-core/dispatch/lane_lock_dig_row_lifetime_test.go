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
// At the time, an in-memory map was the grant authority and the group resolver
// skipped dig-locked lanes from it, so no competing order ever bound to the
// lane. The row was a mirror, not load-bearing for exclusion. And the tripwire
// that would have reported the divergence, LaneLock.CheckDivergence, had no
// production caller at all. The row vanished, memory kept working, nothing
// logged.
//
// That is why this test exists rather than a field measurement — and why it
// stays now that the map is gone. The row IS the lock today, so a hold that
// dies at leg one is no longer invisible: it is a lane nothing believes is
// claimed, with a dig still working it.
//
// The early release itself is CORRECT for plain orders — an outbound hold should
// drop when the bin leaves the lane, an inbound hold when the bin lands. It is
// wrong only for a dig, whose claim must span every leg. So the fix is a mode
// exemption, not a change to the handoff.
//
// ── THE FIXTURE NOW MATCHES THE PARAGRAPH ABOVE IT (2026-08-13) ───────────
//
// "The dig is not over — the blocker is still in the robot's arms, THE TARGET IS
// STILL BURIED, and EVERY REMAINING LEG still needs the lane" is what this test
// has always said it was about, and the fixture modelled none of the second half:
// one child, no bin, nothing left in the lane. So it passed for a reason weaker
// than its own prose — the row survived because NOTHING released it, rather than
// because something still needed it.
//
// FLIP 2 IS WHY THAT DISTINCTION STOPPED BEING FREE. The dug lane's claim now
// drops when the last blocker LEAVES THE LANE rather than at compound terminal
// (§R.64's C2, fused into the dwell by §R.69), so a dig with nothing left in the
// lane is one whose claim is correctly released at exactly this moment. Against
// the old fixture the two rules collide and the test reads as a regression; with
// a remaining leg — which is what the prose describes and what every real
// multi-leg reshuffle has — they are answering different questions and both hold.
//
// WHAT IS STILL PINNED, unchanged and load-bearing: the per-visit HANDOFF must not
// eat a dig row. That is a mode exemption in ReleaseLaneHandoff's SQL, and it has
// nothing to do with flip 2, which is a deliberate decision made by reading what
// the dig has left to do. The mutation is the same as it always was — drop the
// mode predicate from ReleaseLaneHandoff's DELETE and this fires.
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
	// THE REMAINING LEG, and the bin it has not fetched yet. It sits behind the
	// slot the first leg just emptied, which is what "the target is still buried"
	// means and why the lane is still the dig's.
	target := testdb.CreateBinAtNode(t, db, "PAYLOAD-A", slots[len(slots)-1].ID, "BIN-DIGLIFE-TGT")
	testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.ParentOrderID = &parent.ID
		o.BinID = &target.ID
		o.SourceNode = slots[len(slots)-1].Name
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
	// And the lock still reports the lane as held. With the rows as the sole
	// authority this is the same assertion as the count above rather than a
	// second opinion — which is the point: there is no longer a second place for
	// the answer to differ.
	if !ll.IsLocked(lane) {
		t.Fatal("the lane must still be held after the first child's pickup — the dig is not over")
	}
	if got := ll.LockedBy(lane); got != parent.ID {
		t.Fatalf("lane owner = %d, want the compound parent %d", got, parent.ID)
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
