//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// TestDispatchDirect_ConfiguredPayload_ExpandsLoadAndKeepsKeyRouteEmpty proves
// the two F4c features compose on one real dispatched order: the LOAD leg expands
// to the four same-location named blocks (the evidence-doc Postman shape) AND
// keyRoute stays empty — shingo never populates it, so the two top-level concerns
// don't interact. Complete stays true (single-shot).
func TestDispatchDirect_ConfiguredPayload_ExpandsLoadAndKeepsKeyRouteEmpty(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, payload := setupTestData(t, db)

	// Configure the payload with the seeded child-cart sequence.
	payload.AdvancedLoadSequence = "Child cart interlock"
	testutil.MustNoErr(t, db.UpdatePayload(payload), "configure payload sequence")

	backend := testdb.NewTrackingBackend()
	d, _ := newTestDispatcher(t, db, backend)

	order := &orders.Order{
		EdgeUUID:     "cart-1",
		StationID:    "edge.line1",
		OrderType:    OrderTypeRetrieve,
		Status:       StatusPending,
		Quantity:     1,
		PayloadCode:  payload.Code,
		DeliveryNode: lineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")
	testutil.MustNoErr(t, db.UpdateOrderStatus(order.ID, string(StatusPending), "fixture"), "pending")
	order.Status = StatusPending

	if _, err := d.DispatchDirect(order, storageNode, lineNode); err != nil {
		t.Fatalf("DispatchDirect: %v", err)
	}

	reqs := backend.CreateRequests()
	if len(reqs) != 1 {
		t.Fatalf("create requests = %d, want 1", len(reqs))
	}
	req := reqs[0]

	if len(req.KeyRoute) != 0 {
		t.Errorf("KeyRoute = %v, want empty (shingo leaves it unset)", req.KeyRoute)
	}
	if !req.Complete {
		t.Error("Complete = false, want true (single-shot)")
	}

	// Four same-location named load blocks + one delivery block.
	if len(req.Blocks) != 5 {
		t.Fatalf("blocks = %d, want 5 (4 load + 1 deliver): %+v", len(req.Blocks), req.Blocks)
	}
	wantLoad := []string{"Go_AP1", "Spin_90", "load", "Spin_inverse_90"}
	for i, name := range wantLoad {
		if req.Blocks[i].BinTask != name {
			t.Errorf("block[%d] binTask = %q, want %q", i, req.Blocks[i].BinTask, name)
		}
		if req.Blocks[i].Location != storageNode.Name {
			t.Errorf("block[%d] location = %q, want %q (the load location)", i, req.Blocks[i].Location, storageNode.Name)
		}
	}
	if req.Blocks[4].BinTask != "JackUnload" || req.Blocks[4].Location != lineNode.Name {
		t.Errorf("deliver block = {%s %s}, want {JackUnload %s}",
			req.Blocks[4].BinTask, req.Blocks[4].Location, lineNode.Name)
	}
	seen := map[string]bool{}
	for _, b := range req.Blocks {
		if seen[b.BlockID] {
			t.Errorf("duplicate blockId %q", b.BlockID)
		}
		seen[b.BlockID] = true
	}
}

// TestDispatchDirect_UnconfiguredPayload_SingleLoadBlock proves the unconfigured
// path is unchanged: a payload with no sequence emits the classic 2-block
// [JackLoad, JackUnload] order.
func TestDispatchDirect_UnconfiguredPayload_SingleLoadBlock(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, payload := setupTestData(t, db)
	_ = payload // left unconfigured (AdvancedLoadSequence empty)

	backend := testdb.NewTrackingBackend()
	d, _ := newTestDispatcher(t, db, backend)

	order := &orders.Order{
		EdgeUUID:     "plain-1",
		StationID:    "edge.line1",
		OrderType:    OrderTypeRetrieve,
		Status:       StatusPending,
		Quantity:     1,
		PayloadCode:  payload.Code,
		DeliveryNode: lineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")
	testutil.MustNoErr(t, db.UpdateOrderStatus(order.ID, string(StatusPending), "fixture"), "pending")
	order.Status = StatusPending

	if _, err := d.DispatchDirect(order, storageNode, lineNode); err != nil {
		t.Fatalf("DispatchDirect: %v", err)
	}
	req := backend.CreateRequests()[0]
	if len(req.Blocks) != 2 {
		t.Fatalf("blocks = %d, want 2 (unchanged path): %+v", len(req.Blocks), req.Blocks)
	}
	if req.Blocks[0].BinTask != "JackLoad" || req.Blocks[1].BinTask != "JackUnload" {
		t.Errorf("blocks = [%s, %s], want [JackLoad, JackUnload]",
			req.Blocks[0].BinTask, req.Blocks[1].BinTask)
	}
}
