//go:build docker

package inventory_test

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/inventory"
	"shingocore/store/nodes"
	"shingocore/store/payloads"
)

func TestCoverage_ListInventory_Empty(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	rows, err := inventory.List(db.DB)
	if err != nil {
		t.Fatalf("List (empty): %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("empty DB inventory len = %d, want 0", len(rows))
	}
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
	// The listing's qty is DERIVED — uop_remaining x the template's
	// parts_per_cycle — so the fixture needs a template. Without one the
	// manifest names parts nobody can count and every qty is 0, which is what
	// this test asserted against before the derivation landed.
	pay := &payloads.Payload{Code: "PAY-I", UOPCapacity: 10}
	payloads.Create(db.DB, pay)
	payloads.CreateItem(db.DB, &payloads.ManifestItem{PayloadID: pay.ID, PartNumber: "CAT-1", PartsPerCycle: 2}, "")
	payloads.CreateItem(db.DB, &payloads.ManifestItem{PayloadID: pay.ID, PartNumber: "CAT-2", PartsPerCycle: 3}, "")

	binFull := &bins.Bin{BinTypeID: bt.ID, Label: "INV-FULL", NodeID: &nodeA.ID, Status: "available"}
	bins.Create(db.DB, binFull)
	bins.SetManifest(db.DB, binFull.ID, `{"items":[{"catid":"CAT-1"},{"catid":"CAT-2"}]}`, "PAY-I", 10)
	bins.ConfirmManifest(db.DB, binFull.ID, "")
	binEmptyItems := &bins.Bin{BinTypeID: bt.ID, Label: "INV-EMPTY-ITEMS", NodeID: &nodeA.ID, Status: "available"}
	bins.Create(db.DB, binEmptyItems)
	bins.SetManifest(db.DB, binEmptyItems.ID, `{"items":[]}`, "PAY-I", 0)
	binNoManifest := &bins.Bin{BinTypeID: bt.ID, Label: "INV-NO-MAN", NodeID: &nodeB.ID, Status: "available"}
	bins.Create(db.DB, binNoManifest)
	rows, err := inventory.List(db.DB)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("inventory row count = %d, want 4", len(rows))
	}
	byKey := map[string]inventory.Row{}
	for _, r := range rows {
		byKey[r.BinLabel+"|"+r.PartNumber] = r
	}
	r1, ok := byKey["INV-FULL|CAT-1"]
	if !ok {
		t.Fatal("expected INV-FULL|CAT-1 row")
	}
	if r1.Qty != 20 {
		t.Errorf("CAT-1 qty = %d, want 20 (10 cycles x 2 per cycle)", r1.Qty)
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
	if r2.Qty != 30 {
		t.Errorf("CAT-2 qty = %d, want 30 (10 cycles x 3 per cycle)", r2.Qty)
	}
	rE, ok := byKey["INV-EMPTY-ITEMS|"]
	if !ok {
		t.Fatal("expected INV-EMPTY-ITEMS with blank cat_id")
	}
	if rE.Qty != 0 {
		t.Errorf("empty-items qty = %d, want 0", rE.Qty)
	}
	if rE.PayloadCode != "PAY-I" {
		t.Errorf("empty-items payload = %q, want PAY-I", rE.PayloadCode)
	}
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

// TestListInventory_QtyFollowsUOPNotTheManifest is the property the derivation
// exists for: draw the bin down and the listing follows, with no manifest
// rewrite anywhere. The stored qty this replaced could not — nothing rewrote a
// bin's manifest as production consumed it, so a bin at 1 of 10 listed as 10.
//
// It also covers the other half: a manifest line the template has no ratio for
// reads 0 rather than dropping the bin out of the listing.
func TestListInventory_QtyFollowsUOPNotTheManifest(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	bt := &bins.BinType{Code: "DRAIN-BT"}
	bins.CreateType(db.DB, bt)
	node := &nodes.Node{Name: "DRAIN-NODE", Enabled: true}
	nodes.Create(db.DB, node)

	pay := &payloads.Payload{Code: "PAY-DRAIN", UOPCapacity: 10}
	payloads.Create(db.DB, pay)
	payloads.CreateItem(db.DB, &payloads.ManifestItem{PayloadID: pay.ID, PartNumber: "KNOWN", PartsPerCycle: 3}, "")

	b := &bins.Bin{BinTypeID: bt.ID, Label: "DRAIN-BIN", NodeID: &node.ID, Status: "available"}
	bins.Create(db.DB, b)
	// ORPHAN is in the carrier but not in the template — no ratio, no count.
	bins.SetManifest(db.DB, b.ID, `{"items":[{"catid":"KNOWN"},{"catid":"ORPHAN"}]}`, "PAY-DRAIN", 10)

	qtyOf := func(catID string) int64 {
		t.Helper()
		rows, err := inventory.List(db.DB)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, r := range rows {
			if r.BinLabel == "DRAIN-BIN" && r.PartNumber == catID {
				return r.Qty
			}
		}
		t.Fatalf("no row for DRAIN-BIN|%s", catID)
		return 0
	}

	if got := qtyOf("KNOWN"); got != 30 {
		t.Errorf("full bin KNOWN qty = %d, want 30 (10 x 3)", got)
	}
	if got := qtyOf("ORPHAN"); got != 0 {
		t.Errorf("untemplated part qty = %d, want 0 — there is no ratio to count it by", got)
	}

	// Production draws the bin down. Nothing touches the manifest.
	if _, err := db.Exec(`UPDATE bins SET uop_remaining = 4 WHERE id = $1`, b.ID); err != nil {
		t.Fatalf("draw down: %v", err)
	}
	if got := qtyOf("KNOWN"); got != 12 {
		t.Errorf("drawn-down KNOWN qty = %d, want 12 (4 x 3) — the count did not follow uop_remaining", got)
	}
}
