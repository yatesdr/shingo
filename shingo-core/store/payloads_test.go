package store

import "testing"

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

	// ListBinsByType
	byType, err := db.ListBinsByType(bt.ID)
	if err != nil {
		t.Fatalf("list by type: %v", err)
	}
	if len(byType) != 2 {
		t.Errorf("by type len = %d, want 2", len(byType))
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

func TestBlueprintCRUD(t *testing.T) {
	db := testDB(t)

	bp := &Blueprint{
		Code:        "WK-100",
		Description: "Standard widget kit",
		UOPCapacity: 50,
	}
	if err := db.CreateBlueprint(bp); err != nil {
		t.Fatalf("create: %v", err)
	}
	if bp.ID == 0 {
		t.Fatal("ID should be assigned")
	}

	// Get
	got, err := db.GetBlueprint(bp.ID)
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
	byCode, err := db.GetBlueprintByCode("WK-100")
	if err != nil {
		t.Fatalf("getByCode: %v", err)
	}
	if byCode.ID != bp.ID {
		t.Errorf("getByCode ID = %d, want %d", byCode.ID, bp.ID)
	}

	// Update
	got.Code = "WK-200"
	got.UOPCapacity = 75
	if err := db.UpdateBlueprint(got); err != nil {
		t.Fatalf("update: %v", err)
	}
	got2, _ := db.GetBlueprint(bp.ID)
	if got2.Code != "WK-200" {
		t.Errorf("Code after update = %q, want %q", got2.Code, "WK-200")
	}
	if got2.UOPCapacity != 75 {
		t.Errorf("UOPCapacity after update = %d, want 75", got2.UOPCapacity)
	}

	// Delete
	if err := db.DeleteBlueprint(bp.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err = db.GetBlueprint(bp.ID)
	if err == nil {
		t.Error("expected error after delete")
	}
}

func TestBlueprintBinTypeJunction(t *testing.T) {
	db := testDB(t)

	bp := &Blueprint{Code: "MBK-1", UOPCapacity: 100}
	db.CreateBlueprint(bp)

	bt1 := &BinType{Code: "TOTE-A", Description: "Tote type A", WidthIn: 12.0, HeightIn: 8.0}
	db.CreateBinType(bt1)
	bt2 := &BinType{Code: "CRATE-B", Description: "Crate type B", WidthIn: 24.0, HeightIn: 16.0}
	db.CreateBinType(bt2)

	// Set bin types for blueprint
	if err := db.SetBlueprintBinTypes(bp.ID, []int64{bt1.ID, bt2.ID}); err != nil {
		t.Fatalf("set bin types: %v", err)
	}

	// List bin types for blueprint
	types, err := db.ListBinTypesForBlueprint(bp.ID)
	if err != nil {
		t.Fatalf("list bin types for blueprint: %v", err)
	}
	if len(types) != 2 {
		t.Fatalf("bin types len = %d, want 2", len(types))
	}

	// Replace with just one
	if err := db.SetBlueprintBinTypes(bp.ID, []int64{bt1.ID}); err != nil {
		t.Fatalf("replace bin types: %v", err)
	}
	types2, _ := db.ListBinTypesForBlueprint(bp.ID)
	if len(types2) != 1 {
		t.Errorf("bin types after replace = %d, want 1", len(types2))
	}
	if types2[0].Code != "TOTE-A" {
		t.Errorf("remaining bin type code = %q, want %q", types2[0].Code, "TOTE-A")
	}

	// Clear all
	if err := db.SetBlueprintBinTypes(bp.ID, nil); err != nil {
		t.Fatalf("clear bin types: %v", err)
	}
	types3, _ := db.ListBinTypesForBlueprint(bp.ID)
	if len(types3) != 0 {
		t.Errorf("bin types after clear = %d, want 0", len(types3))
	}
}

func TestPayloadCRUD(t *testing.T) {
	db := testDB(t)

	// Create prerequisites: bin_type -> bin -> blueprint -> payload
	bt := &BinType{Code: "BIN-X", Description: "Standard bin", WidthIn: 12.0, HeightIn: 8.0}
	db.CreateBinType(bt)

	node := &Node{Name: "STORAGE-B1", Enabled: true}
	db.CreateNode(node)

	bin := &Bin{BinTypeID: bt.ID, Label: "BX-001", NodeID: &node.ID, Status: "available"}
	db.CreateBin(bin)

	bp := &Blueprint{Code: "BIN-X", UOPCapacity: 200}
	db.CreateBlueprint(bp)

	// Create payload
	payload := &Payload{
		BlueprintID:  bp.ID,
		BinID:        &bin.ID,
		Status:       "available",
		UOPRemaining: 100,
		Notes:        "test payload",
	}
	if err := db.CreatePayload(payload); err != nil {
		t.Fatalf("create payload: %v", err)
	}
	if payload.ID == 0 {
		t.Fatal("ID should be assigned")
	}

	// Get with joined fields
	got, err := db.GetPayload(payload.ID)
	if err != nil {
		t.Fatalf("get payload: %v", err)
	}
	if got.BlueprintCode != "BIN-X" {
		t.Errorf("BlueprintCode = %q, want %q", got.BlueprintCode, "BIN-X")
	}
	if got.BinLabel != "BX-001" {
		t.Errorf("BinLabel = %q, want %q", got.BinLabel, "BX-001")
	}
	if got.NodeName != "STORAGE-B1" {
		t.Errorf("NodeName = %q, want %q", got.NodeName, "STORAGE-B1")
	}
	if got.NodeID == nil || *got.NodeID != node.ID {
		t.Errorf("NodeID = %v, want %d", got.NodeID, node.ID)
	}
	if got.Status != "available" {
		t.Errorf("Status = %q, want %q", got.Status, "available")
	}
	if got.UOPRemaining != 100 {
		t.Errorf("UOPRemaining = %d, want 100", got.UOPRemaining)
	}

	// Update
	got.UOPRemaining = 80
	got.Notes = "updated notes"
	if err := db.UpdatePayload(got); err != nil {
		t.Fatalf("update payload: %v", err)
	}
	got2, _ := db.GetPayload(payload.ID)
	if got2.UOPRemaining != 80 {
		t.Errorf("UOPRemaining after update = %d, want 80", got2.UOPRemaining)
	}
	if got2.Notes != "updated notes" {
		t.Errorf("Notes after update = %q, want %q", got2.Notes, "updated notes")
	}

	// Create a second payload in a different bin at same node
	bin2 := &Bin{BinTypeID: bt.ID, Label: "BX-002", NodeID: &node.ID, Status: "available"}
	db.CreateBin(bin2)
	payload2 := &Payload{BlueprintID: bp.ID, BinID: &bin2.ID, Status: "available", UOPRemaining: 50}
	db.CreatePayload(payload2)

	// ListPayloads
	all, err := db.ListPayloads()
	if err != nil {
		t.Fatalf("list payloads: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("list len = %d, want 2", len(all))
	}

	// ListPayloadsByNode (via bin join)
	byNode, err := db.ListPayloadsByNode(node.ID)
	if err != nil {
		t.Fatalf("list by node: %v", err)
	}
	if len(byNode) != 2 {
		t.Errorf("by node len = %d, want 2", len(byNode))
	}

	// CountBinsByNode
	count, err := db.CountBinsByNode(node.ID)
	if err != nil {
		t.Fatalf("count bins by node: %v", err)
	}
	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}

	// ListPayloadsByStatus
	byStatus, err := db.ListPayloadsByStatus("available")
	if err != nil {
		t.Fatalf("list by status: %v", err)
	}
	if len(byStatus) != 2 {
		t.Errorf("by status len = %d, want 2", len(byStatus))
	}

	// Delete
	if err := db.DeletePayload(payload.ID); err != nil {
		t.Fatalf("delete payload: %v", err)
	}
	remaining, _ := db.ListPayloads()
	if len(remaining) != 1 {
		t.Errorf("remaining after delete = %d, want 1", len(remaining))
	}
}

func TestPayloadLifecycle(t *testing.T) {
	db := testDB(t)

	bt := &BinType{Code: "CRATE-Y", Description: "Standard crate", WidthIn: 24.0, HeightIn: 16.0}
	db.CreateBinType(bt)

	bp := &Blueprint{Code: "CRATE-Y", UOPCapacity: 100}
	db.CreateBlueprint(bp)

	node1 := &Node{Name: "STORE-1", Enabled: true}
	db.CreateNode(node1)
	node2 := &Node{Name: "LINE-1", Enabled: true}
	db.CreateNode(node2)

	bin := &Bin{BinTypeID: bt.ID, Label: "CY-001", NodeID: &node1.ID, Status: "available"}
	db.CreateBin(bin)

	payload := &Payload{BlueprintID: bp.ID, BinID: &bin.ID, Status: "available", UOPRemaining: 100}
	db.CreatePayload(payload)

	// Claim via bin (bin-centric claiming)
	orderID := int64(42)
	if err := db.ClaimBin(bin.ID, orderID); err != nil {
		t.Fatalf("claim bin: %v", err)
	}
	got, _ := db.GetPayload(payload.ID)
	if got.ClaimedBy == nil || *got.ClaimedBy != orderID {
		t.Errorf("ClaimedBy (via bin) = %v, want %d", got.ClaimedBy, orderID)
	}

	// Unclaim via bin
	if err := db.UnclaimBin(bin.ID); err != nil {
		t.Fatalf("unclaim bin: %v", err)
	}
	got2, _ := db.GetPayload(payload.ID)
	if got2.ClaimedBy != nil {
		t.Errorf("ClaimedBy after bin unclaim = %v, want nil", got2.ClaimedBy)
	}

	// MoveBin (replaces MoveInstance)
	if err := db.MoveBin(bin.ID, node2.ID); err != nil {
		t.Fatalf("move bin: %v", err)
	}
	got3, _ := db.GetPayload(payload.ID)
	if got3.NodeID == nil || *got3.NodeID != node2.ID {
		t.Errorf("NodeID after move = %v, want %d", got3.NodeID, node2.ID)
	}

	// Claim the first bin so it's excluded from FIFO source selection
	db.ClaimBin(bin.ID, 99)

	// FindSourcePayloadFIFO -- create two more payloads, verify FIFO order
	bin2 := &Bin{BinTypeID: bt.ID, Label: "CY-002", NodeID: &node1.ID, Status: "available"}
	db.CreateBin(bin2)
	payload2 := &Payload{BlueprintID: bp.ID, BinID: &bin2.ID, Status: "available", UOPRemaining: 50}
	db.CreatePayload(payload2)

	bin3 := &Bin{BinTypeID: bt.ID, Label: "CY-003", NodeID: &node1.ID, Status: "available"}
	db.CreateBin(bin3)
	payload3 := &Payload{BlueprintID: bp.ID, BinID: &bin3.ID, Status: "available", UOPRemaining: 75}
	db.CreatePayload(payload3)

	fifo, err := db.FindSourcePayloadFIFO("CRATE-Y")
	if err != nil {
		t.Fatalf("FindSourcePayloadFIFO: %v", err)
	}
	// payload2 was created first at node1, should be returned (FIFO by delivered_at)
	if fifo.ID != payload2.ID {
		t.Errorf("FIFO payload ID = %d, want %d", fifo.ID, payload2.ID)
	}
}

func TestPayloadEventsCRUD(t *testing.T) {
	db := testDB(t)

	bt := &BinType{Code: "BOX-Z", Description: "Standard box", WidthIn: 10.0, HeightIn: 6.0}
	db.CreateBinType(bt)

	bp := &Blueprint{Code: "BOX-Z", UOPCapacity: 30}
	db.CreateBlueprint(bp)

	node := &Node{Name: "STORE-EVT", Enabled: true}
	db.CreateNode(node)

	bin := &Bin{BinTypeID: bt.ID, Label: "BZ-001", NodeID: &node.ID, Status: "available"}
	db.CreateBin(bin)

	payload := &Payload{BlueprintID: bp.ID, BinID: &bin.ID, Status: "available", UOPRemaining: 30}
	db.CreatePayload(payload) // This should auto-log a "created" event

	// Create additional events
	db.CreatePayloadEvent(&PayloadEvent{
		PayloadID: payload.ID,
		EventType: PayloadEventMoved,
		Detail:    "moved to node 5",
		Actor:     "system",
	})
	db.CreatePayloadEvent(&PayloadEvent{
		PayloadID: payload.ID,
		EventType: PayloadEventClaimed,
		Detail:    "order_id=10",
		Actor:     "dispatch",
	})

	// List events (reverse chronological, limit)
	events, err := db.ListPayloadEvents(payload.ID, 10)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	// Should have 3: created (auto), moved, claimed
	if len(events) != 3 {
		t.Fatalf("events len = %d, want 3", len(events))
	}
	// Verify all expected event types are present
	typeSet := map[string]bool{}
	for _, e := range events {
		typeSet[e.EventType] = true
	}
	for _, want := range []string{PayloadEventCreated, PayloadEventMoved, PayloadEventClaimed} {
		if !typeSet[want] {
			t.Errorf("missing event type %q in results", want)
		}
	}

	// Verify limit works
	limited, err := db.ListPayloadEvents(payload.ID, 2)
	if err != nil {
		t.Fatalf("list events limited: %v", err)
	}
	if len(limited) != 2 {
		t.Errorf("limited events len = %d, want 2", len(limited))
	}
}

func TestBlueprintManifestCRUD(t *testing.T) {
	db := testDB(t)

	bp := &Blueprint{Code: "KIT-M", UOPCapacity: 10}
	db.CreateBlueprint(bp)

	// Create 2 manifest items
	item1 := &BlueprintManifestItem{BlueprintID: bp.ID, PartNumber: "PN-001", Quantity: 5, Description: "Bolt M8"}
	if err := db.CreateBlueprintManifestItem(item1); err != nil {
		t.Fatalf("create item1: %v", err)
	}
	if item1.ID == 0 {
		t.Fatal("item1 ID should be assigned")
	}

	item2 := &BlueprintManifestItem{BlueprintID: bp.ID, PartNumber: "PN-002", Quantity: 10, Description: "Washer M8"}
	if err := db.CreateBlueprintManifestItem(item2); err != nil {
		t.Fatalf("create item2: %v", err)
	}

	// List (ordered by id)
	items, err := db.ListBlueprintManifest(bp.ID)
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
	if err := db.DeleteBlueprintManifestItem(item1.ID); err != nil {
		t.Fatalf("delete item: %v", err)
	}
	remaining, _ := db.ListBlueprintManifest(bp.ID)
	if len(remaining) != 1 {
		t.Errorf("remaining after delete = %d, want 1", len(remaining))
	}

	// ReplaceBlueprintManifest
	replacements := []*BlueprintManifestItem{
		{PartNumber: "PN-100", Quantity: 2, Description: "Nut M10"},
		{PartNumber: "PN-101", Quantity: 4, Description: "Screw M10"},
		{PartNumber: "PN-102", Quantity: 1, Description: "Bracket"},
	}
	if err := db.ReplaceBlueprintManifest(bp.ID, replacements); err != nil {
		t.Fatalf("replace manifest: %v", err)
	}
	replaced, _ := db.ListBlueprintManifest(bp.ID)
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

func TestNodeBlueprintAssignment(t *testing.T) {
	db := testDB(t)

	node := &Node{Name: "STORE-NB", Enabled: true}
	db.CreateNode(node)

	bp1 := &Blueprint{Code: "KIT-A", UOPCapacity: 10}
	db.CreateBlueprint(bp1)
	bp2 := &Blueprint{Code: "KIT-B", UOPCapacity: 20}
	db.CreateBlueprint(bp2)

	// Assign
	if err := db.AssignBlueprintToNode(node.ID, bp1.ID); err != nil {
		t.Fatalf("assign bp1: %v", err)
	}
	if err := db.AssignBlueprintToNode(node.ID, bp2.ID); err != nil {
		t.Fatalf("assign bp2: %v", err)
	}

	// List blueprints for node
	bps, err := db.ListBlueprintsForNode(node.ID)
	if err != nil {
		t.Fatalf("list blueprints for node: %v", err)
	}
	if len(bps) != 2 {
		t.Fatalf("blueprints len = %d, want 2", len(bps))
	}

	// List nodes for blueprint
	nodes, err := db.ListNodesForBlueprint(bp1.ID)
	if err != nil {
		t.Fatalf("list nodes for blueprint: %v", err)
	}
	if len(nodes) != 1 {
		t.Errorf("nodes len = %d, want 1", len(nodes))
	}

	// Unassign
	if err := db.UnassignBlueprintFromNode(node.ID, bp1.ID); err != nil {
		t.Fatalf("unassign: %v", err)
	}
	bps2, _ := db.ListBlueprintsForNode(node.ID)
	if len(bps2) != 1 {
		t.Errorf("blueprints after unassign = %d, want 1", len(bps2))
	}

	// SetNodeBlueprints (replace all)
	if err := db.SetNodeBlueprints(node.ID, []int64{bp1.ID, bp2.ID}); err != nil {
		t.Fatalf("set node blueprints: %v", err)
	}
	bps3, _ := db.ListBlueprintsForNode(node.ID)
	if len(bps3) != 2 {
		t.Errorf("blueprints after set = %d, want 2", len(bps3))
	}
}
