//go:build docker

package engine

import (
	"encoding/json"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingocore/config"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/material"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/cms"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/payloads"
)

// cms_transactions_test.go — coverage for cms_transactions.go.
//
// All three functions on *Engine are thin wrappers around the
// shingocore/material package. The unit-level boundary walk and
// transaction-builder tests live in shingocore/material; these
// integration tests prove the persistence + event-emission layer:
// rows actually land in cms_transactions and EventCMSTransaction
// fires on the bus.

// makeCMSBoundary creates a synthetic node tagged with a cms_storeroom code
// and a child storage slot under it. Returns (boundary, child) so callers
// can move bins between two such trees.
//
// The tag is not optional scenery. A synthetic root used to be a boundary by
// default; it is not any more, and an untagged fixture emits nothing at all.
func makeCMSBoundary(t *testing.T, db *store.DB, name string) (*nodes.Node, *nodes.Node) {
	t.Helper()
	root := &nodes.Node{Name: name + "-ROOT", IsSynthetic: true, Enabled: true}
	if err := db.CreateNode(root); err != nil {
		t.Fatalf("create root %s: %v", name, err)
	}
	if err := db.SetNodeProperty(root.ID, material.CMSStoreroomProperty, name+"-STOREROOM"); err != nil {
		t.Fatalf("set boundary prop on %s: %v", name, err)
	}
	child := &nodes.Node{Name: name + "-SLOT", Enabled: true, ParentID: &root.ID}
	if err := db.CreateNode(child); err != nil {
		t.Fatalf("create child %s: %v", name, err)
	}
	// Re-fetch so joined fields are populated.
	root, _ = db.GetNode(root.ID)
	child, _ = db.GetNode(child.ID)
	return root, child
}

// putManifest gives a bin a single-line manifest AND the payload template line
// that says how to count it, so movement transactions have something to count.
//
// Both halves are needed and neither is optional. The bin manifest names the
// part; the template's parts_per_cycle is the only place the ratio lives, and a
// bin whose payload has no template line for its part contributes no CMS rows
// at all. Seeding one without the other produced a fixture that silently
// emitted nothing.
//
// The count the movement will carry is uop x perCycle.
func putManifest(t *testing.T, db *store.DB, binID int64, payloadCode, catID string, uop int, perCycle int64) {
	t.Helper()
	m := bins.Manifest{Items: []bins.ManifestEntry{{PartNumber: catID}}}
	data, _ := json.Marshal(m)
	testutil.MustNoErr(t, db.SetBinManifest(binID, string(data), payloadCode, uop), "set bin manifest")

	p, err := db.GetPayloadByCode(payloadCode)
	if err != nil {
		t.Fatalf("payload %q for manifest fixture: %v", payloadCode, err)
	}
	testutil.MustNoErr(t, db.CreatePayloadManifestItem(&payloads.ManifestItem{
		PayloadID: p.ID, PartNumber: catID, PartsPerCycle: perCycle,
	}, ""), "seed payload template line")
}

// ── FindCMSBoundary ─────────────────────────────────────────────────
//
// These call material.FindCMSBoundary against a real Postgres rather
// than the material package's fake store. The *Engine wrapper they used
// to call is gone; it collapsed store errors to a nil node, and the
// third test here used to pin that collapse as if it were the contract.

// TestFindCMSBoundary_LogsAtSyntheticRoot covers the happy path: the
// walk returns the synthetic ancestor that has the boundary property.
func TestFindCMSBoundary_LogsAtSyntheticRoot(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	root, child := makeCMSBoundary(t, db, "AREA-A")

	got, code, err := material.FindCMSBoundary(db, child.ID)
	if err != nil {
		t.Fatalf("boundary walk: %v", err)
	}
	if got == nil {
		t.Fatalf("expected boundary, got nil")
	}
	if got.ID != root.ID {
		t.Errorf("boundary id = %d, want %d (root of tree)", got.ID, root.ID)
	}
	if code != "AREA-A-STOREROOM" {
		t.Errorf("storeroom = %q, want the property's value", code)
	}
}

// TestFindCMSBoundary_NoBoundary_NonSyntheticChain returns (nil, nil)
// when no synthetic ancestor exists — "no logging here" is not an error.
func TestFindCMSBoundary_NoBoundary_NonSyntheticChain(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	// Plain non-synthetic node with no synthetic parent.
	n := &nodes.Node{Name: "PLAIN-1", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(n), "create node")

	got, code, err := material.FindCMSBoundary(db, n.ID)
	if err != nil {
		t.Fatalf("boundary walk: %v", err)
	}
	if got != nil || code != "" {
		t.Errorf("expected nil boundary for plain node, got %+v / %q", got, code)
	}
}

// TestFindCMSBoundary_StoreErrorPropagates is the inverse of the test
// this replaces. A store failure must reach the caller: "the lookup
// failed" and "there is no boundary here" are different answers, and
// collapsing them emitted zero CMS rows for real physical moves.
func TestFindCMSBoundary_StoreErrorPropagates(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	// Non-existent node — the store's "not found" must come back as an error.
	got, code, err := material.FindCMSBoundary(db, 99999)
	if err == nil {
		t.Fatalf("expected error for missing node, got nil (node %+v)", got)
	}
	if got != nil || code != "" {
		t.Errorf("expected nil node and empty code alongside the error, got %+v / %q", got, code)
	}
}

// ── RecordMovementTransactions ──────────────────────────────────────

// TestRecordMovementTransactions_PersistsAndEmits drives a real bin move
// across two CMS boundaries and asserts (a) cms_transactions rows are
// inserted by the wrapper and (b) EventCMSTransaction is published.
func TestRecordMovementTransactions_PersistsAndEmits(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, _, bp := setupTestData(t, db)
	eng := newTestEngine(t, db, simulator.New())

	srcRoot, srcSlot := makeCMSBoundary(t, db, "SRC")
	dstRoot, dstSlot := makeCMSBoundary(t, db, "DST")

	bin := createTestBinAtNode(t, db, bp.Code, srcSlot.ID, "BIN-MV-1")
	putManifest(t, db, bin.ID, bp.Code, "PART-A", 5, 1)

	// Subscribe BEFORE invoking so we capture the emitted event.
	captured := make(chan Event, 4)
	eng.Events.SubscribeTypes(func(evt Event) { captured <- evt }, EventCMSTransaction)

	eng.RecordMovementTransactions(BinUpdatedEvent{
		BinID:       bin.ID,
		FromNodeID:  srcSlot.ID,
		ToNodeID:    dstSlot.ID,
		PayloadCode: bp.Code,
		Action:      "moved",
	})

	// Two rows expected: one decrement at src boundary, one increment at dst.
	srcRows, err := db.ListCMSTransactions(srcRoot.ID, 10, 0)
	if err != nil {
		t.Fatalf("list src txns: %v", err)
	}
	if len(srcRows) != 1 {
		t.Errorf("src txns = %d, want 1: %+v", len(srcRows), srcRows)
	} else if srcRows[0].Delta != -5 {
		t.Errorf("src txn = %+v, want delta=-5", srcRows[0])
	}

	dstRows, err := db.ListCMSTransactions(dstRoot.ID, 10, 0)
	if err != nil {
		t.Fatalf("list dst txns: %v", err)
	}
	if len(dstRows) != 1 {
		t.Errorf("dst txns = %d, want 1: %+v", len(dstRows), dstRows)
	} else if dstRows[0].Delta != 5 {
		t.Errorf("dst txn = %+v, want delta=5", dstRows[0])
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

// TestRecordMovementTransactions_SkipsReplay is the unit-level half of
// the recovery-replay guard: the skip lives on the recorder, so it holds
// for any future emitter that tags an event as a replay, not just for
// RecoveryService. The end-to-end pin is
// TestReapplyOrderCompletion_SkipsCMSBuildForReplay.
func TestRecordMovementTransactions_SkipsReplay(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, _, bp := setupTestData(t, db)
	eng := newTestEngine(t, db, simulator.New())

	srcRoot, srcSlot := makeCMSBoundary(t, db, "RSK-SRC")
	dstRoot, dstSlot := makeCMSBoundary(t, db, "RSK-DST")

	bin := createTestBinAtNode(t, db, bp.Code, srcSlot.ID, "BIN-MV-REPLAY")
	putManifest(t, db, bin.ID, bp.Code, "PART-A", 5, 1)

	ev := BinUpdatedEvent{
		Action:      "moved",
		BinID:       bin.ID,
		PayloadCode: bp.Code,
		FromNodeID:  srcSlot.ID,
		ToNodeID:    dstSlot.ID,
	}

	emitted := false
	eng.Events.SubscribeTypes(func(Event) { emitted = true }, EventCMSTransaction)

	ev.Replay = true
	eng.RecordMovementTransactions(ev)

	srcRows, _ := db.ListCMSTransactions(srcRoot.ID, 10, 0)
	dstRows, _ := db.ListCMSTransactions(dstRoot.ID, 10, 0)
	if len(srcRows) != 0 || len(dstRows) != 0 {
		t.Errorf("replay produced rows: src=%d dst=%d, want 0/0", len(srcRows), len(dstRows))
	}
	if emitted {
		t.Error("replay emitted EventCMSTransaction, want none")
	}

	// Same event without the tag: the rows this test says are absent must
	// be rows this setup can actually produce.
	ev.Replay = false
	eng.RecordMovementTransactions(ev)

	srcRows, _ = db.ListCMSTransactions(srcRoot.ID, 10, 0)
	dstRows, _ = db.ListCMSTransactions(dstRoot.ID, 10, 0)
	if len(srcRows) != 1 || len(dstRows) != 1 {
		t.Fatalf("untagged move produced src=%d dst=%d, want 1/1 — the skip assertion above is vacuous",
			len(srcRows), len(dstRows))
	}
}

// TestRecordMovementTransactions_NoOpWhenSameBoundary confirms the
// wrapper short-circuits cleanly: no rows persisted, no event emitted.
func TestRecordMovementTransactions_NoOpWhenSameBoundary(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, _, bp := setupTestData(t, db)
	eng := newTestEngine(t, db, simulator.New())

	root, slotA := makeCMSBoundary(t, db, "ONE")
	// A second slot under the SAME boundary root.
	slotB := &nodes.Node{Name: "ONE-SLOT2", Enabled: true, ParentID: &root.ID}
	testutil.MustNoErr(t, db.CreateNode(slotB), "create slotB")

	bin := createTestBinAtNode(t, db, bp.Code, slotA.ID, "BIN-MV-2")
	putManifest(t, db, bin.ID, bp.Code, "PART-A", 3, 1)

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

// ── the mover: robot_id and order_id ────────────────────────────────────

// TestOrdinaryDelivery_StampsRobotAndOrder is the round-1 finding as an
// end-to-end regression pin, and it has to be end-to-end: the defect was not in
// the builder but in WHERE the builder got its answer. It read bin.ClaimedBy,
// and ApplyArrival releases that claim before the event fires — so order_id was
// NULL on every ordinary AMR delivery, a write-only column that looked
// populated in every unit test that set the claim by hand.
//
// This drives a real handleOrderDelivered so the release happens for real.
func TestOrdinaryDelivery_StampsRobotAndOrder(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, _, bp := setupTestData(t, db)
	eng := newTestEngine(t, db, simulator.New())

	srcRoot, srcSlot := makeCMSBoundary(t, db, "MOVER-SRC")
	dstRoot, dstSlot := makeCMSBoundary(t, db, "MOVER-DST")

	bin := createTestBinAtNode(t, db, bp.Code, srcSlot.ID, "BIN-MOVER")
	putManifest(t, db, bin.ID, bp.Code, "PART-A", 6, 1)

	order := &orders.Order{
		EdgeUUID:     "mover-1",
		StationID:    "line-1",
		OrderType:    "retrieve",
		Status:       "delivered",
		SourceNode:   srcRoot.Name + "." + srcSlot.Name,
		DeliveryNode: dstRoot.Name + "." + dstSlot.Name,
		BinID:        &bin.ID,
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")
	// robot_id is NOT an INSERT column — the fleet stamps it when it assigns a
	// robot, so setting the struct field before CreateOrder writes nothing and
	// the test would assert against an order that never had a robot.
	testutil.MustNoErr(t, db.UpdateOrderRobotID(order.ID, "AMR-007"), "assign robot")
	testdb.ClaimBinForTest(t, db, bin.ID, order.ID)
	testutil.MustNoErr(t, db.UpdateOrderStatus(order.ID, "delivered", "test: delivered"), "set delivered")
	order, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "reload order")
	if order.RobotID != "AMR-007" {
		t.Fatalf("fixture: the order carries robot_id %q, not AMR-007 — the assertion below "+
			"would be about an order with no robot", order.RobotID)
	}

	eng.handleOrderDelivered(order)

	// The claim really is gone by now — which is the whole reason the event has
	// to carry the answer. Assert it, so this test cannot pass by accident on a
	// path that left the claim in place.
	movedBin, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "reload bin")
	if movedBin.ClaimedBy != nil {
		t.Fatalf("the bin is still claimed by %d after delivery — this fixture no longer "+
			"reproduces the condition the fix is for", *movedBin.ClaimedBy)
	}

	rows, err := db.ListCMSTransactions(dstRoot.ID, 10, 0)
	testutil.MustNoErr(t, err, "list dst txns")
	if len(rows) != 1 {
		t.Fatalf("dst txns = %d, want 1: %+v", len(rows), rows)
	}
	if rows[0].RobotID != "AMR-007" {
		t.Errorf("robot_id = %q, want AMR-007", rows[0].RobotID)
	}
	if rows[0].OrderID == nil || *rows[0].OrderID != order.ID {
		t.Errorf("order_id = %v, want %d — NULL here is the round-1 defect returning",
			rows[0].OrderID, order.ID)
	}
}

// ── the posting queue's filter ──────────────────────────────────────────

// TestCMSPostingSubscriber_QueuesMovementsAndFiltersCorrections.
//
// The correction path that also emitted EventCMSTransaction is gone, so today
// this filter removes nothing — which is exactly why it needs a test. A guard
// with no live input is a guard nobody can tell is working, and the reason it
// exists is the day someone reintroduces corrections: they should have to
// decide, explicitly, whether an intra-bin manifest edit belongs on a
// storeroom-TRANSFER feed, rather than arriving on it by inheritance.
//
// The movement half is not scenery either: without it a filter that dropped
// everything would pass.
func TestCMSPostingSubscriber_QueuesMovementsAndFiltersCorrections(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, _, bp := setupTestData(t, db)

	// The block goes in BEFORE New: the poster is built there now, so a cfg
	// edited afterwards is a value nothing will read again.
	eng := newUnstartedEngineWith(t, db, simulator.New(), func(cfg *config.Config) {
		cfg.CMS = config.CMSConfig{
			BaseURL: "http://middleware.example.invalid/api", AccessKey: "AK", SecretKey: "SK",
			PollInterval: time.Hour, SettleWindow: time.Hour, MaxAttempts: 3,
		}
	})
	eng.Start()
	t.Cleanup(eng.Stop)

	if eng.cmsPoster == nil {
		t.Fatal("a configured cms: block did not produce a poster — the rest of this test is vacuous")
	}

	srcRoot, srcSlot := makeCMSBoundary(t, db, "QUEUE-SRC")
	_, dstSlot := makeCMSBoundary(t, db, "QUEUE-DST")
	bin := createTestBinAtNode(t, db, bp.Code, srcSlot.ID, "BIN-QUEUE")
	putManifest(t, db, bin.ID, bp.Code, "PART-A", 4, 1)

	countPostings := func() int {
		t.Helper()
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM cms_postings`).Scan(&n); err != nil {
			t.Fatalf("count postings: %v", err)
		}
		return n
	}

	// A correction-sourced batch must produce nothing.
	eng.Events.Emit(Event{Type: EventCMSTransaction, Payload: CMSTransactionEvent{
		Transactions: []*cms.Transaction{{
			ID: 999_001, NodeID: srcRoot.ID, CatID: "PART-A", Delta: 3,
			SourceType: "correction", Storeroom: "X",
		}},
	}})
	if n := countPostings(); n != 0 {
		t.Errorf("a correction-sourced batch created %d postings, want 0", n)
	}

	// A real movement must produce one.
	eng.RecordMovementTransactions(BinUpdatedEvent{
		Action: "moved", BinID: bin.ID, PayloadCode: bp.Code,
		FromNodeID: srcSlot.ID, ToNodeID: dstSlot.ID,
	})
	if n := countPostings(); n != 1 {
		t.Fatalf("a real movement created %d postings, want 1 — if this is 0 the filter above "+
			"proves nothing, because nothing was getting through either way", n)
	}

	// And the posting must actually carry the rows, not just exist.
	var attached int
	if err := db.QueryRow(`SELECT count(*) FROM cms_transactions WHERE posting_id IS NOT NULL`).Scan(&attached); err != nil {
		t.Fatalf("count attached: %v", err)
	}
	if attached != 2 {
		t.Errorf("attached transactions = %d, want 2 (one per boundary)", attached)
	}
}
