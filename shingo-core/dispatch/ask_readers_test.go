package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingocore/store/orders"
)

// askRowStates are the rows the ask's Go readers are asked about:
//
//   - today: inside a create the row reads `dispatched` with no vendor id and
//     no ask (the hand's status write);
//   - pending and queued with an ask open: inside a create once the claim is
//     the column and the row keeps its status;
//   - queued with no ask: an order at rest, which every reader below answers
//     differently, so each assertion is about the ask and not about the status.
func askRowStates() (today, pendingAsking, queuedAsking, queuedAtRest *orders.Order) {
	today = &orders.Order{ID: 1, Status: StatusDispatched}
	pendingAsking = &orders.Order{ID: 1, Status: StatusPending, AskedVendorOrderID: "sg-1-ask"}
	queuedAsking = &orders.Order{ID: 1, Status: StatusQueued, AskedVendorOrderID: "sg-1-ask"}
	queuedAtRest = &orders.Order{ID: 1, Status: StatusQueued}
	return
}

// TestAskReaders_AnOpenAskAnswersAsTodaysCreateWindow: each Go reader that means
// "Core has claimed it" answers for a pending or queued row with an ask open
// exactly what it answers for today's in-create `dispatched` row, and answers a
// queued row at rest the other way. The readers: the redirect gate and
// entryStepIndex (PreDispatch), the scanner's and complex dispatch's gate
// (Acquiring), the pair's partner read (pairLegCommitted) and gate 3
// (swapLegCommittedToFleet).
func TestAskReaders_AnOpenAskAnswersAsTodaysCreateWindow(t *testing.T) {
	t.Parallel()
	today, pendingAsking, queuedAsking, atRest := askRowStates()
	// One pickup, one wait, then a dropoff: a resting order enters at the
	// pickup (0); one standing at its first wait enters after it (2).
	steps := []resolvedStep{{Action: protocol.ActionPickup}, {Action: protocol.ActionWait}, {Action: protocol.ActionDropoff}}
	entry := func(o *orders.Order) int {
		i, ok := entryStepIndex(o, steps)
		if !ok {
			return -1
		}
		return i
	}
	readers := []struct {
		name string
		read func(*orders.Order) any
	}{
		{"PreDispatch (the redirect gate)", func(o *orders.Order) any { return o.PreDispatch() }},
		{"Acquiring (the scanner, complex dispatch)", func(o *orders.Order) any { return o.Acquiring() }},
		{"AwaitingFleet", func(o *orders.Order) any { return o.AwaitingFleet() }},
		{"entryStepIndex", func(o *orders.Order) any { return entry(o) }},
		{"pairLegCommitted", func(o *orders.Order) any { return pairLegCommitted(o) }},
		{"swapLegCommittedToFleet", func(o *orders.Order) any { return swapLegCommittedToFleet(o) }},
	}
	for _, r := range readers {
		want := r.read(today)
		for _, row := range []*orders.Order{pendingAsking, queuedAsking} {
			if got := r.read(row); got != want {
				t.Errorf("%s: a %s row with an ask open reads %v, want %v (today's in-create dispatched row)",
					r.name, row.Status, got, want)
			}
		}
		if got := r.read(atRest); got == want {
			t.Errorf("%s: a queued row at rest reads %v, the same as an asked one: the pin would not see the ask",
				r.name, got)
		}
	}
}
