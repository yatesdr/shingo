//go:build docker

package service

import (
	"testing"

	"shingocore/store"
)

// seedMission inserts a mission_telemetry row (via UpsertMissionTelemetry)
// and returns it. The underlying row is order-keyed; callers supply a
// unique orderID per test row.
//
// BlocksJSON/ErrorsJSON/WarningsJSON/NoticesJSON must be valid JSON — the
// underlying columns are jsonb, so empty string is rejected. We seed "[]"
// for consistency with store/telemetry_test.go.
func seedMission(t *testing.T, db *store.DB, orderID int64, station, robot, state string) *store.MissionTelemetry {
	t.Helper()
	tel := &store.MissionTelemetry{
		OrderID:       orderID,
		VendorOrderID: "vendor-" + station,
		RobotID:       robot,
		StationID:     station,
		OrderType:     "delivery",
		TerminalState: state,
		DurationMS:    1000,
		BlocksJSON:    "[]",
		ErrorsJSON:    "[]",
		WarningsJSON:  "[]",
		NoticesJSON:   "[]",
	}
	if err := db.UpsertMissionTelemetry(tel); err != nil {
		t.Fatalf("UpsertMissionTelemetry: %v", err)
	}
	return tel
}

func TestMissionService_Telemetry_RoundTripsRow(t *testing.T) {
	db := testDB(t)
	svc := NewMissionService(db)

	seed := seedMission(t, db, 101, "STN-A", "R1", "FINISHED")

	got, err := svc.Telemetry(101)
	if err != nil {
		t.Fatalf("Telemetry: %v", err)
	}
	if got.OrderID != seed.OrderID || got.StationID != "STN-A" || got.RobotID != "R1" {
		t.Errorf("Telemetry = %+v, want order=%d station=STN-A robot=R1", got, seed.OrderID)
	}
}

func TestMissionService_ListEvents_ReturnsInserted(t *testing.T) {
	db := testDB(t)
	svc := NewMissionService(db)

	// Seed two events for the same order and one for a different order.
	// BlocksJSON/ErrorsJSON columns are jsonb — empty strings are rejected,
	// so seed "[]".
	for _, state := range []string{"started", "moving"} {
		e := &store.MissionEvent{
			OrderID:       55,
			VendorOrderID: "v55",
			OldState:      "",
			NewState:      state,
			RobotID:       "R1",
			BlocksJSON:    "[]",
			ErrorsJSON:    "[]",
		}
		if err := db.InsertMissionEvent(e); err != nil {
			t.Fatalf("InsertMissionEvent: %v", err)
		}
	}
	other := &store.MissionEvent{
		OrderID:       56,
		VendorOrderID: "v56",
		NewState:      "x",
		BlocksJSON:    "[]",
		ErrorsJSON:    "[]",
	}
	if err := db.InsertMissionEvent(other); err != nil {
		t.Fatalf("InsertMissionEvent (other): %v", err)
	}

	events, err := svc.ListEvents(55)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("len(events) = %d, want 2", len(events))
	}
	for _, e := range events {
		if e.OrderID != 55 {
			t.Errorf("event order = %d, want 55", e.OrderID)
		}
	}
}

func TestMissionService_List_CountsMatchFilter(t *testing.T) {
	db := testDB(t)
	svc := NewMissionService(db)

	seedMission(t, db, 201, "STN-A", "R1", "FINISHED")
	seedMission(t, db, 202, "STN-A", "R2", "FAILED")
	seedMission(t, db, 203, "STN-B", "R1", "FINISHED")

	// No filter — 3 rows.
	rows, total, err := svc.List(store.MissionFilter{})
	if err != nil {
		t.Fatalf("List (all): %v", err)
	}
	if total != 3 || len(rows) != 3 {
		t.Errorf("all: total=%d rows=%d, want 3/3", total, len(rows))
	}

	// Station filter — 2 rows at STN-A.
	rows, total, err = svc.List(store.MissionFilter{StationID: "STN-A"})
	if err != nil {
		t.Fatalf("List (station): %v", err)
	}
	if total != 2 || len(rows) != 2 {
		t.Errorf("STN-A: total=%d rows=%d, want 2/2", total, len(rows))
	}
}

func TestMissionService_Stats_CountsCompletionsAndFailures(t *testing.T) {
	db := testDB(t)
	svc := NewMissionService(db)

	seedMission(t, db, 301, "STN-S", "R", "FINISHED")
	seedMission(t, db, 302, "STN-S", "R", "FINISHED")
	seedMission(t, db, 303, "STN-S", "R", "FAILED")
	seedMission(t, db, 304, "STN-S", "R", "STOPPED")

	s, err := svc.Stats(store.MissionFilter{StationID: "STN-S"})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if s.TotalMissions != 4 {
		t.Errorf("TotalMissions = %d, want 4", s.TotalMissions)
	}
	if s.Completed != 2 {
		t.Errorf("Completed = %d, want 2", s.Completed)
	}
	if s.Failed != 1 {
		t.Errorf("Failed = %d, want 1", s.Failed)
	}
	if s.Cancelled != 1 {
		t.Errorf("Cancelled = %d, want 1", s.Cancelled)
	}
}
