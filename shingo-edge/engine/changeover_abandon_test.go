package engine

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/orders"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// changeover_abandon_test.go — C(ii) Edge state machine: the awaiting_material
// park made visible (entry via the queue-reason push), the operator exits
// (α plain abandon, β in-flight refusal, γ accept-half-swap), and the gate
// interplay (awaiting blocks conjunct 1; abandoned is terminal and unblocks).

// seedAwaitingScenario stands up the Phase-3 swap changeover, parks both legs
// at queued, stamps the task awaiting_material, and drains the outbox so
// envelope assertions are exact.
func seedAwaitingScenario(t *testing.T) (eng *Engine, db *store.DB, processID int64, nodeID int64, changeoverID int64, task *processes.NodeTask, supplyID, evacID int64) {
	t.Helper()
	db = testEngineDB(t)
	eng = testEngine(t, db)
	var toStyleID int64
	processID, nodeID, _, toStyleID = seedPhase3SwapScenario(t, db)
	eng.wireEventHandlers()
	changeover, tk := startChangeover(t, eng, db, processID, toStyleID)
	if tk.NextMaterialOrderID == nil || tk.OldMaterialReleaseOrderID == nil {
		t.Fatal("scenario needs both a supply and an evac order on the task")
	}
	changeoverID = changeover.ID
	supplyID, evacID = *tk.NextMaterialOrderID, *tk.OldMaterialReleaseOrderID
	testutil.MustNoErr(t, db.UpdateOrderStatus(supplyID, string(orders.StatusQueued)), "park supply")
	testutil.MustNoErr(t, db.UpdateOrderStatus(evacID, string(orders.StatusQueued)), "evac queued")
	testutil.MustNoErr(t, db.UpdateChangeoverNodeTaskState(tk.ID, domain.NodeTaskAwaitingMaterial), "stamp awaiting")
	drainAbandonOutbox(t, db)
	task = tk
	return
}

func drainAbandonOutbox(t *testing.T, db *store.DB) {
	t.Helper()
	pending, _ := db.ListPendingOutbox(100)
	for _, m := range pending {
		_ = db.AckOutbox(m.ID)
	}
}

// decodeCancel unwraps an outbox OrderCancel envelope.
func decodeCancel(t *testing.T, db *store.DB) []protocol.OrderCancel {
	t.Helper()
	var out []protocol.OrderCancel
	for _, msg := range findOutboxByType(t, db, protocol.TypeOrderCancel) {
		var env protocol.Envelope
		testutil.MustNoErr(t, json.Unmarshal(msg.Payload, &env), "unmarshal envelope")
		var oc protocol.OrderCancel
		testutil.MustNoErr(t, env.DecodePayload(&oc), "decode OrderCancel")
		out = append(out, oc)
	}
	return out
}

// TestAwaitingMaterial_EntryFromQueueReasonPush — V1a: Core's
// waiting_for_material push on a changeover SUPPLY order stamps the owning
// task awaiting_material. Any other code, or the same code on the EVAC leg,
// or a terminal task/changeover, must not.
func TestAwaitingMaterial_EntryFromQueueReasonPush(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	processID, _, _, toStyleID := seedPhase3SwapScenario(t, db)
	eng.wireEventHandlers()
	_, task := startChangeover(t, eng, db, processID, toStyleID)
	if task.NextMaterialOrderID == nil || task.OldMaterialReleaseOrderID == nil {
		t.Fatal("scenario needs both legs")
	}
	supply, err := db.GetOrder(*task.NextMaterialOrderID)
	testutil.MustNoErr(t, err, "get supply")
	evac, err := db.GetOrder(*task.OldMaterialReleaseOrderID)
	testutil.MustNoErr(t, err, "get evac")

	// A non-material code does not stamp.
	testutil.MustNoErr(t, eng.orderMgr.SetOrderQueueReason(supply.UUID, "capacity", "waiting_for_capacity"), "other code")
	tk, err := db.GetChangeoverNodeTaskByNode(task.ProcessChangeoverID, task.ProcessNodeID)
	testutil.MustNoErr(t, err, "re-read task")
	if tk.State == domain.NodeTaskAwaitingMaterial {
		t.Fatal("a non-material queue code stamped awaiting_material")
	}

	// The evac leg never stamps (an evac doesn't park for material).
	testutil.MustNoErr(t, eng.orderMgr.SetOrderQueueReason(evac.UUID, "waiting", string(protocol.QueueWaitingForMaterial)), "evac push")
	tk, _ = db.GetChangeoverNodeTaskByNode(task.ProcessChangeoverID, task.ProcessNodeID)
	if tk.State == domain.NodeTaskAwaitingMaterial {
		t.Fatal("a waiting_for_material push on the EVAC leg stamped the task")
	}

	// The supply leg stamps.
	testutil.MustNoErr(t, eng.orderMgr.SetOrderQueueReason(supply.UUID, "Waiting for material", string(protocol.QueueWaitingForMaterial)), "supply push")
	tk, _ = db.GetChangeoverNodeTaskByNode(task.ProcessChangeoverID, task.ProcessNodeID)
	if tk.State != domain.NodeTaskAwaitingMaterial {
		t.Fatalf("task state = %q, want awaiting_material after the supply park push", tk.State)
	}

	// A terminal task never regresses to awaiting.
	testutil.MustNoErr(t, db.UpdateChangeoverNodeTaskState(tk.ID, domain.NodeTaskSwitched), "terminal task")
	testutil.MustNoErr(t, eng.orderMgr.SetOrderQueueReason(supply.UUID, "Waiting for material", string(protocol.QueueWaitingForMaterial)), "re-push")
	tk, _ = db.GetChangeoverNodeTaskByNode(task.ProcessChangeoverID, task.ProcessNodeID)
	if tk.State != domain.NodeTaskSwitched {
		t.Fatalf("terminal task regressed to %q on a late park push", tk.State)
	}
}

// TestAbandon_PlainCancelsSupplyAndSettlesTask — α. The supply cancel goes up
// with an ordinary reason (Core's swap-peer arm cascades to the evac
// fail-closed); Edge itself must NOT locally cancel the evac (single cancel
// writer — Core's push does it). Task lands abandoned-terminal with a note.
func TestAbandon_PlainCancelsSupplyAndSettlesTask(t *testing.T) {
	t.Parallel()
	eng, db, processID, nodeID, _, task, supplyID, evacID := seedAwaitingScenario(t)

	testutil.MustNoErr(t, eng.AbandonChangeoverSupply(processID, nodeID, false, "test-operator"), "abandon")

	cancels := decodeCancel(t, db)
	if len(cancels) != 1 {
		t.Fatalf("cancel envelopes = %d, want exactly 1 (the supply)", len(cancels))
	}
	supply, _ := db.GetOrder(supplyID)
	if cancels[0].OrderUUID != supply.UUID {
		t.Errorf("cancel uuid = %q, want the supply %q", cancels[0].OrderUUID, supply.UUID)
	}
	if cancels[0].Reason == protocol.CancelReasonAcceptHalfSwap {
		t.Error("plain abandon used the accept_half_swap reason — Core would spare the evac it must cascade to")
	}
	if supply.Status != orders.StatusCancelled {
		t.Errorf("supply status = %q, want cancelled", supply.Status)
	}
	evac, _ := db.GetOrder(evacID)
	if evac.Status != orders.StatusQueued {
		t.Errorf("evac status = %q, want queued untouched — Core cascades, Edge must not double-cancel", evac.Status)
	}
	tk, _ := db.GetChangeoverNodeTaskByNode(task.ProcessChangeoverID, task.ProcessNodeID)
	if tk.State != domain.NodeTaskAbandoned {
		t.Fatalf("task state = %q, want abandoned", tk.State)
	}
	if !tk.State.IsTerminal(tk.Situation) {
		t.Error("abandoned must be terminal for the completion gate")
	}
	if tk.SkipNote == "" {
		t.Error("no skip note — the operator-facing story is required")
	}
}

// TestAbandon_RefusedWhileEvacInFlight — β. A fleet-active partner evac
// forbids the plain abandon (the cascade would yank a robot mid-move).
func TestAbandon_RefusedWhileEvacInFlight(t *testing.T) {
	t.Parallel()
	eng, db, processID, nodeID, _, task, supplyID, evacID := seedAwaitingScenario(t)
	testutil.MustNoErr(t, db.UpdateOrderStatus(evacID, string(protocol.StatusInTransit)), "evac in flight")

	err := eng.AbandonChangeoverSupply(processID, nodeID, false, "test-operator")
	if !errors.Is(err, ErrPartnerInFlight) {
		t.Fatalf("err = %v, want ErrPartnerInFlight", err)
	}
	if cancels := decodeCancel(t, db); len(cancels) != 0 {
		t.Errorf("cancel envelopes = %d, want 0 on refusal", len(cancels))
	}
	supply, _ := db.GetOrder(supplyID)
	if supply.Status != orders.StatusQueued {
		t.Errorf("supply status = %q, want still queued", supply.Status)
	}
	tk, _ := db.GetChangeoverNodeTaskByNode(task.ProcessChangeoverID, task.ProcessNodeID)
	if tk.State != domain.NodeTaskAwaitingMaterial {
		t.Errorf("task state = %q, want still awaiting_material", tk.State)
	}
}

// TestAbandon_AcceptHalfSwapReason — γ, the V1b share: the cancel goes up
// with protocol.CancelReasonAcceptHalfSwap (Core spares the evac), allowed
// even while the evac is fleet-active.
func TestAbandon_AcceptHalfSwapReason(t *testing.T) {
	t.Parallel()
	eng, db, processID, nodeID, _, task, _, evacID := seedAwaitingScenario(t)
	testutil.MustNoErr(t, db.UpdateOrderStatus(evacID, string(protocol.StatusInTransit)), "evac in flight")

	testutil.MustNoErr(t, eng.AbandonChangeoverSupply(processID, nodeID, true, "test-operator"), "accept half-swap")

	cancels := decodeCancel(t, db)
	if len(cancels) != 1 {
		t.Fatalf("cancel envelopes = %d, want 1", len(cancels))
	}
	if cancels[0].Reason != protocol.CancelReasonAcceptHalfSwap {
		t.Fatalf("cancel reason = %q, want %q — this string IS the no-cascade contract with Core",
			cancels[0].Reason, protocol.CancelReasonAcceptHalfSwap)
	}
	evac, _ := db.GetOrder(evacID)
	if evac.Status != protocol.StatusInTransit {
		t.Errorf("evac status = %q, want in_transit untouched", evac.Status)
	}
	tk, _ := db.GetChangeoverNodeTaskByNode(task.ProcessChangeoverID, task.ProcessNodeID)
	if tk.State != domain.NodeTaskAbandoned {
		t.Fatalf("task state = %q, want abandoned", tk.State)
	}
	if !strings.Contains(tk.SkipNote, "half-swap") {
		t.Errorf("skip note %q should tell the half-swap story", tk.SkipNote)
	}
}

// TestAbandon_Guards: not-awaiting tasks and in-motion supplies refuse.
func TestAbandon_Guards(t *testing.T) {
	t.Parallel()
	eng, db, processID, nodeID, _, task, supplyID, _ := seedAwaitingScenario(t)

	// Supply un-parked and moving: material is coming — let it arrive.
	testutil.MustNoErr(t, db.UpdateOrderStatus(supplyID, string(protocol.StatusInTransit)), "supply in motion")
	if err := eng.AbandonChangeoverSupply(processID, nodeID, false, "t"); err == nil {
		t.Fatal("abandon accepted while the supply is in motion")
	}
	testutil.MustNoErr(t, db.UpdateOrderStatus(supplyID, string(orders.StatusQueued)), "re-park")

	// Task not awaiting: nothing to abandon.
	testutil.MustNoErr(t, db.UpdateChangeoverNodeTaskState(task.ID, domain.NodeTaskStagingRequested), "un-stamp")
	if err := eng.AbandonChangeoverSupply(processID, nodeID, false, "t"); err == nil {
		t.Fatal("abandon accepted on a non-awaiting task")
	}
	if cancels := decodeCancel(t, db); len(cancels) != 0 {
		t.Errorf("cancel envelopes = %d, want 0 — both guards must refuse before any cancel", len(cancels))
	}
}

// TestAwaitingMaterial_BlocksGateUntilAbandon closes the loop with the
// completion gate: awaiting_material is non-terminal (conjunct 1 blocks,
// and the blocker names the node for the panel); after the abandon the task
// is terminal and the gate opens.
func TestAwaitingMaterial_BlocksGateUntilAbandon(t *testing.T) {
	t.Parallel()
	eng, db, processID, nodeID, changeoverID, _, _, evacID := seedAwaitingScenario(t)
	// Keep conjunct 2′ quiet: the evac's steps place nothing at a participant.
	testutil.MustNoErr(t, db.UpdateOrderStepsJSON(evacID,
		`[{"action":"pickup","node":"P3-NODE"},{"action":"dropoff","node":"MARKET-X"}]`), "outbound evac")

	ok, blockers, err := eng.canCompleteChangeover(changeoverID)
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	if ok {
		t.Fatal("gate open with a task awaiting material — the park must block completion")
	}
	if joined := strings.Join(domain.BlockersToReasons(blockers), "; "); !strings.Contains(joined, "Phase3 Swap Node") && !strings.Contains(joined, "P3-NODE") {
		t.Errorf("no blocker names the awaiting node; blockers = %q", joined)
	}

	testutil.MustNoErr(t, eng.AbandonChangeoverSupply(processID, nodeID, false, "test-operator"), "abandon")

	ok, blockers, err = eng.canCompleteChangeover(changeoverID)
	if err != nil {
		t.Fatalf("gate after abandon: %v", err)
	}
	if !ok {
		t.Fatalf("gate still blocked after abandon: %v — abandoned must read terminal", domain.BlockersToReasons(blockers))
	}
}
