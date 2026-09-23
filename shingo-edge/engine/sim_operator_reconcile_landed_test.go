//go:build sim

package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/store/processes"
)

// reconcileScheduledAt runs one reconcile pass over a single delivered order
// tracked at FGN_001 and reports whether it scheduled a LOAD/CLEAR there.
// classify is the first thing a scheduled run does, so a stub that records its
// node and declines answers the question without running any action.
func reconcileScheduledAt(t *testing.T, delivery, source string) bool {
	t.Helper()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: 1, CoreNodeName: "FGN_001", Code: "FG1", Name: "FGN_001", Sequence: 1, Enabled: true,
	})
	testutil.MustNoErr(t, err, "create node")
	id, err := db.CreateOrder("reconcile-leg", orders.TypeMove, &nodeID, false, 1,
		delivery, "", source, "", true, "ASSY", "", "")
	testutil.MustNoErr(t, err, "create order")
	testutil.MustNoErr(t, db.UpdateOrderStatus(id, string(protocol.StatusDelivered)), "mark delivered")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel) // releases the confirm worker parked on the manual clock
	op := newTestSimOperator(clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))
	op.e = eng
	op.ctx = ctx
	op.confirming = make(map[int64]bool)
	var mu sync.Mutex
	scheduled := map[int64]bool{}
	done := make(chan struct{}, 1)
	op.classify = func(n int64) (time.Duration, string, func() error, bool) {
		mu.Lock()
		scheduled[n] = true
		mu.Unlock()
		done <- struct{}{}
		return 0, "", nil, false
	}

	op.reconcile()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
	}
	mu.Lock()
	defer mu.Unlock()
	return scheduled[nodeID]
}

// A delivered U2 whose delivery is NOT this node — the carrier leaving FGN_001
// for SYN_PRESS_EMPTIES — gets no CLEAR from the restart-safety sweep, the same
// answer onDelivered gives through deliveryLandedHere
// (TestSimOperator_OutboundDeliveryDoesNotScheduleAClear). Before the fix the
// sweep skipped that check and scheduled the spurious clear every ten seconds.
func TestSimOperator_ReconcileOutboundDeliveryDoesNotScheduleAClear(t *testing.T) {
	if reconcileScheduledAt(t, "SYN_PRESS_EMPTIES", "FGN_001") {
		t.Error("reconcile scheduled a CLEAR at FGN_001 for the U2 empty-out leg — the bin left " +
			"this node, and onDelivered refuses the same leg")
	}
}

// The inbound arrival is the delivery the clear exists for. It schedules before
// the fix and after it.
func TestSimOperator_ReconcileInboundDeliverySchedules(t *testing.T) {
	if !reconcileScheduledAt(t, "FGN_001", "SYN_MARKET") {
		t.Error("reconcile did not schedule a CLEAR for the U1 full-in leg delivered to FGN_001")
	}
}
