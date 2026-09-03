//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// How far the moot arm can actually see a SELF-CONTAINED leg — one that lifts
// the line's bin and puts a fresh one back in the same trip, and so reads false
// to both legTakesLineBin and legPlacesLineBin. See the six-mode census in
// swap_leg_role.go for which builders produce that shape; there are more of them
// than single_robot.
//
// This file exists because the arm's exposure is easy to over-count from prose.
// "A single_robot consume swap pinned to a concrete empty source node skips
// terminally" is true and leaves out the half that makes it narrow.

// selfContainedSteps is the single_robot swap shape
// (shingo-edge/engine/material_orders.go:216-226) reduced to the two needs that
// decide the arm: the source fetch and the line pickup. The staging hops are
// relays of this order's own dropoffs and are never needs.
func selfContainedSteps(source, line, outDest string) []resolvedStep {
	return []resolvedStep{
		{Action: protocol.ActionPickup, Node: source},
		{Action: protocol.ActionWait, Node: line, WaitKind: WaitKindStation},
		{Action: protocol.ActionPickup, Node: line},
		{Action: protocol.ActionDropoff, Node: outDest},
		{Action: protocol.ActionDropoff, Node: line},
	}
}

// TestReserve_SelfContainedLegWithALineBinAlreadyParks pins the reachability
// condition on allocator.go:345.
//
// The arm fires on `len(assigned) == 0 && !anyMissWithBins`. A self-contained
// leg's line pickup is one of its needs — it precedes the line dropoff in every
// Edge builder, so complexPickups never marks it potentialRelay — so with a bin
// still on the line the leg RESERVES it, `len(assigned)` is 1, and the arm is
// never reached at all: the order falls to reserveHolding on the missing source,
// exactly as a two_robot supply leg does.
//
// The arm is therefore only reachable for this shape when the line is empty AND
// every other source is empty, which is a much smaller population than "a
// single_robot swap against a dry source" and is a state in which the skip is
// the right answer (the census says why).
//
// MUTATION: make the line node empty and this fails at the assigned == 1 check —
// which is the configuration the arm needs, and the one anyone reasoning about
// the arm has to hold in mind.
func TestReserve_SelfContainedLegWithALineBinAlreadyParks(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	line := lineNode(t, db, "SCL-LINE")
	source := lineNode(t, db, "SCL-SOURCE") // concrete, and DELIBERATELY EMPTY
	outDest := lineNode(t, db, "SCL-OUT")
	bp := mootPayload(t, db, "SCLP")
	createTestBinAtNode(t, db, bp.Code, line.ID, "BIN-SCL-LINE")

	steps := selfContainedSteps(source.Name, line.Name, outDest.Name)

	// Sanity: this is the self-contained shape — neither a pure evac nor a pure
	// filler. Both predicates read false, which is what puts it on the arm's side
	// of the switch in the first place.
	if legTakesLineBin(steps, line.Name) || legPlacesLineBin(steps, line.Name) {
		t.Fatal("fixture is not the self-contained shape — it is some other row of the census")
	}

	order := selfContainedOrderAt(t, db, line, source)
	order.PayloadCode = bp.Code

	assigned, outcome, err := d.allocator.reserveComplexPlan(order, &ComplexPlan{ResolvedSteps: steps})
	testutil.MustNoErr(t, err, "reserve")

	if len(assigned) != 1 {
		t.Fatalf("assigned = %d, want 1 (the line bin) — the moot arm is only reachable at zero, "+
			"so a bin on the line means this leg never gets near it", len(assigned))
	}
	if outcome != reserveHolding {
		t.Errorf("outcome = %v, want reserveHolding — the leg holds a partial set and waits for "+
			"its source to be stocked", outcome)
	}
}

func selfContainedOrderAt(t *testing.T, db *store.DB, process, source *nodes.Node) *orders.Order {
	t.Helper()
	return testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.OrderType = OrderTypeComplex
		o.Status = "sourcing"
		o.ProcessNode = process.Name
		o.SourceNode = source.Name
	})
}
