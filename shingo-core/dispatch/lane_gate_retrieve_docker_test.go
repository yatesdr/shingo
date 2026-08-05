//go:build docker

package dispatch

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// lane_gate_retrieve_docker_test.go — the retrieve-direction gate valve + release
// evaluator, against a real DB and a recording fleet backend.
//
// The store docker battery (lane_gate_dispatch_docker_test.go /
// lane_gate_release_docker_test.go) proves the INBOUND (store) gate. This is the
// OUTBOUND (retrieve) mirror: a retrieve whose SOURCE is a gated lane slot ships
// unsealed to the lane's wait point and has its [pickup@slot, dropoff@line] tail
// appended when the lane is safe. Same valve, the other end of the order.

// placeBin drops a bin into a slot so a retrieve has something to pull out.
func placeBin(t *testing.T, db *store.DB, slot *nodes.Node) {
	t.Helper()
	testdb.CreateBinAtNode(t, db, "DEFAULT", slot.ID, "BIN-"+slot.Name)
}

// gateRetrieveLane is gateChoreoLane with a BIN placed in the deep slot, so a
// retrieve has something to pull out. Returns the lane id, the deep slot (the
// retrieve's source), and the shallow slot.
func gateRetrieveLane(t *testing.T, db *store.DB, name, gatePoint string) (laneID int64, s0, s1 *nodes.Node) {
	t.Helper()
	laneID, s0, s1 = gateChoreoLane(t, db, name, gatePoint)
	placeBin(t, db, s1) // the bin a retrieve pulls out of the deep slot
	return laneID, s0, s1
}

// TestGateChoreo_RetrieveOpenValveCreatesUnsealedThenAppends: with the lane clear,
// a gated retrieve ships unsealed as [wait@gate] and its [pickup@slot, dropoff@line]
// tail is appended in the same call. The uniform-shape claim applied to the retrieve
// direction — no bypass class, the open valve is the immediate append.
//
// The create carries ONLY the wait (the wait comes FIRST for a retrieve — there is no
// legal work before the lane opens). Contrast the store create [pickup, wait], where
// the pickup at the line source precedes the dwell.
func TestGateChoreo_RetrieveOpenValveCreatesUnsealedThenAppends(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	_, _, s1 := gateRetrieveLane(t, db, "GCRET", "GCRET-WAIT")
	line := lineNode(t, db, "GCRET-LINE")

	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.OrderType = OrderTypeRetrieve
		o.SourceNode = s1.Name
		o.DeliveryNode = line.Name
		o.Status = "sourcing"
	})

	vendorID, err := d.DispatchDirect(order, s1, line)
	if err != nil {
		t.Fatalf("DispatchDirect: %v", err)
	}

	creates := backend.CreateRequests()
	if len(creates) != 1 {
		t.Fatalf("create calls = %d, want 1", len(creates))
	}
	c := creates[0]
	if c.Complete {
		t.Error("a gated retrieve must be created UNSEALED (Complete=false) even when the lane is clear — no bypass class")
	}
	if len(c.Blocks) != 1 {
		t.Fatalf("create blocks = %d (%v), want 1: [wait@gate] — a retrieve has no work before the lane opens", len(c.Blocks), c.Blocks)
	}
	if c.Blocks[0].Location != "GCRET-WAIT" {
		t.Errorf("create block = %s, want GCRET-WAIT", c.Blocks[0].Location)
	}
	if c.Blocks[0].BinTask != "Wait" {
		t.Errorf("create block binTask = %q, want Wait", c.Blocks[0].BinTask)
	}
	for _, b := range c.Blocks {
		if b.Location == s1.Name {
			t.Error("the create must NOT contain the pickup — the slot is bound at append time")
		}
	}

	// The valve was open, so the tail went out immediately.
	appends := backend.ReleaseCalls()
	if len(appends) != 1 {
		t.Fatalf("append calls = %d, want 1 (an open valve appends immediately)", len(appends))
	}
	a := appends[0]
	if a.VendorOrderID != vendorID {
		t.Errorf("append targeted %q, want the created order %q", a.VendorOrderID, vendorID)
	}
	if !a.Complete {
		t.Error("the tail append must SEAL the order (complete=true)")
	}
	if len(a.Blocks) != 2 {
		t.Fatalf("append blocks = %d (%v), want 2: [pickup@slot, dropoff@line]", len(a.Blocks), a.Blocks)
	}
	if a.Blocks[0].Location != s1.Name {
		t.Errorf("append pickup block = %s, want the lane slot %s", a.Blocks[0].Location, s1.Name)
	}
	if a.Blocks[1].Location != line.Name {
		t.Errorf("append dropoff block = %s, want the line %s", a.Blocks[1].Location, line.Name)
	}
	for _, b := range a.Blocks {
		if b.BlockID == c.Blocks[0].BlockID {
			t.Errorf("appended block id %q collides with the create block id", b.BlockID)
		}
	}

	// Durable truth on the row: the tail landed, so wait_index advanced and the order
	// is no longer gate-staged.
	reloaded, err := db.GetOrder(order.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.WaitIndex != 1 {
		t.Errorf("wait_index = %d, want 1 after the tail sealed the order", reloaded.WaitIndex)
	}
	if IsGateStaged(reloaded) {
		t.Error("an order whose valve opened must not read as gate-staged")
	}
	var steps []resolvedStep
	if err := json.Unmarshal([]byte(reloaded.StepsJSON), &steps); err != nil {
		t.Fatalf("stored plan is not parseable: %v", err)
	}
	if len(steps) != 3 || steps[0].Action != protocol.ActionWait || steps[1].Action != protocol.ActionPickup {
		t.Errorf("stored plan = %+v, want [wait, pickup, dropoff]", steps)
	}
}

// TestGateChoreo_RetrieveContendedHoldsThenEvaluatorReleases: when a dig holds the
// lane, the retrieve creates unsealed and DWELLS — no tail. The release evaluator
// then releases it once the dig clears (the lane-lock drops).
//
// This is the buried-retrieve increment's production assertion: a retrieve
// pre-positions at the gate while a dig works, and Core releases it on dig
// completion. The gain is travel overlap (the sim side is proven in scenesim's
// buried_retrieve_test.go); this asserts the production valve + evaluator do their half.
func TestGateChoreo_RetrieveContendedHoldsThenEvaluatorReleases(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	laneID, _, s1 := gateRetrieveLane(t, db, "GCRET2", "GCRET2-WAIT")
	line := lineNode(t, db, "GCRET2-LINE")

	// A dig holds the lane: lock it. (Production acquires this in complex_reshuffle /
	// planning_service; here we take it directly to model "a dig is active".)
	//
	// The owner has to be a REAL order now. The lock used to grant from an
	// in-memory map and mirror the row on a best-effort basis, so a fabricated
	// owner id took the lock and merely logged an FK violation on the mirror.
	// The row IS the lock today: no row, no hold, and TryLock says so.
	digger := testdb.CreateOrder(t, db)
	if !d.laneLock.TryLock(laneID, digger.ID) {
		t.Fatal("TryLock on a free lane must succeed")
	}

	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.OrderType = OrderTypeRetrieve
		o.SourceNode = s1.Name
		o.DeliveryNode = line.Name
		o.Status = "sourcing"
	})
	if _, err := d.DispatchDirect(order, s1, line); err != nil {
		t.Fatalf("DispatchDirect: %v", err)
	}

	creates := backend.CreateRequests()
	if len(creates) != 1 || creates[0].Complete {
		t.Fatalf("want exactly 1 UNSEALED create, got %d (complete=%v)", len(creates), len(creates) > 0 && creates[0].Complete)
	}
	if n := len(backend.ReleaseCalls()); n != 0 {
		t.Fatalf("append calls = %d, want 0 — a dig-held retrieve must hold its tail", n)
	}
	reloaded, err := db.GetOrder(order.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !IsGateStaged(reloaded) {
		t.Fatalf("a dig-held retrieve must read as gate-staged (steps=%q wait=%d vendor=%q)",
			reloaded.StepsJSON, reloaded.WaitIndex, reloaded.VendorOrderID)
	}

	// The dig clears: drop the lock and fire the evaluator (as EventOrderCompleted would).
	d.laneLock.Unlock(laneID, digger.ID)
	d.EvaluateLaneReleases(laneID)

	appends := backend.ReleaseCalls()
	if len(appends) != 1 {
		t.Fatalf("after dig clear, append calls = %d, want 1 (the evaluator released the retrieve)", len(appends))
	}
	a := appends[0]
	if !a.Complete {
		t.Error("the release append must SEAL the retrieve")
	}
	if len(a.Blocks) != 2 || a.Blocks[0].Location != s1.Name || a.Blocks[1].Location != line.Name {
		t.Errorf("append blocks = %v, want [pickup@%s, dropoff@%s]", a.Blocks, s1.Name, line.Name)
	}
	if final, _ := db.GetOrder(order.ID); IsGateStaged(final) || final.WaitIndex != 1 {
		t.Errorf("after release, order should be sealed (wait_index=1, not staged); got wait_index=%d staged=%v",
			final.WaitIndex, IsGateStaged(final))
	}
}

// TestGateChoreo_RetrieveOwnDigLegIsNotParked is defect 1: a reshuffle leg parked
// at the gate by its OWN parent's dig lock, which only that leg can clear.
//
// The chain the fixture reproduces, all production shapes:
//
//   - the dig lock's owner is the buried retrieve itself — complex_reshuffle.go
//     and planning_service.go both call TryLock(laneID, order.ID) and then make
//     that same order the compound parent. So the parent IS the dig owner.
//   - a leg's SOURCE is a lane slot, and legs are not Coordinated, so
//     dispatchToFleetCore routes them through resolveLaneGateSource →
//     dispatchGatedRetrieve like any other lane-sourced order. Legs never meet
//     the scanner's AcquireLanesForOrder, so the gate is the only gate they meet.
//   - laneGateRetrieveCause then parked on "a dig holds this lane" without asking
//     WHOSE. The parent's lock releases when the reshuffle completes, and the
//     reshuffle completes by running this leg. Deadlock.
//
// ONE LEG, DELIBERATELY. Hold B (compound.go's laneOccupiedForChild) refuses to
// dispatch a leg while a sibling is inside the lane, so a two-leg fixture that
// asserted "the second leg never runs" would pass identically whether the gate is
// wrong or not — it would be proving Hold B works. With a single leg there is no
// sibling, Hold B is satisfied, the leg reaches the fleet, and the only thing that
// can hold it at the gate is its own parent's dig.
//
// The leg delivers to a LINE, which is target-node reshuffle mode
// (PlanReshuffleToTarget): the blocker leaves the lane. That keeps the case about
// the SOURCE gate — an expose-mode leg dropping into a shuffle slot in the same
// lane would take resolveLaneGateTarget's store branch instead, and laneEntryCause
// never consults the dig lock at all.
//
// MUTATION (verified): make laneOwnerFor return the order's own id instead of its
// parent's (drop the ParentOrderID hop). The exemption then fails to match, the leg
// parks, and this test's own "append calls = 0" assertion fires. That mutation is
// the point: the test depends on the PARENT hop, not merely on some exemption
// existing.
func TestGateChoreo_RetrieveOwnDigLegIsNotParked(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	laneID, _, s1 := gateRetrieveLane(t, db, "GCDIG", "GCDIG-WAIT")
	line := lineNode(t, db, "GCDIG-LINE")

	// The reshuffle parent takes the dig, exactly as the two production planners do.
	parent := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.Status = protocol.StatusReshuffling
	})
	if !d.laneLock.TryLock(laneID, parent.ID) {
		t.Fatal("TryLock on a free lane must succeed")
	}

	leg := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.OrderType = OrderTypeMove
		o.SourceNode = s1.Name
		o.DeliveryNode = line.Name
		o.ParentOrderID = &parent.ID
		o.Sequence = 1
		o.Status = "sourcing"
	})

	if _, err := d.DispatchDirect(leg, s1, line); err != nil {
		t.Fatalf("DispatchDirect: %v", err)
	}

	// THE ASSERTION. The leg's tail went out: it entered the lane its own parent is
	// digging, because that dig is what the leg exists to perform.
	appends := backend.ReleaseCalls()
	if len(appends) != 1 {
		reloaded, _ := db.GetOrder(leg.ID)
		t.Fatalf("append calls = %d, want 1 — the leg is parked at the gate by its own parent's dig "+
			"(leg %d, parent/dig owner %d, gate-staged=%v). Only this leg can end that dig, so nothing "+
			"clears the lock and the reshuffle never completes",
			len(appends), leg.ID, parent.ID, IsGateStaged(reloaded))
	}
	a := appends[0]
	if len(a.Blocks) != 2 || a.Blocks[0].Location != s1.Name || a.Blocks[1].Location != line.Name {
		t.Errorf("append blocks = %v, want [pickup@%s, dropoff@%s]", a.Blocks, s1.Name, line.Name)
	}
	if final, _ := db.GetOrder(leg.ID); IsGateStaged(final) || final.WaitIndex != 1 {
		t.Errorf("the leg should be sealed (wait_index=1, not staged); got wait_index=%d staged=%v",
			final.WaitIndex, IsGateStaged(final))
	}

	// The dig row is untouched: the exemption lets the leg THROUGH, it does not end
	// the dig. Only the reshuffle's own completion does that (LaneLock.Unlock).
	if owner := d.laneLock.LockedBy(laneID); owner != parent.ID {
		t.Errorf("dig owner after the leg passed = %d, want the parent %d still holding — "+
			"an exemption that released the dig would be a second writer for the lane hold",
			owner, parent.ID)
	}
}
