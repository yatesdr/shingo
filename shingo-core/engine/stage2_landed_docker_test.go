//go:build docker

package engine

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// TestStage2Pull_LandedMoveConfirmsAndFreesItsClaim: a core-s2- move sets a
// bare cart down on an empty stage-2 window. There is no human receipt to wait
// for — the Edge's own U2/L2 moves are autoConfirm for the same reason — and a
// delivered order keeps its bin's claim, so until it confirms the stage-2 U2
// cannot source the cart and the window counts in flight. Core confirms it the
// moment it is delivered, and the claim goes with it.
func TestStage2Pull_LandedMoveConfirmsAndFreesItsClaim(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, _, _ := setupTestData(t, db)
	win := &nodes.Node{Name: "S2L-WIN", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(win), "window")
	bin := createTestBinAtNode(t, db, "", storageNode.ID, "BIN-S2L")
	sim := simulator.New()
	eng := newTestEngine(t, db, sim)

	res, err := eng.CreateBinMove(BinMoveRequest{
		Selection: BinSelectionByLabel, BinLabel: bin.Label, DestNodeID: win.ID,
		StationID: "test-station", EdgeUUID: "core-s2-landed",
	})
	testutil.MustNoErr(t, err, "core-s2 move")
	order, err := db.GetOrder(res.OrderID)
	testutil.MustNoErr(t, err, "order")
	sim.DriveSimpleLifecycle(order.VendorOrderID)

	after := awaitOrderStatus(t, db, res.OrderID, dispatch.StatusConfirmed)
	if after.Status != dispatch.StatusConfirmed {
		t.Errorf("core-s2 move status = %s after landing, want confirmed: nobody else will confirm it for up to ten minutes", after.Status)
	}
	got, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "bin after")
	if got.NodeID == nil || *got.NodeID != win.ID {
		t.Errorf("cart at %v, want the window %d", got.NodeID, win.ID)
	}
	if got.ClaimedBy != nil {
		t.Errorf("cart still claimed by order %d after landing: stage 2's U2 cannot source it", *got.ClaimedBy)
	}
}

// awaitOrderStatus polls an order until it reaches want or a few seconds pass:
// Core confirms a landed pull on the pull's tracked goroutine, after the
// delivered event's subscriber chain.
func awaitOrderStatus(t *testing.T, db *store.DB, orderID int64, want protocol.Status) *orders.Order {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		o, err := db.GetOrder(orderID)
		testutil.MustNoErr(t, err, "order")
		if o.Status == want || time.Now().After(deadline) {
			return o
		}
		time.Sleep(20 * time.Millisecond)
	}
}
