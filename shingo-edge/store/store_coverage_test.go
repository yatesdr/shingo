package store

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	pid, err := db.CreateProcess(procName, "desc", "active_production", "", "", false)
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

	if err := db.UpdateAdminPassword("alice", "hash-v2"); err != nil {
		t.Fatalf("update: %v", err)
	}
	got2, err := db.GetAdminUser("alice")
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if got2.PasswordHash != "hash-v2" {
		t.Errorf("password hash = %q, want hash-v2", got2.PasswordHash)
	}
}

func TestAdminUsers_GetMissingReturnsError(t *testing.T) {
	db := coverageDB(t)
	if _, err := db.GetAdminUser("ghost"); err == nil {
		t.Fatal("expected error for missing user")
	}
}

// ============================================================================
// processes.go
// ============================================================================

func TestProcesses_CreateListGetUpdateDelete(t *testing.T) {
	db := coverageDB(t)

	id, err := db.CreateProcess("LINE-A", "main line", "active_production", "PLC1", "TAG1", true)
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

	if err := db.UpdateProcess(id, "LINE-A-v2", "updated", "", "PLC2", "TAG2", false); err != nil {
		t.Fatalf("update: %v", err)
	}
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

	if err := db.DeleteProcess(id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	rest, _ := db.ListProcesses()
	if len(rest) != 0 {
		t.Errorf("list after delete = %d, want 0", len(rest))
	}
}

func TestProcesses_CreateDefaultsProductionState(t *testing.T) {
	db := coverageDB(t)
	id, err := db.CreateProcess("DEF", "", "", "", "", false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, _ := db.GetProcess(id)
	if got.ProductionState != "active_production" {
		t.Errorf("default productionState = %q, want active_production", got.ProductionState)
	}
}

func TestProcesses_ActiveAndTargetStyle(t *testing.T) {
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

	if err := db.SetActiveStyle(pid, &sid); err != nil {
		t.Fatalf("set active: %v", err)
	}
	active, _ = db.GetActiveStyleID(pid)
	if active == nil || *active != sid {
		t.Errorf("active style = %v, want %d", active, sid)
	}

	// Set target style.
	sid2, _ := db.CreateStyle("S2", "", pid)
	if err := db.SetTargetStyle(pid, &sid2); err != nil {
		t.Fatalf("set target: %v", err)
	}
	got, _ := db.GetProcess(pid)
	if got.TargetStyleID == nil || *got.TargetStyleID != sid2 {
		t.Errorf("target style = %v, want %d", got.TargetStyleID, sid2)
	}

	// Clear active by nil.
	if err := db.SetActiveStyle(pid, nil); err != nil {
		t.Fatalf("clear active: %v", err)
	}
	active, _ = db.GetActiveStyleID(pid)
	if active != nil {
		t.Errorf("active after nil = %v", active)
	}
}

func TestProcesses_SetProductionState(t *testing.T) {
	db := coverageDB(t)
	id, err := db.CreateProcess("P", "", "", "", "", false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := db.SetProcessProductionState(id, "changeover_active"); err != nil {
		t.Fatalf("set state: %v", err)
	}
	got, _ := db.GetProcess(id)
	if got.ProductionState != "changeover_active" {
		t.Errorf("state = %q, want changeover_active", got.ProductionState)
	}
}

// ============================================================================
// styles.go
// ============================================================================

func TestStyles_CRUDAndListing(t *testing.T) {
	db := coverageDB(t)
	pid, err := db.CreateProcess("P", "", "", "", "", false)
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

	if err := db.UpdateStyle(s1, "WIDGET-A-v2", "renamed", pid); err != nil {
		t.Fatalf("update: %v", err)
	}
	got2, _ := db.GetStyle(s1)
	if got2.Name != "WIDGET-A-v2" {
		t.Errorf("name after update = %q", got2.Name)
	}

	if err := db.DeleteStyle(s2); err != nil {
		t.Fatalf("delete: %v", err)
	}
	after, _ := db.ListStyles()
	if len(after) != 1 {
		t.Errorf("list after delete = %d, want 1", len(after))
	}
}

func TestStyles_GetMissingReturnsError(t *testing.T) {
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
	db := coverageDB(t)

	if err := db.UpsertShift(1, "Day", "06:00", "14:00"); err != nil {
		t.Fatalf("upsert 1: %v", err)
	}
	if err := db.UpsertShift(2, "Swing", "14:00", "22:00"); err != nil {
		t.Fatalf("upsert 2: %v", err)
	}

	list, err := db.ListShifts()
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %v len=%d", err, len(list))
	}
	if list[0].ShiftNumber != 1 || list[0].Name != "Day" {
		t.Errorf("got[0] = %+v", list[0])
	}

	// Upsert conflict -> updates name/start/end, keeps id
	if err := db.UpsertShift(1, "Day Updated", "07:00", "15:00"); err != nil {
		t.Fatalf("upsert update: %v", err)
	}
	list2, _ := db.ListShifts()
	if list2[0].Name != "Day Updated" || list2[0].StartTime != "07:00" {
		t.Errorf("after upsert-update: %+v", list2[0])
	}

	if err := db.DeleteShift(1); err != nil {
		t.Fatalf("delete: %v", err)
	}
	list3, _ := db.ListShifts()
	if len(list3) != 1 || list3[0].ShiftNumber != 2 {
		t.Errorf("after delete: %+v", list3)
	}
}

// ============================================================================
// payload_catalog.go
// ============================================================================

func TestPayloadCatalog_UpsertListGet(t *testing.T) {
	db := coverageDB(t)

	entry := &PayloadCatalogEntry{
		ID:          10,
		Name:        "Widget Tote",
		Code:        "WT-10",
		Description: "10-unit tote",
		UOPCapacity: 10,
	}
	if err := db.UpsertPayloadCatalog(entry); err != nil {
		t.Fatalf("upsert: %v", err)
	}

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
	if err := db.UpsertPayloadCatalog(entry); err != nil {
		t.Fatalf("upsert-update: %v", err)
	}
	byCode2, _ := db.GetPayloadCatalogByCode("WT-10")
	if byCode2.UOPCapacity != 12 || !strings.Contains(byCode2.Description, "v2") {
		t.Errorf("after upsert-update: %+v", byCode2)
	}
}

func TestPayloadCatalog_DeleteStale(t *testing.T) {
	db := coverageDB(t)
	for _, id := range []int64{1, 2, 3} {
		e := &PayloadCatalogEntry{ID: id, Name: "E", Code: "C"}
		if err := db.UpsertPayloadCatalog(e); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}

	// Empty activeIDs is a no-op (safety).
	if err := db.DeleteStalePayloadCatalogEntries(nil); err != nil {
		t.Fatalf("delete stale (nil): %v", err)
	}
	list, _ := db.ListPayloadCatalog()
	if len(list) != 3 {
		t.Fatalf("len after no-op delete = %d, want 3", len(list))
	}

	// Keep only id=2.
	if err := db.DeleteStalePayloadCatalogEntries([]int64{2}); err != nil {
		t.Fatalf("delete stale: %v", err)
	}
	list2, _ := db.ListPayloadCatalog()
	if len(list2) != 1 || list2[0].ID != 2 {
		t.Errorf("after delete stale: %+v", list2)
	}
}

func TestPayloadCatalog_GetMissingReturnsError(t *testing.T) {
	db := coverageDB(t)
	if _, err := db.GetPayloadCatalogByCode("none"); err == nil {
		t.Fatal("expected error for missing code")
	}
}

// ============================================================================
// reporting_points.go
// ============================================================================

func TestReportingPoints_CRUD(t *testing.T) {
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
	if err := db.UpdateReportingPoint(id, "PLC2", "COUNTER_B", sid2, true); err != nil {
		t.Fatalf("update: %v", err)
	}
	got2, _ := db.GetReportingPoint(id)
	if !got2.Enabled || got2.PLCName != "PLC2" || got2.TagName != "COUNTER_B" || got2.StyleID != sid2 {
		t.Errorf("after update: %+v", got2)
	}

	if err := db.DeleteReportingPoint(id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	after, _ := db.ListReportingPoints()
	if len(after) != 0 {
		t.Errorf("after delete: %d", len(after))
	}
}

func TestReportingPoints_UpdateCounter(t *testing.T) {
	db := coverageDB(t)
	_, sid := seedProcessStyle(t, db, "P", "S")

	id, _ := db.CreateReportingPoint("PLC", "TAG", sid)
	if err := db.UpdateReportingPointCounter(id, 42); err != nil {
		t.Fatalf("update counter: %v", err)
	}
	got, _ := db.GetReportingPoint(id)
	if got.LastCount != 42 {
		t.Errorf("last_count = %d, want 42", got.LastCount)
	}
	if got.LastPollAt == nil {
		t.Error("last_poll_at should be populated after counter update")
	}
}

func TestReportingPoints_SetManaged(t *testing.T) {
	db := coverageDB(t)
	_, sid := seedProcessStyle(t, db, "P", "S")
	id, _ := db.CreateReportingPoint("PLC", "TAG", sid)

	if err := db.SetReportingPointManaged(id, true); err != nil {
		t.Fatalf("set managed: %v", err)
	}
	got, _ := db.GetReportingPoint(id)
	if !got.WarlinkManaged {
		t.Error("expected warlink_managed=true")
	}

	if err := db.SetReportingPointManaged(id, false); err != nil {
		t.Fatalf("set managed false: %v", err)
	}
	got2, _ := db.GetReportingPoint(id)
	if got2.WarlinkManaged {
		t.Error("expected warlink_managed=false")
	}
}

func TestReportingPoints_ListEnabledAndLookups(t *testing.T) {
	db := coverageDB(t)
	_, sid := seedProcessStyle(t, db, "P", "S")

	id1, _ := db.CreateReportingPoint("PLC1", "T1", sid)
	id2, _ := db.CreateReportingPoint("PLC2", "T2", sid)

	// Schema default for `enabled` is 1, so both rows start enabled.
	// Disable id2 to verify ListEnabledReportingPoints actually filters.
	if err := db.UpdateReportingPoint(id2, "PLC2", "T2", sid, false); err != nil {
		t.Fatalf("disable id2: %v", err)
	}

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
	if err := db.ConfirmAnomaly(anomalyID); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	list2, _ := db.ListUnconfirmedAnomalies()
	if len(list2) != 0 {
		t.Errorf("after confirm: %d", len(list2))
	}

	// Dismissing a second anomaly deletes it.
	dismissID, _ := db.InsertCounterSnapshot(rpID, 300, 100, "jump", false)
	if err := db.DismissAnomaly(dismissID); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	list3, _ := db.ListUnconfirmedAnomalies()
	if len(list3) != 0 {
		t.Errorf("after dismiss: %d", len(list3))
	}
}

func TestCounterSnapshots_DismissOnlyUnconfirmedJump(t *testing.T) {
	db := coverageDB(t)
	_, sid := seedProcessStyle(t, db, "P", "S")
	rpID, _ := db.CreateReportingPoint("PLC", "TAG", sid)

	// Insert, then confirm — dismiss should not delete a confirmed row.
	id, _ := db.InsertCounterSnapshot(rpID, 100, 10, "jump", false)
	db.ConfirmAnomaly(id)

	if err := db.DismissAnomaly(id); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
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
	db := coverageDB(t)
	pid, sid := seedProcessStyle(t, db, "P", "S")
	date := "2026-04-19"

	if err := db.UpsertHourlyCount(pid, sid, date, 8, 5); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := db.UpsertHourlyCount(pid, sid, date, 8, 3); err != nil {
		t.Fatalf("upsert second: %v", err)
	}
	if err := db.UpsertHourlyCount(pid, sid, date, 9, 7); err != nil {
		t.Fatalf("upsert hour 9: %v", err)
	}

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
	db := coverageDB(t)
	pid, _ := db.CreateProcess("P1", "", "", "", "", false)
	nid, err := db.CreateProcessNode(ProcessNodeInput{
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
	db := coverageDB(t)
	id, _ := db.CreateOrder("u", "retrieve", nil, false, 1, "", "", "", "", false, "")

	// ProcessNode assignment
	pid, _ := db.CreateProcess("P", "", "", "", "", false)
	nid, _ := db.CreateProcessNode(ProcessNodeInput{
		ProcessID: pid, CoreNodeName: "N", Code: "N", Name: "N", Sequence: 1, Enabled: true,
	})
	if err := db.UpdateOrderProcessNode(id, &nid); err != nil {
		t.Fatalf("update node: %v", err)
	}

	// Waybill + ETA
	if err := db.UpdateOrderWaybill(id, "WB-1", "2026-04-19T10:00:00Z"); err != nil {
		t.Fatalf("waybill: %v", err)
	}

	// ETA-only update
	if err := db.UpdateOrderETA(id, "2026-04-19T12:00:00Z"); err != nil {
		t.Fatalf("eta: %v", err)
	}

	// Final count
	if err := db.UpdateOrderFinalCount(id, 42, true); err != nil {
		t.Fatalf("final count: %v", err)
	}

	// Delivery node
	if err := db.UpdateOrderDeliveryNode(id, "DEST"); err != nil {
		t.Fatalf("delivery: %v", err)
	}

	// Steps JSON
	if err := db.UpdateOrderStepsJSON(id, `{"step":1}`); err != nil {
		t.Fatalf("steps: %v", err)
	}

	// Staged expire
	exp := time.Now().UTC().Add(10 * time.Minute)
	if err := db.UpdateOrderStagedExpireAt(id, &exp); err != nil {
		t.Fatalf("expire: %v", err)
	}

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
	if err := db.UpdateOrderStagedExpireAt(id, nil); err != nil {
		t.Fatalf("clear expire: %v", err)
	}
	got2, _ := db.GetOrder(id)
	if got2.StagedExpireAt != nil {
		t.Errorf("staged_expire_at after clear = %v", got2.StagedExpireAt)
	}
}

func TestOrders_HistoryInsertAndList(t *testing.T) {
	db := coverageDB(t)
	id, _ := db.CreateOrder("u", "retrieve", nil, false, 1, "", "", "", "", false, "")

	if err := db.InsertOrderHistory(id, "pending", "submitted", "auto-submit"); err != nil {
		t.Fatalf("insert history: %v", err)
	}
	if err := db.InsertOrderHistory(id, "submitted", "acknowledged", "core ack"); err != nil {
		t.Fatalf("insert history 2: %v", err)
	}

	list, err := db.ListOrderHistory(id)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("history len = %d, want 2", len(list))
	}
	// Check as a set — the two rows share the same created_at to the second,
	// so SQLite's ORDER BY does not guarantee which comes first.
	seen := map[string]OrderHistory{}
	for _, h := range list {
		seen[h.NewStatus] = h
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
	db := coverageDB(t)
	pid, _ := db.CreateProcess("P", "", "", "", "", false)

	// Empty Code + Sequence trigger auto-generation paths.
	id, err := db.CreateOperatorStation(OperatorStationInput{
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
	id2, err := db.CreateOperatorStation(OperatorStationInput{
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
	if err := db.UpdateOperatorStation(id, OperatorStationInput{
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

	if err := db.DeleteOperatorStation(id2); err != nil {
		t.Fatalf("delete: %v", err)
	}
	after, _ := db.ListOperatorStations()
	if len(after) != 1 {
		t.Errorf("after delete: %d", len(after))
	}
}

func TestOperatorStations_TouchUpdatesHealthAndLastSeen(t *testing.T) {
	db := coverageDB(t)
	pid, _ := db.CreateProcess("P", "", "", "", "", false)
	id, _ := db.CreateOperatorStation(OperatorStationInput{
		ProcessID: pid, Name: "S", Enabled: true,
	})

	if err := db.TouchOperatorStation(id, "ok"); err != nil {
		t.Fatalf("touch: %v", err)
	}
	got, _ := db.GetOperatorStation(id)
	if got.HealthStatus != "ok" {
		t.Errorf("health = %q", got.HealthStatus)
	}
	if got.LastSeenAt == nil {
		t.Error("last_seen_at should be populated")
	}
}

func TestOperatorStations_MoveUpDown(t *testing.T) {
	db := coverageDB(t)
	pid, _ := db.CreateProcess("P", "", "", "", "", false)
	a, _ := db.CreateOperatorStation(OperatorStationInput{ProcessID: pid, Name: "A"})
	b, _ := db.CreateOperatorStation(OperatorStationInput{ProcessID: pid, Name: "B"})
	c, _ := db.CreateOperatorStation(OperatorStationInput{ProcessID: pid, Name: "C"})

	// Initial sequences: A=1, B=2, C=3.

	// Move B up → swap with A (seq 2 <-> 1).
	if err := db.MoveOperatorStation(b, "up"); err != nil {
		t.Fatalf("move up: %v", err)
	}
	sa, _ := db.GetOperatorStation(a)
	sb, _ := db.GetOperatorStation(b)
	if sa.Sequence != 2 || sb.Sequence != 1 {
		t.Errorf("after up: a=%d b=%d, want 2,1", sa.Sequence, sb.Sequence)
	}

	// Move C up → swap with A (now at seq 2).
	if err := db.MoveOperatorStation(c, "up"); err != nil {
		t.Fatalf("move up 2: %v", err)
	}
	sa2, _ := db.GetOperatorStation(a)
	sc, _ := db.GetOperatorStation(c)
	if sa2.Sequence != 3 || sc.Sequence != 2 {
		t.Errorf("after 2nd up: a=%d c=%d", sa2.Sequence, sc.Sequence)
	}

	// Move at the edge is a no-op (no error).
	if err := db.MoveOperatorStation(b, "up"); err != nil {
		t.Fatalf("no-op up: %v", err)
	}
	if err := db.MoveOperatorStation(a, "down"); err != nil {
		t.Fatalf("no-op down: %v", err)
	}

	// down works too
	if err := db.MoveOperatorStation(b, "down"); err != nil {
		t.Fatalf("move down: %v", err)
	}
}

func TestOperatorStations_SetStationNodes(t *testing.T) {
	db := coverageDB(t)
	pid, _ := db.CreateProcess("P", "", "", "", "", false)
	id, _ := db.CreateOperatorStation(OperatorStationInput{ProcessID: pid, Name: "S"})

	// Initial set.
	if err := db.SetStationNodes(id, []string{"N1", "N2"}); err != nil {
		t.Fatalf("set 1: %v", err)
	}
	names, err := db.GetStationNodeNames(id)
	if err != nil {
		t.Fatalf("get names: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("names len = %d", len(names))
	}

	// Verify runtime rows got created for each node.
	nodes, _ := db.ListProcessNodesByStation(id)
	if len(nodes) != 2 {
		t.Fatalf("nodes len = %d", len(nodes))
	}
	for _, n := range nodes {
		rt, err := db.GetProcessNodeRuntime(n.ID)
		if err != nil {
			t.Errorf("runtime missing for node %d: %v", n.ID, err)
		}
		if rt == nil {
			t.Errorf("runtime nil for node %d", n.ID)
		}
	}

	// Update: remove N1, add N3. N1 has no active orders, so it's deleted.
	if err := db.SetStationNodes(id, []string{"N2", "N3"}); err != nil {
		t.Fatalf("set 2: %v", err)
	}
	nodes2, _ := db.ListProcessNodesByStation(id)
	if len(nodes2) != 2 {
		t.Fatalf("nodes len after update = %d", len(nodes2))
	}
	nameSet := map[string]bool{}
	for _, n := range nodes2 {
		nameSet[n.CoreNodeName] = true
	}
	if !nameSet["N2"] || !nameSet["N3"] || nameSet["N1"] {
		t.Errorf("after update: %v", nameSet)
	}

	// Input with duplicates + whitespace — dedupe + trim paths.
	if err := db.SetStationNodes(id, []string{" N2 ", "N2", "", "N4"}); err != nil {
		t.Fatalf("set 3: %v", err)
	}
	nodes3, _ := db.ListProcessNodesByStation(id)
	if len(nodes3) != 2 { // N2, N4 — N3 removed
		t.Errorf("nodes after dedup = %d, want 2", len(nodes3))
	}
}

func TestOperatorStations_SetStationNodesDisablesRatherThanDeletesWhenOrdersActive(t *testing.T) {
	db := coverageDB(t)
	pid, _ := db.CreateProcess("P", "", "", "", "", false)
	id, _ := db.CreateOperatorStation(OperatorStationInput{ProcessID: pid, Name: "S"})

	db.SetStationNodes(id, []string{"N-KEEP"})
	nodes, _ := db.ListProcessNodesByStation(id)
	var nodeID int64
	for _, n := range nodes {
		if n.CoreNodeName == "N-KEEP" {
			nodeID = n.ID
		}
	}
	// Attach an active order to the node we're about to drop.
	_, err := db.CreateOrder("keep-me", "retrieve", &nodeID, false, 1, "", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}

	// Drop N-KEEP. Node should be disabled, not deleted.
	if err := db.SetStationNodes(id, []string{"N-NEW"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	n, err := db.GetProcessNode(nodeID)
	if err != nil {
		t.Fatalf("get node: %v (should still exist)", err)
	}
	if n.Enabled {
		t.Error("expected disabled=true for node with active orders")
	}
}

// ============================================================================
// process_nodes.go
// ============================================================================

func TestProcessNodes_CRUDAndListing(t *testing.T) {
	db := coverageDB(t)
	pid, _ := db.CreateProcess("P", "", "", "", "", false)
	sid, _ := db.CreateOperatorStation(OperatorStationInput{ProcessID: pid, Name: "S"})

	// Auto-code, auto-sequence, auto-name-from-core.
	id, err := db.CreateProcessNode(ProcessNodeInput{
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
	id2, _ := db.CreateProcessNode(ProcessNodeInput{
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
	if err := db.UpdateProcessNode(id, ProcessNodeInput{
		ProcessID: pid, CoreNodeName: "NODE-A", Name: "Renamed", Enabled: true,
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	gotU, _ := db.GetProcessNode(id)
	if gotU.Name != "Renamed" || gotU.Code != got.Code || gotU.Sequence != got.Sequence {
		t.Errorf("preservation after update: %+v (was %+v)", gotU, got)
	}

	if err := db.DeleteProcessNode(id2); err != nil {
		t.Fatalf("delete: %v", err)
	}
	after, _ := db.ListProcessNodes()
	if len(after) != 1 {
		t.Errorf("after delete = %d", len(after))
	}
}

func TestProcessNodes_InvalidStationIDCoercedToNil(t *testing.T) {
	db := coverageDB(t)
	pid, _ := db.CreateProcess("P", "", "", "", "", false)

	// Pass OperatorStationID pointer to 0 — create should coerce to nil.
	zero := int64(0)
	id, err := db.CreateProcessNode(ProcessNodeInput{
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
	db := coverageDB(t)
	pid, _ := db.CreateProcess("P", "", "", "", "", false)
	nid, _ := db.CreateProcessNode(ProcessNodeInput{
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
	if err := db.SetProcessNodeRuntime(nid, &cid, 75); err != nil {
		t.Fatalf("set: %v", err)
	}
	rt, _ := db.GetProcessNodeRuntime(nid)
	if rt.ActiveClaimID == nil || *rt.ActiveClaimID != 42 || rt.RemainingUOP != 75 {
		t.Errorf("after set: %+v", rt)
	}

	// UpdateProcessNodeRuntimeOrders.
	a, b := int64(100), int64(200)
	if err := db.UpdateProcessNodeRuntimeOrders(nid, &a, &b); err != nil {
		t.Fatalf("update orders: %v", err)
	}
	rt2b, _ := db.GetProcessNodeRuntime(nid)
	if rt2b.ActiveOrderID == nil || *rt2b.ActiveOrderID != 100 ||
		rt2b.StagedOrderID == nil || *rt2b.StagedOrderID != 200 {
		t.Errorf("orders: %+v", rt2b)
	}

	// UpdateProcessNodeUOP
	if err := db.UpdateProcessNodeUOP(nid, 30); err != nil {
		t.Fatalf("update uop: %v", err)
	}
	rt3, _ := db.GetProcessNodeRuntime(nid)
	if rt3.RemainingUOP != 30 {
		t.Errorf("uop = %d, want 30", rt3.RemainingUOP)
	}

	// SetActivePull
	if err := db.SetActivePull(nid, true); err != nil {
		t.Fatalf("set active pull: %v", err)
	}
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
	db := coverageDB(t)
	_, sid := seedProcessStyle(t, db, "P", "S")

	// Insert with defaults (role blanks → "consume", swap_mode blank → "simple").
	id, err := db.UpsertStyleNodeClaim(StyleNodeClaimInput{
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
	id2, _ := db.UpsertStyleNodeClaim(StyleNodeClaimInput{
		StyleID: sid, CoreNodeName: "N2", Role: "produce", PayloadCode: "PL-2",
	})
	got2, _ := db.GetStyleNodeClaim(id2)
	if got2.Sequence != 2 {
		t.Errorf("second sequence = %d", got2.Sequence)
	}

	// Upsert on existing (styleID + coreNodeName match) — returns same id, updates fields.
	id3, err := db.UpsertStyleNodeClaim(StyleNodeClaimInput{
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

func TestStyleNodeClaims_ManualSwapRequiresOutboundDestination(t *testing.T) {
	db := coverageDB(t)
	_, sid := seedProcessStyle(t, db, "P", "S")

	// Missing outbound_destination → error.
	_, err := db.UpsertStyleNodeClaim(StyleNodeClaimInput{
		StyleID: sid, CoreNodeName: "N", SwapMode: "manual_swap", PayloadCode: "PL",
	})
	if err == nil {
		t.Fatal("expected error for manual_swap without outbound_destination")
	}

	// With outbound_destination → auto-confirm is forced.
	id, err := db.UpsertStyleNodeClaim(StyleNodeClaimInput{
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
	claim := &StyleNodeClaim{}
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

func TestStyleNodeClaims_Delete(t *testing.T) {
	db := coverageDB(t)
	_, sid := seedProcessStyle(t, db, "P", "S")
	id, _ := db.UpsertStyleNodeClaim(StyleNodeClaimInput{
		StyleID: sid, CoreNodeName: "N", PayloadCode: "PL",
	})
	if err := db.DeleteStyleNodeClaim(id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := db.GetStyleNodeClaim(id); err == nil {
		t.Fatal("expected error after delete")
	}
}

// ============================================================================
// process_changeovers.go
// ============================================================================

func TestProcessChangeovers_CreateAtomic(t *testing.T) {
	db := coverageDB(t)
	pid, fromStyle := seedProcessStyle(t, db, "P", "S-FROM")
	toStyle, _ := db.CreateStyle("S-TO", "", pid)
	sid, _ := db.CreateOperatorStation(OperatorStationInput{ProcessID: pid, Name: "S"})
	// Seed an existing process node (the atomic create should reuse it
	// instead of auto-creating a duplicate).
	existNode, _ := db.CreateProcessNode(ProcessNodeInput{
		ProcessID: pid, OperatorStationID: &sid, CoreNodeName: "N-EXIST",
		Code: "NE", Name: "NE", Sequence: 1, Enabled: true,
	})
	existingNodes, _ := db.ListProcessNodesByProcess(pid)

	cid, err := db.CreateChangeover(
		pid, &fromStyle, toStyle, "alice", "swap A",
		[]int64{sid},
		[]ChangeoverNodeTaskInput{
			{ProcessID: pid, CoreNodeName: "N-EXIST", Situation: "refill", State: "waiting"},
			{ProcessID: pid, CoreNodeName: "N-NEW", Situation: "introduce", State: "waiting"},
		},
		existingNodes,
	)
	if err != nil {
		t.Fatalf("create changeover: %v", err)
	}
	if cid == 0 {
		t.Fatal("expected nonzero id")
	}

	// Process target style + production state updated.
	proc, _ := db.GetProcess(pid)
	if proc.TargetStyleID == nil || *proc.TargetStyleID != toStyle {
		t.Errorf("target style = %v, want %d", proc.TargetStyleID, toStyle)
	}
	if proc.ProductionState != "changeover_active" {
		t.Errorf("production_state = %q", proc.ProductionState)
	}

	// Station tasks created.
	stTasks, _ := db.ListChangeoverStationTasks(cid)
	if len(stTasks) != 1 || stTasks[0].OperatorStationID != sid || stTasks[0].State != "waiting" {
		t.Errorf("station tasks: %+v", stTasks)
	}

	// Node tasks: one reuses existing, one auto-creates.
	nodeTasks, _ := db.ListChangeoverNodeTasks(cid)
	if len(nodeTasks) != 2 {
		t.Fatalf("node tasks len = %d", len(nodeTasks))
	}
	var reusedExisting bool
	for _, nt := range nodeTasks {
		if nt.ProcessNodeID == existNode {
			reusedExisting = true
		}
	}
	if !reusedExisting {
		t.Error("expected the existing process node to be reused")
	}

	// Runtime rows created for every node task.
	for _, nt := range nodeTasks {
		if _, err := db.GetProcessNodeRuntime(nt.ProcessNodeID); err != nil {
			t.Errorf("runtime missing for node %d: %v", nt.ProcessNodeID, err)
		}
	}

	// Active changeover lookup.
	active, err := db.GetActiveProcessChangeover(pid)
	if err != nil || active.ID != cid {
		t.Errorf("active: %v %+v", err, active)
	}
	if active.FromStyleName != "S-FROM" || active.ToStyleName != "S-TO" {
		t.Errorf("joined style names: %+v", active)
	}
}

func TestProcessChangeovers_StateTransitionsAndListing(t *testing.T) {
	db := coverageDB(t)
	pid, fromStyle := seedProcessStyle(t, db, "P", "F")
	toStyle, _ := db.CreateStyle("T", "", pid)

	cid, err := db.CreateChangeover(pid, &fromStyle, toStyle, "x", "", nil, nil, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// In-flight state transitions.
	if err := db.UpdateProcessChangeoverState(cid, "phase_3"); err != nil {
		t.Fatalf("update state: %v", err)
	}

	// Second changeover to test listing and exclusion-by-state.
	cid2, _ := db.CreateChangeover(pid, &fromStyle, toStyle, "y", "", nil, nil, nil)
	_ = cid2

	list, _ := db.ListProcessChangeovers(pid)
	if len(list) != 2 {
		t.Errorf("list len = %d", len(list))
	}

	// Mark first as completed → GetActive returns the remaining one only.
	if err := db.UpdateProcessChangeoverState(cid, "completed"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	active, err := db.GetActiveProcessChangeover(pid)
	if err != nil {
		t.Fatalf("get active after complete: %v", err)
	}
	if active.ID == cid {
		t.Error("active still points at completed changeover")
	}

	// Completed changeover has completed_at populated.
	histList, _ := db.ListProcessChangeovers(pid)
	var completed *ProcessChangeover
	for i := range histList {
		if histList[i].ID == cid {
			completed = &histList[i]
		}
	}
	if completed == nil || completed.CompletedAt == nil {
		t.Errorf("expected completed_at to be populated: %+v", completed)
	}
}

func TestProcessChangeovers_NodeAndStationTaskMutations(t *testing.T) {
	db := coverageDB(t)
	pid, fromStyle := seedProcessStyle(t, db, "P", "F")
	toStyle, _ := db.CreateStyle("T", "", pid)
	sid, _ := db.CreateOperatorStation(OperatorStationInput{ProcessID: pid, Name: "S"})

	// We also want node tasks filtered by station.
	nid, _ := db.CreateProcessNode(ProcessNodeInput{
		ProcessID: pid, OperatorStationID: &sid, CoreNodeName: "N",
		Code: "N", Name: "N", Sequence: 1, Enabled: true,
	})
	existingNodes, _ := db.ListProcessNodesByProcess(pid)

	cid, err := db.CreateChangeover(pid, &fromStyle, toStyle, "x", "",
		[]int64{sid},
		[]ChangeoverNodeTaskInput{{ProcessID: pid, CoreNodeName: "N", Situation: "refill", State: "waiting"}},
		existingNodes)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Station task lookup + update.
	st, err := db.GetChangeoverStationTaskByStation(cid, sid)
	if err != nil {
		t.Fatalf("get station task: %v", err)
	}
	if err := db.UpdateChangeoverStationTaskState(st.ID, "in_progress"); err != nil {
		t.Fatalf("update station task: %v", err)
	}
	st2, _ := db.GetChangeoverStationTaskByStation(cid, sid)
	if st2.State != "in_progress" {
		t.Errorf("station task state = %q", st2.State)
	}

	// Node task lookup + update + link orders.
	nt, err := db.GetChangeoverNodeTaskByNode(cid, nid)
	if err != nil {
		t.Fatalf("get node task: %v", err)
	}
	if err := db.UpdateChangeoverNodeTaskState(nt.ID, "in_progress"); err != nil {
		t.Fatalf("update node task: %v", err)
	}

	// Link material orders.
	orderA, _ := db.CreateOrder("next", "retrieve", &nid, false, 1, "", "", "", "", false, "")
	orderB, _ := db.CreateOrder("old", "retrieve", &nid, false, 1, "", "", "", "", false, "")
	if err := db.LinkChangeoverNodeOrders(nt.ID, &orderA, &orderB); err != nil {
		t.Fatalf("link orders: %v", err)
	}
	nt2, _ := db.GetChangeoverNodeTaskByNode(cid, nid)
	if nt2.State != "in_progress" {
		t.Errorf("node task state = %q", nt2.State)
	}
	if nt2.NextMaterialOrderID == nil || *nt2.NextMaterialOrderID != orderA {
		t.Errorf("next material = %v", nt2.NextMaterialOrderID)
	}
	if nt2.OldMaterialReleaseOrderID == nil || *nt2.OldMaterialReleaseOrderID != orderB {
		t.Errorf("old material = %v", nt2.OldMaterialReleaseOrderID)
	}

	// Partial link (only old) — COALESCE keeps previous next.
	orderC, _ := db.CreateOrder("old2", "retrieve", &nid, false, 1, "", "", "", "", false, "")
	if err := db.LinkChangeoverNodeOrders(nt.ID, nil, &orderC); err != nil {
		t.Fatalf("partial link: %v", err)
	}
	nt3, _ := db.GetChangeoverNodeTaskByNode(cid, nid)
	if nt3.NextMaterialOrderID == nil || *nt3.NextMaterialOrderID != orderA {
		t.Errorf("partial link: next should be preserved: %v", nt3.NextMaterialOrderID)
	}
	if nt3.OldMaterialReleaseOrderID == nil || *nt3.OldMaterialReleaseOrderID != orderC {
		t.Errorf("partial link: old should be updated: %v", nt3.OldMaterialReleaseOrderID)
	}

	// ListChangeoverNodeTasksByStation — only node tasks for nodes attached to sid.
	byStation, err := db.ListChangeoverNodeTasksByStation(cid, sid)
	if err != nil {
		t.Fatalf("list by station: %v", err)
	}
	if len(byStation) != 1 {
		t.Errorf("by station = %d, want 1", len(byStation))
	}
}
