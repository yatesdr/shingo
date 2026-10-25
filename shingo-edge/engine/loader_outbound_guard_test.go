package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// outboundOrder files a move LEAVING coreNode — the shape the guard must see.
func outboundOrder(t *testing.T, db *store.DB, nodeID int64, uuid, coreNode, dest string, status protocol.Status) int64 {
	t.Helper()
	id, err := db.CreateOrder(uuid, orders.TypeMove, &nodeID, false, 1,
		dest, "", coreNode, protocol.Height{}, true, "STUD", "", "")
	testutil.MustNoErr(t, err, "create outbound "+uuid)
	testutil.MustNoErr(t, db.UpdateOrderStatus(id, string(status)), "set status "+string(status))
	return id
}

// TestOutboundMoveInFlight_SeesTheSlotsOwnMove is the guard's core question.
//
// One loader window is one physical slot holding one bin, so an outbound move
// already owed means the bin is spoken for. Missing this is what produced two
// orders per bin: the winner carried it away and the loser asked an empty node
// for material forever.
func TestOutboundMoveInFlight_SeesTheSlotsOwnMove(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	e := testEngine(t, db)
	_, nodeID := seedProcessNode(t, db)

	if e.outboundMoveInFlight(nodeID, "PLK_X2") {
		t.Fatal("an idle loader reported a move in flight — this blocks the L2 that should be created")
	}

	outboundOrder(t, db, nodeID, "u-out-1", "PLK_X2", "SYN_COMP", protocol.StatusQueued)

	if !e.outboundMoveInFlight(nodeID, "PLK_X2") {
		t.Error("a queued outbound move was not seen. A queued order is exactly the one that " +
			"matters here — it has not sourced yet, so a second order created alongside it is the " +
			"duplicate that never finds a bin")
	}
}

// TestOutboundMoveInFlight_IgnoresArrivals — a move DELIVERING to this node is
// somebody else's bin arriving. Counting it would make a loader that is being
// restocked look like one that already owes a move, and it would never ship.
func TestOutboundMoveInFlight_IgnoresArrivals(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	e := testEngine(t, db)
	_, nodeID := seedProcessNode(t, db)

	// delivery_node = this loader, source elsewhere: an inbound move.
	id, err := db.CreateOrder("u-in-1", orders.TypeMove, &nodeID, false, 1,
		"PLK_X2", "", "SYN_COMP", protocol.Height{}, true, "STUD", "", "")
	testutil.MustNoErr(t, err, "create inbound")
	testutil.MustNoErr(t, db.UpdateOrderStatus(id, string(protocol.StatusQueued)), "queue inbound")

	if e.outboundMoveInFlight(nodeID, "PLK_X2") {
		t.Error("an INBOUND move was counted as this slot's outbound. The guard would then " +
			"suppress the real outbound and the loaded bin would never leave")
	}
}

// TestOutboundMoveInFlight_TerminalDoesNotBlock — yesterday's completed move is
// not a claim on today's bin. A terminal order counting would wedge the loader
// permanently after its first cycle.
func TestOutboundMoveInFlight_TerminalDoesNotBlock(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	e := testEngine(t, db)
	_, nodeID := seedProcessNode(t, db)

	outboundOrder(t, db, nodeID, "u-done", "PLK_X2", "SYN_COMP", protocol.StatusConfirmed)

	if e.outboundMoveInFlight(nodeID, "PLK_X2") {
		t.Error("a confirmed move still blocked the slot — the loader would never ship again")
	}
}

// TestFileLoadedOutbound_FilesOnceForOneBin: a second LOAD on the same bin (a
// double tap, or a stale board) finds the move the first one filed and files
// nothing. The order layer has no dedup for move orders; this guard is it.
func TestFileLoadedOutbound_FilesOnceForOneBin(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	e := testEngine(t, db)
	_, nodeID := seedProcessNode(t, db)
	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "load node")
	claim := &processes.NodeClaim{OutboundDestination: "SYN_COMP"}

	e.fileLoadedOutbound(node, claim, "STUD", 0)
	e.fileLoadedOutbound(node, claim, "STUD", 0)

	moves, err := db.ListActiveOrdersByProcessNodeOrSource(nodeID, node.CoreNodeName)
	testutil.MustNoErr(t, err, "list active")
	n := 0
	for _, o := range moves {
		if o.OrderType == orders.TypeMove && o.SourceNode == node.CoreNodeName {
			n++
		}
	}
	if n != 1 {
		t.Errorf("loader owes %d outbound moves after two LOADs, want exactly 1 — one bin leaves a loader "+
			"once; the second would sit queued against an empty node reporting finder-node-empty", n)
	}
}

// TestFileLoadedOutbound_NoDestinationFilesNothing — a loader with no
// OutboundDestination is misconfigured, not a reason to create a move with an
// empty destination.
func TestFileLoadedOutbound_NoDestinationFilesNothing(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	e := testEngine(t, db)
	_, nodeID := seedProcessNode(t, db)
	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "load node")

	e.fileLoadedOutbound(node, &processes.NodeClaim{}, "STUD", 0)
	if e.outboundMoveInFlight(nodeID, node.CoreNodeName) {
		t.Error("filed an outbound move with no destination")
	}
}
