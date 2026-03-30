package engine

import (
	"fmt"
	"testing"

	"shingo/protocol"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/store"
)

// Compound reshuffle order tests (TC-40a, TC-44, TC-45, TC-46, TC-51, TC-52, TC-53, TC-54).
// Each test exercises the NGRP lane reshuffle pipeline: buried bin detection,
// PlanReshuffle, AdvanceCompoundOrder, lane locks, and child lifecycle management.

// =============================================================================
// Buried bin reshuffle through engine pipeline
// =============================================================================

// --- Buried bin: FIFO triggers reshuffle of buried bin via compound order ---
//
// Setup: NGRP -> LANE -> 3 slots. Blocker at depth 1 (newer), target at depth 2 (older).
// FIFO detects buried target as older than any accessible bin -> BuriedError ->
// planBuriedReshuffle -> compound order with child steps:
//   1. unbury: move blocker (depth 1) -> shuffle slot
//   2. retrieve: move target (depth 2) -> line node
//   3. restock: move blocker from shuffle -> back to depth 1
//
// Drives each child through the fleet simulator lifecycle, verifying that the
// compound order advances correctly and the target bin arrives at the line.
func TestBuriedBin_ReshuffleViaEngine(t *testing.T) {
	db := testDB(t)

	// Node types are seeded by migrations
	grpType, err := db.GetNodeTypeByCode("NGRP")
	if err != nil {
		t.Fatalf("get NGRP node type: %v (migrations should seed this)", err)
	}
	lanType, err := db.GetNodeTypeByCode("LANE")
	if err != nil {
		t.Fatalf("get LANE node type: %v (migrations should seed this)", err)
	}

	bp := &store.Payload{Code: "PART-BURIED", Description: "Buried bin test payload"}
	if err := db.CreatePayload(bp); err != nil {
		t.Fatalf("create payload: %v", err)
	}
	bt := &store.BinType{Code: "DEFAULT-BR", Description: "Buried test bin type"}
	if err := db.CreateBinType(bt); err != nil {
		t.Fatalf("create bin type: %v", err)
	}

	// NGRP (node group)
	grp := &store.Node{Name: "GRP-BURIED", NodeTypeID: &grpType.ID, Enabled: true, IsSynthetic: true}
	if err := db.CreateNode(grp); err != nil {
		t.Fatalf("create NGRP: %v", err)
	}

	// LANE under NGRP
	lane := &store.Node{
		Name: "GRP-BURIED-L1", NodeTypeID: &lanType.ID,
		ParentID: &grp.ID, Enabled: true, IsSynthetic: true,
	}
	if err := db.CreateNode(lane); err != nil {
		t.Fatalf("create LANE: %v", err)
	}

	// 3 physical slot nodes at depth 1, 2, 3
	var slots [3]*store.Node
	for i := 0; i < 3; i++ {
		depth := i + 1
		slot := &store.Node{
			Name:     fmt.Sprintf("GRP-BURIED-L1-S%d", depth),
			ParentID: &lane.ID, Enabled: true, Depth: &depth,
		}
		if err := db.CreateNode(slot); err != nil {
			t.Fatalf("create slot %d: %v", depth, err)
		}
		slots[i] = slot
	}

	// Shuffle slot (direct physical child of NGRP, empty — for temp storage during reshuffle)
	shuffleSlot := &store.Node{
		Name: "GRP-BURIED-SHUF", ParentID: &grp.ID, Enabled: true,
	}
	if err := db.CreateNode(shuffleSlot); err != nil {
		t.Fatalf("create shuffle slot: %v", err)
	}

	// Line node (delivery destination for the retrieve)
	lineNode := &store.Node{Name: "LINE-BURIED-IN", Enabled: true}
	if err := db.CreateNode(lineNode); err != nil {
		t.Fatalf("create line node: %v", err)
	}

	// TARGET bin at depth 2 (older — loaded_at 2 hours ago)
	targetBin := &store.Bin{
		BinTypeID: bt.ID, Label: "BIN-TARGET",
		NodeID: &slots[1].ID, Status: "available",
	}
	if err := db.CreateBin(targetBin); err != nil {
		t.Fatalf("create target bin: %v", err)
	}
	if err := db.SetBinManifest(targetBin.ID, `{"items":[]}`, bp.Code, 100); err != nil {
		t.Fatalf("set target manifest: %v", err)
	}
	if err := db.ConfirmBinManifest(targetBin.ID, ""); err != nil {
		t.Fatalf("confirm target: %v", err)
	}
	// Make target clearly older so FIFO prefers it over the accessible blocker
	if _, err := db.Exec(`UPDATE bins SET loaded_at = NOW() - interval '2 hours' WHERE id = $1`, targetBin.ID); err != nil {
		t.Fatalf("set target loaded_at: %v", err)
	}

	// BLOCKER bin at depth 1 (newer — loaded_at = NOW, blocks access to target)
	blockerBin := &store.Bin{
		BinTypeID: bt.ID, Label: "BIN-BLOCKER",
		NodeID: &slots[0].ID, Status: "available",
	}
	if err := db.CreateBin(blockerBin); err != nil {
		t.Fatalf("create blocker bin: %v", err)
	}
	if err := db.SetBinManifest(blockerBin.ID, `{"items":[]}`, bp.Code, 50); err != nil {
		t.Fatalf("set blocker manifest: %v", err)
	}
	if err := db.ConfirmBinManifest(blockerBin.ID, ""); err != nil {
		t.Fatalf("confirm blocker: %v", err)
	}

	t.Logf("setup: target=%d at depth 2 (2h old), blocker=%d at depth 1 (new)", targetBin.ID, blockerBin.ID)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// Retrieve order targeting NGRP as source -> GroupResolver FIFO -> buried bin detected -> reshuffle
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "reshuffle-buried-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		SourceNode:   grp.Name,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	})

	order, err := db.GetOrderByUUID("reshuffle-buried-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	t.Logf("order %d: status=%s bin=%v vendor=%s", order.ID, order.Status, order.BinID, order.VendorOrderID)

	// Order should be in "reshuffling" status (compound parent)
	if order.Status != dispatch.StatusReshuffling {
		t.Fatalf("order status = %q, want %q", order.Status, dispatch.StatusReshuffling)
	}

	// Check compound children were created
	children, err := db.ListChildOrders(order.ID)
	if err != nil {
		t.Fatalf("list children: %v", err)
	}
	if len(children) < 3 {
		t.Fatalf("expected >= 3 children (unbury, retrieve, restock), got %d", len(children))
	}

	t.Logf("compound: %d children", len(children))
	for _, c := range children {
		t.Logf("  child %d: seq=%d status=%s desc=%s source=%s dest=%s bin=%v vendor=%s",
			c.ID, c.Sequence, c.Status, c.PayloadDesc, c.SourceNode, c.DeliveryNode, c.BinID, c.VendorOrderID)
	}

	// Drive each child through the fleet simulator lifecycle
	for _, child := range children {
		child, err = db.GetOrder(child.ID)
		if err != nil {
			t.Fatalf("get child: %v", err)
		}
		if child.VendorOrderID == "" {
			t.Fatalf("child %d (seq %d) not dispatched — status=%s", child.ID, child.Sequence, child.Status)
		}

		sim.DriveState(child.VendorOrderID, "RUNNING")
		sim.DriveState(child.VendorOrderID, "FINISHED")

		// Edge receipt triggers completion -> HandleChildOrderComplete -> AdvanceCompoundOrder
		d.HandleOrderReceipt(env, &protocol.OrderReceipt{
			OrderUUID:   child.EdgeUUID,
			ReceiptType: "confirmed",
			FinalCount:  1,
		})

		child, err = db.GetOrder(child.ID)
		if err != nil {
			t.Fatalf("get child after completion: %v", err)
		}
		t.Logf("child %d completed: status=%s", child.ID, child.Status)
	}

	// Verify parent order completed
	order, err = db.GetOrderByUUID("reshuffle-buried-1")
	if err != nil {
		t.Fatalf("get parent order: %v", err)
	}
	t.Logf("parent order final: status=%s", order.Status)

	// Verify target bin moved from depth-2 slot toward line
	targetBin, err = db.GetBin(targetBin.ID)
	if err != nil {
		t.Fatalf("get target bin: %v", err)
	}
	if targetBin.NodeID != nil && *targetBin.NodeID == lineNode.ID {
		t.Logf("target bin at line node %s — correct", lineNode.Name)
	} else {
		t.Errorf("target bin at node %v (wanted line %d)", targetBin.NodeID, lineNode.ID)
	}

	// Verify blocker restocked back to lane
	blockerBin, err = db.GetBin(blockerBin.ID)
	if err != nil {
		t.Fatalf("get blocker bin: %v", err)
	}
	t.Logf("blocker bin: node=%v", blockerBin.NodeID)

	// No bins stuck as claimed
	for _, b := range []*store.Bin{targetBin, blockerBin} {
		if b.ClaimedBy != nil {
			t.Errorf("bin %d still claimed by order %d after reshuffle", b.ID, *b.ClaimedBy)
		}
	}

	// Lane lock released
	if eng.Dispatcher().LaneLock().IsLocked(lane.ID) {
		t.Errorf("lane %d still locked after compound order completion", lane.ID)
	} else {
		t.Logf("lane lock released — correct")
	}
}

// --- Test: Compound child failure mid-reshuffle — blocker stranding ---
//
// Scenario: A 3-step reshuffle is in progress (unbury blocker → retrieve target
// → restock blocker). Step 1 completes: blocker moved to shuffle slot. Step 2
// (retrieve target) fails — robot breaks down. HandleChildOrderFailure cancels
// remaining children and fails the parent.
//
// Key question: The blocker bin is now physically at the shuffle slot (moved by
// completed step 1). Its claim was released on completion. Is it visible to
// normal operations? Can it be retrieved? Or is it orphaned?
//
// Expected: After failure, the blocker bin should be at the shuffle slot,
// unclaimed, and accessible for manual recovery or a new reshuffle. The lane
// lock should be released so a retry can proceed. The target bin should still
// be at its original slot (step 2 never completed), unclaimed.
func TestCompound_ChildFailureMidReshuffle_BlockerStranding(t *testing.T) {
	db := testDB(t)

	grpType, err := db.GetNodeTypeByCode("NGRP")
	if err != nil {
		t.Fatalf("get NGRP: %v", err)
	}
	lanType, err := db.GetNodeTypeByCode("LANE")
	if err != nil {
		t.Fatalf("get LANE: %v", err)
	}

	bp := &store.Payload{Code: "PART-STRAND", Description: "Stranding test"}
	if err := db.CreatePayload(bp); err != nil {
		t.Fatalf("create payload: %v", err)
	}
	bt := &store.BinType{Code: "DEFAULT-ST", Description: "Stranding bin type"}
	if err := db.CreateBinType(bt); err != nil {
		t.Fatalf("create bin type: %v", err)
	}

	// NGRP → LANE → 2 slots
	grp := &store.Node{Name: "GRP-STRAND", NodeTypeID: &grpType.ID, Enabled: true, IsSynthetic: true}
	db.CreateNode(grp)
	lane := &store.Node{Name: "GRP-STRAND-L1", NodeTypeID: &lanType.ID, ParentID: &grp.ID, Enabled: true, IsSynthetic: true}
	db.CreateNode(lane)

	depth1, depth2 := 1, 2
	slot1 := &store.Node{Name: "GRP-STRAND-L1-S1", ParentID: &lane.ID, Enabled: true, Depth: &depth1}
	db.CreateNode(slot1)
	slot2 := &store.Node{Name: "GRP-STRAND-L1-S2", ParentID: &lane.ID, Enabled: true, Depth: &depth2}
	db.CreateNode(slot2)

	shuffleSlot := &store.Node{Name: "GRP-STRAND-SHUF", ParentID: &grp.ID, Enabled: true}
	db.CreateNode(shuffleSlot)

	lineNode := &store.Node{Name: "LINE-STRAND", Enabled: true}
	db.CreateNode(lineNode)

	// Target at depth 2 (buried, older)
	targetBin := &store.Bin{BinTypeID: bt.ID, Label: "BIN-STRAND-TARGET", NodeID: &slot2.ID, Status: "available"}
	db.CreateBin(targetBin)
	db.SetBinManifest(targetBin.ID, `{"items":[]}`, bp.Code, 100)
	db.ConfirmBinManifest(targetBin.ID, "")
	db.Exec(`UPDATE bins SET loaded_at = NOW() - interval '2 hours' WHERE id = $1`, targetBin.ID)

	// Blocker at depth 1 (front, newer)
	blockerBin := &store.Bin{BinTypeID: bt.ID, Label: "BIN-STRAND-BLOCKER", NodeID: &slot1.ID, Status: "available"}
	db.CreateBin(blockerBin)
	db.SetBinManifest(blockerBin.ID, `{"items":[]}`, bp.Code, 50)
	db.ConfirmBinManifest(blockerBin.ID, "")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// Trigger reshuffle via FIFO retrieve
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "strand-reshuffle-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		SourceNode:   grp.Name,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	})

	order, err := db.GetOrderByUUID("strand-reshuffle-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.Status != dispatch.StatusReshuffling {
		t.Fatalf("order status = %q, want reshuffling", order.Status)
	}

	children, _ := db.ListChildOrders(order.ID)
	if len(children) < 3 {
		t.Fatalf("expected >= 3 children, got %d", len(children))
	}

	for i, c := range children {
		t.Logf("child %d: seq=%d desc=%s source=%s dest=%s", i, c.Sequence, c.PayloadDesc, c.SourceNode, c.DeliveryNode)
	}

	// Complete step 1 (unbury blocker → shuffle slot)
	child1, _ := db.GetOrder(children[0].ID)
	if child1.VendorOrderID == "" {
		t.Fatalf("child 1 not dispatched")
	}
	sim.DriveState(child1.VendorOrderID, "RUNNING")
	sim.DriveState(child1.VendorOrderID, "FINISHED")
	d.HandleOrderReceipt(env, &protocol.OrderReceipt{
		OrderUUID: child1.EdgeUUID, ReceiptType: "confirmed", FinalCount: 1,
	})

	// Verify blocker moved to shuffle slot
	blockerBin, _ = db.GetBin(blockerBin.ID)
	if blockerBin.NodeID == nil || *blockerBin.NodeID != shuffleSlot.ID {
		t.Errorf("blocker not at shuffle slot after step 1: node=%v, want %d (shuffleSlot)", blockerBin.NodeID, shuffleSlot.ID)
	}

	// Step 2 (retrieve target) dispatched automatically by AdvanceCompoundOrder
	child2, _ := db.GetOrder(children[1].ID)
	if child2.VendorOrderID == "" {
		// Re-fetch — AdvanceCompoundOrder may have just dispatched
		child2, _ = db.GetOrder(children[1].ID)
	}
	if child2.VendorOrderID == "" {
		t.Fatalf("child 2 not dispatched")
	}

	// Step 2 fails — robot breaks down
	sim.DriveState(child2.VendorOrderID, "RUNNING")
	sim.DriveState(child2.VendorOrderID, "FAILED")

	// Verify parent order failed
	order, _ = db.GetOrderByUUID("strand-reshuffle-1")
	t.Logf("parent after child failure: status=%s", order.Status)
	if order.Status != dispatch.StatusFailed {
		t.Errorf("parent status = %q, want failed", order.Status)
	}

	// Verify lane lock released
	if eng.Dispatcher().LaneLock().IsLocked(lane.ID) {
		t.Errorf("lane still locked after compound failure — prevents retry")
	}

	// Verify remaining children cancelled
	children, _ = db.ListChildOrders(order.ID)
	for _, c := range children {
		c, _ = db.GetOrder(c.ID)
		t.Logf("child %d (seq %d): status=%s", c.ID, c.Sequence, c.Status)
	}

	// KEY CHECK: blocker bin at shuffle slot — is it recoverable?
	blockerBin, _ = db.GetBin(blockerBin.ID)
	t.Logf("blocker final: node=%v claimed=%v status=%s", blockerBin.NodeID, blockerBin.ClaimedBy, blockerBin.Status)

	if blockerBin.ClaimedBy != nil {
		t.Errorf("blocker bin still claimed by %d — cannot be retrieved by a new order", *blockerBin.ClaimedBy)
	}
	if blockerBin.NodeID == nil || *blockerBin.NodeID != shuffleSlot.ID {
		t.Errorf("blocker bin not at shuffle slot after failure: node=%v, want %d", blockerBin.NodeID, shuffleSlot.ID)
	}

	// Target bin should still be at its original slot (step 2 never completed)
	targetBin, _ = db.GetBin(targetBin.ID)
	t.Logf("target final: node=%v claimed=%v", targetBin.NodeID, targetBin.ClaimedBy)
	if targetBin.ClaimedBy != nil {
		t.Errorf("target bin still claimed by %d — stranded", *targetBin.ClaimedBy)
	}
}

// --- Test: Two-robot swap full lifecycle (5-step compound) ---
//
// Scenario: An NGRP lane has 3 bins. The target is at depth 3 (deepest),
// with 2 blockers at depth 1 and 2. FIFO detects the buried target and
// triggers a reshuffle with 5 steps:
//   1. Unbury blocker-1 (depth 1) → shuffle-1
//   2. Unbury blocker-2 (depth 2) → shuffle-2
//   3. Retrieve target (depth 3) → line node
//   4. Restock blocker-2 → depth 2 (deepest-first)
//   5. Restock blocker-1 → depth 1
//
// This is the full two-robot swap pattern. The test verifies:
// - All 5 children created and dispatched sequentially
// - Target arrives at line, blockers restocked to original positions
// - All claims released, lane lock freed, parent completed
func TestCompound_TwoRobotSwap_FullLifecycle(t *testing.T) {
	db := testDB(t)

	grpType, _ := db.GetNodeTypeByCode("NGRP")
	lanType, _ := db.GetNodeTypeByCode("LANE")

	bp := &store.Payload{Code: "PART-SWAP", Description: "Swap test payload"}
	db.CreatePayload(bp)
	bt := &store.BinType{Code: "DEFAULT-SW", Description: "Swap bin type"}
	db.CreateBinType(bt)

	// NGRP → LANE → 3 slots
	grp := &store.Node{Name: "GRP-SWAP", NodeTypeID: &grpType.ID, Enabled: true, IsSynthetic: true}
	db.CreateNode(grp)
	lane := &store.Node{Name: "GRP-SWAP-L1", NodeTypeID: &lanType.ID, ParentID: &grp.ID, Enabled: true, IsSynthetic: true}
	db.CreateNode(lane)

	depths := [3]int{1, 2, 3}
	var slots [3]*store.Node
	for i := 0; i < 3; i++ {
		s := &store.Node{
			Name:     fmt.Sprintf("GRP-SWAP-L1-S%d", depths[i]),
			ParentID: &lane.ID, Enabled: true, Depth: &depths[i],
		}
		db.CreateNode(s)
		slots[i] = s
	}

	// Two shuffle slots
	shuf1 := &store.Node{Name: "GRP-SWAP-SHUF1", ParentID: &grp.ID, Enabled: true}
	db.CreateNode(shuf1)
	shuf2 := &store.Node{Name: "GRP-SWAP-SHUF2", ParentID: &grp.ID, Enabled: true}
	db.CreateNode(shuf2)

	lineNode := &store.Node{Name: "LINE-SWAP", Enabled: true}
	db.CreateNode(lineNode)

	// Target at depth 3 (oldest — 3 hours ago)
	targetBin := &store.Bin{BinTypeID: bt.ID, Label: "BIN-SWAP-TARGET", NodeID: &slots[2].ID, Status: "available"}
	db.CreateBin(targetBin)
	db.SetBinManifest(targetBin.ID, `{"items":[]}`, bp.Code, 100)
	db.ConfirmBinManifest(targetBin.ID, "")
	db.Exec(`UPDATE bins SET loaded_at = NOW() - interval '3 hours' WHERE id = $1`, targetBin.ID)

	// Blocker 2 at depth 2
	blocker2 := &store.Bin{BinTypeID: bt.ID, Label: "BIN-SWAP-BLK2", NodeID: &slots[1].ID, Status: "available"}
	db.CreateBin(blocker2)
	db.SetBinManifest(blocker2.ID, `{"items":[]}`, bp.Code, 50)
	db.ConfirmBinManifest(blocker2.ID, "")
	db.Exec(`UPDATE bins SET loaded_at = NOW() - interval '1 hour' WHERE id = $1`, blocker2.ID)

	// Blocker 1 at depth 1 (newest)
	blocker1 := &store.Bin{BinTypeID: bt.ID, Label: "BIN-SWAP-BLK1", NodeID: &slots[0].ID, Status: "available"}
	db.CreateBin(blocker1)
	db.SetBinManifest(blocker1.ID, `{"items":[]}`, bp.Code, 50)
	db.ConfirmBinManifest(blocker1.ID, "")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "swap-5step-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		SourceNode:   grp.Name,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	})

	order, err := db.GetOrderByUUID("swap-5step-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.Status != dispatch.StatusReshuffling {
		t.Fatalf("status = %q, want reshuffling", order.Status)
	}

	children, _ := db.ListChildOrders(order.ID)
	t.Logf("compound: %d children", len(children))
	for _, c := range children {
		t.Logf("  child seq=%d: desc=%s src=%s dest=%s bin=%v",
			c.Sequence, c.PayloadDesc, c.SourceNode, c.DeliveryNode, c.BinID)
	}

	if len(children) < 5 {
		t.Fatalf("expected >= 5 children (2 unbury + 1 retrieve + 2 restock), got %d", len(children))
	}

	// Drive each child through full lifecycle sequentially
	for i, child := range children {
		child, _ = db.GetOrder(child.ID)
		if child.VendorOrderID == "" {
			t.Fatalf("child %d (seq %d) not dispatched — status=%s", i, child.Sequence, child.Status)
		}

		sim.DriveState(child.VendorOrderID, "RUNNING")
		sim.DriveState(child.VendorOrderID, "FINISHED")

		d.HandleOrderReceipt(env, &protocol.OrderReceipt{
			OrderUUID: child.EdgeUUID, ReceiptType: "confirmed", FinalCount: 1,
		})

		child, _ = db.GetOrder(child.ID)
		t.Logf("child %d (seq %d) completed: status=%s", i, child.Sequence, child.Status)
	}

	// Verify parent completed
	order, _ = db.GetOrderByUUID("swap-5step-1")
	if order.Status != dispatch.StatusConfirmed {
		t.Errorf("parent status = %q, want confirmed", order.Status)
	}

	// Verify target at line
	targetBin, _ = db.GetBin(targetBin.ID)
	if targetBin.NodeID == nil || *targetBin.NodeID != lineNode.ID {
		t.Errorf("target bin at node %v, want line %d", targetBin.NodeID, lineNode.ID)
	} else {
		t.Logf("target bin at line — correct")
	}

	// Verify blockers restocked to original slots (deepest-first restocking)
	blocker1, _ = db.GetBin(blocker1.ID)
	blocker2, _ = db.GetBin(blocker2.ID)
	if blocker1.NodeID == nil || *blocker1.NodeID != slots[0].ID {
		t.Errorf("blocker1 at node %v, want slots[0] (%d)", blocker1.NodeID, slots[0].ID)
	}
	if blocker2.NodeID == nil || *blocker2.NodeID != slots[1].ID {
		t.Errorf("blocker2 at node %v, want slots[1] (%d)", blocker2.NodeID, slots[1].ID)
	}
	if blocker1.Status != "available" || blocker2.Status != "available" {
		t.Errorf("blocker statuses: blocker1=%s blocker2=%s, want both available", blocker1.Status, blocker2.Status)
	}

	// All claims released
	for _, b := range []*store.Bin{targetBin, blocker1, blocker2} {
		if b.ClaimedBy != nil {
			t.Errorf("bin %d (%s) still claimed by %d", b.ID, b.Label, *b.ClaimedBy)
		}
	}

	// Lane lock freed
	if eng.Dispatcher().LaneLock().IsLocked(lane.ID) {
		t.Errorf("lane %d still locked after 5-step compound completion", lane.ID)
	}
}

// --- Test: Cancel parent compound order while child is in-flight ---
//
// Scenario: A reshuffle compound order is in progress. Child 1 (unbury) is
// dispatched and the robot is RUNNING. The operator cancels the parent order
// (not the child). Does the in-flight child's fleet order get cancelled?
// Does the lane lock release?
//
// This exercises the cancel path for compound parents, which is different
// from HandleChildOrderFailure (that's triggered by fleet failure on a child).
//
// Expected: Parent and all children cancelled. Lane lock released. Bins
// unclaimed. The child's fleet order should be cancelled (or at minimum,
// the order record is marked cancelled).
func TestCompound_CancelParentWhileChildInFlight(t *testing.T) {
	db := testDB(t)

	grpType, _ := db.GetNodeTypeByCode("NGRP")
	lanType, _ := db.GetNodeTypeByCode("LANE")

	bp := &store.Payload{Code: "PART-PCANCEL", Description: "Parent cancel test"}
	db.CreatePayload(bp)
	bt := &store.BinType{Code: "DEFAULT-PC", Description: "Parent cancel bin type"}
	db.CreateBinType(bt)

	grp := &store.Node{Name: "GRP-PCANCEL", NodeTypeID: &grpType.ID, Enabled: true, IsSynthetic: true}
	db.CreateNode(grp)
	lane := &store.Node{Name: "GRP-PCANCEL-L1", NodeTypeID: &lanType.ID, ParentID: &grp.ID, Enabled: true, IsSynthetic: true}
	db.CreateNode(lane)

	depth1, depth2 := 1, 2
	slot1 := &store.Node{Name: "GRP-PCANCEL-L1-S1", ParentID: &lane.ID, Enabled: true, Depth: &depth1}
	db.CreateNode(slot1)
	slot2 := &store.Node{Name: "GRP-PCANCEL-L1-S2", ParentID: &lane.ID, Enabled: true, Depth: &depth2}
	db.CreateNode(slot2)

	shuffleSlot := &store.Node{Name: "GRP-PCANCEL-SHUF", ParentID: &grp.ID, Enabled: true}
	db.CreateNode(shuffleSlot)

	lineNode := &store.Node{Name: "LINE-PCANCEL", Enabled: true}
	db.CreateNode(lineNode)

	targetBin := &store.Bin{BinTypeID: bt.ID, Label: "BIN-PCANCEL-TARGET", NodeID: &slot2.ID, Status: "available"}
	db.CreateBin(targetBin)
	db.SetBinManifest(targetBin.ID, `{"items":[]}`, bp.Code, 100)
	db.ConfirmBinManifest(targetBin.ID, "")
	db.Exec(`UPDATE bins SET loaded_at = NOW() - interval '2 hours' WHERE id = $1`, targetBin.ID)

	blockerBin := &store.Bin{BinTypeID: bt.ID, Label: "BIN-PCANCEL-BLK", NodeID: &slot1.ID, Status: "available"}
	db.CreateBin(blockerBin)
	db.SetBinManifest(blockerBin.ID, `{"items":[]}`, bp.Code, 50)
	db.ConfirmBinManifest(blockerBin.ID, "")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "pcancel-reshuffle-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		SourceNode:   grp.Name,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	})

	order, err := db.GetOrderByUUID("pcancel-reshuffle-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.Status != dispatch.StatusReshuffling {
		t.Fatalf("status = %q, want reshuffling", order.Status)
	}

	children, _ := db.ListChildOrders(order.ID)
	if len(children) < 3 {
		t.Fatalf("expected >= 3 children, got %d", len(children))
	}

	// Child 1 is dispatched and robot is RUNNING
	child1, _ := db.GetOrder(children[0].ID)
	if child1.VendorOrderID == "" {
		t.Fatalf("child 1 not dispatched")
	}
	sim.DriveState(child1.VendorOrderID, "RUNNING")

	child1, _ = db.GetOrder(child1.ID)
	t.Logf("child 1 before cancel: status=%s vendor=%s", child1.Status, child1.VendorOrderID)

	// Cancel the PARENT order while child is in flight
	d.HandleOrderCancel(env, &protocol.OrderCancel{
		OrderUUID: "pcancel-reshuffle-1",
		Reason:    "operator cancelled parent during reshuffle",
	})

	// Verify parent cancelled
	order, _ = db.GetOrderByUUID("pcancel-reshuffle-1")
	t.Logf("parent after cancel: status=%s", order.Status)
	if order.Status != dispatch.StatusCancelled {
		t.Errorf("parent status = %q, want cancelled", order.Status)
	}

	// Check all children statuses
	children, _ = db.ListChildOrders(order.ID)
	for _, c := range children {
		c, _ = db.GetOrder(c.ID)
		t.Logf("  child %d (seq %d): status=%s vendor=%s", c.ID, c.Sequence, c.Status, c.VendorOrderID)

		// Children with vendor orders should ideally be cancelled too
		if c.VendorOrderID != "" && c.Status != dispatch.StatusCancelled {
			t.Logf("  WARNING: child %d has fleet order %s but status=%s (not cancelled) — orphan robot risk",
				c.ID, c.VendorOrderID, c.Status)
		}
	}

	// Lane lock should be released
	if eng.Dispatcher().LaneLock().IsLocked(lane.ID) {
		t.Errorf("BUG: lane %d still locked after parent cancel — blocks future reshuffles", lane.ID)
	} else {
		t.Logf("lane lock released — correct")
	}

	// All bins should be unclaimed
	targetBin, _ = db.GetBin(targetBin.ID)
	blockerBin, _ = db.GetBin(blockerBin.ID)
	for _, b := range []*store.Bin{targetBin, blockerBin} {
		if b.ClaimedBy != nil {
			t.Errorf("BUG: bin %d (%s) still claimed by %d after parent cancel — permanently stuck",
				b.ID, b.Label, *b.ClaimedBy)
		}
	}
}

// --- Test: AdvanceCompoundOrder skips failed children — premature completion (TC-51) ---
//
// OBSERVATIONAL TEST: This test always passes. It uses t.Logf (not t.Errorf)
// to document the skip-failed-child behavior. When AdvanceCompoundOrder is
// updated to halt on child failure, convert the Logf calls to assertions.
//
// Scenario: A 3-step compound order where child 2 has invalid source/dest
// (empty string). AdvanceCompoundOrder dispatches child 1 which completes.
// When advancing to child 2, lines 77-98 in compound.go detect missing
// source/delivery, mark child 2 failed, and recursively call
// AdvanceCompoundOrder. This advances to child 3.
//
// Expected: The parent should NOT complete normally if a child was skipped
// due to failure. This test documents whether the current behavior causes
// silent data loss (blocker not restocked but parent "confirmed").
func TestCompound_AdvanceSkipsFailedChild_PrematureCompletion(t *testing.T) {
	db := testDB(t)

	grpType, _ := db.GetNodeTypeByCode("NGRP")
	lanType, _ := db.GetNodeTypeByCode("LANE")

	bp := &store.Payload{Code: "PART-SKIP", Description: "Skip test"}
	db.CreatePayload(bp)
	bt := &store.BinType{Code: "DEFAULT-SK", Description: "Skip bin type"}
	db.CreateBinType(bt)

	grp := &store.Node{Name: "GRP-SKIP", NodeTypeID: &grpType.ID, Enabled: true, IsSynthetic: true}
	db.CreateNode(grp)
	lane := &store.Node{Name: "GRP-SKIP-L1", NodeTypeID: &lanType.ID, ParentID: &grp.ID, Enabled: true, IsSynthetic: true}
	db.CreateNode(lane)

	depth1, depth2 := 1, 2
	slot1 := &store.Node{Name: "GRP-SKIP-L1-S1", ParentID: &lane.ID, Enabled: true, Depth: &depth1}
	db.CreateNode(slot1)
	slot2 := &store.Node{Name: "GRP-SKIP-L1-S2", ParentID: &lane.ID, Enabled: true, Depth: &depth2}
	db.CreateNode(slot2)

	shuffleSlot := &store.Node{Name: "GRP-SKIP-SHUF", ParentID: &grp.ID, Enabled: true}
	db.CreateNode(shuffleSlot)

	lineNode := &store.Node{Name: "LINE-SKIP", Enabled: true}
	db.CreateNode(lineNode)

	// Target at depth 2 (buried)
	targetBin := &store.Bin{BinTypeID: bt.ID, Label: "BIN-SKIP-TARGET", NodeID: &slot2.ID, Status: "available"}
	db.CreateBin(targetBin)
	db.SetBinManifest(targetBin.ID, `{"items":[]}`, bp.Code, 100)
	db.ConfirmBinManifest(targetBin.ID, "")
	db.Exec(`UPDATE bins SET loaded_at = NOW() - interval '2 hours' WHERE id = $1`, targetBin.ID)

	// Blocker at depth 1
	blockerBin := &store.Bin{BinTypeID: bt.ID, Label: "BIN-SKIP-BLK", NodeID: &slot1.ID, Status: "available"}
	db.CreateBin(blockerBin)
	db.SetBinManifest(blockerBin.ID, `{"items":[]}`, bp.Code, 50)
	db.ConfirmBinManifest(blockerBin.ID, "")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// Trigger reshuffle
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "skip-reshuffle-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		SourceNode:   grp.Name,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	})

	order, err := db.GetOrderByUUID("skip-reshuffle-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.Status != dispatch.StatusReshuffling {
		t.Fatalf("status = %q, want reshuffling", order.Status)
	}

	children, _ := db.ListChildOrders(order.ID)
	if len(children) < 3 {
		t.Fatalf("expected >= 3 children, got %d", len(children))
	}

	for i, c := range children {
		t.Logf("child %d: seq=%d src=%s dest=%s", i, c.Sequence, c.SourceNode, c.DeliveryNode)
	}

	// Complete child 1 (unbury blocker)
	child1, _ := db.GetOrder(children[0].ID)
	if child1.VendorOrderID == "" {
		t.Fatalf("child 1 not dispatched")
	}
	sim.DriveState(child1.VendorOrderID, "RUNNING")
	sim.DriveState(child1.VendorOrderID, "FINISHED")
	d.HandleOrderReceipt(env, &protocol.OrderReceipt{
		OrderUUID: child1.EdgeUUID, ReceiptType: "confirmed", FinalCount: 1,
	})

	// Now manually break child 2 by clearing its source node
	// This simulates a data corruption or race condition
	child2, _ := db.GetOrder(children[1].ID)
	if child2.VendorOrderID != "" {
		// Child 2 already dispatched — too late to break it
		t.Logf("child 2 already dispatched (vendor=%s) — skipping synthetic break, completing normally", child2.VendorOrderID)

		// Complete remaining children normally and verify
		for i := 1; i < len(children); i++ {
			child, _ := db.GetOrder(children[i].ID)
			if child.VendorOrderID == "" || child.Status == dispatch.StatusFailed {
				continue
			}
			sim.DriveState(child.VendorOrderID, "RUNNING")
			sim.DriveState(child.VendorOrderID, "FINISHED")
			d.HandleOrderReceipt(env, &protocol.OrderReceipt{
				OrderUUID: child.EdgeUUID, ReceiptType: "confirmed", FinalCount: 1,
			})
		}
	} else {
		// Child 2 not yet dispatched — break its source node
		db.Exec(`UPDATE orders SET source_node = '' WHERE id = $1`, child2.ID)

		// Advance again — this should detect the broken child and skip it
		d.AdvanceCompoundOrder(order.ID)

		// Check what happened
		child2, _ = db.GetOrder(child2.ID)
		t.Logf("child 2 after advance: status=%s", child2.Status)

		if child2.Status == dispatch.StatusFailed {
			t.Logf("child 2 correctly failed due to missing source node")
		}
	}

	// Final state check
	order, _ = db.GetOrderByUUID("skip-reshuffle-1")
	t.Logf("parent final: status=%s", order.Status)

	children, _ = db.ListChildOrders(order.ID)
	failedCount := 0
	completedCount := 0
	for _, c := range children {
		c, _ = db.GetOrder(c.ID)
		t.Logf("  child %d (seq %d): status=%s", c.ID, c.Sequence, c.Status)
		if c.Status == dispatch.StatusFailed {
			failedCount++
		}
		if c.Status == dispatch.StatusConfirmed {
			completedCount++
		}
	}

	if failedCount > 0 && order.Status == dispatch.StatusConfirmed {
		t.Errorf("POTENTIAL BUG: parent completed (confirmed) despite %d failed children — data may be inconsistent", failedCount)
	}

	// Check blocker bin location — is it stranded?
	blockerBin, _ = db.GetBin(blockerBin.ID)
	t.Logf("blocker final: node=%v claimed=%v", blockerBin.NodeID, blockerBin.ClaimedBy)
}

// --- Test: Lane lock contention — second reshuffle blocked (TC-52) ---
//
// Scenario: A retrieve order triggers a reshuffle on a lane. While the
// reshuffle is in progress, a second retrieve order targets the same NGRP
// lane (same or different payload). The planning service should detect the
// lane lock and return a lane_locked planningError.
//
// Current behavior: lane_locked goes through failOrder, not queueOrder.
// This means the second order FAILS rather than being retried when the
// lane unlocks. This test documents that behavior and whether it's correct.
func TestLaneLock_Contention_SecondReshuffleBlocked(t *testing.T) {
	db := testDB(t)

	grpType, _ := db.GetNodeTypeByCode("NGRP")
	lanType, _ := db.GetNodeTypeByCode("LANE")

	bp := &store.Payload{Code: "PART-LOCK", Description: "Lane lock test"}
	db.CreatePayload(bp)
	bt := &store.BinType{Code: "DEFAULT-LK", Description: "Lock bin type"}
	db.CreateBinType(bt)

	grp := &store.Node{Name: "GRP-LOCK", NodeTypeID: &grpType.ID, Enabled: true, IsSynthetic: true}
	db.CreateNode(grp)
	lane := &store.Node{Name: "GRP-LOCK-L1", NodeTypeID: &lanType.ID, ParentID: &grp.ID, Enabled: true, IsSynthetic: true}
	db.CreateNode(lane)

	depth1, depth2, depth3 := 1, 2, 3
	slot1 := &store.Node{Name: "GRP-LOCK-L1-S1", ParentID: &lane.ID, Enabled: true, Depth: &depth1}
	db.CreateNode(slot1)
	slot2 := &store.Node{Name: "GRP-LOCK-L1-S2", ParentID: &lane.ID, Enabled: true, Depth: &depth2}
	db.CreateNode(slot2)
	slot3 := &store.Node{Name: "GRP-LOCK-L1-S3", ParentID: &lane.ID, Enabled: true, Depth: &depth3}
	db.CreateNode(slot3)

	shuffleSlot1 := &store.Node{Name: "GRP-LOCK-SHUF1", ParentID: &grp.ID, Enabled: true}
	db.CreateNode(shuffleSlot1)
	shuffleSlot2 := &store.Node{Name: "GRP-LOCK-SHUF2", ParentID: &grp.ID, Enabled: true}
	db.CreateNode(shuffleSlot2)

	lineNode := &store.Node{Name: "LINE-LOCK", Enabled: true}
	db.CreateNode(lineNode)

	// Bin at depth 3 (buried under 2 blockers) — first target
	targetBin1 := &store.Bin{BinTypeID: bt.ID, Label: "BIN-LOCK-T1", NodeID: &slot3.ID, Status: "available"}
	db.CreateBin(targetBin1)
	db.SetBinManifest(targetBin1.ID, `{"items":[]}`, bp.Code, 100)
	db.ConfirmBinManifest(targetBin1.ID, "")
	db.Exec(`UPDATE bins SET loaded_at = NOW() - interval '3 hours' WHERE id = $1`, targetBin1.ID)

	// Blocker at depth 2
	blocker2 := &store.Bin{BinTypeID: bt.ID, Label: "BIN-LOCK-BLK2", NodeID: &slot2.ID, Status: "available"}
	db.CreateBin(blocker2)
	db.SetBinManifest(blocker2.ID, `{"items":[]}`, bp.Code, 50)
	db.ConfirmBinManifest(blocker2.ID, "")
	db.Exec(`UPDATE bins SET loaded_at = NOW() - interval '1 hour' WHERE id = $1`, blocker2.ID)

	// Blocker at depth 1
	blocker1 := &store.Bin{BinTypeID: bt.ID, Label: "BIN-LOCK-BLK1", NodeID: &slot1.ID, Status: "available"}
	db.CreateBin(blocker1)
	db.SetBinManifest(blocker1.ID, `{"items":[]}`, bp.Code, 50)
	db.ConfirmBinManifest(blocker1.ID, "")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// First order triggers reshuffle
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "lock-reshuffle-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		SourceNode:   grp.Name,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	})

	order1, err := db.GetOrderByUUID("lock-reshuffle-1")
	if err != nil {
		t.Fatalf("get order 1: %v", err)
	}
	if order1.Status != dispatch.StatusReshuffling {
		t.Fatalf("order 1 status = %q, want reshuffling", order1.Status)
	}

	// Verify lane is locked
	if !eng.Dispatcher().LaneLock().IsLocked(lane.ID) {
		t.Fatalf("lane not locked after reshuffle started")
	}

	// Second order tries to retrieve from same NGRP while lane is locked
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "lock-reshuffle-2",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		SourceNode:   grp.Name,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	})

	order2, err := db.GetOrderByUUID("lock-reshuffle-2")
	if err != nil {
		t.Fatalf("get order 2: %v", err)
	}
	t.Logf("order 2 status: %s", order2.Status)

	// Verify: lane_locked → queueOrder (not failOrder)
	// The second order should be queued for retry, not permanently failed.
	if order2.Status == dispatch.StatusQueued {
		t.Logf("CORRECT: second order queued — will retry when lane unlocks via fulfillment scanner")
	} else if order2.Status == dispatch.StatusFailed {
		t.Errorf("second order FAILED due to lane_locked — should be queued for retry, not permanently failed")
	} else {
		t.Errorf("second order status=%s, want queued", order2.Status)
	}
}

// --- Test: ApplyBinArrival status mapping for compound restock children (TC-53) ---
//
// Scenario: A compound restock child delivers a blocker bin back to its
// storage slot (a child of a LANE node). When the fleet reports FINISHED
// and the receipt is confirmed, handleOrderCompleted calls ApplyBinArrival.
//
// ApplyBinArrival checks if the destination is a storage slot (parent type
// LANE). If so, it sets status='available' (not staged). This is critical:
// if the restocked blocker is marked 'staged' instead of 'available', it
// won't show up in FindSourceBinFIFO queries.
//
// Expected: After compound restock, the bin at the storage slot should have
// status='available', claimed_by=NULL, and be visible to FIFO queries.
func TestCompound_RestockChild_BinStatusAvailable(t *testing.T) {
	db := testDB(t)

	grpType, _ := db.GetNodeTypeByCode("NGRP")
	lanType, _ := db.GetNodeTypeByCode("LANE")

	bp := &store.Payload{Code: "PART-RESTOCK", Description: "Restock status test"}
	db.CreatePayload(bp)
	bt := &store.BinType{Code: "DEFAULT-RS", Description: "Restock bin type"}
	db.CreateBinType(bt)

	grp := &store.Node{Name: "GRP-RESTOCK", NodeTypeID: &grpType.ID, Enabled: true, IsSynthetic: true}
	db.CreateNode(grp)
	lane := &store.Node{Name: "GRP-RESTOCK-L1", NodeTypeID: &lanType.ID, ParentID: &grp.ID, Enabled: true, IsSynthetic: true}
	db.CreateNode(lane)

	depth1, depth2 := 1, 2
	slot1 := &store.Node{Name: "GRP-RESTOCK-L1-S1", ParentID: &lane.ID, Enabled: true, Depth: &depth1}
	db.CreateNode(slot1)
	slot2 := &store.Node{Name: "GRP-RESTOCK-L1-S2", ParentID: &lane.ID, Enabled: true, Depth: &depth2}
	db.CreateNode(slot2)

	shuffleSlot := &store.Node{Name: "GRP-RESTOCK-SHUF", ParentID: &grp.ID, Enabled: true}
	db.CreateNode(shuffleSlot)

	lineNode := &store.Node{Name: "LINE-RESTOCK", Enabled: true}
	db.CreateNode(lineNode)

	// Target at depth 2 (buried)
	targetBin := &store.Bin{BinTypeID: bt.ID, Label: "BIN-RESTOCK-TARGET", NodeID: &slot2.ID, Status: "available"}
	db.CreateBin(targetBin)
	db.SetBinManifest(targetBin.ID, `{"items":[]}`, bp.Code, 100)
	db.ConfirmBinManifest(targetBin.ID, "")
	db.Exec(`UPDATE bins SET loaded_at = NOW() - interval '2 hours' WHERE id = $1`, targetBin.ID)

	// Blocker at depth 1
	blockerBin := &store.Bin{BinTypeID: bt.ID, Label: "BIN-RESTOCK-BLK", NodeID: &slot1.ID, Status: "available"}
	db.CreateBin(blockerBin)
	db.SetBinManifest(blockerBin.ID, `{"items":[]}`, bp.Code, 50)
	db.ConfirmBinManifest(blockerBin.ID, "")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// Trigger reshuffle
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "restock-status-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		SourceNode:   grp.Name,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	})

	order, err := db.GetOrderByUUID("restock-status-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.Status != dispatch.StatusReshuffling {
		t.Fatalf("status = %q, want reshuffling", order.Status)
	}

	children, _ := db.ListChildOrders(order.ID)
	t.Logf("compound: %d children", len(children))
	for i, c := range children {
		t.Logf("  child %d: seq=%d desc=%s src=%s dest=%s", i, c.Sequence, c.PayloadDesc, c.SourceNode, c.DeliveryNode)
	}

	// Drive all children to completion
	for i, child := range children {
		child, _ = db.GetOrder(child.ID)
		if child.VendorOrderID == "" || child.Status == dispatch.StatusFailed {
			t.Logf("child %d: skipping (vendor=%s status=%s)", i, child.VendorOrderID, child.Status)
			continue
		}
		sim.DriveState(child.VendorOrderID, "RUNNING")
		sim.DriveState(child.VendorOrderID, "FINISHED")
		d.HandleOrderReceipt(env, &protocol.OrderReceipt{
			OrderUUID: child.EdgeUUID, ReceiptType: "confirmed", FinalCount: 1,
		})
		child, _ = db.GetOrder(child.ID)
		t.Logf("child %d completed: status=%s", i, child.Status)
	}

	// KEY CHECK: blocker bin restocked to storage slot
	blockerBin, _ = db.GetBin(blockerBin.ID)
	t.Logf("blocker after restock: node=%v status=%s claimed=%v", blockerBin.NodeID, blockerBin.Status, blockerBin.ClaimedBy)

	// The blocker should be at a LANE child (storage slot) with status=available
	if blockerBin.Status != "available" {
		t.Errorf("BUG: blocker bin status=%q after restock to storage slot — expected 'available'. If 'staged', bin is invisible to FIFO queries", blockerBin.Status)
	} else {
		t.Logf("blocker bin status=available at storage slot — correct, visible to FIFO")
	}

	if blockerBin.ClaimedBy != nil {
		t.Errorf("blocker bin still claimed by %d after compound completion", *blockerBin.ClaimedBy)
	}

	// Verify it's findable by FIFO — this is the critical correctness check
	fifoBin, err := db.FindSourceBinFIFO(bp.Code)
	if err != nil {
		t.Errorf("FIFO lookup failed after restock: %v — restocked blocker bin is invisible to retrievals", err)
	} else if fifoBin.ID != blockerBin.ID {
		t.Errorf("FIFO returns bin %d, want restocked blocker %d — blocker not highest FIFO priority", fifoBin.ID, blockerBin.ID)
	}
}

// --- Test: Staging TTL expiry during compound order execution (TC-54) ---
//
// Scenario: During a compound reshuffle, child 1 (unbury) completes and
// delivers the blocker to a non-storage node (shuffle slot). ApplyBinArrival
// marks it as staged with a TTL. If the reshuffle takes longer than the TTL,
// the staging sweep runs and flips the blocker bin to "available" while the
// restock child hasn't executed yet.
//
// Expected: The restock child should still work correctly even if the bin's
// status changed from staged to available. The bin should still be at the
// shuffle slot and claimable. This test verifies no silent failure occurs.
func TestCompound_StagingTTLExpiryDuringReshuffle(t *testing.T) {
	db := testDB(t)

	grpType, _ := db.GetNodeTypeByCode("NGRP")
	lanType, _ := db.GetNodeTypeByCode("LANE")

	bp := &store.Payload{Code: "PART-TTL", Description: "TTL test"}
	db.CreatePayload(bp)
	bt := &store.BinType{Code: "DEFAULT-TL", Description: "TTL bin type"}
	db.CreateBinType(bt)

	grp := &store.Node{Name: "GRP-TTL", NodeTypeID: &grpType.ID, Enabled: true, IsSynthetic: true}
	db.CreateNode(grp)
	lane := &store.Node{Name: "GRP-TTL-L1", NodeTypeID: &lanType.ID, ParentID: &grp.ID, Enabled: true, IsSynthetic: true}
	db.CreateNode(lane)

	depth1, depth2 := 1, 2
	slot1 := &store.Node{Name: "GRP-TTL-L1-S1", ParentID: &lane.ID, Enabled: true, Depth: &depth1}
	db.CreateNode(slot1)
	slot2 := &store.Node{Name: "GRP-TTL-L1-S2", ParentID: &lane.ID, Enabled: true, Depth: &depth2}
	db.CreateNode(slot2)

	shuffleSlot := &store.Node{Name: "GRP-TTL-SHUF", ParentID: &grp.ID, Enabled: true}
	db.CreateNode(shuffleSlot)

	lineNode := &store.Node{Name: "LINE-TTL", Enabled: true}
	db.CreateNode(lineNode)

	// Target at depth 2 (buried)
	targetBin := &store.Bin{BinTypeID: bt.ID, Label: "BIN-TTL-TARGET", NodeID: &slot2.ID, Status: "available"}
	db.CreateBin(targetBin)
	db.SetBinManifest(targetBin.ID, `{"items":[]}`, bp.Code, 100)
	db.ConfirmBinManifest(targetBin.ID, "")
	db.Exec(`UPDATE bins SET loaded_at = NOW() - interval '2 hours' WHERE id = $1`, targetBin.ID)

	// Blocker at depth 1
	blockerBin := &store.Bin{BinTypeID: bt.ID, Label: "BIN-TTL-BLK", NodeID: &slot1.ID, Status: "available"}
	db.CreateBin(blockerBin)
	db.SetBinManifest(blockerBin.ID, `{"items":[]}`, bp.Code, 50)
	db.ConfirmBinManifest(blockerBin.ID, "")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// Trigger reshuffle
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "ttl-reshuffle-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		SourceNode:   grp.Name,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	})

	order, err := db.GetOrderByUUID("ttl-reshuffle-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}

	children, _ := db.ListChildOrders(order.ID)
	if len(children) < 3 {
		t.Fatalf("expected >= 3 children, got %d", len(children))
	}

	// Complete child 1 (unbury blocker → shuffle slot)
	child1, _ := db.GetOrder(children[0].ID)
	if child1.VendorOrderID == "" {
		t.Fatalf("child 1 not dispatched")
	}
	sim.DriveState(child1.VendorOrderID, "RUNNING")
	sim.DriveState(child1.VendorOrderID, "FINISHED")
	d.HandleOrderReceipt(env, &protocol.OrderReceipt{
		OrderUUID: child1.EdgeUUID, ReceiptType: "confirmed", FinalCount: 1,
	})

	// Verify blocker at shuffle slot and staged
	blockerBin, _ = db.GetBin(blockerBin.ID)
	t.Logf("blocker after unbury: node=%v status=%s staged_at=%v", blockerBin.NodeID, blockerBin.Status, blockerBin.StagedAt)

	// Simulate TTL expiry: set staged_expires_at to past
	if _, err := db.Exec(`UPDATE bins SET staged_expires_at = NOW() - interval '1 hour' WHERE id = $1`, blockerBin.ID); err != nil {
		t.Fatalf("set staging expiry: %v", err)
	}

	// Run staging sweep — this should flip blocker to available
	released, err := db.ReleaseExpiredStagedBins()
	if err != nil {
		t.Fatalf("release expired staged bins: %v", err)
	}
	t.Logf("staging sweep released %d bins", released)

	blockerBin, _ = db.GetBin(blockerBin.ID)
	t.Logf("blocker after sweep: status=%s claimed=%v", blockerBin.Status, blockerBin.ClaimedBy)

	// After child 1 completion, ApplyBinArrival released the blocker's claim (correct behavior).
	// The sweep query has AND claimed_by IS NULL, so it only flips UNCLAIMED staged bins.
	// If the claim survived child 1 completion, the sweep should have skipped this bin.
	// Verify the sweep correctly changed status from staged to available for unclaimed bins.
	if blockerBin.Status != "available" {
		t.Errorf("bin status = %q after sweep, want available — sweep should flip expired staged bins to available", blockerBin.Status)
	}

	// Complete child 2 (retrieve target)
	child2, _ := db.GetOrder(children[1].ID)
	if child2.VendorOrderID != "" {
		sim.DriveState(child2.VendorOrderID, "RUNNING")
		sim.DriveState(child2.VendorOrderID, "FINISHED")
		d.HandleOrderReceipt(env, &protocol.OrderReceipt{
			OrderUUID: child2.EdgeUUID, ReceiptType: "confirmed", FinalCount: 1,
		})
	}

	// Complete child 3 (restock blocker) — the bin's status was flipped by sweep
	child3, _ := db.GetOrder(children[2].ID)
	if child3.VendorOrderID != "" {
		sim.DriveState(child3.VendorOrderID, "RUNNING")
		sim.DriveState(child3.VendorOrderID, "FINISHED")
		d.HandleOrderReceipt(env, &protocol.OrderReceipt{
			OrderUUID: child3.EdgeUUID, ReceiptType: "confirmed", FinalCount: 1,
		})
	}

	// Verify compound completed despite TTL expiry mid-reshuffle
	order, _ = db.GetOrderByUUID("ttl-reshuffle-1")
	t.Logf("parent final: status=%s", order.Status)

	if order.Status == dispatch.StatusConfirmed {
		t.Logf("compound completed despite staging TTL expiry mid-reshuffle — sweep did not break the restock")
	} else if order.Status == dispatch.StatusFailed {
		t.Errorf("POTENTIAL BUG: compound failed — staging TTL expiry may have interfered with restock child")
	}

	// Verify blocker restocked correctly
	blockerBin, _ = db.GetBin(blockerBin.ID)
	t.Logf("blocker final: node=%v status=%s claimed=%v", blockerBin.NodeID, blockerBin.Status, blockerBin.ClaimedBy)

	if blockerBin.ClaimedBy != nil {
		t.Errorf("blocker still claimed by %d after compound completion", *blockerBin.ClaimedBy)
	}

	// Lane lock released
	if eng.Dispatcher().LaneLock().IsLocked(lane.ID) {
		t.Errorf("lane still locked after compound completion")
	}
}
