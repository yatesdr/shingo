//go:build docker

package dispatch

// Tests for the 1c reserve/confirm split (commit 4, D39) against real Postgres.

import (
	"encoding/json"
	"errors"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// ── helpers ──────────────────────────────────────────────────────────────────

func reservesExactly(res []reservations.Reservation, binIDs ...int64) bool {
	if len(res) != len(binIDs) {
		return false
	}
	want := map[int64]bool{}
	for _, id := range binIDs {
		want[id] = true
	}
	for _, r := range res {
		if !want[r.BinID] {
			return false
		}
	}
	return true
}

func stateOf(res []reservations.Reservation, binID int64) string {
	for _, r := range res {
		if r.BinID == binID {
			return r.State
		}
	}
	return ""
}

func mkComplexOrder(t *testing.T, db complexOrderStore, uuid, source, process, delivery, payload string, steps []resolvedStep) *orders.Order {
	t.Helper()
	j, _ := json.Marshal(steps)
	o := &orders.Order{
		EdgeUUID: uuid, StationID: "line-1", OrderType: OrderTypeComplex, Status: StatusSourcing,
		Quantity: 1, PayloadCode: payload, SourceNode: source, ProcessNode: process,
		DeliveryNode: delivery, StepsJSON: string(j),
	}
	testutil.MustNoErr(t, db.CreateOrder(o), "create order "+uuid)
	return o
}

type complexOrderStore interface {
	CreateOrder(*orders.Order) error
}

// ── THE #1 LANDMINE: reconcile keeps the order's own held bins ────────────────

// TestReserveReconcileKeepsOwnHolds pins the reconcile against the per-bin
// unique-index landmine: an order holding bin A must NOT report A missing when it
// re-reserves need {A,B}. A naive "Acquire each need; conflict ⇒ missing" loop
// self-conflicts on A's own row (breaking retry by construction); the owner-aware
// reconcile reuses A and acquires only B.
func TestReserveReconcileKeepsOwnHolds(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	nodeA := &nodes.Node{Name: "SRC-A", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(nodeA), "node A")
	nodeB := &nodes.Node{Name: "SRC-B", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(nodeB), "node B")
	binA := testdb.CreateBinAtNode(t, db, bp.Code, nodeA.ID, "BIN-A")
	binB := testdb.CreateBinAtNode(t, db, bp.Code, nodeB.ID, "BIN-B")

	stepsAB := []resolvedStep{
		{Action: protocol.ActionPickup, Node: nodeA.Name},
		{Action: protocol.ActionDropoff, Node: lineNode.Name},
		{Action: protocol.ActionPickup, Node: nodeB.Name},
		{Action: protocol.ActionDropoff, Node: lineNode.Name},
	}
	order := mkComplexOrder(t, db, "reconcile-1", nodeA.Name, nodeA.Name, lineNode.Name, bp.Code, stepsAB)

	// Prior tick: the order already holds a PENDING reservation on A.
	testdb.ReserveBin(t, db, order.ID, binA.ID)

	planAB := BuildComplexPlan(stepsAB, d.snapshotPickupBins(stepsAB), bp.Code, nodeA.Name)
	_, outcome, err := d.reserveComplexPlan(order, planAB)
	testutil.MustNoErr(t, err, "reserve AB")
	if outcome != reserveComplete {
		t.Fatalf("outcome = %v, want reserveComplete (A kept, B acquired)", outcome)
	}
	res, err := db.ListReservationsByOrder(order.ID)
	testutil.MustNoErr(t, err, "list reservations")
	if !reservesExactly(res, binA.ID, binB.ID) {
		t.Fatalf("held = %+v, want exactly {A=%d, B=%d} (no sibling, no dropped A)", res, binA.ID, binB.ID)
	}
	if st := stateOf(res, binA.ID); st != "pending" {
		t.Errorf("bin A state = %q, want pending (reused hold untouched, no re-Acquire)", st)
	}

	// Re-resolve: the only need is now node C. A and B become strays → released; C acquired.
	nodeC := &nodes.Node{Name: "SRC-C", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(nodeC), "node C")
	binC := testdb.CreateBinAtNode(t, db, bp.Code, nodeC.ID, "BIN-C")
	stepsC := []resolvedStep{
		{Action: protocol.ActionPickup, Node: nodeC.Name},
		{Action: protocol.ActionDropoff, Node: lineNode.Name},
	}
	planC := BuildComplexPlan(stepsC, d.snapshotPickupBins(stepsC), bp.Code, nodeC.Name)
	_, outcome2, err := d.reserveComplexPlan(order, planC)
	testutil.MustNoErr(t, err, "reserve C")
	if outcome2 != reserveComplete {
		t.Fatalf("outcome2 = %v, want reserveComplete", outcome2)
	}
	res2, _ := db.ListReservationsByOrder(order.ID)
	if !reservesExactly(res2, binC.ID) {
		t.Fatalf("held after re-resolve = %+v, want exactly {C=%d} (A, B released)", res2, binC.ID)
	}
}

// TestPartialHoldRetriesToComplete pins the D5 GO gate: a complex order needing
// {A,B} with only A present holds A and reports reserveHolding (NOT complete, NOT
// dispatched); once B appears the next reserve completes and confirm claims both.
func TestPartialHoldRetriesToComplete(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	nodeA := &nodes.Node{Name: "PH-A", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(nodeA), "node A")
	nodeB := &nodes.Node{Name: "PH-B", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(nodeB), "node B")
	binA := testdb.CreateBinAtNode(t, db, bp.Code, nodeA.ID, "PH-BIN-A")

	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: nodeA.Name},
		{Action: protocol.ActionDropoff, Node: lineNode.Name},
		{Action: protocol.ActionPickup, Node: nodeB.Name},
		{Action: protocol.ActionDropoff, Node: lineNode.Name},
	}
	order := mkComplexOrder(t, db, "partial-1", nodeA.Name, nodeA.Name, lineNode.Name, bp.Code, steps)
	plan := BuildComplexPlan(steps, d.snapshotPickupBins(steps), bp.Code, nodeA.Name)

	// Tick 1: only A available (B's node empty, and A is reserved so it's not moot).
	_, outcome1, err := d.reserveComplexPlan(order, plan)
	testutil.MustNoErr(t, err, "reserve tick 1")
	if outcome1 != reserveHolding {
		t.Fatalf("tick 1 outcome = %v, want reserveHolding", outcome1)
	}
	res1, _ := db.ListReservationsByOrder(order.ID)
	if !reservesExactly(res1, binA.ID) {
		t.Fatalf("tick 1 held = %+v, want just A=%d", res1, binA.ID)
	}

	// B appears.
	binB := testdb.CreateBinAtNode(t, db, bp.Code, nodeB.ID, "PH-BIN-B")

	// Tick 2: both available → complete → confirm claims both.
	assigned2, outcome2, err := d.reserveComplexPlan(order, plan)
	testutil.MustNoErr(t, err, "reserve tick 2")
	if outcome2 != reserveComplete {
		t.Fatalf("tick 2 outcome = %v, want reserveComplete", outcome2)
	}
	if cerr := d.confirmComplexPlan(order, plan, assigned2); cerr != nil {
		t.Fatalf("confirm: %v", cerr)
	}
	claimed, _ := db.ListBinsByClaim(order.ID)
	if len(claimed) != 2 {
		t.Fatalf("claimed %d bins, want 2 (A and B)", len(claimed))
	}
	gotA, _ := db.GetBin(binA.ID)
	gotB, _ := db.GetBin(binB.ID)
	if gotA.ClaimedBy == nil || *gotA.ClaimedBy != order.ID || gotB.ClaimedBy == nil || *gotB.ClaimedBy != order.ID {
		t.Errorf("both bins must be claimed by order %d: A=%v B=%v", order.ID, gotA.ClaimedBy, gotB.ClaimedBy)
	}
}

// TestConfirmZeroRowsSurfacesClaimFailed pins that confirm never claims without a
// reservation: if the pending hold vanished (TTL-reaped) between reserve and
// confirm, confirm returns claim_failed and the bin stays unclaimed. It also pins
// rider (a): an already-confirmed reservation claimed by THIS order is skipped
// (idempotent), not re-claimed into a false failure.
func TestConfirmZeroRowsSurfacesClaimFailed(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	nodeA := &nodes.Node{Name: "CZ-A", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(nodeA), "node A")
	binA := testdb.CreateBinAtNode(t, db, bp.Code, nodeA.ID, "CZ-BIN-A")
	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: nodeA.Name},
		{Action: protocol.ActionDropoff, Node: lineNode.Name},
	}
	order := mkComplexOrder(t, db, "confirm-zero-1", nodeA.Name, nodeA.Name, lineNode.Name, bp.Code, steps)
	plan := BuildComplexPlan(steps, d.snapshotPickupBins(steps), bp.Code, nodeA.Name)

	assigned, outcome, err := d.reserveComplexPlan(order, plan)
	testutil.MustNoErr(t, err, "reserve")
	if outcome != reserveComplete {
		t.Fatalf("outcome = %v, want reserveComplete", outcome)
	}

	// Simulate the pending reservation vanishing (TTL reap) between reserve and confirm.
	testutil.MustNoErr(t, db.ReleaseReservation(order.ID, binA.ID), "reap pending")

	cerr := d.confirmComplexPlan(order, plan, assigned)
	var pe *planningError
	if !errors.As(cerr, &pe) || pe.Code != codeClaimFailed {
		t.Fatalf("confirm after reap: got %v, want a codeClaimFailed planningError", cerr)
	}
	gotA, _ := db.GetBin(binA.ID)
	if gotA.ClaimedBy != nil {
		t.Errorf("bin A claimed_by = %v, want nil — no claim without a reservation (seatbelt)", *gotA.ClaimedBy)
	}

	// Rider (a): re-reserve + confirm, then confirm AGAIN — the already-confirmed,
	// claimed-by-us bin is skipped (idempotent), not a false claim_failed.
	assigned2, _, err := d.reserveComplexPlan(order, plan)
	testutil.MustNoErr(t, err, "re-reserve")
	testutil.MustNoErr(t, d.confirmComplexPlan(order, plan, assigned2), "first confirm")
	// Re-derive the assignment (now the reservation is confirmed) and confirm again.
	assigned3, _, err := d.reserveComplexPlan(order, plan)
	testutil.MustNoErr(t, err, "reserve for idempotent confirm")
	if cerr := d.confirmComplexPlan(order, plan, assigned3); cerr != nil {
		t.Fatalf("idempotent confirm of an already-claimed-by-us bin must NOT fail: %v", cerr)
	}
}

// TestReserveMootWhenAllSourcesEmpty pins that an order that can reserve NOTHING
// because every source node is empty is reserveMoot (→ the caller skips it and the
// changeover advances), not reserveHolding (which would hold forever).
func TestReserveMootWhenAllSourcesEmpty(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	empty := &nodes.Node{Name: "MOOT-SRC", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(empty), "empty node")
	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: empty.Name},
		{Action: protocol.ActionDropoff, Node: lineNode.Name},
	}
	order := mkComplexOrder(t, db, "moot-1", empty.Name, empty.Name, lineNode.Name, bp.Code, steps)
	plan := BuildComplexPlan(steps, d.snapshotPickupBins(steps), bp.Code, empty.Name)

	_, outcome, err := d.reserveComplexPlan(order, plan)
	testutil.MustNoErr(t, err, "reserve")
	if outcome != reserveMoot {
		t.Fatalf("outcome = %v, want reserveMoot (no bin at any source, work is void)", outcome)
	}
}

// TestReservePresentButTakenHoldsNotMoot pins the moot/hold boundary (Rider 2, D40):
// the moot-skip route fires ONLY when every unmet need is a concrete node verified
// genuinely EMPTY. A node that HOLDS a bin claimed by ANOTHER order is present-but-
// taken — sourceable once that order finishes — so the reserve HOLDS and retries
// (D18-Q4), never skips the changeover as moot. This is the exact contrast to
// TestReserveMootWhenAllSourcesEmpty (empty node → moot).
//
// The sibling hold case — a group/NGRP-scoped need that can't resolve (a momentarily-
// empty supermarket) — is structurally UNREACHABLE at the reserve: reResolveComplexSteps
// runs first (complex_dispatch.go:269) and returns on ResolutionCapacity
// (complex_dispatch.go:281-289) BEFORE reserveComplexPlan (complex_dispatch.go:453),
// so the reserve only ever sees concrete nodes. Pinning (a) alone covers the reserve's
// moot boundary; (b) is pinned upstream by the NGRP re-resolve path.
func TestReservePresentButTakenHoldsNotMoot(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	nodeA := &nodes.Node{Name: "PBT-SRC", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(nodeA), "node A")
	binA := testdb.CreateBinAtNode(t, db, bp.Code, nodeA.ID, "PBT-BIN")

	// Another order claims the sole bin at the source — present, but taken.
	other := &orders.Order{EdgeUUID: "pbt-other", StationID: "s", OrderType: OrderTypeRetrieve, Status: StatusSourcing, Quantity: 1}
	testutil.MustNoErr(t, db.CreateOrder(other), "create other order")
	testdb.ClaimBinForTest(t, db, binA.ID, other.ID)

	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: nodeA.Name},
		{Action: protocol.ActionDropoff, Node: lineNode.Name},
	}
	order := mkComplexOrder(t, db, "pbt-1", nodeA.Name, nodeA.Name, lineNode.Name, bp.Code, steps)
	plan := BuildComplexPlan(steps, d.snapshotPickupBins(steps), bp.Code, nodeA.Name)

	assigned, outcome, err := d.reserveComplexPlan(order, plan)
	testutil.MustNoErr(t, err, "reserve")
	if outcome != reserveHolding {
		t.Fatalf("outcome = %v, want reserveHolding — a present-but-taken bin is sourceable, must NOT moot-skip", outcome)
	}
	if len(assigned) != 0 {
		t.Fatalf("assigned = %+v, want none (the only bin is taken)", assigned)
	}
	// It reserved nothing of its own — the taken bin stays the other order's.
	res, _ := db.ListReservationsByOrder(order.ID)
	if len(res) != 0 {
		t.Fatalf("held = %+v, want none — must not reserve a bin claimed by another", res)
	}
}

// TestStagingRegrabsNotTreatedAsMissing pins D5's relay rule end-to-end at the
// reserve: a plan that re-picks a bin at staging (a later pickup at a node it
// dropped to earlier) reserves only the DISTINCT source; the empty staging node is
// a re-grab, not a missing need, so the order completes with just its real bin.
func TestStagingRegrabsNotTreatedAsMissing(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	src := &nodes.Node{Name: "RG-SRC", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(src), "src")
	staging := &nodes.Node{Name: "RG-STAGE", Enabled: true} // empty at reserve — the relay target
	testutil.MustNoErr(t, db.CreateNode(staging), "staging")
	binSrc := testdb.CreateBinAtNode(t, db, bp.Code, src.ID, "RG-BIN")

	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: src.Name},      // true source
		{Action: protocol.ActionDropoff, Node: staging.Name}, // park at staging
		{Action: protocol.ActionPickup, Node: staging.Name},  // RE-GRAB (relay, not a need)
		{Action: protocol.ActionDropoff, Node: lineNode.Name},
	}
	order := mkComplexOrder(t, db, "regrab-1", src.Name, src.Name, lineNode.Name, bp.Code, steps)
	plan := BuildComplexPlan(steps, d.snapshotPickupBins(steps), bp.Code, src.Name)

	assigned, outcome, err := d.reserveComplexPlan(order, plan)
	testutil.MustNoErr(t, err, "reserve")
	if outcome != reserveComplete {
		t.Fatalf("outcome = %v, want reserveComplete — the empty staging pickup is a re-grab, not a miss", outcome)
	}
	if len(assigned) != 1 || assigned[0].binID != binSrc.ID {
		t.Fatalf("assigned = %+v, want exactly the source bin %d", assigned, binSrc.ID)
	}
}

// TestMoveToSourcingIdempotent pins the commit-4 helper change: sourcing→sourcing
// is a no-op (the reserve-retry loop re-enters it every tick), while a genuinely
// illegal transition is still rejected.
func TestMoveToSourcingIdempotent(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	srcing := &orders.Order{EdgeUUID: "mts-sourcing", StationID: "s", OrderType: OrderTypeRetrieve, Status: StatusSourcing, Quantity: 1}
	testutil.MustNoErr(t, db.CreateOrder(srcing), "create sourcing order")
	testutil.MustNoErr(t, db.UpdateOrderStatus(srcing.ID, string(StatusSourcing), "setup"), "force sourcing")
	srcing.Status = StatusSourcing
	if err := d.lifecycle.MoveToSourcing(srcing, "test", "retry"); err != nil {
		t.Fatalf("MoveToSourcing(sourcing→sourcing) must be a no-op, got %v", err)
	}
	if srcing.Status != StatusSourcing {
		t.Errorf("status after idempotent MoveToSourcing = %q, want sourcing", srcing.Status)
	}

	// A terminal order → sourcing is still rejected.
	term := &orders.Order{EdgeUUID: "mts-terminal", StationID: "s", OrderType: OrderTypeRetrieve, Status: StatusConfirmed, Quantity: 1}
	testutil.MustNoErr(t, db.CreateOrder(term), "create terminal order")
	term.Status = StatusConfirmed
	if err := d.lifecycle.MoveToSourcing(term, "test", "x"); err == nil {
		t.Error("MoveToSourcing(confirmed→sourcing) must be rejected as illegal, got nil")
	}
}
