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
// orderMgr.ReleaseOrder directly with no remaining_uop — Core never synced
// the bin manifest, evacuation bins arrived at the supermarket tagged with
// stale state, and the bin loader couldn't move them. The fix routes every
// staged evacuation order through ReleaseOrderWithLineside with a real
// disposition so each release envelope carries the bin's intended state.
//
// As of 2026-05-06 the disposition is auto-detected per task from the
// node's runtime cache: RemainingUOPCached > 0 → send_partial_back with
// that count; == 0 → release_empty. The seedPhase3SwapScenario sets the
// runtime to 50, so this test exercises the partial path. The companion
// TestReleaseChangeoverWait_AutoDetectEmpty exercises the zero path.
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
	if err := db.UpdateOrderStatus(orderB.ID, string(orders.StatusStaged)); err != nil {
		t.Fatalf("force order B staged: %v", err)
	}

	// Drain any pending outbox messages from the changeover-start phase so
	// we can assert exactly one OrderRelease lands from the wait release.
	pending, _ := db.ListPendingOutbox(100)
	for _, m := range pending {
		_ = db.AckOutbox(m.ID)
	}

	if _, err := eng.ReleaseChangeoverWait(processID, ReleaseDisposition{CalledBy: "test-operator"}); err != nil {
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
	// Auto-detect: scenario seeded RemainingUOPCached=50, so the evac
	// envelope should carry remaining_uop=50 and disposition=release_partial.
	// Without RemainingUOP being non-nil at all, Core can't sync the
	// manifest — that's the original bypass we lock against.
	if rel.RemainingUOP == nil {
		t.Fatal("OrderRelease.RemainingUOP = nil; manifest sync requires non-nil")
	}
	if *rel.RemainingUOP != 50 {
		t.Errorf("OrderRelease.RemainingUOP = %d, want 50 (auto-detected from runtime cache)", *rel.RemainingUOP)
	}
	if rel.Disposition == nil || rel.Disposition.Kind != protocol.DispositionReleasePartial {
		t.Errorf("OrderRelease.Disposition = %+v, want kind=release_partial", rel.Disposition)
	}

	// Order B should now be in_transit (release dispatched).
	got, _ := db.GetOrder(orderB.ID)
	if got.Status != orders.StatusInTransit {
		t.Errorf("order B status = %q, want %q", got.Status, orders.StatusInTransit)
	}
}

// TestReleaseChangeoverWait_AutoDetectEmpty exercises the auto-detect path
// when the line is empty. RemainingUOPCached=0 → evac is sent as
// release_empty (capture_lineside + empty captures → wire-form release_empty),
// preserving the 2026-04 ALN_001 fix intent (manifest cleared so evac bin
// can't land at OutboundDestination tagged with stale payload).
func TestReleaseChangeoverWait_AutoDetectEmpty(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID := seedPhase3SwapScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	// Override seed: line is empty (operator finished consumption before
	// changeover).
	runtime, err := db.GetProcessNodeRuntime(nodeID)
	if err != nil {
		t.Fatalf("get runtime: %v", err)
	}
	if err := db.SetProcessNodeRuntime(nodeID, runtime.ActiveClaimID, 0); err != nil {
		t.Fatalf("set runtime to 0: %v", err)
	}

	changeover, err := eng.StartProcessChangeover(processID, toStyleID, "test", "auto-detect empty")
	if err != nil {
		t.Fatalf("start changeover: %v", err)
	}
	task, err := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	orderB, _ := db.GetOrder(*task.OldMaterialReleaseOrderID)
	if err := db.UpdateOrderStatus(orderB.ID, string(orders.StatusStaged)); err != nil {
		t.Fatalf("force staged: %v", err)
	}
	pending, _ := db.ListPendingOutbox(100)
	for _, m := range pending {
		_ = db.AckOutbox(m.ID)
	}

	if _, err := eng.ReleaseChangeoverWait(processID, ReleaseDisposition{CalledBy: "test-operator"}); err != nil {
		t.Fatalf("ReleaseChangeoverWait: %v", err)
	}

	releases := findOutboxByType(t, db, protocol.TypeOrderRelease)
	if len(releases) != 1 {
		t.Fatalf("OrderRelease envelopes queued: got %d, want 1", len(releases))
	}
	rel := decodeOrderRelease(t, releases[0])
	if rel.RemainingUOP == nil {
		t.Fatal("RemainingUOP = nil; expected &0 for release_empty")
	}
	if *rel.RemainingUOP != 0 {
		t.Errorf("RemainingUOP = %d, want 0 (auto-detected empty)", *rel.RemainingUOP)
	}
	if rel.Disposition == nil || rel.Disposition.Kind != protocol.DispositionReleaseEmpty {
		t.Errorf("Disposition = %+v, want kind=release_empty", rel.Disposition)
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

	if _, err := eng.ReleaseChangeoverWait(processID, ReleaseDisposition{CalledBy: "test-operator"}); err != nil {
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
	if err := db.UpdateOrderStatus(*task.OldMaterialReleaseOrderID, string(orders.StatusStaged)); err != nil {
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

	_, gotErr := eng.ReleaseChangeoverWait(processID, ReleaseDisposition{CalledBy: "test-operator"})
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

// TestReleaseChangeoverWait_SupplyManifestPreserved locks in the
// evac-vs-supply disposition asymmetry. Plant incident on order 682
// (2026-05-06): the changeover Release fired on a staged supply leg with
// disposition=release_empty, zeroing the supply bin's manifest before it
// arrived at the line. Bin landed empty; lineside runtime cached 0;
// consume drove the counter negative.
//
// Fix: ReleaseChangeoverWait passes the operator's disposition only to
// the evac slot (OldMaterialReleaseOrderID). The supply slot
// (NextMaterialOrderID) always gets Mode="" → buildProtocolDisposition
// returns nil → wire-form omits disposition → Core's
// SyncOrClearForReleased no-ops the manifest.
//
// Regression assertions:
//   - Two OrderRelease envelopes queued (one per slot).
//   - Evac envelope: Disposition.Kind == release_partial, Count == 47
//     (the operator's chosen partial count flows through).
//   - Supply envelope: Disposition is nil (manifest left alone).
func TestReleaseChangeoverWait_SupplyManifestPreserved(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID := seedPhase3SwapScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, err := eng.StartProcessChangeover(processID, toStyleID, "test", "supply manifest preservation")
	if err != nil {
		t.Fatalf("start changeover: %v", err)
	}

	task, err := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if err != nil {
		t.Fatalf("get node task: %v", err)
	}
	if task.OldMaterialReleaseOrderID == nil {
		t.Fatal("expected evac order (OldMaterialReleaseOrderID) to be set after Phase 3 swap start")
	}
	if task.NextMaterialOrderID == nil {
		t.Fatal("expected supply order (NextMaterialOrderID) to be set; this scenario assumes both legs exist")
	}

	evacOrder, err := db.GetOrder(*task.OldMaterialReleaseOrderID)
	if err != nil {
		t.Fatalf("get evac order: %v", err)
	}
	supplyOrder, err := db.GetOrder(*task.NextMaterialOrderID)
	if err != nil {
		t.Fatalf("get supply order: %v", err)
	}

	// Force both orders to staged so ReleaseChangeoverWait's per-slot loop
	// fires on each. In production the fleet tracker advances these as the
	// robots dwell; the dispatcher-level test has no fleet wiring.
	if err := db.UpdateOrderStatus(evacOrder.ID, string(orders.StatusStaged)); err != nil {
		t.Fatalf("force evac staged: %v", err)
	}
	if err := db.UpdateOrderStatus(supplyOrder.ID, string(orders.StatusStaged)); err != nil {
		t.Fatalf("force supply staged: %v", err)
	}

	// Drain outbox so we can count exactly the envelopes produced by the
	// release call.
	pending, _ := db.ListPendingOutbox(100)
	for _, m := range pending {
		_ = db.AckOutbox(m.ID)
	}

	// Operator chooses SEND PARTIAL BACK with count 47 — applied to evac.
	partial := 47
	disp := ReleaseDisposition{
		Mode:         DispositionSendPartialBack,
		PartialCount: &partial,
		CalledBy:     "test-operator",
	}
	result, err := eng.ReleaseChangeoverWait(processID, disp)
	if err != nil {
		t.Fatalf("ReleaseChangeoverWait: %v", err)
	}
	if result.Released != 2 {
		t.Errorf("result.Released = %d, want 2 (both legs were staged)", result.Released)
	}
	if result.Pending != 0 {
		t.Errorf("result.Pending = %d, want 0 (no legs were skipped)", result.Pending)
	}

	releases := findOutboxByType(t, db, protocol.TypeOrderRelease)
	if len(releases) != 2 {
		t.Fatalf("OrderRelease envelopes queued: got %d, want 2 (one per slot)", len(releases))
	}

	var evacRel, supplyRel *protocol.OrderRelease
	for i := range releases {
		decoded := decodeOrderRelease(t, releases[i])
		switch decoded.OrderUUID {
		case evacOrder.UUID:
			d := decoded
			evacRel = &d
		case supplyOrder.UUID:
			d := decoded
			supplyRel = &d
		default:
			t.Errorf("unexpected OrderRelease for UUID %q", decoded.OrderUUID)
		}
	}
	if evacRel == nil {
		t.Fatal("no OrderRelease envelope for evac order")
	}
	if supplyRel == nil {
		t.Fatal("no OrderRelease envelope for supply order")
	}

	// Evac leg: disposition flows through.
	if evacRel.Disposition == nil {
		t.Fatal("evac OrderRelease.Disposition = nil; want release_partial with operator's count")
	}
	if evacRel.Disposition.Kind != protocol.DispositionReleasePartial {
		t.Errorf("evac disposition kind = %q, want %q",
			evacRel.Disposition.Kind, protocol.DispositionReleasePartial)
	}
	if evacRel.Disposition.Count != partial {
		t.Errorf("evac disposition count = %d, want %d (operator's partial count)",
			evacRel.Disposition.Count, partial)
	}

	// Supply leg: NO manifest action. Disposition must be nil so Core's
	// SyncOrClearForReleased no-ops. THIS is the regression lock.
	if supplyRel.Disposition != nil {
		t.Errorf("supply OrderRelease.Disposition = %+v, want nil (manifest must NOT be touched on the supply leg)",
			supplyRel.Disposition)
	}
	// Same regression check on RemainingUOP: must be nil for the supply leg.
	// Anything else (especially &0) is the bug fingerprint that wiped the
	// bin manifest at Core on order 682 / 2026-05-06.
	if supplyRel.RemainingUOP != nil {
		t.Errorf("supply OrderRelease.RemainingUOP = &%d, want nil (manifest preservation contract)",
			*supplyRel.RemainingUOP)
	}
}
