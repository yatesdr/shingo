//go:build docker

package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// TestBinMovePark_TheQueuedRowCarriesItsLaneCause is the third face of the
// blank-history defect, and the one that lives at a door rather than in the
// store.
//
// queueBinMoveForLane wrote the queue detail straight to the store and then
// called Queue(). The transition's historyReason reads the code off the
// IN-MEMORY order (dispatch/lifecycle.go), which the direct store write never
// touched — so the fresh `queued` row was born blank. The store write a moment
// earlier had nothing to land on either: the order was still `pending`, which is
// a birth certificate rather than a wait.
//
// The three setQueueReason helpers are the pattern the door was missing: they
// write the store AND the order together, so the code is on the struct before
// the transition that records it. This asserts the outcome — a person's parked
// move says on its own history row why it is waiting.
func TestBinMovePark_TheQueuedRowCarriesItsLaneCause(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, _ := setupTestData(t, db)

	// A two-deep lane with the move's bin at the back and a blocker in front of
	// it: the bin is named, the finder was never called, so the door's own
	// admission is the first thing to ask whether the move can happen — and the
	// answer is no, this bin is buried.
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
		Selection:  BinSelectionByLabel,
		BinLabel:   target.Label,
		DestNodeID: lineNode.ID,
		StationID:  "test-station",
		Desc:       "move the buried one",
	})
	testutil.MustNoErr(t, err, "create bin move")
	if !res.Queued {
		t.Fatalf("the move dispatched instead of parking (%+v) — the lane must refuse it or this "+
			"test exercises nothing", res)
	}

	// The park's own row, and only that row. The emit at the end of the park runs
	// the scanner synchronously, so this order goes on to have a life — what is
	// asserted here is what the DOOR wrote, at the moment it wrote it.
	history, herr := db.ListOrderHistory(res.OrderID)
	testutil.MustNoErr(t, herr, "list order history")
	var parked *orders.History
	for _, h := range history {
		if h.Status == protocol.StatusQueued {
			parked = h
			break
		}
	}
	if parked == nil {
		var got []string
		for _, h := range history {
			got = append(got, string(h.Status))
		}
		t.Fatalf("no queued row in the parked move's history; it has %v", got)
	}
	if parked.Code != string(protocol.QueueWaitingForSlot) {
		t.Errorf("the queued history row carries code %q, want %q.\n"+
			"orders.queue_code is overwritten in place, so this row is the only durable record of what "+
			"this move waited for. Born blank, the wait is gone the moment anything else touches the "+
			"order. Full history: %v",
			parked.Code, protocol.QueueWaitingForSlot, historyLine(history))
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
