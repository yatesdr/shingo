//go:build docker

package store

import (
	"testing"
	"time"
)

func TestBinTypeCRUD(t *testing.T) {
	db := testDB(t)

	bt := &BinType{
		Code:        "TOTE-SM",
		Description: "Small tote",
		WidthIn:     12.0,
		HeightIn:    8.0,
	}
	if err := db.CreateBinType(bt); err != nil {
		t.Fatalf("create: %v", err)
	}
	if bt.ID == 0 {
		t.Fatal("ID should be assigned")
	}

	// Get
	got, err := db.GetBinType(bt.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Code != "TOTE-SM" {
		t.Errorf("Code = %q, want %q", got.Code, "TOTE-SM")
	}
	if got.Description != "Small tote" {
		t.Errorf("Description = %q, want %q", got.Description, "Small tote")
	}
	if got.WidthIn != 12.0 {
		t.Errorf("WidthIn = %f, want 12.0", got.WidthIn)
	}
	if got.HeightIn != 8.0 {
		t.Errorf("HeightIn = %f, want 8.0", got.HeightIn)
	}

	// GetByCode
	byCode, err := db.GetBinTypeByCode("TOTE-SM")
	if err != nil {
		t.Fatalf("getByCode: %v", err)
	}
	if byCode.ID != bt.ID {
		t.Errorf("getByCode ID = %d, want %d", byCode.ID, bt.ID)
	}

	// Update
	got.Code = "TOTE-SM-V2"
	got.WidthIn = 14.0
	if err := db.UpdateBinType(got); err != nil {
		t.Fatalf("update: %v", err)
	}
	got2, _ := db.GetBinType(bt.ID)
	if got2.Code != "TOTE-SM-V2" {
		t.Errorf("Code after update = %q, want %q", got2.Code, "TOTE-SM-V2")
	}
	if got2.WidthIn != 14.0 {
		t.Errorf("WidthIn after update = %f, want 14.0", got2.WidthIn)
	}

	// List
	bt2 := &BinType{Code: "CRATE-LG", Description: "Large crate", WidthIn: 24.0, HeightIn: 16.0}
	db.CreateBinType(bt2)
	all, err := db.ListBinTypes()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) < 2 {
		t.Errorf("list len = %d, want >= 2", len(all))
	}

	// Delete
	if err := db.DeleteBinType(bt.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err = db.GetBinType(bt.ID)
	if err == nil {
		t.Error("expected error after delete")
	}
}

func TestBinCRUD(t *testing.T) {
	db := testDB(t)

	// Create prerequisites
	bt := &BinType{Code: "TOTE-A", Description: "Standard tote", WidthIn: 12.0, HeightIn: 8.0}
	db.CreateBinType(bt)

	node := &Node{Name: "STORAGE-B1", Enabled: true}
	db.CreateNode(node)

	// Create bin
	bin := &Bin{
		BinTypeID:   bt.ID,
		Label:       "BIN-001",
		Description: "First bin",
		NodeID:      &node.ID,
		Status:      "available",
	}
	if err := db.CreateBin(bin); err != nil {
		t.Fatalf("create bin: %v", err)
	}
	if bin.ID == 0 {
		t.Fatal("ID should be assigned")
	}

	// Get with joined fields
	got, err := db.GetBin(bin.ID)
	if err != nil {
		t.Fatalf("get bin: %v", err)
	}
	if got.Label != "BIN-001" {
		t.Errorf("Label = %q, want %q", got.Label, "BIN-001")
	}
	if got.BinTypeCode != "TOTE-A" {
		t.Errorf("BinTypeCode = %q, want %q", got.BinTypeCode, "TOTE-A")
	}
	if got.NodeName != "STORAGE-B1" {
		t.Errorf("NodeName = %q, want %q", got.NodeName, "STORAGE-B1")
	}
	if got.Status != "available" {
		t.Errorf("Status = %q, want %q", got.Status, "available")
	}

	// GetByLabel
	byLabel, err := db.GetBinByLabel("BIN-001")
	if err != nil {
		t.Fatalf("getByLabel: %v", err)
	}
	if byLabel.ID != bin.ID {
		t.Errorf("getByLabel ID = %d, want %d", byLabel.ID, bin.ID)
	}

	// Update
	got.Description = "Updated bin"
	got.Status = "in_use"
	if err := db.UpdateBin(got); err != nil {
		t.Fatalf("update bin: %v", err)
	}
	got2, _ := db.GetBin(bin.ID)
	if got2.Description != "Updated bin" {
		t.Errorf("Description after update = %q, want %q", got2.Description, "Updated bin")
	}
	if got2.Status != "in_use" {
		t.Errorf("Status after update = %q, want %q", got2.Status, "in_use")
	}

	// Create second bin at same node
	bin2 := &Bin{BinTypeID: bt.ID, Label: "BIN-002", NodeID: &node.ID, Status: "available"}
	db.CreateBin(bin2)

	// ListBins
	all, err := db.ListBins()
	if err != nil {
		t.Fatalf("list bins: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("list len = %d, want 2", len(all))
	}

	// ListBinsByNode
	byNode, err := db.ListBinsByNode(node.ID)
	if err != nil {
		t.Fatalf("list by node: %v", err)
	}
	if len(byNode) != 2 {
		t.Errorf("by node len = %d, want 2", len(byNode))
	}

	// CountBinsByNode
	count, err := db.CountBinsByNode(node.ID)
	if err != nil {
		t.Fatalf("count by node: %v", err)
	}
	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}

	// MoveBin
	node2 := &Node{Name: "LINE-1", Enabled: true}
	db.CreateNode(node2)
	if err := db.MoveBin(bin.ID, node2.ID); err != nil {
		t.Fatalf("move bin: %v", err)
	}
	got3, _ := db.GetBin(bin.ID)
	if got3.NodeID == nil || *got3.NodeID != node2.ID {
		t.Errorf("NodeID after move = %v, want %d", got3.NodeID, node2.ID)
	}

	// Delete
	if err := db.DeleteBin(bin.ID); err != nil {
		t.Fatalf("delete bin: %v", err)
	}
	remaining, _ := db.ListBins()
	if len(remaining) != 1 {
		t.Errorf("remaining after delete = %d, want 1", len(remaining))
	}
}

func TestPayloadCRUD(t *testing.T) {
	db := testDB(t)

	bp := &Payload{
		Code:        "WK-100",
		Description: "Standard widget kit",
		UOPCapacity: 50,
	}
	if err := db.CreatePayload(bp); err != nil {
		t.Fatalf("create: %v", err)
	}
	if bp.ID == 0 {
		t.Fatal("ID should be assigned")
	}

	// Get
	got, err := db.GetPayload(bp.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Code != "WK-100" {
		t.Errorf("Code = %q, want %q", got.Code, "WK-100")
	}
	if got.Description != "Standard widget kit" {
		t.Errorf("Description = %q, want %q", got.Description, "Standard widget kit")
	}
	if got.UOPCapacity != 50 {
		t.Errorf("UOPCapacity = %d, want 50", got.UOPCapacity)
	}

	// GetByCode
	byCode, err := db.GetPayloadByCode("WK-100")
	if err != nil {
		t.Fatalf("getByCode: %v", err)
	}
	if byCode.ID != bp.ID {
		t.Errorf("getByCode ID = %d, want %d", byCode.ID, bp.ID)
	}

	// Update
	got.Code = "WK-200"
	got.UOPCapacity = 75
	if err := db.UpdatePayload(got); err != nil {
		t.Fatalf("update: %v", err)
	}
	got2, _ := db.GetPayload(bp.ID)
	if got2.Code != "WK-200" {
		t.Errorf("Code after update = %q, want %q", got2.Code, "WK-200")
	}
	if got2.UOPCapacity != 75 {
		t.Errorf("UOPCapacity after update = %d, want 75", got2.UOPCapacity)
	}

	// Delete
	if err := db.DeletePayload(bp.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err = db.GetPayload(bp.ID)
	if err == nil {
		t.Error("expected error after delete")
	}
}

func TestPayloadBinTypeJunction(t *testing.T) {
	db := testDB(t)

	bp := &Payload{Code: "MBK-1", UOPCapacity: 100}
	db.CreatePayload(bp)

	bt1 := &BinType{Code: "TOTE-A", Description: "Tote type A", WidthIn: 12.0, HeightIn: 8.0}
	db.CreateBinType(bt1)
	bt2 := &BinType{Code: "CRATE-B", Description: "Crate type B", WidthIn: 24.0, HeightIn: 16.0}
	db.CreateBinType(bt2)

	// Set bin types for payload
	if err := db.SetPayloadBinTypes(bp.ID, []int64{bt1.ID, bt2.ID}); err != nil {
		t.Fatalf("set bin types: %v", err)
	}

	// List bin types for payload
	types, err := db.ListBinTypesForPayload(bp.ID)
	if err != nil {
		t.Fatalf("list bin types for payload: %v", err)
	}
	if len(types) != 2 {
		t.Fatalf("bin types len = %d, want 2", len(types))
	}

	// Replace with just one
	if err := db.SetPayloadBinTypes(bp.ID, []int64{bt1.ID}); err != nil {
		t.Fatalf("replace bin types: %v", err)
	}
	types2, _ := db.ListBinTypesForPayload(bp.ID)
	if len(types2) != 1 {
		t.Errorf("bin types after replace = %d, want 1", len(types2))
	}
	if types2[0].Code != "TOTE-A" {
		t.Errorf("remaining bin type code = %q, want %q", types2[0].Code, "TOTE-A")
	}

	// Clear all
	if err := db.SetPayloadBinTypes(bp.ID, nil); err != nil {
		t.Fatalf("clear bin types: %v", err)
	}
	types3, _ := db.ListBinTypesForPayload(bp.ID)
	if len(types3) != 0 {
		t.Errorf("bin types after clear = %d, want 0", len(types3))
	}
}

func TestPayloadTemplateCRUD(t *testing.T) {
	db := testDB(t)

	p := &Payload{Code: "BIN-X", UOPCapacity: 200, Description: "Test template"}
	if err := db.CreatePayload(p); err != nil {
		t.Fatalf("create payload: %v", err)
	}
	if p.ID == 0 {
		t.Fatal("ID should be assigned")
	}

	// Get
	got, err := db.GetPayload(p.ID)
	if err != nil {
		t.Fatalf("get payload: %v", err)
	}
	if got.Code != "BIN-X" {
		t.Errorf("Code = %q, want %q", got.Code, "BIN-X")
	}
	if got.UOPCapacity != 200 {
		t.Errorf("UOPCapacity = %d, want 200", got.UOPCapacity)
	}

	// Update
	got.UOPCapacity = 150
	if err := db.UpdatePayload(got); err != nil {
		t.Fatalf("update: %v", err)
	}
	got2, _ := db.GetPayload(p.ID)
	if got2.UOPCapacity != 150 {
		t.Errorf("UOPCapacity after update = %d, want 150", got2.UOPCapacity)
	}

	// Create second template
	p2 := &Payload{Code: "BIN-Y", UOPCapacity: 50}
	db.CreatePayload(p2)

	// ListPayloads
	all, err := db.ListPayloads()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("list len = %d, want 2", len(all))
	}

	// Delete
	if err := db.DeletePayload(p.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	remaining, _ := db.ListPayloads()
	if len(remaining) != 1 {
		t.Errorf("remaining after delete = %d, want 1", len(remaining))
	}
}

func TestBinManifestLifecycle(t *testing.T) {
	db := testDB(t)

	bt := &BinType{Code: "CRATE-Y", Description: "Standard crate", WidthIn: 24.0, HeightIn: 16.0}
	db.CreateBinType(bt)

	bp := &Payload{Code: "CRATE-Y", UOPCapacity: 100}
	db.CreatePayload(bp)

	node1 := &Node{Name: "STORE-1", Enabled: true}
	db.CreateNode(node1)
	node2 := &Node{Name: "LINE-1", Enabled: true}
	db.CreateNode(node2)

	bin := &Bin{BinTypeID: bt.ID, Label: "CY-001", NodeID: &node1.ID, Status: "available"}
	db.CreateBin(bin)

	// Set bin manifest
	manifestJSON := `{"items":[{"catid":"PART-1","qty":10}]}`
	if err := db.SetBinManifest(bin.ID, manifestJSON, bp.Code, 100); err != nil {
		t.Fatalf("set manifest: %v", err)
	}
	db.ConfirmBinManifest(bin.ID, "")

	// Verify bin has manifest
	got, _ := db.GetBin(bin.ID)
	if got.PayloadCode != bp.Code {
		t.Errorf("PayloadCode = %q, want %q", got.PayloadCode, bp.Code)
	}
	if got.UOPRemaining != 100 {
		t.Errorf("UOPRemaining = %d, want 100", got.UOPRemaining)
	}
	if !got.ManifestConfirmed {
		t.Error("ManifestConfirmed should be true")
	}

	// Claim via bin
	orderID := int64(42)
	if err := db.ClaimBin(bin.ID, orderID); err != nil {
		t.Fatalf("claim bin: %v", err)
	}
	got2, _ := db.GetBin(bin.ID)
	if got2.ClaimedBy == nil || *got2.ClaimedBy != orderID {
		t.Errorf("ClaimedBy = %v, want %d", got2.ClaimedBy, orderID)
	}

	// Unclaim
	db.UnclaimBin(bin.ID)

	// MoveBin
	if err := db.MoveBin(bin.ID, node2.ID); err != nil {
		t.Fatalf("move bin: %v", err)
	}
	got3, _ := db.GetBin(bin.ID)
	if got3.NodeID == nil || *got3.NodeID != node2.ID {
		t.Errorf("NodeID after move = %v, want %d", got3.NodeID, node2.ID)
	}

	// Claim first bin so it's excluded from FIFO
	db.ClaimBin(bin.ID, 99)

	// FindSourceBinFIFO -- create two more bins with manifests, verify FIFO order
	bin2 := &Bin{BinTypeID: bt.ID, Label: "CY-002", NodeID: &node1.ID, Status: "available"}
	db.CreateBin(bin2)
	db.SetBinManifest(bin2.ID, `{"items":[]}`, bp.Code, 50)
	db.ConfirmBinManifest(bin2.ID, "")

	bin3 := &Bin{BinTypeID: bt.ID, Label: "CY-003", NodeID: &node1.ID, Status: "available"}
	db.CreateBin(bin3)
	db.SetBinManifest(bin3.ID, `{"items":[]}`, bp.Code, 75)
	db.ConfirmBinManifest(bin3.ID, "")

	fifo, err := db.FindSourceBinFIFO("CRATE-Y")
	if err != nil {
		t.Fatalf("FindSourceBinFIFO: %v", err)
	}
	// bin2 was confirmed first, should be returned (FIFO by loaded_at)
	if fifo.ID != bin2.ID {
		t.Errorf("FIFO bin ID = %d, want %d", fifo.ID, bin2.ID)
	}

	// Clear manifest
	if err := db.ClearBinManifest(bin2.ID); err != nil {
		t.Fatalf("clear manifest: %v", err)
	}
	cleared, _ := db.GetBin(bin2.ID)
	if cleared.PayloadCode != "" {
		t.Errorf("PayloadCode after clear = %q, want empty", cleared.PayloadCode)
	}
	if cleared.ManifestConfirmed {
		t.Error("ManifestConfirmed should be false after clear")
	}
}

func TestPayloadManifestCRUD(t *testing.T) {
	db := testDB(t)

	bp := &Payload{Code: "KIT-M", UOPCapacity: 10}
	db.CreatePayload(bp)

	// Create 2 manifest items
	item1 := &PayloadManifestItem{PayloadID: bp.ID, PartNumber: "PN-001", Quantity: 5, Description: "Bolt M8"}
	if err := db.CreatePayloadManifestItem(item1); err != nil {
		t.Fatalf("create item1: %v", err)
	}
	if item1.ID == 0 {
		t.Fatal("item1 ID should be assigned")
	}

	item2 := &PayloadManifestItem{PayloadID: bp.ID, PartNumber: "PN-002", Quantity: 10, Description: "Washer M8"}
	if err := db.CreatePayloadManifestItem(item2); err != nil {
		t.Fatalf("create item2: %v", err)
	}

	// List (ordered by id)
	items, err := db.ListPayloadManifest(bp.ID)
	if err != nil {
		t.Fatalf("list manifest: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("manifest len = %d, want 2", len(items))
	}
	if items[0].PartNumber != "PN-001" {
		t.Errorf("first item part = %q, want %q", items[0].PartNumber, "PN-001")
	}
	if items[1].PartNumber != "PN-002" {
		t.Errorf("second item part = %q, want %q", items[1].PartNumber, "PN-002")
	}

	// Delete one item
	if err := db.DeletePayloadManifestItem(item1.ID); err != nil {
		t.Fatalf("delete item: %v", err)
	}
	remaining, _ := db.ListPayloadManifest(bp.ID)
	if len(remaining) != 1 {
		t.Errorf("remaining after delete = %d, want 1", len(remaining))
	}

	// ReplacePayloadManifest
	replacements := []*PayloadManifestItem{
		{PartNumber: "PN-100", Quantity: 2, Description: "Nut M10"},
		{PartNumber: "PN-101", Quantity: 4, Description: "Screw M10"},
		{PartNumber: "PN-102", Quantity: 1, Description: "Bracket"},
	}
	if err := db.ReplacePayloadManifest(bp.ID, replacements); err != nil {
		t.Fatalf("replace manifest: %v", err)
	}
	replaced, _ := db.ListPayloadManifest(bp.ID)
	if len(replaced) != 3 {
		t.Fatalf("replaced len = %d, want 3", len(replaced))
	}
	if replaced[0].PartNumber != "PN-100" {
		t.Errorf("replaced[0] part = %q, want %q", replaced[0].PartNumber, "PN-100")
	}
	if replaced[2].PartNumber != "PN-102" {
		t.Errorf("replaced[2] part = %q, want %q", replaced[2].PartNumber, "PN-102")
	}
}

func TestNodePayloadAssignment(t *testing.T) {
	db := testDB(t)

	node := &Node{Name: "STORE-NB", Enabled: true}
	db.CreateNode(node)

	bp1 := &Payload{Code: "KIT-A", UOPCapacity: 10}
	db.CreatePayload(bp1)
	bp2 := &Payload{Code: "KIT-B", UOPCapacity: 20}
	db.CreatePayload(bp2)

	// Assign
	if err := db.AssignPayloadToNode(node.ID, bp1.ID); err != nil {
		t.Fatalf("assign bp1: %v", err)
	}
	if err := db.AssignPayloadToNode(node.ID, bp2.ID); err != nil {
		t.Fatalf("assign bp2: %v", err)
	}

	// List payloads for node
	bps, err := db.ListPayloadsForNode(node.ID)
	if err != nil {
		t.Fatalf("list payloads for node: %v", err)
	}
	if len(bps) != 2 {
		t.Fatalf("payloads len = %d, want 2", len(bps))
	}

	// List nodes for payload
	nodes, err := db.ListNodesForPayload(bp1.ID)
	if err != nil {
		t.Fatalf("list nodes for payload: %v", err)
	}
	if len(nodes) != 1 {
		t.Errorf("nodes len = %d, want 1", len(nodes))
	}

	// Unassign
	if err := db.UnassignPayloadFromNode(node.ID, bp1.ID); err != nil {
		t.Fatalf("unassign: %v", err)
	}
	bps2, _ := db.ListPayloadsForNode(node.ID)
	if len(bps2) != 1 {
		t.Errorf("payloads after unassign = %d, want 1", len(bps2))
	}

	// SetNodePayloads (replace all)
	if err := db.SetNodePayloads(node.ID, []int64{bp1.ID, bp2.ID}); err != nil {
		t.Fatalf("set node payloads: %v", err)
	}
	bps3, _ := db.ListPayloadsForNode(node.ID)
	if len(bps3) != 2 {
		t.Errorf("payloads after set = %d, want 2", len(bps3))
	}
}

// TestConfirmBinManifest_ProducedAt verifies that ConfirmBinManifest uses the
// Edge-provided producedAt timestamp for loaded_at instead of server time,
// and that FIFO ordering respects it. This ensures audit-grade lot dating:
// the timestamp reflects when the operator finalized the bin at the cell,
// not when Core processed the message.
func TestConfirmBinManifest_ProducedAt(t *testing.T) {
	db := testDB(t)

	node := &Node{Name: "STORAGE-PA", Enabled: true}
	db.CreateNode(node)
	bt := &BinType{Code: "DEFAULT-PA"}
	db.CreateBinType(bt)
	bp := &Payload{Code: "PART-PA", UOPCapacity: 50}
	db.CreatePayload(bp)

	// --- Test 1: explicit producedAt is written to loaded_at ---
	bin1 := &Bin{BinTypeID: bt.ID, Label: "PA-001", NodeID: &node.ID, Status: "available"}
	db.CreateBin(bin1)
	db.SetBinManifest(bin1.ID, `{"items":[]}`, bp.Code, 50)

	// Use a timestamp 2 hours in the past to simulate Edge-stamped time
	edgeTime := time.Now().UTC().Add(-2 * time.Hour)
	producedAt := edgeTime.Format(time.RFC3339)

	if err := db.ConfirmBinManifest(bin1.ID, producedAt); err != nil {
		t.Fatalf("ConfirmBinManifest with producedAt: %v", err)
	}

	got1, _ := db.GetBin(bin1.ID)
	if got1.LoadedAt == nil {
		t.Fatal("loaded_at should not be nil after ConfirmBinManifest")
	}
	// loaded_at should be close to the Edge timestamp, not server time
	drift := got1.LoadedAt.Sub(edgeTime).Abs()
	if drift > 2*time.Second {
		t.Errorf("loaded_at drift from producedAt = %v (want <2s); loaded_at=%v, producedAt=%v",
			drift, got1.LoadedAt.UTC(), edgeTime)
	}

	// --- Test 2: empty producedAt falls back to server time ---
	bin2 := &Bin{BinTypeID: bt.ID, Label: "PA-002", NodeID: &node.ID, Status: "available"}
	db.CreateBin(bin2)
	db.SetBinManifest(bin2.ID, `{"items":[]}`, bp.Code, 50)

	before := time.Now().UTC()
	if err := db.ConfirmBinManifest(bin2.ID, ""); err != nil {
		t.Fatalf("ConfirmBinManifest with empty producedAt: %v", err)
	}
	after := time.Now().UTC()

	got2, _ := db.GetBin(bin2.ID)
	if got2.LoadedAt == nil {
		t.Fatal("loaded_at should not be nil for empty producedAt fallback")
	}
	if got2.LoadedAt.Before(before.Add(-time.Second)) || got2.LoadedAt.After(after.Add(time.Second)) {
		t.Errorf("loaded_at fallback not near server time: loaded_at=%v, window=[%v, %v]",
			got2.LoadedAt.UTC(), before, after)
	}

	// --- Test 3: FIFO ordering respects producedAt ---
	// bin1 has loaded_at = 2 hours ago (Edge time)
	// bin2 has loaded_at = now (server time)
	// FIFO should return bin1 first (older)
	fifo, err := db.FindSourceBinFIFO(bp.Code)
	if err != nil {
		t.Fatalf("FindSourceBinFIFO: %v", err)
	}
	if fifo.ID != bin1.ID {
		t.Errorf("FIFO should return bin1 (older producedAt) first: got bin %d, want %d", fifo.ID, bin1.ID)
	}

	// Claim bin1 and verify FIFO falls through to bin2
	db.ClaimBin(bin1.ID, 999)
	fifo2, err := db.FindSourceBinFIFO(bp.Code)
	if err != nil {
		t.Fatalf("FindSourceBinFIFO after claim: %v", err)
	}
	if fifo2.ID != bin2.ID {
		t.Errorf("FIFO after claiming bin1: got bin %d, want %d", fifo2.ID, bin2.ID)
	}
}
