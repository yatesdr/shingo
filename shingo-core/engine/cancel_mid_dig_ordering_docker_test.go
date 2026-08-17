//go:build docker

package engine

import (
	"sync"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/eventbus"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
)

// cancel_mid_dig_ordering_docker_test.go — the ORDER of the two lines in
// HandleOrderCancel, which is the whole of the fix and which no test named.
//
// The end state is already pinned — parent cancelled, children cancelled, lane
// released, bins unclaimed (TestCompound_CancelParentWhileChildInFlight), plus
// the dispatch package's two cancel-cascade tests. Those are the same rows under
// either ordering, so none of them is ABOUT the ordering: they would all be
// satisfied by a teardown that reached the right place by the wrong route.
// What differs is what the world can SEE in between, and the world is
// watching: EventOrderCancelled is dispatched SYNCHRONOUSLY, in registration
// order, on the emitting goroutine, and one of its subscribers
// (wiring_lane_gate.go's TERMINAL arm → RedriveHeldCompoundLegs →
// AdvanceCompoundOrder) reaches straight back into the compound being torn down.
//
// Children-first showed that subscriber a parent still reading `reshuffling`
// with half its legs cancelled — a half-torn-down compound indistinguishable
// from a live one. It was survivable while the only reaction was to re-admit a
// leg. With the dissolve disposition it stopped being: the redrive admitted the
// next leg, hit a refusal, dissolved the dig, and the terminal arm raced the
// parent's own cancel to `failed`. An operator asked for cancelled and got
// failed.
//
// So the property is about an intermediate state, and the test has to observe
// one. It subscribes to the real bus and reads the parent's row at the instant
// each cancellation is announced.

// cancelObservation is one subscriber's-eye view of a cancellation: which order
// was announced, and what the parent looked like at that moment.
type cancelObservation struct {
	orderID      int64
	parentStatus protocol.Status
}

// TestCompound_OperatorCancelMidDig_ParentIsTornDownFirst is the ordering.
//
// The observer is registered on the REAL bus after the real wiring, so it sees
// the world the production subscribers have already reacted to — which is the
// stricter reading of "observers never see half-done" and the one that catches a
// subscriber that itself moved the parent.
//
// MUTATION (verified): in dispatcher.go HandleOrderCancel, swap the two teardown
// lines so cancelCompoundChildren runs before lifecycle.CancelOrder. Every
// assertion below fires, in the order the bug happens: the first cancellation
// announced is a LEG; all four legs are announced over a `reshuffling` parent;
// the redrive dissolves the dig ("reshuffle dissolved: the dig's plan went
// stale") and the terminal arm then fails the parent; and the operator's
// cancelled comes back `failed`.
//
// WHAT ELSE THE SWAP BREAKS, measured rather than assumed, because it decides
// what this test is FOR. Every cancel test in the dispatch package stays green —
// they have no bus, so the reaching-back subscriber does not exist for them.
// TestCompound_CancelParentWhileChildInFlight does go red, 5 runs out of 5, on
// its last assertion: `parent status = "failed", want cancelled`. So the end
// state is not blind to this after all — but what it catches is a DOWNSTREAM
// CONSEQUENCE, reached only because the redrive that ordering permits then meets
// an admission verdict that dissolves the dig. That is a fact about the lane and
// about the classifier, both of which are free to change; the ordering is not
// what it asserts, and "want cancelled, got failed" names no mechanism for
// whoever has to fix it. This test asserts the invariant directly, so it holds
// whatever the redrive goes on to decide.
func TestCompound_OperatorCancelMidDig_ParentIsTornDownFirst(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	// A four-deep lane, so the dig has several legs and the teardown has
	// something to be half-way through. Two would demonstrate the ordering; the
	// depth is what makes the window a real one rather than a single instant.
	sc := testdb.SetupCompound(t, db, testdb.CompoundConfig{
		Prefix:      "CANCELORD",
		NumSlots:    4,
		NumShuffles: 4,
		TargetSlot:  4,
		TargetAge:   2 * time.Hour,
	})

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "cancelord-dig",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  sc.Payload.Code,
		SourceNode:   sc.Grp.Name,
		DeliveryNode: sc.LineNode.Name,
		Quantity:     1,
	})

	parent := testdb.RequireOrderStatus(t, db, "cancelord-dig", dispatch.StatusReshuffling)
	children, err := db.ListChildOrders(parent.ID)
	if err != nil {
		t.Fatalf("list children: %v", err)
	}
	if len(children) < 2 {
		t.Fatalf("fixture: expected several legs under the dig, got %d — with one leg there is no "+
			"half-way for an observer to catch", len(children))
	}
	childIDs := map[int64]bool{}
	for _, c := range children {
		childIDs[c.ID] = true
	}

	// The first leg is out and its robot is moving: the operator is cancelling a
	// dig that is genuinely under way, not a plan on paper.
	first, err := db.GetOrder(children[0].ID)
	if err != nil {
		t.Fatalf("get first leg: %v", err)
	}
	if first.VendorOrderID == "" {
		t.Fatal("fixture: the first leg was never dispatched")
	}
	sim.DriveState(first.VendorOrderID, "RUNNING")

	// THE OBSERVER. Reads the parent's row from the database at announcement
	// time — not a cached struct — because the row is what every real subscriber
	// consults, and the guard the dissolve and the terminal arm both check is
	// "has the parent left `reshuffling`".
	var mu sync.Mutex
	var seen []cancelObservation
	var parentFailed bool
	eventbus.SubscribeTyped(eng.Events, func(evt eventbus.TypedEvent[EventType, OrderCancelledEvent]) {
		p, err := db.GetOrder(parent.ID)
		if err != nil {
			return
		}
		mu.Lock()
		seen = append(seen, cancelObservation{orderID: evt.Payload.OrderID, parentStatus: p.Status})
		mu.Unlock()
	}, EventOrderCancelled)
	eventbus.SubscribeTyped(eng.Events, func(evt eventbus.TypedEvent[EventType, OrderFailedEvent]) {
		if evt.Payload.OrderID == parent.ID {
			mu.Lock()
			parentFailed = true
			mu.Unlock()
		}
	}, EventOrderFailed)

	d.HandleOrderCancel(env, &protocol.OrderCancel{
		OrderUUID: "cancelord-dig",
		Reason:    "operator cancelled mid-dig",
	})

	mu.Lock()
	observed := append([]cancelObservation(nil), seen...)
	sawParentFailed := parentFailed
	mu.Unlock()

	if len(observed) == 0 {
		t.Fatal("no cancellation was announced at all — the teardown emitted nothing, so this test " +
			"is asserting over an empty list and would pass for the wrong reason")
	}

	// THE PARENT GOES FIRST. Not "the parent is cancelled by the end" — first,
	// before anything else in the family is announced, because the first thing an
	// observer can see has to be a parent that has left `reshuffling`.
	if observed[0].orderID != parent.ID {
		t.Errorf("the first cancellation announced was order %d (a leg), not the parent %d — a "+
			"subscriber woken by it finds the compound half torn down",
			observed[0].orderID, parent.ID)
	}

	// AND NOBODY EVER SEES HALF-DONE. Every leg's announcement must find the
	// parent already terminal; a leg announced over a `reshuffling` parent is
	// exactly the state RedriveHeldCompoundLegs re-admits into and the dissolve
	// dissolves.
	sawChild := false
	for i, obs := range observed {
		if !childIDs[obs.orderID] {
			continue
		}
		sawChild = true
		if obs.parentStatus != protocol.StatusCancelled {
			t.Errorf("announcement %d cancelled leg %d while the parent read %q — the teardown is "+
				"observable in progress, and the observers of this event reach back into the compound "+
				"(RedriveHeldCompoundLegs → AdvanceCompoundOrder) rather than merely watching it",
				i, obs.orderID, obs.parentStatus)
		}
	}
	if !sawChild {
		t.Error("no LEG cancellation was announced — every child was already terminal, so the " +
			"ordering this test exists for was never exercised")
	}

	// THE OPERATOR GETS WHAT THEY ASKED FOR. `failed` and `cancelled` are both
	// terminal and both release everything, so the end-state assertions in
	// TestCompound_CancelParentWhileChildInFlight cannot tell them apart — but one
	// of them raises an alarm against a dig nothing went wrong with.
	if sawParentFailed {
		t.Error("the parent was announced FAILED during a cancel — an operator asked for cancelled " +
			"and an observer of the teardown turned it into a fault report")
	}
	final, err := db.GetOrder(parent.ID)
	if err != nil {
		t.Fatalf("read the parent back: %v", err)
	}
	if final.Status != protocol.StatusCancelled {
		t.Errorf("parent finished %q, want %q", final.Status, protocol.StatusCancelled)
	}
}
