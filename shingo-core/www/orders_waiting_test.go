package www

import (
	"testing"

	"shingo/protocol"
	"shingocore/domain"
)

// orders_waiting_test.go — WHICH ORDERS THE PAGE SAYS ARE WAITING.
//
// Two seams on the orders page ask this: waitSinceFor, which prints how long a
// wait has been standing, and countQueueCodes, which builds the summary chips.
// Both used to ask protocol.IsAcquiring, and both were wrong in the same
// direction — but only for the populations nobody was watching.

func waitingOrder(status protocol.Status, cause string) *domain.Order {
	return &domain.Order{Status: status, QueueCause: cause, QueueCode: "waiting_for_slot"}
}

// A STAGED ORDER IS A PARKED ROBOT, and it was the one the page said least
// about. PopGateStaged and PopStationWait are both `staged`, and eight causes can
// land ONLY on them — the staged dig-blocker refusals, the four gate causes,
// station-wait. IsAcquiring is {queued, sourcing}, so every one of those rendered
// with no duration and was absent from the summary chips entirely.
func TestOrderIsWaiting_IncludesStagedAndReshuffling(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		status protocol.Status
		why    string
	}{
		{protocol.StatusStaged, "a robot physically parked at a lane's mark, holding an unsealed " +
			"waybill only Core can append to. The most expensive wait in the system"},
		{protocol.StatusReshuffling, "a compound parent whose chapter has stopped with a leg still " +
			"open — a real order on a real board, and it rendered blank"},
	} {
		if !orderIsWaiting(waitingOrder(tc.status, "gate-pickup-elsewhere")) {
			t.Errorf("a %s order carrying a queue cause did not read as waiting: %s", tc.status, tc.why)
		}
	}
	if !orderIsWaiting(waitingOrder(protocol.StatusQueued, "ngrp-full")) {
		t.Error("a queued order carrying a cause stopped reading as waiting — the widening must " +
			"ADD populations, never trade one for another")
	}
	if !orderIsWaiting(waitingOrder(protocol.StatusSourcing, "ngrp-full")) {
		t.Error("a sourcing order carrying a cause stopped reading as waiting")
	}
}

// A CAUSE ON THE ROW IS NOT ENOUGH, and this is the trap in the obvious
// widening. queue_reason/code/cause are cleared ONLY on terminalize
// (store/orders.go) and on ResumeCompound. NOTHING clears them when a park ends
// well, so an order that waited for a slot and then dispatched carries that
// cause all the way to delivery. Selecting on the cause alone would print a wait
// clock beside a robot that is driving.
func TestOrderIsWaiting_ExcludesADispatchedOrderCarryingAStaleCause(t *testing.T) {
	t.Parallel()
	for _, s := range []protocol.Status{
		protocol.StatusDispatched, protocol.StatusInTransit, protocol.StatusDelivered,
		protocol.StatusConfirmed,
	} {
		if orderIsWaiting(waitingOrder(s, "ngrp-full")) {
			t.Errorf("a %s order read as WAITING because it still carries the cause it parked "+
				"under earlier. The columns are not cleared when a park ends successfully, so "+
				"the status has to be part of the question", s)
		}
	}
}

// AND A STATUS ALONE IS NOT ENOUGH EITHER. A staged order with no cause is a
// robot doing its job at a mark, not a wait — the seams must not invent one.
func TestOrderIsWaiting_ExcludesAnOrderWithNoCause(t *testing.T) {
	t.Parallel()
	for _, s := range []protocol.Status{
		protocol.StatusQueued, protocol.StatusSourcing,
		protocol.StatusStaged, protocol.StatusReshuffling,
	} {
		if orderIsWaiting(waitingOrder(s, "")) {
			t.Errorf("a %s order with NO queue cause read as waiting. There is nothing to show a "+
				"duration for and nothing to put in a chip", s)
		}
	}
}

// The summary chips SELECT on the cause and COUNT by the code — the fine value
// says a wait is live, the coarse one is what queueCodeLabels can render.
func TestCountQueueCodes_SelectsOnCauseCountsByCode(t *testing.T) {
	t.Parallel()
	staged := waitingOrder(protocol.StatusStaged, "gate-append-failed")
	queued := waitingOrder(protocol.StatusQueued, "ngrp-full")
	drivingWithStaleCause := waitingOrder(protocol.StatusInTransit, "ngrp-full")
	stagedNoCause := waitingOrder(protocol.StatusStaged, "")

	got := countQueueCodes([]*domain.Order{staged, queued, drivingWithStaleCause, stagedNoCause})
	if got["waiting_for_slot"] != 2 {
		t.Errorf("waiting_for_slot chip = %d, want 2 (the staged order and the queued one). "+
			"The in_transit row carries a stale cause and must not be counted; the staged row "+
			"with no cause is not a wait", got["waiting_for_slot"])
	}
}
