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
	admitted, _, _, err := d.AcquireLanesForOrder(a, line, slot, EntryFreshBin)
	if err != nil || !admitted {
		t.Fatalf("free lane: admitted=%v err=%v, want admitted", admitted, err)
	}
	// Retrieve from the same lane (slot → line, outbound): different mode → conflict.
	admitted, cause, laneName, err := d.AcquireLanesForOrder(b, slot, line, EntryFreshBin)
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

	// AN UNMARKED LANE IS ACQUIRED, NOT SKIPPED — the block here read "A non-mouth
	// group is a no-op — admitted with no hold (byte-identical)". After a1 there is
	// no such thing as a lane the mouth does not sequence, so the assertion is
	// restated as what it can still prove: a free unmarked lane admits, and the
	// hold it takes is real enough for a second order to be refused on it.
	_, noneLaneID, noneSlot := gatedLane(t, db, "AFO-NONE", "")
	admitted, _, _, err = d.AcquireLanesForOrder(a, line, noneSlot, EntryFreshBin)
	if err != nil || !admitted {
		t.Fatalf("free unmarked lane: admitted=%v err=%v, want admitted", admitted, err)
	}
	if n := gateMouthRows(t, db, noneLaneID); n != 1 {
		t.Fatalf("unmarked lane holds %d mouth row(s), want 1 — a lane with no mark is still a "+
			"single-file corridor", n)
	}
}

// The depth-priority test that lived here was deleted with the boost it covered
// (lane_gate.go). Core no longer invents a priority for lane-bound moves.

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

	if adm, _, _, _ := d.AcquireLanesForOrder(store, line, slot, EntryFreshBin); !adm {
		t.Fatal("store must be admitted on a free lane")
	}
	if gateMouthRows(t, db, laneID) != 1 {
		t.Fatal("inbound row must exist after acquire")
	}
	d.ReleaseInboundLaneForOrder(store.ID, slot.Name)
	if n := gateMouthRows(t, db, laneID); n != 0 {
		t.Fatalf("inbound row not released on dropoff = %d, want 0", n)
	}

	if adm, _, _, _ := d.AcquireLanesForOrder(retrieve, slot, line, EntryFreshBin); !adm {
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
	if adm, _, _, _ := d.AcquireLanesForOrder(parent, line, slot, EntryFreshBin); !adm {
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

// gatedLane builds a group (NGRP), a LANE under it, and two depth-ordered slots
// (so the lane is not depth-1 exempt). It returns the group id, lane id, and the
// shallow slot node.
//
// waitPoint is the lane's mark — the whole of the gate's configuration since the
// enforcement mode was deleted. Non-empty means the lane is gated: robots dwell
// there and Core appends their tail when the lane is safe. Empty leaves the lane
// ungated, which is every lane at every plant until a human places a mark.
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

// TestLaneGate_MarkIsTheEnablement replaces the two enforcement-mode tests that
// stood here.
//
// They pinned a three-valued enforcement property on the GROUP, and every arm
// of it described a switch no plant ever set. It is deleted.
//
// What replaces it is one fact on the LANE: the waiting point. There is nothing
// to fall back from and no pair of arms to keep in step — a lane has a mark or it
// does not, and the mark is both the enablement and the place the robot waits.
// The hazard the old tests guarded against (a new arm silently inheriting the
// off branch) cannot exist in a two-state derivation with no arms.
func TestLaneGate_MarkIsTheEnablement(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	_, unmarked, _ := gatedLane(t, db, "MARK-OFF", "")
	_, marked, _ := gatedLane(t, db, "MARK-ON", "MARK-ON-WAIT")

	if d.laneIsGated(unmarked) {
		t.Error("an unmarked lane reports gated. Every lane at both plants is unmarked, so this " +
			"would turn staging on plant-wide the moment the branch deployed — the exact global " +
			"flip the mark ruling exists to avoid")
	}
	if !d.laneIsGated(marked) {
		t.Error("a marked lane reports ungated — placing the mark is the only act that enables a lane")
	}

	// The value reaches the fleet verbatim as a block location, so it must come
	// back exactly as written rather than normalised.
	if got := d.laneWaitPoint(marked); got != "MARK-ON-WAIT" {
		t.Errorf("wait point = %q, want %q verbatim", got, "MARK-ON-WAIT")
	}

	// And the valve agrees with the derivation — one fact, one answer, no second
	// switch that could disagree with it.
	laneNode, err := db.GetNode(marked)
	if err != nil {
		t.Fatalf("get lane: %v", err)
	}
	target, gated, err := d.gateTargetForLane(laneNode)
	if err != nil || !gated {
		t.Fatalf("gateTargetForLane on a marked lane: gated=%v err=%v", gated, err)
	}
	if target.gatePoint != "MARK-ON-WAIT" {
		t.Errorf("valve target = %q, want the lane's mark", target.gatePoint)
	}

	offNode, err := db.GetNode(unmarked)
	if err != nil {
		t.Fatalf("get unmarked lane: %v", err)
	}
	if _, gated, err := d.gateTargetForLane(offNode); err != nil || gated {
		t.Errorf("gateTargetForLane on an unmarked lane: gated=%v err=%v — an unmarked lane must be "+
			"invisible to the valve, not an error", gated, err)
	}
}

func TestLaneGate_ResolveHolds(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	_, mouthLaneID, mouthSlot := gatedLane(t, db, "RES-MOUTH", "mouth")
	_, _, noneSlot := gatedLane(t, db, "RES-NONE", "")
	line := lineNode(t, db, "RES-LINE")

	// Retrieve: pick from a lane, drop at a line → one hold on the SOURCE, and it
	// is the full lane LOCK.
	//
	// RE-POINTED by §R.101: this asserted `ModeOutbound` with the message "want one
	// outbound on lane %d". A source hold shares no longer — a demand that resolves
	// onto a bin owns that lane until the bin leaves by its mover, which is arm 2's
	// dig rule generalized to every order.
	holds, err := d.resolveOrderLaneHolds(mouthSlot, line)
	if err != nil {
		t.Fatalf("resolve (retrieve): %v", err)
	}
	if len(holds) != 1 || holds[0].laneID != mouthLaneID || holds[0].mode != reservations.ModeDig {
		t.Fatalf("retrieve holds = %+v, want one LOCK on lane %d", holds, mouthLaneID)
	}

	// Store: pick from a line, drop into a mouth lane → one INBOUND hold.
	holds, err = d.resolveOrderLaneHolds(line, mouthSlot)
	if err != nil {
		t.Fatalf("resolve (store): %v", err)
	}
	if len(holds) != 1 || holds[0].mode != reservations.ModeInbound {
		t.Fatalf("store holds = %+v, want one inbound", holds)
	}

	// AN UNMARKED LANE STILL CONTRIBUTES — RE-POINTED by §R.95/§R.96 stage 2, a1.
	//
	// This read "A non-mouth group contributes nothing" and asserted
	// `len(holds) != 0` was a failure with the message "want none (gate off)".
	// That was the whole defect stated as a requirement: §R.95's census found zero
	// holds on unmarked lanes at both plants, so the mouth machinery had never run
	// where it mattered. The mark says where a robot WAITS, which is configuration;
	// it never said whether a single-file lane needs sequencing, which is physics.
	holds, err = d.resolveOrderLaneHolds(noneSlot, line)
	if err != nil {
		t.Fatalf("resolve (unmarked lane): %v", err)
	}
	if len(holds) != 1 || holds[0].mode != reservations.ModeDig {
		t.Fatalf("unmarked-lane holds = %+v, want one lock — every lane yields holds; the mark "+
			"chooses where the waiting happens, not whether the mouth is sequenced", holds)
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
	adm := d.acquireOrderLanes(a.ID, holdsA)
	admitted, err := adm.admitted, adm.err
	if err != nil || !admitted {
		t.Fatalf("A acquire: admitted=%v err=%v, want admitted", admitted, err)
	}
	if gateMouthRows(t, db, laneID) != 1 {
		t.Fatalf("rows after A = %d, want 1", gateMouthRows(t, db, laneID))
	}
	// B, same mode (inbound), shares.
	holdsB, _ := d.resolveOrderLaneHolds(line, slot)
	adm = d.acquireOrderLanes(b.ID, holdsB)
	admitted, err = adm.admitted, adm.err
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
	if !d.acquireOrderLanes(a.ID, holdsA).admitted {
		t.Fatal("A must be admitted")
	}
	// B wants outbound (retrieve from the same lane) — different mode → conflict.
	holdsB, _ := d.resolveOrderLaneHolds(slot, line)
	adm := d.acquireOrderLanes(b.ID, holdsB)
	admitted, err := adm.admitted, adm.err
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
	adm := d.acquireOrderLanes(mover.ID, holds)
	admitted, err := adm.admitted, adm.err
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

// TestLaneGate_SameLaneSourceAndDest_OneHoldStrongestMode pins the dedupe
// fix-batch 2b exists for.
//
// The source arm and the dest arm of resolveOrderLaneHolds can resolve to the
// SAME lane — an order that picks a bin out of a lane and drops back into it,
// reachable through the operator bin-move door. Before the dedupe, that
// produced TWO holds for one owner on one lane, and acquireOrderLanes took
// them in two calls whose order a Go map chose at random: whichever mode
// committed first left a row the second mode then conflicted with — through
// admitMouth's "still a caller bug" arm, whose error does not wrap
// ErrReservationConflict, so the conflict rollback never fired, the committed
// row survived, and every retry met the order's own row. The lane was wedged
// by the order that needed it, permanently.
//
// The deduped answer is one hold, dig: an order that both picks from and drops
// into a lane owns it for the whole visit, the same rule
// resolvePlanLaneHolds states for a plan walk.
//
// MUTATION (verified): delete the dedupe (restore the plain append). The
// holds-length assertion fires — and, driven through acquireOrderLanes, the
// wedge itself is pinned by the sibling test below.
func TestLaneGate_SameLaneSourceAndDest_OneHoldStrongestMode(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, laneID, slot := gatedLane(t, db, "SAME-LANE", "mouth")

	// Same node is both source and destination: the operator bin-move shape.
	holds, err := d.resolveOrderLaneHolds(slot, slot)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(holds) != 1 || holds[0].laneID != laneID || holds[0].mode != reservations.ModeDig {
		t.Fatalf("same-lane holds = %+v, want ONE dig hold on lane %d — two rows for one owner on "+
			"one lane is the incoherent state admitMouth refuses, and the refusal it raises never "+
			"triggers the conflict rollback", holds, laneID)
	}

	// And the acquire admits: one row, no self-conflict, no wedge.
	mover := testdb.CreateOrder(t, db)
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

func TestLaneGate_ReleaseDropsHold(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, laneID, slot := gatedLane(t, db, "REL", "mouth")
	line := lineNode(t, db, "REL-LINE")
	a := testdb.CreateOrder(t, db)

	holds, _ := d.resolveOrderLaneHolds(line, slot) // inbound on the lane
	if !d.acquireOrderLanes(a.ID, holds).admitted {
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
	holds, _ := d.resolveOrderLaneHolds(line, slot) // waiter wants inbound
	if c := d.causeForLaneHolds(waiter.ID, holds); c != "lane-held-dig" {
		t.Errorf("cause = %q, want lane-held-dig", c)
	}
}

// TestLaneGate_UnmarkedLaneSerializesOpposingModes is a1's whole point, at the
// acquire rather than at the derivation (§R.95/§R.96 stage 2).
//
// §R.95's census: zero mouth holds on unmarked lanes at Springfield and
// Hopkinsville, and complex never acquiring at all. The machinery was correct and
// had never run where the plant is. An unmarked lane is not a lane with no
// constraint — it is a single-file corridor whose constraint nobody was asking
// about, and one robot in it going out while another comes in is the collision
// the mouth exists to prevent.
//
// MUTATION (verified): restore the `if !d.laneIsGated(lane.ID) { return nil }`
// skip in resolveOrderLaneHolds. B is admitted into the lane A is picking out of
// and the "want refused" assertion fires.
func TestLaneGate_UnmarkedLaneSerializesOpposingModes(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	_, laneID, slot := gatedLane(t, db, "UNMARKED-SER", "") // no mark: the plant's shape
	line := lineNode(t, db, "UNMARKED-SER-LINE")
	a := testdb.CreateOrder(t, db)
	b := testdb.CreateOrder(t, db)

	if d.laneIsGated(laneID) {
		t.Fatal("fixture is wrong: this lane carries a mark, so it proves nothing about the plant")
	}

	// A picks OUT of the lane.
	holdsA, err := d.resolveOrderLaneHolds(slot, line)
	if err != nil {
		t.Fatalf("resolve A: %v", err)
	}
	adm := d.acquireOrderLanes(a.ID, holdsA)
	admitted, err := adm.admitted, adm.err
	if err != nil || !admitted {
		t.Fatalf("A acquire: admitted=%v err=%v, want admitted on a free lane", admitted, err)
	}

	// B wants to drop INTO it while A is picking out. Opposing modes in one
	// single-file corridor.
	holdsB, err := d.resolveOrderLaneHolds(line, slot)
	if err != nil {
		t.Fatalf("resolve B: %v", err)
	}
	adm = d.acquireOrderLanes(b.ID, holdsB)
	admitted, err = adm.admitted, adm.err
	if err != nil {
		t.Fatalf("B acquire: %v", err)
	}
	if admitted {
		t.Fatal("B was admitted into the lane A is picking out of — an unmarked lane is still " +
			"single-file, and this is the collision the mouth exists to prevent")
	}

	// AND IT IS A WAIT, NOT A WALL: when A gives the lane back, B goes in.
	if _, err := reservations.ReleaseLanesByOwner(db.DB, a.ID); err != nil {
		t.Fatalf("release A: %v", err)
	}
	adm = d.acquireOrderLanes(b.ID, holdsB)
	admitted, err = adm.admitted, adm.err
	if err != nil || !admitted {
		t.Fatalf("B after A released: admitted=%v err=%v — a refusal with no releaser is not a wait", admitted, err)
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

	_, ghostLaneID, ghostS0 := gatedLane(t, db, "GHOST", "") // unmarked: the hold is taken here
	ghostS1, err := db.GetNodeByDotName("GHOST-S1")
	if err != nil || ghostS1 == nil {
		t.Fatalf("read GHOST-S1: %v", err)
	}
	line := lineNode(t, db, "GHOST-LINE")

	t.Run("the ghost is refused", func(t *testing.T) {
		_, err := d.resolvePlanLaneHolds([]resolvedStep{
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
		// holderStillOwesTheLane's DeliveryNode arm covers it. It must still take
		// its one dig hold.
		holds, err := d.resolvePlanLaneHolds([]resolvedStep{
			{Action: protocol.ActionPickup, Node: ghostS0.Name},
			{Action: protocol.ActionDropoff, Node: ghostS1.Name},
		})
		if err != nil {
			t.Fatalf("an in-lane move was refused (%v). Its final destination IS the lane, so the "+
				"whole-visit hold is exactly right and the DeliveryNode arm releases it at the drop.", err)
		}
		if len(holds) != 1 || holds[0].laneID != ghostLaneID || holds[0].mode != reservations.ModeDig {
			t.Fatalf("in-lane move holds = %+v, want ONE dig hold on lane %d", holds, ghostLaneID)
		}
	})

	t.Run("a relay is NOT the ghost", func(t *testing.T) {
		// Drop into the lane, then pick back up from it later, finishing elsewhere.
		// The parked bin stands IN the lane between the two visits, claimed, where
		// legStillNeedsLane sees it — so the lane's own state carries this one and
		// there is nothing for a tripwire to say about it.
		holds, err := d.resolvePlanLaneHolds([]resolvedStep{
			{Action: protocol.ActionDropoff, Node: ghostS1.Name},
			{Action: protocol.ActionPickup, Node: ghostS1.Name},
			{Action: protocol.ActionDropoff, Node: line.Name},
		})
		if err != nil {
			t.Fatalf("a relay was refused (%v). Its bin is parked in the lane between visits, claimed, "+
				"which is the state the claim walk reads — the tripwire must not fire on it.", err)
		}
		if len(holds) != 1 || holds[0].mode != reservations.ModeDig {
			t.Fatalf("relay holds = %+v, want ONE dig hold (the stronger mode wins the dedupe)", holds)
		}
	})
}
