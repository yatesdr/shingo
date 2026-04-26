//go:build docker

package store

import (
	"testing"

	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

func TestRepairConfirmedOrderCompletion(t *testing.T) {
	db := testDB(t)

	origin := &nodes.Node{Name: "ORIGIN", Enabled: true}
	dest := &nodes.Node{Name: "DEST", Enabled: true}
	if err := db.CreateNode(origin); err != nil {
		t.Fatalf("create origin node: %v", err)
	}
	if err := db.CreateNode(dest); err != nil {
		t.Fatalf("create dest node: %v", err)
	}
	bt := &bins.BinType{Code: "TOTE"}
	if err := db.CreateBinType(bt); err != nil {
		t.Fatalf("create bin type: %v", err)
	}
	bin := &bins.Bin{BinTypeID: bt.ID, Label: "BIN-1", NodeID: &origin.ID, Status: "available"}
	if err := db.CreateBin(bin); err != nil {
		t.Fatalf("create bin: %v", err)
	}
	order := &orders.Order{
		EdgeUUID:     "repair-order-1",
		StationID:    "edge.1",
		OrderType:    "retrieve",
		Status:       "confirmed",
		SourceNode:   origin.Name,
		DeliveryNode: dest.Name,
		BinID:        &bin.ID,
	}
	if err := db.CreateOrder(order); err != nil {
		t.Fatalf("create order: %v", err)
	}
	if err := db.UpdateOrderStatus(order.ID, "confirmed", "simulated"); err != nil {
		t.Fatalf("confirm order: %v", err)
	}
	if err := db.ClaimBin(bin.ID, order.ID); err != nil {
		t.Fatalf("claim bin: %v", err)
	}

	if err := db.RepairConfirmedOrderCompletion(order.ID, bin.ID, dest.ID, true, nil); err != nil {
		t.Fatalf("repair completion: %v", err)
	}

	gotOrder, err := db.GetOrder(order.ID)
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if gotOrder.CompletedAt == nil {
		t.Fatalf("expected completed_at to be set")
	}

	gotBin, err := db.GetBin(bin.ID)
	if err != nil {
		t.Fatalf("get bin: %v", err)
	}
	if gotBin.NodeID == nil || *gotBin.NodeID != dest.ID {
		t.Fatalf("expected bin at node %d, got %+v", dest.ID, gotBin.NodeID)
	}
	if gotBin.ClaimedBy != nil {
		t.Fatalf("expected bin claim to be released")
	}
	if gotBin.Status != "staged" {
		t.Fatalf("expected staged status, got %q", gotBin.Status)
	}
}

func TestReleaseTerminalBinClaimRejectsActiveOrder(t *testing.T) {
	db := testDB(t)

	node := &nodes.Node{Name: "NODE-A", Enabled: true}
	if err := db.CreateNode(node); err != nil {
		t.Fatalf("create node: %v", err)
	}
	bt := &bins.BinType{Code: "TOTE-A"}
	if err := db.CreateBinType(bt); err != nil {
		t.Fatalf("create bin type: %v", err)
	}
	bin := &bins.Bin{BinTypeID: bt.ID, Label: "BIN-A", NodeID: &node.ID, Status: "available"}
	if err := db.CreateBin(bin); err != nil {
		t.Fatalf("create bin: %v", err)
	}
	order := &orders.Order{
		EdgeUUID:   "active-order",
		StationID:  "edge.1",
		OrderType:  "retrieve",
		Status:     "dispatched",
		SourceNode: node.Name,
		BinID:      &bin.ID,
	}
	if err := db.CreateOrder(order); err != nil {
		t.Fatalf("create order: %v", err)
	}
	if err := db.ClaimBin(bin.ID, order.ID); err != nil {
		t.Fatalf("claim bin: %v", err)
	}

	if _, err := db.ReleaseTerminalBinClaim(bin.ID); err == nil {
		t.Fatalf("expected active claim release to fail")
	}
}

func TestReleaseTerminalBinClaimAllowsCancelledOrder(t *testing.T) {
	db := testDB(t)

	node := &nodes.Node{Name: "NODE-B", Enabled: true}
	if err := db.CreateNode(node); err != nil {
		t.Fatalf("create node: %v", err)
	}
	bt := &bins.BinType{Code: "TOTE-B"}
	if err := db.CreateBinType(bt); err != nil {
		t.Fatalf("create bin type: %v", err)
	}
	bin := &bins.Bin{BinTypeID: bt.ID, Label: "BIN-B", NodeID: &node.ID, Status: "available"}
	if err := db.CreateBin(bin); err != nil {
		t.Fatalf("create bin: %v", err)
	}
	order := &orders.Order{
		EdgeUUID:   "cancelled-order",
		StationID:  "edge.1",
		OrderType:  "retrieve",
		Status:     "cancelled",
		SourceNode: node.Name,
		BinID:      &bin.ID,
	}
	if err := db.CreateOrder(order); err != nil {
		t.Fatalf("create order: %v", err)
	}
	if err := db.UpdateOrderStatus(order.ID, "cancelled", "simulated"); err != nil {
		t.Fatalf("cancel order: %v", err)
	}
	if err := db.ClaimBin(bin.ID, order.ID); err != nil {
		t.Fatalf("claim bin: %v", err)
	}

	gotOrderID, err := db.ReleaseTerminalBinClaim(bin.ID)
	if err != nil {
		t.Fatalf("release terminal claim: %v", err)
	}
	if gotOrderID != order.ID {
		t.Fatalf("expected order id %d, got %d", order.ID, gotOrderID)
	}

	gotBin, err := db.GetBin(bin.ID)
	if err != nil {
		t.Fatalf("get bin: %v", err)
	}
	if gotBin.ClaimedBy != nil {
		t.Fatalf("expected claim to be cleared")
	}
}
