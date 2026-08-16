//go:build docker

package dispatch

import (
	"fmt"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/payloads"
)

// --- Helper: setup node group with direct children for shuffle ---

// THE ERRORS ARE CHECKED, and that is not tidiness. These two lookups used to
// discard their error and then dereference the result, so a transient database
// failure — a Postgres connect timeout under the gate's parallel per-test clone
// databases — arrived as `invalid memory address or nil pointer dereference` on
// the next line. A panic aborts the whole package binary, so it also took down
// every unrelated test running in parallel with it, and the report a reader got
// was a stack trace pointing at a fixture rather than "the database was not
// reachable". Diagnosed as the best-evidenced of three candidates for the docker
// intermittent (PLAN §R.38); this is the amplifier, not the root, and the root
// (the connect timeout itself) is still open.
func setupNodeGroupWithShuffle(t *testing.T, db *store.DB) (grp, lane *nodes.Node, slots []*nodes.Node, shuffleSlots []*nodes.Node, bp *payloads.Payload) {
	t.Helper()
	grpType, err := db.GetNodeTypeByCode("NGRP")
	if err != nil || grpType == nil {
		t.Fatalf("fixture: look up NGRP node type: %v (node type nil: %v). The node types are seeded "+
			"by the schema, so this is the DATABASE being unreachable, not a missing fixture — check "+
			"for `failed SASL auth: timeout` above and re-run the package serially (-p 1)",
			err, grpType == nil)
	}
	lanType, err := db.GetNodeTypeByCode("LANE")
	if err != nil || lanType == nil {
		t.Fatalf("fixture: look up LANE node type: %v (node type nil: %v) — see the NGRP arm above",
			err, lanType == nil)
	}

	bp = &payloads.Payload{Code: "PTX"}
	testutil.MustNoErr(t, db.CreatePayload(bp), "fixture: create payload")

	// Create NGRP
	grp = &nodes.Node{Name: "GRP-TEST", NodeTypeID: &grpType.ID, Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(grp), "fixture: create NGRP")

	// Create 1 lane with 5 slots
	lane = &nodes.Node{Name: "GRP-TEST-L1", NodeTypeID: &lanType.ID, ParentID: &grp.ID, Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(lane), "fixture: create lane")

	slots = make([]*nodes.Node, 5)
	for d := 1; d <= 5; d++ {
		depth := d
		slot := &nodes.Node{
			Name:     fmt.Sprintf("GRP-TEST-L1-S%d", d),
			ParentID: &lane.ID, Enabled: true, Depth: &depth,
		}
		testutil.MustNoErr(t, db.CreateNode(slot), "fixture: create lane slot")
		slots[d-1] = slot
	}

	// Create 4 direct physical children of the group (shuffle slots)
	shuffleSlots = make([]*nodes.Node, 4)
	for i := 0; i < 4; i++ {
		ss := &nodes.Node{
			Name:     fmt.Sprintf("GRP-TEST-DC-%d", i+1),
			ParentID: &grp.ID, Enabled: true,
		}
		testutil.MustNoErr(t, db.CreateNode(ss), "fixture: create shuffle slot")
		shuffleSlots[i] = ss
	}

	// Read back to get joined fields
	grp, err = db.GetNode(grp.ID)
	testutil.MustNoErr(t, err, "fixture: read back NGRP")
	lane, err = db.GetNode(lane.ID)
	testutil.MustNoErr(t, err, "fixture: read back lane")

	return
}

// --- Tests ---

func TestPlanReshuffle_SingleBlocker(t *testing.T) {
	t.Parallel()
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

	// Verify 2 steps: unbury A, retrieve B (no restock — blockers lie)
	if len(plan.Steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(plan.Steps))
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
}

func TestPlanReshuffle_MultipleBlockers(t *testing.T) {
	t.Parallel()
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

	// Verify 3 steps: unbury depth 1, unbury depth 2, retrieve depth 3
	// (no restock — blockers lie where the unbury parked them).
	if len(plan.Steps) != 3 {
		t.Fatalf("steps = %d, want 3", len(plan.Steps))
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

	// Verify sequences
	for i, step := range plan.Steps {
		if step.Sequence != i+1 {
			t.Errorf("step %d sequence = %d, want %d", i+1, step.Sequence, i+1)
		}
	}
}

func TestPlanReshuffle_NoShuffleSlots(t *testing.T) {
	t.Parallel()
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

func TestCompoundOrderCreation(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	grp, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)

	// Place blocker at depth 1
	createTestBinAtNode(t, db, bp.Code, slots[0].ID, "BIN-CMP-BLK")

	// Place target at depth 2
	target := createTestBinAtNode(t, db, bp.Code, slots[1].ID, "BIN-CMP-TGT")

	// Create parent order
	parentOrder := &orders.Order{
		EdgeUUID:     "uuid-compound",
		StationID:    "line-1",
		OrderType:    OrderTypeRetrieve,
		Status:       StatusSourcing,
		DeliveryNode: "LINE1-DEST",
	}
	// Create a delivery node so dispatchToFleet can resolve it
	destNode := &nodes.Node{Name: "LINE1-DEST", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(destNode), "create dest node")
	testutil.MustNoErr(t, db.CreateOrder(parentOrder), "create parent order")

	// Plan the reshuffle
	plan, err := PlanReshuffle(db, target, slots[1], lane, grp.ID)
	if err != nil {
		t.Fatalf("PlanReshuffle: %v", err)
	}

	// Create dispatcher with success backend
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	// Create compound order
	testutil.MustNoErr(t, d.CreateCompoundOrder(parentOrder, plan), "CreateCompoundOrder")

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
	if len(children) != 2 {
		t.Fatalf("child count = %d, want 2 (unbury + retrieve; no restock)", len(children))
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
	for _, seq := range []int{1, 2} {
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
	}
}

func TestHandleChildOrderFailure(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)

	// Create parent order
	parentOrder := &orders.Order{
		EdgeUUID:  "uuid-fail-parent",
		StationID: "line-1",
		OrderType: OrderTypeRetrieve,
		Status:    StatusReshuffling,
	}
	testutil.MustNoErr(t, db.CreateOrder(parentOrder), "create parent order")

	// Create 3 child orders
	child1 := &orders.Order{
		EdgeUUID:      "uuid-fail-parent-step-1",
		StationID:     "line-1",
		OrderType:     OrderTypeMove,
		Status:        StatusConfirmed,
		ParentOrderID: &parentOrder.ID,
		Sequence:      1,
		SourceNode:    slots[0].Name,
		DeliveryNode:  "GRP-TEST-DC-1",
	}
	testutil.MustNoErr(t, db.CreateOrder(child1), "create child1")

	child2 := &orders.Order{
		EdgeUUID:      "uuid-fail-parent-step-2",
		StationID:     "line-1",
		OrderType:     OrderTypeMove,
		Status:        StatusFailed,
		ParentOrderID: &parentOrder.ID,
		Sequence:      2,
		SourceNode:    slots[1].Name,
		DeliveryNode:  "LINE1-DEST",
	}
	testutil.MustNoErr(t, db.CreateOrder(child2), "create child2")

	// Create a bin claimed by child3 to verify unclaim on cancel
	binC3 := createTestBinAtNode(t, db, bp.Code, slots[2].ID, "BIN-C3")

	child3 := &orders.Order{
		EdgeUUID:      "uuid-fail-parent-step-3",
		StationID:     "line-1",
		OrderType:     OrderTypeMove,
		Status:        StatusPending,
		ParentOrderID: &parentOrder.ID,
		Sequence:      3,
		SourceNode:    slots[2].Name,
		DeliveryNode:  slots[0].Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(child3), "create child3")

	// Claim the bin by child3
	testdb.ClaimBinForTest(t, db, binC3.ID, child3.ID)

	// Lock the lane to verify it gets released
	d, emitter := newTestDispatcher(t, db, testdb.NewFailingBackend())
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

// TestHandleChildOrderFailure_InFlightSibling verifies that HandleChildOrderFailure
// cancels ALL remaining non-terminal children — including in-flight ones (dispatched,
// in_transit, staged) — not just pending/sourcing ones. This was Bug 2: the original
// implementation only cancelled StatusPending/StatusSourcing siblings, leaving
// in-flight children as orphan robots with claimed bins.
func TestHandleChildOrderFailure_InFlightSibling(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)

	// Create parent order
	parentOrder := &orders.Order{
		EdgeUUID:  "uuid-inflight-parent",
		StationID: "line-1",
		OrderType: OrderTypeRetrieve,
		Status:    StatusReshuffling,
	}
	testutil.MustNoErr(t, db.CreateOrder(parentOrder), "create parent order")

	// Child 1: already confirmed (done)
	child1 := &orders.Order{
		EdgeUUID:      "uuid-inflight-step-1",
		StationID:     "line-1",
		OrderType:     OrderTypeMove,
		Status:        StatusConfirmed,
		ParentOrderID: &parentOrder.ID,
		Sequence:      1,
		SourceNode:    slots[0].Name,
		DeliveryNode:  "GRP-TEST-DC-1",
	}
	testutil.MustNoErr(t, db.CreateOrder(child1), "create child1")

	// Child 2: the one that fails
	child2 := &orders.Order{
		EdgeUUID:      "uuid-inflight-step-2",
		StationID:     "line-1",
		OrderType:     OrderTypeMove,
		Status:        StatusFailed,
		ParentOrderID: &parentOrder.ID,
		Sequence:      2,
		SourceNode:    slots[1].Name,
		DeliveryNode:  "LINE1-DEST",
	}
	testutil.MustNoErr(t, db.CreateOrder(child2), "create child2")

	// Child 3: IN-FLIGHT (in_transit) — the key test case.
	// Old code would skip this, leaving orphan robot and claimed bin.
	binC3 := createTestBinAtNode(t, db, bp.Code, slots[2].ID, "BIN-C3-INFLIGHT")
	child3 := &orders.Order{
		EdgeUUID:      "uuid-inflight-step-3",
		StationID:     "line-1",
		OrderType:     OrderTypeMove,
		Status:        StatusInTransit,
		VendorOrderID: "vendor-inflight-step-3",
		ParentOrderID: &parentOrder.ID,
		Sequence:      3,
		SourceNode:    slots[2].Name,
		DeliveryNode:  slots[0].Name,
		BinID:         &binC3.ID,
	}
	testutil.MustNoErr(t, db.CreateOrder(child3), "create child3")
	testdb.ClaimBinForTest(t, db, binC3.ID, child3.ID)

	// Child 4: still pending — should also be cancelled
	binC4 := createTestBinAtNode(t, db, bp.Code, slots[0].ID, "BIN-C4-PENDING")
	child4 := &orders.Order{
		EdgeUUID:      "uuid-inflight-step-4",
		StationID:     "line-1",
		OrderType:     OrderTypeMove,
		Status:        StatusPending,
		ParentOrderID: &parentOrder.ID,
		Sequence:      4,
		SourceNode:    slots[0].Name,
		DeliveryNode:  slots[1].Name,
		BinID:         &binC4.ID,
	}
	testutil.MustNoErr(t, db.CreateOrder(child4), "create child4")
	testdb.ClaimBinForTest(t, db, binC4.ID, child4.ID)

	// Lock the lane
	d, _ := newTestDispatcher(t, db, testdb.NewFailingBackend())
	d.laneLock.TryLock(lane.ID, parentOrder.ID)

	// Handle child 2 failure
	d.HandleChildOrderFailure(parentOrder.ID, child2.ID)

	// VERIFY: child 3 (in_transit) MUST be cancelled, not left as orphan
	child3Got, err := db.GetOrder(child3.ID)
	if err != nil {
		t.Fatalf("get child3: %v", err)
	}
	if child3Got.Status != StatusCancelled {
		t.Errorf("BUG: child3 (in_transit) status = %q, want cancelled — in-flight sibling left as orphan robot", child3Got.Status)
	}

	// VERIFY: child 4 (pending) also cancelled
	child4Got, err := db.GetOrder(child4.ID)
	if err != nil {
		t.Fatalf("get child4: %v", err)
	}
	if child4Got.Status != StatusCancelled {
		t.Errorf("child4 (pending) status = %q, want cancelled", child4Got.Status)
	}

	// VERIFY: child 1 (confirmed) untouched
	child1Got, _ := db.GetOrder(child1.ID)
	if child1Got.Status != StatusConfirmed {
		t.Errorf("child1 (confirmed) status = %q, want confirmed (terminal — should not be touched)", child1Got.Status)
	}

	// VERIFY: bins unclaimed
	for _, bc := range []struct {
		name string
		id   int64
	}{
		{"binC3", binC3.ID},
		{"binC4", binC4.ID},
	} {
		bin, err := db.GetBin(bc.id)
		if err != nil {
			t.Fatalf("get %s: %v", bc.name, err)
		}
		if bin.ClaimedBy != nil {
			t.Errorf("BUG: %s still claimed by %d after sibling failure — bin permanently stuck", bc.name, *bin.ClaimedBy)
		}
	}

	// VERIFY: parent failed
	parentGot, _ := db.GetOrder(parentOrder.ID)
	if parentGot.Status != StatusFailed {
		t.Errorf("parent status = %q, want failed", parentGot.Status)
	}

	// VERIFY: lane lock released
	if d.laneLock.IsLocked(lane.ID) {
		t.Error("lane lock is still held after compound failure — prevents retry")
	}
}

// TestAdvanceCompoundOrder_FailedParentEmitsOrderFailed regression-tests the
// fix for the bug where AdvanceCompoundOrder's hasFailed branch (compound.go)
// emitted EmitOrderCompleted for a parent order whose status was StatusFailed.
//
// The wrong event type previously routed failed compound parents through the
// completion handler instead of the failure handler — no auto-return logic
// fired, no edge notification, and the audit trail showed "completed" for a
// failed order.
//
// Scenario: create a parent + one failed child + one terminal child (no
// pending children). Call AdvanceCompoundOrder. Assert the emitter received
// exactly one EmitOrderFailed for the parent and ZERO EmitOrderCompleted.
func TestAdvanceCompoundOrder_FailedParentEmitsOrderFailed(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)

	d, emitter := newTestDispatcher(t, db, testdb.NewFailingBackend())

	parent := &orders.Order{
		EdgeUUID:     "parent-fail-event",
		StationID:    "line-1",
		OrderType:    OrderTypeRetrieve,
		Status:       StatusReshuffling,
		PayloadCode:  bp.Code,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	}
	testutil.MustNoErr(t, db.CreateOrder(parent), "create parent")

	failedChild := &orders.Order{
		EdgeUUID:      "child-fail-event",
		StationID:     parent.StationID,
		OrderType:     OrderTypeMove,
		Status:        StatusFailed,
		ParentOrderID: &parent.ID,
		Sequence:      1,
		SourceNode:    lineNode.Name,
		DeliveryNode:  lineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(failedChild), "create failed child")

	// Reset emitter to ignore receipt events from order creation
	emitter.failed = nil
	emitter.completed = nil

	d.AdvanceCompoundOrder(parent.ID)

	// Assert: parent failure was emitted
	if len(emitter.failed) == 0 {
		t.Fatal("expected EmitOrderFailed for parent with failed children, got none")
	}
	foundParentFailed := false
	for _, f := range emitter.failed {
		if f.orderID == parent.ID {
			foundParentFailed = true
			if f.errorCode != "reshuffle_failed" {
				t.Errorf("parent failure errorCode = %q, want %q", f.errorCode, "reshuffle_failed")
			}
			break
		}
	}
	if !foundParentFailed {
		t.Errorf("EmitOrderFailed did not fire for parent %d (got: %+v)", parent.ID, emitter.failed)
	}

	// Assert: parent completion was NOT emitted (the bug we're regression-guarding)
	for _, c := range emitter.completed {
		if c.orderID == parent.ID {
			t.Errorf("BUG REGRESSION: parent %d emitted EmitOrderCompleted for a failed compound — "+
				"the hasFailed branch in AdvanceCompoundOrder must emit EmitOrderFailed instead",
				parent.ID)
		}
	}

	// Assert: parent DB status reflects failure (sanity check)
	got, _ := db.GetOrder(parent.ID)
	if got.Status != StatusFailed {
		t.Errorf("parent status = %q, want %q", got.Status, StatusFailed)
	}
}

func TestPlanReshuffleUnburyOnly_NoRetrieveNoRestock(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	grp, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)

	// Two blockers + target.
	createTestBinAtNode(t, db, bp.Code, slots[0].ID, "BIN-UO-B1")
	createTestBinAtNode(t, db, bp.Code, slots[1].ID, "BIN-UO-B2")
	target := createTestBinAtNode(t, db, bp.Code, slots[2].ID, "BIN-UO-TGT")

	plan, err := PlanReshuffleUnburyOnly(db, target, slots[2], lane, grp.ID)
	if err != nil {
		t.Fatalf("PlanReshuffleUnburyOnly: %v", err)
	}
	if len(plan.Steps) != 2 {
		t.Fatalf("steps = %d, want 2 (unbury only, no retrieve, no restock)", len(plan.Steps))
	}
	for i, s := range plan.Steps {
		if s.StepType != "unbury" {
			t.Errorf("step %d type = %q, want %q", i+1, s.StepType, "unbury")
		}
	}
}

// TestCreateCompoundOrder_StillCallsBeginReshuffle: belt-and-suspenders
// that the existing CreateCompoundOrder path still fires the
// transition for parents in Pending/Sourcing/Queued.
func TestCreateCompoundOrder_StillCallsBeginReshuffle(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	grp, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)

	parent := &orders.Order{
		EdgeUUID:     "uuid-co-still",
		StationID:    "line-1",
		OrderType:    OrderTypeRetrieve,
		Status:       StatusSourcing, // BeginReshuffle accepts Sourcing→Reshuffling
		DeliveryNode: "LINE1-DEST-STILL",
	}
	destNode := &nodes.Node{Name: "LINE1-DEST-STILL", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(destNode), "create dest")
	testutil.MustNoErr(t, db.CreateOrder(parent), "create parent")
	testutil.MustNoErr(t, db.UpdateOrderStatus(parent.ID, string(StatusSourcing), "fixture"), "set Sourcing")

	createTestBinAtNode(t, db, bp.Code, slots[0].ID, "BIN-CO-S-BLK")
	target := createTestBinAtNode(t, db, bp.Code, slots[1].ID, "BIN-CO-S-TGT")
	plan, _ := PlanReshuffle(db, target, slots[1], lane, grp.ID)

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	testutil.MustNoErr(t, d.CreateCompoundOrder(parent, plan), "CreateCompoundOrder")

	got, _ := db.GetOrder(parent.ID)
	if got.Status != StatusReshuffling {
		t.Errorf("parent status = %q, want %q (BeginReshuffle should have fired Sourcing→Reshuffling)", got.Status, StatusReshuffling)
	}
}

// ────────────────────────────────────────────────────────────────────────
// §12.2 Surface 10: restore-blockers listener (toggle ON).
// ────────────────────────────────────────────────────────────────────────

// TestCreateCompoundOrder_RetrieveInheritsParentDeliveryNode is the
// belt-and-suspenders companion to the test above. Simple-retrieve
// reshuffles still need the inherit-from-parent fallback for the
// retrieve step (the parent retrieve has a real lineside destination).
// This test pins the fallback so removing it would fail loudly.
func TestCreateCompoundOrder_RetrieveInheritsParentDeliveryNode(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	grp, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)

	createTestBinAtNode(t, db, bp.Code, slots[0].ID, "BIN-INH-B")
	target := createTestBinAtNode(t, db, bp.Code, slots[1].ID, "BIN-INH-TGT")

	destNode := &nodes.Node{Name: "LINE1-DEST-INH", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(destNode), "create dest")

	parentOrder := &orders.Order{
		EdgeUUID:     "uuid-inherit",
		StationID:    "line-1",
		OrderType:    OrderTypeRetrieve,
		Status:       StatusSourcing,
		DeliveryNode: "LINE1-DEST-INH",
	}
	testutil.MustNoErr(t, db.CreateOrder(parentOrder), "create parent")

	plan, err := PlanReshuffle(db, target, slots[1], lane, grp.ID)
	if err != nil {
		t.Fatalf("PlanReshuffle: %v", err)
	}
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	testutil.MustNoErr(t, d.CreateCompoundOrder(parentOrder, plan), "CreateCompoundOrder")

	children, _ := db.ListChildOrders(parentOrder.ID)
	var retrieveChild *orders.Order
	for _, c := range children {
		if c.PayloadDesc == fmt.Sprintf("reshuffle retrieve: bin %d", target.ID) {
			retrieveChild = c
			break
		}
	}
	if retrieveChild == nil {
		t.Fatal("retrieve child not found")
	}
	if retrieveChild.DeliveryNode != "LINE1-DEST-INH" {
		t.Errorf("retrieve DeliveryNode = %q, want %q (inherited from parent)",
			retrieveChild.DeliveryNode, "LINE1-DEST-INH")
	}
}

// ────────────────────────────────────────────────────────────────────────
// §12.2 Surface 11: Lane-lock extension for expose mode (v7 Step 4.5).
// ────────────────────────────────────────────────────────────────────────

// seedPendingLaneExtension writes the row that the intake handler
// would have written, so tests that bypass the intake path and call
// CreateCompoundOrder directly still exercise the at-terminal lookup
// in extendLaneLockForExposeMode.
func seedPendingLaneExtension(t *testing.T, d *Dispatcher, complexParentID, laneID, targetBinID, expectedFromNode int64) {
	t.Helper()
	if _, err := d.db.InsertPendingLaneExtension(&store.PendingLaneExtension{
		ComplexParentID:    complexParentID,
		LaneID:             laneID,
		TargetBinID:        targetBinID,
		ExpectedFromNodeID: expectedFromNode,
	}); err != nil {
		t.Fatalf("seed pending_lane_extension: %v", err)
	}
}

// driveCompoundChildrenToConfirmed walks a compound's children and
// drives each one through Sourcing → Dispatched → InTransit →
// Delivered → Confirmed using lifecycle helpers + direct DB writes.
// The harness mirrors what the fleet poller would do in production
// for a successful run; tests that only care about the compound's
// terminal effect (lane lock state, restore listeners firing) can
// use this to short-circuit the full pipeline.
func driveCompoundChildrenToConfirmed(t *testing.T, d *Dispatcher, parentID int64) {
	t.Helper()
	children, err := d.db.ListChildOrders(parentID)
	if err != nil {
		t.Fatalf("ListChildOrders: %v", err)
	}
	for _, child := range children {
		// Direct DB writes: bypass the lifecycle's transition table
		// (the harness needs to fast-forward through several legal
		// states). Status defaults to Pending; jump to Confirmed.
		testdb.SeedOrderStatus(t, d.db, child.ID, string(StatusConfirmed), "test harness")
	}
	if err := d.AdvanceCompoundOrder(parentID); err != nil {
		t.Fatalf("AdvanceCompoundOrder: %v", err)
	}
}

// TestLaneLock_HeldThroughComplexParentPickup_ExposeMode locks in the
// core v7 contract: when an expose-mode compound completes, the lane
// lock is NOT released — it stays held by the complex parent through
// to the bin-transit event for the target bin.
func TestLaneLock_HeldThroughComplexParentPickup_ExposeMode(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	grp, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)

	complexParent := &orders.Order{
		EdgeUUID:  "uuid-ll-expose",
		StationID: "line-1",
		OrderType: OrderTypeComplex,
		Status:    StatusQueued,
		// A production complex reshuffle parent is stamped coordinated at intake; the
		// compound terminal routing (IsCoordinated) reads the Coordinated column.
		Coordinated: true,
		StepsJSON:   `[{"action":"pickup","node":"SRC"},{"action":"dropoff","node":"DST"}]`,
	}
	testutil.MustNoErr(t, db.CreateOrder(complexParent), "create complex parent")

	createTestBinAtNode(t, db, bp.Code, slots[0].ID, "BIN-LL-EX-BLK")
	target := createTestBinAtNode(t, db, bp.Code, slots[1].ID, "BIN-LL-EX-TGT")

	plan, err := PlanReshuffleUnburyOnly(db, target, slots[1], lane, grp.ID)
	if err != nil {
		t.Fatalf("PlanReshuffleUnburyOnly: %v", err)
	}

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	// Mirror planBuriedReshuffleAtIntake's lock acquisition + persist.
	if !d.laneLock.TryLock(lane.ID, complexParent.ID) {
		t.Fatal("seed TryLock failed")
	}
	testutil.MustNoErr(t, d.CreateCompoundOrder(complexParent, plan), "CreateCompoundOrder")
	seedPendingLaneExtension(t, d, complexParent.ID, lane.ID, target.ID, slots[1].ID)

	// Drive children to terminal. After AdvanceCompoundOrder finishes,
	// the parent should be Reshuffling → Queued (ResumeCompound) and
	// the lane lock should STILL be held — that's the v7 invariant.
	driveCompoundChildrenToConfirmed(t, d, complexParent.ID)

	if !d.laneLock.IsLocked(lane.ID) {
		t.Fatal("lane lock released after compound terminal — expose-mode extension failed")
	}
	if held := d.laneLock.LockedBy(lane.ID); held != complexParent.ID {
		t.Errorf("lane locked by %d, want %d (complex parent)", held, complexParent.ID)
	}

	// Fire EventBinEnteredTransit for the target bin leaving slots[1].
	d.HandleBinTransitForLaneLock(target.ID, slots[1].ID)

	if d.laneLock.IsLocked(lane.ID) {
		t.Error("lane lock still held after bin-transit event — listener didn't fire")
	}
}

func TestLaneLock_ExposeMode_ReleasedOnParentCancel(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	grp, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)

	complexParent := &orders.Order{
		EdgeUUID:  "uuid-ll-cancel",
		StationID: "line-1",
		OrderType: OrderTypeComplex,
		Status:    StatusQueued,
		// A production complex reshuffle parent is stamped coordinated at intake; the
		// compound terminal routing (IsCoordinated) reads the Coordinated column.
		Coordinated: true,
		StepsJSON:   `[{"action":"pickup","node":"SRC"},{"action":"dropoff","node":"DST"}]`,
	}
	testutil.MustNoErr(t, db.CreateOrder(complexParent), "create complex parent")

	createTestBinAtNode(t, db, bp.Code, slots[0].ID, "BIN-LL-C-BLK")
	target := createTestBinAtNode(t, db, bp.Code, slots[1].ID, "BIN-LL-C-TGT")
	plan, _ := PlanReshuffleUnburyOnly(db, target, slots[1], lane, grp.ID)

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	if !d.laneLock.TryLock(lane.ID, complexParent.ID) {
		t.Fatal("seed TryLock failed")
	}
	testutil.MustNoErr(t, d.CreateCompoundOrder(complexParent, plan), "CreateCompoundOrder")
	seedPendingLaneExtension(t, d, complexParent.ID, lane.ID, target.ID, slots[1].ID)
	driveCompoundChildrenToConfirmed(t, d, complexParent.ID)

	if !d.laneLock.IsLocked(lane.ID) {
		t.Fatal("lane lock released before parent cancel — extension didn't engage")
	}
	d.HandleComplexParentTerminalForLaneLock(complexParent.ID)
	if d.laneLock.IsLocked(lane.ID) {
		t.Error("lane lock still held after parent cancel — release listener failed")
	}
}

// TestLaneLock_ExposeMode_ReleasedOnParentFail mirrors the cancel
// case via the fail path.
func TestLaneLock_ExposeMode_ReleasedOnParentFail(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	grp, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)

	complexParent := &orders.Order{
		EdgeUUID:  "uuid-ll-fail",
		StationID: "line-1",
		OrderType: OrderTypeComplex,
		Status:    StatusQueued,
		// A production complex reshuffle parent is stamped coordinated at intake; the
		// compound terminal routing (IsCoordinated) reads the Coordinated column.
		Coordinated: true,
		StepsJSON:   `[{"action":"pickup","node":"SRC"},{"action":"dropoff","node":"DST"}]`,
	}
	testutil.MustNoErr(t, db.CreateOrder(complexParent), "create complex parent")

	createTestBinAtNode(t, db, bp.Code, slots[0].ID, "BIN-LL-F-BLK")
	target := createTestBinAtNode(t, db, bp.Code, slots[1].ID, "BIN-LL-F-TGT")
	plan, _ := PlanReshuffleUnburyOnly(db, target, slots[1], lane, grp.ID)

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	if !d.laneLock.TryLock(lane.ID, complexParent.ID) {
		t.Fatal("seed TryLock failed")
	}
	testutil.MustNoErr(t, d.CreateCompoundOrder(complexParent, plan), "CreateCompoundOrder")
	seedPendingLaneExtension(t, d, complexParent.ID, lane.ID, target.ID, slots[1].ID)
	driveCompoundChildrenToConfirmed(t, d, complexParent.ID)

	if !d.laneLock.IsLocked(lane.ID) {
		t.Fatal("lane lock released before parent fail — extension didn't engage")
	}
	// HandleComplexParentTerminalForLaneLock handles BOTH cancel and
	// fail (engine wiring subscribes both events to the same handler).
	d.HandleComplexParentTerminalForLaneLock(complexParent.ID)
	if d.laneLock.IsLocked(lane.ID) {
		t.Error("lane lock still held after parent fail — release listener failed")
	}
}

// ────────────────────────────────────────────────────────────────────────
// Lane-lock extension persistence (cleanup round, post-v7).
// Mirrors the pending_restocks shape. Without persistence the target
// bin gets re-derived at fire time by walking the lane and excluding
// blockers — works today because the lane lock guarantees no
// unrelated bins land during the window, but the contextual invariant
// is fragile. Persistence locks the target bin at scheduling time so
// a future lane-lock refactor can't silently break the derivation.
// ────────────────────────────────────────────────────────────────────────

// TestLaneLockExtension_TargetBinPersistedAtScheduling exercises the
// production write path: planBuriedReshuffleAtIntake should persist
// the lane-extension row with the buried bin's ID and slot, so the
// at-terminal lookup doesn't have to re-derive from lane state.
//
// The parent is seeded directly rather than driven through the front door.
// Everything asserted here happens after the parent exists, and complex intake
// is now the one place that creates it — so building it here is the same row
// the handler would have written, and the test stays about the lane-extension
// row rather than about intake.
func TestLaneLockExtension_TargetBinPersistedAtScheduling(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	grp, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)

	createTestBinAtNode(t, db, bp.Code, slots[0].ID, "BIN-LLP-BLK")
	target := createTestBinAtNode(t, db, bp.Code, slots[1].ID, "BIN-LLP-TGT")

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	buried := &BuriedError{Bin: target, Slot: slots[1], LaneID: lane.ID}

	order := &orders.Order{
		EdgeUUID:    "uuid-llp-schedule",
		StationID:   "test-station",
		OrderType:   OrderTypeComplex,
		Status:      StatusQueued,
		Quantity:    1,
		PayloadCode: bp.Code,
		Coordinated: true,
	}
	if err := db.CreateOrder(order); err != nil {
		t.Fatalf("seed complex parent: %v", err)
	}
	d.planBuriedReshuffleAtIntake(order, bp.Code, "test-station", buried)

	order, err := db.GetOrderByUUID("uuid-llp-schedule")
	if err != nil {
		t.Fatalf("GetOrderByUUID: %v", err)
	}
	row, err := db.GetPendingLaneExtensionByComplexParent(order.ID)
	if err != nil {
		t.Fatalf("GetPendingLaneExtensionByComplexParent: %v (row should exist after intake)", err)
	}
	if row.LaneID != lane.ID {
		t.Errorf("LaneID = %d, want %d", row.LaneID, lane.ID)
	}
	if row.TargetBinID != target.ID {
		t.Errorf("TargetBinID = %d, want %d", row.TargetBinID, target.ID)
	}
	if row.ExpectedFromNodeID != slots[1].ID {
		t.Errorf("ExpectedFromNodeID = %d, want %d", row.ExpectedFromNodeID, slots[1].ID)
	}
	// Sanity: a plan that carries the bin away shouldn't write a row, but we can't
	// easily exercise the negative case without a configured group —
	// covered by the existing TestLaneLock_ReleasedOnCompoundComplete_TargetNodeMode.
	_ = grp
}

// TestLaneLockExtension_TargetBinRecoveredOnCoreBoot: a listener whose complex
// parent is still live SURVIVES boot, with the persisted target bin and
// from-node intact.
//
// The assertion moved with the design. It used to read the re-registered
// in-memory entry; there is no in-memory entry now, because the row is the
// listener and surviving a restart is what a row does. So it reads the row —
// which is also what the handler reads, so the test and production are looking
// at the same thing rather than at a mirror of it.
//
// What it still pins is the original point: recovery must not RE-DERIVE the
// target bin. The row's bin need not exist in the lane, and that is deliberate.
func TestLaneLockExtension_TargetBinRecoveredOnCoreBoot(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	complexParent := &orders.Order{
		EdgeUUID:  "uuid-llp-recover",
		StationID: "line-1",
		OrderType: OrderTypeComplex,
		Status:    StatusReshuffling, // non-terminal
	}
	testutil.MustNoErr(t, db.CreateOrder(complexParent), "create complex parent")
	testutil.MustNoErr(t, db.UpdateOrderStatus(complexParent.ID, string(StatusReshuffling), "fixture"), "set Reshuffling")

	const (
		laneID           int64 = 9001
		targetBinID      int64 = 9002
		expectedFromNode int64 = 9003
	)
	_, err := db.InsertPendingLaneExtension(&store.PendingLaneExtension{
		ComplexParentID:    complexParent.ID,
		LaneID:             laneID,
		TargetBinID:        targetBinID,
		ExpectedFromNodeID: expectedFromNode,
	})
	if err != nil {
		t.Fatalf("InsertPendingLaneExtension: %v", err)
	}

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	if err := d.RecoverPendingLaneExtensions(); err != nil {
		t.Fatalf("RecoverPendingLaneExtensions: %v", err)
	}

	got, err := db.GetPendingLaneExtensionByComplexParent(complexParent.ID)
	if err != nil {
		t.Fatalf("listener row for a LIVE parent did not survive boot: %v", err)
	}
	if got.TargetBinID != targetBinID {
		t.Errorf("recovered targetBinID = %d, want %d (recovery must not re-derive the target bin)",
			got.TargetBinID, targetBinID)
	}
	if got.LaneID != laneID {
		t.Errorf("recovered laneID = %d, want %d", got.LaneID, laneID)
	}
	if got.ExpectedFromNodeID != expectedFromNode {
		t.Errorf("recovered expectedFromNode = %d, want %d", got.ExpectedFromNodeID, expectedFromNode)
	}
}

// TestLaneLockExtension_RowDeletedOnTerminal: parent cancel/fail
// deletes the persisted row. Exercised via the same handler the engine
// wiring calls on EventOrderCancelled / EventOrderFailed.
func TestLaneLockExtension_RowDeletedOnTerminal(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	grp, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)

	complexParent := &orders.Order{
		EdgeUUID:  "uuid-llp-terminal",
		StationID: "line-1",
		OrderType: OrderTypeComplex,
		Status:    StatusQueued,
		// A production complex reshuffle parent is stamped coordinated at intake; the
		// compound terminal routing (IsCoordinated) reads the Coordinated column.
		Coordinated: true,
		StepsJSON:   `[{"action":"pickup","node":"SRC"},{"action":"dropoff","node":"DST"}]`,
	}
	testutil.MustNoErr(t, db.CreateOrder(complexParent), "create complex parent")

	createTestBinAtNode(t, db, bp.Code, slots[0].ID, "BIN-LLT-BLK")
	target := createTestBinAtNode(t, db, bp.Code, slots[1].ID, "BIN-LLT-TGT")
	plan, _ := PlanReshuffleUnburyOnly(db, target, slots[1], lane, grp.ID)

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	if !d.laneLock.TryLock(lane.ID, complexParent.ID) {
		t.Fatal("seed TryLock failed")
	}
	testutil.MustNoErr(t, d.CreateCompoundOrder(complexParent, plan), "CreateCompoundOrder")
	seedPendingLaneExtension(t, d, complexParent.ID, lane.ID, target.ID, slots[1].ID)
	driveCompoundChildrenToConfirmed(t, d, complexParent.ID)

	if _, err := db.GetPendingLaneExtensionByComplexParent(complexParent.ID); err != nil {
		t.Fatalf("row missing before terminal: %v", err)
	}

	d.HandleComplexParentTerminalForLaneLock(complexParent.ID)

	if _, err := db.GetPendingLaneExtensionByComplexParent(complexParent.ID); err == nil {
		t.Error("pending_lane_extensions row still present after parent terminal")
	}
}

// TestLaneLockExtension_RowDeletedOnAnyTerminalPath verifies the
// cleanup contract across all four terminal statuses a complex
// parent can reach after the row is written. Cancelled and Failed
// are the explicit cleanup paths; Skipped is the gap where a
// complex parent reaches a no-pickup terminal (ApplyComplexPlan
// finds no bins) and the bin-transit listener never fires;
// Completed is the defensive path for force-confirm /
// admin-recovery scenarios.
//
// HandleComplexParentTerminalForLaneLock is the single handler all
// four event subscribers route to — testing it directly exercises
// the cleanup behavior independent of the event-dispatch shape.
func TestLaneLockExtension_RowDeletedOnAnyTerminalPath(t *testing.T) {
	t.Parallel()
	terminals := []struct {
		name      string
		uuidSuf   string
		setStatus protocol.Status
	}{
		{"cancelled", "C", StatusCancelled},
		{"failed", "F", StatusFailed},
		{"skipped", "S", StatusSkipped},
		{"completed", "K", StatusConfirmed},
	}
	for _, tc := range terminals {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testDB(t)
			grp, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)

			complexParent := &orders.Order{
				EdgeUUID:  "uuid-llp-any-" + tc.uuidSuf,
				StationID: "line-1",
				OrderType: OrderTypeComplex,
				Status:    StatusQueued,
			}
			testutil.MustNoErr(t, db.CreateOrder(complexParent), "create complex parent")

			createTestBinAtNode(t, db, bp.Code, slots[0].ID, "BIN-LLPA-BLK-"+tc.uuidSuf)
			target := createTestBinAtNode(t, db, bp.Code, slots[1].ID, "BIN-LLPA-TGT-"+tc.uuidSuf)
			plan, _ := PlanReshuffleUnburyOnly(db, target, slots[1], lane, grp.ID)

			d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
			if !d.laneLock.TryLock(lane.ID, complexParent.ID) {
				t.Fatal("seed TryLock failed")
			}
			testutil.MustNoErr(t, d.CreateCompoundOrder(complexParent, plan), "CreateCompoundOrder")
			seedPendingLaneExtension(t, d, complexParent.ID, lane.ID, target.ID, slots[1].ID)
			driveCompoundChildrenToConfirmed(t, d, complexParent.ID)

			if _, err := db.GetPendingLaneExtensionByComplexParent(complexParent.ID); err != nil {
				t.Fatalf("row missing before %s: %v", tc.name, err)
			}

			// Engine wiring fans cancelled/failed/skipped/completed
			// events to the same handler; exercise it directly.
			d.HandleComplexParentTerminalForLaneLock(complexParent.ID)

			if _, err := db.GetPendingLaneExtensionByComplexParent(complexParent.ID); err == nil {
				t.Errorf("pending_lane_extensions row still present after %s terminal", tc.name)
			}
			_ = grp
		})
	}
}

// TestLaneLockExtension_RowDeletedOnBinTransit: the row is also
// deleted on the happy-path release (parent picks up target bin).
func TestLaneLockExtension_RowDeletedOnBinTransit(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	grp, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)

	complexParent := &orders.Order{
		EdgeUUID:  "uuid-llp-transit",
		StationID: "line-1",
		OrderType: OrderTypeComplex,
		Status:    StatusQueued,
		// A production complex reshuffle parent is stamped coordinated at intake; the
		// compound terminal routing (IsCoordinated) reads the Coordinated column.
		Coordinated: true,
		StepsJSON:   `[{"action":"pickup","node":"SRC"},{"action":"dropoff","node":"DST"}]`,
	}
	testutil.MustNoErr(t, db.CreateOrder(complexParent), "create complex parent")

	createTestBinAtNode(t, db, bp.Code, slots[0].ID, "BIN-LLPT-BLK")
	target := createTestBinAtNode(t, db, bp.Code, slots[1].ID, "BIN-LLPT-TGT")
	plan, _ := PlanReshuffleUnburyOnly(db, target, slots[1], lane, grp.ID)

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	if !d.laneLock.TryLock(lane.ID, complexParent.ID) {
		t.Fatal("seed TryLock failed")
	}
	testutil.MustNoErr(t, d.CreateCompoundOrder(complexParent, plan), "CreateCompoundOrder")
	seedPendingLaneExtension(t, d, complexParent.ID, lane.ID, target.ID, slots[1].ID)
	driveCompoundChildrenToConfirmed(t, d, complexParent.ID)

	if _, err := db.GetPendingLaneExtensionByComplexParent(complexParent.ID); err != nil {
		t.Fatalf("row missing before bin transit: %v", err)
	}

	d.HandleBinTransitForLaneLock(target.ID, slots[1].ID)

	if _, err := db.GetPendingLaneExtensionByComplexParent(complexParent.ID); err == nil {
		t.Error("pending_lane_extensions row still present after bin transit release")
	}
}
