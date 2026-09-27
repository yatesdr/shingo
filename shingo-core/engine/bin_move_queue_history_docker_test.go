//go:build docker

package engine

import (
	"errors"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// laneWithSlot builds an unmarked lane in a group, with one slot at the mouth.
// (It lived in the carried-bin lane-park test, which cancel-return removed.)
//
// THE GROUP IS NOT DECORATION. resolveOrderLaneHolds takes no mouth hold when
// `lane.ParentID == nil` ("a lane with no group — no hold"), so a parentless lane
// cannot be held and this test would silently exercise nothing.
func laneWithSlot(t *testing.T, db *store.DB, prefix string) (lane, slot *nodes.Node) {
	t.Helper()
	ngrpType, err := db.GetNodeTypeByCode(protocol.NodeClassNGRP)
	testutil.MustNoErr(t, err, "get NGRP type")
	laneType, err := db.GetNodeTypeByCode(protocol.NodeClassLANE)
	testutil.MustNoErr(t, err, "get LANE type")

	grp := &nodes.Node{Name: prefix + "-GRP", NodeTypeID: &ngrpType.ID, Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(grp), "create lane group")
	lane = &nodes.Node{Name: prefix + "-LANE", NodeTypeID: &laneType.ID, ParentID: &grp.ID,
		Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(lane), "create lane")
	d0 := 0
	slot = &nodes.Node{Name: prefix + "-LANE-S0", ParentID: &lane.ID, Enabled: true, Depth: &d0}
	testutil.MustNoErr(t, db.CreateNode(slot), "create slot")
	return lane, slot
}

// TestBinMovePark_AHistoryRowCarriesItsLaneCause: a person's move that a held
// lane refuses waits, and says on a history row why.
//
// orders.queue_code is overwritten in place, so the history row is the only
// durable record of what a person's move waited for. Since B2 the move is
// parked by acquiring's lane arm (Scanner.admitLanes), which writes the cause
// and moves the order into `sourcing`, the waiting set's soft-holding rung, so
// that row carries it; the lane wait is filed in acquiring's vocabulary,
// storage_rearranging (the old door filed it as a slot wait).
func TestBinMovePark_AHistoryRowCarriesItsLaneCause(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	eng := newTestEngine(t, db, simulator.New())

	lane, slot := laneWithSlot(t, db, "BMQH")
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, slot.ID, "BMQH-BIN")

	// Somebody else is driving INTO the corridor, and the move takes a bin OUT:
	// two modes, so the lane refuses the move (only an exact same-mode share is
	// admitted). Not a dig: the move's own soft hold refuses a bin inside a lane
	// being dug before acquiring is ever asked (reservations' dig arm). In
	// transit, so the move's own acquire is the only thing asked of the lane.
	elsewhere := &nodes.Node{Name: "BMQH-ELSEWHERE", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(elsewhere), "create the blocker's destination")
	blocker := &orders.Order{
		EdgeUUID: "bmqh-blocker", StationID: "edge.test", OrderType: "move",
		Status: protocol.StatusInTransit, Quantity: 1, DeliveryNode: elsewhere.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(blocker), "create blocking order")
	testutil.MustNoErr(t, reservations.AcquireLanes(db.DB, blocker.ID, reservations.ModeInbound,
		"test-blocker", lane.ID), "blocker takes the lane inbound")

	res, err := eng.CreateBinMove(BinMoveRequest{
		Selection: BinSelectionByLabel, BinLabel: bin.Label, DestNodeID: sd.LineNode.ID, StationID: "test-station",
	})
	testutil.MustNoErr(t, err, "create bin move")
	if !res.Queued {
		t.Fatalf("the move dispatched instead of parking (%+v): the held lane must refuse it", res)
	}
	history, err := db.ListOrderHistory(res.OrderID)
	testutil.MustNoErr(t, err, "list order history")
	found := false
	for _, h := range history {
		if h.Code == string(protocol.QueueStorageRearranging) && protocol.IsAcquiring(h.Status) {
			found = true
		}
	}
	if !found {
		t.Errorf("no waiting row carries %q. Full history: %v", protocol.QueueStorageRearranging, historyLine(history))
	}
}

// TestBinMove_NowhereToParkABlockerIsRefusedAtOnce: a named bin buried behind a
// blocker in a lane with no group has nowhere to park the blocker, and the move
// is refused in the same call. The old door answered "queued" while the
// synchronous scanner pass it triggered failed the order underneath; the
// refusal is the truthful answer (the orc's ruling, 2026-09-26; behaviour list).
func TestBinMove_NowhereToParkABlockerIsRefusedAtOnce(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, _ := setupTestData(t, db)

	laneType, err := db.GetNodeTypeByCode(protocol.NodeClassLANE)
	testutil.MustNoErr(t, err, "get LANE type")
	lane := &nodes.Node{Name: "BMQH-L1", NodeTypeID: &laneType.ID, Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(lane), "create lane")
	d1, d2 := 1, 2
	front := &nodes.Node{Name: "BMQH-L1-S1", ParentID: &lane.ID, Enabled: true, Depth: &d1}
	testutil.MustNoErr(t, db.CreateNode(front), "create front slot")
	back := &nodes.Node{Name: "BMQH-L1-S2", ParentID: &lane.ID, Enabled: true, Depth: &d2}
	testutil.MustNoErr(t, db.CreateNode(back), "create back slot")
	createTestBinAtNode(t, db, "PART-A", front.ID, "BMQH-BLOCKER")
	target := createTestBinAtNode(t, db, "PART-A", back.ID, "BMQH-TARGET")

	eng := newTestEngine(t, db, simulator.New())
	res, err := eng.CreateBinMove(BinMoveRequest{
		Selection: BinSelectionByLabel, BinLabel: target.Label, DestNodeID: lineNode.ID, StationID: "test-station",
	})
	if err == nil {
		t.Fatalf("the move was answered %+v: a buried bin with nowhere to park its blocker cannot move", res)
	}
	var bmErr *BinMoveError
	if !errors.As(err, &bmErr) || bmErr.Kind != BinMoveFault {
		t.Errorf("err = %v, want the door's fault refusal carrying the reason", err)
	}
}

// historyLine renders a history for a failure message: status/code per row.
func historyLine(rows []*orders.History) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, string(r.Status)+"/"+r.Code)
	}
	return out
}
