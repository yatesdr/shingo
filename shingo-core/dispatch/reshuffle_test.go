package dispatch

import (
	"fmt"
	"testing"

	"shingocore/fleet"
	"shingocore/store"
)

// --- Mock fleet backend that succeeds ---

type mockSuccessBackend struct{ mockBackend }

func (m *mockSuccessBackend) CreateTransportOrder(req fleet.TransportOrderRequest) (fleet.TransportOrderResult, error) {
	return fleet.TransportOrderResult{VendorOrderID: "vendor-" + req.OrderID}, nil
}

// --- Helper: setup node group with direct children for shuffle ---

func setupNodeGroupWithShuffle(t *testing.T, db *store.DB) (grp, lane *store.Node, slots []*store.Node, shuffleSlots []*store.Node, bp *store.Payload) {
	t.Helper()
	grpType, _ := db.GetNodeTypeByCode("NGRP")
	lanType, _ := db.GetNodeTypeByCode("LANE")

	bp = &store.Payload{Code: "PTX"}
	db.CreatePayload(bp)

	// Create NGRP
	grp = &store.Node{Name: "GRP-TEST", NodeTypeID: &grpType.ID, Enabled: true, IsSynthetic: true}
	db.CreateNode(grp)

	// Create 1 lane with 5 slots
	lane = &store.Node{Name: "GRP-TEST-L1", NodeTypeID: &lanType.ID, ParentID: &grp.ID, Enabled: true, IsSynthetic: true}
	db.CreateNode(lane)

	slots = make([]*store.Node, 5)
	for d := 1; d <= 5; d++ {
		depth := d
		slot := &store.Node{
			Name:     fmt.Sprintf("GRP-TEST-L1-S%d", d),
			ParentID: &lane.ID, Enabled: true, Depth: &depth,
		}
		db.CreateNode(slot)
		slots[d-1] = slot
	}

	// Create 4 direct physical children of the group (shuffle slots)
	shuffleSlots = make([]*store.Node, 4)
	for i := 0; i < 4; i++ {
		ss := &store.Node{
			Name:     fmt.Sprintf("GRP-TEST-DC-%d", i+1),
			ParentID: &grp.ID, Enabled: true,
		}
		db.CreateNode(ss)
		shuffleSlots[i] = ss
	}

	// Read back to get joined fields
	grp, _ = db.GetNode(grp.ID)
	lane, _ = db.GetNode(lane.ID)

	return
}

// --- Tests ---

func TestPlanReshuffle_SingleBlocker(t *testing.T) {
	db := testDB(t)
	grp, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)

	// Place blocker A at depth 1
	blockerA := createTestBinAtNode(t, db, bp.Code, slots[0].ID, "BIN-A")

	// Place target B at depth 2
	targetB := createTestBinAtNode(t, db, bp.Code, slots[1].ID, "BIN-B")

	plan, err := PlanReshuffle(db, targetB, slots[1], lane, grp.ID)
	if err != nil {
		t.Fatalf("PlanReshuffle: %v", err)
	}

	// Verify 3 steps: unbury A, retrieve B, restock A
	if len(plan.Steps) != 3 {
		t.Fatalf("steps = %d, want 3", len(plan.Steps))
	}

	// Step 1: unbury A (depth 1 -> shuffle)
	if plan.Steps[0].StepType != "unbury" {
		t.Errorf("step 1 type = %q, want %q", plan.Steps[0].StepType, "unbury")
	}
	if plan.Steps[0].BinID != blockerA.ID {
		t.Errorf("step 1 bin = %d, want %d", plan.Steps[0].BinID, blockerA.ID)
	}
	if plan.Steps[0].Sequence != 1 {
		t.Errorf("step 1 sequence = %d, want 1", plan.Steps[0].Sequence)
	}

	// Step 2: retrieve B (depth 2)
	if plan.Steps[1].StepType != "retrieve" {
		t.Errorf("step 2 type = %q, want %q", plan.Steps[1].StepType, "retrieve")
	}
	if plan.Steps[1].BinID != targetB.ID {
		t.Errorf("step 2 bin = %d, want %d", plan.Steps[1].BinID, targetB.ID)
	}
	if plan.Steps[1].Sequence != 2 {
		t.Errorf("step 2 sequence = %d, want 2", plan.Steps[1].Sequence)
	}

	// Step 3: restock A (shuffle -> depth 1)
	if plan.Steps[2].StepType != "restock" {
		t.Errorf("step 3 type = %q, want %q", plan.Steps[2].StepType, "restock")
	}
	if plan.Steps[2].BinID != blockerA.ID {
		t.Errorf("step 3 bin = %d, want %d", plan.Steps[2].BinID, blockerA.ID)
	}
	if plan.Steps[2].Sequence != 3 {
		t.Errorf("step 3 sequence = %d, want 3", plan.Steps[2].Sequence)
	}
}

func TestPlanReshuffle_MultipleBlockers(t *testing.T) {
	db := testDB(t)
	grp, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)

	// Place blocker at depth 1
	blocker1 := createTestBinAtNode(t, db, bp.Code, slots[0].ID, "BIN-B1")

	// Place blocker at depth 2
	blocker2 := createTestBinAtNode(t, db, bp.Code, slots[1].ID, "BIN-B2")

	// Place target at depth 3
	target := createTestBinAtNode(t, db, bp.Code, slots[2].ID, "BIN-TGT")

	plan, err := PlanReshuffle(db, target, slots[2], lane, grp.ID)
	if err != nil {
		t.Fatalf("PlanReshuffle: %v", err)
	}

	// Verify 5 steps: unbury depth 1, unbury depth 2, retrieve depth 3, restock depth 2, restock depth 1
	if len(plan.Steps) != 5 {
		t.Fatalf("steps = %d, want 5", len(plan.Steps))
	}

	// Unbury steps: shallowest first (depth 1, then depth 2)
	if plan.Steps[0].StepType != "unbury" {
		t.Errorf("step 1 type = %q, want %q", plan.Steps[0].StepType, "unbury")
	}
	if plan.Steps[0].BinID != blocker1.ID {
		t.Errorf("step 1 bin = %d, want %d (depth 1 blocker)", plan.Steps[0].BinID, blocker1.ID)
	}

	if plan.Steps[1].StepType != "unbury" {
		t.Errorf("step 2 type = %q, want %q", plan.Steps[1].StepType, "unbury")
	}
	if plan.Steps[1].BinID != blocker2.ID {
		t.Errorf("step 2 bin = %d, want %d (depth 2 blocker)", plan.Steps[1].BinID, blocker2.ID)
	}

	// Retrieve step
	if plan.Steps[2].StepType != "retrieve" {
		t.Errorf("step 3 type = %q, want %q", plan.Steps[2].StepType, "retrieve")
	}
	if plan.Steps[2].BinID != target.ID {
		t.Errorf("step 3 bin = %d, want %d (target)", plan.Steps[2].BinID, target.ID)
	}

	// Restock steps: deepest-first (depth 2, then depth 1)
	if plan.Steps[3].StepType != "restock" {
		t.Errorf("step 4 type = %q, want %q", plan.Steps[3].StepType, "restock")
	}
	if plan.Steps[3].BinID != blocker2.ID {
		t.Errorf("step 4 bin = %d, want %d (depth 2 restock first)", plan.Steps[3].BinID, blocker2.ID)
	}

	if plan.Steps[4].StepType != "restock" {
		t.Errorf("step 5 type = %q, want %q", plan.Steps[4].StepType, "restock")
	}
	if plan.Steps[4].BinID != blocker1.ID {
		t.Errorf("step 5 bin = %d, want %d (depth 1 restock last)", plan.Steps[4].BinID, blocker1.ID)
	}

	// Verify sequences
	for i, step := range plan.Steps {
		if step.Sequence != i+1 {
			t.Errorf("step %d sequence = %d, want %d", i+1, step.Sequence, i+1)
		}
	}
}

func TestPlanReshuffle_NoShuffleSlots(t *testing.T) {
	db := testDB(t)
	grp, lane, slots, shuffleSlots, bp := setupNodeGroupWithShuffle(t, db)

	// Fill all 4 direct children (shuffle slots) with bins
	for i, ss := range shuffleSlots {
		createTestBinAtNode(t, db, bp.Code, ss.ID, fmt.Sprintf("BIN-DC-%d", i+1))
	}

	// Place blocker at depth 1
	createTestBinAtNode(t, db, bp.Code, slots[0].ID, "BIN-BLK")

	// Place target at depth 2
	target := createTestBinAtNode(t, db, bp.Code, slots[1].ID, "BIN-TGT")

	_, err := PlanReshuffle(db, target, slots[1], lane, grp.ID)
	if err == nil {
		t.Fatal("expected error about insufficient shuffle slots, got nil")
	}

	_ = grp // used to pass groupID
}

func TestLaneLock_PreventsConcurrent(t *testing.T) {
	ll := NewLaneLock()

	var laneID int64 = 42

	// TryLock(lane, order 1) -> should succeed
	if !ll.TryLock(laneID, 1) {
		t.Fatal("TryLock(lane, order 1) = false, want true")
	}

	// TryLock(lane, order 2) -> should fail
	if ll.TryLock(laneID, 2) {
		t.Fatal("TryLock(lane, order 2) = true, want false (already locked)")
	}

	// IsLocked -> true
	if !ll.IsLocked(laneID) {
		t.Error("IsLocked = false, want true")
	}

	// LockedBy -> order 1
	if got := ll.LockedBy(laneID); got != 1 {
		t.Errorf("LockedBy = %d, want 1", got)
	}

	// Unlock
	ll.Unlock(laneID)

	// IsLocked -> false
	if ll.IsLocked(laneID) {
		t.Error("IsLocked = true after Unlock, want false")
	}

	// TryLock(lane, order 3) -> should succeed
	if !ll.TryLock(laneID, 3) {
		t.Fatal("TryLock(lane, order 3) = false after unlock, want true")
	}
}

func TestCompoundOrderCreation(t *testing.T) {
	db := testDB(t)
	grp, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)

	// Place blocker at depth 1
	createTestBinAtNode(t, db, bp.Code, slots[0].ID, "BIN-CMP-BLK")

	// Place target at depth 2
	target := createTestBinAtNode(t, db, bp.Code, slots[1].ID, "BIN-CMP-TGT")

	// Create parent order
	parentOrder := &store.Order{
		EdgeUUID:     "uuid-compound",
		StationID:    "line-1",
		OrderType:    OrderTypeRetrieve,
		Status:       StatusSourcing,
		DeliveryNode: "LINE1-DEST",
	}
	// Create a delivery node so dispatchToFleet can resolve it
	destNode := &store.Node{Name: "LINE1-DEST", Enabled: true}
	if err := db.CreateNode(destNode); err != nil {
		t.Fatalf("create dest node: %v", err)
	}
	if err := db.CreateOrder(parentOrder); err != nil {
		t.Fatalf("create parent order: %v", err)
	}

	// Plan the reshuffle
	plan, err := PlanReshuffle(db, target, slots[1], lane, grp.ID)
	if err != nil {
		t.Fatalf("PlanReshuffle: %v", err)
	}

	// Create dispatcher with success backend
	d, _ := newTestDispatcher(t, db, &mockSuccessBackend{})

	// Create compound order
	if err := d.CreateCompoundOrder(parentOrder, plan); err != nil {
		t.Fatalf("CreateCompoundOrder: %v", err)
	}

	// Verify parent order status is "reshuffling"
	parentGot, err := db.GetOrder(parentOrder.ID)
	if err != nil {
		t.Fatalf("get parent order: %v", err)
	}
	if parentGot.Status != StatusReshuffling {
		t.Errorf("parent status = %q, want %q", parentGot.Status, StatusReshuffling)
	}

	// Verify child orders
	children, err := db.ListChildOrders(parentOrder.ID)
	if err != nil {
		t.Fatalf("ListChildOrders: %v", err)
	}
	if len(children) != 3 {
		t.Fatalf("child count = %d, want 3", len(children))
	}

	// Verify child orders have correct parent_order_id
	for _, child := range children {
		if child.ParentOrderID == nil || *child.ParentOrderID != parentOrder.ID {
			t.Errorf("child %d parent_order_id = %v, want %d", child.ID, child.ParentOrderID, parentOrder.ID)
		}
	}

	// Verify sequences
	seqSeen := make(map[int]bool)
	for _, child := range children {
		seqSeen[child.Sequence] = true
	}
	for _, seq := range []int{1, 2, 3} {
		if !seqSeen[seq] {
			t.Errorf("missing child with sequence %d", seq)
		}
	}

	// Verify source/delivery nodes on child orders
	for _, child := range children {
		if child.Sequence == 1 {
			// Unbury: pickup from lane slot, delivery to shuffle slot
			if child.SourceNode == "" {
				t.Error("child seq 1 (unbury) has empty source node")
			}
			if child.DeliveryNode == "" {
				t.Error("child seq 1 (unbury) has empty delivery node")
			}
		}
		if child.Sequence == 2 {
			// Retrieve: pickup from target slot, delivery to parent's delivery
			if child.SourceNode == "" {
				t.Error("child seq 2 (retrieve) has empty source node")
			}
		}
		if child.Sequence == 3 {
			// Restock: pickup from shuffle slot, delivery back to lane slot
			if child.SourceNode == "" {
				t.Error("child seq 3 (restock) has empty source node")
			}
			if child.DeliveryNode == "" {
				t.Error("child seq 3 (restock) has empty delivery node")
			}
		}
	}
}

func TestHandleChildOrderFailure(t *testing.T) {
	db := testDB(t)
	_, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)

	// Create parent order
	parentOrder := &store.Order{
		EdgeUUID:  "uuid-fail-parent",
		StationID: "line-1",
		OrderType: OrderTypeRetrieve,
		Status:    StatusReshuffling,
	}
	if err := db.CreateOrder(parentOrder); err != nil {
		t.Fatalf("create parent order: %v", err)
	}

	// Create 3 child orders
	child1 := &store.Order{
		EdgeUUID:      "uuid-fail-parent-step-1",
		StationID:     "line-1",
		OrderType:     OrderTypeMove,
		Status:        StatusConfirmed,
		ParentOrderID: &parentOrder.ID,
		Sequence:      1,
		SourceNode:    slots[0].Name,
		DeliveryNode:  "GRP-TEST-DC-1",
	}
	if err := db.CreateOrder(child1); err != nil {
		t.Fatalf("create child1: %v", err)
	}

	child2 := &store.Order{
		EdgeUUID:      "uuid-fail-parent-step-2",
		StationID:     "line-1",
		OrderType:     OrderTypeMove,
		Status:        StatusFailed,
		ParentOrderID: &parentOrder.ID,
		Sequence:      2,
		SourceNode:    slots[1].Name,
		DeliveryNode:  "LINE1-DEST",
	}
	if err := db.CreateOrder(child2); err != nil {
		t.Fatalf("create child2: %v", err)
	}

	// Create a bin claimed by child3 to verify unclaim on cancel
	binC3 := createTestBinAtNode(t, db, bp.Code, slots[2].ID, "BIN-C3")

	child3 := &store.Order{
		EdgeUUID:      "uuid-fail-parent-step-3",
		StationID:     "line-1",
		OrderType:     OrderTypeMove,
		Status:        StatusPending,
		ParentOrderID: &parentOrder.ID,
		Sequence:      3,
		SourceNode:    slots[2].Name,
		DeliveryNode:  slots[0].Name,
	}
	if err := db.CreateOrder(child3); err != nil {
		t.Fatalf("create child3: %v", err)
	}

	// Claim the bin by child3
	db.ClaimBin(binC3.ID, child3.ID)

	// Lock the lane to verify it gets released
	d, emitter := newTestDispatcher(t, db, &mockBackend{})
	d.laneLock.TryLock(lane.ID, parentOrder.ID)

	// Handle child 2 failure
	d.HandleChildOrderFailure(parentOrder.ID, child2.ID)

	// Verify child 3 is cancelled
	child3Got, err := db.GetOrder(child3.ID)
	if err != nil {
		t.Fatalf("get child3: %v", err)
	}
	if child3Got.Status != StatusCancelled {
		t.Errorf("child3 status = %q, want %q", child3Got.Status, StatusCancelled)
	}

	// Verify parent order is failed
	parentGot, err := db.GetOrder(parentOrder.ID)
	if err != nil {
		t.Fatalf("get parent: %v", err)
	}
	if parentGot.Status != StatusFailed {
		t.Errorf("parent status = %q, want %q", parentGot.Status, StatusFailed)
	}

	// Verify parent failure was emitted
	if len(emitter.failed) != 1 {
		t.Fatalf("failed events = %d, want 1", len(emitter.failed))
	}
	if emitter.failed[0].orderID != parentOrder.ID {
		t.Errorf("failed event order ID = %d, want %d", emitter.failed[0].orderID, parentOrder.ID)
	}

	// Verify bin claimed by child3 was unclaimed
	binGot, err := db.GetBin(binC3.ID)
	if err != nil {
		t.Fatalf("get bin: %v", err)
	}
	if binGot.ClaimedBy != nil {
		t.Errorf("bin claimed_by = %v, want nil (should be unclaimed after cancel)", binGot.ClaimedBy)
	}

	// Verify lane lock is released
	if d.laneLock.IsLocked(lane.ID) {
		t.Error("lane lock is still held after child failure, want released")
	}
}
