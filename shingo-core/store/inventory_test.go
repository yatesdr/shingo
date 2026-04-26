//go:build docker

package store

import (
	"testing"

	"shingocore/store/bins"
	"shingocore/store/inventory"
	"shingocore/store/nodes"
)

func TestListInventory_Empty(t *testing.T) {
	db := testDB(t)
	rows, err := db.ListInventory()
	if err != nil {
		t.Fatalf("ListInventory (empty): %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("empty DB inventory len = %d, want 0", len(rows))
	}
}

func TestListInventory(t *testing.T) {
	db := testDB(t)

	bt := &bins.BinType{Code: "INV-BT", Description: "inv tote"}
	db.CreateBinType(bt)

	nodeA := &nodes.Node{Name: "INV-NODE-A", Zone: "ZA", Enabled: true}
	db.CreateNode(nodeA)
	nodeB := &nodes.Node{Name: "INV-NODE-B", Zone: "ZB", Enabled: true}
	db.CreateNode(nodeB)

	// bins.Bin with manifest having two items — should produce 2 rows (one per cat_id)
	binFull := &bins.Bin{BinTypeID: bt.ID, Label: "INV-FULL", NodeID: &nodeA.ID, Status: "available"}
	db.CreateBin(binFull)
	db.SetBinManifest(binFull.ID,
		`{"items":[{"catid":"CAT-1","qty":4},{"catid":"CAT-2","qty":6}]}`,
		"PAY-I", 10)
	db.ConfirmBinManifest(binFull.ID, "")

	// bins.Bin with empty manifest items — should produce 1 row with cat_id=""
	binEmptyItems := &bins.Bin{BinTypeID: bt.ID, Label: "INV-EMPTY-ITEMS", NodeID: &nodeA.ID, Status: "available"}
	db.CreateBin(binEmptyItems)
	db.SetBinManifest(binEmptyItems.ID, `{"items":[]}`, "PAY-I", 0)

	// bins.Bin with no manifest at all — should produce 1 row
	binNoManifest := &bins.Bin{BinTypeID: bt.ID, Label: "INV-NO-MAN", NodeID: &nodeB.ID, Status: "available"}
	db.CreateBin(binNoManifest)

	rows, err := db.ListInventory()
	if err != nil {
		t.Fatalf("ListInventory: %v", err)
	}
	// Expected: 2 (binFull) + 1 (binEmptyItems) + 1 (binNoManifest) = 4.
	if len(rows) != 4 {
		t.Fatalf("inventory row count = %d, want 4", len(rows))
	}

	// Group rows by bin label + cat_id for easy lookup
	byKey := map[string]inventory.Row{}
	for _, r := range rows {
		byKey[r.BinLabel+"|"+r.CatID] = r
	}

	// Full bin — two rows
	r1, ok := byKey["INV-FULL|CAT-1"]
	if !ok {
		t.Fatal("expected INV-FULL|CAT-1 row")
	}
	if r1.Qty != 4 {
		t.Errorf("CAT-1 qty = %d, want 4", r1.Qty)
	}
	if r1.PayloadCode != "PAY-I" {
		t.Errorf("CAT-1 payload = %q", r1.PayloadCode)
	}
	if r1.NodeName != "INV-NODE-A" {
		t.Errorf("CAT-1 node = %q, want INV-NODE-A", r1.NodeName)
	}
	if r1.Zone != "ZA" {
		t.Errorf("CAT-1 zone = %q, want ZA", r1.Zone)
	}
	if !r1.Confirmed {
		t.Error("CAT-1 should be confirmed")
	}
	if r1.BinType != "INV-BT" {
		t.Errorf("CAT-1 bin type = %q, want INV-BT", r1.BinType)
	}

	r2, ok := byKey["INV-FULL|CAT-2"]
	if !ok {
		t.Fatal("expected INV-FULL|CAT-2 row")
	}
	if r2.Qty != 6 {
		t.Errorf("CAT-2 qty = %d, want 6", r2.Qty)
	}

	// Empty-items bin — one row, cat_id="", qty=0
	rE, ok := byKey["INV-EMPTY-ITEMS|"]
	if !ok {
		t.Fatal("expected INV-EMPTY-ITEMS with blank cat_id")
	}
	if rE.Qty != 0 {
		t.Errorf("empty-items qty = %d, want 0", rE.Qty)
	}
	// Still has payload_code because Set was called
	if rE.PayloadCode != "PAY-I" {
		t.Errorf("empty-items payload = %q, want PAY-I", rE.PayloadCode)
	}

	// No-manifest bin — one row, blank cat_id, blank payload_code
	rN, ok := byKey["INV-NO-MAN|"]
	if !ok {
		t.Fatal("expected INV-NO-MAN row")
	}
	if rN.Qty != 0 {
		t.Errorf("no-manifest qty = %d, want 0", rN.Qty)
	}
	if rN.PayloadCode != "" {
		t.Errorf("no-manifest payload = %q, want empty", rN.PayloadCode)
	}
	if rN.NodeName != "INV-NODE-B" {
		t.Errorf("no-manifest node = %q, want INV-NODE-B", rN.NodeName)
	}
}
