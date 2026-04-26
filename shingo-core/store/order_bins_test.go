//go:build docker

package store

import (
	"testing"

	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

func TestOrderBinsCRUD(t *testing.T) {
	db := testDB(t)

	// Need an order + a bin (FK references)
	o := &orders.Order{EdgeUUID: "ob-uuid", Status: "pending"}
	if err := db.CreateOrder(o); err != nil {
		t.Fatalf("create order: %v", err)
	}

	bt := &bins.BinType{Code: "OB-BT", Description: "t"}
	db.CreateBinType(bt)

	node := &nodes.Node{Name: "OB-NODE", Enabled: true}
	db.CreateNode(node)

	b1 := &bins.Bin{BinTypeID: bt.ID, Label: "OB-B1", NodeID: &node.ID, Status: "available"}
	db.CreateBin(b1)
	b2 := &bins.Bin{BinTypeID: bt.ID, Label: "OB-B2", NodeID: &node.ID, Status: "available"}
	db.CreateBin(b2)
	b3 := &bins.Bin{BinTypeID: bt.ID, Label: "OB-B3", NodeID: &node.ID, Status: "available"}
	db.CreateBin(b3)

	// Insert rows in non-sorted step order — ListOrderBins must order them.
	if err := db.InsertOrderBin(o.ID, b2.ID, 1, "pick", "NODE-B", "DEST-B"); err != nil {
		t.Fatalf("insert 1: %v", err)
	}
	if err := db.InsertOrderBin(o.ID, b1.ID, 0, "pick", "NODE-A", "DEST-A"); err != nil {
		t.Fatalf("insert 0: %v", err)
	}
	if err := db.InsertOrderBin(o.ID, b3.ID, 2, "drop", "NODE-C", "DEST-C"); err != nil {
		t.Fatalf("insert 2: %v", err)
	}

	// ListOrderBins — ordered by step_index ascending
	list, err := db.ListOrderBins(o.ID)
	if err != nil {
		t.Fatalf("ListOrderBins: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("len = %d, want 3", len(list))
	}
	if list[0].StepIndex != 0 || list[1].StepIndex != 1 || list[2].StepIndex != 2 {
		t.Errorf("step order = [%d,%d,%d], want [0,1,2]",
			list[0].StepIndex, list[1].StepIndex, list[2].StepIndex)
	}
	if list[0].BinID != b1.ID || list[1].BinID != b2.ID || list[2].BinID != b3.ID {
		t.Errorf("bin order wrong: %d,%d,%d (want %d,%d,%d)",
			list[0].BinID, list[1].BinID, list[2].BinID,
			b1.ID, b2.ID, b3.ID)
	}
	if list[0].Action != "pick" {
		t.Errorf("list[0].Action = %q, want pick", list[0].Action)
	}
	if list[0].NodeName != "NODE-A" || list[0].DestNode != "DEST-A" {
		t.Errorf("list[0] names = (%q,%q), want (NODE-A,DEST-A)", list[0].NodeName, list[0].DestNode)
	}

	// DeleteOrderBins — removes them all
	db.DeleteOrderBins(o.ID)
	listAfter, err := db.ListOrderBins(o.ID)
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(listAfter) != 0 {
		t.Errorf("after delete len = %d, want 0", len(listAfter))
	}
}

func TestApplyMultiBinArrival(t *testing.T) {
	db := testDB(t)

	bt := &bins.BinType{Code: "ARR-BT", Description: "arrival tote"}
	db.CreateBinType(bt)

	startNode := &nodes.Node{Name: "ARR-START", Enabled: true}
	db.CreateNode(startNode)
	destA := &nodes.Node{Name: "ARR-DEST-A", Enabled: true}
	db.CreateNode(destA)
	destB := &nodes.Node{Name: "ARR-DEST-B", Enabled: true}
	db.CreateNode(destB)

	b1 := &bins.Bin{BinTypeID: bt.ID, Label: "ARR-1", NodeID: &startNode.ID, Status: "available"}
	db.CreateBin(b1)
	b2 := &bins.Bin{BinTypeID: bt.ID, Label: "ARR-2", NodeID: &startNode.ID, Status: "available"}
	db.CreateBin(b2)

	// Claim both for a synthetic order so ApplyMultiBinArrival has something to release.
	db.ClaimBin(b1.ID, 42)
	db.ClaimBin(b2.ID, 42)

	instrs := []orders.BinArrivalInstruction{
		{BinID: b1.ID, ToNodeID: destA.ID, Staged: false},
		{BinID: b2.ID, ToNodeID: destB.ID, Staged: true},
	}
	if err := db.ApplyMultiBinArrival(instrs); err != nil {
		t.Fatalf("ApplyMultiBinArrival: %v", err)
	}

	// b1 — moved, unclaimed, available (not staged)
	got1, _ := db.GetBin(b1.ID)
	if got1.NodeID == nil || *got1.NodeID != destA.ID {
		t.Errorf("b1 NodeID = %v, want %d", got1.NodeID, destA.ID)
	}
	if got1.ClaimedBy != nil {
		t.Errorf("b1 ClaimedBy = %v, want nil", got1.ClaimedBy)
	}
	if got1.Status != "available" {
		t.Errorf("b1 Status = %q, want available", got1.Status)
	}
	if got1.StagedAt != nil {
		t.Errorf("b1 StagedAt = %v, want nil", got1.StagedAt)
	}

	// b2 — moved, unclaimed, staged
	got2, _ := db.GetBin(b2.ID)
	if got2.NodeID == nil || *got2.NodeID != destB.ID {
		t.Errorf("b2 NodeID = %v, want %d", got2.NodeID, destB.ID)
	}
	if got2.ClaimedBy != nil {
		t.Errorf("b2 ClaimedBy = %v, want nil", got2.ClaimedBy)
	}
	if got2.Status != "staged" {
		t.Errorf("b2 Status = %q, want staged", got2.Status)
	}
	if got2.StagedAt == nil {
		t.Error("b2 StagedAt should be set when staged=true")
	}
}
