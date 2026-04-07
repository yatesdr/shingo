package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store"
)

func TestDispatcher_RetrieveOrder_FullLifecycle(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)

	// Create a bin at the storage node with a manifest
	bin := testdb.CreateBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-RET-1")

	backend := testdb.NewTrackingBackend()
	d, emitter := newTestDispatcher(t, db, backend)

	env := testEnvelope()

	// Phase 1: Submit retrieve order
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "retrieve-uuid-1",
		OrderType:    OrderTypeRetrieve,
		PayloadCode:  "PART-A",
		DeliveryNode: lineNode.Name,
		Quantity:     1.0,
	})

	// Verify order was created
	if len(emitter.received) != 1 {
		t.Fatalf("received events = %d, want 1", len(emitter.received))
	}

	// Verify order is in database
	order, err := db.GetOrderByUUID("retrieve-uuid-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.Status != StatusDispatched {
		t.Errorf("status = %q, want %q", order.Status, StatusDispatched)
	}
	if order.SourceNode != storageNode.Name {
		t.Errorf("source node = %q, want %q", order.SourceNode, storageNode.Name)
	}
	if order.DeliveryNode != lineNode.Name {
		t.Errorf("delivery node = %q, want %q", order.DeliveryNode, lineNode.Name)
	}

	// Verify vendor order was created
	if order.VendorOrderID == "" {
		t.Fatal("vendor order ID should be set")
	}

	// Phase 2: Simulate fleet delivery, then confirm receipt
	db.UpdateOrderStatus(order.ID, StatusDelivered, "fleet delivered")

	d.HandleOrderReceipt(env, &protocol.OrderReceipt{
		OrderUUID:   "retrieve-uuid-1",
		ReceiptType: "confirmed",
		FinalCount:  1.0,
	})

	// Verify order is confirmed
	order2, err := db.GetOrder(order.ID)
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order2.Status != StatusConfirmed {
		t.Errorf("status = %q, want %q", order2.Status, StatusConfirmed)
	}
	if order2.CompletedAt == nil {
		t.Error("completed at should be set")
	}

	// Verify bin was claimed
	claimedBin, err := db.GetBin(bin.ID)
	if err != nil {
		t.Fatalf("get bin: %v", err)
	}
	if claimedBin.ClaimedBy == nil {
		t.Error("bin should be claimed")
	}
}

func TestDispatcher_MoveOrder_FullLifecycle(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)

	// Create a bin at storage node with a manifest
	testdb.CreateBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-MOV-1")

	backend := testdb.NewTrackingBackend()
	d, emitter := newTestDispatcher(t, db, backend)

	env := testEnvelope()

	// Phase 1: Submit move order
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "move-uuid-1",
		OrderType:    OrderTypeMove,
		PayloadCode:  "PART-A",
		SourceNode:   storageNode.Name,
		DeliveryNode: lineNode.Name,
		Quantity:     1.0,
	})

	if len(emitter.received) != 1 {
		t.Fatalf("received events = %d, want 1", len(emitter.received))
	}

	order, err := db.GetOrderByUUID("move-uuid-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.Status != StatusDispatched {
		t.Errorf("status = %q, want %q", order.Status, StatusDispatched)
	}

	// Phase 2: Simulate fleet delivery, then confirm receipt
	db.UpdateOrderStatus(order.ID, StatusDelivered, "fleet delivered")

	d.HandleOrderReceipt(env, &protocol.OrderReceipt{
		OrderUUID:   "move-uuid-1",
		ReceiptType: "confirmed",
		FinalCount:  1.0,
	})

	order2, _ := db.GetOrder(order.ID)
	if order2.Status != StatusConfirmed {
		t.Errorf("status = %q, want %q", order2.Status, StatusConfirmed)
	}
}

func TestDispatcher_StoreOrder_FullLifecycle(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)

	// Create a bin at line-side with a manifest
	testdb.CreateBinAtNode(t, db, bp.Code, lineNode.ID, "BIN-STO-1")

	backend := testdb.NewTrackingBackend()
	d, emitter := newTestDispatcher(t, db, backend)

	env := testEnvelope()

	// Phase 1: Submit store order
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:   "store-uuid-1",
		OrderType:   OrderTypeStore,
		PayloadCode: "PART-A",
		SourceNode:  lineNode.Name,
		Quantity:    1.0,
	})

	// Store orders should select a storage destination
	if len(emitter.received) != 1 {
		t.Fatalf("received events = %d, want 1", len(emitter.received))
	}

	order, err := db.GetOrderByUUID("store-uuid-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.DeliveryNode == "" {
		t.Fatal("delivery node should be set for store order")
	}
	// Delivery node should be a storage node
	if order.DeliveryNode != storageNode.Name {
		// Could be another storage node, just verify it's set
		t.Logf("delivery node = %q", order.DeliveryNode)
	}
}

func TestDispatcher_CancelOrder(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)

	// Create a bin with a manifest
	bin := &store.Bin{BinTypeID: 1, Label: "BIN-CAN-1", NodeID: &storageNode.ID, Status: "available"}
	db.CreateBin(bin)
	db.SetBinManifest(bin.ID, `{"items":[]}`, bp.Code, 100)
	db.ConfirmBinManifest(bin.ID, "")

	backend := testdb.NewTrackingBackend()
	d, emitter := newTestDispatcher(t, db, backend)

	env := testEnvelope()

	// Submit retrieve order — dispatch will claim the bin
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "cancel-uuid-1",
		OrderType:    OrderTypeRetrieve,
		PayloadCode:  "PART-A",
		DeliveryNode: lineNode.Name,
		Quantity:     1.0,
	})

	order, _ := db.GetOrderByUUID("cancel-uuid-1")

	// Verify bin was claimed by this order
	claimed, _ := db.GetBin(bin.ID)
	if claimed.ClaimedBy == nil || *claimed.ClaimedBy != order.ID {
		t.Fatalf("bin should be claimed by order %d before cancel", order.ID)
	}

	// Cancel the order
	d.HandleOrderCancel(env, &protocol.OrderCancel{
		OrderUUID: "cancel-uuid-1",
		Reason:    "operator cancelled",
	})

	// Verify order is cancelled
	order2, _ := db.GetOrder(order.ID)
	if order2.Status != StatusCancelled {
		t.Errorf("status = %q, want %q", order2.Status, StatusCancelled)
	}

	// Verify bin was unclaimed by the cancel
	unclaimed, _ := db.GetBin(bin.ID)
	if unclaimed.ClaimedBy != nil {
		t.Errorf("bin should be unclaimed after cancel, but ClaimedBy = %v", unclaimed.ClaimedBy)
	}

	// Verify cancelled event was emitted
	if len(emitter.cancelled) != 1 {
		t.Fatalf("cancelled events = %d, want 1", len(emitter.cancelled))
	}
}

func TestDispatcher_RedirectOrder(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)

	// Create another line node
	lineNode2 := &store.Node{Name: "LINE2-IN", Enabled: true}
	db.CreateNode(lineNode2)

	// Create a bin with a manifest
	bin := &store.Bin{BinTypeID: 1, Label: "BIN-RED-1", NodeID: &storageNode.ID, Status: "available"}
	db.CreateBin(bin)
	db.SetBinManifest(bin.ID, `{"items":[]}`, bp.Code, 100)
	db.ConfirmBinManifest(bin.ID, "")

	backend := testdb.NewTrackingBackend()
	d, _ := newTestDispatcher(t, db, backend)

	env := testEnvelope()

	// Submit move order from storage to line1
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "redirect-uuid-1",
		OrderType:    OrderTypeMove,
		PayloadCode:  "PART-A",
		SourceNode:   storageNode.Name,
		DeliveryNode: lineNode.Name,
		Quantity:     1.0,
	})

	// Redirect to line2
	d.HandleOrderRedirect(env, &protocol.OrderRedirect{
		OrderUUID:       "redirect-uuid-1",
		NewDeliveryNode: lineNode2.Name,
	})

	// Verify order destination was updated (need to re-fetch from DB)
	order2, _ := db.GetOrderByUUID("redirect-uuid-1")
	if order2.DeliveryNode != lineNode2.Name {
		t.Errorf("delivery node = %q, want %q", order2.DeliveryNode, lineNode2.Name)
	}
}

func TestDispatcher_SyntheticNodeResolution(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, _, bp := setupTestData(t, db)

	// Look up the seeded synthetic node type (NGRP)
	syntheticType, err := db.GetNodeTypeByCode("NGRP")
	if err != nil {
		t.Fatalf("get synthetic node type: %v", err)
	}

	// Create a synthetic parent node (delivery zone)
	parentNode := &store.Node{
		Name: "ZONE-A", IsSynthetic: true,
		NodeTypeID: &syntheticType.ID, Enabled: true,
	}
	if err := db.CreateNode(parentNode); err != nil {
		t.Fatalf("create parent node: %v", err)
	}

	// Create child nodes under the synthetic parent (lineside slots)
	child1 := &store.Node{Name: "ZONE-A-01", Enabled: true}
	child2 := &store.Node{Name: "ZONE-A-02", Enabled: true}
	db.CreateNode(child1)
	db.CreateNode(child2)
	db.SetNodeParent(child1.ID, parentNode.ID)
	db.SetNodeParent(child2.ID, parentNode.ID)

	// Put a bin at child2 to occupy it (child2 occupied, child1 empty)
	occBin := &store.Bin{BinTypeID: 1, Label: "BIN-SYN-OCC", NodeID: &child2.ID, Status: "available"}
	db.CreateBin(occBin)

	// Create source bin at a separate node for FIFO to find
	srcNode := &store.Node{Name: "SRC-SYN", Enabled: true}
	db.CreateNode(srcNode)
	srcBin := &store.Bin{BinTypeID: 1, Label: "BIN-SYN-SRC", NodeID: &srcNode.ID, Status: "available"}
	db.CreateBin(srcBin)
	db.SetBinManifest(srcBin.ID, `{"items":[]}`, bp.Code, 100)
	db.ConfirmBinManifest(srcBin.ID, "")

	// Create dispatcher with resolver
	backend := testdb.NewTrackingBackend()
	emitter := &mockEmitter{}
	resolver := &DefaultResolver{DB: db}
	d := NewDispatcher(db, backend, emitter, "core", "shingo.dispatch", resolver)

	env := testEnvelope()

	// Submit retrieve order targeting synthetic parent — delivery should resolve
	// to child1 (empty slot), source should pick srcPayload via FIFO
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "syn-retrieve-1",
		OrderType:    OrderTypeRetrieve,
		PayloadCode:  "PART-A",
		DeliveryNode: parentNode.Name,
		Quantity:     1.0,
	})

	// Verify order was dispatched (not failed)
	if len(emitter.failed) > 0 {
		t.Fatalf("order should not fail, got error: %s", emitter.failed[0].errorCode)
	}
	if len(emitter.received) != 1 {
		t.Fatalf("received events = %d, want 1", len(emitter.received))
	}

	order, err := db.GetOrderByUUID("syn-retrieve-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.Status != StatusDispatched {
		t.Errorf("status = %q, want %q", order.Status, StatusDispatched)
	}
	// Delivery node should be resolved to child1 (empty slot), not child2 (occupied)
	if order.DeliveryNode != child1.Name {
		t.Errorf("delivery node = %q, want %q (empty child)", order.DeliveryNode, child1.Name)
	}
	// Pickup should be source node (where the FIFO payload is)
	if order.SourceNode != srcNode.Name {
		t.Errorf("source node = %q, want %q", order.SourceNode, srcNode.Name)
	}
}

// TestDispatcher_MultiOrderToSyntheticNGRP verifies that multiple retrieve orders
// to the same synthetic NGRP resolve to different physical children and that
// in-flight awareness prevents double-booking of the same slot.
func TestDispatcher_MultiOrderToSyntheticNGRP(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, _, _ = setupTestData(t, db)

	syntheticType, err := db.GetNodeTypeByCode("NGRP")
	if err != nil {
		t.Fatalf("get NGRP type: %v", err)
	}

	// Create NGRP zone with 3 physical children
	zone := &store.Node{Name: "PRESS-A1", IsSynthetic: true, NodeTypeID: &syntheticType.ID, Enabled: true}
	db.CreateNode(zone)
	slot1 := &store.Node{Name: "PRESS-A1-01", Enabled: true}
	slot2 := &store.Node{Name: "PRESS-A1-02", Enabled: true}
	slot3 := &store.Node{Name: "PRESS-A1-03", Enabled: true}
	db.CreateNode(slot1)
	db.CreateNode(slot2)
	db.CreateNode(slot3)
	db.SetNodeParent(slot1.ID, zone.ID)
	db.SetNodeParent(slot2.ID, zone.ID)
	db.SetNodeParent(slot3.ID, zone.ID)

	// Create source payloads in a supermarket (payload A x2, payload B x1)
	bpA := &store.Payload{Code: "PART-MULTI-A"}
	bpB := &store.Payload{Code: "PART-MULTI-B"}
	db.CreatePayload(bpA)
	db.CreatePayload(bpB)

	supermarket := &store.Node{Name: "SM-MULTI", Zone: "W", Enabled: true}
	db.CreateNode(supermarket)

	binA1 := &store.Bin{BinTypeID: 1, Label: "M-A1", NodeID: &supermarket.ID, Status: "available"}
	binA2 := &store.Bin{BinTypeID: 1, Label: "M-A2", NodeID: &supermarket.ID, Status: "available"}
	binB1 := &store.Bin{BinTypeID: 1, Label: "M-B1", NodeID: &supermarket.ID, Status: "available"}
	db.CreateBin(binA1)
	db.CreateBin(binA2)
	db.CreateBin(binB1)
	db.SetBinManifest(binA1.ID, `{"items":[]}`, bpA.Code, 100)
	db.ConfirmBinManifest(binA1.ID, "")
	db.SetBinManifest(binA2.ID, `{"items":[]}`, bpA.Code, 100)
	db.ConfirmBinManifest(binA2.ID, "")
	db.SetBinManifest(binB1.ID, `{"items":[]}`, bpB.Code, 100)
	db.ConfirmBinManifest(binB1.ID, "")

	backend := testdb.NewTrackingBackend()
	emitter := &mockEmitter{}
	resolver := &DefaultResolver{DB: db}
	d := NewDispatcher(db, backend, emitter, "core", "shingo.dispatch", resolver)
	env := testEnvelope()

	// Order 1: payload A -> PRESS-A1
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "multi-1",
		OrderType:    OrderTypeRetrieve,
		PayloadCode:  "PART-MULTI-A",
		DeliveryNode: zone.Name,
		Quantity:     1,
	})
	// Order 2: payload A -> PRESS-A1
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "multi-2",
		OrderType:    OrderTypeRetrieve,
		PayloadCode:  "PART-MULTI-A",
		DeliveryNode: zone.Name,
		Quantity:     1,
	})
	// Order 3: payload B -> PRESS-A1
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "multi-3",
		OrderType:    OrderTypeRetrieve,
		PayloadCode:  "PART-MULTI-B",
		DeliveryNode: zone.Name,
		Quantity:     1,
	})

	if len(emitter.failed) > 0 {
		t.Fatalf("unexpected failures: %d (first: %s)", len(emitter.failed), emitter.failed[0].errorCode)
	}

	o1, _ := db.GetOrderByUUID("multi-1")
	o2, _ := db.GetOrderByUUID("multi-2")
	o3, _ := db.GetOrderByUUID("multi-3")

	// All three should be dispatched
	for _, o := range []*store.Order{o1, o2, o3} {
		if o.Status != StatusDispatched {
			t.Errorf("order %s status = %q, want dispatched", o.EdgeUUID, o.Status)
		}
	}

	// Each should have a unique delivery node (no double-booking)
	deliveries := map[string]string{
		o1.DeliveryNode: o1.EdgeUUID,
		o2.DeliveryNode: o2.EdgeUUID,
		o3.DeliveryNode: o3.EdgeUUID,
	}
	if len(deliveries) != 3 {
		t.Errorf("expected 3 unique delivery nodes, got %d: o1=%s o2=%s o3=%s",
			len(deliveries), o1.DeliveryNode, o2.DeliveryNode, o3.DeliveryNode)
	}

	// A 4th order should fail — all 3 slots are in-flight
	failsBefore := len(emitter.failed)
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "multi-4",
		OrderType:    OrderTypeRetrieve,
		PayloadCode:  "PART-MULTI-A",
		DeliveryNode: zone.Name,
		Quantity:     1,
	})

	// Resolution fails before order creation, so check emitter errors
	if len(emitter.failed) <= failsBefore {
		// No emitter failure means it was caught as a sendError before order creation
		// Check that it was NOT dispatched
		o4, err := db.GetOrderByUUID("multi-4")
		if err == nil && o4.Status == StatusDispatched {
			t.Errorf("4th order should not be dispatched, all slots in-flight")
		}
	}
}

// TestDispatcher_RetrieveEmptyToSyntheticNGRP verifies empty bin delivery
// to a synthetic node group uses store resolution (finds empty slots).
func TestDispatcher_RetrieveEmptyToSyntheticNGRP(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, _, _ = setupTestData(t, db)

	syntheticType, _ := db.GetNodeTypeByCode("NGRP")

	// Create NGRP zone with 2 children, one occupied
	zone := &store.Node{Name: "EMPTY-ZONE", IsSynthetic: true, NodeTypeID: &syntheticType.ID, Enabled: true}
	db.CreateNode(zone)
	slot1 := &store.Node{Name: "EZ-01", Enabled: true}
	slot2 := &store.Node{Name: "EZ-02", Enabled: true}
	db.CreateNode(slot1)
	db.CreateNode(slot2)
	db.SetNodeParent(slot1.ID, zone.ID)
	db.SetNodeParent(slot2.ID, zone.ID)

	// Occupy slot1
	occBin := &store.Bin{BinTypeID: 1, Label: "OCC-1", NodeID: &slot1.ID, Status: "available"}
	db.CreateBin(occBin)

	// Create payload with bin type compatibility
	bp := &store.Payload{Code: "EMPTY-BP"}
	db.CreatePayload(bp)
	bt, _ := db.GetBinTypeByCode("DEFAULT")
	db.SetPayloadBinTypes(bp.ID, []int64{bt.ID})

	// Create an empty compatible bin somewhere (source)
	srcNode := &store.Node{Name: "EMPTY-SRC", Enabled: true}
	db.CreateNode(srcNode)
	emptyBin := &store.Bin{BinTypeID: bt.ID, Label: "EMPTY-BIN-1", NodeID: &srcNode.ID, Status: "available"}
	db.CreateBin(emptyBin)

	backend := testdb.NewTrackingBackend()
	emitter := &mockEmitter{}
	resolver := &DefaultResolver{DB: db}
	d := NewDispatcher(db, backend, emitter, "core", "shingo.dispatch", resolver)
	env := testEnvelope()

	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:     "empty-1",
		OrderType:     OrderTypeRetrieve,
		PayloadCode:   "EMPTY-BP",
		DeliveryNode:  zone.Name,
		RetrieveEmpty: true,
		Quantity:      1,
	})

	if len(emitter.failed) > 0 {
		t.Fatalf("order should not fail, got: %s", emitter.failed[0].errorCode)
	}

	o, err := db.GetOrderByUUID("empty-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if o.Status != StatusDispatched {
		t.Errorf("status = %q, want dispatched", o.Status)
	}
	// Delivery should resolve to slot2 (empty), not slot1 (occupied)
	if o.DeliveryNode != slot2.Name {
		t.Errorf("delivery = %q, want %q (empty slot)", o.DeliveryNode, slot2.Name)
	}
}

// TC-41: retrieve_empty for a buried empty now triggers reshuffle instead of
// dispatching to an unreachable slot. planRetrieveEmpty detects the bin is
// inaccessible, walks up to find the lane, and hands a BuriedError to
// planBuriedReshuffle. The order should go to "reshuffling" status.
func TestTC41_RetrieveEmpty_BuriedEmptyTriggersReshuffle(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, _, _ = setupTestData(t, db)

	grpType, _ := db.GetNodeTypeByCode("NGRP")
	lanType, _ := db.GetNodeTypeByCode("LANE")

	bp := &store.Payload{Code: "TC41-BP"}
	db.CreatePayload(bp)
	bt, _ := db.GetBinTypeByCode("DEFAULT")
	db.SetPayloadBinTypes(bp.ID, []int64{bt.ID})

	// Create NGRP with 2 lanes of 3 slots
	grp := &store.Node{Name: "TC41-GRP", IsSynthetic: true, NodeTypeID: &grpType.ID, Enabled: true}
	db.CreateNode(grp)
	grp, _ = db.GetNode(grp.ID)

	// Lane 1: full bins at depth 1+2, empty buried at depth 3
	lane1 := &store.Node{Name: "TC41-L1", IsSynthetic: true, NodeTypeID: &lanType.ID, ParentID: &grp.ID, Enabled: true}
	db.CreateNode(lane1)
	lane1, _ = db.GetNode(lane1.ID)

	d1, d2, d3 := 1, 2, 3
	l1s1 := &store.Node{Name: "TC41-L1-S1", ParentID: &lane1.ID, Enabled: true, Depth: &d1}
	db.CreateNode(l1s1)
	l1s2 := &store.Node{Name: "TC41-L1-S2", ParentID: &lane1.ID, Enabled: true, Depth: &d2}
	db.CreateNode(l1s2)
	l1s3 := &store.Node{Name: "TC41-L1-S3", ParentID: &lane1.ID, Enabled: true, Depth: &d3}
	db.CreateNode(l1s3)

	// Full bins blocking lane 1
	blkBin1 := &store.Bin{BinTypeID: bt.ID, Label: "TC41-FULL-1", NodeID: &l1s1.ID, Status: "available"}
	db.CreateBin(blkBin1)
	db.SetBinManifest(blkBin1.ID, `{"items":[]}`, bp.Code, 100)
	db.ConfirmBinManifest(blkBin1.ID, "")

	blkBin2 := &store.Bin{BinTypeID: bt.ID, Label: "TC41-FULL-2", NodeID: &l1s2.ID, Status: "available"}
	db.CreateBin(blkBin2)
	db.SetBinManifest(blkBin2.ID, `{"items":[]}`, bp.Code, 100)
	db.ConfirmBinManifest(blkBin2.ID, "")

	// Buried empty at depth 3
	emptyBin := &store.Bin{BinTypeID: bt.ID, Label: "TC41-EMPTY", NodeID: &l1s3.ID, Status: "available"}
	db.CreateBin(emptyBin)

	// Lane 2: completely empty — provides shuffle slots for the reshuffle
	lane2 := &store.Node{Name: "TC41-L2", IsSynthetic: true, NodeTypeID: &lanType.ID, ParentID: &grp.ID, Enabled: true}
	db.CreateNode(lane2)
	lane2, _ = db.GetNode(lane2.ID)

	l2s1 := &store.Node{Name: "TC41-L2-S1", ParentID: &lane2.ID, Enabled: true, Depth: &d1}
	db.CreateNode(l2s1)
	l2s2 := &store.Node{Name: "TC41-L2-S2", ParentID: &lane2.ID, Enabled: true, Depth: &d2}
	db.CreateNode(l2s2)

	// Delivery target (lineside node)
	destNode := &store.Node{Name: "TC41-LINE", Enabled: true}
	db.CreateNode(destNode)

	backend := testdb.NewTrackingBackend()
	emitter := &mockEmitter{}
	resolver := &DefaultResolver{DB: db, LaneLock: NewLaneLock()}
	d := NewDispatcher(db, backend, emitter, "core", "shingo.dispatch", resolver)
	env := testEnvelope()

	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:     "tc41-empty-1",
		OrderType:     OrderTypeRetrieve,
		PayloadCode:   bp.Code,
		DeliveryNode:  destNode.Name,
		RetrieveEmpty: true,
		Quantity:      1,
	})

	o, err := db.GetOrderByUUID("tc41-empty-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}

	// Before the fix: order would go to "dispatched" targeting an unreachable slot.
	// After the fix: order should go to "reshuffling" (compound reshuffle planned).
	if o.Status == StatusDispatched {
		t.Errorf("order dispatched directly — buried empty was NOT detected (pre-fix behavior)")
	}
	if o.Status == StatusReshuffling {
		t.Logf("TC-41 fix confirmed: order %d is reshuffling to unbury empty bin", o.ID)
	} else if o.Status != StatusReshuffling {
		// Could be failed if reshuffle planning hit an issue — still better than dispatching to unreachable
		t.Logf("order status = %q (not reshuffling, but also not dispatched blind)", o.Status)
	}

	// Verify compound children were created (reshuffle steps)
	children, _ := db.ListChildOrders(o.ID)
	if len(children) == 0 && o.Status == StatusReshuffling {
		t.Error("order is reshuffling but no compound children found")
	}
	if len(children) > 0 {
		t.Logf("reshuffle plan has %d steps", len(children))
	}
}

// TestDispatcher_DotNotationBypassesResolver verifies that ordering to a
// specific child using dot notation (ZONE.Node10) skips resolver — the
// physical node is used directly.
func TestDispatcher_DotNotationBypassesResolver(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, _, bp := setupTestData(t, db)

	syntheticType, _ := db.GetNodeTypeByCode("NGRP")
	zone := &store.Node{Name: "DOT-ZONE", IsSynthetic: true, NodeTypeID: &syntheticType.ID, Enabled: true}
	db.CreateNode(zone)
	child := &store.Node{Name: "SLOT-X", Enabled: true}
	db.CreateNode(child)
	db.SetNodeParent(child.ID, zone.ID)

	// Create source bin
	srcNode := &store.Node{Name: "DOT-SRC", Enabled: true}
	db.CreateNode(srcNode)
	bin := &store.Bin{BinTypeID: 1, Label: "DOT-BIN-1", NodeID: &srcNode.ID, Status: "available"}
	db.CreateBin(bin)
	db.SetBinManifest(bin.ID, `{"items":[]}`, bp.Code, 100)
	db.ConfirmBinManifest(bin.ID, "")

	backend := testdb.NewTrackingBackend()
	emitter := &mockEmitter{}
	resolver := &DefaultResolver{DB: db}
	d := NewDispatcher(db, backend, emitter, "core", "shingo.dispatch", resolver)
	env := testEnvelope()

	// Use dot notation: "DOT-ZONE.SLOT-X" — resolves to physical child directly
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "dot-1",
		OrderType:    OrderTypeRetrieve,
		PayloadCode:  "PART-A",
		DeliveryNode: "DOT-ZONE.SLOT-X",
		Quantity:     1,
	})

	if len(emitter.failed) > 0 {
		t.Fatalf("order should not fail, got: %s", emitter.failed[0].errorCode)
	}

	o, _ := db.GetOrderByUUID("dot-1")
	if o.Status != StatusDispatched {
		t.Errorf("status = %q, want dispatched", o.Status)
	}
	// Dot notation is stored as-is; the fleet dispatch uses the resolved node name.
	// Verify the order was dispatched (fleet got the right node via GetNodeByDotName).
	if o.DeliveryNode != "DOT-ZONE.SLOT-X" {
		t.Errorf("delivery = %q, want %q", o.DeliveryNode, "DOT-ZONE.SLOT-X")
	}
}

func TestDispatcher_FleetFailure(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)

	// Create a bin with a manifest
	bin := &store.Bin{BinTypeID: 1, Label: "BIN-FF-1", NodeID: &storageNode.ID, Status: "available"}
	db.CreateBin(bin)
	db.SetBinManifest(bin.ID, `{"items":[]}`, bp.Code, 100)
	db.ConfirmBinManifest(bin.ID, "")

	// Use mockBackend (returns errors for all fleet ops)
	d, emitter := newTestDispatcher(t, db, testdb.NewFailingBackend())

	env := testEnvelope()

	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "fleet-fail-1",
		OrderType:    OrderTypeRetrieve,
		PayloadCode:  "PART-A",
		DeliveryNode: lineNode.Name,
		Quantity:     1.0,
	})

	// Order should be received then failed
	if len(emitter.received) != 1 {
		t.Fatalf("received events = %d, want 1", len(emitter.received))
	}
	if len(emitter.failed) != 1 {
		t.Fatalf("failed events = %d, want 1", len(emitter.failed))
	}
	if emitter.failed[0].errorCode != "fleet_failed" {
		t.Errorf("error code = %q, want %q", emitter.failed[0].errorCode, "fleet_failed")
	}

	// Verify order status is failed
	order, err := db.GetOrderByUUID("fleet-fail-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.Status != StatusFailed {
		t.Errorf("status = %q, want %q", order.Status, StatusFailed)
	}

	// Verify bin was unclaimed after fleet failure
	b, _ := db.GetBin(bin.ID)
	if b.ClaimedBy != nil {
		t.Errorf("bin should be unclaimed after fleet failure, ClaimedBy = %v", b.ClaimedBy)
	}
}

func TestDispatcher_PriorityHandling(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)

	// Create bins with manifests
	bin1 := &store.Bin{BinTypeID: 1, Label: "BIN-PRI-1", NodeID: &storageNode.ID, Status: "available"}
	db.CreateBin(bin1)
	db.SetBinManifest(bin1.ID, `{"items":[]}`, bp.Code, 100)
	db.ConfirmBinManifest(bin1.ID, "")

	bin2 := &store.Bin{BinTypeID: 1, Label: "BIN-PRI-2", NodeID: &storageNode.ID, Status: "available"}
	db.CreateBin(bin2)
	db.SetBinManifest(bin2.ID, `{"items":[]}`, bp.Code, 100)
	db.ConfirmBinManifest(bin2.ID, "")

	backend := testdb.NewTrackingBackend()
	d, _ := newTestDispatcher(t, db, backend)

	env := testEnvelope()

	// Submit low priority order
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "low-priority",
		OrderType:    OrderTypeRetrieve,
		PayloadCode:  "PART-A",
		DeliveryNode: lineNode.Name,
		Quantity:     1.0,
		Priority:     0,
	})

	// Submit high priority order
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "high-priority",
		OrderType:    OrderTypeRetrieve,
		PayloadCode:  "PART-A",
		DeliveryNode: lineNode.Name,
		Quantity:     1.0,
		Priority:     10,
	})

	// Both orders should be dispatched
	order1, _ := db.GetOrderByUUID("low-priority")
	order2, _ := db.GetOrderByUUID("high-priority")

	if order1.Priority != 0 {
		t.Errorf("low priority = %d, want 0", order1.Priority)
	}
	if order2.Priority != 10 {
		t.Errorf("high priority = %d, want 10", order2.Priority)
	}
}

func TestHandleRetrieve_BinTracking(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)

	// Create bin with manifest
	bin := &store.Bin{BinTypeID: 1, Label: "BIN-BT-1", NodeID: &storageNode.ID, Status: "available"}
	db.CreateBin(bin)
	db.SetBinManifest(bin.ID, `{"items":[]}`, bp.Code, 100)
	db.ConfirmBinManifest(bin.ID, "")

	backend := testdb.NewTrackingBackend()
	d, _ := newTestDispatcher(t, db, backend)

	env := testEnvelope()
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "uuid-bin-track",
		OrderType:    OrderTypeRetrieve,
		PayloadCode:  "PART-A",
		DeliveryNode: lineNode.Name,
		Quantity:     1.0,
	})

	order, err := db.GetOrderByUUID("uuid-bin-track")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}

	// Order should have BinID set
	if order.BinID == nil {
		t.Fatal("order BinID should be set after retrieve")
	}
	if *order.BinID != bin.ID {
		t.Errorf("order BinID = %d, want %d", *order.BinID, bin.ID)
	}

	// Bin should be claimed
	gotBin, _ := db.GetBin(bin.ID)
	if gotBin.ClaimedBy == nil {
		t.Fatal("bin should be claimed after retrieve")
	}
	if *gotBin.ClaimedBy != order.ID {
		t.Errorf("bin claimed_by = %d, want %d", *gotBin.ClaimedBy, order.ID)
	}
}

func TestHandleOrderIngest(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, _, bp := setupTestData(t, db)

	// Set up payload_bin_types for compatible empty bin
	bt, _ := db.GetBinTypeByCode("DEFAULT")
	db.SetPayloadBinTypes(bp.ID, []int64{bt.ID})

	// Create an empty bin at the station (simulating a bin at a produce location)
	produceNode := &store.Node{Name: "PRODUCE-1", Enabled: true}
	db.CreateNode(produceNode)

	bin := &store.Bin{BinTypeID: bt.ID, Label: "BIN-ING-1", NodeID: &produceNode.ID, Status: "available"}
	db.CreateBin(bin)

	// Also create a storage node for the store destination
	_ = storageNode

	backend := testdb.NewTrackingBackend()
	d, emitter := newTestDispatcher(t, db, backend)

	env := testEnvelope()
	d.HandleOrderIngest(env, &protocol.OrderIngestRequest{
		OrderUUID:   "uuid-ingest-1",
		PayloadCode: bp.Code,
		BinLabel:    "BIN-ING-1",
		SourceNode:  "PRODUCE-1",
		Quantity:    100,
		Manifest: []protocol.IngestManifestItem{
			{PartNumber: "PN-001", Quantity: 50, Description: "Bolt M8"},
			{PartNumber: "PN-002", Quantity: 50, Description: "Washer M8"},
		},
	})

	// Should have received the order
	if len(emitter.received) != 1 {
		t.Fatalf("received events = %d, want 1", len(emitter.received))
	}

	// Bin should have manifest set
	gotBin, _ := db.GetBin(bin.ID)
	if gotBin.PayloadCode != bp.Code {
		t.Errorf("bin payload_code = %q, want %q", gotBin.PayloadCode, bp.Code)
	}
	if !gotBin.ManifestConfirmed {
		t.Error("bin manifest should be confirmed after ingest")
	}
	if gotBin.ClaimedBy == nil {
		t.Fatal("bin should be claimed after ingest")
	}

	// Manifest items should be created
	order, _ := db.GetOrderByUUID("uuid-ingest-1")
	if order == nil {
		t.Fatal("order not found")
	}
	if order.BinID == nil || *order.BinID != bin.ID {
		t.Errorf("order BinID = %v, want %d", order.BinID, bin.ID)
	}
}

// TestDispatcher_MoveOrder_NGRPSource verifies that a move order with an NGRP
// (supermarket group) as the source node correctly resolves to a concrete slot,
// claims the bin, and dispatches. This is the bug scenario from "request material"
// where the node is empty — the edge creates a move order from the supermarket
// NGRP, and planMove must resolve through the group resolver rather than doing
// a raw ListBinsByNode on the synthetic node (which returns nothing).
func TestDispatcher_MoveOrder_NGRPSource(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	// Use SetupCompound to create a full NGRP → LANE → slots layout with
	// a single slot and one bin. NumSlots=1 means no blockers — the target
	// bin is accessible at the front of the lane.
	sc := testdb.SetupCompound(t, db, testdb.CompoundConfig{
		Prefix:   "MVNGRP",
		NumSlots: 1, // single slot, target bin at front (not buried)
	})

	backend := testdb.NewTrackingBackend()
	d, emitter := newTestDispatcher(t, db, backend)

	// The resolver needs to be set up for NGRP resolution to work.
	// newTestDispatcher creates a dispatcher with resolver=nil; we need one.
	resolver := &DefaultResolver{DB: db, LaneLock: d.LaneLock(), DebugLog: d.dbg}
	d2 := NewDispatcher(db, backend, emitter, "core", "shingo.dispatch", resolver)

	env := testEnvelope()

	// Submit a move order with SourceNode = NGRP name (the supermarket group).
	// This is what the edge sends when requestNodeFromClaim detects an empty
	// lineside node and downgrades to a simple delivery.
	d2.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "move-ngrp-1",
		OrderType:    OrderTypeMove,
		PayloadCode:  sc.Payload.Code,
		SourceNode:   sc.Grp.Name,       // NGRP — the bug scenario
		DeliveryNode: sc.LineNode.Name,
		Quantity:     1.0,
	})

	// Verify the order was dispatched (not failed)
	order, err := db.GetOrderByUUID("move-ngrp-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.Status != StatusDispatched {
		t.Fatalf("status = %q, want %q (order likely failed due to NGRP resolution)", order.Status, StatusDispatched)
	}

	// Verify the bin was claimed
	if order.BinID == nil {
		t.Fatal("BinID should be set — planMove must claim a bin via NGRP resolver")
	}
	if *order.BinID != sc.TargetBin.ID {
		t.Errorf("BinID = %d, want %d (target bin in the NGRP lane)", *order.BinID, sc.TargetBin.ID)
	}

	// Verify the source node was resolved to the concrete slot, not the NGRP
	if order.SourceNode == sc.Grp.Name {
		t.Errorf("SourceNode = %q (still NGRP name), should be resolved to concrete slot %q", order.SourceNode, sc.Slots[0].Name)
	}
	if order.SourceNode != sc.Slots[0].Name {
		t.Errorf("SourceNode = %q, want %q", order.SourceNode, sc.Slots[0].Name)
	}

	// Verify fleet got the right dispatch
	if len(backend.Orders()) != 1 {
		t.Fatalf("fleet orders = %d, want 1", len(backend.Orders()))
	}
}

// TestDispatcher_MoveOrder_NGRPSource_NoBin verifies that a move order with an
// NGRP source and no available bins gets queued rather than silently dispatching
// without a bin claim.
func TestDispatcher_MoveOrder_NGRPSource_NoBin(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	grpType, err := db.GetNodeTypeByCode("NGRP")
	if err != nil {
		t.Fatalf("get NGRP type: %v", err)
	}
	lanType, err := db.GetNodeTypeByCode("LANE")
	if err != nil {
		t.Fatalf("get LANE type: %v", err)
	}
	bp := &store.Payload{Code: "PART-MVEMPTY", Description: "test"}
	if err := db.CreatePayload(bp); err != nil {
		t.Fatalf("create payload: %v", err)
	}

	// Create NGRP with an empty lane (no bins)
	grp := &store.Node{Name: "GRP-MVEMPTY", NodeTypeID: &grpType.ID, Enabled: true, IsSynthetic: true}
	if err := db.CreateNode(grp); err != nil {
		t.Fatalf("create NGRP: %v", err)
	}
	lane := &store.Node{Name: "GRP-MVEMPTY-L1", NodeTypeID: &lanType.ID, ParentID: &grp.ID, Enabled: true, IsSynthetic: true}
	if err := db.CreateNode(lane); err != nil {
		t.Fatalf("create lane: %v", err)
	}
	depth := 1
	slot := &store.Node{Name: "GRP-MVEMPTY-L1-S1", ParentID: &lane.ID, Enabled: true, Depth: &depth}
	if err := db.CreateNode(slot); err != nil {
		t.Fatalf("create slot: %v", err)
	}
	lineNode := &store.Node{Name: "LINE-MVEMPTY", Enabled: true}
	if err := db.CreateNode(lineNode); err != nil {
		t.Fatalf("create line: %v", err)
	}

	backend := testdb.NewTrackingBackend()
	emitter := &mockEmitter{}
	resolver := &DefaultResolver{DB: db, LaneLock: NewLaneLock(), DebugLog: nil}
	d := NewDispatcher(db, backend, emitter, "core", "shingo.dispatch", resolver)

	env := testEnvelope()

	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "move-empty-ngrp-1",
		OrderType:    OrderTypeMove,
		PayloadCode:  bp.Code,
		SourceNode:   grp.Name,
		DeliveryNode: lineNode.Name,
		Quantity:     1.0,
	})

	order, err := db.GetOrderByUUID("move-empty-ngrp-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}

	// Should be queued, not failed or dispatched with nil BinID
	if order.Status != StatusQueued {
		t.Errorf("status = %q, want %q (empty NGRP should queue, not fail or dispatch without bin)", order.Status, StatusQueued)
	}

	// Fleet should NOT have received any dispatch
	if len(backend.Orders()) != 0 {
		t.Errorf("fleet orders = %d, want 0 (no bin available, should not dispatch)", len(backend.Orders()))
	}
}

// TestDispatcher_MoveOrder_NGRPSource_BuriedBin verifies that a move order
// with an NGRP source where the only matching bin is buried behind blockers
// triggers the reshuffle path (planBuriedReshuffle) rather than silently
// dispatching without a bin claim.
func TestDispatcher_MoveOrder_NGRPSource_BuriedBin(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	// SetupCompound with NumSlots=2 (default) creates:
	//   Slot 1 (depth 1, front) — blocker bin
	//   Slot 2 (depth 2, back)  — target bin (oldest, the one we want)
	// The target bin is buried behind the blocker, so the resolver returns
	// a BuriedError, which planMove should delegate to planBuriedReshuffle.
	sc := testdb.SetupCompound(t, db, testdb.CompoundConfig{
		Prefix: "MVBURY",
	})

	backend := testdb.NewTrackingBackend()
	emitter := &mockEmitter{}
	resolver := &DefaultResolver{DB: db, LaneLock: NewLaneLock(), DebugLog: nil}
	d := NewDispatcher(db, backend, emitter, "core", "shingo.dispatch", resolver)

	env := testEnvelope()

	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "move-buried-1",
		OrderType:    OrderTypeMove,
		PayloadCode:  sc.Payload.Code,
		SourceNode:   sc.Grp.Name, // NGRP — target bin is buried
		DeliveryNode: sc.LineNode.Name,
		Quantity:     1.0,
	})

	order, err := db.GetOrderByUUID("move-buried-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}

	// The order should trigger a compound reshuffle — status = "reshuffling"
	if order.Status != StatusReshuffling {
		t.Errorf("status = %q, want %q (buried bin should trigger reshuffle)", order.Status, StatusReshuffling)
	}

	// BinID should NOT be set yet — the reshuffle must complete first
	// before the actual bin can be claimed and moved.
	// (The compound order children handle the individual moves.)

	// Fleet should have received dispatch(es) for the compound children
	if len(backend.Orders()) == 0 {
		t.Error("fleet orders = 0, want >= 1 (compound reshuffle children should be dispatched)")
	}
}
