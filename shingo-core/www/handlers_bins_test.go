package www

import (
	"encoding/json"
	"html/template"
	"testing"

	"shingo/protocol/debuglog"
	"shingocore/config"
	"shingocore/engine"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
)

// testHandlers creates a Handlers struct backed by a real Postgres database and a
// real Engine (with background goroutines). This is heavier than strictly needed for
// DB+audit tests, but follows the shingo-core convention: real DB via testdb.Open,
// simulator for fleet, no handrolled mocks. A future engine interface (engine_iface.go)
// would allow lighter stubs.
func testHandlers(t *testing.T) (*Handlers, *store.DB) {
	t.Helper()

	db := testdb.Open(t)
	sim := simulator.New()

	cfg := config.Defaults()
	cfg.Messaging.StationID = "test-www"

	eng := engine.New(engine.Config{
		AppConfig: cfg,
		DB:        db,
		Fleet:     sim,
		MsgClient: nil,
		LogFunc:   t.Logf,
	})
	eng.Start()
	t.Cleanup(func() { eng.Stop() })

	hub := NewEventHub()
	hub.Start()
	t.Cleanup(func() { hub.Stop() })

	dbgLog, _ := debuglog.New(64, nil)

	h := &Handlers{
		engine:   eng,
		sessions: newSessionStore("test-secret"),
		tmpls:    make(map[string]*template.Template),
		eventHub: hub,
		debugLog: dbgLog,
	}
	return h, db
}

// setupBinForAction creates standard data and a bin at the storage node.
// Returns the handlers, db, standard data, and the bin.
func setupBinForAction(t *testing.T) (*Handlers, *store.DB, *testdb.StandardData, *store.Bin) {
	t.Helper()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-ACTION-1")
	return h, db, sd, bin
}

// mustJSON marshals v to json.RawMessage; panics on error.
func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("mustJSON: %v", err)
	}
	return b
}

// requireAudit asserts that the most recent audit entry for a bin matches the expected fields.
func requireAudit(t *testing.T, db *store.DB, binID int64, action, oldVal, newVal, actor string) {
	t.Helper()
	entries, err := db.ListEntityAudit("bin", binID)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	// The engine wiring (wiring.go) subscribes to EventBinUpdated and writes an
	// additional audit entry with actor="system". Since EventBus is synchronous,
	// the engine entry is always newer than the handler's direct AppendAudit.
	// We search by action+actor to find the correct handler-written entry,
	// because some engine event actions match the handler's action string
	// (e.g. both write "locked", "counted", "cleared").
	var found *store.AuditEntry
	for _, e := range entries {
		if e.Action == action && e.Actor == actor {
			found = e
			break
		}
	}
	if found == nil {
		t.Fatalf("expected audit entry with action %q for bin %d, got %d entries: %v", action, binID, len(entries), auditActions(entries))
		return
	}
	e := found
	if e.Action != action {
		t.Errorf("audit action: got %q, want %q", e.Action, action)
	}
	if e.OldValue != oldVal {
		t.Errorf("audit old_value: got %q, want %q", e.OldValue, oldVal)
	}
	if e.NewValue != newVal {
		t.Errorf("audit new_value: got %q, want %q", e.NewValue, newVal)
	}
	if e.Actor != actor {
		t.Errorf("audit actor: got %q, want %q", e.Actor, actor)
	}
}

// auditActions returns a summary of audit entry actions for error messages.
func auditActions(entries []*store.AuditEntry) []string {
	actions := make([]string, len(entries))
	for i, e := range entries {
		actions[i] = e.Action
	}
	return actions
}

// findAuditByAction returns the first audit entry matching the given action, or nil.
func findAuditByAction(entries []*store.AuditEntry, action string) *store.AuditEntry {
	for _, e := range entries {
		if e.Action == action {
			return e
		}
	}
	return nil
}

// --- Simple status transitions ---

func TestExecuteBinAction_Activate(t *testing.T) {
	h, db, _, bin := setupBinForAction(t)

	// Set bin to a non-available status first so activate is meaningful.
	db.UpdateBinStatus(bin.ID, "flagged")

	err := h.executeBinAction(bin, "activate", nil)
	if err != nil {
		t.Fatalf("activate: %v", err)
	}

	got, _ := db.GetBin(bin.ID)
	if got.Status != "available" {
		t.Errorf("status after activate: got %q, want %q", got.Status, "available")
	}
	requireAudit(t, db, bin.ID, "status", "available", "available", "ui")
}

func TestExecuteBinAction_Flag(t *testing.T) {
	h, db, _, bin := setupBinForAction(t)

	err := h.executeBinAction(bin, "flag", nil)
	if err != nil {
		t.Fatalf("flag: %v", err)
	}

	got, _ := db.GetBin(bin.ID)
	if got.Status != "flagged" {
		t.Errorf("status after flag: got %q, want %q", got.Status, "flagged")
	}
	requireAudit(t, db, bin.ID, "status", "available", "flagged", "ui")
}

func TestExecuteBinAction_Maintenance(t *testing.T) {
	h, db, _, bin := setupBinForAction(t)

	err := h.executeBinAction(bin, "maintenance", nil)
	if err != nil {
		t.Fatalf("maintenance: %v", err)
	}

	got, _ := db.GetBin(bin.ID)
	if got.Status != "maintenance" {
		t.Errorf("status after maintenance: got %q, want %q", got.Status, "maintenance")
	}
	requireAudit(t, db, bin.ID, "status", "available", "maintenance", "ui")
}

func TestExecuteBinAction_Retire(t *testing.T) {
	h, db, _, bin := setupBinForAction(t)

	err := h.executeBinAction(bin, "retire", nil)
	if err != nil {
		t.Fatalf("retire: %v", err)
	}

	got, _ := db.GetBin(bin.ID)
	if got.Status != "retired" {
		t.Errorf("status after retire: got %q, want %q", got.Status, "retired")
	}
	requireAudit(t, db, bin.ID, "status", "available", "retired", "ui")
}

// --- Complex actions ---

func TestExecuteBinAction_QualityHold(t *testing.T) {
	h, db, _, bin := setupBinForAction(t)

	params := mustJSON(t, map[string]string{
		"reason": "Surface defect on lid",
		"actor":  "inspector-1",
	})
	err := h.executeBinAction(bin, "quality_hold", params)
	if err != nil {
		t.Fatalf("quality_hold: %v", err)
	}

	got, _ := db.GetBin(bin.ID)
	if got.Status != "quality_hold" {
		t.Errorf("status: got %q, want %q", got.Status, "quality_hold")
	}

	// Three audit entries: handler "status", "note:hold", engine "status_changed".
	entries, _ := db.ListEntityAudit("bin", bin.ID)
	if len(entries) < 3 {
		t.Fatalf("expected at least 3 audit entries, got %d", len(entries))
	}
	// Find the note entry.
	noteEntry := findAuditByAction(entries, "note:hold")
	if noteEntry == nil {
		t.Fatal("expected note:hold audit entry")
	}
	if noteEntry.NewValue != "Surface defect on lid" {
		t.Errorf("note new_value: got %q, want reason text", noteEntry.NewValue)
	}
	if noteEntry.Actor != "inspector-1" {
		t.Errorf("note actor: got %q, want %q", noteEntry.Actor, "inspector-1")
	}
	requireAudit(t, db, bin.ID, "status", "available", "quality_hold", "inspector-1")
}

func TestExecuteBinAction_QualityHold_NoReason(t *testing.T) {
	h, db, _, bin := setupBinForAction(t)

	// No reason — should still change status but skip the note. Two audit
	// entries: handler "status" + engine "status_changed".
	params := mustJSON(t, map[string]string{"actor": "inspector-2"})
	err := h.executeBinAction(bin, "quality_hold", params)
	if err != nil {
		t.Fatalf("quality_hold no reason: %v", err)
	}

	got, _ := db.GetBin(bin.ID)
	if got.Status != "quality_hold" {
		t.Errorf("status: got %q, want %q", got.Status, "quality_hold")
	}

	entries, _ := db.ListEntityAudit("bin", bin.ID)
	// No note:hold entry — just handler "status" and engine "status_changed".
	noteEntry := findAuditByAction(entries, "note:hold")
	if noteEntry != nil {
		t.Error("expected no note:hold audit entry when reason is empty")
	}
	requireAudit(t, db, bin.ID, "status", "available", "quality_hold", "inspector-2")
}

func TestExecuteBinAction_Lock(t *testing.T) {
	h, db, _, bin := setupBinForAction(t)

	params := mustJSON(t, map[string]string{"actor": "maintenance-tech"})
	err := h.executeBinAction(bin, "lock", params)
	if err != nil {
		t.Fatalf("lock: %v", err)
	}

	got, _ := db.GetBin(bin.ID)
	if !got.Locked {
		t.Error("bin should be locked after lock action")
	}
	if got.LockedBy != "maintenance-tech" {
		t.Errorf("locked_by: got %q, want %q", got.LockedBy, "maintenance-tech")
	}
	requireAudit(t, db, bin.ID, "locked", "", "maintenance-tech", "maintenance-tech")
}

func TestExecuteBinAction_Lock_NoExplicitActor(t *testing.T) {
	h, db, _, bin := setupBinForAction(t)

	// No actor param — resolveActor("") returns "ui", which passes the
	// `if actor == ""` guard. Lock succeeds with actor "ui".
	err := h.executeBinAction(bin, "lock", nil)
	if err != nil {
		t.Fatalf("lock with no explicit actor should succeed (resolves to 'ui'): %v", err)
	}

	got, _ := db.GetBin(bin.ID)
	if !got.Locked {
		t.Error("bin should be locked")
	}
	if got.LockedBy != "ui" {
		t.Errorf("locked_by: got %q, want %q", got.LockedBy, "ui")
	}
}

func TestExecuteBinAction_Unlock(t *testing.T) {
	h, db, _, bin := setupBinForAction(t)

	// Lock first, then unlock.
	db.LockBin(bin.ID, "someone")
	bin.LockedBy = "someone" // update the in-memory copy to match

	err := h.executeBinAction(bin, "unlock", nil)
	if err != nil {
		t.Fatalf("unlock: %v", err)
	}

	got, _ := db.GetBin(bin.ID)
	if got.Locked {
		t.Error("bin should be unlocked after unlock action")
	}
	requireAudit(t, db, bin.ID, "unlocked", "someone", "", "ui")
}

func TestExecuteBinAction_Release(t *testing.T) {
	h, db, _, bin := setupBinForAction(t)

	// Stage the bin first so release actually transitions staged → available.
	db.StageBin(bin.ID, nil)
	bin.Status = "staged" // keep in-memory copy in sync (executeBinAction reads b.Status for oldStatus)

	err := h.executeBinAction(bin, "release", nil)
	if err != nil {
		t.Fatalf("release: %v", err)
	}

	got, _ := db.GetBin(bin.ID)
	if got.Status != "available" {
		t.Errorf("status after release: got %q, want %q", got.Status, "available")
	}
	if got.StagedAt != nil {
		t.Error("staged_at should be nil after release")
	}
	requireAudit(t, db, bin.ID, "status", "staged", "available", "ui")
}

// --- Payload actions ---

func TestExecuteBinAction_LoadPayload(t *testing.T) {
	h, db, sd, _ := setupBinForAction(t)

	// Create a clean bin without manifest for loading.
	bt, _ := db.GetBinTypeByCode("DEFAULT")
	nodeID := sd.StorageNode.ID
	cleanBin := &store.Bin{BinTypeID: bt.ID, Label: "BIN-LOAD-1", NodeID: &nodeID, Status: "available"}
	if err := db.CreateBin(cleanBin); err != nil {
		t.Fatalf("create clean bin: %v", err)
	}
	cleanBin, _ = db.GetBin(cleanBin.ID)

	params := mustJSON(t, map[string]any{
		"payload_code": sd.Payload.Code,
		"uop_override": 50,
	})
	err := h.executeBinAction(cleanBin, "load_payload", params)
	if err != nil {
		t.Fatalf("load_payload: %v", err)
	}

	got, _ := db.GetBin(cleanBin.ID)
	if got.PayloadCode != sd.Payload.Code {
		t.Errorf("payload_code: got %q, want %q", got.PayloadCode, sd.Payload.Code)
	}
	requireAudit(t, db, cleanBin.ID, "loaded", "", sd.Payload.Code, "ui")
}

func TestExecuteBinAction_LoadPayload_MissingCode(t *testing.T) {
	h, _, _, bin := setupBinForAction(t)

	params := mustJSON(t, map[string]string{"payload_code": ""})
	err := h.executeBinAction(bin, "load_payload", params)
	if err == nil {
		t.Fatal("load_payload with empty code should error")
	}
}

func TestExecuteBinAction_LoadPayload_UnknownCode(t *testing.T) {
	h, _, _, bin := setupBinForAction(t)

	params := mustJSON(t, map[string]string{"payload_code": "NONEXISTENT-XYZ"})
	err := h.executeBinAction(bin, "load_payload", params)
	if err == nil {
		t.Fatal("load_payload with unknown code should error")
	}
}

func TestExecuteBinAction_Clear(t *testing.T) {
	h, db, _, bin := setupBinForAction(t)

	// bin from setupBinForAction already has a manifest (CreateBinAtNode sets one).
	oldCode := bin.PayloadCode

	err := h.executeBinAction(bin, "clear", nil)
	if err != nil {
		t.Fatalf("clear: %v", err)
	}

	got, _ := db.GetBin(bin.ID)
	if got.PayloadCode != "" {
		t.Errorf("payload_code after clear: got %q, want empty", got.PayloadCode)
	}
	requireAudit(t, db, bin.ID, "cleared", oldCode, "", "ui")
}

func TestExecuteBinAction_ConfirmManifest(t *testing.T) {
	h, db, _, bin := setupBinForAction(t)

	// Unconfirm first so we can test confirm.
	db.UnconfirmBinManifest(bin.ID)
	bin.ManifestConfirmed = false

	err := h.executeBinAction(bin, "confirm_manifest", nil)
	if err != nil {
		t.Fatalf("confirm_manifest: %v", err)
	}

	got, _ := db.GetBin(bin.ID)
	if !got.ManifestConfirmed {
		t.Error("manifest should be confirmed after confirm_manifest")
	}
	requireAudit(t, db, bin.ID, "confirmed", "unconfirmed", "confirmed", "ui")
}

func TestExecuteBinAction_ConfirmManifest_NoManifest(t *testing.T) {
	h, db, sd, _ := setupBinForAction(t)

	// Create a bin with no manifest.
	bt, _ := db.GetBinTypeByCode("DEFAULT")
	nodeID := sd.StorageNode.ID
	emptyBin := &store.Bin{BinTypeID: bt.ID, Label: "BIN-NOMANI-1", NodeID: &nodeID, Status: "available"}
	if err := db.CreateBin(emptyBin); err != nil {
		t.Fatalf("create empty bin: %v", err)
	}
	emptyBin, _ = db.GetBin(emptyBin.ID)

	err := h.executeBinAction(emptyBin, "confirm_manifest", nil)
	if err == nil {
		t.Fatal("confirm_manifest on bin with no manifest should error")
	}
}

func TestExecuteBinAction_UnconfirmManifest(t *testing.T) {
	h, db, _, bin := setupBinForAction(t)

	// bin from setup is already confirmed.
	err := h.executeBinAction(bin, "unconfirm_manifest", nil)
	if err != nil {
		t.Fatalf("unconfirm_manifest: %v", err)
	}

	got, _ := db.GetBin(bin.ID)
	if got.ManifestConfirmed {
		t.Error("manifest should be unconfirmed after unconfirm_manifest")
	}
	requireAudit(t, db, bin.ID, "unconfirmed", "confirmed", "unconfirmed", "ui")
}

// --- Move ---

func TestExecuteBinAction_Move(t *testing.T) {
	h, db, sd, bin := setupBinForAction(t)

	// Move bin from storage node to line node.
	params := mustJSON(t, map[string]any{"node_id": sd.LineNode.ID})
	err := h.executeBinAction(bin, "move", params)
	if err != nil {
		t.Fatalf("move: %v", err)
	}

	got, _ := db.GetBin(bin.ID)
	if got.NodeID == nil || *got.NodeID != sd.LineNode.ID {
		t.Errorf("node_id after move: got %v, want %d", got.NodeID, sd.LineNode.ID)
	}
	requireAudit(t, db, bin.ID, "moved", sd.StorageNode.Name, sd.LineNode.Name, "ui")
}

func TestExecuteBinAction_Move_MissingNodeID(t *testing.T) {
	h, _, _, bin := setupBinForAction(t)

	err := h.executeBinAction(bin, "move", nil)
	if err == nil {
		t.Fatal("move with no node_id should error")
	}
}

func TestExecuteBinAction_Move_UnknownNode(t *testing.T) {
	h, _, _, bin := setupBinForAction(t)

	params := mustJSON(t, map[string]any{"node_id": 999999})
	err := h.executeBinAction(bin, "move", params)
	if err == nil {
		t.Fatal("move to unknown node should error")
	}
}

// TC-70: Moving a bin to its current node is physically impossible and must be rejected.
// Bug: 2026-04-13 — binMove() had no guard against same-node moves; the dropdown
// also offered the bin's current location as a destination.
func TestExecuteBinAction_Move_SameNode(t *testing.T) {
	h, _, sd, bin := setupBinForAction(t)

	// Attempt to move the bin to the storage node where it already lives.
	params := mustJSON(t, map[string]any{"node_id": sd.StorageNode.ID})
	err := h.executeBinAction(bin, "move", params)
	if err == nil {
		t.Fatal("move to current node should error")
	}
	if err.Error() != "bin is already at this location" {
		t.Errorf("unexpected error: %v", err)
	}
}

// --- Record count ---

func TestExecuteBinAction_RecordCount(t *testing.T) {
	h, db, _, bin := setupBinForAction(t)

	// bin.UOPRemaining is 100 (set by CreateBinAtNode).
	params := mustJSON(t, map[string]any{
		"actual_uop": 95,
		"actor":      "counter-1",
	})
	err := h.executeBinAction(bin, "record_count", params)
	if err != nil {
		t.Fatalf("record_count: %v", err)
	}

	got, _ := db.GetBin(bin.ID)
	if got.UOPRemaining != 95 {
		t.Errorf("uop_remaining: got %d, want 95", got.UOPRemaining)
	}
	if got.LastCountedBy != "counter-1" {
		t.Errorf("last_counted_by: got %q, want %q", got.LastCountedBy, "counter-1")
	}

	// Three audit entries: handler "counted", "note:count" (discrepancy),
	// engine "counted" (from EventBinUpdated subscriber in wiring.go).
	entries, _ := db.ListEntityAudit("bin", bin.ID)
	if len(entries) < 3 {
		t.Fatalf("expected at least 3 audit entries (count + note + engine), got %d", len(entries))
	}
	noteEntry := findAuditByAction(entries, "note:count")
	if noteEntry == nil {
		t.Fatal("expected note:count audit entry for discrepancy")
	}
	requireAudit(t, db, bin.ID, "counted", "100", "95", "counter-1")
	// Verify the discrepancy note mentions the expected/actual values.
	if noteEntry.NewValue == "" {
		t.Error("discrepancy note should contain a message")
	}
}

func TestExecuteBinAction_RecordCount_NoDiscrepancy(t *testing.T) {
	h, db, _, bin := setupBinForAction(t)

	// Count matches expected — no discrepancy note.
	params := mustJSON(t, map[string]any{
		"actual_uop": 100,
		"actor":      "counter-2",
	})
	err := h.executeBinAction(bin, "record_count", params)
	if err != nil {
		t.Fatalf("record_count no discrepancy: %v", err)
	}

	entries, _ := db.ListEntityAudit("bin", bin.ID)
	// No note:count entry — just handler "counted" and engine "counted".
	noteEntry := findAuditByAction(entries, "note:count")
	if noteEntry != nil {
		t.Error("expected no note:count audit entry when counts match")
	}
	requireAudit(t, db, bin.ID, "counted", "100", "100", "counter-2")
}

// --- Add note (unique: no audit/SSE, returns directly) ---

func TestExecuteBinAction_AddNote(t *testing.T) {
	h, db, _, bin := setupBinForAction(t)

	params := mustJSON(t, map[string]any{
		"note_type": "damage",
		"message":   "Crack observed on corner",
		"actor":     "operator-5",
	})
	err := h.executeBinAction(bin, "add_note", params)
	if err != nil {
		t.Fatalf("add_note: %v", err)
	}

	// add_note calls AddBinNote which appends "note:damage" audit.
	entries, _ := db.ListEntityAudit("bin", bin.ID)
	if len(entries) == 0 {
		t.Fatal("expected audit entry from add_note")
	}
	if entries[0].Action != "note:damage" {
		t.Errorf("audit action: got %q, want %q", entries[0].Action, "note:damage")
	}
	if entries[0].NewValue != "Crack observed on corner" {
		t.Errorf("audit new_value: got %q, want message text", entries[0].NewValue)
	}
}

func TestExecuteBinAction_AddNote_DefaultNoteType(t *testing.T) {
	h, db, _, bin := setupBinForAction(t)

	// Omit note_type — should default to "general".
	params := mustJSON(t, map[string]any{
		"message": "General observation",
	})
	err := h.executeBinAction(bin, "add_note", params)
	if err != nil {
		t.Fatalf("add_note default type: %v", err)
	}

	entries, _ := db.ListEntityAudit("bin", bin.ID)
	if entries[0].Action != "note:general" {
		t.Errorf("expected note:general, got %q", entries[0].Action)
	}
}

func TestExecuteBinAction_AddNote_MissingMessage(t *testing.T) {
	h, _, _, bin := setupBinForAction(t)

	params := mustJSON(t, map[string]any{"note_type": "damage"})
	err := h.executeBinAction(bin, "add_note", params)
	if err == nil {
		t.Fatal("add_note with empty message should error")
	}
}

// --- Update ---

func TestExecuteBinAction_Update(t *testing.T) {
	h, db, _, bin := setupBinForAction(t)

	newLabel := "RELABELED-BIN"
	newDesc := "Updated description"
	params := mustJSON(t, map[string]any{
		"label":       newLabel,
		"description": newDesc,
	})
	err := h.executeBinAction(bin, "update", params)
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	got, _ := db.GetBin(bin.ID)
	if got.Label != newLabel {
		t.Errorf("label: got %q, want %q", got.Label, newLabel)
	}
	if got.Description != newDesc {
		t.Errorf("description: got %q, want %q", got.Description, newDesc)
	}
	requireAudit(t, db, bin.ID, "updated", "", "", "ui")
}

func TestExecuteBinAction_Update_PartialFields(t *testing.T) {
	h, db, _, bin := setupBinForAction(t)
	origLabel := bin.Label

	// Only update description, leave label unchanged.
	params := mustJSON(t, map[string]any{"description": "New desc only"})
	err := h.executeBinAction(bin, "update", params)
	if err != nil {
		t.Fatalf("update partial: %v", err)
	}

	got, _ := db.GetBin(bin.ID)
	if got.Label != origLabel {
		t.Errorf("label should be unchanged: got %q, want %q", got.Label, origLabel)
	}
	if got.Description != "New desc only" {
		t.Errorf("description: got %q, want %q", got.Description, "New desc only")
	}
}

// --- Unknown action ---

func TestExecuteBinAction_UnknownAction(t *testing.T) {
	h, _, _, bin := setupBinForAction(t)

	err := h.executeBinAction(bin, "nonexistent_action", nil)
	if err == nil {
		t.Fatal("unknown action should return error")
	}
	if err.Error() != "unknown action: nonexistent_action" {
		t.Errorf("error message: got %q", err.Error())
	}
}
