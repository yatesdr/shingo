//go:build docker

package dispatch

import (
	"errors"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// TestC2bPin_AReleaseAnotherPassMadeIsNotReportedAsThisCalls is F6's other half,
// the packed run's order 171 (2026-10-08). 171 was digging its own lane open; the
// chapter closed and the own-dig resume (releaseAfterOwnDig) appended its tail.
// The floor's evaluator reached it 3 ms later, found it no longer awaiting a tail
// ("another pass appended it"), and returned it as released anyway, so the floor
// recorded a lane_floor_release for a release it did not make. F6's rule
// (SweepLaneWaiters records only what its own call released) holds only if every
// release arm says "not mine" when the reload shows someone else already went.
//
// Each subtest holds the struct its pass read before the other release landed,
// which is the state the losing side of the race is in.
//
// MUTATION: return nil instead of ErrAppendNotOwed from any of the "no longer
// awaiting a tail" arms (appendGateTail, releaseGatedOrder, releaseGatedRetrieve,
// releaseDwellingDigLeg); its subtest reports the release as this call's.
func TestC2bPin_AReleaseAnotherPassMadeIsNotReportedAsThisCalls(t *testing.T) {
	t.Parallel()

	t.Run("a gated entrant", func(t *testing.T) {
		t.Parallel()
		d, db, waiter, laneID, digger := floorWaiterAtHeldLane(t, "C2B171A")
		stale := *waiter

		d.laneLock.Unlock(laneID, digger.ID)
		d.EvaluateLaneReleases(laneID) // the other pass: it appends the tail
		if after, err := db.GetOrder(waiter.ID); err != nil || after.WaitIndex < 1 {
			t.Fatalf("fixture: the other pass did not release the waiter (err %v)", err)
		}

		lane, err := db.GetNode(laneID)
		testutil.MustNoErr(t, err, "read the lane")
		c := candidateFor(t, db, &stale, gateEntryIndexFor(t, &stale))
		if in, _, failed := d.releaseAdmittedCandidate(c, lane); in || failed {
			t.Fatalf("release of an order another pass already appended: in=%v failed=%v, want neither — "+
				"this call sent nothing, and the floor records what its call reports as in", in, failed)
		}
		if err := d.appendGateTail(&stale, "C2B171A probe"); !errors.Is(err, ErrAppendNotOwed) {
			t.Fatalf("appendGateTail on an order whose tail is already appended returned %v, want "+
				"ErrAppendNotOwed (the arm 171's floor pass logged)", err)
		}
	})

	t.Run("a dwelling dig leg", func(t *testing.T) {
		t.Parallel()
		db := testDBShared(t)
		d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
		_, dug, _, _, dugSlots, _, bp := setupDwellGroup(t, db, "C2B171B", 2, true)
		createTestBinAtNode(t, db, bp.Code, dugSlots[0].ID, "C2B171B-BLK")
		createTestBinAtNode(t, db, bp.Code, dugSlots[1].ID, "C2B171B-TGT")
		demand := testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.EdgeUUID = "c2b171b"
			o.OrderType = OrderTypeRetrieve
			o.PayloadCode = bp.Code
			o.DeliveryNode = lineNode(t, db, "C2B171B-LINE").Name
			o.Status = protocol.StatusPending
		})
		legs := planDigFor(t, db, d, bp, dug, dugSlots[1], demand)
		before, err := db.GetOrder(legs[0].ID)
		testutil.MustNoErr(t, err, "read the dwelling leg")
		if !IsGateStaged(before) {
			t.Fatalf("fixture: the leg must be dwelling, awaiting its tail (wait_index=%d)", before.WaitIndex)
		}
		stale := *before

		arriveAtDwell(t, db, d, legs[0]) // the other pass: the arrival releases it
		if after, gErr := db.GetOrder(legs[0].ID); gErr != nil || IsGateStaged(after) {
			t.Fatalf("fixture: the arrival did not release the leg (err %v)", gErr)
		}

		c := gateCandidate{order: &stale, dwell: true}
		if freed, _ := d.releaseDweller(c, dug); freed {
			t.Fatal("releaseDweller reported freeing a leg the arrival had already released — the floor " +
				"would record that release as its own")
		}
	})
}
