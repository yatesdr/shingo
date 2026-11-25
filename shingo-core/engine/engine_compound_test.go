//go:build docker

package engine

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	dplan "shingocore/dispatch/plan"
	"shingocore/domain"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// Compound reshuffle order tests (TC-40a, TC-44, TC-45, TC-46, TC-51, TC-52, TC-53, TC-54).
// Each test exercises the NGRP lane reshuffle pipeline: buried bin detection,
// PlanReshuffle, AdvanceCompoundOrder, lane locks, and child lifecycle management.

// driveLegRunToFinish drives one leg the way the FLEET drives it, including the
// state a dwelling robot actually reports.
//
// ── WHY "WAITING" IS NOT AN EMBELLISHMENT ─────────────────────────────────
//
// A dig leg ships UNSEALED under the outbound dwell: [pickup, wait] and no
// destination. The robot picks, comes to the shallowest slot of the lane it is
// digging, and parks on the Wait block — which RDS reports as WAITING, which
// MapState turns into `staged`, which is what makes Core choose a destination and
// append the tail. A fixture that jumps RUNNING → FINISHED skips the only moment
// at which the leg acquires somewhere to go, so the bin lands nowhere and the
// compound stalls with a leg that "finished" without moving anything.
//
// It is asked of the ORDER rather than assumed of the leg: a retrieve tail is
// sealed and never parks, and driving a wait state for it would be a fixture
// telling a lie about the fleet. IsGateStaged is the same predicate the evaluator
// and the floor use, so the fixture and production agree on who dwells.
//
// The pickup block finishes before the robot parks on the Wait block, as it
// does at the plant: the blocker is lifted into transit first, and a dig leg is
// released only once it has lifted.
func driveLegRunToFinish(t *testing.T, db *store.DB, sim *simulator.SimulatorBackend, childID int64, vendorID string) {
	t.Helper()
	sim.DriveState(vendorID, "RUNNING")
	if fresh, err := db.GetOrder(childID); err == nil && fresh != nil && dispatch.IsGateStaged(fresh) {
		for _, b := range sim.BlocksForOrder(vendorID) {
			if b.Action == protocol.ActionPickup {
				sim.CompleteBlock(vendorID, b.BlockID, b.Location, b.BinTask, 0, 0)
				break
			}
		}
		sim.DriveState(vendorID, "WAITING")
	}
	sim.DriveState(vendorID, "FINISHED")
}

// =============================================================================
// Buried bin reshuffle through engine pipeline
// =============================================================================

// --- Buried bin: FIFO triggers reshuffle of buried bin via compound order ---
//
// Setup: NGRP -> LANE -> 3 slots. Blocker at depth 1 (newer), target at depth 2 (older).
// FIFO detects buried target as older than any accessible bin -> BuriedError ->
// planBuriedReshuffle -> compound order with child steps:
//  1. unbury: move blocker (depth 1) -> shuffle slot
//  2. retrieve: move target (depth 2) -> line node
//
// No restock — the blocker lies in the shuffle slot where the unbury parked it.
//
// Drives each child through the fleet simulator lifecycle, verifying that the
// compound order advances correctly and the target bin arrives at the line.
func TestBuriedBin_ReshuffleViaEngine(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	sc := testdb.SetupCompound(t, db, testdb.CompoundConfig{
		Prefix:     "BURIED",
		NumSlots:   3,
		TargetSlot: 2,
		TargetAge:  2 * time.Hour,
	})
	grp, lane := sc.Grp, sc.Lane
	lineNode, bp := sc.LineNode, sc.Payload
	targetBin, blockerBin := sc.TargetBin, sc.Blockers[0]

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

	order := testdb.RequireOrder(t, db, "reshuffle-buried-1")
	t.Logf("order %d: status=%s bin=%v vendor=%s", order.ID, order.Status, testdb.OnlyCart(t, order), order.VendorOrderID)

	// Order should be in "reshuffling" status (compound parent)
	if order.Status != dispatch.StatusReshuffling {
		t.Fatalf("order status = %q, want %q", order.Status, dispatch.StatusReshuffling)
	}

	// Check compound children were created
	children, err := db.ListChildOrders(order.ID)
	if err != nil {
		t.Fatalf("list children: %v", err)
	}
	if len(children) < 2 {
		t.Fatalf("expected >= 2 children (unbury, retrieve), got %d", len(children))
	}

	t.Logf("compound: %d children", len(children))
	for _, c := range children {
		t.Logf("  child %d: seq=%d status=%s desc=%s source=%s dest=%s bin=%v vendor=%s",
			c.ID, c.Sequence, c.Status, c.PayloadDesc, c.SourceNode, c.DeliveryNode, testdb.OnlyCart(t, c), c.VendorOrderID)
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

		driveLegRunToFinish(t, db, sim, child.ID, child.VendorOrderID)

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
	order = testdb.RequireOrder(t, db, "reshuffle-buried-1")
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

	// Verify the blocker moved out of the lane (no restock — it lies where it landed)
	blockerBin, err = db.GetBin(blockerBin.ID)
	if err != nil {
		t.Fatalf("get blocker bin: %v", err)
	}
	t.Logf("blocker bin: node=%v", blockerBin.NodeID)

	// No bins stuck as claimed
	for _, b := range []*bins.Bin{targetBin, blockerBin} {
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

// TestReshuffle_ChildAutoConfirmsWithoutReceipt pins the #3 fix: a
// robot-internal compound child (ParentOrderID != nil) has no operator at
// the destination to file a receipt, so on fleet FINISHED the engine now
// auto-confirms it immediately rather than leaving it Delivered until the
// 5-minute reconciliation sweep. Without the fix, driving a child to
// FINISHED leaves it Delivered (non-terminal), which the terminal-status
// assertion below should catch. Crucially it calls NO HandleOrderReceipt — the
// existing reshuffle tests mask the bug by confirming each child manually.
func TestReshuffle_ChildAutoConfirmsWithoutReceipt(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	sc := testdb.SetupCompound(t, db, testdb.CompoundConfig{
		Prefix:     "AUTOCONF",
		NumSlots:   3,
		TargetSlot: 2,
		TargetAge:  2 * time.Hour,
	})

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "autoconf-reshuffle-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  sc.Payload.Code,
		SourceNode:   sc.Grp.Name,
		DeliveryNode: sc.LineNode.Name,
		Quantity:     1,
	})

	parent := testdb.RequireOrder(t, db, "autoconf-reshuffle-1")
	children, err := db.ListChildOrders(parent.ID)
	if err != nil {
		t.Fatalf("list children: %v", err)
	}
	if len(children) < 2 {
		t.Fatalf("expected >= 2 children (unbury + retrieve), got %d", len(children))
	}

	// Drive each child to FINISHED only — NO HandleOrderReceipt. The fix must
	// auto-confirm each child so the next one dispatches.
	for i, child := range children {
		child, err = db.GetOrder(child.ID)
		if err != nil {
			t.Fatalf("get child %d: %v", i, err)
		}
		if child.VendorOrderID == "" {
			t.Fatalf("child %d (seq %d) never dispatched — prior sibling did not auto-confirm (status=%s)",
				child.ID, child.Sequence, child.Status)
		}
		driveLegRunToFinish(t, db, sim, child.ID, child.VendorOrderID)

		child, err = db.GetOrder(child.ID)
		if err != nil {
			t.Fatalf("get child %d after FINISHED: %v", i, err)
		}
		if !protocol.IsTerminal(child.Status) {
			t.Errorf("child %d (seq %d) status=%s after FINISHED — want terminal (auto-confirmed without an operator receipt)",
				child.ID, child.Sequence, child.Status)
		}
	}

	// Target bin reached the line with no operator receipt anywhere.
	targetBin, err := db.GetBin(sc.TargetBin.ID)
	if err != nil {
		t.Fatalf("get target bin: %v", err)
	}
	if targetBin.NodeID == nil || *targetBin.NodeID != sc.LineNode.ID {
		t.Errorf("target bin at node %v, want line node %d (compound did not complete via auto-confirm)",
			targetBin.NodeID, sc.LineNode.ID)
	}
}

// --- Test: Compound children dispatch sequentially, one robot at a time ---
//
// Replays the 2026-05-27 compound #51 production scenario, now 3 children
// (2 unbury + 1 retrieve; no restock). fireCompleted fires on BOTH
// (*, Delivered) and (Delivered, Confirmed), so before the
// sibling-in-flight guard each child caused two AdvanceCompoundOrder
// calls — one when the fleet reported FINISHED, another when the edge
// confirmed receipt. Combined with the redundant advanceCompound call
// in planning_service.go, three robots dispatched into the same
// cross-aisle corridor at compound creation.
//
// Pinning behavior (post-#3): robot-internal children auto-confirm on
// Delivered, so FINISHED advances the compound by exactly one child. The
// immediate next child dispatches; the one AFTER it must stay Pending until
// its own predecessor finishes. Walked across all 3 children; if the
// double-advance regresses, two robots launch into the corridor at once.
func TestCompound_SequentialChildDispatch_NoDeliveredCascade(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	// 3 lane slots with target at the deepest → 2 blockers → 2 unbury +
	// 1 retrieve = 3 children (no restock — blockers lie in the shuffle slots).
	// NumShuffles=2 so the planner can park both blockers concurrently.
	sc := testdb.SetupCompound(t, db, testdb.CompoundConfig{
		Prefix:      "CASCADE",
		NumSlots:    3,
		NumShuffles: 2,
		TargetSlot:  3,
		TargetAge:   2 * time.Hour,
	})

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "cascade-test-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  sc.Payload.Code,
		SourceNode:   sc.Grp.Name,
		DeliveryNode: sc.LineNode.Name,
		Quantity:     1,
	})

	parent := testdb.RequireOrder(t, db, "cascade-test-1")
	if parent.Status != dispatch.StatusReshuffling {
		t.Fatalf("parent status = %q, want %q", parent.Status, dispatch.StatusReshuffling)
	}

	children, err := db.ListChildOrders(parent.ID)
	if err != nil {
		t.Fatalf("list children: %v", err)
	}
	if len(children) != 3 {
		t.Fatalf("expected 3 children (2 unbury + 1 retrieve; no restock), got %d", len(children))
	}

	// Only the first child should be dispatched at compound creation;
	// every later child must still be Pending with no VendorOrderID.
	for i, c := range children {
		c, err = db.GetOrder(c.ID)
		if err != nil {
			t.Fatalf("get child %d: %v", i, err)
		}
		if i == 0 {
			if c.VendorOrderID == "" {
				t.Fatalf("first child not dispatched at compound creation — status=%s", c.Status)
			}
			continue
		}
		if c.VendorOrderID != "" {
			t.Fatalf("child %d (seq %d) dispatched at compound creation — vendor=%s status=%s; should wait for predecessor confirm",
				c.ID, c.Sequence, c.VendorOrderID, c.Status)
		}
		if c.Status != dispatch.StatusPending {
			t.Fatalf("child %d (seq %d) status = %q at compound creation, want %q",
				c.ID, c.Sequence, c.Status, dispatch.StatusPending)
		}
	}

	// Walk each child through FINISHED. Since #3 a robot-internal child
	// auto-confirms on Delivered (no operator receipt), so FINISHED drives it
	// terminal and advances the compound — by EXACTLY ONE child.
	for i, child := range children {
		child, err = db.GetOrder(child.ID)
		if err != nil {
			t.Fatalf("get child %d: %v", i, err)
		}
		if child.VendorOrderID == "" {
			t.Fatalf("child %d (seq %d) not dispatched at start of iteration — status=%s", child.ID, child.Sequence, child.Status)
		}

		driveLegRunToFinish(t, db, sim, child.ID, child.VendorOrderID)

		// Auto-confirm: the child must be terminal with no receipt filed.
		child, err = db.GetOrder(child.ID)
		if err != nil {
			t.Fatalf("get child %d after FINISHED: %v", i, err)
		}
		if !protocol.IsTerminal(child.Status) {
			t.Errorf("child %d (seq %d) status=%s after FINISHED — want terminal (auto-confirmed, no receipt)",
				child.ID, child.Sequence, child.Status)
		}

		// The immediate next sibling must now be dispatched (one advance).
		if i+1 < len(children) {
			next, err := db.GetOrder(children[i+1].ID)
			if err != nil {
				t.Fatalf("get next child after sibling %d: %v", child.Sequence, err)
			}
			if next.VendorOrderID == "" {
				t.Fatalf("child %d (seq %d) not dispatched after sibling %d auto-confirmed — status=%s",
					next.ID, next.Sequence, child.Sequence, next.Status)
			}
		}

		// AT MOST ONE CHILD INSIDE THE LANE. This replaces the cascade guard,
		// which asserted that child i+2 still had an empty VendorOrderID while
		// child i+1 was in flight — i.e. that no two children were ever
		// dispatched at once. That property is deliberately retired: two legs of
		// one reshuffle overlapping is the point of the change, and
		// TestCompound_TwoChildrenInFlightAtOnce asserts the overlap positively.
		//
		// What must NOT happen is two legs inside the lane together, and that is
		// a statement about the lane, so it is read from the lane: the occupancy
		// rows, which are taken at dispatch and released when a leg places its
		// bin. The 2026-05-27 failure it inherits — several robots sent into one
		// corridor — is caught here exactly as it was before, while legitimate
		// overlap no longer trips it.
		occupants, oerr := reservations.OccupantsOf(db.DB, sc.Lane.ID)
		if oerr != nil {
			t.Fatalf("read lane occupants after sibling %d: %v", child.Sequence, oerr)
		}
		if len(occupants) > 1 {
			t.Fatalf("%d children are inside lane %s at once (orders %v) after sibling %d — a reshuffle "+
				"may overlap its legs but only one may be in the lane",
				len(occupants), sc.Lane.Name, occupants, child.Sequence)
		}
	}

	// Parent must terminate at Confirmed after all five children confirmed.
	parent = testdb.RequireOrder(t, db, "cascade-test-1")
	if parent.Status != dispatch.StatusConfirmed {
		t.Errorf("parent status = %q, want %q", parent.Status, dispatch.StatusConfirmed)
	}
}

// --- Test: Compound child failure mid-reshuffle — blocker stranding ---
//
// Scenario: A 2-step reshuffle is in progress (unbury blocker → retrieve
// target; no restock). Step 1 completes: blocker moved to shuffle slot. Step 2
// (retrieve target) fails — robot breaks down. The fleet's FAILED puts the leg
// in faulted, a grace period, and nothing has terminated yet.
//
// Key question: The blocker bin is now physically at the shuffle slot (moved by
// completed step 1). Its claim was released on completion. Is it visible to
// normal operations? Can it be retrieved? Or is it orphaned?
//
// Expected, during that grace period: the blocker bin is at the shuffle slot,
// the parent is not terminal and the lane stays locked. What follows the
// grace's expiry is not driven here.
func TestCompound_ChildFailureMidReshuffle_BlockerStranding(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	sc := testdb.SetupCompound(t, db, testdb.CompoundConfig{Prefix: "STRAND"})
	grp, lane := sc.Grp, sc.Lane
	lineNode, bp := sc.LineNode, sc.Payload
	targetBin, blockerBin := sc.TargetBin, sc.Blockers[0]
	shuffleSlot := sc.ShuffleSlots[0]

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

	order := testdb.RequireOrderStatus(t, db, "strand-reshuffle-1", dispatch.StatusReshuffling)

	children, _ := db.ListChildOrders(order.ID)
	if len(children) < 2 {
		t.Fatalf("expected >= 2 children (unbury + retrieve), got %d", len(children))
	}

	for i, c := range children {
		t.Logf("child %d: seq=%d desc=%s source=%s dest=%s", i, c.Sequence, c.PayloadDesc, c.SourceNode, c.DeliveryNode)
	}

	// Complete step 1 (unbury blocker → shuffle slot)
	child1, _ := db.GetOrder(children[0].ID)
	if child1.VendorOrderID == "" {
		t.Fatalf("child 1 not dispatched")
	}
	driveLegRunToFinish(t, db, sim, child1.ID, child1.VendorOrderID)
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

	// Verify the child faulted and the parent did not end
	// Note: FAILED vendor state now maps to faulted (non-terminal) instead of failed.
	// The parent cascade only fires on grace expiry (faulted -> failed).
	// This test now verifies the child enters faulted state.
	child2, _ = db.GetOrder(child2.ID)
	t.Logf("child2 after FAILED vendor state: status=%s", child2.Status)
	if child2.Status != dispatch.StatusFaulted {
		t.Errorf("child2 status = %q, want faulted", child2.Status)
	}
	// Parent stays non-terminal until grace expiry triggers faulted->failed cascade.
	order, _ = db.GetOrderByUUID("strand-reshuffle-1")
	t.Logf("parent after child faulted: status=%s", order.Status)
	if order.Status.IsTerminal() {
		t.Errorf("parent should not be terminal while child is in grace period, got %q", order.Status)
	}

	// Verify lane lock held
	// Lane lock is still held while child is in faulted grace period.
	// It will be released when grace expires and the child transitions to failed.
	if !eng.Dispatcher().LaneLock().IsLocked(lane.ID) {
		t.Errorf("lane lock released during grace period — should stay locked until terminal")
	}

	// Log the children as the grace period leaves them
	children, _ = db.ListChildOrders(order.ID)
	for _, c := range children {
		c, _ = db.GetOrder(c.ID)
		t.Logf("child %d (seq %d): status=%s", c.ID, c.Sequence, c.Status)
	}

	// KEY CHECK: blocker bin at shuffle slot — step 1 completed cleanly,
	// so the blocker should be parked at the shuffle slot regardless of
	// the step-2 failure. Under the new contract (failed-order-recovery,
	// commit bdd6f9b) RDS FAILED maps to faulted (non-terminal) so claims
	// on the in-flight target bin persist during grace; the blocker
	// however was claimed by step 1 which is already confirmed, so its
	// claim should have been released at step-1 completion.
	blockerBin, _ = db.GetBin(blockerBin.ID)
	t.Logf("blocker final: node=%v claimed=%v status=%s", blockerBin.NodeID, blockerBin.ClaimedBy, blockerBin.Status)
	if blockerBin.NodeID == nil || *blockerBin.NodeID != shuffleSlot.ID {
		t.Errorf("blocker bin not at shuffle slot after failure: node=%v, want %d", blockerBin.NodeID, shuffleSlot.ID)
	}

	// Target bin: claimed by the failed child during the grace period.
	// Bin claim persists through faulted state and releases on terminal
	// transition (grace expiry → failed). Operator-facing recovery
	// surface unblocks the bin via manual force-fail or explicit
	// recovery action.
	targetBin, _ = db.GetBin(targetBin.ID)
	t.Logf("target final: node=%v claimed=%v", targetBin.NodeID, targetBin.ClaimedBy)
	if targetBin.ClaimedBy == nil {
		t.Logf("target bin already unclaimed (grace already cleaned up — also acceptable)")
	}
}

// --- Test: Two-robot swap full lifecycle (3-step compound) ---
//
// Scenario: An NGRP lane has 3 bins. The target is at depth 3 (deepest),
// with 2 blockers at depth 1 and 2. FIFO detects the buried target and
// triggers a reshuffle with 3 steps:
//  1. Unbury blocker-1 (depth 1) → shuffle-1
//  2. Unbury blocker-2 (depth 2) → shuffle-2
//  3. Retrieve target (depth 3) → line node
//
// No restock — the blockers lie in the shuffle slots the unbury parked them in.
// Deepest-first shuffle-slot selection (findShuffleSlots) keeps the lane packed
// without a restock pass. FIFO is unaffected because FindSourceFIFO keys on
// loaded_at age, not slot depth, so a bin in a different slot keeps its order.
//
// This is the full two-robot swap pattern. The test verifies:
// - All 3 children created and dispatched sequentially
// - Target arrives at line, both blockers out of their lane slots
// - All claims released, lane lock freed, parent completed
func TestCompound_TwoRobotSwap_FullLifecycle(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	sc := testdb.SetupCompound(t, db, testdb.CompoundConfig{
		Prefix:      "SWAP",
		NumSlots:    3,
		NumShuffles: 2,
		TargetAge:   3 * time.Hour,
		BlockerAges: map[int]time.Duration{2: 1 * time.Hour},
	})
	grp, lane := sc.Grp, sc.Lane
	slots := sc.Slots
	lineNode, bp := sc.LineNode, sc.Payload
	targetBin := sc.TargetBin
	blocker1, blocker2 := sc.Blockers[0], sc.Blockers[1]

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

	order := testdb.RequireOrderStatus(t, db, "swap-5step-1", dispatch.StatusReshuffling)

	children, _ := db.ListChildOrders(order.ID)
	t.Logf("compound: %d children", len(children))
	for _, c := range children {
		t.Logf("  child seq=%d: desc=%s src=%s dest=%s bin=%v",
			c.Sequence, c.PayloadDesc, c.SourceNode, c.DeliveryNode, testdb.OnlyCart(t, c))
	}

	if len(children) < 3 {
		t.Fatalf("expected >= 3 children (2 unbury + 1 retrieve; no restock), got %d", len(children))
	}

	// Drive each child through full lifecycle sequentially
	for i, child := range children {
		child, _ = db.GetOrder(child.ID)
		if child.VendorOrderID == "" {
			t.Fatalf("child %d (seq %d) not dispatched — status=%s", i, child.Sequence, child.Status)
		}

		driveLegRunToFinish(t, db, sim, child.ID, child.VendorOrderID)

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

	// No restock: the blockers lie where the unbury parked them (in shuffle slots),
	// not back in the lane. Verify they moved OUT of their original lane slots
	// (slots[0], slots[1]) and stayed findable — available and unclaimed.
	blocker1, _ = db.GetBin(blocker1.ID)
	blocker2, _ = db.GetBin(blocker2.ID)
	if blocker1.NodeID != nil && *blocker1.NodeID == slots[0].ID {
		t.Errorf("blocker1 still at its original slot %d — should have been unburied", slots[0].ID)
	}
	if blocker2.NodeID != nil && *blocker2.NodeID == slots[1].ID {
		t.Errorf("blocker2 still at its original slot %d — should have been unburied", slots[1].ID)
	}
	if blocker1.Status != "available" || blocker2.Status != "available" {
		t.Errorf("blocker statuses: blocker1=%s blocker2=%s, want both available", blocker1.Status, blocker2.Status)
	}

	// All claims released
	for _, b := range []*bins.Bin{targetBin, blocker1, blocker2} {
		if b.ClaimedBy != nil {
			t.Errorf("bin %d (%s) still claimed by %d", b.ID, b.Label, *b.ClaimedBy)
		}
	}

	// Lane lock freed
	if eng.Dispatcher().LaneLock().IsLocked(lane.ID) {
		t.Errorf("lane %d still locked after compound completion", lane.ID)
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
	t.Parallel()
	db := testDB(t)

	sc := testdb.SetupCompound(t, db, testdb.CompoundConfig{Prefix: "PCANCEL"})
	grp, lane := sc.Grp, sc.Lane
	lineNode, bp := sc.LineNode, sc.Payload
	targetBin, blockerBin := sc.TargetBin, sc.Blockers[0]

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

	order := testdb.RequireOrderStatus(t, db, "pcancel-reshuffle-1", dispatch.StatusReshuffling)

	children, _ := db.ListChildOrders(order.ID)
	if len(children) < 2 {
		t.Fatalf("expected >= 2 children (unbury + retrieve), got %d", len(children))
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

		// Children with vendor orders must be cancelled (cancelCompoundChildren fix)
		if c.VendorOrderID != "" && c.Status != dispatch.StatusCancelled {
			t.Errorf("BUG: child %d has fleet order %s but status=%s (not cancelled) — orphan robot risk",
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
	for _, b := range []*bins.Bin{targetBin, blockerBin} {
		if b.ClaimedBy != nil {
			t.Errorf("BUG: bin %d (%s) still claimed by %d after parent cancel — permanently stuck",
				b.ID, b.Label, *b.ClaimedBy)
		}
	}
}

// --- Test: AdvanceCompoundOrder skips failed children — premature completion (TC-51) ---
//
// Scenario: a two-leg dig (unbury the blocker, retrieve the target). While the
// unbury is out, the retrieve leg waits its turn with no vendor order, and its
// source node is cleared, as a damaged row would have it. The unbury's landing
// advances the compound, which fails that leg ("missing source node") and
// advances past it to the chapter's end.
//
// Expected: the parent does not complete over the failed leg, and is not left
// in reshuffling either. The demand goes back to be re-planned, and the blocker
// lies at its shuffle slot, unclaimed.
//
// Scope: the failed leg is the chapter's last one. This test does not cover a
// failed leg with another still pending behind it. Nor does it pin the advance
// that follows the failure inside advanceCompound: the unbury's own completion
// re-drives the compound, so the test stays green with that advance removed.
//
// MUTATION (verified), in dispatch/compound.go:
//   - skip the stopped-short arm of advanceCompoundChapterEnd: the parent
//     confirms.
//   - delete the empty-source arm: the leg fails as a config failure instead.
func TestCompound_AdvanceSkipsFailedChild_PrematureCompletion(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	sc := testdb.SetupCompound(t, db, testdb.CompoundConfig{Prefix: "SKIP"})
	grp := sc.Grp
	lineNode, bp := sc.LineNode, sc.Payload

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

	order := testdb.RequireOrderStatus(t, db, "skip-reshuffle-1", dispatch.StatusReshuffling)

	children, err := db.ListChildOrders(order.ID)
	testutil.MustNoErr(t, err, "list the legs")
	if len(children) != 2 {
		t.Fatalf("expected 2 children (unbury + retrieve), got %d", len(children))
	}
	child1, child2 := children[0], children[1]
	if child1.VendorOrderID == "" {
		t.Fatalf("child 1 not dispatched")
	}

	// Break child 2 before child 1's landing can dispatch it.
	if child2.VendorOrderID != "" {
		t.Fatalf("child 2 went out (%s) with child 1 still in the lane", child2.VendorOrderID)
	}
	_, err = db.Exec(`UPDATE orders SET source_node = '' WHERE id = $1`, child2.ID)
	testutil.MustNoErr(t, err, "clear child 2's source node")

	// The landing confirms child 1 and advances the compound over child 2.
	driveLegRunToFinish(t, db, sim, child1.ID, child1.VendorOrderID)

	child2, err = db.GetOrder(child2.ID)
	testutil.MustNoErr(t, err, "reload child 2")
	if child2.Status != dispatch.StatusFailed || child2.ErrorDetail != "missing source node" {
		t.Fatalf("child 2 = %q (%q), want failed on its missing source node", child2.Status, child2.ErrorDetail)
	}

	// The re-planned parent may already be pending, dispatched or on its robot.
	order = testdb.RequireOrder(t, db, "skip-reshuffle-1")
	switch {
	case order.Status == dispatch.StatusConfirmed:
		t.Errorf("parent confirmed despite a failed child — data inconsistent")
	case order.Status.IsTerminal():
		t.Errorf("parent %q (%q) with one failed leg, want the demand re-planned", order.Status, order.ErrorDetail)
	case order.Status == dispatch.StatusReshuffling:
		t.Errorf("parent still reshuffling after its last leg failed, want the chapter closed")
	}

	blockerBin := testdb.RequireBin(t, db, sc.Blockers[0].ID)
	if blockerBin.NodeID == nil || *blockerBin.NodeID != sc.ShuffleSlots[0].ID || blockerBin.ClaimedBy != nil {
		t.Errorf("blocker at node=%s claimed_by=%v, want %s and unclaimed",
			blockerBin.NodeName, blockerBin.ClaimedBy, sc.ShuffleSlots[0].Name)
	}
}

// blankNodeName leaves a node with no name, as a row saved before the store
// refused a blank name (store/nodes.ErrNameBlank) would be. Raw SQL, because
// every store door now refuses one; the one place the set-up lives, so its
// callers stay as they are.
func blankNodeName(t *testing.T, db *store.DB, nodeID int64) {
	t.Helper()
	_, err := db.Exec(`UPDATE nodes SET name = '' WHERE id = $1`, nodeID)
	testutil.MustNoErr(t, err, "blank the node's name")
}

// --- Test: a dig through a nameless slot in a lane (docket B1) ---
//
// Scenario: a lane three deep with a bin on every slot, the wanted bin
// deepest, and the middle slot with no name (blankNodeName). The buried bin
// is requested and the first unbury is driven to its landing. The bound is
// three re-plans: the engine is stopped there and the rows are read.
//
// THIS PINS TODAY'S BEHAVIOUR, WHICH IS THE BUG: the order plans the same dig
// again and again and never ends. The middle unbury copies its pickup from the
// slot's name, so it is written with none; its turn fails it with a store write
// that emits no event (advanceCompound's empty-source arm), and the advance
// goes on to the retrieve behind it. Admission refuses that one, because the
// nameless slot's bin still stands in front of it, no order is coming for that
// bin, and the dig dissolves. The parent goes back to `pending`, its acquire
// runs at once and plans the same dig, with the nameless slot as its first
// blocker. Each turn writes two more legs and sends no robot, and the parent's
// own row says the plan went stale.
//
// A `queued` row in the parent's history is allowed and none was seen
// (2026-11-25): a re-plan that met the lane still held would park until the
// next pass, and the loop would go on from there.
//
// The fix (study round 5, section 2) turns this test over: the order fails,
// its sentence names the slot, and no leg is written after the first plan.
func TestCompound_DigThroughANamelessSlotInALane(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	sc := testdb.SetupCompound(t, db, testdb.CompoundConfig{Prefix: "NONAME", NumSlots: 3, NumShuffles: 2})
	nameless := sc.Slots[1]
	blankNodeName(t, db, nameless.ID)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)

	eng.Dispatcher().HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID:    "noname-dig-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  sc.Payload.Code,
		SourceNode:   sc.Grp.Name,
		DeliveryNode: sc.LineNode.Name,
		Quantity:     1,
	})
	parent := testdb.RequireOrderStatus(t, db, "noname-dig-1", dispatch.StatusReshuffling)

	legs := legsByID(t, db, parent.ID)
	if len(legs) != 3 {
		t.Fatalf("the first plan wrote %d legs, want 3 (two unburies and the retrieve)", len(legs))
	}
	if legs[0].VendorOrderID == "" || legs[1].SourceNode != "" || legs[2].SourceNode != sc.Slots[2].Name {
		t.Fatalf("the first plan: leg 1 vendor=%q, leg 2 source=%q, leg 3 source=%q; want leg 1 sent, "+
			"leg 2 with no pickup and leg 3 from %s", legs[0].VendorOrderID, legs[1].SourceNode,
			legs[2].SourceNode, sc.Slots[2].Name)
	}
	driveLegRunToFinish(t, db, sim, legs[0].ID, legs[0].VendorOrderID)

	// THE BOUND: three re-plans (two legs each), or the parent ending. The
	// limit covers one periodic pass, for a re-plan that parked.
	const replans = 3
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if len(legsByID(t, db, parent.ID)) >= 3+2*replans ||
			testdb.RequireOrder(t, db, "noname-dig-1").Status.IsTerminal() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	eng.Stop() // what is running finishes; t.Cleanup's second Stop is a no-op

	// The parent's status sequence: never an end, and a `reshuffling` a plan.
	history, err := db.ListOrderHistory(parent.ID)
	testutil.MustNoErr(t, err, "read the parent's history")
	var sequence []protocol.Status
	plans := 0
	for i, h := range history {
		sequence = append(sequence, h.Status)
		switch h.Status {
		case dispatch.StatusReshuffling:
			plans++
		case dispatch.StatusPending:
			if i > 0 && h.Detail != dispatch.ReshuffleDissolveDetail {
				t.Errorf("the parent went back to pending with %q, want the stale-plan sentence %q",
					h.Detail, dispatch.ReshuffleDissolveDetail)
			}
		case dispatch.StatusQueued:
		default:
			t.Errorf("the parent's history holds %q (%q); today it never leaves pending, queued and "+
				"reshuffling", h.Status, h.Detail)
		}
	}
	t.Logf("parent status sequence: %v", sequence)
	if plans < 1+replans {
		t.Fatalf("the parent was planned %d time(s) in the bound, want at least %d: today it re-plans "+
			"the same dig with nothing between turns (sequence %v)", plans, 1+replans, sequence)
	}

	// The child count: three legs for the first plan, two for every plan after.
	legs = legsByID(t, db, parent.ID)
	t.Logf("children: %d over %d plans", len(legs), plans)
	if want := 3 + 2*(plans-1); len(legs) != want {
		t.Fatalf("%d legs over %d plans, want %d (3, then 2 a re-plan)", len(legs), plans, want)
	}
	wantEnd := func(leg *orders.Order, status protocol.Status, detail string) {
		t.Helper()
		if leg.Status != status || leg.ErrorDetail != detail {
			t.Errorf("leg %d (sequence %d) is %q (%q), want %q (%q)", leg.ID, leg.Sequence,
				leg.Status, leg.ErrorDetail, status, detail)
		}
	}
	wantEnd(legs[0], dispatch.StatusConfirmed, "")
	wantEnd(legs[1], dispatch.StatusFailed, "missing source node")
	wantEnd(legs[2], dispatch.StatusCancelled, dispatch.ReshuffleDissolveDetail)
	for i := 3; i < len(legs); i += 2 {
		unbury, retrieve := legs[i], legs[i+1]
		if unbury.SourceNode != "" || unbury.VendorOrderID != "" || retrieve.VendorOrderID != "" {
			t.Errorf("re-plan legs %d and %d: unbury source=%q vendor=%q, retrieve vendor=%q; want the "+
				"nameless slot's unbury first and no robot sent for either", unbury.ID, retrieve.ID,
				unbury.SourceNode, unbury.VendorOrderID, retrieve.VendorOrderID)
		}
		wantEnd(unbury, dispatch.StatusFailed, "missing source node")
		wantEnd(retrieve, dispatch.StatusCancelled, dispatch.ReshuffleDissolveDetail)
	}

	// The floor: the first blocker was carried out once, and nothing else moved.
	first := testdb.RequireBin(t, db, sc.Blockers[0].ID)
	if first.NodeID == nil || (*first.NodeID != sc.ShuffleSlots[0].ID && *first.NodeID != sc.ShuffleSlots[1].ID) ||
		first.ClaimedBy != nil {
		t.Errorf("the first blocker is at %q claimed_by=%v, want a shuffle slot and unclaimed",
			first.NodeName, first.ClaimedBy)
	}
	testdb.AssertBinAtNode(t, db, sc.Blockers[1].ID, nameless.ID)
	testdb.AssertBinUnclaimed(t, db, sc.Blockers[1].ID)
	testdb.AssertBinAtNode(t, db, sc.TargetBin.ID, sc.Slots[2].ID)
	testdb.AssertBinUnclaimed(t, db, sc.TargetBin.ID)
}

// legsByID lists a compound's legs oldest first. ListChildOrders orders by
// sequence, which restarts with every plan, so the plans of a re-planned
// parent interleave there.
func legsByID(t *testing.T, db *store.DB, parentID int64) []*orders.Order {
	t.Helper()
	legs, err := db.ListChildOrders(parentID)
	testutil.MustNoErr(t, err, "list the legs")
	sort.Slice(legs, func(i, j int) bool { return legs[i].ID < legs[j].ID })
	return legs
}

// --- Test: a dig's last leg names a node that is not there (docket B1, pin 3) ---
//
// Scenario: a two-leg dig (unbury the blocker, retrieve the target). While the
// unbury is out, the line's node is renamed, so the retrieve leg names a
// delivery node that no longer resolves. The unbury is driven to its landing.
// (Not the target's slot: a lane's re-drive finds a held leg by its node's
// name, ListHeldLegParentsInLane, so a leg whose pickup was renamed is reached
// only at the unbury's landing, after the carry this pin is about.)
//
// Expected, and the rule that must hold through B1's fix: a leg that can never
// be built fails the order with the leg's own sentence, and a person can read
// which node to fix on the order itself. The blocker lies at its shuffle slot.
//
// AND THE ORDER WAITS FOR THE UNBURY (study round 5, section 2, point 3): the
// retrieve is refused once the unbury has left the lane with its bin, and the
// order is failed, and its lane let go, only once the unbury has set it down.
func TestCompound_UnknownNodeOnTheLastLeg_FailsTheOrderWithTheLegsSentence(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	sc := testdb.SetupCompound(t, db, testdb.CompoundConfig{Prefix: "RENAMED"})
	sim := simulator.New()
	eng := newTestEngine(t, db, sim)

	eng.Dispatcher().HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID:    "renamed-dig-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  sc.Payload.Code,
		SourceNode:   sc.Grp.Name,
		DeliveryNode: sc.LineNode.Name,
		Quantity:     1,
	})
	parent := testdb.RequireOrderStatus(t, db, "renamed-dig-1", dispatch.StatusReshuffling)
	legs := legsByID(t, db, parent.ID)
	if len(legs) != 2 || legs[0].VendorOrderID == "" || legs[1].VendorOrderID != "" {
		t.Fatalf("fixture: want two legs with only the first sent, got %d", len(legs))
	}

	// The retrieve leg was written with the line's old name.
	oldName := sc.LineNode.Name
	line, err := db.GetNode(sc.LineNode.ID)
	testutil.MustNoErr(t, err, "read the line's node")
	line.Name = oldName + "-RENAMED"
	testutil.MustNoErr(t, db.UpdateNode(line), "rename the line's node under the open dig")

	// The unbury lifts and is released from its dwell: it is carrying, and the
	// retrieve's turn comes and fails on the renamed slot.
	unbury := legs[0]
	sim.DriveState(unbury.VendorOrderID, "RUNNING")
	for _, b := range sim.BlocksForOrder(unbury.VendorOrderID) {
		if b.Action == protocol.ActionPickup {
			sim.CompleteBlock(unbury.VendorOrderID, b.BlockID, b.Location, b.BinTask, 0, 0)
			break
		}
	}
	sim.DriveState(unbury.VendorOrderID, "WAITING")
	testutil.AssertEventually(t, 5*time.Millisecond, 30*time.Second, func() bool {
		return legsByID(t, db, parent.ID)[1].Status == dispatch.StatusFailed
	})
	if got := testdb.RequireOrder(t, db, "renamed-dig-1"); got.Status != dispatch.StatusReshuffling {
		t.Errorf("the order is %q (%q) with the unbury still carrying its bin, want reshuffling until "+
			"it sets it down", got.Status, got.ErrorDetail)
	}
	if !eng.Dispatcher().LaneLock().IsLocked(sc.Lane.ID) {
		t.Errorf("lane %s was let go with the unbury still carrying its bin", sc.Lane.Name)
	}

	sim.DriveState(unbury.VendorOrderID, "FINISHED")
	// The order's end is the engine's to reach, on whichever goroutine closes
	// the chapter; its lane goes just after its status.
	testutil.AssertEventually(t, 5*time.Millisecond, 30*time.Second, func() bool {
		return testdb.RequireOrder(t, db, "renamed-dig-1").Status.IsTerminal() &&
			!eng.Dispatcher().LaneLock().IsLocked(sc.Lane.ID)
	})

	legs = legsByID(t, db, parent.ID)
	sentence := dplan.ConfigFailure("delivery node", oldName)
	if legs[1].Status != dispatch.StatusFailed || legs[1].ErrorDetail != sentence || legs[1].VendorOrderID != "" {
		t.Fatalf("the retrieve leg is %q (%q) vendor=%q, want failed with %q and never sent",
			legs[1].Status, legs[1].ErrorDetail, legs[1].VendorOrderID, sentence)
	}
	if legs[0].Status != dispatch.StatusConfirmed {
		t.Errorf("the unbury leg is %q (%q), want confirmed: it had its bin up and set it down",
			legs[0].Status, legs[0].ErrorDetail)
	}
	parent = testdb.RequireOrder(t, db, "renamed-dig-1")
	if parent.Status != dispatch.StatusFailed || parent.ErrorDetail != sentence {
		t.Errorf("the order is %q (%q), want failed with the leg's own sentence %q",
			parent.Status, parent.ErrorDetail, sentence)
	}
	if len(legsByID(t, db, parent.ID)) != 2 {
		t.Errorf("the order has %d legs, want the first plan's 2 and no re-plan", len(legsByID(t, db, parent.ID)))
	}
	if eng.Dispatcher().LaneLock().IsLocked(sc.Lane.ID) {
		t.Errorf("lane %s is still held by an order that has ended", sc.Lane.Name)
	}
	blocker := testdb.RequireBin(t, db, sc.Blockers[0].ID)
	if blocker.NodeID == nil || *blocker.NodeID != sc.ShuffleSlots[0].ID || blocker.ClaimedBy != nil {
		t.Errorf("the blocker is at %q claimed_by=%v, want %s and unclaimed",
			blocker.NodeName, blocker.ClaimedBy, sc.ShuffleSlots[0].Name)
	}
	testdb.AssertBinAtNode(t, db, sc.TargetBin.ID, sc.Slots[1].ID)
	testdb.AssertBinUnclaimed(t, db, sc.TargetBin.ID)
}

// --- Test: the fleet fails a leg while a sibling carries its bin (docket B1, pin 4) ---
//
// Scenario: a two-leg dig. The unbury lifts the blocker and leaves the lane
// with it, which lets the retrieve go out behind it. The fleet then fails the
// retrieve (FAILED, and the grace expiring) with the unbury still carrying.
//
// Expected, and what B1's fix must not change (gate 1): the carrying leg is
// not cancelled and sets its bin down; the order is not failed. It goes back to
// be planned, saying which leg failed, and its own acquire sends it for the
// bin, which nothing stands in front of any more.
func TestCompound_FleetFailsALegWithASiblingCarrying_TheOrderReplans(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	sc := testdb.SetupCompound(t, db, testdb.CompoundConfig{Prefix: "CARRY"})
	sim := simulator.New()
	eng := newTestEngine(t, db, sim)

	eng.Dispatcher().HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID:    "carry-dig-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  sc.Payload.Code,
		SourceNode:   sc.Grp.Name,
		DeliveryNode: sc.LineNode.Name,
		Quantity:     1,
	})
	parent := testdb.RequireOrderStatus(t, db, "carry-dig-1", dispatch.StatusReshuffling)
	legs := legsByID(t, db, parent.ID)
	if len(legs) != 2 || legs[0].VendorOrderID == "" {
		t.Fatalf("fixture: want two legs with the first sent, got %d", len(legs))
	}
	unbury := legs[0]

	// The unbury lifts and is released from its dwell: it is carrying.
	sim.DriveState(unbury.VendorOrderID, "RUNNING")
	for _, b := range sim.BlocksForOrder(unbury.VendorOrderID) {
		if b.Action == protocol.ActionPickup {
			sim.CompleteBlock(unbury.VendorOrderID, b.BlockID, b.Location, b.BinTask, 0, 0)
			break
		}
	}
	sim.DriveState(unbury.VendorOrderID, "WAITING")
	if at := binNodeName(t, db, sc.Blockers[0].ID); at != domain.TransitNodeName {
		t.Fatalf("fixture: the blocker is at %q, want it on the unbury's robot", at)
	}

	// The retrieve went out behind it; the fleet fails it.
	retrieve := legsByID(t, db, parent.ID)[1]
	if retrieve.VendorOrderID == "" {
		t.Fatalf("fixture: the retrieve leg was not sent behind the carrying unbury (%q, %q)",
			retrieve.Status, retrieve.QueueCause)
	}
	sim.DriveState(retrieve.VendorOrderID, "RUNNING")
	sim.DriveState(retrieve.VendorOrderID, "FAILED")
	eng.handleGraceExpired(GraceExpiredEvent{OrderID: retrieve.ID, VendorOrderID: retrieve.VendorOrderID})

	legs = legsByID(t, db, parent.ID)
	if legs[1].Status != dispatch.StatusFailed {
		t.Fatalf("the retrieve leg is %q, want failed by the grace expiry", legs[1].Status)
	}
	if legs[0].Status.IsTerminal() {
		t.Fatalf("the carrying leg is %q (%q): a robot holds its bin, and no door cancels it",
			legs[0].Status, legs[0].ErrorDetail)
	}
	if got := testdb.RequireOrder(t, db, "carry-dig-1"); got.Status != dispatch.StatusReshuffling {
		t.Fatalf("the order is %q with a leg still carrying, want reshuffling until it lands", got.Status)
	}

	// The carrier lands; the chapter closes and the order is planned again, on
	// whichever goroutine reaches the chapter's end first.
	sim.DriveState(unbury.VendorOrderID, "FINISHED")
	testutil.AssertEventually(t, 5*time.Millisecond, 30*time.Second, func() bool {
		o := testdb.RequireOrder(t, db, "carry-dig-1")
		return o.VendorOrderID != "" || o.Status.IsTerminal()
	})

	legs = legsByID(t, db, parent.ID)
	if legs[0].Status != dispatch.StatusConfirmed {
		t.Errorf("the carrying leg ended %q (%q), want confirmed", legs[0].Status, legs[0].ErrorDetail)
	}
	blocker := testdb.RequireBin(t, db, sc.Blockers[0].ID)
	if blocker.NodeID == nil || *blocker.NodeID != sc.ShuffleSlots[0].ID {
		t.Errorf("the blocker is at %q, want %s", blocker.NodeName, sc.ShuffleSlots[0].Name)
	}
	history, err := db.ListOrderHistory(parent.ID)
	testutil.MustNoErr(t, err, "read the order's history")
	var sequence []protocol.Status
	restarted, sentAfter := "", false
	for i, h := range history {
		sequence = append(sequence, h.Status)
		switch {
		case i > 0 && h.Status == dispatch.StatusPending:
			restarted = h.Detail
		case restarted != "" && h.Status == dispatch.StatusDispatched:
			sentAfter = true
		}
	}
	t.Logf("order status sequence: %v; restarted with %q", sequence, restarted)
	wantRestart := fmt.Sprintf("reshuffle dissolved: dig leg %d failed", legs[1].ID)
	if !strings.HasPrefix(restarted, wantRestart) {
		t.Errorf("the order went back to pending with %q, want a sentence starting %q", restarted, wantRestart)
	}
	parent = testdb.RequireOrder(t, db, "carry-dig-1")
	if !sentAfter || parent.Status.IsTerminal() || parent.VendorOrderID == "" {
		t.Errorf("the order is %q (%q) vendor=%q after %v, want it sent for the bin the dig uncovered",
			parent.Status, parent.ErrorDetail, parent.VendorOrderID, sequence)
	}
	if len(legs) != 2 {
		t.Errorf("the order has %d legs, want 2: the lane is clear and no second dig was needed", len(legs))
	}
}

// --- Test: Lane lock contention — second reshuffle blocked (TC-52) ---
//
// Scenario: A retrieve order triggers a reshuffle on a lane. While the
// reshuffle is in progress, a second retrieve order targets the same NGRP
// lane (same or different payload). The planning service should detect the
// lane lock and return a lane_locked planningError.
//
// Expected: lane_locked queues the second order (a transient planningError)
// rather than failing it, so it can be retried when the lane unlocks.
func TestLaneLock_Contention_SecondReshuffleBlocked(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	sc := testdb.SetupCompound(t, db, testdb.CompoundConfig{
		Prefix:      "LOCK",
		NumSlots:    3,
		NumShuffles: 2,
		TargetAge:   3 * time.Hour,
		BlockerAges: map[int]time.Duration{2: 1 * time.Hour},
	})
	grp, lane := sc.Grp, sc.Lane
	lineNode, bp := sc.LineNode, sc.Payload

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

	testdb.RequireOrderStatus(t, db, "lock-reshuffle-1", dispatch.StatusReshuffling)

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

	order2 := testdb.RequireOrder(t, db, "lock-reshuffle-2")
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
