//go:build docker

package dispatch

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// lane_gate_candidate_rekey_docker_test.go — the evaluator finds its work by the
// WAIT, not by an endpoint column.
//
// Two queries used to do candidate discovery: delivery_node against the lane's
// slot names for stores, source_node for retrieves. Both asked "is one END of
// this order in my lane". The question is "is this order parked at a wait that
// belongs to me", and only the second one survives a plan whose lane entry is in
// the middle.

// TestRekey_MidPlanLaneIsFound is the case the endpoint queries structurally
// missed, and the reason this task exists.
//
// The plan enters the lane in its MIDDLE: it picks at one line, drops into the
// lane, picks back out, and delivers to another line. extractEndpoints therefore
// puts two LINE nodes on the order's source_node/delivery_node columns, and
// neither endpoint query could ever match a slot of this lane. The order would
// dwell at its gate with no evaluator able to see it, until the abandon sweep
// cancelled a committed robot.
//
// Nothing the plain valve authored had this shape — its plans are three steps
// with the lane at one end — which is why it never bit. The splice produces it
// routinely, which is why this lands before the splice.
//
// MUTATION (verified): key gateStagedForLane on delivery_node/source_node
// against the lane's slot names, as before. This test's candidate set comes back
// empty.
func TestRekey_MidPlanLaneIsFound(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	_, laneID, _ := gatedLane(t, db, "REKEY-MID", "REKEY-MID"+"-WAIT")
	slots := laneSlots(t, db, laneID)
	lane := mustNode(t, db, laneID)
	from := lineNode(t, db, "REKEY-MID-FROM")
	to := lineNode(t, db, "REKEY-MID-TO")

	// [pickup@LINE-A, wait@gate(lane), dropoff@slot0, pickup@slot1, dropoff@LINE-B]
	plan := []resolvedStep{
		{Action: protocol.ActionPickup, Node: from.Name},
		{Action: protocol.ActionWait, Node: "REKEY-MID-GATE", WaitKind: WaitKindLane, WaitLane: laneID},
		{Action: protocol.ActionDropoff, Node: slots[0].Name},
		{Action: protocol.ActionPickup, Node: slots[1].Name},
		{Action: protocol.ActionDropoff, Node: to.Name},
	}
	planJSON, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "rekey-mid"
		o.StationID = "line-1"
		o.Coordinated = true
		// What extractEndpoints would write: FIRST and LAST actionable node.
		// Both are lines. Neither is in the lane. That is the whole point.
		o.SourceNode = from.Name
		o.DeliveryNode = to.Name
		o.Status = protocol.StatusStaged
	})
	if err := db.UpdateOrderStepsJSON(order.ID, string(planJSON)); err != nil {
		t.Fatalf("persist plan: %v", err)
	}
	if err := db.UpdateOrderVendor(order.ID, "REKEY-MID-V1", "CREATED", ""); err != nil {
		t.Fatalf("set vendor: %v", err)
	}

	// The endpoint queries' own answer, asserted so the premise is not assumed:
	// this order is invisible to a delivery_node match against the lane's slots.
	lanePrefix := lane.Name + "."
	var slotNames []string
	for _, s := range slots {
		slotNames = append(slotNames, s.Name, lanePrefix+s.Name)
	}
	byEndpoint, err := db.ActiveLaneStores(slotNames)
	if err != nil {
		t.Fatalf("ActiveLaneStores: %v", err)
	}
	for _, o := range byEndpoint {
		if o.ID == order.ID {
			t.Fatal("fixture: the endpoint query CAN see this order, so it does not exercise the " +
				"blind spot — the plan's lane entry must be interior")
		}
	}

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	cands, err := d.gateStagedForLane(lane)
	if err != nil {
		t.Fatalf("gateStagedForLane: %v", err)
	}
	if !containsOrder(cands, order.ID) {
		t.Fatal("an order parked at this lane's gate is not in its evaluator's candidate set. Its " +
			"lane entry is in the MIDDLE of its plan, so neither endpoint column names the lane — " +
			"nothing would ever release it and the robot dwells until the sweep")
	}
	for _, c := range cands {
		if c.order.ID != order.ID {
			continue
		}
		if c.retrieve {
			t.Error("direction read as retrieve; the first actionable step after the wait is a " +
				"DROPOFF, so this robot is going in to place")
		}
		if c.node == nil || c.node.ID != slots[0].ID {
			t.Errorf("lane-relevant node = %v, want %s — the classifier and the depth sort both "+
				"read it", c.node, slots[0].Name)
		}
	}
}

// TestRekey_RefusedCandidateStaysInTheSet is the property the unification made
// load-bearing.
//
// The evaluator now refuses a gate-staged retrieve while somebody else occupies
// the lane (skipsForGateStagedRetrieve died). A refusal must not remove the order
// from consideration: the candidate set is derived from the order's own durable
// state and never from a verdict, so the next firing re-derives the same set and
// re-asks. If a refusal could evict a candidate, the order would dwell forever
// with the lane free.
//
// MUTATION (verified): filter the candidate set by the classifier's verdict
// inside gateStagedForLane. The post-refusal assertion fires.
func TestRekey_RefusedCandidateStaysInTheSet(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	laneID, _, s1 := gateRetrieveLane(t, db, "REKEY-REF", "REKEY-REF-WAIT")
	lane := mustNode(t, db, laneID)
	line := lineNode(t, db, "REKEY-REF-LINE")
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	// A dig holds the lane so the retrieve is created unsealed and dwells.
	digger := testdb.CreateOrder(t, db)
	if !d.laneLock.TryLock(laneID, digger.ID) {
		t.Fatal("TryLock on a free lane must succeed")
	}
	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.OrderType = OrderTypeRetrieve
		o.SourceNode = s1.Name
		o.DeliveryNode = line.Name
		o.Status = "sourcing"
	})
	if _, err := d.DispatchDirect(order, s1, line); err != nil {
		t.Fatalf("DispatchDirect: %v", err)
	}
	d.laneLock.Unlock(laneID, digger.ID)

	// Now a PLAIN STORE is inside the lane. Since the unification this refuses
	// the staged retrieve, and it is the refusal that used to be impossible.
	inside := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "rekey-ref-inside" })
	if _, err := reservations.AcquireOccupancy(db.DB, inside.ID, laneID); err != nil {
		t.Fatalf("acquire occupancy: %v", err)
	}

	cands, err := d.gateStagedForLane(lane)
	if err != nil {
		t.Fatalf("gateStagedForLane: %v", err)
	}
	if !containsOrder(cands, order.ID) {
		t.Fatal("fixture: the staged retrieve is not a candidate at all")
	}

	// The classifier refuses it — the occupancy read that skipsForGateStagedRetrieve
	// used to skip.
	v, err := d.laneGateRetrieveCause(s1, order)
	if err != nil {
		t.Fatalf("laneGateRetrieveCause: %v", err)
	}
	if v.Admitted() || v.Cause() != CauseLaneOccupied {
		t.Fatalf("fixture: expected a lane-occupied refusal, got admitted=%v cause=%q",
			v.Admitted(), v.Cause())
	}

	// A full pass makes the refusal really happen, then the order must STILL be a
	// candidate.
	d.EvaluateLaneReleases(laneID)

	cands, err = d.gateStagedForLane(lane)
	if err != nil {
		t.Fatalf("gateStagedForLane after the refused pass: %v", err)
	}
	if !containsOrder(cands, order.ID) {
		t.Fatal("a refused order dropped out of the candidate set. Refused this pass is not the " +
			"same as gone: the condition clears on an event this evaluator is subscribed to, and " +
			"nothing else advances a lane wait")
	}

	// And when the lane clears it goes.
	if err := reservations.ReleaseAllOccupancy(db.DB, inside.ID); err != nil {
		t.Fatalf("release occupancy: %v", err)
	}
	v, err = d.laneGateRetrieveCause(s1, order)
	if err != nil {
		t.Fatalf("laneGateRetrieveCause after clear: %v", err)
	}
	if !v.Admitted() {
		t.Errorf("the retrieve is still refused (%q) with the lane empty", v.Cause())
	}
}
