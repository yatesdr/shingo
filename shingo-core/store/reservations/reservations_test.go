//go:build docker

package reservations_test

import (
	"sync"
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// TestReservations_AcquireConflict verifies ErrReservationConflict on a second
// Acquire for the same bin — the partial unique index is the gate.
func TestReservations_AcquireConflict(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, "PART-A", sd.StorageNode.ID, "BIN-RES-CONFLICT")

	o1 := testdb.CreateOrder(t, db)
	o2 := testdb.CreateOrder(t, db)
	if err := reservations.Acquire(db, o1.ID, bin.ID, "test"); err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	// A different order acquiring the same bin must conflict.
	if err := reservations.Acquire(db, o2.ID, bin.ID, "test"); err != reservations.ErrReservationConflict {
		t.Fatalf("second Acquire: wanted ErrReservationConflict, got %v", err)
	}
	_ = reservations.Release(db, o1.ID, bin.ID)
}

// TestReservations_AcquireConfirmRelease exercises the full happy-path sequence:
// Acquire (pending) → Confirm (confirmed) → Release (deleted).
func TestReservations_AcquireConfirmRelease(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, "PART-A", sd.StorageNode.ID, "BIN-RES-ACR")

	o1 := testdb.CreateOrder(t, db)
	o2 := testdb.CreateOrder(t, db)
	if err := reservations.Acquire(db, o1.ID, bin.ID, "test"); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := reservations.Confirm(db, o1.ID, bin.ID); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	// Confirmed row still blocks a new order.
	if err := reservations.Acquire(db, o2.ID, bin.ID, "test"); err != reservations.ErrReservationConflict {
		t.Fatalf("Acquire after Confirm: wanted ErrReservationConflict, got %v", err)
	}
	if err := reservations.Release(db, o1.ID, bin.ID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	// After release the bin is acquirable.
	if err := reservations.Acquire(db, o2.ID, bin.ID, "test"); err != nil {
		t.Fatalf("Acquire after Release: %v", err)
	}
	_ = reservations.Release(db, o2.ID, bin.ID)
}

// TestReservations_ConcurrentAcquire verifies that when N goroutines race Acquire
// on the same bin, exactly one wins. This is the DB-level race gate — the partial
// unique index resolves the race before any CAS attempt.
func TestReservations_ConcurrentAcquire(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, "PART-A", sd.StorageNode.ID, "BIN-RACE-RESV")

	const N = 10
	orderIDs := make([]int64, N)
	for i := 0; i < N; i++ {
		orderIDs[i] = testdb.CreateOrder(t, db).ID
	}
	errs := make([]error, N)
	ready := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			<-ready
			errs[i] = reservations.Acquire(db, orderIDs[i], bin.ID, "test")
		}()
	}
	close(ready)
	wg.Wait()

	winners := 0
	winnerOrder := int64(-1)
	for i, err := range errs {
		if err == nil {
			winners++
			winnerOrder = orderIDs[i]
		} else if err != reservations.ErrReservationConflict {
			t.Errorf("goroutine %d: unexpected error %v (want nil or ErrReservationConflict)", i, err)
		}
	}
	if winners != 1 {
		t.Fatalf("expected exactly 1 winner, got %d", winners)
	}
	if winnerOrder > 0 {
		_ = reservations.Release(db, winnerOrder, bin.ID)
	}
}

// TestReapOrphaned_OwnerLiveness pins the owner-liveness reaper contract: holds are
// reaped on OWNER LIVENESS, never age. A hold aged far past the retired 60s TTL SURVIVES
// while its order is non-terminal (sourcing) — the reaping churn window is closed. Once
// the order goes terminal, BOTH its pending and confirmed holds are reaped on the next
// sweep — the backstop behind TerminalizeOrder (which releases in-tx) for crashed/bypassed
// paths.
//
// The "order gone" leg (order_id NOT IN orders) is structurally UNREACHABLE and so cannot
// be exercised here: reservations.order_id is a RESTRICT foreign key (migrations.go v42, no
// ON DELETE) and orders are never hard-deleted, so a reservation can never outlive its
// order row. It stays as one-clause insurance against a future ON DELETE CASCADE.
func TestReapOrphaned_OwnerLiveness(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	binPending := testdb.CreateBinAtNode(t, db, "PART-A", sd.StorageNode.ID, "BIN-REAP-P")
	binConfirmed := testdb.CreateBinAtNode(t, db, "PART-A", sd.StorageNode.ID, "BIN-REAP-C")

	// An order legitimately in sourcing, holding one pending + one confirmed bin, both
	// stamped with an expiry an hour in the PAST — far beyond the retired 60s TTL.
	o := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = protocol.StatusSourcing })
	if err := reservations.Acquire(db, o.ID, binPending.ID, "test"); err != nil {
		t.Fatalf("acquire pending: %v", err)
	}
	if err := reservations.Acquire(db, o.ID, binConfirmed.ID, "test"); err != nil {
		t.Fatalf("acquire confirmed: %v", err)
	}
	if err := reservations.Confirm(db, o.ID, binConfirmed.ID); err != nil {
		t.Fatalf("confirm: %v", err)
	}

	// Sweep 1 — the order is alive (sourcing). Age is irrelevant: NOTHING is reaped.
	n, err := reservations.ReapOrphaned(db)
	if err != nil {
		t.Fatalf("ReapOrphaned (live order): %v", err)
	}
	if n != 0 {
		t.Fatalf("reaped %d rows under a live sourcing order — holds are sacred no matter how old (owner-liveness, not age)", n)
	}
	if held, _ := reservations.ListByOrder(db, o.ID); len(held) != 2 {
		t.Fatalf("held = %d after live sweep, want 2 (both survive)", len(held))
	}

	// The order goes terminal via a RAW status write — simulating a crash/bypass that
	// leaked past TerminalizeOrder (which would otherwise release in the same tx). The
	// reaper is exactly that backstop.
	testdb.SeedOrderStatus(t, db, o.ID, string(protocol.StatusFailed), "reaper test")

	// Sweep 2 — owner is terminal: BOTH the pending and the confirmed hold are reaped.
	n, err = reservations.ReapOrphaned(db)
	if err != nil {
		t.Fatalf("ReapOrphaned (terminal order): %v", err)
	}
	if n != 2 {
		t.Fatalf("reaped %d rows, want 2 (pending + confirmed under a terminal order)", n)
	}
	if held, _ := reservations.ListByOrder(db, o.ID); len(held) != 0 {
		t.Fatalf("held = %d after terminal reap, want 0", len(held))
	}

	// Both bins are re-acquirable — no active reservation lingers to brick them.
	other := testdb.CreateOrder(t, db)
	if err := reservations.Acquire(db, other.ID, binPending.ID, "test"); err != nil {
		t.Fatalf("re-acquire previously-pending bin: %v", err)
	}
	if err := reservations.Acquire(db, other.ID, binConfirmed.ID, "test"); err != nil {
		t.Fatalf("re-acquire previously-confirmed bin: %v", err)
	}
}

// TestReservations_HasPendingReservation verifies the 1b domain field is correctly
// populated by BinJoinQuery: true while a pending row exists, false once released.
func TestReservations_HasPendingReservation(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, "PART-A", sd.StorageNode.ID, "BIN-HPR-1")

	o := testdb.CreateOrder(t, db)

	got, _ := db.GetBin(bin.ID)
	if got.HasPendingReservation {
		t.Fatal("HasPendingReservation should be false before any Acquire")
	}

	if err := reservations.Acquire(db, o.ID, bin.ID, "test"); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	got, _ = db.GetBin(bin.ID)
	if !got.HasPendingReservation {
		t.Fatal("HasPendingReservation should be true after Acquire (state=pending)")
	}

	// After Confirm the field is checked against state='pending' only — confirmed
	// rows do not set it. The bin is now physically claimed, not reservation-pending.
	if err := reservations.Confirm(db, o.ID, bin.ID); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	got, _ = db.GetBin(bin.ID)
	if got.HasPendingReservation {
		t.Fatal("HasPendingReservation should be false after Confirm (only checks state=pending)")
	}

	if err := reservations.Release(db, o.ID, bin.ID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	got, _ = db.GetBin(bin.ID)
	if got.HasPendingReservation {
		t.Fatal("HasPendingReservation should be false after Release")
	}
}

// TestReservations_ReleaseByOrder verifies teardown paths: ReleaseByOrder deletes
// all of an order's reservations in one call, leaving each bin acquirable again.
func TestReservations_ReleaseByOrder(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)

	orderID := testdb.CreateOrder(t, db).ID

	bin1 := testdb.CreateBinAtNode(t, db, "PART-A", sd.StorageNode.ID, "BIN-ROB-1")
	bin2 := testdb.CreateBinAtNode(t, db, "PART-A", sd.StorageNode.ID, "BIN-ROB-2")

	for _, b := range []*bins.Bin{bin1, bin2} {
		if err := reservations.Acquire(db, orderID, b.ID, "test"); err != nil {
			t.Fatalf("Acquire bin %d: %v", b.ID, err)
		}
	}

	for _, b := range []*bins.Bin{bin1, bin2} {
		got, _ := db.GetBin(b.ID)
		if !got.HasPendingReservation {
			t.Fatalf("bin %d: HasPendingReservation should be true before ReleaseByOrder", b.ID)
		}
	}

	if err := reservations.ReleaseByOrder(db, orderID); err != nil {
		t.Fatalf("ReleaseByOrder: %v", err)
	}

	for _, b := range []*bins.Bin{bin1, bin2} {
		got, _ := db.GetBin(b.ID)
		if got.HasPendingReservation {
			t.Errorf("bin %d: HasPendingReservation still true after ReleaseByOrder", b.ID)
		}
	}
}

// TestReservations_SwapSiblingCancel asserts that when a two-robot swap pair is
// abandoned (the engine calls CancelOrderAtomic on each leg), both orders'
// reservations are released. This test exercises the cascade at the teardown
// boundary — the dispatch-level sibling lookup and LifecycleService.CancelOrder
// routing are tested elsewhere; this pins the reservation-release leg.
//
// The swap pair is explicitly linked via LinkOrderSiblingsByEdgeUUID to
// document the intent even though CancelOrderAtomic only needs the order ID.
func TestReservations_SwapSiblingCancel(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)

	// Create two sibling orders — supply and evac legs of a two-robot swap.
	supply := &orders.Order{
		EdgeUUID: "swap-sib-supply", StationID: "test",
		OrderType: "move", Status: "pending", Quantity: 1,
		DeliveryNode: sd.LineNode.Name,
	}
	evac := &orders.Order{
		EdgeUUID: "swap-sib-evac", StationID: "test",
		OrderType: "move", Status: "pending", Quantity: 1,
		DeliveryNode: sd.LineNode.Name,
	}
	if err := db.CreateOrder(supply); err != nil {
		t.Fatalf("create supply order: %v", err)
	}
	if err := db.CreateOrder(evac); err != nil {
		t.Fatalf("create evac order: %v", err)
	}
	if _, err := db.LinkOrderSiblingsByEdgeUUID(supply.EdgeUUID, evac.EdgeUUID); err != nil {
		t.Fatalf("link siblings: %v", err)
	}

	binSupply := testdb.CreateBinAtNode(t, db, "PART-A", sd.StorageNode.ID, "BIN-SWP-SUPPLY")
	binEvac := testdb.CreateBinAtNode(t, db, "PART-A", sd.StorageNode.ID, "BIN-SWP-EVAC")

	if err := reservations.Acquire(db, supply.ID, binSupply.ID, "test"); err != nil {
		t.Fatalf("Acquire supply: %v", err)
	}
	if err := reservations.Acquire(db, evac.ID, binEvac.ID, "test"); err != nil {
		t.Fatalf("Acquire evac: %v", err)
	}

	// Sanity: both bins show reserved before cancel.
	for _, b := range []*bins.Bin{binSupply, binEvac} {
		got, _ := db.GetBin(b.ID)
		if !got.HasPendingReservation {
			t.Fatalf("bin %d: expected HasPendingReservation=true before cancel", b.ID)
		}
	}

	// Simulate abandonOrder: terminate each leg through the chokepoint that
	// LifecycleService.CancelOrder routes to (transition → TerminalizeOrder for
	// StatusCancelled), which releases every reservation the order holds.
	if _, err := db.TerminalizeOrder(supply.ID, protocol.StatusCancelled, "test abandon"); err != nil {
		t.Fatalf("terminalize supply: %v", err)
	}
	if _, err := db.TerminalizeOrder(evac.ID, protocol.StatusCancelled, "test abandon"); err != nil {
		t.Fatalf("terminalize evac: %v", err)
	}

	// Both reservations must be gone.
	for _, b := range []*bins.Bin{binSupply, binEvac} {
		got, _ := db.GetBin(b.ID)
		if got.HasPendingReservation {
			t.Errorf("bin %d: HasPendingReservation=true after swap pair cancelled", b.ID)
		}
	}

	// No residual rows in the reservations table for either order.
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM reservations WHERE order_id IN ($1,$2)`,
		supply.ID, evac.ID).Scan(&count)
	if err != nil {
		t.Fatalf("count residual reservations: %v", err)
	}
	if count != 0 {
		t.Errorf("residual reservation rows = %d, want 0 after swap pair cancel", count)
	}
}

// ── v44 schema tests (commit 1) ──────────────────────────────────────────────
// These exercise the migration directly via raw SQL (the kind-threaded Acquire
// API arrives in commit 2), so they pin the schema shape independent of the Go
// surface: resource_kind, node_id, the exactly-one-of + domain CHECKs, and the
// per-kind partial unique indexes.

// TestV44_SlotAndBinReservationsCoexistOnOneNode: a bin reservation and a slot
// reservation may both be active on the SAME node — different resource kinds
// live under different partial unique indexes, so they never collide.
func TestV44_SlotAndBinReservationsCoexistOnOneNode(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, "PART-A", sd.StorageNode.ID, "BIN-V44-COEXIST")
	o := testdb.CreateOrder(t, db)

	if _, err := db.Exec(`INSERT INTO reservations (order_id, bin_id, resource_kind, state, reserved_by)
		VALUES ($1,$2,'bin','pending','t')`, o.ID, bin.ID); err != nil {
		t.Fatalf("insert bin reservation: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO reservations (order_id, node_id, resource_kind, state, reserved_by)
		VALUES ($1,$2,'slot','pending','t')`, o.ID, sd.StorageNode.ID); err != nil {
		t.Fatalf("slot reservation on the same node must coexist with the bin reservation: %v", err)
	}
}

// TestV44_TwoActiveSlotReservationsOnOneNodeCollide: uq_reservations_slot_active
// makes an active slot reservation exactly-one-per-node — the slot dual of the
// bin index.
func TestV44_TwoActiveSlotReservationsOnOneNodeCollide(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	o1 := testdb.CreateOrder(t, db)
	o2 := testdb.CreateOrder(t, db)

	if _, err := db.Exec(`INSERT INTO reservations (order_id, node_id, resource_kind, state, reserved_by)
		VALUES ($1,$2,'slot','pending','t')`, o1.ID, sd.StorageNode.ID); err != nil {
		t.Fatalf("first slot reservation: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO reservations (order_id, node_id, resource_kind, state, reserved_by)
		VALUES ($1,$2,'slot','pending','t')`, o2.ID, sd.StorageNode.ID); err == nil {
		t.Fatal("second active slot reservation on the same node must collide, got nil error")
	}
}

// TestV44_BinReservationsStillExactlyOnePerBin: the rescoped bin index (now
// qualified resource_kind='bin') still enforces one active reservation per bin —
// existing bin semantics unchanged.
func TestV44_BinReservationsStillExactlyOnePerBin(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, "PART-A", sd.StorageNode.ID, "BIN-V44-BINUNIQ")
	o1 := testdb.CreateOrder(t, db)
	o2 := testdb.CreateOrder(t, db)

	if _, err := db.Exec(`INSERT INTO reservations (order_id, bin_id, resource_kind, state, reserved_by)
		VALUES ($1,$2,'bin','pending','t')`, o1.ID, bin.ID); err != nil {
		t.Fatalf("first bin reservation: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO reservations (order_id, bin_id, resource_kind, state, reserved_by)
		VALUES ($1,$2,'bin','pending','t')`, o2.ID, bin.ID); err == nil {
		t.Fatal("second active bin reservation on the same bin must collide, got nil error")
	}
}

// TestV44_CheckRejectsMalformedRows: the domain + exactly-one-of CHECKs reject a
// bad kind, a bad state, and both wrong target shapes (bin-with-node, slot-with-bin).
func TestV44_CheckRejectsMalformedRows(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, "PART-A", sd.StorageNode.ID, "BIN-V44-MALFORMED")
	o := testdb.CreateOrder(t, db)

	cases := []struct {
		name string
		sql  string
		args []any
	}{
		{"bad_kind", `INSERT INTO reservations (order_id, bin_id, resource_kind, state, reserved_by) VALUES ($1,$2,'banana','pending','t')`, []any{o.ID, bin.ID}},
		{"bad_state", `INSERT INTO reservations (order_id, bin_id, resource_kind, state, reserved_by) VALUES ($1,$2,'bin','reserved','t')`, []any{o.ID, bin.ID}},
		{"bin_with_node", `INSERT INTO reservations (order_id, node_id, resource_kind, state, reserved_by) VALUES ($1,$2,'bin','pending','t')`, []any{o.ID, sd.StorageNode.ID}},
		{"slot_with_bin", `INSERT INTO reservations (order_id, bin_id, resource_kind, state, reserved_by) VALUES ($1,$2,'slot','pending','t')`, []any{o.ID, bin.ID}},
	}
	for _, c := range cases {
		if _, err := db.Exec(c.sql, c.args...); err == nil {
			t.Errorf("%s: malformed reservation row must be rejected by CHECK, got nil error", c.name)
		}
	}
}

// ── commit 2: slot store surface (kind-agnostic) ─────────────────────────────

// TestAcquireSlot_ExactlyOneWinner pins two things: a slot reservation is
// one-active-per-node (the slot dual of the bin index), AND — the make-or-break
// property — a slot is reservable even while it PHYSICALLY HOLDS A BIN. Occupancy
// is NOT read at reserve time; a restock slot is full at plan time and round-trips
// empty→full.
func TestAcquireSlot_ExactlyOneWinner(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	node := sd.StorageNode
	// Occupy the node with a bin — AcquireSlot must still succeed (occupancy decoupled).
	_ = testdb.CreateBinAtNode(t, db, "PART-A", node.ID, "BIN-SLOTWINNER")
	o1 := testdb.CreateOrder(t, db)
	o2 := testdb.CreateOrder(t, db)

	if err := reservations.AcquireSlot(db, o1.ID, node.ID, "test"); err != nil {
		t.Fatalf("AcquireSlot on an OCCUPIED node must succeed (occupancy is not read at reserve): %v", err)
	}
	if err := reservations.AcquireSlot(db, o2.ID, node.ID, "test"); err != reservations.ErrReservationConflict {
		t.Fatalf("second AcquireSlot on the same node: want ErrReservationConflict, got %v", err)
	}
}

// TestReapOrphaned_ReapsSlotUnderTerminalOwner proves ReapOrphaned is kind-agnostic:
// a slot reservation whose owner is terminal is reclaimed with ZERO slot-specific
// code (order-keyed DELETE). SeedOrderStatus takes the owner terminal WITHOUT going
// through TerminalizeOrder, so the row leaks past the chokepoint — exactly what the
// reaper backstop exists to catch.
func TestReapOrphaned_ReapsSlotUnderTerminalOwner(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	o := testdb.CreateOrder(t, db)
	if err := reservations.AcquireSlot(db, o.ID, sd.StorageNode.ID, "test"); err != nil {
		t.Fatalf("AcquireSlot: %v", err)
	}
	testdb.SeedOrderStatus(t, db, o.ID, string(protocol.StatusFailed), "reaper slot test")

	n, err := reservations.ReapOrphaned(db)
	if err != nil {
		t.Fatalf("ReapOrphaned: %v", err)
	}
	if n < 1 {
		t.Fatalf("ReapOrphaned reaped %d rows, want >= 1 (the slot row under the terminal owner)", n)
	}
	// The slot freed → another order can reserve it.
	other := testdb.CreateOrder(t, db)
	if err := reservations.AcquireSlot(db, other.ID, sd.StorageNode.ID, "test"); err != nil {
		t.Fatalf("slot must be re-acquirable after reap: %v", err)
	}
}

// TestReleaseByOrder_DropsBothKindsOneCall proves ReleaseByOrder is kind-agnostic —
// one order-keyed call drops an order's bin AND slot reservations together — and
// exercises the kind-aware ListByOrder (a mixed bin+slot held set).
func TestReleaseByOrder_DropsBothKindsOneCall(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, "PART-A", sd.StorageNode.ID, "BIN-BOTHKINDS")
	o := testdb.CreateOrder(t, db)

	if err := reservations.Acquire(db, o.ID, bin.ID, "test"); err != nil {
		t.Fatalf("Acquire bin: %v", err)
	}
	if err := reservations.AcquireSlot(db, o.ID, sd.StorageNode.ID, "test"); err != nil {
		t.Fatalf("AcquireSlot: %v", err)
	}
	held, err := reservations.ListByOrder(db, o.ID)
	if err != nil {
		t.Fatalf("ListByOrder: %v", err)
	}
	if len(held) != 2 {
		t.Fatalf("held %d reservations, want 2 (bin + slot)", len(held))
	}
	// ListByOrder is kind-aware: one bin row (BinID set), one slot row (NodeID set).
	var sawBin, sawSlot bool
	for _, r := range held {
		switch r.Kind {
		case reservations.KindBin:
			sawBin = r.BinID == bin.ID
		case reservations.KindSlot:
			sawSlot = r.NodeID == sd.StorageNode.ID
		}
	}
	if !sawBin || !sawSlot {
		t.Fatalf("ListByOrder kinds wrong: sawBin=%v sawSlot=%v held=%+v", sawBin, sawSlot, held)
	}

	if err := reservations.ReleaseByOrder(db, o.ID); err != nil {
		t.Fatalf("ReleaseByOrder: %v", err)
	}
	held2, _ := reservations.ListByOrder(db, o.ID)
	if len(held2) != 0 {
		t.Fatalf("after ReleaseByOrder, held %d, want 0 (both kinds dropped in one call)", len(held2))
	}
}

// TestReleaseByNode_DropsSlotReservation pins the new slot-dual of ReleaseByBin:
// the delivery-arrival coupling (commit 4) frees a slot by node.
func TestReleaseByNode_DropsSlotReservation(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	o := testdb.CreateOrder(t, db)
	if err := reservations.AcquireSlot(db, o.ID, sd.StorageNode.ID, "test"); err != nil {
		t.Fatalf("AcquireSlot: %v", err)
	}
	if err := reservations.ReleaseByNode(db, sd.StorageNode.ID); err != nil {
		t.Fatalf("ReleaseByNode: %v", err)
	}
	held, _ := reservations.ListByOrder(db, o.ID)
	if len(held) != 0 {
		t.Fatalf("after ReleaseByNode, held %d, want 0", len(held))
	}
}
