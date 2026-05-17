//go:build docker

package inventory_test

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/inventory"
	"shingocore/store/nodes"
)

func TestCoverage_ListInventory_Empty(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	rows, err := inventory.List(db.DB)
	if err != nil { t.Fatalf("List (empty): %v", err) }
	if len(rows) != 0 { t.Errorf("empty DB inventory len = %d, want 0", len(rows)) }
}

func TestCoverage_ListInventory(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	bt := &bins.BinType{Code: "INV-BT", Description: "inv tote"}
	bins.CreateType(db.DB, bt)
	nodeA := &nodes.Node{Name: "INV-NODE-A", Zone: "ZA", Enabled: true}
	nodes.Create(db.DB, nodeA)
	nodeB := &nodes.Node{Name: "INV-NODE-B", Zone: "ZB", Enabled: true}
	nodes.Create(db.DB, nodeB)
	binFull := &bins.Bin{BinTypeID: bt.ID, Label: "INV-FULL", NodeID: &nodeA.ID, Status: "available"}
	bins.Create(db.DB, binFull)
	bins.SetManifest(db.DB, binFull.ID, `{"items":[{"catid":"CAT-1","qty":4},{"catid":"CAT-2","qty":6}]}`, "PAY-I", 10)
	bins.ConfirmManifest(db.DB, binFull.ID, "")
	binEmptyItems := &bins.Bin{BinTypeID: bt.ID, Label: "INV-EMPTY-ITEMS", NodeID: &nodeA.ID, Status: "available"}
	bins.Create(db.DB, binEmptyItems)
	bins.SetManifest(db.DB, binEmptyItems.ID, `{"items":[]}`, "PAY-I", 0)
	binNoManifest := &bins.Bin{BinTypeID: bt.ID, Label: "INV-NO-MAN", NodeID: &nodeB.ID, Status: "available"}
	bins.Create(db.DB, binNoManifest)
	rows, err := inventory.List(db.DB)
	if err != nil { t.Fatalf("List: %v", err) }
	if len(rows) != 4 { t.Fatalf("inventory row count = %d, want 4", len(rows)) }
	byKey := map[string]inventory.Row{}
	for _, r := range rows { byKey[r.BinLabel+"|"+r.CatID] = r }
	r1, ok := byKey["INV-FULL|CAT-1"]
	if !ok { t.Fatal("expected INV-FULL|CAT-1 row") }
	if r1.Qty != 4 { t.Errorf("CAT-1 qty = %d, want 4", r1.Qty) }
	if r1.PayloadCode != "PAY-I" { t.Errorf("CAT-1 payload = %q", r1.PayloadCode) }
	if r1.NodeName != "INV-NODE-A" { t.Errorf("CAT-1 node = %q, want INV-NODE-A", r1.NodeName) }
	if r1.Zone != "ZA" { t.Errorf("CAT-1 zone = %q, want ZA", r1.Zone) }
	if !r1.Confirmed { t.Error("CAT-1 should be confirmed") }
	if r1.BinType != "INV-BT" { t.Errorf("CAT-1 bin type = %q, want INV-BT", r1.BinType) }
	r2, ok := byKey["INV-FULL|CAT-2"]
	if !ok { t.Fatal("expected INV-FULL|CAT-2 row") }
	if r2.Qty != 6 { t.Errorf("CAT-2 qty = %d, want 6", r2.Qty) }
	rE, ok := byKey["INV-EMPTY-ITEMS|"]
	if !ok { t.Fatal("expected INV-EMPTY-ITEMS with blank cat_id") }
	if rE.Qty != 0 { t.Errorf("empty-items qty = %d, want 0", rE.Qty) }
	if rE.PayloadCode != "PAY-I" { t.Errorf("empty-items payload = %q, want PAY-I", rE.PayloadCode) }
	rN, ok := byKey["INV-NO-MAN|"]
	if !ok { t.Fatal("expected INV-NO-MAN row") }
	if rN.Qty != 0 { t.Errorf("no-manifest qty = %d, want 0", rN.Qty) }
	if rN.PayloadCode != "" { t.Errorf("no-manifest payload = %q, want empty", rN.PayloadCode) }
	if rN.NodeName != "INV-NODE-B" { t.Errorf("no-manifest node = %q, want INV-NODE-B", rN.NodeName) }
}
