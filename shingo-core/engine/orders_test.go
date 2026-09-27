//go:build docker

package engine

import (
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/store/orders"
)

// orders_test.go — coverage for orders.go.
//
// Covers CreateBinMove (success + same-source/dest, missing nodes,
// fleet failure), TerminateOrder (success + terminal-status guard +
// missing-order error), and failOrderAndEmit (asserted via the indirect
// behavior contract: fleet failure inside CreateBinMove leaves
// the order in failed/dispatch error state and emits an EventOrderFailed).

// ── CreateBinMove happy path ────────────────────────────────────

func TestCreateBinMove_Success_PersistsAndDispatches(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, _ := setupTestData(t, db)
	createTestBinAtNode(t, db, "PART-A", storageNode.ID, "BIN-DIRECT-OK")
	eng := newTestEngine(t, db, simulator.New())

	captured := make(chan OrderDispatchedEvent, 2)
	eng.Events.SubscribeTypes(func(evt Event) {
		captured <- evt.Payload.(OrderDispatchedEvent)
	}, EventOrderDispatched)

	res, err := eng.CreateBinMove(BinMoveRequest{Selection: BinSelectionAuto,
		SourceNodeID: storageNode.ID,
		DestNodeID:   lineNode.ID,
		StationID:    "test-station",
		Priority:     3,
		Desc:         "manual move",
	})
	if err != nil {
		t.Fatalf("CreateBinMove: %v", err)
	}
	if res == nil || res.OrderID == 0 {
		t.Fatalf("result missing OrderID: %+v", res)
	}
	if res.VendorOrderID == "" {
		t.Error("expected non-empty VendorOrderID after dispatch")
	}
	if res.FromNode != storageNode.Name || res.ToNode != lineNode.Name {
		t.Errorf("from/to = %s/%s, want %s/%s", res.FromNode, res.ToNode, storageNode.Name, lineNode.Name)
	}

	// Verify order persisted with the right shape.
	got, err := db.GetOrder(res.OrderID)
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if got.OrderType != "move" {
		t.Errorf("OrderType = %q, want move", got.OrderType)
	}
	if got.SourceNode != storageNode.Name || got.DeliveryNode != lineNode.Name {
		t.Errorf("source/delivery = %s/%s", got.SourceNode, got.DeliveryNode)
	}
	if got.Priority != 3 {
		t.Errorf("Priority = %d, want 3", got.Priority)
	}
	// After successful DispatchDirect, status should be "dispatched".
	if got.Status != dispatch.StatusDispatched {
		t.Errorf("Status = %q, want %q", got.Status, dispatch.StatusDispatched)
	}

	// Dispatch event fired.
	select {
	case ev := <-captured:
		if ev.OrderID != res.OrderID {
			t.Errorf("event OrderID = %d, want %d", ev.OrderID, res.OrderID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("EventOrderDispatched not fired")
	}
}

// ── CreateBinMove error paths ───────────────────────────────────

func TestCreateBinMove_SameSourceAndDest(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, _, _ := setupTestData(t, db)
	eng := newTestEngine(t, db, simulator.New())

	res, err := eng.CreateBinMove(BinMoveRequest{Selection: BinSelectionAuto,
		SourceNodeID: storageNode.ID,
		DestNodeID:   storageNode.ID,
		StationID:    "test",
	})
	if err == nil {
		t.Fatal("expected error for same source/dest")
	}
	if res != nil {
		t.Errorf("expected nil result, got %+v", res)
	}
	if !strings.Contains(err.Error(), "different") {
		t.Errorf("err = %v, want mention of 'different'", err)
	}
}

func TestCreateBinMove_MissingSourceNode(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, _ := setupTestData(t, db)
	eng := newTestEngine(t, db, simulator.New())

	_, err := eng.CreateBinMove(BinMoveRequest{Selection: BinSelectionAuto,
		SourceNodeID: 99999,
		DestNodeID:   lineNode.ID,
		StationID:    "test",
	})
	if err == nil {
		t.Fatal("expected error for missing source")
	}
	if !strings.Contains(err.Error(), "source") {
		t.Errorf("err = %v, want mention of 'source'", err)
	}
}

func TestCreateBinMove_MissingDestNode(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, _, _ := setupTestData(t, db)
	eng := newTestEngine(t, db, simulator.New())

	_, err := eng.CreateBinMove(BinMoveRequest{Selection: BinSelectionAuto,
		SourceNodeID: storageNode.ID,
		DestNodeID:   88888,
		StationID:    "test",
	})
	if err == nil {
		t.Fatal("expected error for missing dest")
	}
	if !strings.Contains(err.Error(), "destination") {
		t.Errorf("err = %v, want mention of 'destination'", err)
	}
}

// TestCreateBinMove_FleetRefusalQueues: a bin move the fleet refuses waits
// under fleet_unavailable like every other order, instead of failing (the orc's
// Q2 ruling, v4 §2.1). The door no longer dispatches; acquiring does, and its
// fleet-refusal disposition is the demotion to queued with the paper kept.
func TestCreateBinMove_FleetRefusalQueues(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, _ := setupTestData(t, db)
	bin := createTestBinAtNode(t, db, "PART-A", storageNode.ID, "BIN-DIRECT-FAIL")
	sim := simulator.New(simulator.WithCreateFailure())
	eng := newTestEngine(t, db, sim)

	failed := make(chan OrderFailedEvent, 4)
	eng.Events.SubscribeTypes(func(evt Event) {
		failed <- evt.Payload.(OrderFailedEvent)
	}, EventOrderFailed)

	res, err := eng.CreateBinMove(BinMoveRequest{Selection: BinSelectionAuto,
		SourceNodeID: storageNode.ID,
		DestNodeID:   lineNode.ID,
		StationID:    "test",
		Desc:         "the fleet refuses this",
	})
	if err != nil {
		t.Fatalf("a fleet refusal came back as a refusal (%v): it is congestion, and the move waits", err)
	}
	if !res.Queued || res.QueueReason == "" {
		t.Fatalf("result %+v, want Queued with the sentence saying what it waits for", res)
	}
	o, err := db.GetOrder(res.OrderID)
	if err != nil {
		t.Fatalf("reload the move: %v", err)
	}
	if !protocol.IsAcquiring(o.Status) || o.QueueCode != string(protocol.QueueFleetUnavailable) {
		t.Errorf("move is %s under %q, want waiting (queued or sourcing) under %q", o.Status, o.QueueCode, protocol.QueueFleetUnavailable)
	}
	if o.BinID == nil || *o.BinID != bin.ID {
		t.Errorf("the waiting move lost its bin (bin_id %v): it keeps its paper and retries", o.BinID)
	}
	select {
	case ev := <-failed:
		t.Errorf("EventOrderFailed fired for order %d: a fleet refusal is not a failure", ev.OrderID)
	case <-time.After(200 * time.Millisecond):
	}
}

// ── TerminateOrder ──────────────────────────────────────────────────

// TestTerminateOrder_CancelsActiveOrder makes a real order and then
// cancels it — verifies status flip to cancelled, audit, and emitted
// EventOrderCancelled.
func TestTerminateOrder_CancelsActiveOrder(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, _ := setupTestData(t, db)
	createTestBinAtNode(t, db, "PART-A", storageNode.ID, "BIN-TERMINATE")
	eng := newTestEngine(t, db, simulator.New())

	// Direct order to give us something dispatched.
	res, err := eng.CreateBinMove(BinMoveRequest{Selection: BinSelectionAuto,
		SourceNodeID: storageNode.ID,
		DestNodeID:   lineNode.ID,
		StationID:    "term-test",
		Desc:         "to be cancelled",
	})
	if err != nil {
		t.Fatalf("seed CreateBinMove: %v", err)
	}

	cancelled := make(chan OrderCancelledEvent, 2)
	eng.Events.SubscribeTypes(func(evt Event) {
		cancelled <- evt.Payload.(OrderCancelledEvent)
	}, EventOrderCancelled)

	testutil.MustNoErr(t, eng.TerminateOrder(res.OrderID, "operator-x"), "TerminateOrder")

	got, err := db.GetOrder(res.OrderID)
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if got.Status != dispatch.StatusCancelled {
		t.Errorf("Status = %q, want cancelled", got.Status)
	}

	select {
	case ev := <-cancelled:
		if ev.OrderID != res.OrderID {
			t.Errorf("cancel event OrderID = %d, want %d", ev.OrderID, res.OrderID)
		}
		if !strings.Contains(ev.Reason, "operator-x") {
			t.Errorf("cancel reason = %q, want operator name", ev.Reason)
		}
		if ev.PreviousStatus == "" {
			t.Errorf("PreviousStatus should be populated, got empty")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("EventOrderCancelled not fired")
	}
}

func TestTerminateOrder_RejectsTerminalStatus(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	setupTestData(t, db)
	eng := newTestEngine(t, db, simulator.New())

	order := &orders.Order{
		EdgeUUID:  "term-already-done",
		StationID: "s1",
		OrderType: "retrieve",
		Status:    dispatch.StatusConfirmed,
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")

	err := eng.TerminateOrder(order.ID, "op")
	if err == nil {
		t.Fatal("expected error for terminal status")
	}
	if !strings.Contains(err.Error(), "cannot terminate") {
		t.Errorf("err = %v, want guard message", err)
	}
}

func TestTerminateOrder_MissingOrder(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	err := eng.TerminateOrder(7777777, "op")
	if err == nil {
		t.Fatal("expected error for missing order")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("err = %v, want 'not found'", err)
	}
}

// ── failOrderAndEmit (indirect) ─────────────────────────────────────

// TestFailOrderAndEmit_Direct exercises the helper directly — package
// privacy lets the test call it. This path covers the success branch
// (fail + emit) plus the GetOrder lookup that populates the event
// envelope so Edge knows which station to notify.
func TestFailOrderAndEmit_PopulatesEventFromDB(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	setupTestData(t, db)
	eng := newTestEngine(t, db, simulator.New())

	order := &orders.Order{
		EdgeUUID:  "fail-helper-1",
		StationID: "edge-stationX",
		OrderType: "retrieve",
		Status:    "in_transit",
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")

	failed := make(chan OrderFailedEvent, 2)
	eng.Events.SubscribeTypes(func(evt Event) {
		failed <- evt.Payload.(OrderFailedEvent)
	}, EventOrderFailed)

	eng.failOrderAndEmit(order.ID, "scanner_err", "scanner-driven failure")

	got, err := db.GetOrder(order.ID)
	if err != nil {
		t.Fatalf("reload order: %v", err)
	}
	if got.Status != "failed" {
		t.Errorf("Status = %q, want failed", got.Status)
	}

	select {
	case ev := <-failed:
		if ev.OrderID != order.ID {
			t.Errorf("OrderID = %d, want %d", ev.OrderID, order.ID)
		}
		if ev.StationID != "edge-stationX" {
			t.Errorf("StationID = %q, want hydrated from DB", ev.StationID)
		}
		if ev.EdgeUUID != "fail-helper-1" {
			t.Errorf("EdgeUUID = %q, want hydrated from DB", ev.EdgeUUID)
		}
		if ev.ErrorCode != "scanner_err" {
			t.Errorf("ErrorCode = %q", ev.ErrorCode)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("EventOrderFailed not emitted")
	}
}
