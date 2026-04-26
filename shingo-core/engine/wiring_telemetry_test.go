//go:build docker

package engine

import (
	"encoding/json"
	"testing"
	"time"

	"shingocore/dispatch"
	"shingocore/fleet"
	"shingocore/fleet/simulator"
	"shingocore/store"
	"shingocore/store/orders"
)

// wiring_telemetry_test.go — coverage for wiring_telemetry.go.
//
// The two functions under test (recordMissionEvent + finalizeMissionTelemetry)
// are pure side-effect-on-DB code. We exercise them by direct call (the
// production wiring subscribes them to EventOrderStatusChanged, so calling
// the helpers directly sidesteps event-bus timing flakiness while still
// covering every branch).
//
// Verifications:
//   - InsertMissionEvent persisted a row with snapshot fields hydrated
//   - For terminal states, UpsertMissionTelemetry rolled up the summary
//   - For non-terminal states, NO telemetry row was written
//   - Snapshot JSON columns reflect the marshaled vendor blocks/errors

// makeTelemetryOrder seeds an order so finalizeMissionTelemetry's
// GetOrder lookup succeeds; returns the persisted *orders.Order.
func makeTelemetryOrder(t *testing.T, db *store.DB, edgeUUID, station string) *orders.Order {
	t.Helper()
	o := &orders.Order{
		EdgeUUID:      edgeUUID,
		StationID:     station,
		OrderType:     dispatch.OrderTypeRetrieve,
		Status:        dispatch.StatusInTransit,
		VendorOrderID: "V-" + edgeUUID,
		SourceNode:    "STORAGE-A1",
		DeliveryNode:  "LINE1-IN",
	}
	if err := db.CreateOrder(o); err != nil {
		t.Fatalf("create order: %v", err)
	}
	return o
}

// ── recordMissionEvent ──────────────────────────────────────────────

// TestRecordMissionEvent_InsertsRowWithSnapshotJSON proves the row
// lands in mission_events and the Blocks/Errors JSON columns are
// populated from the OrderSnapshot.
func TestRecordMissionEvent_InsertsRowWithSnapshotJSON(t *testing.T) {
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	order := makeTelemetryOrder(t, db, "tel-1", "station-tel-1")

	snap := &fleet.OrderSnapshot{
		VendorOrderID: order.VendorOrderID,
		State:         "RUNNING",
		Blocks: []fleet.BlockSnapshot{
			{BlockID: "B1", Location: "P1", State: "EXECUTING"},
		},
		Errors: []fleet.OrderMessage{
			{Code: 42, Desc: "minor hiccup", Times: 1},
		},
	}

	eng.recordMissionEvent(OrderStatusChangedEvent{
		OrderID:       order.ID,
		VendorOrderID: order.VendorOrderID,
		OldStatus:     "CREATED",
		NewStatus:     "RUNNING", // non-terminal — telemetry row should NOT be written
		RobotID:       "AMR-X",
		Detail:        "moving",
		Snapshot:      snap,
	})

	events, err := db.ListMissionEvents(order.ID)
	if err != nil {
		t.Fatalf("ListMissionEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("mission_events rows = %d, want 1", len(events))
	}
	me := events[0]
	if me.OldState != "CREATED" || me.NewState != "RUNNING" {
		t.Errorf("state transition = %s→%s", me.OldState, me.NewState)
	}
	if me.RobotID != "AMR-X" {
		t.Errorf("RobotID = %q, want AMR-X", me.RobotID)
	}
	if me.Detail != "moving" {
		t.Errorf("Detail = %q", me.Detail)
	}

	// Blocks JSON should round-trip the snapshot block.
	var blocks []fleet.BlockSnapshot
	if err := json.Unmarshal([]byte(me.BlocksJSON), &blocks); err != nil {
		t.Fatalf("decode BlocksJSON: %v", err)
	}
	if len(blocks) != 1 || blocks[0].BlockID != "B1" {
		t.Errorf("BlocksJSON content = %+v", blocks)
	}

	// Errors JSON likewise.
	var errs []fleet.OrderMessage
	if err := json.Unmarshal([]byte(me.ErrorsJSON), &errs); err != nil {
		t.Fatalf("decode ErrorsJSON: %v", err)
	}
	if len(errs) != 1 || errs[0].Code != 42 {
		t.Errorf("ErrorsJSON content = %+v", errs)
	}

	// Non-terminal → no mission_telemetry summary row.
	if mt, err := db.GetMissionTelemetry(order.ID); err == nil {
		t.Errorf("expected no telemetry row for non-terminal state, got %+v", mt)
	}
}

// TestRecordMissionEvent_DefaultsToEmptyJSONWithoutSnapshot — when no
// Snapshot is supplied, the function must still write the row, with
// the JSON columns falling back to the "[]" defaults.
func TestRecordMissionEvent_DefaultsToEmptyJSONWithoutSnapshot(t *testing.T) {
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	order := makeTelemetryOrder(t, db, "tel-2", "station-tel-2")

	eng.recordMissionEvent(OrderStatusChangedEvent{
		OrderID:       order.ID,
		VendorOrderID: order.VendorOrderID,
		OldStatus:     "RUNNING",
		NewStatus:     "WAITING",
		RobotID:       "",
		Snapshot:      nil,
	})

	events, _ := db.ListMissionEvents(order.ID)
	if len(events) != 1 {
		t.Fatalf("mission_events = %d, want 1", len(events))
	}
	if events[0].BlocksJSON != "[]" || events[0].ErrorsJSON != "[]" {
		t.Errorf("expected default empty arrays, got blocks=%q errors=%q",
			events[0].BlocksJSON, events[0].ErrorsJSON)
	}
}

// ── finalizeMissionTelemetry (via terminal recordMissionEvent) ──────

// TestRecordMissionEvent_TerminalStateWritesTelemetrySummary proves that
// when the new status is one the simulator marks terminal (FINISHED),
// finalizeMissionTelemetry runs and a mission_telemetry row is upserted
// with vendor durations + snapshot JSON populated.
func TestRecordMissionEvent_TerminalStateWritesTelemetrySummary(t *testing.T) {
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	order := makeTelemetryOrder(t, db, "tel-term", "station-term")

	// Vendor times in ms epoch → should land as VendorCreated/Completed.
	vendorCreate := time.Now().Add(-2 * time.Minute).UnixMilli()
	vendorEnd := time.Now().UnixMilli()
	snap := &fleet.OrderSnapshot{
		VendorOrderID: order.VendorOrderID,
		State:         "FINISHED",
		CreateTime:    vendorCreate,
		TerminalTime:  vendorEnd,
		Blocks:        []fleet.BlockSnapshot{{BlockID: "B-final", State: "DONE"}},
		Warnings:      []fleet.OrderMessage{{Code: 7, Desc: "slowdown"}},
	}

	eng.recordMissionEvent(OrderStatusChangedEvent{
		OrderID:       order.ID,
		VendorOrderID: order.VendorOrderID,
		OldStatus:     "RUNNING",
		NewStatus:     "FINISHED", // terminal in simulator
		RobotID:       "AMR-Y",
		Snapshot:      snap,
	})

	// Mission event row.
	events, _ := db.ListMissionEvents(order.ID)
	if len(events) != 1 {
		t.Fatalf("mission_events = %d, want 1", len(events))
	}

	// Telemetry summary row.
	mt, err := db.GetMissionTelemetry(order.ID)
	if err != nil {
		t.Fatalf("GetMissionTelemetry: %v", err)
	}
	if mt.OrderID != order.ID {
		t.Errorf("OrderID = %d, want %d", mt.OrderID, order.ID)
	}
	if mt.RobotID != "AMR-Y" {
		t.Errorf("RobotID = %q, want AMR-Y", mt.RobotID)
	}
	if mt.TerminalState != "FINISHED" {
		t.Errorf("TerminalState = %q, want FINISHED", mt.TerminalState)
	}
	if mt.StationID != order.StationID {
		t.Errorf("StationID = %q, want %q (hydrated from order row)", mt.StationID, order.StationID)
	}
	if mt.SourceNode != order.SourceNode || mt.DeliveryNode != order.DeliveryNode {
		t.Errorf("source/delivery = %s/%s, want %s/%s",
			mt.SourceNode, mt.DeliveryNode, order.SourceNode, order.DeliveryNode)
	}
	if mt.DurationMS <= 0 {
		t.Errorf("DurationMS = %d, want > 0", mt.DurationMS)
	}
	if mt.VendorDurationMS <= 0 {
		t.Errorf("VendorDurationMS = %d, want > 0", mt.VendorDurationMS)
	}
	if mt.VendorCreated == nil || mt.VendorCompleted == nil {
		t.Errorf("vendor created/completed = %v/%v", mt.VendorCreated, mt.VendorCompleted)
	}
	if mt.CoreCreated == nil || mt.CoreCompleted == nil {
		t.Errorf("core created/completed = %v/%v", mt.CoreCreated, mt.CoreCompleted)
	}

	// Snapshot JSON columns rolled into the summary.
	var blocks []fleet.BlockSnapshot
	if err := json.Unmarshal([]byte(mt.BlocksJSON), &blocks); err != nil {
		t.Fatalf("decode BlocksJSON: %v", err)
	}
	if len(blocks) != 1 || blocks[0].BlockID != "B-final" {
		t.Errorf("blocks summary = %+v", blocks)
	}
	var warns []fleet.OrderMessage
	if err := json.Unmarshal([]byte(mt.WarningsJSON), &warns); err != nil {
		t.Fatalf("decode WarningsJSON: %v", err)
	}
	if len(warns) != 1 || warns[0].Code != 7 {
		t.Errorf("warnings summary = %+v", warns)
	}
}

// TestFinalizeMissionTelemetry_MissingOrderIsNoOp — when the order can't
// be loaded, the helper logs and returns without panicking, and crucially
// does NOT write a telemetry row.
func TestFinalizeMissionTelemetry_MissingOrderIsNoOp(t *testing.T) {
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	const missingID = 88888888
	eng.finalizeMissionTelemetry(OrderStatusChangedEvent{
		OrderID:       missingID,
		VendorOrderID: "v-missing",
		NewStatus:     "FINISHED",
	})

	if mt, err := db.GetMissionTelemetry(missingID); err == nil {
		t.Errorf("expected no telemetry row for missing order, got %+v", mt)
	}
}

// TestFinalizeMissionTelemetry_HandlesNilSnapshot — ev.Snapshot==nil is
// allowed; the row is still written with default empty JSON arrays and
// duration computed from CoreCreated/CoreCompleted only.
func TestFinalizeMissionTelemetry_HandlesNilSnapshot(t *testing.T) {
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	order := makeTelemetryOrder(t, db, "tel-nilsnap", "station-nilsnap")

	eng.finalizeMissionTelemetry(OrderStatusChangedEvent{
		OrderID:       order.ID,
		VendorOrderID: order.VendorOrderID,
		NewStatus:     "FINISHED",
		RobotID:       "AMR-Z",
		Snapshot:      nil,
	})

	mt, err := db.GetMissionTelemetry(order.ID)
	if err != nil {
		t.Fatalf("GetMissionTelemetry: %v", err)
	}
	if mt.RobotID != "AMR-Z" {
		t.Errorf("RobotID = %q, want AMR-Z", mt.RobotID)
	}
	if mt.BlocksJSON != "[]" || mt.WarningsJSON != "[]" {
		t.Errorf("expected default empty arrays for nil snapshot: blocks=%q warnings=%q",
			mt.BlocksJSON, mt.WarningsJSON)
	}
	if mt.VendorDurationMS != 0 {
		t.Errorf("VendorDurationMS = %d, want 0 (no snapshot)", mt.VendorDurationMS)
	}
}
