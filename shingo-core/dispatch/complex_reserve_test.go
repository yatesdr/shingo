//go:build docker

package dispatch

// Tests for the reserve/confirm split (plan-time reserve + apply-as-confirm)
// against real Postgres.

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
	"shingocore/store/payloads"
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

// TestPartialHoldRetriesToComplete pins the dispatch GO gate (the relay rule):
// a complex order needing {A,B} with only A present holds A and reports
// reserveHolding (NOT complete, NOT
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

// TestConfirmHealsClaimedButPendingBin is the wedge pin. It reproduces the
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

// TestReserveFillerLegWithEmptySourceHoldsNotMoot is the other side of the moot
// boundary, and the one Hopkinsville fell through on 2026-07-22.
//
// A leg that PLACES a bin on the line (a press-index R2, a two_robot supply) reads
// IDENTICALLY to a moot evac at the reserve — nothing reserved, source node
// genuinely empty — but it means the opposite. The evac's source is empty because
// the bin it came to remove is gone, so the work is void. The filler's source is
// empty because the replacement has not been STAGED there yet, which is demand,
// and demand is operator-driven and never evaporates.
//
// Skipping it deleted the order ~5ms after creation and took its evac sibling down
// with it via the swap cascade, leaving nothing on the board to explain why. Worse,
// it was shape-dependent: a GROUP source falls to SourceFinder and queues
// (waiting_for_material), so only a CONCRETE source — which is exactly what every
// press-index and paired-position swap uses — could never wait for material.
func TestReserveFillerLegWithEmptySourceHoldsNotMoot(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	// The index/staging position the filler collects from — created empty, i.e.
	// nothing has stocked it yet.
	index := &nodes.Node{Name: "INDEX-POS", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(index), "index node")
	steps := []resolvedStep{
		{Action: protocol.ActionWait, Node: index.Name},
		{Action: protocol.ActionPickup, Node: index.Name},
		{Action: protocol.ActionDropoff, Node: lineNode.Name},
	}
	// ProcessNode is the LINE — this leg fills the shared line position from the
	// index position. That is what makes it a filler rather than an evac, and it is
	// read from the steps (legPlacesLineBin), not from the node names.
	order := mkComplexOrder(t, db, "filler-1", index.Name, lineNode.Name, lineNode.Name, bp.Code, steps)
	plan := BuildComplexPlan(steps, d.snapshotPickupBins(steps), bp.Code, lineNode.Name)

	_, outcome, err := d.allocator.reserveComplexPlan(order, plan)
	testutil.MustNoErr(t, err, "reserve")
	if outcome != reserveHolding {
		t.Fatalf("outcome = %v, want reserveHolding — an unstaged filler source is demand waiting for material, not void work", outcome)
	}
}

// TestReservePresentButTakenHoldsNotMoot pins the moot/hold boundary: the
// moot-skip route fires ONLY when every unmet need is a concrete node verified
// genuinely EMPTY. A node that HOLDS a bin claimed by ANOTHER order is present-but-
// taken — sourceable once that order finishes — so the reserve HOLDS and retries
// (operator-driven demand, never aged out), never skips the changeover as moot.
// This is the exact contrast to TestReserveMootWhenAllSourcesEmpty (empty node → moot).
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

// TestStagingRegrabsNotTreatedAsMissing pins the relay rule end-to-end at the
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

// ── the reconcile wire (the split-brain fix) ─────────────────────────

// TestComplexHoldingSlotsAsReservationsAcrossTicks is the split-brain PIN. An
// incomplete complex order (its source bin not yet available) holds its destination
// slot as a revocable RESERVATION while it sits in `sourcing` — NOT as a hard
// nodes.claimed_by. Before the slot substrate the hard-claim slot loop set
// nodes.claimed_by at dispatch
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
		t.Fatalf("slot claimed_by=%d while SOURCING — the split-brain: an incomplete order must hold slots as RESERVATIONS, not hard nodes.claimed_by", *slotN.ClaimedBy)
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

// TestDeclaredStagingDropoffIsReserved is the SPR AMR-04 fix, both directions.
//
// ── THE DEFECT ────────────────────────────────────────────────────────────
//
// slotNeeds gated on isConcreteStorageDropoff, which bails at `ParentID == nil`.
// A staging node is seeded as a station with NO PARENT, so it never reached the
// LANE/NGRP test and was reserved by nothing and capacity-checked by nothing.
// slotNeeds' own docstring claimed "staging/relay included" — the predicate's
// name and its comment both promised coverage that was not there, which is why
// nobody re-read it.
//
// Springfield, 2026-08-12: AMR-04 held a bin 48 minutes at a full SLN_003, the
// fleet reporting RUNNING with no error, until an admin cancelled the order two
// hours in.
//
// ── WHY A DECLARATION AND NOT A WIDER PREDICATE ───────────────────────────
//
// Accepting every parentless station would readmit LINE nodes, and gating those
// re-creates the deadlock 2b05dce fixed. Core cannot tell the two apart: one
// STATION node type, an advisory plantspec Kind that is never persisted, and the
// staging designation living in the EDGE cell config. So the author declares it.
//
// BOTH ARMS ARE THE POINT. The fixture is identical apart from the flag, so a
// change that reserved every station dropoff would pass the first arm and fail
// the second — which is the deadlock coming back.
//
// MUTATION (verified): drop the `!s.ExclusiveSlot &&` clause from slotNeeds. The
// declared arm stops reserving and fails.
func TestDeclaredStagingDropoffIsReserved(t *testing.T) {
	t.Parallel()

	// run builds the fixture once and reports whether the dropoff node ended up
	// reserved. `declared` is the ONLY thing that differs between the two arms.
	run := func(t *testing.T, tag string, declared bool) bool {
		t.Helper()
		db := testDB(t)
		_, _, bp := setupTestData(t, db)
		d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

		// A STAGING node, built the way the seeder builds one: a station with NO
		// PARENT. That nil parent is the whole defect — isConcreteStorageDropoff
		// bails on it before it ever asks about LANE/NGRP.
		staging := &nodes.Node{Name: tag + "-STAGING", Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(staging), "create staging node")
		if staging.ParentID != nil {
			t.Fatal("the staging fixture has a parent — then isConcreteStorageDropoff might cover " +
				"it structurally and this test proves nothing about the declaration")
		}
		src := &nodes.Node{Name: tag + "-SRC", Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(src), "create src")

		// A bin already taken at the source, so the order HOLDS in sourcing rather
		// than completing — the slot reserve runs first and its result stays
		// observable. Same device as TestComplexHoldingSlotsAsReservationsAcrossTicks.
		takenBin := testdb.CreateBinAtNode(t, db, bp.Code, src.ID, tag+"-TAKEN")
		other := testdb.CreateOrder(t, db)
		testdb.ClaimBinForTest(t, db, takenBin.ID, other.ID)

		steps := []resolvedStep{
			{Action: protocol.ActionPickup, Node: src.Name},
			{Action: protocol.ActionDropoff, Node: staging.Name, ExclusiveSlot: declared},
		}
		order := mkComplexOrder(t, db, tag+"-order", src.Name, src.Name, staging.Name, bp.Code, steps)
		_ = d.DispatchPreparedComplex(order) // holds; the reserve is what we read

		held, lerr := db.ListReservationsByOrder(order.ID)
		testutil.MustNoErr(t, lerr, "list reservations")
		for _, r := range held {
			if r.Kind == reservations.KindSlot && r.NodeID == staging.ID {
				return true
			}
		}
		return false
	}

	t.Run("declared — the node is reserved", func(t *testing.T) {
		if !run(t, "DECL", true) {
			t.Fatal("a DECLARED staging dropoff reserved nothing. Nothing then stops a second order " +
				"taking the node, and the first robot arrives to find it occupied — SPR AMR-04, " +
				"48 minutes")
		}
	})

	t.Run("undeclared — unchanged, and deliberately so", func(t *testing.T) {
		if run(t, "PLAIN", false) {
			t.Fatal("an UNDECLARED station dropoff was reserved. That is the LINE node case: a " +
				"supply leg delivers to a node its sibling evac is on the way to clear, and " +
				"reserving it re-creates the deadlock 2b05dce fixed. Absent must stay exactly " +
				"today's behaviour")
		}
	})
}

// TestDeclaredStagingSlotConflictHolds is the protection itself: the reservation
// is worth having only if the SECOND order cannot have the node.
//
// A staging dropoff is fixed-concrete — it carries no NGRP group, because there
// is no group of interchangeable staging nodes to re-resolve within. So the loser
// takes the HOLD arm rather than the revert arm (the contrast
// TestSlotConflictRevertsToNGRP draws), and stays queued until the node frees.
// That is the whole behaviour change: the order waits in a list instead of a
// robot waiting in an aisle.
func TestDeclaredStagingSlotConflictHolds(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, _, bp := setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	staging := &nodes.Node{Name: "CONF-STAGING", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(staging), "create staging node")
	src := &nodes.Node{Name: "CONF-SRC", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(src), "create src")

	takenBin := testdb.CreateBinAtNode(t, db, bp.Code, src.ID, "CONF-TAKEN")
	blocker := testdb.CreateOrder(t, db)
	testdb.ClaimBinForTest(t, db, takenBin.ID, blocker.ID)

	steps := func() []resolvedStep {
		return []resolvedStep{
			{Action: protocol.ActionPickup, Node: src.Name},
			{Action: protocol.ActionDropoff, Node: staging.Name, ExclusiveSlot: true},
		}
	}
	first := mkComplexOrder(t, db, "conf-first", src.Name, src.Name, staging.Name, bp.Code, steps())
	_ = d.DispatchPreparedComplex(first)

	second := mkComplexOrder(t, db, "conf-second", src.Name, src.Name, staging.Name, bp.Code, steps())
	_ = d.DispatchPreparedComplex(second)

	// THE PIN: exactly one holder. Two orders both believing they own the staging
	// node is the pre-fix world with extra bookkeeping.
	owners := 0
	for _, id := range []int64{first.ID, second.ID} {
		held, lerr := db.ListReservationsByOrder(id)
		testutil.MustNoErr(t, lerr, "list reservations")
		for _, r := range held {
			if r.Kind == reservations.KindSlot && r.NodeID == staging.ID {
				owners++
			}
		}
	}
	if owners != 1 {
		t.Fatalf("%d orders hold a slot reservation on the staging node, want exactly 1 — the "+
			"per-node unique index is what makes the declaration mean anything, and without it "+
			"this is bookkeeping rather than arbitration", owners)
	}
}

// TestDeclaredStagingOccupiedByABinQueues is the OTHER gate — the one that reads
// the floor rather than the reservation table.
//
// The slot reservation arbitrates between ORDERS. It cannot see a bin that is
// physically standing on the node with no order behind it: an operator's manual
// move, a dig parking a blocker, anything that placed a bin outside the order
// path. Only CheckDropoffCapacity looks at the bins. Dropping either gate leaves
// a way to arrive at an occupied node, so both run.
//
// ── AND THIS FIXTURE IS THE COMPOSITION HOLE ──────────────────────────────
//
// The staging node here IS order.DeliveryNode — the BuildStageSteps shape,
// pickup source then dropoff staging with nothing after it. That combination
// slips through both arms if they are composed carelessly: the DeliveryNode arm
// skips it (a staging node fails isConcreteStorageDropoff), and a declared-loop
// that skipped nodes merely for EQUALLING DeliveryNode would skip it too. The
// arms have to compose on what was ASKED, not on which node it was, which is why
// the caller carries finalChecked rather than comparing names.
//
// MUTATION (verified): change the loop's guard to `s.Node == order.DeliveryNode`
// without the finalChecked term. This test fails — the order dispatches onto an
// occupied staging node.
func TestDeclaredStagingOccupiedByABinQueues(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, _, bp := setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	staging := &nodes.Node{Name: "OCC-STAGING", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(staging), "create staging node")
	src := &nodes.Node{Name: "OCC-SRC", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(src), "create src")

	// A bin the order path knows nothing about, standing on the staging node —
	// the manual-move / dig-blocker case. No reservation exists for it, so the
	// slot reserve would happily hand the node out.
	testdb.CreateBinAtNode(t, db, bp.Code, staging.ID, "OCC-SQUATTER")
	// And a free source bin, so nothing ELSE holds this order back and a dispatch
	// would genuinely happen if the gate let it.
	testdb.CreateBinAtNode(t, db, bp.Code, src.ID, "OCC-SOURCE")

	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: src.Name},
		{Action: protocol.ActionDropoff, Node: staging.Name, ExclusiveSlot: true},
	}
	order := mkComplexOrder(t, db, "occ-order", src.Name, src.Name, staging.Name, bp.Code, steps)

	err := d.DispatchPreparedComplex(order)
	if err == nil {
		t.Fatal("the order dispatched onto a staging node with a bin already standing on it. " +
			"The reservation table is empty for that bin — nothing but the capacity check reads " +
			"the floor, and this is the 48-minute stall with the robot already committed")
	}
	if !strings.Contains(err.Error(), "dropoff capacity") {
		t.Fatalf("order was held by %q, want the dropoff-capacity gate — a different reason "+
			"means this fixture stopped exercising the arm it is named after", err)
	}
	got, gerr := db.GetOrder(order.ID)
	testutil.MustNoErr(t, gerr, "reload order")
	if got.Status == StatusDispatched || got.Status == StatusInTransit {
		t.Errorf("order status=%q — it must park, not proceed", got.Status)
	}
}

// TestChoreographyRefillsANodeItEmptiesItself is the keep-staged shape, and the
// self-wedge the capacity gate would otherwise have introduced.
//
// ── THE SHAPE ─────────────────────────────────────────────────────────────
//
// BuildKeepStagedCombinedSteps (Edge):
//
//  1. pickup  InboundStaging   ← carries the keep-staged bin away
//  2. dropoff InboundSource        (returns it to the market)
//  3. pickup  InboundSource        (grabs the changeover material)
//  4. dropoff InboundStaging   ← puts the NEW bin where the old one was
//
// At dispatch, before a robot has moved, the staging node IS occupied — by the
// bin step 1 exists to remove. A capacity gate that reads the floor and stops
// there parks the order waiting for a node that only the parked order would have
// cleared. Nothing else ever will, so it waits forever: strictly worse than the
// 48-minute stall this whole change is about, because a stall ends.
//
// This is the 2b05dce deadlock one rung in — the clearing leg belongs to THIS
// order rather than a sibling. The difference is that Core can see it: the plan
// is in hand, so no declaration and no SiblingOrderID is needed to know the node
// will be free on arrival.
//
// ── AND THE RESERVATION STILL HAPPENS ─────────────────────────────────────
//
// Asserted below, because it is the half that must NOT be relaxed. The exemption
// is about the FLOOR (a bin standing there now), not about other ORDERS. This
// order still takes the staging node so a second order cannot have it while the
// choreography is mid-flight.
//
// MUTATION (verified): drop the clearedEarlierInPlan guard. The order parks on
// dropoff-occupied and never dispatches.
func TestChoreographyRefillsANodeItEmptiesItself(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, _, bp := setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	staging := &nodes.Node{Name: "CHOREO-STAGING", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(staging), "create staging node")
	market := &nodes.Node{Name: "CHOREO-MARKET", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(market), "create market")

	// THE KEEP-STAGED BIN, standing on the staging node at dispatch time. This is
	// the occupancy the naive gate trips over.
	testdb.CreateBinAtNode(t, db, bp.Code, staging.ID, "CHOREO-KEPT")
	// And the changeover material the order picks up at step 3.
	testdb.CreateBinAtNode(t, db, bp.Code, market.ID, "CHOREO-NEW")

	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: staging.Name},
		{Action: protocol.ActionDropoff, Node: market.Name},
		{Action: protocol.ActionPickup, Node: market.Name},
		{Action: protocol.ActionDropoff, Node: staging.Name, ExclusiveSlot: true},
	}
	order := mkComplexOrder(t, db, "choreo", staging.Name, staging.Name, staging.Name, bp.Code, steps)

	if err := d.DispatchPreparedComplex(order); err != nil &&
		strings.Contains(err.Error(), "dropoff capacity") {
		t.Fatalf("the order was parked on dropoff capacity: %v\n"+
			"Its step 4 places onto a node its OWN step 1 empties. Gating that is a self-wedge — "+
			"the order waits for a node only the parked order would have cleared, so it waits "+
			"forever. A choreography that refills what it empties is not a conflict", err)
	}

	// THE OTHER HALF: the exemption is about the floor, not about other orders.
	// The staging node must still be reserved, or a second order can walk into
	// the middle of the choreography.
	held, lerr := db.ListReservationsByOrder(order.ID)
	testutil.MustNoErr(t, lerr, "list reservations")
	reserved := false
	for _, r := range held {
		if r.Kind == reservations.KindSlot && r.NodeID == staging.ID {
			reserved = true
		}
	}
	if !reserved {
		t.Errorf("the order holds no reservation on %s. Exempting the node from the CAPACITY check "+
			"must not exempt it from arbitration between orders — the choreography still owns that "+
			"node for its duration", staging.Name)
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

// TestEvacMismatchedPressBin_DispatchesCompleteAndSurfaces is the reworked
// Hopkinsville 2026-07-14 pin. The original incident was a PARTIAL dispatch: a
// two_robot_press_index R1 leg has two pickups — the spent bin on the press and a
// fresh carrier from the market — and someone re-stamped the press bin's payload
// (PIA16 press, bin tagged PIA15). Under a payload-filtered pickup only the market
// bin claimed, so order.BinID fell back to that EMPTY CARRIER, Core shipped a
// single-bin envelope, and Edge bound the empty tote to the press and drove its
// UOP tile to 0.
//
// The real invariant that damage violated is "never ship a partial / wrong-bin
// order", NOT "never touch a mismatched bin". An evac's job is to remove whatever
// is resident, so the removal binds the press bin BY NODE regardless of its
// payload — which makes the order COMPLETE (both pickups bound to the RIGHT bins),
// so there is no fallback-to-empty-carrier BinID at all. Holding on the mismatch
// (the old behavior) was the wrong lever: it wedges forever on a benign off-style
// leftover (SPR ALN_006) and stops the line over a mislabel a robot could finish.
// The mismatch is instead SURFACED (audit + log) for operator follow-up.
//
// So this pins the new contract: the evac binds the resident bin, the order
// dispatches complete with the press pickup bound to the PRESS bin (never the
// carrier), and the mismatch is surfaced.
func TestEvacMismatchedPressBin_DispatchesCompleteAndSurfaces(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	// The press (process node) and the market.
	press := &nodes.Node{Name: "PMM-PRESS", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(press), "press node")
	market := &nodes.Node{Name: "PMM-MARKET", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(market), "market node")

	// A DIFFERENT payload — this is the mis-stamp. The bin sitting on the press
	// carries it, so it cannot be claimed for an order whose payload is bp.Code.
	other := &payloads.Payload{Code: "PART-WRONG", Description: "mis-stamped", UOPCapacity: 1000}
	testutil.MustNoErr(t, db.CreatePayload(other), "other payload")

	pressBin := testdb.CreateBinAtNode(t, db, other.Code, press.ID, "PMM-PRESS-BIN") // unclaimable
	marketBin := testdb.CreateBinAtNode(t, db, bp.Code, market.ID, "PMM-MARKET-BIN") // claimable

	// R1: clear the press → store it → fetch a fresh carrier → stage it.
	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: press.Name},
		{Action: protocol.ActionDropoff, Node: lineNode.Name},
		{Action: protocol.ActionPickup, Node: market.Name},
		{Action: protocol.ActionDropoff, Node: lineNode.Name},
	}
	order := mkComplexOrder(t, db, "pmm-1", press.Name, press.Name, lineNode.Name, bp.Code, steps)

	// Drive the REAL dispatch entry point. The evac must BIND the resident
	// off-payload press bin (remove whatever is there), so the order dispatches
	// COMPLETE instead of shipping a 1-of-2 partial that stamps the empty market
	// carrier as the press's bin (the original damage).
	if derr := d.DispatchPreparedComplex(order); derr != nil {
		t.Fatalf("DispatchPreparedComplex returned %v — an evac must bind the resident bin regardless of its payload, not wedge on a mismatch", derr)
	}
	got, gerr := db.GetOrder(order.ID)
	testutil.MustNoErr(t, gerr, "reload order")

	// COMPLETE, not partial: both pickups bound their bins.
	pb, _ := db.GetBin(pressBin.ID)
	if pb.ClaimedBy == nil || *pb.ClaimedBy != order.ID {
		t.Fatalf("press bin claimed_by = %v, want evac order %d — the removal must bind the resident bin by node, payload-agnostic", pb.ClaimedBy, order.ID)
	}
	mb, _ := db.GetBin(marketBin.ID)
	if mb.ClaimedBy == nil || *mb.ClaimedBy != order.ID {
		t.Fatalf("market bin claimed_by = %v, want evac order %d — a complete reserve claims BOTH pickups", mb.ClaimedBy, order.ID)
	}

	// THE ANTI-DAMAGE: the press pickup is bound to the PRESS bin, never the empty
	// carrier. On the old build order.BinID fell back to the market tote, which is
	// what drove the press UOP tile to 0. A complete bind records the right bin
	// against the press pickup.
	obs, _ := db.ListOrderBins(order.ID)
	if len(obs) != 2 {
		t.Fatalf("order_bins rows = %d, want 2 — a complete evac records both pickups; a 1-of-2 partial is exactly the HK damage", len(obs))
	}
	var pressRowBin int64 = -1
	for _, ob := range obs {
		if ob.NodeName == press.Name {
			pressRowBin = ob.BinID
		}
	}
	if pressRowBin != pressBin.ID {
		t.Fatalf("press pickup bound bin %d, want the resident press bin %d — never the empty carrier (HK 2026-07-14 drove the press to 0 by stamping the market tote here)", pressRowBin, pressBin.ID)
	}

	// It dispatched — the whole point is no wedge.
	if got.VendorOrderID == "" {
		t.Error("no vendor order created — a complete evac must dispatch, not hold")
	}

	// And the mismatch was SURFACED, not silently swallowed.
	entries, _ := db.ListEntityAudit("bin", pressBin.ID)
	surfaced := false
	for _, e := range entries {
		if e.Action == "evac_payload_mismatch" {
			surfaced = true
		}
	}
	if !surfaced {
		t.Error("no evac_payload_mismatch audit entry — an off-payload evac must surface the anomaly for operator follow-up")
	}
}
