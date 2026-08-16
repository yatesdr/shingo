//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// lane_gate_append_rollback_docker_test.go — PLAN unit 2c.
//
// THE DEFECT, IN TWO HALVES THAT MEET IN ONE FAILURE:
//
//  1. appendGateTail's failure rollback used the ORDER-WIDE release
//     (ReleaseLaneOccupancy → ReleaseAllOccupancy) for what is a ONE-lane take,
//     and the per-lane ReleaseOccupancyForLane sat one file away.
//  2. The `dwelling` guard meant to stop that rollback firing for a dweller was
//     STRUCTURALLY DEAD: the dwell's release binds the tail BEFORE appending
//     (bindDwellTail — plan first, column second, §R.5's crash ordering), so by
//     the time appendGateTail read the plan, the dwell wait already had an
//     actionable step after it and waitGatesAnAppend returned false for every
//     dweller that ever reached this line.
//
// Put together: a dweller whose tail append the fleet refused — one busy second
// at RDS — ran the INBOUND rollback, which dropped EVERY occupancy row the
// order held, including the source-lane row its own dispatch took while the
// robot stood inside that corridor holding a bin. The table then said the lane
// was empty; the next leg was admitted; two robots met nose to tail in a
// single-file corridor. The guard that existed to prevent exactly this had
// never once fired true.
//
// THE FIX, AND WHY THE GUARD WAS DELETED RATHER THAN REPAIRED: no plan-shape
// discriminator read inside appendGateTail can survive bindDwellTail, because
// the bind must precede the append for crash ordering and the bind is what
// destroys the shape the guard read. The row's own insert is the only witness
// that survives both writers: AcquireOccupancy now reports whether THIS call
// inserted, and the rollback gives back only what this call took, per-lane.
//
// MUTATION A (both tests' first arm): restore the order-wide release —
// `d.ReleaseLaneOccupancy(order.ID)` in place of the per-lane one. The dweller
// test's source-lane assertion fires (the row is gone while the robot stands
// inside) and the inbound test's other-lane assertion fires (a row the failed
// append never touched is dropped).
//
// MUTATION B (the dead guard, re-killed): invert the rollback condition to
// `!IsAppendLanded(err)` without the take report — i.e. release on every
// failure, dweller or not. The dweller test's source-lane assertion fires the
// same way, which is the exact sentence the dead guard was written to say and
// never could.

// TestAppendRollback_DwellerKeepsItsSourceLaneRow is the dweller half: the
// robot is INSIDE the dug lane holding a bin, the fleet refuses its tail, and
// the row that declares the corridor occupied must survive — because the
// failed append never took it. The release's own take, on the DESTINATION lane,
// is the one that must go.
//
// No park: the destination has to be a sibling-lane SLOT, which is what makes
// the take real. A direct group child contributes no row (dig_dwell.go's own
// rule), and with no row taken the rollback would have nothing to get wrong.
func TestAppendRollback_DwellerKeepsItsSourceLaneRow(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	_, dug, sib, _, dugSlots, _, bp := setupDwellGroup(t, db, "APPRB-DW", 2, false)
	createTestBinAtNode(t, db, bp.Code, dugSlots[0].ID, "APPRB-DW-BLK")
	createTestBinAtNode(t, db, bp.Code, dugSlots[1].ID, "APPRB-DW-TGT")

	demand := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "apprb-dw"
		o.OrderType = OrderTypeRetrieve
		o.PayloadCode = bp.Code
		o.DeliveryNode = lineNode(t, db, "APPRB-DW-LINE").Name
		o.Status = protocol.StatusPending
	})
	legs := planDigFor(t, db, d, bp, dug, dugSlots[1], demand)
	if legs[0].VendorOrderID == "" {
		t.Fatalf("the dig leg was never dispatched (queue_cause %q)", legs[0].QueueCause)
	}

	// The robot lifted its blocker and is dwelling at the shallowest slot. Its
	// dispatch-time occupancy row on the DUG lane is the premise: the robot is
	// inside this corridor, and the row is what says so.
	liftBin(t, db, d, legs[0], dugSlots[0], transitNode(t, db, "APPRB-DW-TRANSIT"))
	if !occupies(t, db, dug.ID, legs[0].ID) {
		t.Fatalf("premise failed: the dwelling leg holds no occupancy row on %s, so there is "+
			"nothing whose survival this test can pin", dug.Name)
	}

	// THE FLEET REFUSES THE TAIL. The dwell's release now chooses a destination
	// in the sibling lane, binds the tail into the plan, TAKES the sibling
	// lane's row, and calls the append — which fails. This is the one moment
	// both halves of the defect line up: the bind has already destroyed the
	// plan shape (dwelling=false under the old guard) and the robot still
	// stands in the dug lane.
	backend.SetFail(true)
	d.EvaluateWaitLaneForStagedOrder(legs[0].ID)

	// THE ASSERTION THE FIX IS FOR. The robot never got its tail, so it is
	// still standing in the dug lane holding the blocker, and the row that says
	// so must still be there. The failed append took the SIBLING lane's row —
	// not this one — and a rollback that reaches across lanes declares an
	// occupied corridor empty to the next entrant.
	if !occupies(t, db, dug.ID, legs[0].ID) {
		t.Errorf("lane %s no longer records leg %d inside it after a REFUSED tail append. The robot "+
			"is still standing in that corridor holding its blocker; the failed append's rollback "+
			"dropped a row it never took (the dispatch-time source-lane one), and the lane now reads "+
			"empty to the next leg — two robots nose to tail in a single-file lane. This is the "+
			"phantom-absence twin of §R.54's phantom row, from the same order-wide release",
			dug.Name, legs[0].ID)
	}

	// AND THE ROW IT DID TAKE IS GIVEN BACK. The release's own take was the
	// sibling lane's, the robot is not going there, and that row must not
	// survive the failure — the other half of "only what this call took": a
	// rollback that releases NOTHING (the over-correction this fix's inverse
	// would be) walls the sibling lane behind a robot that never entered it.
	if occupies(t, db, sib.ID, legs[0].ID) {
		t.Errorf("lane %s still records the leg inside it after a REFUSED tail append — the release "+
			"took that row moments before the fleet refused, and the robot is not going there. The "+
			"rollback must give back the taken row while keeping the pre-existing one, not choose "+
			"between releasing everything and nothing", sib.Name)
	}

	// AND THE RETRY IS STILL LIVE. The fleet comes back, the next firing
	// re-asks, and the leg must release for real — a robot parked forever at a
	// dwell with its cause cleared is the §R.93 bar unmet, not a rollback fix.
	backend.SetFail(false)
	released := releaseDwell(t, d, db, legs[0])
	if released.DeliveryNode == "" {
		t.Fatalf("the dwelling leg never released after the fleet recovered (cause %q). A rollback "+
			"that leaves the leg releasable is the whole point; one that strands it is just a "+
			"different corridor held", released.QueueCause)
	}
}

// TestAppendRollback_InboundFailureReleasesOnlyTheTakenLane is the inbound
// half: a gated order at a mark, whose append fails, must lose the row THIS
// append took on THIS lane — and nothing else. The other-lane row is the
// discriminator between the per-lane release and the order-wide one.
func TestAppendRollback_InboundFailureReleasesOnlyTheTakenLane(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	// The rig's orientation: pick out of an UNGATED lane (a row the robot's
	// dispatch already took), place into a GATED one (deferred to the append).
	wall, park, w, p, bp := clearLaneFixture(t, db, "APPRB-IN")
	createTestBinAtNode(t, db, bp.Code, p[0].ID, "APPRB-IN-BIN")

	// Contend the gated lane so the leg actually dwells at the mark rather than
	// being appended back to back with its create: an undispatched deeper store
	// parks the leg on Tier-2 ordering, the same fixture shape
	// TestGatedLeg_TakesNoOccupancyOnTheLaneItStandsOutsideOf uses.
	deeper := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.Status = StatusQueued
		o.DeliveryNode = w[2].Name
	})
	_ = deeper
	parent := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.Status = StatusReshuffling
	})
	leg := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.ParentOrderID = &parent.ID
		o.Sequence = 1
		o.Status = StatusPending
		o.SourceNode = p[0].Name
		o.DeliveryNode = w[0].Name
	})
	testutil.MustNoErr(t, d.AdvanceCompoundOrder(parent.ID), "dispatch the leg")
	sent, err := db.GetOrder(leg.ID)
	testutil.MustNoErr(t, err, "reload the leg")
	if !IsGateStaged(sent) {
		t.Fatalf("the leg is not gate-staged (status %q wait_index=%d) — it drove straight in, so "+
			"the deferred-append moment this test is about never happened", sent.Status, sent.WaitIndex)
	}

	// The ungated source lane holds the robot's dispatch-time row. This row is
	// NOT this append's to touch.
	if !occupies(t, db, park.ID, leg.ID) {
		t.Fatalf("premise failed: the leg holds no occupancy row on its ungated source lane %s",
			park.Name)
	}

	// Clear the contention so the evaluator will release, then refuse the
	// append: the take on the GATED lane happens, the fleet says no, and the
	// rollback fires with took=true.
	_, termErr := db.TerminalizeOrder(deeper.ID, protocol.StatusCancelled, "test: contention cleared")
	testutil.MustNoErr(t, termErr, "clear the deeper store")
	backend.SetFail(true)
	d.EvaluateLaneReleases(wall.ID)

	// THE ROW THIS APPEND TOOK IS GONE. The robot is still at the mark, outside
	// the gated lane, and that lane must not read occupied.
	if occupies(t, db, wall.ID, leg.ID) {
		t.Errorf("lane %s still records the leg inside it after a REFUSED entry append — the robot "+
			"never got its tail and stands at the mark, so the row this append took is a leftover "+
			"that walls every other entrant", wall.Name)
	}

	// AND THE OTHER LANE'S ROW STANDS. The robot IS in its ungated source lane;
	// the failed gated entry says nothing about that presence, and dropping it
	// is the order-wide release's signature.
	if !occupies(t, db, park.ID, leg.ID) {
		t.Errorf("the leg's source-lane row on %s was dropped by the failure of a DIFFERENT lane's "+
			"append. Presence is per-lane; a rollback that reaches across lanes declares a corridor "+
			"the robot is standing in empty — the order-wide release firing through the one door "+
			"every append funnels through", park.Name)
	}

	// AND THE RETRY IS STILL LIVE.
	backend.SetFail(false)
	d.EvaluateLaneReleases(wall.ID)
	fresh, err := db.GetOrder(leg.ID)
	testutil.MustNoErr(t, err, "reload after recovery")
	if IsGateStaged(fresh) {
		t.Fatalf("the gated leg never released after the fleet recovered (status %q cause %q) — "+
			"a rollback that strands the robot at the mark forever is congestion turned permanent",
			fresh.Status, fresh.QueueCause)
	}
}
