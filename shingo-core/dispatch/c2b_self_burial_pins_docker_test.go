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
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// c2b_self_burial_pins_docker_test.go — C2b A.5, Q1: a swap never drops a bin in
// front of one it is about to pick up in the same lane.
//
// The shape is C2a S.2's swap 270 (2-3 a run on base): the plan drops into a lane
// at step i and picks from a deeper slot of the SAME lane at step j > i. The
// store selector exempts the asker in its burial clause on purpose — a re-bind
// may aim behind a bin the order itself is coming for (store/nodes/lanes.go) — and
// it has no view of plan order, so nothing stops the drop walling the pick.
//
// Ruling (RULINGS-C2b-P0-2026-10-06.md §3 Q1, (b)): the fact is the plan's. One
// helper in `plan`, used by intake step resolution and by rebindGatedDropoff; the
// drop re-asks with that lane excluded (the existing widen); no lane left in the
// group parks under the existing no-slot cause.

// c2bSelfBurialLane is a group with one lane of two slots, the deeper one holding
// the empty the swap will fetch: the only free slot is the mouth, in front of it.
func c2bSelfBurialLane(t *testing.T, db *store.DB, prefix, mark string) (grp string, laneID int64, mouth, deep *nodes.Node) {
	t.Helper()
	_, laneID, mouth = gatedLane(t, db, prefix, mark)
	slots, err := db.ListLaneSlots(laneID)
	testutil.MustNoErr(t, err, "list lane slots")
	for _, s := range slots {
		if s.ID != mouth.ID {
			deep = s
		}
	}
	return prefix + "-GRP", laneID, mouth, deep
}

// selfBurial reports a drop step that lands in front of a later pickup of the same
// lane: same lane, shallower slot, pickup after the drop.
func selfBurial(t *testing.T, db *store.DB, steps []resolvedStep) []string {
	t.Helper()
	type at struct {
		lane  int64
		depth int
	}
	where := func(name string) (at, bool) {
		n, err := db.GetNodeByDotName(name)
		if err != nil || n == nil || n.ParentID == nil {
			return at{}, false
		}
		lane, err := db.LaneForNode(n.ID)
		if err != nil || lane == nil {
			return at{}, false
		}
		dep, err := db.GetSlotDepth(n.ID)
		if err != nil {
			return at{}, false
		}
		return at{lane.ID, dep}, true
	}
	var out []string
	for i, s := range steps {
		if s.Action != protocol.ActionDropoff {
			continue
		}
		di, ok := where(s.Node)
		if !ok {
			continue
		}
		for j := i + 1; j < len(steps); j++ {
			p := steps[j]
			if p.Action != protocol.ActionPickup {
				continue
			}
			pj, ok := where(p.Node)
			if ok && pj.lane == di.lane && pj.depth > di.depth {
				out = append(out, fmt.Sprintf("step %d drops at %s (depth %d) in front of step %d's pickup at %s "+
					"(depth %d), same lane", i, s.Node, di.depth, j, p.Node, pj.depth))
			}
		}
	}
	return out
}

// Q6 — THE SYNTHETIC LOCATION PARKS, IT DOES NOT FAIL. The commit seam refuses
// a create still naming a group as a SyntheticLocation, whose own sentence says
// the order "is parked rather than sent", and the plain path parks it under
// ngrp-resolve (FleetRefusalCause). The complex door terminal-failed it as
// fleet_failed at the base, for a create the fleet never saw.
//
// THIS DRIVES THE BACKSTOP, NOT A PRODUCTION ARM, and the fixture says how. The
// test dispatcher has no resolver (newTestDispatcher passes nil), and
// plan.StepNode resolves a group only when it has one, so the group's name rides
// intake and the dispatch pass untouched. Production always wires a resolver
// (engine_lifecycle.go), and with one the interior drop resolves or waits under
// ngrp-resolve before any create (TestC2bPin_AnInteriorGroupDropWaitsForAFreeChild,
// below). The seam stays the wire's last guard, and this pins what it does when
// reached.
func TestC2bPin_AComplexOrderSentToAGroupParksUnderNGRPResolve(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	grp, _, _, deep := c2bSelfBurialLane(t, db, "C2BSB1", "")
	press, press2 := lineNode(t, db, "C2BSB1-PRESS"), lineNode(t, db, "C2BSB1-PRESS2")
	prResident(t, db, press, sd.BinType.ID, sd.Payload.Code, "C2BSB1-FULL")
	testdb.CreateBinAtNode(t, db, "", deep.ID, "C2BSB1-EMPTY")

	d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID: "c2bsb1", PayloadCode: sd.Payload.Code, Quantity: 1, ProcessNode: press.Name,
		Steps: []protocol.ComplexOrderStep{
			prPick(press.Name), prDrop(grp),
			{Action: protocol.ActionPickup, Node: deep.Name, Empty: true}, prDrop(press2.Name),
		},
	})
	prScanPass(t, d, db, "c2bsb1")

	o := prReloadUUID(t, db, "c2bsb1")
	var red []string
	if o.Status == StatusFailed {
		red = append(red, fmt.Sprintf("order %d failed (%q) for a synthetic location the fleet never saw",
			o.ID, o.ErrorDetail))
	}
	if o.QueueCause != string(CauseNGRPResolve) {
		red = append(red, fmt.Sprintf("order %d is %s under %q, want waiting under %q", o.ID, o.Status,
			o.QueueCause, CauseNGRPResolve))
	}
	if len(red) != 0 {
		t.Fatalf("%v", red)
	}
	testdb.AssertNoHoldsAtRest(t, db, "parked on a synthetic location")
}

// TestC2bPin_AnInteriorGroupDropWaitsForAFreeChild is the releaser for an
// interior group drop, with the resolver production has. A swap's interior drop
// names a group with no free child: intake keeps the group's name and parks it
// (intake-resolve), each dispatch pass re-resolves every step still naming a
// group (plan.ReComplexSteps) and parks it under ngrp-resolve holding nothing,
// and the pass after a child frees binds the drop and sends the order. The same
// capacity wait a final group drop has; no create ever carries the group's name.
//
// MUTATION: make ReComplexSteps pass an NGRP step through unresolved; the order
// fails at the commit seam instead of dispatching on the second pass.
func TestC2bPin_AnInteriorGroupDropWaitsForAFreeChild(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcherWithResolver(t, db)
	grp, _, mouth, deep := c2bSelfBurialLane(t, db, "C2BIG", "")
	press, press2, cell := lineNode(t, db, "C2BIG-PRESS"), lineNode(t, db, "C2BIG-PRESS2"), lineNode(t, db, "C2BIG-CELL")
	prResident(t, db, press, sd.BinType.ID, sd.Payload.Code, "C2BIG-FULL")
	atMouth := testdb.CreateBinAtNode(t, db, sd.Payload.Code, mouth.ID, "C2BIG-M")
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, deep.ID, "C2BIG-D")
	testdb.CreateBinAtNode(t, db, "", cell.ID, "C2BIG-EMPTY")

	d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID: "c2big", PayloadCode: sd.Payload.Code, Quantity: 1, ProcessNode: press.Name,
		Steps: []protocol.ComplexOrderStep{
			prPick(press.Name), prDrop(grp),
			{Action: protocol.ActionPickup, Node: cell.Name, Empty: true}, prDrop(press2.Name),
		},
	})
	prScanPass(t, d, db, "c2big")
	o := prReloadUUID(t, db, "c2big")
	if o.Status != StatusQueued || o.QueueCause != string(CauseNGRPResolve) {
		t.Fatalf("with the group full the swap is %s under %q, want %s under %q", o.Status, o.QueueCause,
			StatusQueued, CauseNGRPResolve)
	}
	testdb.AssertNoHoldsAtRest(t, db, "waiting for a child of the group")

	// A child frees: the mouth's bin leaves.
	testutil.MustNoErr(t, db.MoveBinClearingStaging(atMouth.ID, press2.ID, false), "the mouth's bin leaves")
	prScanPass(t, d, db, "c2big")
	o = prReloadUUID(t, db, "c2big")
	if o.VendorOrderID == "" || protocol.IsAcquiring(o.Status) {
		t.Fatalf("the swap is %s under %q after a child freed, want it sent", o.Status, o.QueueCause)
	}
	var steps []resolvedStep
	testutil.MustNoErr(t, json.Unmarshal([]byte(o.StepsJSON), &steps), "read the plan")
	steps = withoutInboundLaneWaits(steps) // the gate's waits, in front of each lane entry
	if steps[1].Node != mouth.Name {
		t.Fatalf("the interior drop was sent to %q, want the freed child %s", steps[1].Node, mouth.Name)
	}
}

// AT THE GATE RE-BIND: the same shape on a marked lane. The swap waits at its
// station; the release puts it at the lane's wait, and the gate re-binds the drop
// with FindStoreSlotInLaneExcluding, whose burial clause exempts the asker: the
// mouth, in front of the empty the swap fetches next.
//
// It was red at the base: the gate released the swap into the lane with its drop
// at the mouth. Now the re-bind sets that lane aside (plan.OwnPickupLane): the
// widen, which declines an interior drop, so the swap waits at the mark.
func TestC2bPin_ASwapNeverDropsInFrontOfItsOwnPickup(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	sd := testdb.SetupStandardData(t, db)
	_, laneID, mouth, deep := c2bSelfBurialLane(t, db, "C2BSB2", "C2BSB2-WAIT")
	press, press2 := lineNode(t, db, "C2BSB2-PRESS"), lineNode(t, db, "C2BSB2-PRESS2")
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, deep.ID, "C2BSB2-EMPTY")

	plan := []resolvedStep{
		{Action: protocol.ActionWait, Node: press.Name, WaitKind: WaitKindStation},
		{Action: protocol.ActionPickup, Node: press.Name},
		{Action: protocol.ActionWait, Node: "C2BSB2-WAIT", WaitKind: WaitKindLane, WaitLane: laneID},
		{Action: protocol.ActionDropoff, Node: mouth.Name},
		{Action: protocol.ActionWait, Node: "C2BSB2-WAIT", WaitKind: WaitKindLane, WaitLane: laneID},
		{Action: protocol.ActionPickup, Node: deep.Name, Empty: true},
		{Action: protocol.ActionDropoff, Node: press2.Name},
	}
	planJSON, err := json.Marshal(plan)
	testutil.MustNoErr(t, err, "marshal the swap's plan")
	swap := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "c2bsb2-swap"
		o.StationID = "line-1"
		o.OrderType = OrderTypeComplex
		o.Coordinated = true
		o.PayloadCode = sd.Payload.Code
		o.SourceNode = press.Name
		o.DeliveryNode = press2.Name
		o.Status = protocol.StatusStaged
	})
	testutil.MustNoErr(t, db.UpdateOrderStepsJSON(swap.ID, string(planJSON)), "persist the plan")
	testutil.MustNoErr(t, db.UpdateOrderVendor(swap.ID, "C2BSB2-V1", "WAITING", "AMR-1"), "set the vendor order")

	atStation, err := db.GetOrder(swap.ID)
	testutil.MustNoErr(t, err, "reload the swap")
	d.HandleOrderRelease(d.syntheticEnvelope(atStation.StationID),
		&protocol.OrderRelease{OrderUUID: atStation.EdgeUUID})
	d.EvaluateLaneReleases(laneID)

	after, err := db.GetOrder(swap.ID)
	testutil.MustNoErr(t, err, "reload after the gate's evaluation")
	var steps []resolvedStep
	testutil.MustNoErr(t, json.Unmarshal([]byte(after.StepsJSON), &steps), "read the plan")
	var red []string
	if after.WaitIndex > 1 {
		red = selfBurial(t, db, steps)
	}
	if len(red) != 0 {
		t.Fatalf("%v", red)
	}
}

// TestC2bPin_AnIntakeDropNeverWallsItsOwnPickup is Q1 at intake, with the
// resolver production has. The swap's interior drop names a group; its only free
// child is the mouth of the lane whose deeper slot holds the empty the swap
// fetches next. Alone, the group has nowhere else: the swap waits under the
// capacity cause a full group has, holding nothing, and is never sent with its
// drop in front of its own pickup. With a second lane in the group, the drop goes
// there.
//
// MUTATION: make plan.OwnPickupLane answer 0; the first case dispatches with
// its drop at the mouth, and so does the gate pin above. (The second case may
// land in the other lane by the group's own ranking either way; it pins that the
// set-aside re-ask finds it.)
func TestC2bPin_AnIntakeDropNeverWallsItsOwnPickup(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		secondLane bool
	}{{"one lane: it waits", false}, {"two lanes: the other", true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testDB(t)
			sd := testdb.SetupStandardData(t, db)
			d, _ := newTestDispatcherWithResolver(t, db)
			prefix := "C2BQ1A"
			if tc.secondLane {
				prefix = "C2BQ1B"
			}
			grp, laneID, mouth, deep := c2bSelfBurialLane(t, db, prefix, "")
			var other *nodes.Node
			if tc.secondLane {
				g, err := db.GetNodeByDotName(grp)
				g = testutil.Must(t, g, err, "read the group")
				laneType, err := db.GetNodeTypeByCode(protocol.NodeClassLANE)
				testutil.MustNoErr(t, err, "LANE type")
				testdb.MarkGroup(t, db, g.ID)
				lane2 := &nodes.Node{Name: prefix + "-LANE2", IsSynthetic: true, Enabled: true, NodeTypeID: &laneType.ID, ParentID: &g.ID}
				testutil.MustNoErr(t, db.CreateNode(lane2), "create the second lane")
				d0, d1 := 0, 1
				other = &nodes.Node{Name: prefix + "-L2S0", Enabled: true, ParentID: &lane2.ID, Depth: &d0}
				testutil.MustNoErr(t, db.CreateNode(other), "slot")
				testutil.MustNoErr(t, db.CreateNode(&nodes.Node{Name: prefix + "-L2S1", Enabled: true, ParentID: &lane2.ID, Depth: &d1}), "slot")
			}
			press, press2 := lineNode(t, db, prefix+"-PRESS"), lineNode(t, db, prefix+"-PRESS2")
			prResident(t, db, press, sd.BinType.ID, sd.Payload.Code, prefix+"-FULL")
			testdb.CreateBinAtNode(t, db, "", deep.ID, prefix+"-EMPTY")

			uuid := strings.ToLower(prefix)
			d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
				OrderUUID: uuid, PayloadCode: sd.Payload.Code, Quantity: 1, ProcessNode: press.Name,
				Steps: []protocol.ComplexOrderStep{
					prPick(press.Name), prDrop(grp),
					{Action: protocol.ActionPickup, Node: deep.Name, Empty: true}, prDrop(press2.Name),
				},
			})
			prScanPass(t, d, db, uuid)
			o := prReloadUUID(t, db, uuid)
			var steps []resolvedStep
			testutil.MustNoErr(t, json.Unmarshal([]byte(o.StepsJSON), &steps), "read the plan")
			if red := selfBurial(t, db, steps); len(red) != 0 {
				t.Fatalf("the swap's plan walls its own pickup: %v (status %s)", red, o.Status)
			}
			if !tc.secondLane {
				if o.Status != StatusQueued || o.QueueCause != string(CauseNGRPResolve) {
					t.Fatalf("with lane %d its group's only lane the swap is %s under %q, want %s under %q",
						laneID, o.Status, o.QueueCause, StatusQueued, CauseNGRPResolve)
				}
				testdb.AssertNoHoldsAtRest(t, db, "the swap waits for somewhere that does not wall its pickup")
				return
			}
			steps = withoutInboundLaneWaits(steps) // the gate's waits, in front of each lane entry
			if o.VendorOrderID == "" || protocol.IsAcquiring(o.Status) || !strings.HasPrefix(steps[1].Node, prefix+"-L2S") {
				t.Fatalf("with a second lane the swap is %s with its drop at %q, want it sent into %s's lane "+
					"(not %s)", o.Status, steps[1].Node, other.Name, mouth.Name)
			}
		})
	}
}
