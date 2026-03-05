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

// --- Helper: setup supermarket with shuffle row ---

func setupSupermarketWithShuffle(t *testing.T, db *store.DB) (sup, lane *store.Node, slots []*store.Node, shuffleSlots []*store.Node, style *store.PayloadStyle) {
	t.Helper()
	supType, _ := db.GetNodeTypeByCode("SUP")
	lanType, _ := db.GetNodeTypeByCode("LAN")
	shfType, _ := db.GetNodeTypeByCode("SHF")

	style = &store.PayloadStyle{Name: "PART-X", Code: "PTX", FormFactor: "tote", DefaultManifestJSON: "{}"}
	db.CreatePayloadStyle(style)

	// Create SUP
	sup = &store.Node{Name: "SM-TEST", NodeType: "storage", NodeTypeID: &supType.ID, Capacity: 0, Enabled: true}
	db.CreateNode(sup)

	// Create 1 lane with 5 slots
	lane = &store.Node{Name: "SM-TEST-L1", NodeType: "storage", NodeTypeID: &lanType.ID, ParentID: &sup.ID, Capacity: 0, Enabled: true}
	db.CreateNode(lane)

	slots = make([]*store.Node, 5)
	for d := 1; d <= 5; d++ {
		slot := &store.Node{
			Name: fmt.Sprintf("SM-TEST-L1-S%d", d), NodeType: "storage",
			ParentID: &lane.ID, Capacity: 1, Enabled: true,
			VendorLocation: fmt.Sprintf("LOC-L1-S%d", d),
		}
		db.CreateNode(slot)
		db.SetNodeProperty(slot.ID, "depth", fmt.Sprintf("%d", d))
		slots[d-1] = slot
	}

	// Create SHF with 4 shuffle slots
	shf := &store.Node{Name: "SM-TEST-SHF", NodeType: "storage", NodeTypeID: &shfType.ID, ParentID: &sup.ID, Capacity: 0, Enabled: true}
	db.CreateNode(shf)

	shuffleSlots = make([]*store.Node, 4)
	for i := 0; i < 4; i++ {
		ss := &store.Node{
			Name: fmt.Sprintf("SM-TEST-SHF-%d", i+1), NodeType: "storage",
			ParentID: &shf.ID, Capacity: 1, Enabled: true,
			VendorLocation: fmt.Sprintf("LOC-SHF-%d", i+1),
		}
		db.CreateNode(ss)
		shuffleSlots[i] = ss
	}

	// Read back to get joined fields
	sup, _ = db.GetNode(sup.ID)
	lane, _ = db.GetNode(lane.ID)

	return
}

// --- Tests ---

func TestPlanReshuffle_SingleBlocker(t *testing.T) {
	db := testDB(t)
	sup, lane, slots, _, style := setupSupermarketWithShuffle(t, db)

	// Place blocker A at depth 1
	blockerA := &store.PayloadInstance{StyleID: style.ID, NodeID: &slots[0].ID, Status: "available", TagID: "A"}
	if err := db.CreateInstance(blockerA); err != nil {
		t.Fatalf("create blocker A: %v", err)
	}

	// Place target B at depth 2
	targetB := &store.PayloadInstance{StyleID: style.ID, NodeID: &slots[1].ID, Status: "available", TagID: "B"}
	if err := db.CreateInstance(targetB); err != nil {
		t.Fatalf("create target B: %v", err)
	}

	plan, err := PlanReshuffle(db, targetB, slots[1], lane, sup.ID)
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
	if plan.Steps[0].InstanceID != blockerA.ID {
		t.Errorf("step 1 instance = %d, want %d", plan.Steps[0].InstanceID, blockerA.ID)
	}
	if plan.Steps[0].Sequence != 1 {
		t.Errorf("step 1 sequence = %d, want 1", plan.Steps[0].Sequence)
	}

	// Step 2: retrieve B (depth 2)
	if plan.Steps[1].StepType != "retrieve" {
		t.Errorf("step 2 type = %q, want %q", plan.Steps[1].StepType, "retrieve")
	}
	if plan.Steps[1].InstanceID != targetB.ID {
		t.Errorf("step 2 instance = %d, want %d", plan.Steps[1].InstanceID, targetB.ID)
	}
	if plan.Steps[1].Sequence != 2 {
		t.Errorf("step 2 sequence = %d, want 2", plan.Steps[1].Sequence)
	}

	// Step 3: restock A (shuffle -> depth 1)
	if plan.Steps[2].StepType != "restock" {
		t.Errorf("step 3 type = %q, want %q", plan.Steps[2].StepType, "restock")
	}
	if plan.Steps[2].InstanceID != blockerA.ID {
		t.Errorf("step 3 instance = %d, want %d", plan.Steps[2].InstanceID, blockerA.ID)
	}
	if plan.Steps[2].Sequence != 3 {
		t.Errorf("step 3 sequence = %d, want 3", plan.Steps[2].Sequence)
	}
}

func TestPlanReshuffle_MultipleBlockers(t *testing.T) {
	db := testDB(t)
	sup, lane, slots, _, style := setupSupermarketWithShuffle(t, db)

	// Place blocker at depth 1
	blocker1 := &store.PayloadInstance{StyleID: style.ID, NodeID: &slots[0].ID, Status: "available", TagID: "B1"}
	if err := db.CreateInstance(blocker1); err != nil {
		t.Fatalf("create blocker1: %v", err)
	}

	// Place blocker at depth 2
	blocker2 := &store.PayloadInstance{StyleID: style.ID, NodeID: &slots[1].ID, Status: "available", TagID: "B2"}
	if err := db.CreateInstance(blocker2); err != nil {
		t.Fatalf("create blocker2: %v", err)
	}

	// Place target at depth 3
	target := &store.PayloadInstance{StyleID: style.ID, NodeID: &slots[2].ID, Status: "available", TagID: "TGT"}
	if err := db.CreateInstance(target); err != nil {
		t.Fatalf("create target: %v", err)
	}

	plan, err := PlanReshuffle(db, target, slots[2], lane, sup.ID)
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
	if plan.Steps[0].InstanceID != blocker1.ID {
		t.Errorf("step 1 instance = %d, want %d (depth 1 blocker)", plan.Steps[0].InstanceID, blocker1.ID)
	}

	if plan.Steps[1].StepType != "unbury" {
		t.Errorf("step 2 type = %q, want %q", plan.Steps[1].StepType, "unbury")
	}
	if plan.Steps[1].InstanceID != blocker2.ID {
		t.Errorf("step 2 instance = %d, want %d (depth 2 blocker)", plan.Steps[1].InstanceID, blocker2.ID)
	}

	// Retrieve step
	if plan.Steps[2].StepType != "retrieve" {
		t.Errorf("step 3 type = %q, want %q", plan.Steps[2].StepType, "retrieve")
	}
	if plan.Steps[2].InstanceID != target.ID {
		t.Errorf("step 3 instance = %d, want %d (target)", plan.Steps[2].InstanceID, target.ID)
	}

	// Restock steps: deepest-first (depth 2, then depth 1)
	if plan.Steps[3].StepType != "restock" {
		t.Errorf("step 4 type = %q, want %q", plan.Steps[3].StepType, "restock")
	}
	if plan.Steps[3].InstanceID != blocker2.ID {
		t.Errorf("step 4 instance = %d, want %d (depth 2 restock first)", plan.Steps[3].InstanceID, blocker2.ID)
	}

	if plan.Steps[4].StepType != "restock" {
		t.Errorf("step 5 type = %q, want %q", plan.Steps[4].StepType, "restock")
	}
	if plan.Steps[4].InstanceID != blocker1.ID {
		t.Errorf("step 5 instance = %d, want %d (depth 1 restock last)", plan.Steps[4].InstanceID, blocker1.ID)
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
	sup, lane, slots, shuffleSlots, style := setupSupermarketWithShuffle(t, db)

	// Fill all 4 shuffle slots with instances
	for i, ss := range shuffleSlots {
		inst := &store.PayloadInstance{
			StyleID: style.ID, NodeID: &ss.ID, Status: "available",
			TagID: fmt.Sprintf("SHF-%d", i+1),
		}
		if err := db.CreateInstance(inst); err != nil {
			t.Fatalf("create shuffle instance %d: %v", i+1, err)
		}
	}

	// Place blocker at depth 1
	blocker := &store.PayloadInstance{StyleID: style.ID, NodeID: &slots[0].ID, Status: "available", TagID: "BLK"}
	if err := db.CreateInstance(blocker); err != nil {
		t.Fatalf("create blocker: %v", err)
	}

	// Place target at depth 2
	target := &store.PayloadInstance{StyleID: style.ID, NodeID: &slots[1].ID, Status: "available", TagID: "TGT"}
	if err := db.CreateInstance(target); err != nil {
		t.Fatalf("create target: %v", err)
	}

	_, err := PlanReshuffle(db, target, slots[1], lane, sup.ID)
	if err == nil {
		t.Fatal("expected error about insufficient shuffle slots, got nil")
	}

	_ = sup // used to pass supermarketID
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
	sup, lane, slots, _, style := setupSupermarketWithShuffle(t, db)

	// Place blocker at depth 1
	blocker := &store.PayloadInstance{StyleID: style.ID, NodeID: &slots[0].ID, Status: "available", TagID: "BLK"}
	if err := db.CreateInstance(blocker); err != nil {
		t.Fatalf("create blocker: %v", err)
	}

	// Place target at depth 2
	target := &store.PayloadInstance{StyleID: style.ID, NodeID: &slots[1].ID, Status: "available", TagID: "TGT"}
	if err := db.CreateInstance(target); err != nil {
		t.Fatalf("create target: %v", err)
	}

	// Create parent order
	parentOrder := &store.Order{
		EdgeUUID:     "uuid-compound",
		StationID:    "line-1",
		OrderType:    OrderTypeRetrieve,
		Status:       StatusSourcing,
		DeliveryNode: "LINE1-DEST",
	}
	// Create a delivery node so dispatchToFleet can resolve it
	destNode := &store.Node{Name: "LINE1-DEST", NodeType: "line_side", VendorLocation: "LOC-DEST", Capacity: 5, Enabled: true}
	if err := db.CreateNode(destNode); err != nil {
		t.Fatalf("create dest node: %v", err)
	}
	if err := db.CreateOrder(parentOrder); err != nil {
		t.Fatalf("create parent order: %v", err)
	}

	// Plan the reshuffle
	plan, err := PlanReshuffle(db, target, slots[1], lane, sup.ID)
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

	// Verify pickup/delivery nodes on child orders
	for _, child := range children {
		if child.Sequence == 1 {
			// Unbury: pickup from lane slot, delivery to shuffle slot
			if child.PickupNode == "" {
				t.Error("child seq 1 (unbury) has empty pickup node")
			}
			if child.DeliveryNode == "" {
				t.Error("child seq 1 (unbury) has empty delivery node")
			}
		}
		if child.Sequence == 2 {
			// Retrieve: pickup from target slot, delivery to parent's delivery
			if child.PickupNode == "" {
				t.Error("child seq 2 (retrieve) has empty pickup node")
			}
		}
		if child.Sequence == 3 {
			// Restock: pickup from shuffle slot, delivery back to lane slot
			if child.PickupNode == "" {
				t.Error("child seq 3 (restock) has empty pickup node")
			}
			if child.DeliveryNode == "" {
				t.Error("child seq 3 (restock) has empty delivery node")
			}
		}
	}
}

func TestHandleChildOrderFailure(t *testing.T) {
	db := testDB(t)
	_, lane, slots, _, style := setupSupermarketWithShuffle(t, db)

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
		PickupNode:    slots[0].Name,
		DeliveryNode:  "SM-TEST-SHF-1",
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
		PickupNode:    slots[1].Name,
		DeliveryNode:  "LINE1-DEST",
	}
	if err := db.CreateOrder(child2); err != nil {
		t.Fatalf("create child2: %v", err)
	}

	// Create an instance claimed by child3 to verify unclaim on cancel
	inst := &store.PayloadInstance{StyleID: style.ID, NodeID: &slots[2].ID, Status: "available", TagID: "C3"}
	if err := db.CreateInstance(inst); err != nil {
		t.Fatalf("create instance: %v", err)
	}

	child3 := &store.Order{
		EdgeUUID:      "uuid-fail-parent-step-3",
		StationID:     "line-1",
		OrderType:     OrderTypeMove,
		Status:        StatusPending,
		ParentOrderID: &parentOrder.ID,
		Sequence:      3,
		PickupNode:    slots[2].Name,
		DeliveryNode:  slots[0].Name,
	}
	if err := db.CreateOrder(child3); err != nil {
		t.Fatalf("create child3: %v", err)
	}

	// Claim the instance by child3
	db.ClaimInstance(inst.ID, child3.ID)

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

	// Verify instance claimed by child3 was unclaimed
	instGot, err := db.GetInstance(inst.ID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if instGot.ClaimedBy != nil {
		t.Errorf("instance claimed_by = %v, want nil (should be unclaimed after cancel)", instGot.ClaimedBy)
	}

	// Verify lane lock is released
	if d.laneLock.IsLocked(lane.ID) {
		t.Error("lane lock is still held after child failure, want released")
	}
}
