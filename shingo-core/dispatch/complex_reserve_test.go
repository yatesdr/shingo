//go:build docker

package dispatch

// Tests for the 1c reserve/confirm split (commit 4, D39) against real Postgres.

import (
	"encoding/json"
	"errors"
	"strings"
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
			return string(r.State)
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
	_, outcome, err := d.allocator.reserveComplexPlan(order, planAB)
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
	_, outcome2, err := d.allocator.reserveComplexPlan(order, planC)
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
	_, outcome1, err := d.allocator.reserveComplexPlan(order, plan)
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
	assigned2, outcome2, err := d.allocator.reserveComplexPlan(order, plan)
	testutil.MustNoErr(t, err, "reserve tick 2")
	if outcome2 != reserveComplete {
		t.Fatalf("tick 2 outcome = %v, want reserveComplete", outcome2)
	}
	if cerr := d.allocator.confirmComplexPlan(order, plan, assigned2); cerr != nil {
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

	assigned, outcome, err := d.allocator.reserveComplexPlan(order, plan)
	testutil.MustNoErr(t, err, "reserve")
	if outcome != reserveComplete {
		t.Fatalf("outcome = %v, want reserveComplete", outcome)
	}

	// Simulate the pending reservation vanishing (TTL reap) between reserve and confirm.
	testutil.MustNoErr(t, db.ReleaseReservation(order.ID, binA.ID), "reap pending")

	cerr := d.allocator.confirmComplexPlan(order, plan, assigned)
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
	assigned2, _, err := d.allocator.reserveComplexPlan(order, plan)
	testutil.MustNoErr(t, err, "re-reserve")
	testutil.MustNoErr(t, d.allocator.confirmComplexPlan(order, plan, assigned2), "first confirm")
	// Re-derive the assignment (now the reservation is confirmed) and confirm again.
	assigned3, _, err := d.allocator.reserveComplexPlan(order, plan)
	testutil.MustNoErr(t, err, "reserve for idempotent confirm")
	if cerr := d.allocator.confirmComplexPlan(order, plan, assigned3); cerr != nil {
		t.Fatalf("idempotent confirm of an already-claimed-by-us bin must NOT fail: %v", cerr)
	}
}

// TestConfirmHealsClaimedButPendingBin is the D45 wedge pin. It reproduces the
// exact half-state a transient DB error / core restart leaves between the two
// separate writes ConfirmClaim used to make — the hard claim COMMITTED but the
// reservation confirm NOT run — and asserts the next confirm HEALS it instead of
// wedging codeClaimFailed forever.
//
// Pre-fix mechanism: bin is claimed_by=order with the reservation still pending;
// reconcile matches it (confirmed:false), the alreadyOurs skip was gated on
// rp.confirmed so it fell through to ConfirmClaim → bins.Claim required
// claimed_by IS NULL → 0 rows → claim_failed → requeue, every tick, forever (the
// owner-liveness reaper never fires: the order is live in `sourcing`). Fixed by the
// owner-idempotent claim CAS + claim/confirm-in-one-tx + the honest claimed-by-us skip.
func TestConfirmHealsClaimedButPendingBin(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	nodeA := &nodes.Node{Name: "HEAL-SRC", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(nodeA), "node A")
	binA := testdb.CreateBinAtNode(t, db, bp.Code, nodeA.ID, "HEAL-BIN")

	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: nodeA.Name},
		{Action: protocol.ActionDropoff, Node: lineNode.Name},
	}
	order := mkComplexOrder(t, db, "heal-1", nodeA.Name, nodeA.Name, lineNode.Name, bp.Code, steps)
	plan := BuildComplexPlan(steps, d.snapshotPickupBins(steps), bp.Code, nodeA.Name)

	// Seed the wedge: a PENDING reservation on binA AND the bin already hard-claimed
	// by the order (ClaimBin does not confirm the reservation) — claim committed,
	// confirm never ran.
	testdb.ReserveBin(t, db, order.ID, binA.ID)
	testutil.MustNoErr(t, db.ClaimBin(binA.ID, order.ID), "seed committed claim (reservation stays pending)")
	if seed, _ := db.ListReservationsByOrder(order.ID); stateOf(seed, binA.ID) != "pending" {
		t.Fatalf("seed reservation = %q, want pending (the wedge half-state)", stateOf(seed, binA.ID))
	}

	assigned, outcome, err := d.allocator.reserveComplexPlan(order, plan)
	testutil.MustNoErr(t, err, "reserve")
	if outcome != reserveComplete {
		t.Fatalf("outcome = %v, want reserveComplete", outcome)
	}
	if cerr := d.allocator.confirmComplexPlan(order, plan, assigned); cerr != nil {
		t.Fatalf("confirm of a claimed-but-pending bin must SUCCEED (heal the wedge), got %v", cerr)
	}

	// Reservation healed pending→confirmed.
	res, _ := db.ListReservationsByOrder(order.ID)
	if st := stateOf(res, binA.ID); st != "confirmed" {
		t.Errorf("bin A reservation = %q, want confirmed after heal", st)
	}
	// No second bin claimed; the order still owns exactly binA.
	claimed, _ := db.ListBinsByClaim(order.ID)
	if len(claimed) != 1 || claimed[0].ID != binA.ID {
		t.Fatalf("claimed = %+v, want exactly binA=%d (heal must not claim a second bin)", claimed, binA.ID)
	}
	gotA, _ := db.GetBin(binA.ID)
	if gotA.ClaimedBy == nil || *gotA.ClaimedBy != order.ID {
		t.Errorf("bin A claimed_by = %v, want order %d", gotA.ClaimedBy, order.ID)
	}
}

// TestConfirmPartialFailureConverges pins the multi-bin confirm state machine
// (indigo-shrike §4.1: the most subtle on the branch, previously untested). A
// complex order with three needs confirms #1, then a mid-loop failure on #2 (its
// pending reservation reaped, the seatbelt's 0-rows path) requeues the whole
// attempt; the next tick re-reserves the reaped need and converges — every bin
// claimed by the order exactly once, every reservation confirmed, no stray
// reservations, order.BinID + order_bins correct, nothing double-claimed.
func TestConfirmPartialFailureConverges(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	nodeA := &nodes.Node{Name: "PF-A", Enabled: true}
	nodeB := &nodes.Node{Name: "PF-B", Enabled: true}
	nodeC := &nodes.Node{Name: "PF-C", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(nodeA), "node A")
	testutil.MustNoErr(t, db.CreateNode(nodeB), "node B")
	testutil.MustNoErr(t, db.CreateNode(nodeC), "node C")
	binA := testdb.CreateBinAtNode(t, db, bp.Code, nodeA.ID, "PF-BIN-A")
	binB := testdb.CreateBinAtNode(t, db, bp.Code, nodeB.ID, "PF-BIN-B")
	binC := testdb.CreateBinAtNode(t, db, bp.Code, nodeC.ID, "PF-BIN-C")

	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: nodeA.Name}, // process node → order.BinID
		{Action: protocol.ActionDropoff, Node: lineNode.Name},
		{Action: protocol.ActionPickup, Node: nodeB.Name}, // need #2 — forced to fail
		{Action: protocol.ActionDropoff, Node: lineNode.Name},
		{Action: protocol.ActionPickup, Node: nodeC.Name},
		{Action: protocol.ActionDropoff, Node: lineNode.Name},
	}
	order := mkComplexOrder(t, db, "partialfail-1", nodeA.Name, nodeA.Name, lineNode.Name, bp.Code, steps)
	plan := BuildComplexPlan(steps, d.snapshotPickupBins(steps), bp.Code, nodeA.Name)

	// ── Tick 1: reserve all three, then force the confirm to fail on need #2. ──
	assigned1, outcome1, err := d.allocator.reserveComplexPlan(order, plan)
	testutil.MustNoErr(t, err, "reserve tick 1")
	if outcome1 != reserveComplete {
		t.Fatalf("tick 1 outcome = %v, want reserveComplete", outcome1)
	}
	// Reap need #2's (binB) pending reservation mid-tick: ConfirmClaim on binB then
	// sees 0 rows (seatbelt) and fails AFTER binA (#1) has been confirmed+claimed.
	testutil.MustNoErr(t, db.ReleaseReservation(order.ID, binB.ID), "reap binB reservation")

	cerr := d.allocator.confirmComplexPlan(order, plan, assigned1)
	var pe *planningError
	if !errors.As(cerr, &pe) || pe.Code != codeClaimFailed {
		t.Fatalf("tick 1 confirm: got %v, want a codeClaimFailed planningError (requeue)", cerr)
	}
	gotA, _ := db.GetBin(binA.ID)
	if gotA.ClaimedBy == nil || *gotA.ClaimedBy != order.ID {
		t.Fatalf("tick 1: bin A claimed_by = %v, want order %d (confirmed before the fail)", gotA.ClaimedBy, order.ID)
	}
	gotB, _ := db.GetBin(binB.ID)
	gotC, _ := db.GetBin(binC.ID)
	if gotB.ClaimedBy != nil || gotC.ClaimedBy != nil {
		t.Fatalf("tick 1: B/C must stay unclaimed after the fail, got B=%v C=%v", gotB.ClaimedBy, gotC.ClaimedBy)
	}
	if c1, _ := db.ListBinsByClaim(order.ID); len(c1) != 1 {
		t.Fatalf("tick 1: %d bins claimed, want exactly 1 (A) — no partial double-claim", len(c1))
	}

	// ── Tick 2: re-reserve (re-acquires B) and confirm converges the whole set. ──
	assigned2, outcome2, err := d.allocator.reserveComplexPlan(order, plan)
	testutil.MustNoErr(t, err, "reserve tick 2")
	if outcome2 != reserveComplete {
		t.Fatalf("tick 2 outcome = %v, want reserveComplete", outcome2)
	}
	if cerr := d.allocator.confirmComplexPlan(order, plan, assigned2); cerr != nil {
		t.Fatalf("tick 2 confirm must converge, got %v", cerr)
	}

	claimed, _ := db.ListBinsByClaim(order.ID)
	if len(claimed) != 3 {
		t.Fatalf("converged claims = %d, want 3 (A, B, C)", len(claimed))
	}
	for _, id := range []int64{binA.ID, binB.ID, binC.ID} {
		g, _ := db.GetBin(id)
		if g.ClaimedBy == nil || *g.ClaimedBy != order.ID {
			t.Errorf("bin %d claimed_by = %v, want order %d", id, g.ClaimedBy, order.ID)
		}
	}
	res, _ := db.ListReservationsByOrder(order.ID)
	if !reservesExactly(res, binA.ID, binB.ID, binC.ID) {
		t.Fatalf("held = %+v, want exactly {A,B,C} confirmed, no strays", res)
	}
	for _, id := range []int64{binA.ID, binB.ID, binC.ID} {
		if st := stateOf(res, id); st != "confirmed" {
			t.Errorf("bin %d reservation = %q, want confirmed", id, st)
		}
	}
	if order.BinID == nil || *order.BinID != binA.ID {
		t.Errorf("order.BinID = %v, want binA=%d (process node)", order.BinID, binA.ID)
	}
	obs, _ := db.ListOrderBins(order.ID)
	if len(obs) != 3 {
		t.Errorf("order_bins rows = %d, want 3", len(obs))
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

	_, outcome, err := d.allocator.reserveComplexPlan(order, plan)
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

	assigned, outcome, err := d.allocator.reserveComplexPlan(order, plan)
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

	assigned, outcome, err := d.allocator.reserveComplexPlan(order, plan)
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

// ── commit 4: the reconcile wire (D4 split-brain fix) ─────────────────────────

// TestComplexHoldingSlotsAsReservationsAcrossTicks is the D4 split-brain PIN. An
// incomplete complex order (its source bin not yet available) holds its destination
// slot as a revocable RESERVATION while it sits in `sourcing` — NOT as a hard
// nodes.claimed_by. Pre-1d the hard-claim slot loop set nodes.claimed_by at dispatch
// even while the order held bins only as reservations across ticks: bins soft, slots
// hard — the split-brain. Now both halves are reservations until the complete set
// confirms together.
func TestComplexHoldingSlotsAsReservationsAcrossTicks(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, _, bp := setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	// A concrete storage-dropoff slot (child of an NGRP) and an EMPTY source node.
	grpType, err := db.GetNodeTypeByCode("NGRP")
	testutil.MustNoErr(t, err, "NGRP type")
	grp := &nodes.Node{Name: "SBR-NGRP", Enabled: true, IsSynthetic: true, NodeTypeID: &grpType.ID}
	testutil.MustNoErr(t, db.CreateNode(grp), "create NGRP")
	slot := &nodes.Node{Name: "SBR-SLOT", Enabled: true, ParentID: &grp.ID}
	testutil.MustNoErr(t, db.CreateNode(slot), "create slot")
	src := &nodes.Node{Name: "SBR-SRC", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(src), "create src")
	// A bin at src that is already TAKEN by another order — the bin need is
	// present-but-taken, so the order HOLDS and retries (not moot-skips, which would
	// correctly release the slot). This exercises the sourcing-with-partials state.
	takenBin := testdb.CreateBinAtNode(t, db, bp.Code, src.ID, "SBR-TAKEN")
	other := testdb.CreateOrder(t, db)
	testdb.ClaimBinForTest(t, db, takenBin.ID, other.ID)

	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: src.Name},
		{Action: protocol.ActionDropoff, Node: slot.Name},
	}
	order := mkComplexOrder(t, db, "split-brain-1", src.Name, src.Name, slot.Name, bp.Code, steps)

	// Dispatch: the slot reserve completes, the bin reserve is incomplete (empty
	// source) → the order HOLDS in sourcing.
	if derr := d.DispatchPreparedComplex(order); derr == nil {
		t.Fatal("order dispatched; expected a hold (bin source empty)")
	}

	// THE PIN: the slot is held as a RESERVATION, not a hard claim, while sourcing.
	slotN, _ := db.GetNode(slot.ID)
	if slotN.ClaimedBy != nil {
		t.Fatalf("slot claimed_by=%d while SOURCING — the D4 split-brain: an incomplete order must hold slots as RESERVATIONS, not hard nodes.claimed_by", *slotN.ClaimedBy)
	}
	held, _ := db.ListReservationsByOrder(order.ID)
	slotReserved := false
	for _, r := range held {
		if r.Kind == reservations.KindSlot && r.NodeID == slot.ID {
			slotReserved = true
		}
	}
	if !slotReserved {
		t.Fatalf("order holds no slot reservation on %s while sourcing; held=%+v", slot.Name, held)
	}
	if got, _ := db.GetOrder(order.ID); got.Status != StatusSourcing {
		t.Errorf("order status=%q, want sourcing (holding partials)", got.Status)
	}
}

// TestSlotConflictRevertsToNGRP pins the escape valve: a FUNGIBLE dropoff (a concrete
// slot carrying its NGRP group) whose slot is already reserved by another order
// reverts its step Node back to the group, so the next tick re-resolves to a free
// child. Fixed-concrete dropoffs (no group) instead hold — that contrast is the ABBA
// port above.
func TestSlotConflictRevertsToNGRP(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, _, bp := setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	grpType, err := db.GetNodeTypeByCode("NGRP")
	testutil.MustNoErr(t, err, "NGRP type")
	grp := &nodes.Node{Name: "RVT-NGRP", Enabled: true, IsSynthetic: true, NodeTypeID: &grpType.ID}
	testutil.MustNoErr(t, db.CreateNode(grp), "create NGRP")
	slot := &nodes.Node{Name: "RVT-SLOT", Enabled: true, ParentID: &grp.ID}
	testutil.MustNoErr(t, db.CreateNode(slot), "create slot")

	// Another order already holds the slot's reservation.
	other := testdb.CreateOrder(t, db)
	testutil.MustNoErr(t, db.ReserveSlot(slot.ID, other.ID), "other reserves slot")

	// Our order's dropoff resolved to the (taken) concrete slot but carries its NGRP
	// group — fungible, so a conflict reverts to the group.
	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: "RVT-SRC"},
		{Action: protocol.ActionDropoff, Node: slot.Name, Group: grp.Name},
	}
	order := mkComplexOrder(t, db, "revert-1", "RVT-SRC", "RVT-SRC", slot.Name, bp.Code, steps)

	outcome, err := d.allocator.reserveComplexSlots(order, steps)
	testutil.MustNoErr(t, err, "reserve slots")
	if outcome != reserveHolding {
		t.Fatalf("outcome = %v, want reserveHolding (fungible slot conflict)", outcome)
	}
	// The step reverted to the NGRP group in-place for the next tick's re-resolution.
	if steps[1].Node != grp.Name {
		t.Fatalf("step Node = %q, want reverted to NGRP %q (escape valve)", steps[1].Node, grp.Name)
	}
	// ...and persisted.
	if got, _ := db.GetOrder(order.ID); !strings.Contains(got.StepsJSON, grp.Name) {
		t.Errorf("persisted StepsJSON did not record the revert to %s: %s", grp.Name, got.StepsJSON)
	}
	// The other order's reservation is untouched.
	oHeld, _ := db.ListReservationsByOrder(other.ID)
	if len(oHeld) != 1 || oHeld[0].NodeID != slot.ID {
		t.Errorf("other order's slot reservation disturbed: %+v", oHeld)
	}
}
