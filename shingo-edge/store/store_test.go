package store

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingoedge/store/catalog"
	"shingoedge/store/orders"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

// coverageDB opens a fresh in-tempdir SQLite file and returns a migrated DB.
// A local helper (rather than reusing testDB from outbox_test.go, which is
// gated behind //go:build docker) so these tests run under the default build.
func coverageDB(t *testing.T) *DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "cov.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open cov db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// seedProcessStyle inserts a process + style and returns their ids.
// Used throughout this file where a minimal parent hierarchy is needed.
func seedProcessStyle(t *testing.T, db *DB, procName, styleName string) (int64, int64) {
	t.Helper()
	pid, err := db.CreateProcess(procName, "desc", "active_production", "", "", false, false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	sid, err := db.CreateStyle(styleName, "style desc", pid)
	if err != nil {
		t.Fatalf("create style: %v", err)
	}
	return pid, sid
}

// ============================================================================
// admin_users.go
// ============================================================================

func TestAdminUsers_CreateGetUpdateExists(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)

	exists, err := db.AdminUserExists()
	if err != nil {
		t.Fatalf("exists: %v", err)
	}
	if exists {
		t.Fatal("expected no admin users in fresh DB")
	}

	id, err := db.CreateAdminUser("alice", "hash-v1")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if id == 0 {
		t.Fatal("expected nonzero id")
	}

	exists, err = db.AdminUserExists()
	if err != nil || !exists {
		t.Fatalf("exists after create: %v exists=%v", err, exists)
	}

	got, err := db.GetAdminUser("alice")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Username != "alice" || got.PasswordHash != "hash-v1" {
		t.Errorf("got %+v", got)
	}
	if got.CreatedAt.IsZero() {
		t.Error("expected non-zero created_at")
	}

	testutil.MustNoErr(t, db.UpdateAdminPassword("alice", "hash-v2"), "update")
	got2, err := db.GetAdminUser("alice")
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if got2.PasswordHash != "hash-v2" {
		t.Errorf("password hash = %q, want hash-v2", got2.PasswordHash)
	}
}

func TestAdminUsers_GetMissingReturnsError(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	if _, err := db.GetAdminUser("ghost"); err == nil {
		t.Fatal("expected error for missing user")
	}
}

// ============================================================================
// processes.go
// ============================================================================

func TestProcesses_CreateListGetUpdateDelete(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)

	id, err := db.CreateProcess("LINE-A", "main line", "active_production", "PLC1", "TAG1", true, false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	list, err := db.ListProcesses()
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v len=%d", err, len(list))
	}
	if list[0].Name != "LINE-A" || !list[0].CounterEnabled {
		t.Errorf("got %+v", list[0])
	}

	got, err := db.GetProcess(id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.CounterPLCName != "PLC1" || got.CounterTagName != "TAG1" {
		t.Errorf("counter fields wrong: %+v", got)
	}

	testutil.MustNoErr(t, db.UpdateProcess(id, "LINE-A-v2", "updated", "", "PLC2", "TAG2", false, false), "update")
	got2, _ := db.GetProcess(id)
	if got2.Name != "LINE-A-v2" {
		t.Errorf("name after update = %q", got2.Name)
	}
	// UpdateProcess with empty productionState should default to active_production.
	if got2.ProductionState != "active_production" {
		t.Errorf("productionState default = %q, want active_production", got2.ProductionState)
	}
	if got2.CounterEnabled {
		t.Error("expected counter_enabled=false after update")
	}

	testutil.MustNoErr(t, db.DeleteProcess(id), "delete")
	rest, _ := db.ListProcesses()
	if len(rest) != 0 {
		t.Errorf("list after delete = %d, want 0", len(rest))
	}
}

func TestProcesses_CreateDefaultsProductionState(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	id, err := db.CreateProcess("DEF", "", "", "", "", false, false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, _ := db.GetProcess(id)
	if got.ProductionState != "active_production" {
		t.Errorf("default productionState = %q, want active_production", got.ProductionState)
	}
}

func TestProcesses_ActiveAndTargetStyle(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	pid, sid := seedProcessStyle(t, db, "P", "S1")

	// No active style initially.
	active, err := db.GetActiveStyleID(pid)
	if err != nil {
		t.Fatalf("get active: %v", err)
	}
	if active != nil {
		t.Errorf("active style = %v, want nil", active)
	}

	testutil.MustNoErr(t, db.SetActiveStyle(pid, &sid), "set active")
	active, _ = db.GetActiveStyleID(pid)
	if active == nil || *active != sid {
		t.Errorf("active style = %v, want %d", active, sid)
	}

	// Set target style.
	sid2, _ := db.CreateStyle("S2", "", pid)
	testutil.MustNoErr(t, db.SetTargetStyle(pid, &sid2), "set target")
	got, _ := db.GetProcess(pid)
	if got.TargetStyleID == nil || *got.TargetStyleID != sid2 {
		t.Errorf("target style = %v, want %d", got.TargetStyleID, sid2)
	}

	// Clear active by nil.
	testutil.MustNoErr(t, db.SetActiveStyle(pid, nil), "clear active")
	active, _ = db.GetActiveStyleID(pid)
	if active != nil {
		t.Errorf("active after nil = %v", active)
	}
}

func TestProcesses_SetProductionState(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	id, err := db.CreateProcess("P", "", "", "", "", false, false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	testutil.MustNoErr(t, db.SetProcessProductionState(id, "changeover_active"), "set state")
	got, _ := db.GetProcess(id)
	if got.ProductionState != "changeover_active" {
		t.Errorf("state = %q, want changeover_active", got.ProductionState)
	}
}

// ============================================================================
// styles.go
// ============================================================================

func TestStyles_CRUDAndListing(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	pid, err := db.CreateProcess("P", "", "", "", "", false, false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}

	s1, err := db.CreateStyle("WIDGET-A", "first", pid)
	if err != nil {
		t.Fatalf("create s1: %v", err)
	}
	s2, err := db.CreateStyle("WIDGET-B", "second", pid)
	if err != nil {
		t.Fatalf("create s2: %v", err)
	}

	all, err := db.ListStyles()
	if err != nil || len(all) != 2 {
		t.Fatalf("list all: %v len=%d", err, len(all))
	}

	byProc, err := db.ListStylesByProcess(pid)
	if err != nil || len(byProc) != 2 {
		t.Fatalf("list by process: %v len=%d", err, len(byProc))
	}

	got, err := db.GetStyle(s1)
	if err != nil || got.Name != "WIDGET-A" {
		t.Errorf("get by id: %v name=%q", err, got.Name)
	}

	gotByName, err := db.GetStyleByName("WIDGET-B")
	if err != nil || gotByName.ID != s2 {
		t.Errorf("get by name: %v id=%d", err, gotByName.ID)
	}

	testutil.MustNoErr(t, db.UpdateStyle(s1, "WIDGET-A-v2", "renamed", pid), "update")
	got2, _ := db.GetStyle(s1)
	if got2.Name != "WIDGET-A-v2" {
		t.Errorf("name after update = %q", got2.Name)
	}

	testutil.MustNoErr(t, db.DeleteStyle(s2), "delete")
	after, _ := db.ListStyles()
	if len(after) != 1 {
		t.Errorf("list after delete = %d, want 1", len(after))
	}
}

func TestStyles_GetMissingReturnsError(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	if _, err := db.GetStyle(9999); err == nil {
		t.Fatal("expected error for missing id")
	}
	if _, err := db.GetStyleByName("missing"); err == nil {
		t.Fatal("expected error for missing name")
	}
}

// ============================================================================
// shifts.go
// ============================================================================

func TestShifts_UpsertListDelete(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)

	testutil.MustNoErr(t, db.UpsertShift(1, "Day", "06:00", "14:00"), "upsert 1")
	testutil.MustNoErr(t, db.UpsertShift(2, "Swing", "14:00", "22:00"), "upsert 2")

	list, err := db.ListShifts()
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %v len=%d", err, len(list))
	}
	if list[0].ShiftNumber != 1 || list[0].Name != "Day" {
		t.Errorf("got[0] = %+v", list[0])
	}

	// Upsert conflict -> updates name/start/end, keeps id
	testutil.MustNoErr(t, db.UpsertShift(1, "Day Updated", "07:00", "15:00"), "upsert update")
	list2, _ := db.ListShifts()
	if list2[0].Name != "Day Updated" || list2[0].StartTime != "07:00" {
		t.Errorf("after upsert-update: %+v", list2[0])
	}

	testutil.MustNoErr(t, db.DeleteShift(1), "delete")
	list3, _ := db.ListShifts()
	if len(list3) != 1 || list3[0].ShiftNumber != 2 {
		t.Errorf("after delete: %+v", list3)
	}
}

// ============================================================================
// payload_catalog.go
// ============================================================================

func TestPayloadCatalog_UpsertListGet(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)

	entry := &catalog.CatalogEntry{
		ID:          10,
		Name:        "Widget Tote",
		Code:        "WT-10",
		Description: "10-unit tote",
		UOPCapacity: 10,
	}
	testutil.MustNoErr(t, db.UpsertPayloadCatalog(entry), "upsert")

	list, err := db.ListPayloadCatalog()
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v len=%d", err, len(list))
	}
	if list[0].Code != "WT-10" {
		t.Errorf("code = %q", list[0].Code)
	}

	byCode, err := db.GetPayloadCatalogByCode("WT-10")
	if err != nil || byCode.ID != 10 || byCode.UOPCapacity != 10 {
		t.Errorf("get: %v %+v", err, byCode)
	}
	if byCode.UpdatedAt.IsZero() {
		t.Error("updated_at should be populated")
	}

	// Upsert conflict — same ID updates the fields in place.
	entry.Description = "10-unit tote (v2)"
	entry.UOPCapacity = 12
	testutil.MustNoErr(t, db.UpsertPayloadCatalog(entry), "upsert-update")
	byCode2, _ := db.GetPayloadCatalogByCode("WT-10")
	if byCode2.UOPCapacity != 12 || !strings.Contains(byCode2.Description, "v2") {
		t.Errorf("after upsert-update: %+v", byCode2)
	}
}

func TestPayloadCatalog_DeleteStale(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	for _, id := range []int64{1, 2, 3} {
		e := &catalog.CatalogEntry{ID: id, Name: "E", Code: "C"}
		testutil.MustNoErr(t, db.UpsertPayloadCatalog(e), "upsert")
	}

	// Empty activeIDs is a no-op (safety).
	testutil.MustNoErr(t, db.DeleteStalePayloadCatalogEntries(nil), "delete stale (nil)")
	list, _ := db.ListPayloadCatalog()
	if len(list) != 3 {
		t.Fatalf("len after no-op delete = %d, want 3", len(list))
	}

	// Keep only id=2.
	testutil.MustNoErr(t, db.DeleteStalePayloadCatalogEntries([]int64{2}), "delete stale")
	list2, _ := db.ListPayloadCatalog()
	if len(list2) != 1 || list2[0].ID != 2 {
		t.Errorf("after delete stale: %+v", list2)
	}
}

func TestPayloadCatalog_GetMissingReturnsError(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	if _, err := db.GetPayloadCatalogByCode("none"); err == nil {
		t.Fatal("expected error for missing code")
	}
}

// ============================================================================
// reporting_points.go
// ============================================================================

func TestReportingPoints_CRUD(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	_, sid := seedProcessStyle(t, db, "P", "S1")

	id, err := db.CreateReportingPoint("PLC1", "COUNTER_A", sid)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	list, err := db.ListReportingPoints()
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v len=%d", err, len(list))
	}
	if list[0].PLCName != "PLC1" || list[0].TagName != "COUNTER_A" {
		t.Errorf("got %+v", list[0])
	}
	// Default created with enabled=false per schema; confirm fields.
	if list[0].LastCount != 0 {
		t.Errorf("default last_count = %d", list[0].LastCount)
	}

	got, err := db.GetReportingPoint(id)
	if err != nil || got.ID != id {
		t.Errorf("get: %v %+v", err, got)
	}

	// Enable + rename + change style
	_, sid2 := seedProcessStyle(t, db, "P2", "S2")
	testutil.MustNoErr(t, db.UpdateReportingPoint(id, "PLC2", "COUNTER_B", sid2, true), "update")
	got2, _ := db.GetReportingPoint(id)
	if !got2.Enabled || got2.PLCName != "PLC2" || got2.TagName != "COUNTER_B" || got2.StyleID != sid2 {
		t.Errorf("after update: %+v", got2)
	}

	testutil.MustNoErr(t, db.DeleteReportingPoint(id), "delete")
	after, _ := db.ListReportingPoints()
	if len(after) != 0 {
		t.Errorf("after delete: %d", len(after))
	}
}

func TestReportingPoints_UpdateCounter(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	_, sid := seedProcessStyle(t, db, "P", "S")

	id, _ := db.CreateReportingPoint("PLC", "TAG", sid)
	testutil.MustNoErr(t, db.UpdateReportingPointCounter(id, 42), "update counter")
	got, _ := db.GetReportingPoint(id)
	if got.LastCount != 42 {
		t.Errorf("last_count = %d, want 42", got.LastCount)
	}
	if got.LastPollAt == nil {
		t.Error("last_poll_at should be populated after counter update")
	}
}

func TestReportingPoints_SetManaged(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	_, sid := seedProcessStyle(t, db, "P", "S")
	id, _ := db.CreateReportingPoint("PLC", "TAG", sid)

	testutil.MustNoErr(t, db.SetReportingPointManaged(id, true), "set managed")
	got, _ := db.GetReportingPoint(id)
	if !got.WarlinkManaged {
		t.Error("expected warlink_managed=true")
	}

	testutil.MustNoErr(t, db.SetReportingPointManaged(id, false), "set managed false")
	got2, _ := db.GetReportingPoint(id)
	if got2.WarlinkManaged {
		t.Error("expected warlink_managed=false")
	}
}

func TestReportingPoints_ListEnabledAndLookups(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	_, sid := seedProcessStyle(t, db, "P", "S")

	id1, _ := db.CreateReportingPoint("PLC1", "T1", sid)
	id2, _ := db.CreateReportingPoint("PLC2", "T2", sid)

	// Schema default for `enabled` is 1, so both rows start enabled.
	// Disable id2 to verify ListEnabledReportingPoints actually filters.
	testutil.MustNoErr(t, db.UpdateReportingPoint(id2, "PLC2", "T2", sid, false), "disable id2")

	enabled, err := db.ListEnabledReportingPoints()
	if err != nil {
		t.Fatalf("list enabled: %v", err)
	}
	if len(enabled) != 1 || enabled[0].ID != id1 {
		t.Errorf("enabled = %+v, want only id1=%d", enabled, id1)
	}
	if enabled[0].ProcessID == 0 {
		t.Error("expected ProcessID joined through style")
	}

	byTag, err := db.GetReportingPointByTag("PLC2", "T2")
	if err != nil || byTag.ID != id2 {
		t.Errorf("by tag: %v %+v", err, byTag)
	}

	byStyle, err := db.GetReportingPointByStyleID(sid)
	if err != nil || byStyle == nil {
		t.Errorf("by style: %v %+v", err, byStyle)
	}
}

func TestReportingPoints_LookupMissingReturnsError(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	if _, err := db.GetReportingPoint(9999); err == nil {
		t.Fatal("expected error")
	}
	if _, err := db.GetReportingPointByTag("x", "y"); err == nil {
		t.Fatal("expected error")
	}
	if _, err := db.GetReportingPointByStyleID(9999); err == nil {
		t.Fatal("expected error")
	}
}

// ============================================================================
// counter_snapshots.go
// ============================================================================

func TestCounterSnapshots_InsertListConfirmDismiss(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	_, sid := seedProcessStyle(t, db, "P", "S")
	rpID, _ := db.CreateReportingPoint("PLC", "TAG", sid)

	// Non-jump snapshot (no anomaly) — should not appear in ListUnconfirmedAnomalies.
	if _, err := db.InsertCounterSnapshot(rpID, 100, 10, "", false); err != nil {
		t.Fatalf("insert clean: %v", err)
	}

	// Jump anomaly, unconfirmed.
	anomalyID, err := db.InsertCounterSnapshot(rpID, 200, 100, "jump", false)
	if err != nil {
		t.Fatalf("insert anomaly: %v", err)
	}

	list, err := db.ListUnconfirmedAnomalies()
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v len=%d", err, len(list))
	}
	if list[0].ID != anomalyID {
		t.Errorf("anomaly id = %d, want %d", list[0].ID, anomalyID)
	}
	if list[0].Anomaly == nil || *list[0].Anomaly != "jump" {
		t.Errorf("anomaly field = %v", list[0].Anomaly)
	}

	// Confirming the anomaly removes it from the unconfirmed list.
	testutil.MustNoErr(t, db.ConfirmAnomaly(anomalyID), "confirm")
	list2, _ := db.ListUnconfirmedAnomalies()
	if len(list2) != 0 {
		t.Errorf("after confirm: %d", len(list2))
	}

	// Dismissing a second anomaly deletes it.
	dismissID, _ := db.InsertCounterSnapshot(rpID, 300, 100, "jump", false)
	testutil.MustNoErr(t, db.DismissAnomaly(dismissID), "dismiss")
	list3, _ := db.ListUnconfirmedAnomalies()
	if len(list3) != 0 {
		t.Errorf("after dismiss: %d", len(list3))
	}
}

func TestCounterSnapshots_DismissOnlyUnconfirmedJump(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	_, sid := seedProcessStyle(t, db, "P", "S")
	rpID, _ := db.CreateReportingPoint("PLC", "TAG", sid)

	// Insert, then confirm — dismiss should not delete a confirmed row.
	id, _ := db.InsertCounterSnapshot(rpID, 100, 10, "jump", false)
	db.ConfirmAnomaly(id)

	testutil.MustNoErr(t, db.DismissAnomaly(id), "dismiss")
	// Row should still exist (dismiss is a no-op for confirmed).
	var count int
	db.QueryRow(`SELECT COUNT(*) FROM counter_snapshots WHERE id=?`, id).Scan(&count)
	if count != 1 {
		t.Errorf("confirmed anomaly row count = %d, want 1 (dismiss should skip confirmed)", count)
	}
}

// ============================================================================
// hourly_counts.go
// ============================================================================

func TestHourlyCounts_UpsertAccumulates(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	pid, sid := seedProcessStyle(t, db, "P", "S")
	date := "2026-04-19"

	testutil.MustNoErr(t, db.UpsertHourlyCount(pid, sid, date, 8, 5), "upsert")
	testutil.MustNoErr(t, db.UpsertHourlyCount(pid, sid, date, 8, 3), "upsert second")
	testutil.MustNoErr(t, db.UpsertHourlyCount(pid, sid, date, 9, 7), "upsert hour 9")

	list, err := db.ListHourlyCounts(pid, sid, date)
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %v len=%d", err, len(list))
	}
	// Ordered by hour.
	if list[0].Hour != 8 || list[0].Delta != 8 {
		t.Errorf("hour 8 = %+v, want delta=8 (5+3)", list[0])
	}
	if list[1].Hour != 9 || list[1].Delta != 7 {
		t.Errorf("hour 9 = %+v", list[1])
	}
}

func TestHourlyCounts_Totals(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	pid, sid1 := seedProcessStyle(t, db, "P", "S1")
	sid2, _ := db.CreateStyle("S2", "", pid)
	date := "2026-04-19"

	db.UpsertHourlyCount(pid, sid1, date, 8, 10)
	db.UpsertHourlyCount(pid, sid2, date, 8, 5)
	db.UpsertHourlyCount(pid, sid1, date, 9, 3)

	totals, err := db.HourlyCountTotals(pid, date)
	if err != nil {
		t.Fatalf("totals: %v", err)
	}
	if totals[8] != 15 {
		t.Errorf("hour 8 total = %d, want 15", totals[8])
	}
	if totals[9] != 3 {
		t.Errorf("hour 9 total = %d, want 3", totals[9])
	}
}

// ============================================================================
// reconciliation.go
// ============================================================================

func TestReconciliationSummary_OkOnEmptyDB(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	s, err := db.GetReconciliationSummary()
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if s.Status != "ok" {
		t.Errorf("status = %q, want ok (empty DB)", s.Status)
	}
	if s.TotalAnomalies != 0 || s.OutboxPending != 0 || s.DeadLetters != 0 {
		t.Errorf("counts on empty DB: %+v", s)
	}
}

func TestReconciliationSummary_DegradedOnPendingOutbox(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)

	// Enqueue 1 message — pending, not yet stale enough for critical.
	if _, err := db.EnqueueOutbox([]byte(`{}`), "test"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	s, err := db.GetReconciliationSummary()
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if s.OutboxPending != 1 {
		t.Errorf("pending = %d, want 1", s.OutboxPending)
	}
	if s.Status != "degraded" {
		t.Errorf("status = %q, want degraded (pending but not critical yet)", s.Status)
	}
}

func TestReconciliationSummary_CriticalOnDeadLetter(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)

	id, _ := db.EnqueueOutbox([]byte(`{}`), "test")
	for i := 0; i < MaxOutboxRetries; i++ {
		db.IncrementOutboxRetries(id)
	}

	s, err := db.GetReconciliationSummary()
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if s.DeadLetters != 1 {
		t.Errorf("dead letters = %d, want 1", s.DeadLetters)
	}
	if s.Status != "critical" {
		t.Errorf("status = %q, want critical", s.Status)
	}
}

func TestReconciliationAnomalies_DetectsStuckAndDelivered(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)

	// Two orders — backdate updated_at to push past the thresholds.
	stuckID, _ := db.CreateOrder("stuck", "retrieve", nil, false, 1, "", "", "", "", false, "CODE")
	deliveredID, _ := db.CreateOrder("delivered", "retrieve", nil, false, 1, "", "", "", "", false, "CODE")

	db.UpdateOrderStatus(stuckID, "submitted")
	db.UpdateOrderStatus(deliveredID, "delivered")

	// Force updated_at 1 hour in the past so both cross their thresholds
	// (30 min for active_order_stuck, 10 min for delivered_unconfirmed).
	old := time.Now().UTC().Add(-1 * time.Hour).Format("2006-01-02 15:04:05")
	if _, err := db.Exec(`UPDATE orders SET updated_at=?`, old); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	anomalies, err := db.ListReconciliationAnomalies()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(anomalies) != 2 {
		t.Fatalf("anomalies = %d, want 2", len(anomalies))
	}

	var stuck, delivered bool
	for _, a := range anomalies {
		switch a.Issue {
		case "active_order_stuck":
			stuck = true
			if a.RecommendedAction != "sync_order_status" {
				t.Errorf("stuck action = %q", a.RecommendedAction)
			}
		case "delivered_order_unconfirmed":
			delivered = true
		}
	}
	if !stuck || !delivered {
		t.Errorf("expected both issues set: stuck=%v delivered=%v", stuck, delivered)
	}

	s, _ := db.GetReconciliationSummary()
	if s.StuckOrders != 1 || s.DeliveredUnconfirmed != 1 {
		t.Errorf("summary counts: %+v", s)
	}
	if s.Status != "degraded" {
		t.Errorf("status = %q, want degraded", s.Status)
	}
}

// ============================================================================
// orders.go — coverage not already provided by orders-package tests
// ============================================================================

func TestOrders_CreateGetListByUUID(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)

	id, err := db.CreateOrder("uuid-1", "retrieve", nil, false, 5,
		"DELIVERY", "STAGING", "SOURCE", "type_a", true, "PL-A")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := db.GetOrder(id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.UUID != "uuid-1" || got.DeliveryNode != "DELIVERY" || got.StagingNode != "STAGING" ||
		got.SourceNode != "SOURCE" || got.LoadType != "type_a" || !got.AutoConfirm ||
		got.PayloadCode != "PL-A" || got.Quantity != 5 {
		t.Errorf("got %+v", got)
	}

	byUUID, err := db.GetOrderByUUID("uuid-1")
	if err != nil || byUUID.ID != id {
		t.Errorf("by uuid: %v %+v", err, byUUID)
	}

	all, err := db.ListOrders()
	if err != nil || len(all) != 1 {
		t.Fatalf("list: %v len=%d", err, len(all))
	}
}

func TestOrders_ActiveListFilters(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)

	a, _ := db.CreateOrder("a", "retrieve", nil, false, 1, "", "", "", "", false, "")
	b, _ := db.CreateOrder("b", "retrieve", nil, false, 1, "", "", "", "", false, "")
	c, _ := db.CreateOrder("c", "retrieve", nil, false, 1, "", "", "", "", false, "")

	// a stays pending, b confirmed, c cancelled.
	db.UpdateOrderStatus(b, "confirmed")
	db.UpdateOrderStatus(c, "cancelled")

	active, err := db.ListActiveOrders()
	if err != nil || len(active) != 1 || active[0].ID != a {
		t.Fatalf("active: %v len=%d", err, len(active))
	}

	n := db.CountActiveOrders()
	if n != 1 {
		t.Errorf("count = %d, want 1", n)
	}

	// Mark c failed too — still excluded from CountActiveOrders (but ListActiveOrders
	// only excludes confirmed/cancelled, so c still shows in ListActiveOrders).
	db.UpdateOrderStatus(c, "failed")
	if got := db.CountActiveOrders(); got != 1 {
		t.Errorf("count after failing c = %d, want 1", got)
	}
}

func TestOrders_ByProcessAndNodeFilters(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	pid, _ := db.CreateProcess("P1", "", "", "", "", false, false)
	nid, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: pid, CoreNodeName: "N1", Code: "N1", Name: "N1", Sequence: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}

	oid, _ := db.CreateOrder("u-r", "retrieve", &nid, false, 1, "", "", "", "", false, "")
	oid2, _ := db.CreateOrder("u-m", "move", &nid, false, 1, "", "", "", "", false, "")

	// By process
	byProc, err := db.ListActiveOrdersByProcess(pid)
	if err != nil || len(byProc) != 2 {
		t.Fatalf("by process: %v len=%d", err, len(byProc))
	}

	// By process+node
	byNode, err := db.ListActiveOrdersByProcessNode(nid)
	if err != nil || len(byNode) != 2 {
		t.Fatalf("by node: %v len=%d", err, len(byNode))
	}

	// By type
	retrieveOnly, err := db.ListActiveOrdersByProcessNodeAndType(nid, "retrieve")
	if err != nil || len(retrieveOnly) != 1 || retrieveOnly[0].ID != oid {
		t.Fatalf("by type retrieve: %v %+v", err, retrieveOnly)
	}

	// Staged filter — mark oid2 staged, ListStagedOrdersByProcessNode returns it.
	db.UpdateOrderStatus(oid2, "staged")
	staged, err := db.ListStagedOrdersByProcessNode(nid)
	if err != nil || len(staged) != 1 || staged[0].ID != oid2 {
		t.Errorf("staged by node: %v %+v", err, staged)
	}
}

func TestOrders_UpdateMutations(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	id, _ := db.CreateOrder("u", "retrieve", nil, false, 1, "", "", "", "", false, "")

	// ProcessNode assignment
	pid, _ := db.CreateProcess("P", "", "", "", "", false, false)
	nid, _ := db.CreateProcessNode(processes.NodeInput{
		ProcessID: pid, CoreNodeName: "N", Code: "N", Name: "N", Sequence: 1, Enabled: true,
	})
	testutil.MustNoErr(t, db.UpdateOrderProcessNode(id, &nid), "update node")

	// Waybill + ETA
	testutil.MustNoErr(t, db.UpdateOrderWaybill(id, "WB-1", "2026-04-19T10:00:00Z"), "waybill")

	// ETA-only update
	testutil.MustNoErr(t, db.UpdateOrderETA(id, "2026-04-19T12:00:00Z"), "eta")

	// Final count
	testutil.MustNoErr(t, db.UpdateOrderFinalCount(id, 42, true), "final count")

	// Delivery node
	testutil.MustNoErr(t, db.UpdateOrderDeliveryNode(id, "DEST"), "delivery")

	// Steps JSON
	testutil.MustNoErr(t, db.UpdateOrderStepsJSON(id, `{"step":1}`), "steps")

	// Staged expire
	exp := time.Now().UTC().Add(10 * time.Minute)
	testutil.MustNoErr(t, db.UpdateOrderStagedExpireAt(id, &exp), "expire")

	got, err := db.GetOrder(id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ProcessNodeID == nil || *got.ProcessNodeID != nid {
		t.Errorf("node = %v, want %d", got.ProcessNodeID, nid)
	}
	if got.WaybillID == nil || *got.WaybillID != "WB-1" {
		t.Errorf("waybill = %v", got.WaybillID)
	}
	if got.ETA == nil || *got.ETA != "2026-04-19T12:00:00Z" {
		t.Errorf("eta = %v", got.ETA)
	}
	if got.FinalCount == nil || *got.FinalCount != 42 || !got.CountConfirmed {
		t.Errorf("final count fields: %+v", got)
	}
	if got.DeliveryNode != "DEST" {
		t.Errorf("delivery = %q", got.DeliveryNode)
	}
	if got.StagedExpireAt == nil {
		t.Error("staged_expire_at should be populated")
	}

	// Clear expire
	testutil.MustNoErr(t, db.UpdateOrderStagedExpireAt(id, nil), "clear expire")
	got2, _ := db.GetOrder(id)
	if got2.StagedExpireAt != nil {
		t.Errorf("staged_expire_at after clear = %v", got2.StagedExpireAt)
	}
}

func TestOrders_HistoryInsertAndList(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	id, _ := db.CreateOrder("u", "retrieve", nil, false, 1, "", "", "", "", false, "")

	testutil.MustNoErr(t, db.InsertOrderHistory(id, "pending", "submitted", "auto-submit"), "insert history")
	testutil.MustNoErr(t, db.InsertOrderHistory(id, "submitted", "acknowledged", "core ack"), "insert history 2")

	list, err := db.ListOrderHistory(id)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("history len = %d, want 2", len(list))
	}
	// Check as a set — the two rows share the same created_at to the second,
	// so SQLite's ORDER BY does not guarantee which comes first.
	seen := map[string]orders.History{}
	for _, h := range list {
		seen[string(h.NewStatus)] = h
		if h.OrderID != id {
			t.Errorf("wrong order_id: %+v", h)
		}
	}
	if h, ok := seen["submitted"]; !ok || h.OldStatus != "pending" || h.Detail != "auto-submit" {
		t.Errorf("submitted row: %+v ok=%v", h, ok)
	}
	if h, ok := seen["acknowledged"]; !ok || h.OldStatus != "submitted" || h.Detail != "core ack" {
		t.Errorf("acknowledged row: %+v ok=%v", h, ok)
	}
}

func TestOrders_GetMissingReturnsError(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	if _, err := db.GetOrder(9999); err == nil {
		t.Fatal("expected error for missing id")
	}
	if _, err := db.GetOrderByUUID("none"); err == nil {
		t.Fatal("expected error for missing uuid")
	}
}

// ============================================================================
// operator_stations.go
// ============================================================================

func TestOperatorStations_CRUD(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	pid, _ := db.CreateProcess("P", "", "", "", "", false, false)

	// Empty Code + Sequence trigger auto-generation paths.
	id, err := db.CreateOperatorStation(stations.Input{
		ProcessID: pid, Name: "Main Station", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create (auto): %v", err)
	}
	got, err := db.GetOperatorStation(id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Code == "" {
		t.Error("expected auto-generated code")
	}
	if got.Sequence != 1 {
		t.Errorf("sequence = %d, want 1", got.Sequence)
	}
	if got.DeviceMode != "fixed_hmi" {
		t.Errorf("device_mode default = %q, want fixed_hmi", got.DeviceMode)
	}
	if got.ProcessName != "P" {
		t.Errorf("joined process_name = %q", got.ProcessName)
	}

	// Second create keeps incrementing sequence.
	id2, err := db.CreateOperatorStation(stations.Input{
		ProcessID: pid, Code: "S-CUSTOM", Name: "Second", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create 2: %v", err)
	}
	got2, _ := db.GetOperatorStation(id2)
	if got2.Code != "S-CUSTOM" || got2.Sequence != 2 {
		t.Errorf("got2 = %+v", got2)
	}

	// List / ByProcess
	list, err := db.ListOperatorStations()
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %v len=%d", err, len(list))
	}
	byProc, _ := db.ListOperatorStationsByProcess(pid)
	if len(byProc) != 2 {
		t.Errorf("by process = %d, want 2", len(byProc))
	}

	// Update — empty Code + Sequence triggers preservation paths.
	if err := db.UpdateOperatorStation(id, stations.Input{
		ProcessID: pid, Name: "Main Renamed", AreaLabel: "A1",
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	gotU, _ := db.GetOperatorStation(id)
	if gotU.Name != "Main Renamed" || gotU.AreaLabel != "A1" {
		t.Errorf("after update: %+v", gotU)
	}
	if gotU.Code != got.Code {
		t.Errorf("code changed during update: %q -> %q", got.Code, gotU.Code)
	}

	testutil.MustNoErr(t, db.DeleteOperatorStation(id2), "delete")
	after, _ := db.ListOperatorStations()
	if len(after) != 1 {
		t.Errorf("after delete: %d", len(after))
	}
}

func TestOperatorStations_TouchUpdatesHealthAndLastSeen(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	pid, _ := db.CreateProcess("P", "", "", "", "", false, false)
	id, _ := db.CreateOperatorStation(stations.Input{
		ProcessID: pid, Name: "S", Enabled: true,
	})

	testutil.MustNoErr(t, db.TouchOperatorStation(id, "ok"), "touch")
	got, _ := db.GetOperatorStation(id)
	if got.HealthStatus != "ok" {
		t.Errorf("health = %q", got.HealthStatus)
	}
	if got.LastSeenAt == nil {
		t.Error("last_seen_at should be populated")
	}
}

func TestOperatorStations_MoveUpDown(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	pid, _ := db.CreateProcess("P", "", "", "", "", false, false)
	a, _ := db.CreateOperatorStation(stations.Input{ProcessID: pid, Name: "A"})
	b, _ := db.CreateOperatorStation(stations.Input{ProcessID: pid, Name: "B"})
	c, _ := db.CreateOperatorStation(stations.Input{ProcessID: pid, Name: "C"})

	// Initial sequences: A=1, B=2, C=3.

	// Move B up → swap with A (seq 2 <-> 1).
	testutil.MustNoErr(t, db.MoveOperatorStation(b, "up"), "move up")
	sa, _ := db.GetOperatorStation(a)
	sb, _ := db.GetOperatorStation(b)
	if sa.Sequence != 2 || sb.Sequence != 1 {
		t.Errorf("after up: a=%d b=%d, want 2,1", sa.Sequence, sb.Sequence)
	}

	// Move C up → swap with A (now at seq 2).
	testutil.MustNoErr(t, db.MoveOperatorStation(c, "up"), "move up 2")
	sa2, _ := db.GetOperatorStation(a)
	sc, _ := db.GetOperatorStation(c)
	if sa2.Sequence != 3 || sc.Sequence != 2 {
		t.Errorf("after 2nd up: a=%d c=%d", sa2.Sequence, sc.Sequence)
	}

	// Move at the edge is a no-op (no error).
	testutil.MustNoErr(t, db.MoveOperatorStation(b, "up"), "no-op up")
	testutil.MustNoErr(t, db.MoveOperatorStation(a, "down"), "no-op down")

	// down works too
	testutil.MustNoErr(t, db.MoveOperatorStation(b, "down"), "move down")
}

// TestOperatorStations_SetStationNodes* moved to
// service/station_service_test.go in Phase 6.4a after the
// (db *DB).SetStationNodes method was retired in favor of
// StationService.SetNodes.

// ============================================================================
// process_nodes.go
// ============================================================================

func TestProcessNodes_CRUDAndListing(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	pid, _ := db.CreateProcess("P", "", "", "", "", false, false)
	sid, _ := db.CreateOperatorStation(stations.Input{ProcessID: pid, Name: "S"})

	// Auto-code, auto-sequence, auto-name-from-core.
	id, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID:         pid,
		OperatorStationID: &sid,
		CoreNodeName:      "NODE-A",
		Enabled:           true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := db.GetProcessNode(id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Code == "" || got.Name != "NODE-A" || got.Sequence != 1 {
		t.Errorf("auto paths: %+v", got)
	}
	if got.StationName != "S" || got.ProcessName != "P" {
		t.Errorf("joined names: %+v", got)
	}

	// Second node increments sequence.
	id2, _ := db.CreateProcessNode(processes.NodeInput{
		ProcessID: pid, CoreNodeName: "NODE-B", Code: "N-B-explicit", Sequence: 5, Enabled: true,
	})
	got2, _ := db.GetProcessNode(id2)
	if got2.Sequence != 5 {
		t.Errorf("explicit sequence = %d", got2.Sequence)
	}

	// List at multiple scopes.
	all, _ := db.ListProcessNodes()
	if len(all) != 2 {
		t.Errorf("list all = %d", len(all))
	}
	byProc, _ := db.ListProcessNodesByProcess(pid)
	if len(byProc) != 2 {
		t.Errorf("by process = %d", len(byProc))
	}
	byStation, _ := db.ListProcessNodesByStation(sid)
	if len(byStation) != 1 {
		t.Errorf("by station = %d", len(byStation))
	}

	// Update with empty Code/Sequence preserves existing values.
	if err := db.UpdateProcessNode(id, processes.NodeInput{
		ProcessID: pid, CoreNodeName: "NODE-A", Name: "Renamed", Enabled: true,
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	gotU, _ := db.GetProcessNode(id)
	if gotU.Name != "Renamed" || gotU.Code != got.Code || gotU.Sequence != got.Sequence {
		t.Errorf("preservation after update: %+v (was %+v)", gotU, got)
	}

	testutil.MustNoErr(t, db.DeleteProcessNode(id2), "delete")
	after, _ := db.ListProcessNodes()
	if len(after) != 1 {
		t.Errorf("after delete = %d", len(after))
	}
}

func TestProcessNodes_InvalidStationIDCoercedToNil(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	pid, _ := db.CreateProcess("P", "", "", "", "", false, false)

	// Pass OperatorStationID pointer to 0 — create should coerce to nil.
	zero := int64(0)
	id, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID:         pid,
		OperatorStationID: &zero,
		CoreNodeName:      "N",
		Enabled:           true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, _ := db.GetProcessNode(id)
	if got.OperatorStationID != nil {
		t.Errorf("station id = %v, want nil", got.OperatorStationID)
	}
}

// ============================================================================
// process_node_runtime.go
// ============================================================================

func TestProcessNodeRuntime_EnsureGetSet(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	pid, _ := db.CreateProcess("P", "", "", "", "", false, false)
	nid, _ := db.CreateProcessNode(processes.NodeInput{
		ProcessID: pid, CoreNodeName: "N", Code: "N", Name: "N", Sequence: 1, Enabled: true,
	})

	// First Ensure creates; second returns existing.
	rt1, err := db.EnsureProcessNodeRuntime(nid)
	if err != nil {
		t.Fatalf("ensure first: %v", err)
	}
	rt2, err := db.EnsureProcessNodeRuntime(nid)
	if err != nil {
		t.Fatalf("ensure second: %v", err)
	}
	if rt1.ID != rt2.ID {
		t.Errorf("ensure idempotency: id %d vs %d", rt1.ID, rt2.ID)
	}

	// SetProcessNodeRuntime updates claim + uop.
	cid := int64(42)
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(nid, &cid, 75), "set")
	rt, _ := db.GetProcessNodeRuntime(nid)
	if rt.ActiveClaimID == nil || *rt.ActiveClaimID != 42 || rt.RemainingUOPCached != 75 {
		t.Errorf("after set: %+v", rt)
	}

	// UpdateProcessNodeRuntimeOrders.
	a, b := int64(100), int64(200)
	testutil.MustNoErr(t, db.UpdateProcessNodeRuntimeOrders(nid, &a, &b), "update orders")
	rt2b, _ := db.GetProcessNodeRuntime(nid)
	if rt2b.ActiveOrderID == nil || *rt2b.ActiveOrderID != 100 ||
		rt2b.StagedOrderID == nil || *rt2b.StagedOrderID != 200 {
		t.Errorf("orders: %+v", rt2b)
	}

	// UpdateProcessNodeUOP
	testutil.MustNoErr(t, db.UpdateProcessNodeUOP(nid, 30), "update uop")
	rt3, _ := db.GetProcessNodeRuntime(nid)
	if rt3.RemainingUOPCached != 30 {
		t.Errorf("uop = %d, want 30", rt3.RemainingUOPCached)
	}

	// SetActivePull
	testutil.MustNoErr(t, db.SetActivePull(nid, true), "set active pull")
	rt4, _ := db.GetProcessNodeRuntime(nid)
	if !rt4.ActivePull {
		t.Error("active_pull should be true")
	}
	db.SetActivePull(nid, false)
	rt5, _ := db.GetProcessNodeRuntime(nid)
	if rt5.ActivePull {
		t.Error("active_pull should be false")
	}
}

// ============================================================================
// style_node_claims.go
// ============================================================================

func TestStyleNodeClaims_InsertUpdateGetList(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	_, sid := seedProcessStyle(t, db, "P", "S")

	// Insert with defaults (role blanks → "consume", swap_mode blank → "simple").
	id, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: sid, CoreNodeName: "N1", PayloadCode: "PL-1",
	})
	if err != nil {
		t.Fatalf("upsert insert: %v", err)
	}
	got, _ := db.GetStyleNodeClaim(id)
	if got.Role != "consume" || got.SwapMode != "simple" {
		t.Errorf("defaults: role=%q swap_mode=%q", got.Role, got.SwapMode)
	}
	if got.Sequence != 1 {
		t.Errorf("auto sequence = %d, want 1", got.Sequence)
	}

	// Second insert on a different node — sequence auto-increments.
	id2, _ := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: sid, CoreNodeName: "N2", Role: "produce", PayloadCode: "PL-2",
	})
	got2, _ := db.GetStyleNodeClaim(id2)
	if got2.Sequence != 2 {
		t.Errorf("second sequence = %d", got2.Sequence)
	}

	// Upsert on existing (styleID + coreNodeName match) — returns same id, updates fields.
	id3, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: sid, CoreNodeName: "N1", Role: "produce", PayloadCode: "PL-1-v2",
		AllowedPayloadCodes: []string{"PL-1-v2", "PL-FALLBACK"},
	})
	if err != nil {
		t.Fatalf("upsert existing: %v", err)
	}
	if id3 != id {
		t.Errorf("upsert reinsert id %d != original %d", id3, id)
	}
	got3, _ := db.GetStyleNodeClaim(id)
	if got3.Role != "produce" || got3.PayloadCode != "PL-1-v2" {
		t.Errorf("after upsert-update: %+v", got3)
	}
	if len(got3.AllowedPayloadCodes) != 2 {
		t.Errorf("allowed codes = %v", got3.AllowedPayloadCodes)
	}

	// GetByNode
	byNode, err := db.GetStyleNodeClaimByNode(sid, "N1")
	if err != nil || byNode.ID != id {
		t.Errorf("by node: %v %+v", err, byNode)
	}

	// List
	list, _ := db.ListStyleNodeClaims(sid)
	if len(list) != 2 {
		t.Errorf("list = %d", len(list))
	}
}

// The press_position SwapMode is the in-memory marker used by the
// per-position fan-out post-processor. Persisting it to style_node_claims
// would surface synthesized fragments as if they were real configured
// claims, so UpsertClaim rejects it (and any other unknown SwapMode).
func TestUpsertClaim_RejectsPressPositionSwapMode(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	_, sid := seedProcessStyle(t, db, "P", "S")

	_, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: sid, CoreNodeName: "N", SwapMode: "press_position", PayloadCode: "PL",
	})
	if err == nil {
		t.Fatal("expected error for swap_mode=press_position; got nil")
	}
	if !strings.Contains(err.Error(), "press_position") {
		t.Errorf("err = %q, want it to name the rejected swap_mode", err)
	}
}

// Any unknown swap_mode value (typo, stale import, future-mode) should
// be rejected at config time so the dispatcher doesn't see surprise
// modes at plan time.
func TestUpsertClaim_RejectsUnknownSwapMode(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	_, sid := seedProcessStyle(t, db, "PU", "SU")

	_, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: sid, CoreNodeName: "N", SwapMode: "does_not_exist", PayloadCode: "PL",
	})
	if err == nil {
		t.Fatal("expected error for unknown swap_mode")
	}
}

func TestStyleNodeClaims_ManualSwapRequiresOutboundDestination(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	_, sid := seedProcessStyle(t, db, "P", "S")

	// Missing outbound_destination → error.
	_, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: sid, CoreNodeName: "N", SwapMode: "manual_swap", PayloadCode: "PL",
	})
	if err == nil {
		t.Fatal("expected error for manual_swap without outbound_destination")
	}

	// With outbound_destination → auto-confirm is forced.
	id, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: sid, CoreNodeName: "N", SwapMode: "manual_swap", PayloadCode: "PL",
		OutboundDestination: "DEST", AutoConfirm: false,
	})
	if err != nil {
		t.Fatalf("manual_swap with dest: %v", err)
	}
	got, _ := db.GetStyleNodeClaim(id)
	if !got.AutoConfirm {
		t.Error("manual_swap should force auto_confirm=true")
	}
}

func TestStyleNodeClaims_AllowedPayloads(t *testing.T) {
	t.Parallel()
	claim := &processes.NodeClaim{}
	if got := claim.AllowedPayloads(); got != nil {
		t.Errorf("empty claim: %v, want nil", got)
	}

	claim.PayloadCode = "PL-PRIMARY"
	got := claim.AllowedPayloads()
	if len(got) != 1 || got[0] != "PL-PRIMARY" {
		t.Errorf("primary-only: %v", got)
	}

	claim.AllowedPayloadCodes = []string{"A", "B"}
	got = claim.AllowedPayloads()
	if len(got) != 2 || got[0] != "A" || got[1] != "B" {
		t.Errorf("explicit list: %v", got)
	}
}

// TestStyleNodeClaims_LinesideSoftThreshold_Roundtrip verifies the
// phase-6 column roundtrips cleanly through insert, update, get, and
// get-by-node. Default is zero ("off"); explicit values must survive
// both INSERT and UPDATE paths.
func TestStyleNodeClaims_LinesideSoftThreshold_Roundtrip(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	_, sid := seedProcessStyle(t, db, "P", "S")

	// Default (unset) persists as 0.
	id, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: sid, CoreNodeName: "N1", PayloadCode: "PL",
	})
	if err != nil {
		t.Fatalf("insert default: %v", err)
	}
	got, _ := db.GetStyleNodeClaim(id)
	if got.LinesideSoftThreshold != 0 {
		t.Errorf("default LinesideSoftThreshold = %d, want 0", got.LinesideSoftThreshold)
	}

	// Explicit value on update survives.
	if _, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: sid, CoreNodeName: "N1", PayloadCode: "PL",
		LinesideSoftThreshold: 12,
	}); err != nil {
		t.Fatalf("update with threshold: %v", err)
	}
	got, _ = db.GetStyleNodeClaim(id)
	if got.LinesideSoftThreshold != 12 {
		t.Errorf("after update: LinesideSoftThreshold = %d, want 12", got.LinesideSoftThreshold)
	}

	// Verified via GetByNode too (same scanner, but covers the second read path).
	byNode, _ := db.GetStyleNodeClaimByNode(sid, "N1")
	if byNode.LinesideSoftThreshold != 12 {
		t.Errorf("GetByNode: LinesideSoftThreshold = %d, want 12", byNode.LinesideSoftThreshold)
	}

	// Explicit value on fresh insert (different node) also survives.
	id2, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: sid, CoreNodeName: "N2", PayloadCode: "PL2",
		LinesideSoftThreshold: 5,
	})
	if err != nil {
		t.Fatalf("insert with threshold: %v", err)
	}
	got2, _ := db.GetStyleNodeClaim(id2)
	if got2.LinesideSoftThreshold != 5 {
		t.Errorf("fresh insert: LinesideSoftThreshold = %d, want 5", got2.LinesideSoftThreshold)
	}
}

func TestStyleNodeClaims_Delete(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	_, sid := seedProcessStyle(t, db, "P", "S")
	id, _ := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: sid, CoreNodeName: "N", PayloadCode: "PL",
	})
	testutil.MustNoErr(t, db.DeleteStyleNodeClaim(id), "delete")
	if _, err := db.GetStyleNodeClaim(id); err == nil {
		t.Fatal("expected error after delete")
	}
}

// ============================================================================
// process_changeovers.go
// ============================================================================

// TestProcessChangeovers_CreateAtomic / StateTransitionsAndListing /
// NodeAndStationTaskMutations moved to
// service/changeover_service_test.go in Phase 6.4a after the
// (db *DB).CreateChangeover method was retired in favor of
// ChangeoverService.Create.
