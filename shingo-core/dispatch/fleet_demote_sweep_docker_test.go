//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
)

// TestFleetRefusal_ADemotedOrderIsWatched: a fleet-demoted order rests where the
// anomaly board reports it, and nothing cancels it on a timer (Core's stuck sweep,
// which round 6 worried a long outage could age a demoted burst into, is gone).
//
// A demoted order that keeps being refused writes NOTHING on its retries:
// Park self-skips queued→queued, and setQueueReason short-circuits an unchanged
// cause. So the staleness clock — MAX(order_history.created_at), since the
// retrying-order fix — stays at the demote's own row, and ListAnomalies raises
// the order after its bound. That is a REPORT, not a reap: a fleet outage lasting
// half an hour is exactly what somebody should be told about.
func TestFleetRefusal_ADemotedOrderIsWatched(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewFailingBackend())
	order, _ := armedOrderAwaitingFleet(t, db, d, "demote-sweep")

	srcNode, lineNode, _ := testdb.SetupTestData(t, db)
	derr := func() error {
		_, err := d.DispatchDirect(order, srcNode, lineNode)
		return err
	}()
	if derr == nil {
		t.Fatal("the fleet refused the create; DispatchDirect must report it")
	}
	orderRaw, err := db.GetOrder(order.ID)
	order = testutil.Must(t, orderRaw, err, "db.GetOrder(order.ID)")
	code, cause, params := FleetRefusalCause(derr, order.DeliveryNode)
	d.DemoteAfterFleetRefusal(order, code, cause, params)

	got, gerr := db.GetOrder(order.ID)
	testutil.MustNoErr(t, gerr, "re-read the order")
	// It IS watched: the anomaly board reports it. A demoted order nobody can
	// see is the failure this pins.
	if !protocol.IsRuntimeStuckCandidate(got.Status) {
		t.Errorf("a fleet-demoted order rests in %q, which nothing flags as runtime-stuck.\n"+
			"An outage long enough to strand a burst has to reach somebody; a population that is "+
			"neither swept nor reported is the least observable state in the system.", got.Status)
	}
}
