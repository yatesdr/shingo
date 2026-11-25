//go:build docker

package engine

import (
	"errors"
	"fmt"
	"shingocore/internal/testdb"
	"slices"
	"strings"
	"sync"
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
	storageNode, lineNode, _ := testdb.SetupTestData(t, db)
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
	storageNode, _, _ := testdb.SetupTestData(t, db)
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
	_, lineNode, _ := testdb.SetupTestData(t, db)
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
	storageNode, _, _ := testdb.SetupTestData(t, db)
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
	storageNode, lineNode, _ := testdb.SetupTestData(t, db)
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
	if testdb.OnlyCart(t, o) == nil || *testdb.OnlyCart(t, o) != bin.ID {
		t.Errorf("the waiting move lost its bin (bin_id %v): it keeps its paper and retries", testdb.OnlyCart(t, o))
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
	storageNode, lineNode, _ := testdb.SetupTestData(t, db)
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
	testdb.SetupTestData(t, db)
	eng := newTestEngine(t, db, simulator.New())

	order := &orders.Order{
		EdgeUUID:   "term-already-done",
		StationUID: "s1",
		OrderType:  "retrieve",
		Status:     dispatch.StatusConfirmed,
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

// TestTerminateOrder_ATerminateReadBeforeTheYesStopsTheFleetJob: an order the
// UI terminate ends naming a fleet job has had a stop sent to the fleet for that
// job, when the handover's yes lands between the terminate's read of the order
// and its ending. The wire door's twin is
// dispatch.TestHandover_ACancelQueuedBehindTheYesStopsTheFleetJob.
//
// A person's bin move is acquired at its door and handed to the fleet. Inside
// the create, a transaction of the test's own takes the move's row, so the yes
// waits for it. TerminateOrder is started: it reads the order (no fleet job yet)
// and its ending, through CancelOrderWithCascade and
// LifecycleService.CancelOrder, waits behind the yes. The test lets the row go;
// the yes commits, and the ending lands on a `dispatched` row.
//
// RED AT a63768f5 (docket B20): CancelOrder decided the stop from the copy
// TerminateOrder read. Green since the ending stops the job it finds on the
// row.
func TestTerminateOrder_ATerminateReadBeforeTheYesStopsTheFleetJob(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, _ := testdb.SetupTestData(t, db)
	createTestBinAtNode(t, db, "PART-A", storageNode.ID, "BIN-TERM-BEFORE-YES")
	backend := testdb.NewTrackingBackend()
	eng := newTestEngine(t, db, backend)
	const uuid = "TERMINATE-read-before-yes"

	// waiting returns once n of this database's sessions wait on a lock: the
	// fact each step below waits for.
	waiting := func(n int) error {
		deadline := time.Now().Add(10 * time.Second)
		for {
			var got int
			if err := db.DB.QueryRow(`SELECT COUNT(*) FROM pg_stat_activity
				WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&got); err != nil {
				return err
			}
			if got == n {
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("%d session(s) wait on a lock, want %d", got, n)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	queued := make(chan error, 1)
	terminated := make(chan error, 1)
	var once sync.Once
	backend.SetOnCreate(func() {
		once.Do(func() {
			// Inside the create: the hand has committed and the yes is next.
			o, err := db.GetOrderByUUID(uuid)
			if err != nil {
				queued <- fmt.Errorf("find the move: %w", err)
				return
			}
			hold, err := db.DB.Begin()
			if err != nil {
				queued <- fmt.Errorf("begin the hold: %w", err)
				return
			}
			if _, err := hold.Exec(`SELECT 1 FROM orders WHERE id=$1 FOR UPDATE`, o.ID); err != nil {
				queued <- errors.Join(fmt.Errorf("take the move's row: %w", err), hold.Rollback())
				return
			}
			go func() {
				err := waiting(1) // the yes waits for the row
				if err == nil {
					go func() { terminated <- eng.TerminateOrder(o.ID, "operator-x") }()
					err = waiting(2) // the terminate has read the order, and its ending waits behind the yes
				}
				if cerr := hold.Commit(); err == nil {
					err = cerr
				}
				queued <- err
			}()
		})
	})

	res, err := eng.CreateBinMove(BinMoveRequest{Selection: BinSelectionAuto,
		SourceNodeID: storageNode.ID, DestNodeID: lineNode.ID,
		StationID: "term-before-yes", EdgeUUID: uuid, Desc: "terminated while the fleet takes it"})
	// Either answer is right: the terminate lands beside the move's own re-read.
	t.Logf("the move answered %+v, %v", res, err)
	testutil.MustNoErr(t, <-queued, "queue the yes and the terminate's ending behind the held row")
	select {
	case err := <-terminated:
		testutil.MustNoErr(t, err, "TerminateOrder")
	case <-time.After(10 * time.Second):
		t.Fatal("TerminateOrder never returned (fixture broken)")
	}
	creates := backend.CreateRequests()
	if len(creates) != 1 {
		t.Fatalf("expected exactly one create, got %d (fixture broken)", len(creates))
	}
	job := creates[0].OrderID

	after, err := db.GetOrderByUUID(uuid)
	testutil.MustNoErr(t, err, "reload the move")
	if after.Status != dispatch.StatusCancelled || after.VendorOrderID != job {
		t.Fatalf("the move reads %s naming fleet job %q, want cancelled naming %q: the yes did not land "+
			"ahead of the terminate's ending (fixture broken)", after.Status, after.VendorOrderID, job)
	}
	if slices.Contains(backend.CancelRequests(), job) {
		return
	}
	t.Errorf("order %d ended cancelled naming fleet job %s, and no stop was sent to the fleet for it "+
		"(stops sent: %v). TerminateOrder read the order before the yes and ended it after: the UI "+
		"terminate must reach the stop through the cancel door, which stops the job its ending finds "+
		"on the row", after.ID, job, backend.CancelRequests())
}

// ── failOrderAndEmit (indirect) ─────────────────────────────────────

// TestFailOrderAndEmit_PopulatesEventFromDB exercises the helper directly —
// package privacy lets the test call it. This path covers the success branch
// (fail + emit) plus the GetOrder lookup that populates the event
// envelope so Edge knows which station to notify.
func TestFailOrderAndEmit_PopulatesEventFromDB(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	testdb.SetupTestData(t, db)
	eng := newTestEngine(t, db, simulator.New())

	order := &orders.Order{
		EdgeUUID:   "fail-helper-1",
		StationUID: "edge-stationX",
		OrderType:  "retrieve",
		Status:     "in_transit",
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
