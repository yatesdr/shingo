//go:build docker

package engine

import (
	"sync/atomic"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// reconciliation_service_test.go — coverage tests for
// reconciliation_service.go.
//
// ReconciliationService is a thin layer over *store.DB:
//
//   newReconciliationService — constructor, copies deps and zero-inits hook
//   Summary                  — delegates to DB.GetReconciliationSummary
//   ListAnomalies            — delegates to DB.ListReconciliationAnomalies
//   ListRecoveryActions      — delegates to DB.ListRecoveryActions
//   RequeueOutbox            — delegates to DB.RequeueOutbox
//   ListDeadLetterOutbox     — delegates to DB.ListDeadLetterOutbox
//   Loop                     — ticker-driven summary+auto-confirm; timing
//                              tested by the explicit AutoConfirm* method
//                              below to keep the test deterministic.
//   AutoConfirmStuckDeliveredOrders — delegates to confirmDelivered for
//                              each stuck order past a timeout, then
//                              records a recovery_actions audit row.
//                              Production wires confirmDelivered to
//                              LifecycleService.ConfirmReceipt; tests
//                              substitute a fake callback that mutates
//                              the test DB.
//
// Tests focus on seeding DB state (stuck orders, expired staged bins,
// dead-lettered outbox rows) and asserting the returned data matches.
// Auto-confirm tests bypass the ticker in Loop so the timing is explicit.

// newReconService builds a bare ReconciliationService wired to a fresh DB
// without spinning the full Engine. Mirrors the edge-side harness.
func newReconService(t *testing.T, db *store.DB) *ReconciliationService {
	t.Helper()
	return newReconciliationService(db, t.Logf)
}

// ── constructor ─────────────────────────────────────────────────────

func TestNewReconciliationService_WiresDeps(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := newReconciliationService(db, t.Logf)
	if svc == nil {
		t.Fatal("newReconciliationService returned nil")
	}
	if svc.db != db {
		t.Error("db not stored on service")
	}
	if svc.logFn == nil {
		t.Error("logFn should be wired")
	}
	if svc.confirmDelivered != nil {
		t.Error("confirmDelivered should default to nil (wired by Engine.New)")
	}
}

// ── reshuffle liveness backstop ─────────────────────────────────────

// TestAdvanceStuckReshuffleParents_ReDrivesOnlyStranded verifies the query selects
// a compound parent stranded in `reshuffling` with ALL children terminal (incl. the
// cancelled-child vector) and re-drives it, while leaving a parent with an in-flight
// child and a childless parent untouched.
func TestAdvanceStuckReshuffleParents_ReDrivesOnlyStranded(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := newReconService(t, db)

	var advanced []int64
	svc.advanceCompound = func(parentID int64) error {
		advanced = append(advanced, parentID)
		return nil
	}

	mkParent := func(uuid string) int64 {
		p := &orders.Order{EdgeUUID: uuid, StationID: "line-1", OrderType: protocol.OrderTypeRetrieve, Status: protocol.StatusReshuffling, Quantity: 1}
		testutil.MustNoErr(t, db.CreateOrder(p), "create parent "+uuid)
		return p.ID
	}
	mkChild := func(uuid string, parentID int64, status protocol.Status) {
		c := &orders.Order{EdgeUUID: uuid, StationID: "line-1", OrderType: protocol.OrderTypeMove, Status: status, ParentOrderID: &parentID, Quantity: 1}
		testutil.MustNoErr(t, db.CreateOrder(c), "create child "+uuid)
	}

	// Stranded: all children terminal, one cancelled (the vector with no event arm).
	stranded := mkParent("resh-stranded")
	mkChild("resh-stranded-c1", stranded, protocol.StatusConfirmed)
	mkChild("resh-stranded-c2", stranded, protocol.StatusCancelled)

	// In-flight child — NOT stranded, must not be selected.
	inflight := mkParent("resh-inflight")
	mkChild("resh-inflight-c1", inflight, protocol.StatusConfirmed)
	mkChild("resh-inflight-c2", inflight, protocol.StatusInTransit)

	// Childless reshuffling parent — no children to be terminal, must not be selected.
	mkParent("resh-childless")

	n, err := svc.AdvanceStuckReshuffleParents()
	testutil.MustNoErr(t, err, "AdvanceStuckReshuffleParents")
	if n != 1 || len(advanced) != 1 || advanced[0] != stranded {
		t.Fatalf("advanced = %v (n=%d), want exactly [%d] (only the all-terminal parent)", advanced, n, stranded)
	}
}

// TestAdvanceStuckReshuffleParents_SkipsOpenParent is the SECOND sealedness
// guard, and deliberately not the load-bearing one — AdvanceCompoundOrder
// refuses an open parent itself, and the poller and event paths reach that
// refusal without coming through this sweep at all.
//
// What rests on this predicate is the forensic record. Without it the sweep
// selects every open parent on every pass — it runs on the periodic ticker, not
// only at boot — re-drives it into a refusal, and writes a RecordRecoveryAction
// saying it rescued a reshuffle stranded in `reshuffling`. Nothing was
// stranded and nothing was rescued. An alarm that fires when nothing happened
// costs the same thing as one that cannot fire when something did: the next
// reader stops believing it. Somebody will eventually count these rows.
//
// DESIGN §16 rule 7: the open parent here satisfies every other clause of the
// predicate — status `reshuffling`, has children, all of them terminal — so
// sealedness is the only thing that can exclude it. A fixture with a pending
// child would be excluded by the clause that already existed and would pass
// with this guard removed.
//
// The parent is opened through the production writer; nothing opens one on its
// own until the fold lands (5c), so the fixture is ahead of the trigger.
//
// MUTATION (verified): drop `AND NOT p.open_for_children` from the predicate.
// This fires with advanced = [open, sealed] — both selected — and the message
// names the false recovery record as the cost.
func TestAdvanceStuckReshuffleParents_SkipsOpenParent(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := newReconService(t, db)

	var advanced []int64
	svc.advanceCompound = func(parentID int64) error {
		advanced = append(advanced, parentID)
		return nil
	}

	mk := func(uuid string, open bool) int64 {
		p := &orders.Order{EdgeUUID: uuid, StationID: "line-1", OrderType: protocol.OrderTypeRetrieve,
			Status: protocol.StatusReshuffling, Quantity: 1}
		testutil.MustNoErr(t, db.CreateOrder(p), "create parent "+uuid)
		c := &orders.Order{EdgeUUID: uuid + "-c1", StationID: "line-1", OrderType: protocol.OrderTypeMove,
			Status: protocol.StatusConfirmed, ParentOrderID: &p.ID, Quantity: 1}
		testutil.MustNoErr(t, db.CreateOrder(c), "create child for "+uuid)
		if open {
			testutil.MustNoErr(t, db.SetCompoundOpen(p.ID, true), "open "+uuid)
		}
		return p.ID
	}

	// Identical in every respect the predicate looks at, except sealedness.
	openParent := mk("resh-open", true)
	sealedParent := mk("resh-sealed", false)

	n, err := svc.AdvanceStuckReshuffleParents()
	testutil.MustNoErr(t, err, "AdvanceStuckReshuffleParents")

	if len(advanced) != 1 || advanced[0] != sealedParent {
		t.Fatalf("advanced = %v (n=%d), want exactly [%d]. The open parent (%d) is mid-dig, not "+
			"stranded: re-driving it logs a recovery that did not happen, every pass, forever",
			advanced, n, sealedParent, openParent)
	}
}

// TestAdvanceStuckReshuffleParents_RescuesADigThatNamesATarget is the deletion
// of a carve-out, pinned so it cannot come back by accident.
//
// A service dig used to hold its lane past its last blocker until the bin it
// uncovered was collected, which left it sealed, in `reshuffling`, with every
// child terminal — every clause of this sweep's SELECT and none of its meaning.
// So the sweep learned to skip such a parent, or it would have re-driven a
// working dig into AdvanceCompoundOrder's refusal and written an
// advance_stuck_reshuffle recovery row every pass, forever.
//
// THE HOLD IS GONE. A finished dig hands its corridor to the live demand in the
// episode it was raised for and terminates on the ordinary path, so a dig
// sitting in `reshuffling` with every child terminal is stuck in exactly the
// plain sense this sweep was written for — and re-driving it is the rescue, not
// a false record of one.
//
// THE FIXTURE IS THE OLD TEST'S, UNCHANGED, AND THE EXPECTATION IS INVERTED.
// Both parents name a target slot and differ only in whether a bin is still
// standing there, which was the whole of the deleted predicate. Both must now be
// re-driven: the sweep does not ask that question any more, and a parent whose
// target bin is still standing would otherwise be the one row it silently
// refused to rescue.
//
// MUTATION: restore the `if owes { continue }` arm. This fires with advanced =
// [collected] alone — the parent whose target bin is still standing is skipped,
// which is a dig left in `reshuffling` with nothing coming to release it.
func TestAdvanceStuckReshuffleParents_RescuesADigThatNamesATarget(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := newReconService(t, db)
	_, _, bp := testdb.SetupTestData(t, db)

	var advanced []int64
	svc.advanceCompound = func(parentID int64) error {
		advanced = append(advanced, parentID)
		return nil
	}

	// Each dig gets its own target slot. The slot exists in both cases; only the
	// bin differs, because the predicate is about the bin and not the geometry.
	mk := func(uuid string, binStillThere bool) (parentID int64, slotName string) {
		slot := &nodes.Node{Name: uuid + "-TGT", Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(slot), "create target slot for "+uuid)
		if binStillThere {
			createTestBinAtNode(t, db, bp.Code, slot.ID, uuid+"-BIN")
		}
		p := &orders.Order{EdgeUUID: uuid, StationID: "line-1", OrderType: protocol.OrderTypeMove,
			Status: protocol.StatusReshuffling, Quantity: 1}
		testutil.MustNoErr(t, db.CreateOrder(p), "create dig parent "+uuid)
		c := &orders.Order{EdgeUUID: uuid + "-c1", StationID: "line-1", OrderType: protocol.OrderTypeMove,
			Status: protocol.StatusConfirmed, ParentOrderID: &p.ID, Quantity: 1}
		testutil.MustNoErr(t, db.CreateOrder(c), "create child for "+uuid)
		return p.ID, slot.Name
	}

	standing, standingSlot := mk("dig-standing", true)
	collected, _ := mk("dig-collected", false)

	n, err := svc.AdvanceStuckReshuffleParents()
	testutil.MustNoErr(t, err, "AdvanceStuckReshuffleParents")

	if len(advanced) != 2 {
		t.Fatalf("advanced = %v (n=%d), want both %d and %d. A dig no longer holds its lane for the "+
			"bin at %s, so a dig parked in `reshuffling` with every child terminal is stranded and "+
			"this sweep is what un-strands it", advanced, n, standing, collected, standingSlot)
	}
}

// ── Summary — fresh DB ──────────────────────────────────────────────

func TestReconciliationService_Summary_FreshDB(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := newReconService(t, db)
	summary, err := svc.Summary()
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if summary == nil {
		t.Fatal("summary is nil")
	}
	if summary.Status != "ok" {
		t.Errorf("fresh DB status = %q, want ok", summary.Status)
	}
	if summary.TotalAnomalies != 0 {
		t.Errorf("TotalAnomalies = %d, want 0", summary.TotalAnomalies)
	}
	if summary.DeadLetters != 0 {
		t.Errorf("DeadLetters = %d, want 0", summary.DeadLetters)
	}
	if summary.OutboxPending != 0 {
		t.Errorf("OutboxPending = %d, want 0", summary.OutboxPending)
	}
}

// ── Summary — degraded by stuck order ───────────────────────────────

func TestReconciliationService_Summary_StuckOrderDegrades(t *testing.T) {
	t.Parallel()
	testdb.DisableWedgeSweep(t, "this fixture BACKDATES a `dispatched` order with no vendor id on purpose — that state is what the sweep under test is for, so the crash-sliver clause is correctly reporting the thing being arranged")
	db := testDB(t)
	testdb.SetupTestData(t, db)
	svc := newReconService(t, db)

	// Seed a dispatched order and backdate it past the stuck-age threshold (30
	// minutes). Must be older than stuckOrderAge in store/reconciliation.go.
	//
	// BOTH CLOCKS, and the second one is the one that counts: the detector reads
	// the last STATUS TRANSITION (an order_history row), falling back to
	// created_at for an order that has never transitioned, which this is. An
	// updated_at-only fixture describes an order that has just progressed.
	order := &orders.Order{
		EdgeUUID:     "stuck-uuid",
		StationID:    "line-1",
		OrderType:    "retrieve",
		Status:       "dispatched",
		SourceNode:   "STORAGE-A1",
		DeliveryNode: "LINE1-IN",
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")
	if _, err := db.Exec(
		`UPDATE orders SET updated_at = NOW() - INTERVAL '2 hours',
		                   created_at = NOW() - INTERVAL '2 hours' WHERE id = $1`, order.ID); err != nil {
		t.Fatalf("backdate order: %v", err)
	}
	// The birth row too. orders.Create writes one from the INSERT, so the
	// COALESCE fallback to created_at no longer reaches — an un-backdated birth
	// row makes the order's newest transition its own creation, a second ago.
	if _, err := db.Exec(
		`UPDATE order_history SET created_at = NOW() - INTERVAL '2 hours' WHERE order_id = $1`,
		order.ID); err != nil {
		t.Fatalf("backdate birth row: %v", err)
	}

	summary, err := svc.Summary()
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if summary.StuckOrders < 1 {
		t.Errorf("StuckOrders = %d, want >= 1", summary.StuckOrders)
	}
	if summary.Status != "degraded" && summary.Status != "critical" {
		t.Errorf("status = %q, want degraded or critical", summary.Status)
	}
}

// ── Summary — critical by dead letter ───────────────────────────────

func TestReconciliationService_Summary_DeadLetterCritical(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := newReconService(t, db)

	// Enqueue one outbox row and push it past MaxOutboxRetries.
	testutil.MustNoErr(t, db.EnqueueOutbox("t1", []byte(`{"msg":1}`), "test.event", "line-1"), "enqueue")
	// Fetch its ID — EnqueueOutbox doesn't return one.
	pending, err := db.ListPendingOutbox(10)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("want 1 pending row, got %d", len(pending))
	}
	id := pending[0].ID
	for i := 0; i < store.MaxOutboxRetries; i++ {
		testutil.MustNoErr(t, db.IncrementOutboxRetries(id), "increment")
	}

	summary, err := svc.Summary()
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if summary.DeadLetters != 1 {
		t.Errorf("DeadLetters = %d, want 1", summary.DeadLetters)
	}
	if summary.Status != "critical" {
		t.Errorf("status = %q, want critical", summary.Status)
	}
}

// ── ListAnomalies ───────────────────────────────────────────────────

func TestReconciliationService_ListAnomalies_Empty(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := newReconService(t, db)
	anomalies, err := svc.ListAnomalies()
	if err != nil {
		t.Fatalf("ListAnomalies: %v", err)
	}
	if len(anomalies) != 0 {
		t.Errorf("fresh DB anomalies = %d, want 0", len(anomalies))
	}
}

func TestReconciliationService_ListAnomalies_StuckOrder(t *testing.T) {
	t.Parallel()
	testdb.DisableWedgeSweep(t, "this fixture BACKDATES a `dispatched` order with no vendor id on purpose — that state is what the sweep under test is for, so the crash-sliver clause is correctly reporting the thing being arranged")
	db := testDB(t)
	testdb.SetupTestData(t, db)
	svc := newReconService(t, db)

	order := &orders.Order{
		EdgeUUID:  "anom-uuid",
		StationID: "line-1",
		OrderType: "retrieve",
		Status:    "dispatched",
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")
	// AGE IS AGE SINCE THE LAST TRANSITION. active_order_stuck keys on an
	// order_history row now, with orders.created_at as the fallback for one that
	// has never transitioned — which this is — so backdating updated_at alone
	// describes an order that has JUST progressed and correctly raises nothing.
	// Same translation store/reconciliation's own fixtures took; this package was
	// not run when they did, so these two went red on main unnoticed.
	if _, err := db.Exec(
		`UPDATE orders SET updated_at = NOW() - INTERVAL '2 hours',
		                   created_at = NOW() - INTERVAL '2 hours' WHERE id = $1`, order.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	// And the birth row the INSERT wrote — see the sibling fixture above.
	if _, err := db.Exec(
		`UPDATE order_history SET created_at = NOW() - INTERVAL '2 hours' WHERE order_id = $1`,
		order.ID); err != nil {
		t.Fatalf("backdate birth row: %v", err)
	}

	anomalies, err := svc.ListAnomalies()
	if err != nil {
		t.Fatalf("ListAnomalies: %v", err)
	}
	found := false
	for _, a := range anomalies {
		if a.Issue == "active_order_stuck" && a.OrderID != nil && *a.OrderID == order.ID {
			found = true
			if a.Category != "order_runtime" {
				t.Errorf("anomaly category = %q, want order_runtime", a.Category)
			}
			// IT USED TO ASSERT `cancel_stuck_order`, AND THAT IS THE CHANGE. The
			// board turned that recommendation into this row's only affordance —
			// a "Cancel Stuck Order" button — and cancelling a stuck order is
			// ruled 4/4 never the answer: it clears the row and leaves the robot
			// exactly where it was, minus the evidence.
			if a.RecommendedAction != "investigate_stuck_order" {
				t.Errorf("recommended action = %q, want investigate_stuck_order — the board must not "+
					"propose the one act this house ruled is never right", a.RecommendedAction)
			}
			if a.Detail == "" {
				t.Error("the anomaly carries no detail. The row renders an enum, an order id and a " +
					"recommendation; without a sentence saying what is wrong it is not a diagnosis")
			}
			break
		}
	}
	if !found {
		t.Errorf("stuck order not surfaced in anomalies: %+v", anomalies)
	}
}

func TestReconciliationService_ListAnomalies_ExpiredStagedBin(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, _, bp := testdb.SetupTestData(t, db)
	svc := newReconService(t, db)

	bin := createTestBinAtNode(t, db, bp.Code, storageNode.ID, "STAGED-1")
	// Stage the bin with an already-past expiration. StageBin uses the ts arg.
	past := time.Now().Add(-10 * time.Minute)
	testutil.MustNoErr(t, db.StageBin(bin.ID, &past), "stage bin")

	anomalies, err := svc.ListAnomalies()
	if err != nil {
		t.Fatalf("ListAnomalies: %v", err)
	}
	found := false
	for _, a := range anomalies {
		if a.Issue == "staged_bin_expired" && a.BinID != nil && *a.BinID == bin.ID {
			found = true
			if a.Category != "bin_staging" {
				t.Errorf("category = %q, want bin_staging", a.Category)
			}
			break
		}
	}
	if !found {
		t.Errorf("expired staged bin not surfaced in anomalies: %+v", anomalies)
	}
}

// ── RequeueOutbox + ListDeadLetterOutbox ────────────────────────────

func TestReconciliationService_ListDeadLetterAndRequeue(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := newReconService(t, db)

	testutil.MustNoErr(t, db.EnqueueOutbox("t1", []byte(`{"msg":"dl"}`), "test", "line-1"), "enqueue")
	pending, err := db.ListPendingOutbox(10)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("want 1 pending row, got %d", len(pending))
	}
	id := pending[0].ID
	for i := 0; i < store.MaxOutboxRetries; i++ {
		testutil.MustNoErr(t, db.IncrementOutboxRetries(id), "increment")
	}

	dead, err := svc.ListDeadLetterOutbox(10)
	if err != nil {
		t.Fatalf("ListDeadLetterOutbox: %v", err)
	}
	if len(dead) != 1 || dead[0].ID != id {
		t.Fatalf("dead-letter = %+v, want [%d]", dead, id)
	}

	// RequeueOutbox zeros the retries — the row moves off the dead-letter list.
	testutil.MustNoErr(t, svc.RequeueOutbox(id), "RequeueOutbox")
	dead2, err := svc.ListDeadLetterOutbox(10)
	if err != nil {
		t.Fatalf("ListDeadLetterOutbox after requeue: %v", err)
	}
	if len(dead2) != 0 {
		t.Errorf("after requeue, dead-letter list should be empty, got %d", len(dead2))
	}
	// And it should be pending again.
	pending2, err := db.ListPendingOutbox(10)
	if err != nil {
		t.Fatalf("list pending after requeue: %v", err)
	}
	if len(pending2) != 1 || pending2[0].ID != id {
		t.Errorf("after requeue, pending list = %+v, want [%d]", pending2, id)
	}
}

// ── ListRecoveryActions ─────────────────────────────────────────────

func TestReconciliationService_ListRecoveryActions(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := newReconService(t, db)

	// Empty → empty list.
	acts, err := svc.ListRecoveryActions(10)
	if err != nil {
		t.Fatalf("ListRecoveryActions: %v", err)
	}
	if len(acts) != 0 {
		t.Errorf("fresh DB should have no actions, got %d", len(acts))
	}

	// Seed two rows directly via the store — the service should surface both,
	// newest-first (DESC by id per the store query).
	testutil.MustNoErr(t, db.RecordRecoveryAction("release_claim", "order", 1, "first", "sys"), "record 1")
	testutil.MustNoErr(t, db.RecordRecoveryAction("auto_confirm_delivered", "order", 2, "second", "sys"), "record 2")

	acts, err = svc.ListRecoveryActions(10)
	if err != nil {
		t.Fatalf("ListRecoveryActions: %v", err)
	}
	if len(acts) != 2 {
		t.Fatalf("got %d actions, want 2", len(acts))
	}
	// Newest first: action 2 precedes action 1.
	if acts[0].TargetID != 2 || acts[1].TargetID != 1 {
		t.Errorf("order = [%d, %d], want [2, 1]", acts[0].TargetID, acts[1].TargetID)
	}

	// Limit parameter is forwarded to DB.
	limited, err := svc.ListRecoveryActions(1)
	if err != nil {
		t.Fatalf("ListRecoveryActions(limit=1): %v", err)
	}
	if len(limited) != 1 {
		t.Errorf("limit=1 returned %d rows", len(limited))
	}
}

// ── AutoConfirmStuckDeliveredOrders ─────────────────────────────────

func TestReconciliationService_AutoConfirm_NoTimeout(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := newReconService(t, db)
	// timeout <= 0 is a no-op and must never touch the DB.
	n, err := svc.AutoConfirmStuckDeliveredOrders(0)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if n != 0 {
		t.Errorf("n = %d, want 0", n)
	}
	n, err = svc.AutoConfirmStuckDeliveredOrders(-1 * time.Second)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if n != 0 {
		t.Errorf("n = %d, want 0", n)
	}
}

func TestReconciliationService_AutoConfirm_NothingDelivered(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := newReconService(t, db)
	// Fresh DB — no delivered rows, must return 0.
	n, err := svc.AutoConfirmStuckDeliveredOrders(1 * time.Second)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if n != 0 {
		t.Errorf("n = %d, want 0 on empty DB", n)
	}
}

func TestReconciliationService_AutoConfirm_ConfirmsStuckDelivered(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	testdb.SetupTestData(t, db)
	svc := newReconService(t, db)

	// Seed a delivered order with updated_at well past the timeout.
	order := &orders.Order{
		EdgeUUID:     "auto-confirm-uuid",
		StationID:    "line-1",
		OrderType:    "retrieve",
		Status:       "delivered",
		SourceNode:   "STORAGE-A1",
		DeliveryNode: "LINE1-IN",
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")
	if _, err := db.Exec(`UPDATE orders SET updated_at = NOW() - INTERVAL '2 hours' WHERE id = $1`, order.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	// Substitute a fake confirmDelivered that mimics the side effects
	// LifecycleService.ConfirmReceipt performs (status flip + CompleteOrder).
	// Production wires the real ConfirmReceipt; this unit test stays
	// dispatcher-free and asserts the callback contract instead.
	var hookCount atomic.Int64
	var hookOrderID atomic.Int64
	svc.confirmDelivered = func(o *orders.Order) error {
		hookCount.Add(1)
		hookOrderID.Store(o.ID)
		testdb.SeedOrderStatus(t, db, o.ID, "confirmed", "fake confirm")
		return db.CompleteOrder(o.ID)
	}

	n, err := svc.AutoConfirmStuckDeliveredOrders(30 * time.Minute)
	if err != nil {
		t.Fatalf("AutoConfirm: %v", err)
	}
	if n != 1 {
		t.Errorf("confirmed count = %d, want 1", n)
	}

	// Verify the status transition and completed_at — the fake callback
	// is responsible for these writes, so this also confirms the
	// callback was actually invoked end-to-end.
	got, err := db.GetOrder(order.ID)
	if err != nil {
		t.Fatalf("reload order: %v", err)
	}
	if got.Status != "confirmed" {
		t.Errorf("order status = %q, want confirmed", got.Status)
	}
	if got.CompletedAt == nil {
		t.Error("CompletedAt should be set after auto-confirm")
	}

	// Callback fired once with the right order ID.
	if hookCount.Load() != 1 {
		t.Errorf("confirmDelivered invoked %d times, want 1", hookCount.Load())
	}
	if hookOrderID.Load() != order.ID {
		t.Errorf("callback order id = %d, want %d", hookOrderID.Load(), order.ID)
	}

	// A recovery_actions row is written for audit.
	acts, err := db.ListRecoveryActions(10)
	if err != nil {
		t.Fatalf("list actions: %v", err)
	}
	found := false
	for _, a := range acts {
		if a.Action == "auto_confirm_delivered" && a.TargetID == order.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no recovery_actions row for auto_confirm_delivered on order %d: %+v", order.ID, acts)
	}
}

func TestReconciliationService_AutoConfirm_SkipsFreshDelivered(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	testdb.SetupTestData(t, db)
	svc := newReconService(t, db)

	// A delivered order updated moments ago — too fresh for auto-confirm.
	order := &orders.Order{
		EdgeUUID:     "fresh-uuid",
		StationID:    "line-1",
		OrderType:    "retrieve",
		Status:       "delivered",
		SourceNode:   "STORAGE-A1",
		DeliveryNode: "LINE1-IN",
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")

	n, err := svc.AutoConfirmStuckDeliveredOrders(30 * time.Minute)
	if err != nil {
		t.Fatalf("AutoConfirm: %v", err)
	}
	if n != 0 {
		t.Errorf("confirmed count = %d, want 0 (too fresh)", n)
	}
	// Status untouched.
	got, err := db.GetOrder(order.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.Status != "delivered" {
		t.Errorf("status = %q, want delivered (unchanged)", got.Status)
	}
	if got.CompletedAt != nil {
		t.Error("CompletedAt should not be set when skipped")
	}
}

func TestReconciliationService_AutoConfirm_SkipsNonDelivered(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	testdb.SetupTestData(t, db)
	svc := newReconService(t, db)

	// A stuck but not-delivered order — query only matches status='delivered'.
	order := &orders.Order{
		EdgeUUID:  "ndo-uuid",
		StationID: "line-1",
		OrderType: "retrieve",
		Status:    "in_transit",
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")
	if _, err := db.Exec(`UPDATE orders SET updated_at = NOW() - INTERVAL '2 hours' WHERE id = $1`, order.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	n, err := svc.AutoConfirmStuckDeliveredOrders(30 * time.Minute)
	if err != nil {
		t.Fatalf("AutoConfirm: %v", err)
	}
	if n != 0 {
		t.Errorf("confirmed = %d, want 0 (non-delivered should not be confirmed)", n)
	}
}

func TestReconciliationService_AutoConfirm_NoHookIsSafe(t *testing.T) {
	t.Parallel()
	// If confirmDelivered is nil (service built outside Engine), the
	// auto-confirm path must not panic — it logs and returns 0. The
	// status transition is the callback's job; without one wired,
	// there's nothing to do. Production always wires it from
	// engine.New (see engine.go), so this branch is purely defensive
	// for bare unit fixtures and the Loop's first tick before Start
	// completes (the Loop is itself started inside Start, but the
	// invariant is still worth guarding).
	db := testDB(t)
	testdb.SetupTestData(t, db)
	svc := newReconService(t, db)
	if svc.confirmDelivered != nil {
		t.Fatal("confirmDelivered should default to nil on a bare service")
	}

	order := &orders.Order{
		EdgeUUID:     "no-hook-uuid",
		StationID:    "line-1",
		OrderType:    "retrieve",
		Status:       "delivered",
		SourceNode:   "STORAGE-A1",
		DeliveryNode: "LINE1-IN",
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")
	if _, err := db.Exec(`UPDATE orders SET updated_at = NOW() - INTERVAL '2 hours' WHERE id = $1`, order.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	n, err := svc.AutoConfirmStuckDeliveredOrders(30 * time.Minute)
	if err != nil {
		t.Fatalf("AutoConfirm: %v", err)
	}
	if n != 0 {
		t.Errorf("confirmed = %d, want 0 (no callback wired → no confirmations)", n)
	}
	// Order must remain in 'delivered' — the callback is the only thing
	// authorised to transition it. Direct DB writes here would re-introduce
	// the state-machine bypass the forbidigo rule guards against.
	got, err := db.GetOrder(order.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.Status != "delivered" {
		t.Errorf("status = %q, want delivered (callback not wired, no transition expected)", got.Status)
	}
}

// ── Loop — smoke test ───────────────────────────────────────────────

// TestReconciliationService_Loop_StopsOnSignal verifies Loop returns
// promptly when stopCh is closed. Guards against the goroutine leak
// that would happen if the select missed a shutdown.
func TestReconciliationService_Loop_StopsOnSignal(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := newReconService(t, db)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		// 1-second tick — but we close stop immediately so it shouldn't fire.
		svc.Loop(stop, 1*time.Second, 0)
		close(done)
	}()
	close(stop)
	select {
	case <-done:
		// expected — returned within 2s of stop signal.
	case <-time.After(3 * time.Second):
		t.Fatal("Loop did not return within 3s of stopCh close")
	}
}
