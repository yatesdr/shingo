//go:build docker

package dispatch

import (
	"errors"
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// The depth-priority test that lived here was deleted with the boost it covered
// (lane_gate.go). Core no longer invents a priority for lane-bound moves.

// TestLaneGateRelease_Outbound: the §4 early handoff — a retrieve's source hold
// frees when its bin transits out. (A store takes no hold to free: every lane is
// marked, so its entry is the gate's tail append, not a dispatch-time hold.)
func TestLaneGateRelease_Outbound(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, laneID, slot := gatedLane(t, db, "RELG", "")
	line := lineNode(t, db, "RELG-LINE")
	retrieve := testdb.CreateOrder(t, db)

	if adm, _, _, _ := d.AcquireLanesForOrder(retrieve, slot, line, EntryFreshBin); !adm {
		t.Fatal("retrieve must be admitted on a free lane")
	}
	if gateMouthRows(t, db, laneID) != 1 {
		t.Fatal("source row must exist after acquire")
	}
	d.HandleTransitForLaneGate(retrieve.ID, slot.ID)
	if n := gateMouthRows(t, db, laneID); n != 0 {
		t.Fatalf("source row not released on transit = %d, want 0", n)
	}
}

// TestLaneGateRelease_ChildRoutesToParent: a compound child's block progress
// releases the PARENT-owned mouth row (children never own rows, §2).
func TestLaneGateRelease_ChildRoutesToParent(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, laneID, slot := gatedLane(t, db, "RELG-CHILD", "")
	line := lineNode(t, db, "RELG-CHILD-LINE")
	parent := testdb.CreateOrder(t, db)
	child := testdb.CreateOrder(t, db, func(o *orders.Order) { o.ParentOrderID = &parent.ID })

	// The parent owns the source hold.
	if adm, _, _, _ := d.AcquireLanesForOrder(parent, slot, line, EntryFreshBin); !adm {
		t.Fatal("parent must be admitted")
	}
	if gateMouthRows(t, db, laneID) != 1 {
		t.Fatal("parent source row must exist")
	}
	// The CHILD carries the bin out; the release must route to the parent's row.
	d.HandleTransitForLaneGate(child.ID, slot.ID)
	if n := gateMouthRows(t, db, laneID); n != 0 {
		t.Fatalf("child transit did not release the parent-owned hold = %d, want 0", n)
	}
}

// gatedLane builds a group (NGRP), a LANE under it, and two depth-ordered slots
// (so the lane is not depth-1 exempt). It returns the group id, lane id, and the
// shallow slot node.
//
// The group is always marked (testdb.MarkGroup): a lane cannot exist without a
// waiting spot. waitPoint, when set, is the lane's own legacy lane_gate_point,
// which wins over the group's points.
//
// It used to take an enforcement value and write it on the GROUP. The property
// had no writer outside tests and is gone; the parameter reads the same at every
// call site and now configures the thing that actually decides.
func gatedLane(t *testing.T, db *store.DB, name, waitPoint string) (groupID, laneID int64, slot *nodes.Node) {
	t.Helper()
	ngrpType, err := db.GetNodeTypeByCode(protocol.NodeClassNGRP)
	if err != nil {
		t.Fatalf("get NGRP type: %v", err)
	}
	laneType, err := db.GetNodeTypeByCode(protocol.NodeClassLANE)
	if err != nil {
		t.Fatalf("get LANE type: %v", err)
	}
	grp := &nodes.Node{Name: name + "-GRP", IsSynthetic: true, Enabled: true, NodeTypeID: &ngrpType.ID}
	if err := db.CreateNode(grp); err != nil {
		t.Fatalf("create group: %v", err)
	}
	testdb.MarkGroup(t, db, grp.ID)
	lane := &nodes.Node{Name: name + "-LANE", IsSynthetic: true, Enabled: true, NodeTypeID: &laneType.ID, ParentID: &grp.ID}
	if err := db.CreateNode(lane); err != nil {
		t.Fatalf("create lane: %v", err)
	}
	if waitPoint != "" {
		if err := db.SetNodeProperty(lane.ID, PropLaneGatePoint, waitPoint); err != nil {
			t.Fatalf("set wait point: %v", err)
		}
	}
	d0, d1 := 0, 1
	s0 := &nodes.Node{Name: name + "-S0", Enabled: true, ParentID: &lane.ID, Depth: &d0}
	if err := db.CreateNode(s0); err != nil {
		t.Fatalf("create slot0: %v", err)
	}
	s1 := &nodes.Node{Name: name + "-S1", Enabled: true, ParentID: &lane.ID, Depth: &d1}
	if err := db.CreateNode(s1); err != nil {
		t.Fatalf("create slot1: %v", err)
	}
	return grp.ID, lane.ID, s0
}

func lineNode(t *testing.T, db *store.DB, name string) *nodes.Node {
	t.Helper()
	n := &nodes.Node{Name: name, Enabled: true}
	if err := db.CreateNode(n); err != nil {
		t.Fatalf("create line node: %v", err)
	}
	return n
}

func gateMouthRows(t *testing.T, db *store.DB, laneID int64) int {
	t.Helper()
	rows, err := reservations.ActiveMouthRows(db.DB, laneID)
	if err != nil {
		t.Fatalf("ActiveMouthRows: %v", err)
	}
	return len(rows)
}

// TestLaneGate_ResolveHolds: what a plain order holds at dispatch. Every lane is
// marked, so a lane ENTRY (a store's drop) is the gate's tail append and takes no
// hold here; a SOURCE is still the full lane lock, held until the bin leaves by
// its mover (§R.101). The hold is read off the source alone.
func TestLaneGate_ResolveHolds(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	_, laneID, slot := gatedLane(t, db, "RES-MOUTH", "")
	line := lineNode(t, db, "RES-LINE")

	holds, err := d.resolveOrderLaneHolds(slot)
	if err != nil {
		t.Fatalf("resolve (retrieve): %v", err)
	}
	if len(holds) != 1 || holds[0].laneID != laneID || holds[0].mode != reservations.ModeDig {
		t.Fatalf("retrieve holds = %+v, want one LOCK on lane %d", holds, laneID)
	}

	holds, err = d.resolveOrderLaneHolds(line)
	if err != nil {
		t.Fatalf("resolve (store): %v", err)
	}
	if len(holds) != 0 {
		t.Fatalf("store holds = %+v, want none — the store's entry is the gate's, at the tail append", holds)
	}

}

// TestLaneGate_SameLaneSourceAndDest_OneHoldStrongestMode pins the wedge
// fix-batch 2b existed for.
//
// An order that picks a bin out of a lane and drops back into it (the operator
// bin-move door) once produced TWO holds for one owner on one lane, and
// acquireOrderLanes took them in two calls: whichever mode committed first left
// a row the second mode then conflicted with — through admitMouth's "still a
// caller bug" arm, whose error does not wrap ErrReservationConflict, so the
// conflict rollback never fired, the committed row survived, and every retry met
// the order's own row. The lane was wedged by the order that needed it.
//
// The destination takes no hold now (every lane is marked; the entry is the
// gate's), so the answer is one dig hold by construction, and it must acquire.
func TestLaneGate_SameLaneSourceAndDest_OneHoldStrongestMode(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, laneID, slot := gatedLane(t, db, "SAME-LANE", "")

	// Same node is both source and destination: the operator bin-move shape.
	holds, err := d.resolveOrderLaneHolds(slot)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(holds) != 1 || holds[0].laneID != laneID || holds[0].mode != reservations.ModeDig {
		t.Fatalf("same-lane holds = %+v, want ONE dig hold on lane %d — two rows for one owner on "+
			"one lane is the incoherent state admitMouth refuses, and the refusal it raises never "+
			"triggers the conflict rollback", holds, laneID)
	}

	// And the acquire admits: one row, no self-conflict, no wedge.
	mover := testdb.CreateOrder(t, db, asDispatched) // (C1.5: a lane holder is dispatched)
	adm := d.acquireOrderLanes(mover.ID, holds)
	admitted, err := adm.admitted, adm.err
	if err != nil || !admitted {
		t.Fatalf("same-lane acquire: admitted=%v err=%v — the deduped hold set must acquire "+
			"cleanly; before the fix this was the wedge (the order's own committed row refusing "+
			"its own second mode)", admitted, err)
	}
	if n := gateMouthRows(t, db, laneID); n != 1 {
		t.Fatalf("lane rows after same-lane acquire = %d, want 1", n)
	}
}

func TestLaneGate_CauseDig(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, laneID, slot := gatedLane(t, db, "CAUSE-DIG", "")
	digger := testdb.CreateOrder(t, db)
	waiter := testdb.CreateOrder(t, db)

	// THE TAG IS THE FIXTURE SAYING WHAT IT ALWAYS MEANT. This planted the row
	// with the reservedBy `"test"`, which was exact while mode='dig' had one
	// meaning. §R.101 gave every demand's SOURCE hold that mode, so the kind is
	// now read off reserved_by (reservations.IsExcavation) and an untagged row is
	// a source lock — which is what this test started reporting. The assertion is
	// unchanged: a foreign EXCAVATION is lane-held-dig, and the fixture now says
	// it is one. The source-lock half of the table is pinned in
	// lane_hold_kind_test.go.
	if err := reservations.AcquireLanes(db.DB, digger.ID, reservations.ModeDig,
		reservations.ByExcavation, laneID); err != nil {
		t.Fatalf("digger acquire: %v", err)
	}
	holds, _ := d.resolveOrderLaneHolds(slot) // waiter wants the lane as its source
	if c := d.causeForLaneHolds(waiter.ID, holds); c != "lane-held-dig" {
		t.Errorf("cause = %q, want lane-held-dig", c)
	}
}

// TestLaneGate_ResolveHoldsRefusesAPlanThatRevisitsALaneItLeaves is the TRIPWIRE
// on the ghost shape, and the ghost shape is a plan nothing builds.
//
// ── WHAT WAS DELETED, AND WHY A TRIPWIRE REPLACED IT ──────────────────────
//
// holderStillOwesTheLane briefly read the holder's remaining ROUTE, looking for a
// later dropoff back into a lane it had already picked from — "owns it for the
// whole visit". The route read was wrong in both of its regimes (it started from
// the wait index, which sits one gate AHEAD of the robot, and fell back to the
// whole plan, which cannot tell a completed drop from a pending one), and the
// shape it defended has no producer:
//
//	THE OPERATOR BIN-MOVE DOOR is structurally two steps, so its in-lane form
//	has its DeliveryNode inside the lane — the arm that survived already covers it.
//
//	THE RELAY parks a bin IN the lane between its two visits, claimed, where
//	legStillNeedsLane sees it. Its dropoff also comes BEFORE its pickup, so it is
//	not this shape at all.
//
// So later visits belong to the LANE'S OWN STATE, not to a route read. What is
// left is the possibility that some future door starts emitting the ghost: a
// pickup in lane L, a LATER dropoff back into L, and a final destination outside
// L. One mouth row cannot honestly express that — it would be held from dispatch
// to terminalization on a lane the robot leaves in between — so the plan is
// REFUSED, loudly and by name, the first time anyone builds one. A wait would
// hide it; a fail says who to talk to.
func TestLaneGate_ResolveHoldsRefusesAPlanThatRevisitsALaneItLeaves(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	_, ghostLaneID, ghostS0 := gatedLane(t, db, "GHOST", "")
	ghostS1, err := db.GetNodeByDotName("GHOST-S1")
	if err != nil || ghostS1 == nil {
		t.Fatalf("read GHOST-S1: %v", err)
	}
	line := lineNode(t, db, "GHOST-LINE")

	t.Run("the ghost is refused", func(t *testing.T) {
		err := d.refuseLaneRevisit([]resolvedStep{
			{Action: protocol.ActionPickup, Node: ghostS0.Name},
			{Action: protocol.ActionDropoff, Node: ghostS1.Name},
			{Action: protocol.ActionPickup, Node: line.Name},
			{Action: protocol.ActionDropoff, Node: line.Name},
		})
		var revisit *LaneRevisitError
		if !errors.As(err, &revisit) {
			t.Fatalf("a plan that picks from lane %d, later drops BACK into it, and finishes somewhere "+
				"else was accepted (err=%v). No door builds that shape, and one mouth row cannot express "+
				"it honestly — the row would be held from dispatch to terminalization across a visit the "+
				"robot leaves in the middle. Refuse it by name, or the first producer of it silently "+
				"mis-holds a corridor forever.", ghostLaneID, err)
		}
		if revisit.Lane == "" || revisit.PickupNode == "" || revisit.DropoffNode == "" {
			t.Fatalf("the refusal is %+v — an operator has to be able to read which lane and which "+
				"steps, or the tripwire reports that something is wrong without saying what", revisit)
		}
	})

	t.Run("an in-lane move is NOT the ghost", func(t *testing.T) {
		// Pick from the lane, drop back into it, and FINISH there. This is the real
		// two-steps-one-lane shape, it is what the operator bin-move door emits, and
		// holderStillOwesTheLane's DeliveryNode arm covers it.
		err := d.refuseLaneRevisit([]resolvedStep{
			{Action: protocol.ActionPickup, Node: ghostS0.Name},
			{Action: protocol.ActionDropoff, Node: ghostS1.Name},
		})
		if err != nil {
			t.Fatalf("an in-lane move was refused (%v). Its final destination IS the lane, so the "+
				"whole-visit hold is exactly right and the DeliveryNode arm releases it at the drop.", err)
		}
	})

	t.Run("a relay is NOT the ghost", func(t *testing.T) {
		// Drop into the lane, then pick back up from it later, finishing elsewhere.
		// The parked bin stands IN the lane between the two visits, claimed, where
		// legStillNeedsLane sees it — so the lane's own state carries this one and
		// there is nothing for a tripwire to say about it.
		err := d.refuseLaneRevisit([]resolvedStep{
			{Action: protocol.ActionDropoff, Node: ghostS1.Name},
			{Action: protocol.ActionPickup, Node: ghostS1.Name},
			{Action: protocol.ActionDropoff, Node: line.Name},
		})
		if err != nil {
			t.Fatalf("a relay was refused (%v). Its bin is parked in the lane between visits, claimed, "+
				"which is the state the claim walk reads — the tripwire must not fire on it.", err)
		}
	})
}
