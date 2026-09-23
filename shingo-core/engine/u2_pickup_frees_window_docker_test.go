//go:build docker

package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
)

// u2_pickup_frees_window_docker_test.go — what Core's node-bins read says about
// an unloader window between its empty-out's (U2's) PICKUP and its LANDING.
//
// The Edge can re-pull the window's next full the moment the U2 lifts the
// carrier only if Core stops counting the carrier resident then. The reader is
// www apiTelemetryNodeBins, which answers occupied from NodeService
// GetByDotName + ListBinsByNode — the same two calls asserted here.
//
// Core's pickup block handler (handlePickupBlockCompleted) moves the claimed bin
// to the synthetic _TRANSIT node BEFORE it sends BinPickedUp to the Edge, so by
// the time the Edge hears of the pickup the window reads empty.
func TestU2Pickup_WindowReadsEmptyBeforeTheLanding(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	testdb.SetupStandardData(t, db)
	eng := newTestEngine(t, db, simulator.New())
	window := rebindNode(t, "U2P-WINDOW", db.CreateNode)
	empties := rebindNode(t, "U2P-EMPTIES", db.CreateNode)
	carrier := testdb.CreateBinAtNode(t, db, "", window.ID, "U2P-CARRIER")

	// The U2 exactly as the Edge sends it: a move, from the window, naming no part.
	eng.Dispatcher().HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID: "u2p-move", OrderType: dispatch.OrderTypeMove,
		SourceNode: window.Name, DeliveryNode: empties.Name, Quantity: 1,
	})
	u2 := testdb.RequireOrder(t, db, "u2p-move")
	if u2.BinID == nil || *u2.BinID != carrier.ID {
		t.Fatalf("fixture: the U2 claimed bin %v, want the carrier %d (status %q)", u2.BinID, carrier.ID, u2.Status)
	}

	nodes := eng.NodeService()
	resident := func() int {
		n, err := nodes.GetByDotName(window.Name)
		testutil.MustNoErr(t, err, "read the window")
		list, err := nodes.ListBinsByNode(n.ID)
		testutil.MustNoErr(t, err, "list bins at the window")
		return len(list)
	}
	if got := resident(); got != 1 {
		t.Fatalf("before pickup the window holds %d bin(s), want the carrier", got)
	}

	testutil.MustNoErr(t, db.UpdateOrderStatus(u2.ID, string(dispatch.StatusInTransit), "robot moving"), "in transit")
	eng.handlePickupBlockCompleted(BlockCompletedEvent{
		OrderID: u2.ID, BlockID: "u2p-b1", Location: window.Name, BinTask: "JackLoad",
	})

	if got := resident(); got != 0 {
		t.Errorf("after the pickup block the window holds %d bin(s), want 0 — node-bins would still say "+
			"occupied and an Edge re-pull at pickup would read to_fire=0", got)
	}
	moved, err := db.GetBin(carrier.ID)
	testutil.MustNoErr(t, err, "reload the carrier")
	if moved.NodeID == nil || *moved.NodeID == window.ID {
		t.Errorf("the carrier is at node %v after pickup, want the transit node", moved.NodeID)
	}
}
