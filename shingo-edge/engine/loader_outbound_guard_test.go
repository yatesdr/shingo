package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/store"
)

// outboundOrder files a move LEAVING coreNode — the shape the guard must see.
func outboundOrder(t *testing.T, db *store.DB, nodeID int64, uuid, coreNode, dest string, status protocol.Status) int64 {
	t.Helper()
	id, err := db.CreateOrder(uuid, orders.TypeMove, &nodeID, false, 1,
		dest, "", coreNode, "", true, "STUD")
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
		"PLK_X2", "", "SYN_COMP", "", true, "STUD")
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

// TestCreateLoaderOutbound_FilesOnceForOneBin is the fix, stated as the
// behaviour it buys.
//
// Both L2 paths — the L1 completion and LoadBin's fallback — run against the
// same slot. The first files the move; the second must decline. On the
// lane-stress rig, without this, PLK_X2 took in 5 empties and produced 10
// outbound moves.
func TestCreateLoaderOutbound_FilesOnceForOneBin(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	e := testEngine(t, db)
	_, nodeID := seedProcessNode(t, db)

	id1, created1 := e.createLoaderOutbound(nodeID, "PLK_X2", "SYN_COMP", "STUD", "L1-completion")
	if !created1 || id1 == 0 {
		t.Fatal("the first outbound move was not filed — the loaded bin has nothing to carry it away")
	}

	id2, created2 := e.createLoaderOutbound(nodeID, "PLK_X2", "SYN_COMP", "STUD", "load-fallback")
	if created2 {
		t.Errorf("a SECOND outbound move (%d) was filed for the same bin. One of the two will "+
			"carry the bin off and the other will sit queued against an empty node reporting "+
			"finder-node-empty, which reads as a supply problem at the loader", id2)
	}

	moves, err := db.ListActiveOrdersByProcessNodeOrSource(nodeID, "PLK_X2")
	testutil.MustNoErr(t, err, "list active")
	n := 0
	for _, o := range moves {
		if o.OrderType == orders.TypeMove && o.SourceNode == "PLK_X2" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("loader owes %d outbound moves, want exactly 1 — one bin leaves a loader once", n)
	}
}

// TestCreateLoaderOutbound_NoDestinationFilesNothing — a loader with no
// OutboundDestination is misconfigured, not a reason to create a move with an
// empty destination.
func TestCreateLoaderOutbound_NoDestinationFilesNothing(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	e := testEngine(t, db)
	_, nodeID := seedProcessNode(t, db)

	if _, created := e.createLoaderOutbound(nodeID, "PLK_X2", "", "STUD", "L1-completion"); created {
		t.Error("filed an outbound move with no destination")
	}
}
