//go:build docker

package engine

import (
	"encoding/json"
	"testing"
	"time"

	"shingocore/fleet/simulator"
	"shingocore/store"
)

// cms_transactions_test.go — coverage for cms_transactions.go.
//
// All three functions on *Engine are thin wrappers around the
// shingocore/material package. The unit-level boundary walk and
// transaction-builder tests live in shingocore/material; these
// integration tests prove the persistence + event-emission layer:
// rows actually land in cms_transactions and EventCMSTransaction
// fires on the bus.

// makeCMSBoundary creates a synthetic node with log_cms_transactions=true
// and a child storage slot under it. Returns (boundary, child) so callers
// can move bins between two such trees.
func makeCMSBoundary(t *testing.T, db *store.DB, name string) (*store.Node, *store.Node) {
	t.Helper()
	root := &store.Node{Name: name + "-ROOT", IsSynthetic: true, Enabled: true}
	if err := db.CreateNode(root); err != nil {
		t.Fatalf("create root %s: %v", name, err)
	}
	if err := db.SetNodeProperty(root.ID, "log_cms_transactions", "true"); err != nil {
		t.Fatalf("set boundary prop on %s: %v", name, err)
	}
	child := &store.Node{Name: name + "-SLOT", Enabled: true, ParentID: &root.ID}
	if err := db.CreateNode(child); err != nil {
		t.Fatalf("create child %s: %v", name, err)
	}
	// Re-fetch so joined fields are populated.
	root, _ = db.GetNode(root.ID)
	child, _ = db.GetNode(child.ID)
	return root, child
}

// putManifest gives a bin a single-line manifest so movement transactions
// have something to count.
func putManifest(t *testing.T, db *store.DB, binID int64, payloadCode, catID string, qty int64) {
	t.Helper()
	m := store.BinManifest{Items: []store.ManifestEntry{{CatID: catID, Quantity: qty}}}
	data, _ := json.Marshal(m)
	if err := db.SetBinManifest(binID, string(data), payloadCode, 100); err != nil {
		t.Fatalf("set bin manifest: %v", err)
	}
}

// ── FindCMSBoundary ─────────────────────────────────────────────────

// TestFindCMSBoundary_LogsAtSyntheticRoot covers the happy path: the
// wrapper returns the synthetic ancestor that has the boundary property.
func TestFindCMSBoundary_LogsAtSyntheticRoot(t *testing.T) {
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	root, child := makeCMSBoundary(t, db, "AREA-A")

	got := eng.FindCMSBoundary(child.ID)
	if got == nil {
		t.Fatalf("expected boundary, got nil")
	}
	if got.ID != root.ID {
		t.Errorf("boundary id = %d, want %d (root of tree)", got.ID, root.ID)
	}
}

// TestFindCMSBoundary_NoBoundary_NonSyntheticChain returns nil when no
// synthetic ancestor exists — matches the wrapper's collapse-to-nil
// contract for "no logging here".
func TestFindCMSBoundary_NoBoundary_NonSyntheticChain(t *testing.T) {
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	// Plain non-synthetic node with no synthetic parent.
	n := &store.Node{Name: "PLAIN-1", Enabled: true}
	if err := db.CreateNode(n); err != nil {
		t.Fatalf("create node: %v", err)
	}

	if got := eng.FindCMSBoundary(n.ID); got != nil {
		t.Errorf("expected nil boundary for plain node, got %+v", got)
	}
}

// TestFindCMSBoundary_StoreError_CollapsedToNil confirms the wrapper
// swallows the underlying material error (cycle or store miss) so the
// rest of the engine sees a single nil-return contract.
func TestFindCMSBoundary_StoreError_CollapsedToNil(t *testing.T) {
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	// Non-existent node — store returns "not found" → wrapper returns nil.
	if got := eng.FindCMSBoundary(99999); got != nil {
		t.Errorf("expected nil for missing node, got %+v", got)
	}
}

// ── RecordMovementTransactions ──────────────────────────────────────

// TestRecordMovementTransactions_PersistsAndEmits drives a real bin move
// across two CMS boundaries and asserts (a) cms_transactions rows are
// inserted by the wrapper and (b) EventCMSTransaction is published.
func TestRecordMovementTransactions_PersistsAndEmits(t *testing.T) {
	db := testDB(t)
	_, _, bp := setupTestData(t, db)
	eng := newTestEngine(t, db, simulator.New())

	srcRoot, srcSlot := makeCMSBoundary(t, db, "SRC")
	dstRoot, dstSlot := makeCMSBoundary(t, db, "DST")

	bin := createTestBinAtNode(t, db, bp.Code, srcSlot.ID, "BIN-MV-1")
	putManifest(t, db, bin.ID, bp.Code, "PART-A", 5)

	// Subscribe BEFORE invoking so we capture the emitted event.
	captured := make(chan Event, 4)
	eng.Events.SubscribeTypes(func(evt Event) { captured <- evt }, EventCMSTransaction)

	eng.RecordMovementTransactions(BinUpdatedEvent{
		BinID:      bin.ID,
		FromNodeID: srcSlot.ID,
		ToNodeID:   dstSlot.ID,
		PayloadCode: bp.Code,
		Action:     "moved",
	})

	// Two rows expected: one decrement at src boundary, one increment at dst.
	srcRows, err := db.ListCMSTransactions(srcRoot.ID, 10, 0)
	if err != nil {
		t.Fatalf("list src txns: %v", err)
	}
	if len(srcRows) != 1 {
		t.Errorf("src txns = %d, want 1: %+v", len(srcRows), srcRows)
	} else if srcRows[0].Delta != -5 || srcRows[0].TxnType != "decrease" {
		t.Errorf("src txn = %+v, want delta=-5 decrease", srcRows[0])
	}

	dstRows, err := db.ListCMSTransactions(dstRoot.ID, 10, 0)
	if err != nil {
		t.Fatalf("list dst txns: %v", err)
	}
	if len(dstRows) != 1 {
		t.Errorf("dst txns = %d, want 1: %+v", len(dstRows), dstRows)
	} else if dstRows[0].Delta != 5 || dstRows[0].TxnType != "increase" {
		t.Errorf("dst txn = %+v, want delta=5 increase", dstRows[0])
	}

	// Bus emission. We expect exactly one EventCMSTransaction; drain the channel.
	select {
	case evt := <-captured:
		payload, ok := evt.Payload.(CMSTransactionEvent)
		if !ok {
			t.Fatalf("event payload type = %T", evt.Payload)
		}
		if len(payload.Transactions) != 2 {
			t.Errorf("event txn count = %d, want 2", len(payload.Transactions))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("EventCMSTransaction not emitted within timeout")
	}
}

// TestRecordMovementTransactions_NoOpWhenSameBoundary confirms the
// wrapper short-circuits cleanly: no rows persisted, no event emitted.
func TestRecordMovementTransactions_NoOpWhenSameBoundary(t *testing.T) {
	db := testDB(t)
	_, _, bp := setupTestData(t, db)
	eng := newTestEngine(t, db, simulator.New())

	root, slotA := makeCMSBoundary(t, db, "ONE")
	// A second slot under the SAME boundary root.
	slotB := &store.Node{Name: "ONE-SLOT2", Enabled: true, ParentID: &root.ID}
	if err := db.CreateNode(slotB); err != nil {
		t.Fatalf("create slotB: %v", err)
	}

	bin := createTestBinAtNode(t, db, bp.Code, slotA.ID, "BIN-MV-2")
	putManifest(t, db, bin.ID, bp.Code, "PART-A", 3)

	emitted := false
	eng.Events.SubscribeTypes(func(Event) { emitted = true }, EventCMSTransaction)

	eng.RecordMovementTransactions(BinUpdatedEvent{
		BinID:      bin.ID,
		FromNodeID: slotA.ID,
		ToNodeID:   slotB.ID,
	})

	rows, _ := db.ListCMSTransactions(root.ID, 10, 0)
	if len(rows) != 0 {
		t.Errorf("expected no txns on same-boundary move, got %d", len(rows))
	}
	if emitted {
		t.Error("EventCMSTransaction must not fire on same-boundary move")
	}
}

// ── RecordCorrectionTransactions ────────────────────────────────────

// TestRecordCorrectionTransactions_PersistsAndEmits exercises the
// correction-diff path: an old manifest with one CatID is replaced with
// a different quantity → exactly one adjust row is logged at the
// boundary, and the bus event fires.
func TestRecordCorrectionTransactions_PersistsAndEmits(t *testing.T) {
	db := testDB(t)
	_, _, bp := setupTestData(t, db)
	eng := newTestEngine(t, db, simulator.New())

	root, slot := makeCMSBoundary(t, db, "CORR")
	bin := createTestBinAtNode(t, db, bp.Code, slot.ID, "BIN-CORR-1")
	putManifest(t, db, bin.ID, bp.Code, "PART-A", 4)

	captured := make(chan Event, 2)
	eng.Events.SubscribeTypes(func(evt Event) { captured <- evt }, EventCMSTransaction)

	old := []store.ManifestEntry{{CatID: "PART-A", Quantity: 4}}
	new := []store.ManifestEntry{{CatID: "PART-A", Quantity: 7}}
	eng.RecordCorrectionTransactions(bin.ID, slot.ID, old, new, "operator drift fix")

	rows, err := db.ListCMSTransactions(root.ID, 10, 0)
	if err != nil {
		t.Fatalf("list correction txns: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("correction txns = %d, want 1: %+v", len(rows), rows)
	}
	if rows[0].Delta != 3 || rows[0].TxnType != "increase" {
		t.Errorf("correction row = %+v, want delta=3 increase", rows[0])
	}
	if rows[0].SourceType != "correction" {
		t.Errorf("source_type = %q, want correction", rows[0].SourceType)
	}

	select {
	case evt := <-captured:
		p, ok := evt.Payload.(CMSTransactionEvent)
		if !ok || len(p.Transactions) != 1 {
			t.Errorf("event payload = %+v", evt.Payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("EventCMSTransaction not emitted for correction")
	}
}

// TestRecordCorrectionTransactions_NoOpWhenManifestUnchanged confirms
// the wrapper short-circuits: same in == same out → zero rows, no event.
func TestRecordCorrectionTransactions_NoOpWhenManifestUnchanged(t *testing.T) {
	db := testDB(t)
	_, _, bp := setupTestData(t, db)
	eng := newTestEngine(t, db, simulator.New())

	root, slot := makeCMSBoundary(t, db, "NOOP")
	bin := createTestBinAtNode(t, db, bp.Code, slot.ID, "BIN-CORR-2")
	putManifest(t, db, bin.ID, bp.Code, "PART-A", 4)

	emitted := false
	eng.Events.SubscribeTypes(func(Event) { emitted = true }, EventCMSTransaction)

	same := []store.ManifestEntry{{CatID: "PART-A", Quantity: 4}}
	eng.RecordCorrectionTransactions(bin.ID, slot.ID, same, same, "no change")

	rows, _ := db.ListCMSTransactions(root.ID, 10, 0)
	if len(rows) != 0 {
		t.Errorf("expected no txns on identity correction, got %d", len(rows))
	}
	if emitted {
		t.Error("EventCMSTransaction must not fire on identity correction")
	}
}
