//go:build docker

package dispatch

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// c2b_gate_pins_docker_test.go — C2b A.4: the lane gate serves its waiters on
// their own events, and a refusal says who it is waiting behind.
//
// Source: unification-C2a-2026-10-06/STUDY-floor-and-gate.md §2 (F2, F5); the
// run's order 484 (s1-after-660fd228-20m, 23:47:16.514-18.024): a gated store
// created at lane wait 0, refused at the create's valve under
// lane-deeper-pending, freed by the floor 1.5 s later with the row reading
// "(none)", and no blocker named anywhere to find the missed event by.

// c2bDeeperPending builds a marked lane with a deeper store still owed and a
// shallow store about to be dispatched into the same lane, so the shallow one is
// refused at the gate under lane-deeper-pending.
func c2bDeeperPending(t *testing.T, prefix string) (*Dispatcher, *orders.Order, *orders.Order, *nodes.Node, *nodes.Node) {
	t.Helper()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, laneID, s0 := gatedLane(t, db, prefix, prefix+"-WAIT")
	line := lineNode(t, db, prefix+"-LINE")
	slots, err := db.ListLaneSlots(laneID)
	testutil.MustNoErr(t, err, "list slots")
	var s1 *nodes.Node
	for _, s := range slots {
		if s.ID != s0.ID {
			s1 = s
		}
	}
	// The deeper store is waiting to be dispatched: a store bound to a deeper
	// slot of the lane, not yet sent, still blocks a shallower entrant
	// (TestLaneEntryTiers_ReleasesOnPlacement's last case).
	deep := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = strings.ToLower(prefix) + "-deep"
		o.SourceNode = line.Name
		o.DeliveryNode = s1.Name
		o.Status = "queued"
	})
	shallow := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = strings.ToLower(prefix) + "-shallow"
		o.StationID = "line-1"
		o.SourceNode = line.Name
		o.DeliveryNode = s0.Name
		o.Status = "queued"
	})
	return d, deep, shallow, line, s0
}

// F2 + F5 — a create the gate's valve refuses at lane wait 0 records that
// refusal on the row, naming the order it is waiting behind.
//
// It was red at the base: the valve asked gateEntryVerdict, logged "staged at
// <mark> … - awaiting release", and wrote nothing, so the row carried no cause,
// nothing named the deeper store, and the next evaluation of that lane was
// whichever event or floor tick came first. The valve now writes the refusal
// through the evaluator's own write (recordLaneWait).
func TestC2bPin_AGatedCreateTheValveRefusesSaysWhoItWaitsBehind(t *testing.T) {
	t.Parallel()
	d, deep, shallow, line, s0 := c2bDeeperPending(t, "C2BG1")

	if _, err := d.DispatchDirect(shallow, line, s0); err != nil {
		t.Fatalf("fixture: the shallow store's gated create failed: %v", err)
	}
	after, err := d.db.GetOrder(shallow.ID)
	testutil.MustNoErr(t, err, "reload the shallow store")
	if after.VendorOrderID == "" || !IsGateStaged(after) {
		t.Fatalf("fixture: the shallow store is %s (vendor %q) — the pin needs it created gated and held at the mark",
			after.Status, after.VendorOrderID)
	}
	var violations []string
	if after.QueueCause != string(CauseLaneDeeperPending) {
		violations = append(violations, fmt.Sprintf("F2: the valve's refusal left the row under %q, want %q",
			after.QueueCause, CauseLaneDeeperPending))
	}
	if !strings.Contains(after.QueueReason, fmt.Sprint(deep.ID)) {
		violations = append(violations, fmt.Sprintf("F5: the wait %q does not name the deeper store, order %d",
			after.QueueReason, deep.ID))
	}
	if len(violations) != 0 {
		t.Fatalf("%v", violations)
	}
}

// stagedAtMarkByLane — "who is standing outside" has one spelling.
//
// A DWELLING dig leg ([pickup, wait]: it lifts its blocker and stands in the
// lane while Core chooses where the blocker goes) is parked at its lane wait
// from dispatch (IsGateStaged reads the plan, not the robot), but until it lifts
// its robot is driving to the blocker INSIDE the lane. waitingAtLane leaves such
// a leg out (digLegNotYetLifted). A leg whose wait comes BEFORE its pickup
// stands at the mark, outside, and is counted, rightly.
// stagedAtMarkByLane, which exempts the orders standing at a group's marks from
// refusing a dig-mode acquire, does not ask: it counts the unlifted leg as
// standing outside, so a dig on a sibling lane is told the leg's robot is out of
// the way when it is not.
//
// It was red at the base; stagedAtMarkByLane now asks waitingAtLane.
func TestC2bPin_AnUnliftedDigLegIsNotStandingOutside(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	_, laneID, s0 := gatedLane(t, db, "C2BS", "C2BS-WAIT")
	sd := testdb.SetupStandardData(t, db)
	blocker := testdb.CreateBinAtNode(t, db, sd.Payload.Code, s0.ID, "C2BS-BLK")
	parent := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "c2bs-parent"; o.Status = StatusReshuffling })

	// The dwell plan: lift the blocker, then wait in the lane for Core to choose
	// where it goes (dig_dwell.go). Nothing follows the wait.
	plan := []resolvedStep{
		{Action: protocol.ActionPickup, Node: s0.Name},
		{Action: protocol.ActionWait, Node: s0.Name, WaitKind: WaitKindLane, WaitLane: laneID},
	}
	planJSON, err := json.Marshal(plan)
	testutil.MustNoErr(t, err, "marshal the leg's plan")
	leg := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "c2bs-leg"
		o.OrderType = OrderTypeMove
		o.ParentOrderID = &parent.ID
		o.BinID = &blocker.ID
		o.SourceNode = s0.Name
		o.Status = StatusInTransit
	})
	testutil.MustNoErr(t, db.UpdateOrderStepsJSON(leg.ID, string(planJSON)), "persist the leg's plan")
	testutil.MustNoErr(t, db.UpdateOrderVendor(leg.ID, "C2BS-V1", "RUNNING", "AMR-1"), "the leg's vendor order")

	fresh, err := db.GetOrder(leg.ID)
	testutil.MustNoErr(t, err, "reload the leg")
	if !IsGateStaged(fresh) {
		t.Fatal("fixture: the leg must read gate-staged at its lane wait")
	}
	var red []string
	if staged := stagedAtMarkByLane(db, laneID); staged[laneID][leg.ID] {
		red = append(red, fmt.Sprintf("dig leg %d, its blocker still at %s, is counted as standing outside lane %d",
			leg.ID, s0.Name, laneID))
	}
	if len(red) != 0 {
		t.Fatalf("%v", red)
	}
}
