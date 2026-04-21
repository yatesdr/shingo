//go:build docker

package store

import "testing"

func TestBinManifestSetConfirmClearGet(t *testing.T) {
	db := testDB(t)

	bt := &BinType{Code: "BM-BT", Description: "bm tote"}
	db.CreateBinType(bt)

	node := &Node{Name: "BM-NODE", Enabled: true}
	db.CreateNode(node)

	bin := &Bin{BinTypeID: bt.ID, Label: "BM-B1", NodeID: &node.ID, Status: "available"}
	db.CreateBin(bin)

	// GetBinManifest on a bin with no manifest — should return an empty, non-nil Manifest
	mMiss, err := db.GetBinManifest(bin.ID)
	if err != nil {
		t.Fatalf("GetBinManifest (empty): %v", err)
	}
	if mMiss == nil {
		t.Fatal("empty manifest should be non-nil")
	}
	if len(mMiss.Items) != 0 {
		t.Errorf("empty manifest Items len = %d, want 0", len(mMiss.Items))
	}

	// Set a manifest
	mJSON := `{"items":[{"catid":"CAT-1","qty":4},{"catid":"CAT-2","qty":6}]}`
	if err := db.SetBinManifest(bin.ID, mJSON, "PAY-1", 10); err != nil {
		t.Fatalf("SetBinManifest: %v", err)
	}

	// Read back bin directly — verify payload_code, uop_remaining, confirmed=false
	afterSet, _ := db.GetBin(bin.ID)
	if afterSet.PayloadCode != "PAY-1" {
		t.Errorf("PayloadCode = %q, want PAY-1", afterSet.PayloadCode)
	}
	if afterSet.UOPRemaining != 10 {
		t.Errorf("UOPRemaining = %d, want 10", afterSet.UOPRemaining)
	}
	if afterSet.ManifestConfirmed {
		t.Error("ManifestConfirmed should be false after Set")
	}

	// GetBinManifest hit — parse JSON
	mHit, err := db.GetBinManifest(bin.ID)
	if err != nil {
		t.Fatalf("GetBinManifest (hit): %v", err)
	}
	if len(mHit.Items) != 2 {
		t.Fatalf("items len = %d, want 2", len(mHit.Items))
	}
	if mHit.Items[0].CatID != "CAT-1" || mHit.Items[0].Quantity != 4 {
		t.Errorf("items[0] = (%s,%d), want (CAT-1,4)", mHit.Items[0].CatID, mHit.Items[0].Quantity)
	}
	if mHit.Items[1].CatID != "CAT-2" || mHit.Items[1].Quantity != 6 {
		t.Errorf("items[1] = (%s,%d)", mHit.Items[1].CatID, mHit.Items[1].Quantity)
	}

	// Confirm
	if err := db.ConfirmBinManifest(bin.ID, ""); err != nil {
		t.Fatalf("ConfirmBinManifest: %v", err)
	}
	confirmed, _ := db.GetBin(bin.ID)
	if !confirmed.ManifestConfirmed {
		t.Error("ManifestConfirmed should be true after Confirm")
	}
	if confirmed.LoadedAt == nil {
		t.Error("LoadedAt should be set after Confirm")
	}

	// Clear
	if err := db.ClearBinManifest(bin.ID); err != nil {
		t.Fatalf("ClearBinManifest: %v", err)
	}
	cleared, _ := db.GetBin(bin.ID)
	if cleared.PayloadCode != "" {
		t.Errorf("PayloadCode after clear = %q, want empty", cleared.PayloadCode)
	}
	if cleared.UOPRemaining != 0 {
		t.Errorf("UOPRemaining after clear = %d, want 0", cleared.UOPRemaining)
	}
	if cleared.ManifestConfirmed {
		t.Error("ManifestConfirmed should be false after Clear")
	}
	if cleared.LoadedAt != nil {
		t.Error("LoadedAt should be cleared")
	}
}

func TestFindSourceBinFIFO(t *testing.T) {
	db := testDB(t)

	bt := &BinType{Code: "FIFO-BT", Description: "fifo tote"}
	db.CreateBinType(bt)

	// Storage node — must be enabled + non-synthetic per FindSourceFIFO.
	node := &Node{Name: "FIFO-NODE", Enabled: true}
	db.CreateNode(node)

	// Create three bins; confirm with distinct timestamps to control loaded_at ordering.
	older := &Bin{BinTypeID: bt.ID, Label: "FIFO-OLD", NodeID: &node.ID, Status: "available"}
	db.CreateBin(older)
	db.SetBinManifest(older.ID, `{"items":[]}`, "PAY-F", 10)
	db.ConfirmBinManifest(older.ID, "2024-01-01 00:00:00")

	middle := &Bin{BinTypeID: bt.ID, Label: "FIFO-MID", NodeID: &node.ID, Status: "available"}
	db.CreateBin(middle)
	db.SetBinManifest(middle.ID, `{"items":[]}`, "PAY-F", 10)
	db.ConfirmBinManifest(middle.ID, "2024-06-01 00:00:00")

	newer := &Bin{BinTypeID: bt.ID, Label: "FIFO-NEW", NodeID: &node.ID, Status: "available"}
	db.CreateBin(newer)
	db.SetBinManifest(newer.ID, `{"items":[]}`, "PAY-F", 10)
	db.ConfirmBinManifest(newer.ID, "2024-12-01 00:00:00")

	// FIFO — should pick the oldest confirmed bin
	found, err := db.FindSourceBinFIFO("PAY-F")
	if err != nil {
		t.Fatalf("FindSourceBinFIFO: %v", err)
	}
	if found.ID != older.ID {
		t.Errorf("FIFO returned bin %d (%s), want %d (%s)",
			found.ID, found.Label, older.ID, older.Label)
	}

	// Claim the oldest — next FIFO should return middle
	db.ClaimBin(older.ID, 1)
	found2, err := db.FindSourceBinFIFO("PAY-F")
	if err != nil {
		t.Fatalf("FindSourceBinFIFO after claim: %v", err)
	}
	if found2.ID != middle.ID {
		t.Errorf("FIFO after claim returned %d (%s), want %d (%s)",
			found2.ID, found2.Label, middle.ID, middle.Label)
	}

	// No match for a different payload
	if _, err := db.FindSourceBinFIFO("NO-SUCH-PAYLOAD"); err == nil {
		t.Error("FindSourceBinFIFO(no such) should error")
	}
}

func TestFindStorageDestination(t *testing.T) {
	db := testDB(t)

	bt := &BinType{Code: "DEST-BT", Description: "dest tote"}
	db.CreateBinType(bt)

	// Two physical storage nodes, one synthetic (should be excluded), and one disabled.
	nodeA := &Node{Name: "DEST-A", Enabled: true}
	db.CreateNode(nodeA)
	nodeB := &Node{Name: "DEST-B", Enabled: true}
	db.CreateNode(nodeB)
	syntheticNode := &Node{Name: "DEST-SYN", Enabled: true, IsSynthetic: true}
	db.CreateNode(syntheticNode)
	disabledNode := &Node{Name: "DEST-OFF", Enabled: false}
	db.CreateNode(disabledNode)

	// Put a matching-payload bin at nodeB — consolidation path should prefer nodeB.
	existing := &Bin{BinTypeID: bt.ID, Label: "DEST-EX", NodeID: &nodeB.ID, Status: "available", PayloadCode: "DEST-PAY"}
	db.CreateBin(existing)
	db.SetBinManifest(existing.ID, `{"items":[]}`, "DEST-PAY", 10)
	db.ConfirmBinManifest(existing.ID, "")

	// Case 1: consolidation — should pick nodeB (already has DEST-PAY)
	dest, err := db.FindStorageDestination("DEST-PAY")
	if err != nil {
		t.Fatalf("FindStorageDestination consolidation: %v", err)
	}
	if dest.ID != nodeB.ID {
		t.Errorf("consolidation dest = %d (%s), want %d (%s)",
			dest.ID, dest.Name, nodeB.ID, nodeB.Name)
	}

	// Case 2: no existing bins for an unseen payload — fall back to empty enabled physical node.
	dest2, err := db.FindStorageDestination("UNSEEN-PAY")
	if err != nil {
		t.Fatalf("FindStorageDestination fallback: %v", err)
	}
	// nodeA is empty & enabled & physical — nodeB already has a bin.
	if dest2.ID != nodeA.ID {
		t.Errorf("fallback dest = %d (%s), want %d (%s)",
			dest2.ID, dest2.Name, nodeA.ID, nodeA.Name)
	}
	// Must not be the synthetic or disabled node
	if dest2.IsSynthetic {
		t.Error("fallback should skip synthetic nodes")
	}
	if !dest2.Enabled {
		t.Error("fallback should skip disabled nodes")
	}
}

func TestSetBinManifestFromTemplate(t *testing.T) {
	db := testDB(t)

	bt := &BinType{Code: "TMPL-BT", Description: "tmpl tote"}
	db.CreateBinType(bt)

	node := &Node{Name: "TMPL-NODE", Enabled: true}
	db.CreateNode(node)

	// Payload with a manifest template
	p := &Payload{Code: "TMPL-PAY", UOPCapacity: 77}
	db.CreatePayload(p)
	db.ReplacePayloadManifest(p.ID, []*PayloadManifestItem{
		{PayloadID: p.ID, PartNumber: "PART-X", Quantity: 3},
		{PayloadID: p.ID, PartNumber: "PART-Y", Quantity: 5},
	})

	bin := &Bin{BinTypeID: bt.ID, Label: "TMPL-B", NodeID: &node.ID, Status: "available"}
	db.CreateBin(bin)

	// Apply the template — uopCapacity=0 means use payload's default
	if err := db.SetBinManifestFromTemplate(bin.ID, p.Code, 0); err != nil {
		t.Fatalf("SetBinManifestFromTemplate: %v", err)
	}

	// Verify bin fields
	got, _ := db.GetBin(bin.ID)
	if got.PayloadCode != "TMPL-PAY" {
		t.Errorf("PayloadCode = %q, want TMPL-PAY", got.PayloadCode)
	}
	if got.UOPRemaining != 77 {
		t.Errorf("UOPRemaining = %d, want 77 (payload default)", got.UOPRemaining)
	}

	// Parsed manifest should mirror the template items
	m, err := db.GetBinManifest(bin.ID)
	if err != nil {
		t.Fatalf("GetBinManifest: %v", err)
	}
	if len(m.Items) != 2 {
		t.Fatalf("items len = %d, want 2", len(m.Items))
	}

	// CatID maps from PartNumber; Quantity carries through.
	found := map[string]int64{}
	for _, e := range m.Items {
		found[e.CatID] = e.Quantity
	}
	if found["PART-X"] != 3 {
		t.Errorf("PART-X qty = %d, want 3", found["PART-X"])
	}
	if found["PART-Y"] != 5 {
		t.Errorf("PART-Y qty = %d, want 5", found["PART-Y"])
	}

	// Override uopCapacity
	if err := db.SetBinManifestFromTemplate(bin.ID, p.Code, 200); err != nil {
		t.Fatalf("SetBinManifestFromTemplate override: %v", err)
	}
	got2, _ := db.GetBin(bin.ID)
	if got2.UOPRemaining != 200 {
		t.Errorf("UOPRemaining after override = %d, want 200", got2.UOPRemaining)
	}
}
