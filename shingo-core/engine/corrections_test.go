//go:build docker

package engine

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"shingocore/fleet/simulator"
	"shingocore/store"
)

// corrections_test.go — coverage for corrections.go.
//
// Exercises ApplyCorrection (add_item, remove_item, adjust_qty) and
// ApplyBatchCorrection (diffing + no-op). Every happy-path test reads
// the bin back from the DB and asserts the manifest + a corrections
// row landed; error-path tests assert the wrapped error surfaces.

func TestApplyCorrection_AddItem_PersistsManifestAndRecord(t *testing.T) {
	db := testDB(t)
	_, _, bp := setupTestData(t, db)
	eng := newTestEngine(t, db, simulator.New())

	node := &store.Node{Name: "CORR-NODE-ADD", Enabled: true}
	if err := db.CreateNode(node); err != nil {
		t.Fatalf("create node: %v", err)
	}
	bin := createTestBinAtNode(t, db, bp.Code, node.ID, "BIN-ADD")

	// Capture emitted CorrectionApplied event to prove side effects wired up.
	captured := make(chan CorrectionAppliedEvent, 2)
	eng.Events.SubscribeTypes(func(evt Event) {
		captured <- evt.Payload.(CorrectionAppliedEvent)
	}, EventCorrectionApplied)

	id, err := eng.ApplyCorrection(ApplyCorrectionRequest{
		CorrectionType: "add_item",
		NodeID:         node.ID,
		BinID:          bin.ID,
		CatID:          "PART-NEW",
		Quantity:       7,
		Reason:         "inventory audit",
		Actor:          "op1",
	})
	if err != nil {
		t.Fatalf("ApplyCorrection: %v", err)
	}
	if id == 0 {
		t.Error("expected non-zero correction id")
	}

	// Verify manifest now contains PART-NEW.
	mf, err := db.GetBinManifest(bin.ID)
	if err != nil {
		t.Fatalf("GetBinManifest: %v", err)
	}
	found := false
	for _, e := range mf.Items {
		if e.CatID == "PART-NEW" && e.Quantity == 7 {
			found = true
			if !strings.Contains(e.Notes, "inventory audit") {
				t.Errorf("correction notes = %q, want reason string", e.Notes)
			}
		}
	}
	if !found {
		t.Errorf("PART-NEW qty=7 not in manifest: %+v", mf.Items)
	}

	// Verify correction row persisted.
	rows, _ := db.ListCorrectionsByNode(node.ID, 10)
	if len(rows) != 1 || rows[0].CorrectionType != "add_item" || rows[0].CatID != "PART-NEW" {
		t.Errorf("correction rows = %+v", rows)
	}

	// Verify event fired.
	select {
	case ev := <-captured:
		if ev.CorrectionType != "add_item" || ev.Actor != "op1" {
			t.Errorf("event payload = %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("EventCorrectionApplied not emitted")
	}
}

func TestApplyCorrection_AdjustQty_UpdatesExistingEntry(t *testing.T) {
	db := testDB(t)
	_, _, bp := setupTestData(t, db)
	eng := newTestEngine(t, db, simulator.New())

	node := &store.Node{Name: "CORR-NODE-ADJ", Enabled: true}
	if err := db.CreateNode(node); err != nil {
		t.Fatalf("create node: %v", err)
	}
	bin := createTestBinAtNode(t, db, bp.Code, node.ID, "BIN-ADJ")
	// Seed an existing manifest entry.
	putManifest(t, db, bin.ID, bp.Code, "PART-ADJ", 5)

	if _, err := eng.ApplyCorrection(ApplyCorrectionRequest{
		CorrectionType: "adjust_qty",
		NodeID:         node.ID,
		BinID:          bin.ID,
		CatID:          "PART-ADJ",
		Quantity:       12,
		Reason:         "recount",
		Actor:          "op2",
	}); err != nil {
		t.Fatalf("ApplyCorrection adjust: %v", err)
	}

	mf, _ := db.GetBinManifest(bin.ID)
	var got int64 = -1
	for _, e := range mf.Items {
		if e.CatID == "PART-ADJ" {
			got = e.Quantity
		}
	}
	if got != 12 {
		t.Errorf("adjusted qty = %d, want 12", got)
	}
}

func TestApplyCorrection_RemoveItem_DropsEntry(t *testing.T) {
	db := testDB(t)
	_, _, bp := setupTestData(t, db)
	eng := newTestEngine(t, db, simulator.New())

	node := &store.Node{Name: "CORR-NODE-RM", Enabled: true}
	if err := db.CreateNode(node); err != nil {
		t.Fatalf("create node: %v", err)
	}
	bin := createTestBinAtNode(t, db, bp.Code, node.ID, "BIN-RM")
	putManifest(t, db, bin.ID, bp.Code, "PART-RM", 2)

	if _, err := eng.ApplyCorrection(ApplyCorrectionRequest{
		CorrectionType: "remove_item",
		NodeID:         node.ID,
		BinID:          bin.ID,
		CatID:          "PART-RM",
		Reason:         "scrap",
		Actor:          "op3",
	}); err != nil {
		t.Fatalf("ApplyCorrection remove: %v", err)
	}

	mf, _ := db.GetBinManifest(bin.ID)
	for _, e := range mf.Items {
		if e.CatID == "PART-RM" {
			t.Errorf("PART-RM should have been removed, manifest still has: %+v", mf.Items)
		}
	}
}

// TestApplyCorrection_MissingBin surfaces the error-path wrap when GetBin fails.
func TestApplyCorrection_MissingBin(t *testing.T) {
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	_, err := eng.ApplyCorrection(ApplyCorrectionRequest{
		CorrectionType: "add_item",
		NodeID:         1,
		BinID:          999999, // no such bin
		CatID:          "X",
	})
	if err == nil {
		t.Fatal("expected error for missing bin, got nil")
	}
	if !strings.Contains(err.Error(), "get bin") {
		t.Errorf("error = %v, want wrap containing %q", err, "get bin")
	}
}

// ── ApplyBatchCorrection ────────────────────────────────────────────

func TestApplyBatchCorrection_DiffsAndEmits(t *testing.T) {
	db := testDB(t)
	_, _, bp := setupTestData(t, db)
	eng := newTestEngine(t, db, simulator.New())

	// Make a boundary so the correction transactions path runs clean.
	root, slot := makeCMSBoundary(t, db, "BATCH")
	bin := createTestBinAtNode(t, db, bp.Code, slot.ID, "BIN-BATCH")
	// Seed old manifest: P1=5, P2=3.
	putManifest(t, db, bin.ID, bp.Code, "P1", 5)
	// Second entry via a full rewrite.
	m := store.BinManifest{Items: []store.ManifestEntry{
		{CatID: "P1", Quantity: 5},
		{CatID: "P2", Quantity: 3},
	}}
	data, _ := json.Marshal(m)
	if err := db.SetBinManifest(bin.ID, string(data), bp.Code, 100); err != nil {
		t.Fatalf("reset manifest: %v", err)
	}

	// Capture events.
	var appliedSeen, binUpdatedSeen bool
	eng.Events.SubscribeTypes(func(evt Event) { appliedSeen = true }, EventCorrectionApplied)
	eng.Events.SubscribeTypes(func(evt Event) {
		ev := evt.Payload.(BinUpdatedEvent)
		if ev.Action == "batch_correction" {
			binUpdatedSeen = true
		}
	}, EventBinUpdated)

	// Submit a new manifest: P1=8 (adjust), P2 removed, P3=4 (add).
	err := eng.ApplyBatchCorrection(BatchCorrectionRequest{
		BinID:  bin.ID,
		NodeID: slot.ID,
		Reason: "full recount",
		Actor:  "op4",
		Items: []BatchCorrectionItem{
			{CatID: "P1", Quantity: 8},
			{CatID: "P3", Quantity: 4},
		},
	})
	if err != nil {
		t.Fatalf("ApplyBatchCorrection: %v", err)
	}

	// Verify manifest replaced entirely.
	mf, _ := db.GetBinManifest(bin.ID)
	byCat := map[string]int64{}
	for _, e := range mf.Items {
		byCat[e.CatID] = e.Quantity
	}
	if byCat["P1"] != 8 || byCat["P3"] != 4 {
		t.Errorf("batch-updated manifest = %+v", byCat)
	}
	if _, stillThere := byCat["P2"]; stillThere {
		t.Errorf("P2 should be removed, got %+v", byCat)
	}

	// Corrections table should have 3 rows (P1 adjust, P2 remove, P3 add).
	rows, _ := db.ListCorrectionsByNode(slot.ID, 10)
	if len(rows) != 3 {
		t.Errorf("correction rows = %d, want 3: %+v", len(rows), rows)
	}

	// CMS correction transactions also persisted against the boundary.
	txns, _ := db.ListCMSTransactions(root.ID, 10, 0)
	if len(txns) == 0 {
		t.Errorf("expected CMS correction txns at boundary, got none")
	}

	// Give synchronous bus a moment — Emit is synchronous so flags are set.
	if !appliedSeen {
		t.Error("EventCorrectionApplied not emitted")
	}
	if !binUpdatedSeen {
		t.Error("EventBinUpdated (batch_correction) not emitted")
	}
}

// TestApplyBatchCorrection_NoChanges short-circuits before touching the DB.
func TestApplyBatchCorrection_NoChanges(t *testing.T) {
	db := testDB(t)
	_, _, bp := setupTestData(t, db)
	eng := newTestEngine(t, db, simulator.New())

	node := &store.Node{Name: "BATCH-NOOP", Enabled: true}
	if err := db.CreateNode(node); err != nil {
		t.Fatalf("create node: %v", err)
	}
	bin := createTestBinAtNode(t, db, bp.Code, node.ID, "BIN-NOOP")
	putManifest(t, db, bin.ID, bp.Code, "P1", 5)

	emitted := false
	eng.Events.SubscribeTypes(func(Event) { emitted = true }, EventCorrectionApplied)

	err := eng.ApplyBatchCorrection(BatchCorrectionRequest{
		BinID:  bin.ID,
		NodeID: node.ID,
		Reason: "no change",
		Actor:  "op5",
		Items:  []BatchCorrectionItem{{CatID: "P1", Quantity: 5}},
	})
	if err != nil {
		t.Fatalf("ApplyBatchCorrection: %v", err)
	}

	rows, _ := db.ListCorrectionsByNode(node.ID, 10)
	if len(rows) != 0 {
		t.Errorf("expected no correction rows on identity diff, got %d", len(rows))
	}
	if emitted {
		t.Error("EventCorrectionApplied must not fire on identity diff")
	}
}

// TestApplyBatchCorrection_MissingBin — validation error path.
func TestApplyBatchCorrection_MissingBin(t *testing.T) {
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	err := eng.ApplyBatchCorrection(BatchCorrectionRequest{
		BinID:  999999,
		NodeID: 1,
		Reason: "bad",
		Actor:  "op",
		Items:  []BatchCorrectionItem{{CatID: "X", Quantity: 1}},
	})
	if err == nil {
		t.Fatal("expected error for missing bin")
	}
	if !strings.Contains(err.Error(), "get bin") {
		t.Errorf("error = %v, want wrap containing %q", err, "get bin")
	}
}

