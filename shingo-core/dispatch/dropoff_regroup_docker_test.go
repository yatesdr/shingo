//go:build docker

package dispatch

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
)

// A FUNGIBLE DROPOFF RE-ASKS INSTEAD OF WAITING ON ONE SLOT — order 227.
//
// Gated sim 2026-08-31. A CLIP evac out of ALN_006 was bound to SMN_015, which
// held an unrelated STUD bin. SMN_013 and SMN_014, the same lane, were EMPTY the
// whole run. The order re-queued 4,520 times — 96% of every dropoff-occupied
// event in that run — and never re-asked. It was also the evac for ALN_006, so
// that cell kept a bin nobody was coming for and the supply leg (order 225) stood
// HOLDING there unable to place: one un-re-asked slot pinned two orders and a
// machine.
//
// The step already remembers where it came from; resolvedStep.Group says so and
// says why. The allocator uses it on a reservation conflict. The capacity block
// did not, which is the whole defect.
//
// RED before the fix: the step keeps Node=<the occupied slot>, and delivery_node
// with it.
func TestDropoffRegroup_OccupiedFungibleDropoffRevertsToItsGroup(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	_, _, w, _, _ := clearLaneFixture(t, db, "RG1")
	line := lineNode(t, db, "RG1-LINE")

	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: line.Name},
		{Action: protocol.ActionDropoff, Node: w[0].Name, Group: "SYN_MARKET"},
	}
	raw, mErr := json.Marshal(steps)
	testutil.MustNoErr(t, mErr, "marshal steps")

	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.SourceNode = line.Name
		o.DeliveryNode = w[0].Name
		o.StepsJSON = string(raw)
		o.Status = protocol.StatusPending
	})

	if !d.revertFungibleDropoffToGroup(order, steps) {
		t.Fatal("the dropoff carries a group and was not reverted — the order will sit on this one " +
			"slot forever, which is order 227 and 4,520 re-queues")
	}
	if steps[1].Node != "SYN_MARKET" {
		t.Errorf("step node = %q, want the group SYN_MARKET", steps[1].Node)
	}

	reloaded, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "reload")
	if reloaded.DeliveryNode != "SYN_MARKET" {
		t.Errorf("delivery_node = %q, want SYN_MARKET — the endpoint must follow the step, or the "+
			"next tick re-resolves against a node the row no longer names", reloaded.DeliveryNode)
	}
	var back []resolvedStep
	testutil.MustNoErr(t, json.Unmarshal([]byte(reloaded.StepsJSON), &back), "reparse persisted steps")
	if len(back) != 2 || back[1].Node != "SYN_MARKET" || back[1].Group != "SYN_MARKET" {
		t.Errorf("persisted steps = %+v, want the dropoff reverted to its group AND the group kept "+
			"(losing Group would make the next block un-re-askable)", back)
	}
}

// A CONCRETE DROPOFF IS LEFT ALONE, and this is the arm that keeps the fix from
// becoming Core overruling whoever authored the plan.
//
// A step with no Group was named deliberately — a loader home, a staging node, a
// line position. Re-picking it is not a recalculation, it is a different
// decision, and it is the rule redirectStoreOffDugLane already states for an
// operator's choice. Those keep the old behaviour: hold, and retry the one slot.
func TestDropoffRegroup_ConcreteDropoffIsNotReverted(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	_, _, w, _, _ := clearLaneFixture(t, db, "RG2")
	line := lineNode(t, db, "RG2-LINE")

	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: line.Name},
		{Action: protocol.ActionDropoff, Node: w[0].Name}, // no Group: author-named
	}
	raw, mErr := json.Marshal(steps)
	testutil.MustNoErr(t, mErr, "marshal steps")

	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.SourceNode = line.Name
		o.DeliveryNode = w[0].Name
		o.StepsJSON = string(raw)
		o.Status = protocol.StatusPending
	})

	if d.revertFungibleDropoffToGroup(order, steps) {
		t.Fatal("a dropoff with no group was reverted — Core re-picking an author-named destination " +
			"is a different decision from re-resolving a fungible one")
	}
	if steps[1].Node != w[0].Name {
		t.Errorf("step node = %q, want it untouched at %s", steps[1].Node, w[0].Name)
	}
}

// THE LIVE PATH, ELICITED — order 227's shape driven through DispatchPreparedComplex.
//
// The two tests above pin the ARMS of revertFungibleDropoffToGroup by calling it
// directly, and they are mutation-RED. Neither says the fix WORKS: the sim run
// that shipped it recorded `regroup fires: 0`, and the dropoff-occupied collapse
// it was credited with (4,690 → 8) is attributable to the PANEL-B drain removing
// the congestion, not to this arm. So the arms were pinned and the path was
// unproven, which under the reachable-population law is not a certification.
//
// This constructs the population the run did not produce: a fungible complex
// dropoff whose resolved slot is occupied while a sibling slot in the same group
// stands empty.
//
// Since C2b, acquiring makes every group choice again (asIntent), so the order
// is sent to the free sibling on the first tick and the occupied slot is never
// asked. It used to assert two ticks: park and revert on the first, land on the
// second.
//
// MUTATION: make asIntent return the plan as stored. The tick then asks the
// occupied slot and parks, and the test fires on delivery_node.
func TestDropoffRegroup_OccupiedSlotIsReChosenAtAcquiring(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	// A REAL RESOLVER, because this test is about the re-resolve and
	// newTestDispatcher wires resolver=nil. Under nil, resolveStepNode's NGRP arm
	// is gated off and the reverted step stays at the group name forever — which
	// would make this test pass its revert assertion and prove nothing about the
	// tick that has to land it.
	backend := testdb.NewSuccessBackend()
	emitter := &mockEmitter{}
	d := NewDispatcher(db, backend, emitter, "core", "shingo.dispatch", &DefaultResolver{DB: db})

	_, _, w, _, _ := clearLaneFixture(t, db, "RG3")
	line := lineNode(t, db, "RG3-LINE")

	// Order 227's floor: a bin standing on the resolved slot, and its siblings in
	// the same group EMPTY the whole time. Nothing claims the blocker — it is an
	// unrelated payload somebody else's plan put there, which is exactly why the
	// slot-RESERVATION arm never fires for this shape and the capacity block does.
	placeBin(t, db, w[0])
	placeBin(t, db, line) // the bin the pickup leg lifts off the line

	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: line.Name},
		{Action: protocol.ActionDropoff, Node: w[0].Name, Group: "RG3-GRP"},
	}
	raw, mErr := json.Marshal(steps)
	testutil.MustNoErr(t, mErr, "marshal steps")

	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.SourceNode = line.Name
		o.DeliveryNode = w[0].Name
		o.StepsJSON = string(raw)
		o.Status = protocol.StatusQueued
	})

	// ONE TICK, SINCE ACQUIRING MAKES THE CHOICE (asIntent). The plan's slot is an
	// earlier pass's choice; acquiring puts it back to the group and chooses
	// again, so the occupied slot is never asked and the free sibling is taken
	// on the first tick. It used to park once (dropoff-occupied), revert, and
	// land on the second tick.
	testutil.MustNoErr(t, d.DispatchPreparedComplex(order), "the one tick")

	after, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "reload after the tick")
	if after.DeliveryNode == w[0].Name {
		t.Fatalf("delivery_node = %q — acquiring kept the occupied slot an earlier pass chose, which is "+
			"order 227's spin", after.DeliveryNode)
	}
	if after.DeliveryNode == "RG3-GRP" {
		t.Fatalf("delivery_node is still the unresolved group — the choice landed nowhere, so the order " +
			"waits on a group that has free slots in it")
	}
	if protocol.IsAcquiring(after.Status) {
		t.Errorf("status = %q, want dispatched — a free sibling slot was available the whole time", after.Status)
	}
	if parks := dropoffOccupiedParks(t, db, order.ID); parks != 0 {
		t.Errorf("dropoff-occupied parks = %d, want 0 — the occupied slot is never asked", parks)
	}
}

// dropoffOccupiedParks counts how many times this order was recorded as parked
// on an occupied dropoff. The history row is the durable record; the queue_cause
// column only shows the LAST one, which cannot tell a single park from a spin.
func dropoffOccupiedParks(t *testing.T, db *store.DB, orderID int64) int {
	t.Helper()
	var n int
	err := db.DB.QueryRow(
		`SELECT COUNT(*) FROM order_history WHERE order_id = $1 AND code = $2`,
		orderID, string(protocol.QueueWaitingForSlot)).Scan(&n)
	testutil.MustNoErr(t, err, "count dropoff-occupied history rows")
	return n
}
