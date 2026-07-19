//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// TestAcquireLanesForOrder_GatedByConfig exercises the exported scanner wrapper:
// a free mouth lane admits, a different-mode order conflicts (with the operator
// cause + a lane name for the sentence), and a non-mouth group is a no-op.
func TestAcquireLanesForOrder_GatedByConfig(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	_, _, slot := gatedLane(t, db, "AFO-MOUTH", "mouth")
	line := lineNode(t, db, "AFO-LINE")
	a := testdb.CreateOrder(t, db)
	b := testdb.CreateOrder(t, db)

	// Store into the free mouth lane (line → slot, inbound): admitted.
	admitted, _, _, err := d.AcquireLanesForOrder(a.ID, line, slot)
	if err != nil || !admitted {
		t.Fatalf("free lane: admitted=%v err=%v, want admitted", admitted, err)
	}
	// Retrieve from the same lane (slot → line, outbound): different mode → conflict.
	admitted, cause, laneName, err := d.AcquireLanesForOrder(b.ID, slot, line)
	if err != nil {
		t.Fatalf("conflict acquire err: %v", err)
	}
	if admitted {
		t.Fatal("outbound into an inbound-held lane must not be admitted")
	}
	if cause != "lane-held-traffic" {
		t.Errorf("cause = %q, want lane-held-traffic", cause)
	}
	if laneName == "" {
		t.Error("expected a contended-lane name for the queue sentence")
	}

	// A non-mouth group is a no-op — admitted with no hold (byte-identical).
	_, _, noneSlot := gatedLane(t, db, "AFO-NONE", "")
	admitted, _, _, err = d.AcquireLanesForOrder(a.ID, line, noneSlot)
	if err != nil || !admitted {
		t.Fatalf("non-mouth group: admitted=%v err=%v, want admitted no-op", admitted, err)
	}
}

// TestLaneGateRelease_InboundAndOutbound: the §4 early handoff — a store's
// inbound hold frees when it drops, a retrieve's outbound hold frees when its bin
// transits out.
func TestLaneGateRelease_InboundAndOutbound(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, laneID, slot := gatedLane(t, db, "RELG", "mouth")
	line := lineNode(t, db, "RELG-LINE")
	store := testdb.CreateOrder(t, db)
	retrieve := testdb.CreateOrder(t, db)

	if adm, _, _, _ := d.AcquireLanesForOrder(store.ID, line, slot); !adm {
		t.Fatal("store must be admitted on a free lane")
	}
	if gateMouthRows(t, db, laneID) != 1 {
		t.Fatal("inbound row must exist after acquire")
	}
	d.ReleaseInboundLaneForOrder(store.ID, slot.Name)
	if n := gateMouthRows(t, db, laneID); n != 0 {
		t.Fatalf("inbound row not released on dropoff = %d, want 0", n)
	}

	if adm, _, _, _ := d.AcquireLanesForOrder(retrieve.ID, slot, line); !adm {
		t.Fatal("retrieve must be admitted after the store released")
	}
	if gateMouthRows(t, db, laneID) != 1 {
		t.Fatal("outbound row must exist after acquire")
	}
	d.HandleTransitForLaneGate(retrieve.ID, slot.ID)
	if n := gateMouthRows(t, db, laneID); n != 0 {
		t.Fatalf("outbound row not released on transit = %d, want 0", n)
	}
}

// TestLaneGateRelease_ChildRoutesToParent: a compound child's block progress
// releases the PARENT-owned mouth row (children never own rows, §2).
func TestLaneGateRelease_ChildRoutesToParent(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, laneID, slot := gatedLane(t, db, "RELG-CHILD", "mouth")
	line := lineNode(t, db, "RELG-CHILD-LINE")
	parent := testdb.CreateOrder(t, db)
	child := testdb.CreateOrder(t, db, func(o *orders.Order) { o.ParentOrderID = &parent.ID })

	// The parent owns the inbound hold.
	if adm, _, _, _ := d.AcquireLanesForOrder(parent.ID, line, slot); !adm {
		t.Fatal("parent must be admitted")
	}
	if gateMouthRows(t, db, laneID) != 1 {
		t.Fatal("parent inbound row must exist")
	}
	// The CHILD drops; the release must route to the parent's row.
	d.ReleaseInboundLaneForOrder(child.ID, slot.Name)
	if n := gateMouthRows(t, db, laneID); n != 0 {
		t.Fatalf("child dropoff did not release the parent-owned hold = %d, want 0", n)
	}
}

// gatedLane builds a group (NGRP) with the given lane_enforcement value, a LANE
// under it, and two depth-ordered slots (so the lane is not depth-1 exempt). It
// returns the group id, lane id, and the shallow slot node. enforcement "" leaves
// the property unset (defaults to none).
func gatedLane(t *testing.T, db *store.DB, name, enforcement string) (groupID, laneID int64, slot *nodes.Node) {
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
	if enforcement != "" {
		if err := db.SetNodeProperty(grp.ID, PropLaneEnforcement, enforcement); err != nil {
			t.Fatalf("set enforcement: %v", err)
		}
	}
	lane := &nodes.Node{Name: name + "-LANE", IsSynthetic: true, Enabled: true, NodeTypeID: &laneType.ID, ParentID: &grp.ID}
	if err := db.CreateNode(lane); err != nil {
		t.Fatalf("create lane: %v", err)
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

func TestLaneGate_EnforcementMode(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	gNone, _, _ := gatedLane(t, db, "MODE-NONE", "")
	gMouth, _, _ := gatedLane(t, db, "MODE-MOUTH", "mouth")
	gDeleg, _, _ := gatedLane(t, db, "MODE-DELEG", "delegated")
	gJunk, _, _ := gatedLane(t, db, "MODE-JUNK", "banana")

	if got := d.laneEnforcementMode(gNone); got != LaneEnforceNone {
		t.Errorf("unset = %q, want none", got)
	}
	if got := d.laneEnforcementMode(gMouth); got != LaneEnforceMouth {
		t.Errorf("mouth = %q, want mouth", got)
	}
	if got := d.laneEnforcementMode(gDeleg); got != LaneEnforceDelegated {
		t.Errorf("delegated = %q, want delegated", got)
	}
	if got := d.laneEnforcementMode(gJunk); got != LaneEnforceNone {
		t.Errorf("unrecognized = %q, want none", got)
	}
}

func TestLaneGate_ResolveHolds(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	_, mouthLaneID, mouthSlot := gatedLane(t, db, "RES-MOUTH", "mouth")
	_, _, noneSlot := gatedLane(t, db, "RES-NONE", "")
	line := lineNode(t, db, "RES-LINE")

	// Retrieve: pick from a mouth lane, drop at a line → one OUTBOUND hold.
	holds, err := d.resolveOrderLaneHolds(mouthSlot, line)
	if err != nil {
		t.Fatalf("resolve (retrieve): %v", err)
	}
	if len(holds) != 1 || holds[0].laneID != mouthLaneID || holds[0].mode != reservations.ModeOutbound {
		t.Fatalf("retrieve holds = %+v, want one outbound on lane %d", holds, mouthLaneID)
	}

	// Store: pick from a line, drop into a mouth lane → one INBOUND hold.
	holds, err = d.resolveOrderLaneHolds(line, mouthSlot)
	if err != nil {
		t.Fatalf("resolve (store): %v", err)
	}
	if len(holds) != 1 || holds[0].mode != reservations.ModeInbound {
		t.Fatalf("store holds = %+v, want one inbound", holds)
	}

	// A non-mouth group contributes nothing.
	holds, err = d.resolveOrderLaneHolds(noneSlot, line)
	if err != nil {
		t.Fatalf("resolve (none group): %v", err)
	}
	if len(holds) != 0 {
		t.Fatalf("none-group holds = %+v, want none (gate off)", holds)
	}

	// Two mouth lanes (a move) → two holds, one per direction.
	_, _, mouthSlotB := gatedLane(t, db, "RES-MOUTH-B", "mouth")
	holds, err = d.resolveOrderLaneHolds(mouthSlot, mouthSlotB)
	if err != nil {
		t.Fatalf("resolve (move): %v", err)
	}
	if len(holds) != 2 {
		t.Fatalf("move holds = %+v, want two", holds)
	}
}

func TestLaneGate_AcquireAdmitsFreeAndSharesSameMode(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, laneID, slot := gatedLane(t, db, "ACQ", "mouth")
	line := lineNode(t, db, "ACQ-LINE")
	a := testdb.CreateOrder(t, db)
	b := testdb.CreateOrder(t, db)

	holdsA, _ := d.resolveOrderLaneHolds(line, slot) // inbound
	admitted, err := d.acquireOrderLanes(a.ID, holdsA)
	if err != nil || !admitted {
		t.Fatalf("A acquire: admitted=%v err=%v, want admitted", admitted, err)
	}
	if gateMouthRows(t, db, laneID) != 1 {
		t.Fatalf("rows after A = %d, want 1", gateMouthRows(t, db, laneID))
	}
	// B, same mode (inbound), shares.
	holdsB, _ := d.resolveOrderLaneHolds(line, slot)
	admitted, err = d.acquireOrderLanes(b.ID, holdsB)
	if err != nil || !admitted {
		t.Fatalf("B acquire same-mode: admitted=%v err=%v, want admitted (share)", admitted, err)
	}
	if gateMouthRows(t, db, laneID) != 2 {
		t.Fatalf("rows after B share = %d, want 2", gateMouthRows(t, db, laneID))
	}
}

func TestLaneGate_AcquireConflictRequeues(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, laneID, slot := gatedLane(t, db, "CONF", "mouth")
	line := lineNode(t, db, "CONF-LINE")
	a := testdb.CreateOrder(t, db)
	b := testdb.CreateOrder(t, db)

	// A holds inbound.
	holdsA, _ := d.resolveOrderLaneHolds(line, slot)
	if admitted, _ := d.acquireOrderLanes(a.ID, holdsA); !admitted {
		t.Fatal("A must be admitted")
	}
	// B wants outbound (retrieve from the same lane) — different mode → conflict.
	holdsB, _ := d.resolveOrderLaneHolds(slot, line)
	admitted, err := d.acquireOrderLanes(b.ID, holdsB)
	if err != nil {
		t.Fatalf("B acquire err: %v", err)
	}
	if admitted {
		t.Fatal("B outbound into A's inbound lane must NOT be admitted")
	}
	if gateMouthRows(t, db, laneID) != 1 {
		t.Fatalf("rows after conflict = %d, want 1 (only A)", gateMouthRows(t, db, laneID))
	}
	// Cause classifies as traffic (A is inbound, not a dig).
	if c := d.causeForLaneHolds(b.ID, holdsB); c != "lane-held-traffic" {
		t.Errorf("cause = %q, want lane-held-traffic", c)
	}
}

func TestLaneGate_AcquireAllOrNothingAcrossModes(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, laneA, slotA := gatedLane(t, db, "AON-A", "mouth")
	_, laneB, slotB := gatedLane(t, db, "AON-B", "mouth")
	holder := testdb.CreateOrder(t, db)
	mover := testdb.CreateOrder(t, db)

	// holder digs laneB (dig excludes everyone).
	if err := reservations.AcquireLanes(db.DB, holder.ID, reservations.ModeDig, "test", laneB); err != nil {
		t.Fatalf("holder acquire laneB: %v", err)
	}
	// mover wants a move: outbound on laneA + inbound on laneB → laneB conflicts
	// with the dig.
	holds, _ := d.resolveOrderLaneHolds(slotA, slotB)
	admitted, err := d.acquireOrderLanes(mover.ID, holds)
	if err != nil {
		t.Fatalf("mover acquire err: %v", err)
	}
	if admitted {
		t.Fatal("mover must NOT be admitted (laneB conflict)")
	}
	// All-or-nothing: mover holds NOTHING, including laneA.
	if n := gateMouthRows(t, db, laneA); n != 0 {
		t.Fatalf("laneA rows after rolled-back move = %d, want 0 (all-or-nothing)", n)
	}
}

func TestLaneGate_ReleaseDropsHold(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, laneID, slot := gatedLane(t, db, "REL", "mouth")
	line := lineNode(t, db, "REL-LINE")
	a := testdb.CreateOrder(t, db)

	holds, _ := d.resolveOrderLaneHolds(line, slot) // inbound on the lane
	if admitted, _ := d.acquireOrderLanes(a.ID, holds); !admitted {
		t.Fatal("A must be admitted")
	}
	if gateMouthRows(t, db, laneID) != 1 {
		t.Fatal("row must exist after acquire")
	}
	// Release keyed on a slot in the lane (the block landed there).
	if err := d.releaseOrderLaneFor(a.ID, slot); err != nil {
		t.Fatalf("release: %v", err)
	}
	if gateMouthRows(t, db, laneID) != 0 {
		t.Fatalf("rows after release = %d, want 0", gateMouthRows(t, db, laneID))
	}
}

func TestLaneGate_CauseDig(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, laneID, slot := gatedLane(t, db, "CAUSE-DIG", "mouth")
	line := lineNode(t, db, "CAUSE-DIG-LINE")
	digger := testdb.CreateOrder(t, db)
	waiter := testdb.CreateOrder(t, db)

	if err := reservations.AcquireLanes(db.DB, digger.ID, reservations.ModeDig, "test", laneID); err != nil {
		t.Fatalf("digger acquire: %v", err)
	}
	holds, _ := d.resolveOrderLaneHolds(line, slot) // waiter wants inbound
	if c := d.causeForLaneHolds(waiter.ID, holds); c != "lane-held-dig" {
		t.Errorf("cause = %q, want lane-held-dig", c)
	}
}
