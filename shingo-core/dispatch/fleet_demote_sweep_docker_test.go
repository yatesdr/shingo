//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
)

// TestFleetRefusal_ADemotedOrderIsOutOfTheDestructiveSweep answers round 6's
// speculation, and answers it structurally rather than with a clock.
//
// The worry was that a long fleet outage could age a demoted burst into
// AbandonStuckOrders — paper-burning by another name, the exact thing §8
// forbids — unless the retry path touched updated_at on every attempt.
//
// ── THE TRACE ─────────────────────────────────────────────────────────────
//
// It cannot, and updated_at is not why. AbandonStuckOrders selects
// `status IN (StuckSweepStatusSQLList())` (engine/reconciliation_service.go,
// the candidate SELECT) and re-checks protocol.IsStuckSweepCandidate on the row
// it reloads. That predicate is {dispatched, staged}. A demoted order parks in
// `sourcing`, which is in neither — so the destructive sweep cannot see it at
// any age, and the retry's clock is irrelevant to it.
//
// AND IT SURVIVES THE RUNG MOVE. When the meaning migration moves this door's
// park to `queued`, the answer does not change: `queued` is deliberately outside
// IsStuckSweepCandidate and inside IsRuntimeStuckCandidate, and that status's own
// doc says why at length — "a queued order waiting on material must get the
// first and must not get the second: demand does not evaporate, so cancelling it
// would delete the ask while the need is still real."
//
// ── WHAT DOES HAPPEN, AND IT IS THE POINT ─────────────────────────────────
//
// A demoted order that keeps being refused writes NOTHING on its retries:
// MoveToSourcing self-skips sourcing→sourcing, and setQueueReason
// short-circuits an unchanged cause. So the staleness clock — MAX(order_history
// .created_at), since the retrying-order fix — stays at the demote's own row,
// and ListAnomalies raises the order after its bound. That is a REPORT, not a
// reap: a fleet outage lasting half an hour is exactly what somebody should be
// told about, and nothing cancels anything.
//
// This test is the pin. If a future status change puts the demoted population
// into the sweep, it fails here rather than on a plant during an outage.
func TestFleetRefusal_ADemotedOrderIsOutOfTheDestructiveSweep(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewFailingBackend())
	order, _ := armedOrderAwaitingFleet(t, db, d, "demote-sweep")

	srcNode, lineNode, _ := setupTestData(t, db)
	derr := func() error {
		_, err := d.DispatchDirect(order, srcNode, lineNode)
		return err
	}()
	if derr == nil {
		t.Fatal("the fleet refused the create; DispatchDirect must report it")
	}
	order, _ = db.GetOrder(order.ID)
	code, cause, params := FleetRefusalCause(derr, order.DeliveryNode)
	d.DemoteAfterFleetRefusal(order, code, cause, params)

	got, gerr := db.GetOrder(order.ID)
	testutil.MustNoErr(t, gerr, "re-read the order")
	if protocol.IsStuckSweepCandidate(got.Status) {
		t.Errorf("a fleet-demoted order rests in %q, which IS a stuck-sweep candidate.\n"+
			"AbandonStuckOrders cancels on a timer. A refusal burst aged into it during a long "+
			"outage is the paper-burning ruling 8 exists to forbid — the order kept its bin, its "+
			"paper and its place in the line precisely so it could be retried.", got.Status)
	}
	// And it IS watched: the anomaly board reports it, which is the half that
	// should happen. A demoted order nobody can see is the other failure.
	if !protocol.IsRuntimeStuckCandidate(got.Status) {
		t.Errorf("a fleet-demoted order rests in %q, which nothing flags as runtime-stuck.\n"+
			"An outage long enough to strand a burst has to reach somebody; a population that is "+
			"neither swept nor reported is the least observable state in the system.", got.Status)
	}
}
