//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// lane_gate_mark_cleared_docker_test.go — a dwelling robot says why it waits.
//
// This file held the ROLLBACK rule's two tests (a mark cleared mid-dwell, and
// the next order into a now-unmarked lane). A lane is never without a mark now:
// the store refuses removing a group's last wait point while it has lanes
// (store/nodes TestLane_NeverWithoutAMark), so neither state can be produced
// and both tests are retired.

// TestGateDwell_CarriesItsCauseAndClearsItOnEntry closes the operator gap F-11
// exposed: a dwelling robot with nothing on its row to say why.
//
// Three orders sat `staged` for 77 minutes on the lane-stress rig with a blank
// queue_cause. The board showed three robots doing nothing and no sentence
// anywhere explaining it — and the answer was in the evaluator's own verdict on
// every pass, thrown away into a debug log.
//
// DESIGN §16 rule 7: occupancy is the first refusal a dweller can take here. The
// lane is marked, the order is staged with its wait at index 0, and another
// order holds the lane — so the verdict is lane-occupied and that is what has to
// reach the row.
//
// MUTATION (verified 2026-08-10): delete the setQueueReason on the refusal arm
// of EvaluateLaneReleases. The cause assertion fires with an empty string, which
// is the state the rig sat in.
func TestGateDwell_CarriesItsCauseAndClearsItOnEntry(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	srcNode, _, bp := setupTestData(t, db)
	_, laneID, mouth := gatedLane(t, db, "DWELLCAUSE", "DWELLCAUSE-WAIT")
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	blocker := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "dwellcause-blocker"
		o.Status = StatusDispatched
	})
	testutil.MustNoErr(t, d.TakeLaneOccupancy(blocker.ID, mouth), "somebody is inside the lane")

	bin := testdb.CreateBinAtNode(t, db, bp.Code, srcNode.ID, "DWELLCAUSE-BIN")
	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "dwellcause"
		o.OrderType = OrderTypeMove
		o.PayloadCode = bp.Code
		o.SourceNode = srcNode.Name
		o.DeliveryNode = mouth.Name
		o.Status = StatusPending
	})
	testutil.MustNoErr(t, db.UpdateOrderBinID(order.ID, bin.ID), "stamp the bin")
	orderRaw, err := db.GetOrder(order.ID)
	order = testutil.Must(t, orderRaw, err, "db.GetOrder(order.ID)")
	_, dErr := d.DispatchDirect(order, srcNode, mouth)
	testutil.MustNoErr(t, dErr, "the gated dispatch must create, unsealed")

	// The evaluator passes over a dweller it cannot admit.
	d.EvaluateLaneReleases(laneID)

	held, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "db.GetOrder(order.ID)")
	if held.QueueCause == "" {
		t.Fatalf("order %d is dwelling at the mark with NO cause on its row. An operator sees a "+
			"robot parked and nothing that says why — which is how three of these went 77 minutes "+
			"unexplained on the rig.", order.ID)
	}
	if QueueCause(held.QueueCause) != CauseLaneOccupied {
		t.Errorf("cause = %q, want %q — the sentence has to name what it is actually waiting for",
			held.QueueCause, CauseLaneOccupied)
	}

	// The lane clears; the dweller goes in; the cause must not survive it.
	d.ReleaseLaneOccupancy(blocker.ID) // the blocker leaves the lane
	d.EvaluateLaneReleases(laneID)

	entered, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "db.GetOrder(order.ID)")
	if entered.QueueCause != "" {
		t.Errorf("order %d entered the lane still carrying cause %q — a stale wait on a robot that "+
			"is now driving is the same lie as the blank was, told the other way round",
			order.ID, entered.QueueCause)
	}
}
