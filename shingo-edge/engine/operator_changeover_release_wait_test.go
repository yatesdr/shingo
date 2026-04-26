package engine

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingoedge/orders"
	"shingoedge/store"
	"shingoedge/store/messaging"
)

// findOutboxByType returns the most recent pending outbox messages of the
// given type. Used to assert that ReleaseChangeoverWait emitted the right
// envelope shape — the hard guarantee being that release events now carry
// remaining_uop=0, which is what causes Core to clear the bin's manifest
// before the fleet picks the bin up. Without that, we re-introduce the
// ALN_001 → SLN_002 → SMN_005 incident the rerouting fixed.
func findOutboxByType(t *testing.T, db *store.DB, msgType string) []messaging.Message {
	t.Helper()
	msgs, err := db.ListPendingOutbox(100)
	if err != nil {
		t.Fatalf("ListPendingOutbox: %v", err)
	}
	var matches []messaging.Message
	for _, m := range msgs {
		if m.MsgType == msgType {
			matches = append(matches, m)
		}
	}
	return matches
}

// decodeOrderRelease unmarshals an outbox row into an OrderRelease envelope
// and the inner payload.
func decodeOrderRelease(t *testing.T, msg messaging.Message) protocol.OrderRelease {
	t.Helper()
	var env protocol.Envelope
	if err := json.Unmarshal(msg.Payload, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if env.Type != protocol.TypeOrderRelease {
		t.Fatalf("envelope.Type = %q, want %q", env.Type, protocol.TypeOrderRelease)
	}
	var rel protocol.OrderRelease
	if err := env.DecodePayload(&rel); err != nil {
		t.Fatalf("decode OrderRelease: %v", err)
	}
	return rel
}

// TestReleaseChangeoverWait_RoutesThroughLinesideRelease locks down the bug
// fix from commit 7421c3c. Before that commit, ReleaseChangeoverWait called
// orderMgr.ReleaseOrder directly with no remaining_uop — Core never cleared
// the bin manifest, evacuation bins arrived at the supermarket still tagged
// as loaded, and the bin loader couldn't move them. The fix routes every
// staged evacuation order through ReleaseOrderWithLineside with the
// capture_lineside disposition so each release envelope carries
// remaining_uop=0.
//
// This test asserts the envelope shape is correct so a future refactor
// can't silently re-introduce the bypass.
func TestReleaseChangeoverWait_RoutesThroughLinesideRelease(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID := seedPhase3SwapScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	// Start changeover — Phase 3 creates Order A (staging) + Order B (swap
	// with embedded wait). Order B is the evacuation order whose release we
	// care about for the manifest-clear bug fix.
	changeover, err := eng.StartProcessChangeover(processID, toStyleID, "test", "release-wait reroute")
	if err != nil {
		t.Fatalf("start changeover: %v", err)
	}

	task, err := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if err != nil {
		t.Fatalf("get node task: %v", err)
	}
	if task.OldMaterialReleaseOrderID == nil {
		t.Fatal("expected Order B (OldMaterialReleaseOrderID) to be set after Phase 3 swap start")
	}
	orderB, err := db.GetOrder(*task.OldMaterialReleaseOrderID)
	if err != nil {
		t.Fatalf("get order B: %v", err)
	}

	// Force Order B to staged so ReleaseChangeoverWait will pick it up. In
	// production the fleet tracker advances the order to staged when the
	// robot reports WAITING; the dispatcher-level test has no fleet wiring,
	// so we set it directly.
	if err := db.UpdateOrderStatus(orderB.ID, orders.StatusStaged); err != nil {
		t.Fatalf("force order B staged: %v", err)
	}

	// Drain any pending outbox messages from the changeover-start phase so
	// we can assert exactly one OrderRelease lands from the wait release.
	pending, _ := db.ListPendingOutbox(100)
	for _, m := range pending {
		_ = db.AckOutbox(m.ID)
	}

	if err := eng.ReleaseChangeoverWait(processID, "test-operator"); err != nil {
		t.Fatalf("ReleaseChangeoverWait: %v", err)
	}

	// Exactly one OrderRelease envelope queued — for Order B.
	releases := findOutboxByType(t, db, protocol.TypeOrderRelease)
	if len(releases) != 1 {
		t.Fatalf("OrderRelease envelopes queued: got %d, want 1", len(releases))
	}

	rel := decodeOrderRelease(t, releases[0])
	if rel.OrderUUID != orderB.UUID {
		t.Errorf("released OrderUUID = %q, want %q (Order B)", rel.OrderUUID, orderB.UUID)
	}
	// The bug fix: remaining_uop must be present and zero. Without this Core
	// won't clear the bin's manifest before the fleet picks the bin up — the
	// exact failure mode the rerouting through ReleaseOrderWithLineside +
	// DispositionCaptureLineside was meant to close.
	if rel.RemainingUOP == nil {
		t.Fatal("OrderRelease.RemainingUOP = nil; bug fix expects &0 so Core clears the bin's manifest before fleet release")
	}
	if *rel.RemainingUOP != 0 {
		t.Errorf("OrderRelease.RemainingUOP = %d, want 0 (capture_lineside disposition)", *rel.RemainingUOP)
	}

	// Order B should now be in_transit (release dispatched).
	got, _ := db.GetOrder(orderB.ID)
	if got.Status != orders.StatusInTransit {
		t.Errorf("order B status = %q, want %q", got.Status, orders.StatusInTransit)
	}
}

// TestReleaseChangeoverWait_NoStagedOrdersIsNoOp verifies that when there
// are no staged evacuation orders (e.g. all tasks are unchanged or already
// past staged), ReleaseChangeoverWait succeeds without touching the outbox.
func TestReleaseChangeoverWait_NoStagedOrdersIsNoOp(t *testing.T) {
	db := testEngineDB(t)
	processID, _, _, toStyleID := seedPhase3SwapScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	if _, err := eng.StartProcessChangeover(processID, toStyleID, "test", "no-op release"); err != nil {
		t.Fatalf("start changeover: %v", err)
	}

	// Order B is in its initial post-start status (not yet staged), so
	// ReleaseChangeoverWait should iterate, see no staged orders, and exit.
	pending, _ := db.ListPendingOutbox(100)
	for _, m := range pending {
		_ = db.AckOutbox(m.ID)
	}

	if err := eng.ReleaseChangeoverWait(processID, "test-operator"); err != nil {
		t.Fatalf("ReleaseChangeoverWait: %v", err)
	}

	releases := findOutboxByType(t, db, protocol.TypeOrderRelease)
	if len(releases) != 0 {
		t.Errorf("expected 0 OrderRelease envelopes for no-staged-orders case, got %d", len(releases))
	}
}

// TestReleaseChangeoverWait_PartialFailureSurfacesError verifies the
// errors.Join change: when one task's release fails, the function returns
// a non-nil error mentioning the failed node instead of swallowing it.
//
// Pre-fix behaviour: log.Printf + continue, return nil — the operator got
// a 200 OK while one bin's manifest stayed stale (recreating the original
// ALN_001 incident on partial failure).
func TestReleaseChangeoverWait_PartialFailureSurfacesError(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID := seedPhase3SwapScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, err := eng.StartProcessChangeover(processID, toStyleID, "test", "partial failure")
	if err != nil {
		t.Fatalf("start changeover: %v", err)
	}

	task, err := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if err != nil || task.OldMaterialReleaseOrderID == nil {
		t.Fatalf("expected task with OldMaterialReleaseOrderID; got err=%v task=%+v", err, task)
	}

	// Force Order B to staged so ReleaseChangeoverWait will pick it up.
	if err := db.UpdateOrderStatus(*task.OldMaterialReleaseOrderID, orders.StatusStaged); err != nil {
		t.Fatalf("force order staged: %v", err)
	}

	// Inject a failure: orphan the order's ProcessNodeID so
	// ReleaseOrderWithLineside fails inside e.db.GetProcessNode. The order
	// itself still loads (the GetOrder at the top of the loop succeeds) but
	// the lineside path errors out, exercising the new failure-collection
	// code path.
	const orphanedNodeID int64 = 9999999
	if _, err := db.Exec("UPDATE orders SET process_node_id=$1 WHERE id=$2",
		orphanedNodeID, *task.OldMaterialReleaseOrderID); err != nil {
		t.Fatalf("orphan order's process node: %v", err)
	}

	gotErr := eng.ReleaseChangeoverWait(processID, "test-operator")
	if gotErr == nil {
		t.Fatal("expected non-nil error from ReleaseChangeoverWait when a task fails; got nil (the swallow-and-lie regression)")
	}
	// The error message must mention the failed node so the operator/handler
	// can tell which bin needs attention.
	msg := gotErr.Error()
	if !contains(msg, task.NodeName) {
		t.Errorf("error %q does not mention failed node %q", msg, task.NodeName)
	}
}

// contains is a small substring-match helper. Avoids importing "strings"
// just for this check; the existing tests in this package don't pull it
// in and one extra import in a test file is noise.
func contains(s, substr string) bool {
	if len(substr) == 0 {
		return true
	}
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
