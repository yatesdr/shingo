//go:build docker

package telemetry_test

import (
	"testing"
	"time"

	"shingocore/internal/testdb"
	"shingocore/store/telemetry"
)

func TestCoverage_InsertAndListMissionEvents(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	orderID := int64(100)
	e1 := &telemetry.Event{OrderID: orderID, VendorOrderID: "rds-100", OldState: "CREATED", NewState: "RUNNING", RobotID: "AMB-1", RobotStation: "LINE-1", BlocksJSON: "[]", ErrorsJSON: "[]", Detail: "dispatched"}
	if err := telemetry.InsertEvent(db.DB, e1); err != nil { t.Fatalf("InsertEvent 1: %v", err) }
	e2 := &telemetry.Event{OrderID: orderID, VendorOrderID: "rds-100", OldState: "RUNNING", NewState: "FINISHED", RobotID: "AMB-1", RobotStation: "LINE-1", BlocksJSON: "[]", ErrorsJSON: "[]"}
	if err := telemetry.InsertEvent(db.DB, e2); err != nil { t.Fatalf("InsertEvent 2: %v", err) }
	eOther := &telemetry.Event{OrderID: 200, OldState: "CREATED", NewState: "RUNNING", BlocksJSON: "[]", ErrorsJSON: "[]"}
	telemetry.InsertEvent(db.DB, eOther)
	events, err := telemetry.ListEvents(db.DB, orderID)
	if err != nil { t.Fatalf("ListEvents: %v", err) }
	if len(events) != 2 { t.Fatalf("len = %d, want 2", len(events)) }
	for _, e := range events { if e.OrderID != orderID { t.Errorf("event OrderID = %d, want %d (filter leaked)", e.OrderID, orderID) } }
	if events[0].NewState != "RUNNING" { t.Errorf("events[0].NewState = %q, want RUNNING", events[0].NewState) }
	if events[1].NewState != "FINISHED" { t.Errorf("events[1].NewState = %q, want FINISHED", events[1].NewState) }
}

func TestCoverage_UpsertAndGetMissionTelemetry(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	created := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	completed := time.Date(2025, 1, 1, 10, 5, 0, 0, time.UTC)
	mt := &telemetry.Mission{OrderID: 500, VendorOrderID: "rds-500", RobotID: "AMB-1", StationID: "STN-1", OrderType: "retrieve", SourceNode: "SRC", DeliveryNode: "DST", TerminalState: "FINISHED", CoreCreated: &created, CoreCompleted: &completed, DurationMS: 300000, BlocksJSON: "[]", ErrorsJSON: "[]", WarningsJSON: "[]", NoticesJSON: "[]"}
	if err := telemetry.UpsertMission(db.DB, mt); err != nil { t.Fatalf("UpsertMission (insert): %v", err) }
	got, err := telemetry.GetMission(db.DB, 500)
	if err != nil { t.Fatalf("GetMission: %v", err) }
	if got.OrderID != 500 { t.Errorf("OrderID = %d, want 500", got.OrderID) }
	if got.TerminalState != "FINISHED" { t.Errorf("TerminalState = %q, want FINISHED", got.TerminalState) }
	if got.DurationMS != 300000 { t.Errorf("DurationMS = %d, want 300000", got.DurationMS) }
	if got.RobotID != "AMB-1" { t.Errorf("RobotID = %q", got.RobotID) }
	mt.TerminalState = "FAILED"; mt.DurationMS = 450000; mt.RobotID = "AMB-2"
	if err := telemetry.UpsertMission(db.DB, mt); err != nil { t.Fatalf("UpsertMission (update): %v", err) }
	updated, err := telemetry.GetMission(db.DB, 500)
	if err != nil { t.Fatalf("GetMission (after update): %v", err) }
	if updated.TerminalState != "FAILED" { t.Errorf("TerminalState after upsert = %q, want FAILED", updated.TerminalState) }
	if updated.DurationMS != 450000 { t.Errorf("DurationMS after upsert = %d, want 450000", updated.DurationMS) }
	if updated.RobotID != "AMB-2" { t.Errorf("RobotID after upsert = %q, want AMB-2", updated.RobotID) }
	if updated.ID != got.ID { t.Errorf("ID changed across upsert: before=%d after=%d", got.ID, updated.ID) }
}

func TestCoverage_ListMissionsFilter(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	times := []time.Time{time.Date(2025,1,1,10,0,0,0,time.UTC), time.Date(2025,2,1,10,0,0,0,time.UTC), time.Date(2025,3,1,10,0,0,0,time.UTC), time.Date(2025,4,1,10,0,0,0,time.UTC)}
	rows := []*telemetry.Mission{
		{OrderID:1001, StationID:"STN-1", TerminalState:"FINISHED", DurationMS:1000, CoreCompleted:&times[0], BlocksJSON:"[]", ErrorsJSON:"[]", WarningsJSON:"[]", NoticesJSON:"[]"},
		{OrderID:1002, StationID:"STN-1", TerminalState:"FAILED", DurationMS:2000, CoreCompleted:&times[1], BlocksJSON:"[]", ErrorsJSON:"[]", WarningsJSON:"[]", NoticesJSON:"[]"},
		{OrderID:1003, StationID:"STN-2", TerminalState:"FINISHED", DurationMS:3000, CoreCompleted:&times[2], BlocksJSON:"[]", ErrorsJSON:"[]", WarningsJSON:"[]", NoticesJSON:"[]"},
		{OrderID:1004, StationID:"STN-1", TerminalState:"FINISHED", DurationMS:4000, CoreCompleted:&times[3], BlocksJSON:"[]", ErrorsJSON:"[]", WarningsJSON:"[]", NoticesJSON:"[]"},
	}
	for _, r := range rows { if err := telemetry.UpsertMission(db.DB, r); err != nil { t.Fatalf("insert row %d: %v", r.OrderID, err) } }
	list1, total1, err := telemetry.ListMissions(db.DB, telemetry.Filter{StationID: "STN-1", Limit: 50})
	if err != nil { t.Fatalf("ListMissions STN-1: %v", err) }
	if total1 != 3 { t.Errorf("STN-1 total = %d, want 3", total1) }
	if len(list1) != 3 { t.Errorf("STN-1 returned = %d, want 3", len(list1)) }
	since := time.Date(2025,2,1,0,0,0,0,time.UTC)
	until := time.Date(2025,4,1,23,0,0,0,time.UTC)
	list2, total2, err := telemetry.ListMissions(db.DB, telemetry.Filter{Since: &since, Until: &until, Limit: 50})
	if err != nil { t.Fatalf("ListMissions date-range: %v", err) }
	if total2 != 3 { t.Errorf("date-range total = %d, want 3", total2) }
	if len(list2) != 3 { t.Errorf("date-range returned = %d, want 3", len(list2)) }
	list3, total3, err := telemetry.ListMissions(db.DB, telemetry.Filter{StationID: "STN-1", Since: &since, Until: &until, Limit: 50})
	if err != nil { t.Fatalf("ListMissions combined: %v", err) }
	if total3 != 2 { t.Errorf("combined total = %d, want 2", total3) }
	if len(list3) != 2 { t.Errorf("combined returned = %d, want 2", len(list3)) }
}

func TestCoverage_GetMissionStats(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	rows := []*telemetry.Mission{
		{OrderID:2001, StationID:"S", TerminalState:"FINISHED", DurationMS:1000, BlocksJSON:"[]", ErrorsJSON:"[]", WarningsJSON:"[]", NoticesJSON:"[]"},
		{OrderID:2002, StationID:"S", TerminalState:"FINISHED", DurationMS:3000, BlocksJSON:"[]", ErrorsJSON:"[]", WarningsJSON:"[]", NoticesJSON:"[]"},
		{OrderID:2003, StationID:"S", TerminalState:"FAILED", DurationMS:2000, BlocksJSON:"[]", ErrorsJSON:"[]", WarningsJSON:"[]", NoticesJSON:"[]"},
	{OrderID:2004, StationID:"S", TerminalState:"STOPPED", DurationMS:500, BlocksJSON:"[]", ErrorsJSON:"[]", WarningsJSON:"[]", NoticesJSON:"[]"},
	}
	for _, r := range rows { telemetry.UpsertMission(db.DB, r) }
	stats, err := telemetry.GetStats(db.DB, telemetry.Filter{})
	if err != nil { t.Fatalf("GetStats: %v", err) }
	if stats.TotalMissions != 4 { t.Errorf("TotalMissions = %d, want 4", stats.TotalMissions) }
	if stats.Completed != 2 { t.Errorf("Completed = %d, want 2", stats.Completed) }
	if stats.Failed != 1 { t.Errorf("Failed = %d, want 1", stats.Failed) }
	if stats.Cancelled != 1 { t.Errorf("Cancelled = %d, want 1", stats.Cancelled) }
	if stats.SuccessRate != 50.0 { t.Errorf("SuccessRate = %v, want 50", stats.SuccessRate) }
	if stats.AvgDurationMS != 1625 { t.Errorf("AvgDurationMS = %d, want 1625", stats.AvgDurationMS) }
	if stats.P50DurationMS < 1000 || stats.P50DurationMS > 2000 { t.Errorf("P50DurationMS = %d, want in [1000,2000]", stats.P50DurationMS) }
	if stats.P95DurationMS < 2000 || stats.P95DurationMS > 3000 { t.Errorf("P95DurationMS = %d, want in [2000,3000]", stats.P95DurationMS) }
}

func TestCoverage_GetMissionStats_EmptyPopulation(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	stats, err := telemetry.GetStats(db.DB, telemetry.Filter{StationID: "NOBODY"})
	if err != nil { t.Fatalf("GetStats (empty): %v", err) }
	if stats.TotalMissions != 0 { t.Errorf("empty TotalMissions = %d, want 0", stats.TotalMissions) }
	if stats.SuccessRate != 0 { t.Errorf("empty SuccessRate = %v, want 0", stats.SuccessRate) }
}
