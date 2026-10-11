//go:build docker

package dispatch

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// c2c_group_choice_pins_docker_test.go — C2c (Lane C): the oracle chooses a
// drop across the whole node group at every release. Pins P1 and P2 of
// DESIGN-C2c §9, written before the source (2026-10-10).
//
// The rule (BRIEF-unification-C2c Lane C, owner's ruling of 2026-10-08 §2): the
// plan targets the group; the drop is chosen across the group by its configured
// store algorithm, hard rules first and no scoring; the gate is keyed by group;
// room is a group count; the stop is the group's waiting spot. The orc's
// rulings 1-7 bound it; D13 settles the forks P0 found (F1 no schema change,
// the paper is a pending slot row tagged reserved_by='groupRoom'; F2
// commitment wins over rank on a staged robot's paper).
//
// THE FIXTURE is one group G with two lanes, A and B, under the group's own
// waiting spot (testdb.MarkGroup: "<group>-WAIT"; no lane carries a legacy
// gate point, as on every rig fixture, DESIGN-C2c §4). A swap stands at that
// spot with its interior drop pencilled in lane A and stamped with G, the way
// intake writes a group drop. Each pin then runs ONE gate pass and reads where
// the drop went.
//
// THE GATE PASS IS THE BASE'S LANE-KEYED ENTRY (EvaluateLaneReleases on the
// lane the wait names). C2c re-keys it to the group (§6.3); c2cGatePass is the
// one place to convert, and every pin in the c2c_* files follows it.

// c2cGroup is G: two lanes of slots, [0] the mouth, deeper as the index grows.
type c2cGroup struct {
	grp          *nodes.Node
	spot         string
	laneA, laneB *nodes.Node
	a, b         []*nodes.Node
}

// c2cTwoLaneGroup builds G with lanes A (depthA slots) and B (depthB slots),
// marked by the group's waiting spot.
func c2cTwoLaneGroup(t *testing.T, db *store.DB, prefix string, depthA, depthB int) c2cGroup {
	t.Helper()
	ngrp, err := db.GetNodeTypeByCode(protocol.NodeClassNGRP)
	testutil.MustNoErr(t, err, "NGRP type")
	laneType, err := db.GetNodeTypeByCode(protocol.NodeClassLANE)
	testutil.MustNoErr(t, err, "LANE type")
	grp := &nodes.Node{Name: prefix + "-GRP", IsSynthetic: true, Enabled: true, NodeTypeID: &ngrp.ID}
	testutil.MustNoErr(t, db.CreateNode(grp), "create the group")
	testdb.MarkGroup(t, db, grp.ID)
	mk := func(name string, depth int) (*nodes.Node, []*nodes.Node) {
		lane := &nodes.Node{Name: name, IsSynthetic: true, Enabled: true, NodeTypeID: &laneType.ID, ParentID: &grp.ID}
		testutil.MustNoErr(t, db.CreateNode(lane), "create "+name)
		var slots []*nodes.Node
		for i := 0; i < depth; i++ {
			d := i
			s := &nodes.Node{Name: fmt.Sprintf("%s-S%d", name, i), Enabled: true, ParentID: &lane.ID, Depth: &d}
			testutil.MustNoErr(t, db.CreateNode(s), "create a slot of "+name)
			slots = append(slots, s)
		}
		return lane, slots
	}
	g := c2cGroup{grp: grp, spot: grp.Name + "-WAIT"}
	g.laneA, g.a = mk(prefix+"-LA", depthA)
	g.laneB, g.b = mk(prefix+"-LB", depthB)
	return g
}

// c2cSwapPlan is a coordinated swap: the press's full to a drop pencilled at
// pencil (stamped with G), then a carrier from fetchAt, then press2. The lane
// wait names lane A, as the base's splice writes it. A later step in lane A
// carries no wait of its own: the splice reads it as still inside.
func c2cSwapPlan(g c2cGroup, press, pencil, fetchAt, press2 string, fetchEmpty bool) []resolvedStep {
	return []resolvedStep{
		{Action: protocol.ActionWait, Node: press, WaitKind: WaitKindStation},
		{Action: protocol.ActionPickup, Node: press},
		{Action: protocol.ActionWait, Node: g.spot, WaitKind: WaitKindLane, WaitLane: g.laneA.ID},
		{Action: protocol.ActionDropoff, Node: pencil, Group: g.grp.Name},
		{Action: protocol.ActionPickup, Node: fetchAt, Empty: fetchEmpty},
		{Action: protocol.ActionDropoff, Node: press2},
	}
}

// c2cAtSpot stages the swap AT G's waiting spot: dispatched (a vendor order),
// staged, and indexed at the lane wait (wait index 1: the station wait is 0).
// It is the committed robot of the model ("dispatched and waiting at this
// lane's group's spot", D13 F5). priority ranks it by DemandRank.
func c2cAtSpot(t *testing.T, db *store.DB, uuid, payload string, priority int, steps []resolvedStep) *orders.Order {
	t.Helper()
	planJSON, err := json.Marshal(steps)
	testutil.MustNoErr(t, err, "marshal the plan of "+uuid)
	o := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = uuid
		o.StationID = "line-1"
		o.OrderType = OrderTypeComplex
		o.Coordinated = true
		o.PayloadCode = payload
		o.SourceNode = steps[1].Node
		o.DeliveryNode = steps[len(steps)-1].Node
		o.Status = protocol.StatusStaged
	})
	testutil.MustNoErr(t, db.UpdateOrderStepsJSON(o.ID, string(planJSON)), "persist the plan of "+uuid)
	testutil.MustNoErr(t, db.UpdateOrderVendor(o.ID, strings.ToUpper(uuid)+"-V1", "WAITING", "AMR-1"), "set the vendor order")
	testutil.MustNoErr(t, db.UpdateOrderWaitIndex(o.ID, 1), "stand it at the group's spot")
	if priority != 0 {
		_, err := db.Exec(`UPDATE orders SET priority = $1 WHERE id = $2`, priority, o.ID)
		testutil.MustNoErr(t, err, "rank "+uuid)
	}
	o, err = db.GetOrder(o.ID)
	testutil.MustNoErr(t, err, "reload "+uuid)
	if !IsGateStaged(o) {
		t.Fatalf("fixture: %s must be gate-staged at %s (wait index %d)", uuid, steps[2].Node, o.WaitIndex)
	}
	return o
}

// c2cPaper writes the plan's hold on its pencilled slot the way D13 F1 rules it:
// one PENDING slot row tagged reserved_by='groupRoom'. The column is free text,
// so the base accepts the row; at the base it is an ordinary pending slot row.
func c2cPaper(t *testing.T, db *store.DB, orderID int64, slot *nodes.Node) {
	t.Helper()
	testutil.MustNoErr(t, reservations.AcquireSlot(db.DB, orderID, slot.ID, "groupRoom"),
		"pencil the paper on "+slot.Name)
}

// c2cGatePass is one gate pass over G's waiters. See the file comment: the
// base's entry is per lane, and every waiter here waits on lane A.
func c2cGatePass(d *Dispatcher, g c2cGroup) {
	d.EvaluateLaneReleases(g.laneA.ID)
}

// c2cReload re-reads o and its plan.
func c2cReload(t *testing.T, db *store.DB, o *orders.Order) (*orders.Order, []resolvedStep) {
	t.Helper()
	after, err := db.GetOrder(o.ID)
	testutil.MustNoErr(t, err, "reload order")
	var steps []resolvedStep
	testutil.MustNoErr(t, json.Unmarshal([]byte(after.StepsJSON), &steps), "read the plan")
	return after, steps
}

// c2cInteriorDrop is the swap's first drop, wherever a wait was spliced around it.
func c2cInteriorDrop(steps []resolvedStep) (int, resolvedStep) {
	for i, s := range steps {
		if s.Action == protocol.ActionDropoff {
			return i, s
		}
	}
	return -1, resolvedStep{}
}

// c2cLaneOf is the lane a slot stands in, or 0.
func c2cLaneOf(t *testing.T, db *store.DB, name string) int64 {
	t.Helper()
	n, err := db.GetNodeByDotName(name)
	if err != nil || n == nil {
		return 0
	}
	lane, err := db.LaneForNode(n.ID)
	if err != nil || lane == nil {
		return 0
	}
	return lane.ID
}

// c2cSlotRowsIn counts the order's live slot rows on slots of one lane.
func c2cSlotRowsIn(t *testing.T, db *store.DB, orderID, laneID int64) int {
	t.Helper()
	var n int
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT COUNT(*) FROM reservations r JOIN nodes n ON n.id = r.node_id
		WHERE r.order_id = $1 AND r.resource_kind = 'slot' AND r.state <> 'released' AND n.parent_id = $2`,
		orderID, laneID).Scan(&n), "count slot rows in a lane")
	return n
}

// TestC2cPin_OrderTwoSixtySixsShapePassesAtTheGroupsSpot is P1 (a), order 266's
// shape (packed, 297290ba..b6880b5d): the drop is pencilled at A's mouth, in
// front of the empty the same swap fetches next from A1. Ruling 1's hard rule
// "never bury the order's own later pickup" sets A aside before the group's
// algorithm runs, so the drop lands in B; the later pickup in A keeps a lane
// wait of its own (ruling 6: a re-aimed mid-trip drop still gates its later
// lane entries); the delivery stays the press.
//
// AT THE BASE: GREEN, by the widen arm (widenDropoffToGroup from
// rebindGatedDropoff's self-wall branch) — it is the C2b pin
// TestC2bPin_AMidTripDropThatWouldWallItsOwnPickupIsWidened re-built on the
// group's spot. The arm's deletion is P1 (b), TestC2cPin_OneChooserNoWidenArm,
// which is the red half: this one has to stay green when the arm goes.
func TestC2cPin_OrderTwoSixtySixsShapePassesAtTheGroupsSpot(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcherWithResolver(t, db)
	g := c2cTwoLaneGroup(t, db, "C2CP1A", 2, 2)
	press, press2 := lineNode(t, db, "C2CP1A-PRESS"), lineNode(t, db, "C2CP1A-PRESS2")
	testdb.CreateBinAtNode(t, db, "", g.a[1].ID, "C2CP1A-EMPTY")
	swap := c2cAtSpot(t, db, "c2cp1a-swap", sd.Payload.Code, 0,
		c2cSwapPlan(g, press.Name, g.a[0].Name, g.a[1].Name, press2.Name, true))
	c2cPaper(t, db, swap.ID, g.a[0])

	c2cGatePass(d, g)

	after, steps := c2cReload(t, db, swap)
	_, drop := c2cInteriorDrop(steps)
	if lane := c2cLaneOf(t, db, drop.Node); lane != g.laneB.ID {
		t.Fatalf("the interior drop is at %q (lane %d), want lane B (%d): A is walled by the swap's own "+
			"later pickup at %s, and B has room (order 266 stood at the mark for the rest of a run)",
			drop.Node, lane, g.laneB.ID, g.a[1].Name)
	}
	if after.WaitIndex < 2 {
		t.Fatalf("the swap is still at G's spot (wait index %d, cause %q)", after.WaitIndex, after.QueueCause)
	}
	for i, s := range steps {
		if s.Action == protocol.ActionPickup && s.Node == g.a[1].Name {
			if i == 0 || steps[i-1].Action != protocol.ActionWait || steps[i-1].WaitKind != WaitKindLane ||
				steps[i-1].WaitLane != g.laneA.ID {
				t.Errorf("the later pickup in lane A (step %d) has no lane-A wait in front of it: the robot "+
					"would drive into A ungated (ruling 6)", i)
			}
		}
	}
	if after.DeliveryNode != press2.Name {
		t.Errorf("delivery_node = %q, want the trip's end %s: a mid-trip choice moves the drop, not the delivery",
			after.DeliveryNode, press2.Name)
	}
	if red := selfBurial(t, db, steps); len(red) != 0 {
		t.Errorf("%v", red)
	}
	if err := d.assertEachWaitGatesItsEntry(steps); err != nil {
		t.Errorf("the released plan no longer holds: %v", err)
	}
}

// TestC2cPin_TheReleaseChoosesAcrossTheGroupNotInsideThePencilledLane is P1's
// plan-time twin. Nothing is wrong with lane A: its deepest slot A1 is free,
// reachable and walls nothing, and the plan pencilled it. But B holds a bin of
// the same payload, and the group's algorithm is like-kind (LKND, the default;
// candidateBetter: hasMatch first), so asked across G it answers B's mouth.
// Ruling 1 ("one spelling ... called at acquiring and at release") and the
// rule ("the oracle chooses the drop across the whole node group at every
// release"): the release answers what the group answers, and the pencil is
// paper, not a destination (ruling 2; D13 F1).
//
// RED AT BASE, by reading: rebindGatedDropoff asks FindStoreSlotInLaneExcluding
// on lane A only, gets A1 (the slot it already holds), and releases into A with
// no writes. B is never asked unless A has nowhere.
func TestC2cPin_TheReleaseChoosesAcrossTheGroupNotInsideThePencilledLane(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcherWithResolver(t, db)
	g := c2cTwoLaneGroup(t, db, "C2CP1T", 2, 2)
	press, press2 := lineNode(t, db, "C2CP1T-PRESS"), lineNode(t, db, "C2CP1T-PRESS2")
	elsewhere := lineNode(t, db, "C2CP1T-EMPTYSRC")
	testdb.CreateBinAtNode(t, db, "", elsewhere.ID, "C2CP1T-EMPTY")
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, g.b[1].ID, "C2CP1T-SAMEKIND")
	swap := c2cAtSpot(t, db, "c2cp1t-swap", sd.Payload.Code, 0,
		c2cSwapPlan(g, press.Name, g.a[1].Name, elsewhere.Name, press2.Name, true))
	c2cPaper(t, db, swap.ID, g.a[1])

	c2cGatePass(d, g)

	after, steps := c2cReload(t, db, swap)
	_, drop := c2cInteriorDrop(steps)
	if drop.Node != g.b[0].Name {
		t.Fatalf("the interior drop is at %q, want %s: the group's like-kind answer is B (it holds the "+
			"payload), and the release must ask the group, not re-bind inside the pencilled lane",
			drop.Node, g.b[0].Name)
	}
	if after.WaitIndex < 2 {
		t.Errorf("the swap is still at G's spot (wait index %d, cause %q)", after.WaitIndex, after.QueueCause)
	}
	if n := c2cSlotRowsIn(t, db, swap.ID, g.laneA.ID); n != 0 {
		t.Errorf("the swap still holds %d slot row(s) in lane A after its drop was chosen in B: the paper "+
			"moves with the choice (ruling 3: armor on the chosen slot)", n)
	}
}

// TestC2cPin_ADropPlannedOnAFullLaneLandsInASiblingWithNoWait is P2. The drop
// is pencilled at A1; before the release, A's only free slot is walled by an
// UNCLAIMED bin set down at A's mouth, so a dig would help. B is empty.
// Ruling 1's hard rule "reachable" makes A1 no candidate at all, so the group's
// algorithm answers B, and ruling 3 ("a refused mouth is the next candidate,
// not a wait, while the group has one"): released into B on this pass, with no
// queue cause, the wait index advanced, and no dig summoned. Heal first holds
// only for a group with no candidate (D13 F3, DESIGN-C2c §7.3.4).
//
// RED AT BASE, by reading: rebindGatedDropoff finds nothing in A,
// acceptanceDigNeeded answers "a dig would help" (A0's bin is unclaimed, A1 is
// empty, nobody is inside A), the release refuses under
// gate-rebind-unavailable, and the pass summons the swap's own dig out of A.
// It is the inverse of TestWiden_ADigThatWouldHelpIsPreferredToWidening, which
// D13 F3 converts with this source.
func TestC2cPin_ADropPlannedOnAFullLaneLandsInASiblingWithNoWait(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcherWithResolver(t, db)
	g := c2cTwoLaneGroup(t, db, "C2CP2", 2, 2)
	press, press2 := lineNode(t, db, "C2CP2-PRESS"), lineNode(t, db, "C2CP2-PRESS2")
	elsewhere := lineNode(t, db, "C2CP2-EMPTYSRC")
	testdb.CreateBinAtNode(t, db, "", elsewhere.ID, "C2CP2-EMPTY")
	swap := c2cAtSpot(t, db, "c2cp2-swap", sd.Payload.Code, 0,
		c2cSwapPlan(g, press.Name, g.a[1].Name, elsewhere.Name, press2.Name, true))
	c2cPaper(t, db, swap.ID, g.a[1])
	wall := testdb.CreateBinAtNode(t, db, sd.Payload.Code, g.a[0].ID, "C2CP2-WALL")
	if wall.ClaimedBy != nil {
		t.Fatal("fixture: the wall must be UNCLAIMED, or no dig would help and this is P3's arm")
	}

	c2cGatePass(d, g)

	after, steps := c2cReload(t, db, swap)
	_, drop := c2cInteriorDrop(steps)
	var red []string
	if lane := c2cLaneOf(t, db, drop.Node); lane != g.laneB.ID {
		red = append(red, fmt.Sprintf("the interior drop is at %q (lane %d), want lane B (%d): A's only free "+
			"slot is unreachable, so it is not a candidate, and B has room", drop.Node, lane, g.laneB.ID))
	}
	if after.WaitIndex < 2 {
		red = append(red, fmt.Sprintf("the swap is still at G's spot (wait index %d)", after.WaitIndex))
	}
	if after.QueueCause != "" {
		red = append(red, fmt.Sprintf("the swap carries queue cause %q; a drop the group can take never waits",
			after.QueueCause))
	}
	if dig := digParentFor(t, db, g.a[0].Name); dig != nil {
		red = append(red, fmt.Sprintf("a dig (order %d) was summoned out of lane A for a drop its group "+
			"could take elsewhere", dig.ID))
	}
	if n := c2cSlotRowsIn(t, db, swap.ID, g.laneA.ID); n != 0 {
		red = append(red, fmt.Sprintf("the swap still holds %d slot row(s) in lane A", n))
	}
	if len(red) != 0 {
		t.Fatalf("P2: %v", red)
	}
}

// ── RULINGS-C2c-run-through §3.2 (2026-10-10 evening): a plain store into a
// group is a group drop; a flat child of a laned group is a candidate ──────

// c2cPlainStore is a station's plain move of the full at press into dest —
// with no origin class given, a demand-side order, so Core, not a person,
// chooses its slot — holding its bin softly, as the scanner leaves it before
// the hand.
func c2cPlainStore(t *testing.T, db *store.DB, uuid, payload string, press, dest *nodes.Node, bin int64,
	originClass string) *orders.Order {
	t.Helper()
	o := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = uuid
		o.StationID = "line-1"
		o.OrderType = OrderTypeMove
		o.SourceIntent = SourceIntentLocal
		o.PayloadCode = payload
		o.SourceNode = press.Name
		o.DeliveryNode = dest.Name
		o.Status = protocol.StatusPending
		o.OriginClass = originClass
	})
	testdb.ReserveBin(t, db, o.ID, bin)
	testutil.MustNoErr(t, db.UpdateOrderBinID(o.ID, bin), "stamp the held bin")
	o, err := db.GetOrder(o.ID)
	testutil.MustNoErr(t, err, "reload "+uuid)
	return o
}

// c2cSlotRow is the order's slot row on slot: its state and its tag ("" for none).
func c2cSlotRow(t *testing.T, db *store.DB, orderID, slotID int64) (state, tag string) {
	t.Helper()
	err := db.DB.QueryRow(`SELECT state, COALESCE(reserved_by, '') FROM reservations
		WHERE order_id = $1 AND node_id = $2 AND resource_kind = 'slot'`, orderID, slotID).Scan(&state, &tag)
	if err != nil {
		return "", ""
	}
	return state, tag
}

// c2cOccupy puts another order's robot inside lane; it returns that order.
func c2cOccupy(t *testing.T, db *store.DB, uuid string, lane, away *nodes.Node) *orders.Order {
	t.Helper()
	occupant := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = uuid
		o.DeliveryNode = away.Name
		o.Status = "in_transit"
	})
	if ok, err := reservations.AcquireOccupancy(db.DB, occupant.ID, lane.ID); err != nil || !ok {
		t.Fatalf("fixture: put a robot inside %s: ok=%v err=%v", lane.Name, ok, err)
	}
	return occupant
}

// c2cLastDrop is the plan's last dropoff.
func c2cLastDrop(steps []resolvedStep) (int, resolvedStep) {
	for i := len(steps) - 1; i >= 0; i-- {
		if steps[i].Action == protocol.ActionDropoff {
			return i, steps[i]
		}
	}
	return -1, resolvedStep{}
}

// TestC2cPin_APlainStoreIntoAGroupIsAGroupDrop is the owner's ruling (19:57,
// "why wouldnt they all pick across a group?") as the orc wrote it: a plain
// store into a group is a group drop. Its plan keeps the group it chose from,
// its slot is paper until the release (the hand does not confirm it), and its
// release is the group's pass, like a complex drop's.
//
// The store is aimed at G. Like-kind puts it in A (A holds the payload), so
// acquiring settles A's mouth. Then both lanes hold another robot, and the
// store reaches G's spot and waits. B's robot leaves; the group's pass releases
// the store into B, though A — the lane it was pencilled in — is still taken.
//
// RED AT BASE (56a702c6): the plain plan carries no group (D25 F6, "plain
// stores stay in their lane"): its slot row is an ordinary reserveSlot row,
// the hand confirms it at dispatch, and the release asks lane A only.
func TestC2cPin_APlainStoreIntoAGroupIsAGroupDrop(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcherWithResolver(t, db)
	g := c2cTwoLaneGroup(t, db, "C2CPS", 2, 2)
	press, away := lineNode(t, db, "C2CPS-PRESS"), lineNode(t, db, "C2CPS-AWAY")
	full := testdb.CreateBinAtNode(t, db, sd.Payload.Code, press.ID, "C2CPS-FULL")
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, g.a[1].ID, "C2CPS-SAMEKIND")
	order := c2cPlainStore(t, db, "c2cps-store", sd.Payload.Code, press, g.grp, full.ID, "")

	var red []string
	dest := d.ReserveStorageDropoff(order)
	if dest.Refused() || dest.Node.ID != g.a[0].ID {
		t.Fatalf("fixture: acquiring must settle A's mouth %s (like-kind), got %v (%v)",
			g.a[0].Name, dest.Node, dest.Err)
	}
	if _, tag := c2cSlotRow(t, db, order.ID, g.a[0].ID); tag != reservations.GroupRoomTag {
		red = append(red, fmt.Sprintf("the store's row on %s is tagged %q, want %q: the plain path keeps the "+
			"group it chose from, and its hold is the group's paper (ruling 2)",
			g.a[0].Name, tag, reservations.GroupRoomTag))
	}

	c2cOccupy(t, db, "c2cps-in-a", g.laneA, away)
	inB := c2cOccupy(t, db, "c2cps-in-b", g.laneB, away)
	order, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "reload the store")
	_, err = d.DispatchHand(order, full.ID, press, dest.Node)
	testutil.MustNoErr(t, err, "dispatch the store")
	waiting, steps := c2cReload(t, db, order)
	if !IsGateStaged(waiting) {
		t.Fatalf("fixture: with both lanes taken the store must wait at G's spot, got %s wait index %d cause %q",
			waiting.Status, waiting.WaitIndex, waiting.QueueCause)
	}
	if _, drop := c2cLastDrop(steps); drop.Group != g.grp.Name {
		red = append(red, fmt.Sprintf("the store's drop carries group %q, want %q: the plan keeps the group it "+
			"chose from, so the release asks it", drop.Group, g.grp.Name))
	}
	if state, _ := c2cSlotRow(t, db, order.ID, g.a[0].ID); state != string(reservations.StatePending) {
		red = append(red, fmt.Sprintf("the store's row on %s is %q at the spot, want pending: a group drop's "+
			"slot stays paper until its release (rulings 2 and 3)", g.a[0].Name, state))
	}
	if n, nErr := db.GetNode(g.a[0].ID); nErr == nil && n.ClaimedBy != nil {
		red = append(red, fmt.Sprintf("%s is claimed by order %d while the store waits at the spot",
			g.a[0].Name, *n.ClaimedBy))
	}

	testutil.MustNoErr(t, reservations.ReleaseOccupancy(db.DB, inB.ID, g.laneB.ID), "B's robot leaves")
	d.EvaluateGroupReleases(g.grp.ID)

	after, steps := c2cReload(t, db, order)
	_, drop := c2cLastDrop(steps)
	if drop.Node != g.b[1].Name || after.WaitIndex < 1 {
		red = append(red, fmt.Sprintf("after B's robot left, the store's drop is %q at wait index %d (cause %q), "+
			"want it released into %s: A is taken, B has room, and the group's pass asks every lane",
			drop.Node, after.WaitIndex, after.QueueCause, g.b[1].Name))
	}
	if len(red) != 0 {
		t.Fatalf("a plain store into a group: %v", red)
	}
	if after.DeliveryNode != g.b[1].Name {
		t.Errorf("delivery_node = %q, want %s (the store's drop is its last)", after.DeliveryNode, g.b[1].Name)
	}
	if n := c2cSlotRowsIn(t, db, order.ID, g.laneA.ID); n != 0 {
		t.Errorf("the store still holds %d slot row(s) in lane A: its paper moves with the choice", n)
	}
}

// TestC2cPin_APersonNamedPlainStoreStaysInItsLane is the control the ruling
// keeps unchanged: a slot a person named (a no-demand order naming the slot) is
// lane-scoped. A is taken and B is empty; the person's store waits in A, its
// slot confirmed at dispatch as it always was. GREEN AT BASE AND AFTER.
func TestC2cPin_APersonNamedPlainStoreStaysInItsLane(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcherWithResolver(t, db)
	g := c2cTwoLaneGroup(t, db, "C2CPN", 2, 2)
	press, away := lineNode(t, db, "C2CPN-PRESS"), lineNode(t, db, "C2CPN-AWAY")
	full := testdb.CreateBinAtNode(t, db, sd.Payload.Code, press.ID, "C2CPN-FULL")
	order := c2cPlainStore(t, db, "c2cpn-store", sd.Payload.Code, press, g.a[1], full.ID, protocol.OriginClassNoDemand)

	dest := d.ReserveStorageDropoff(order)
	if dest.Refused() || dest.Node.ID != g.a[1].ID {
		t.Fatalf("fixture: the named slot %s must be reserved, got %v (%v)", g.a[1].Name, dest.Node, dest.Err)
	}
	c2cOccupy(t, db, "c2cpn-in-a", g.laneA, away)
	_, err := d.DispatchHand(order, full.ID, press, dest.Node)
	testutil.MustNoErr(t, err, "dispatch the person's store")
	d.EvaluateGroupReleases(g.grp.ID)

	after, steps := c2cReload(t, db, order)
	_, drop := c2cLastDrop(steps)
	if drop.Node != g.a[1].Name || !IsGateStaged(after) || after.QueueCause != string(CauseLaneOccupied) {
		t.Fatalf("the person's store is at %q (staged=%v, cause %q), want it waiting for %s in lane A: a slot "+
			"a person named is not re-chosen", drop.Node, IsGateStaged(after), after.QueueCause, g.a[1].Name)
	}
	if drop.Group != "" {
		t.Errorf("the person's drop carries group %q: a named slot has none", drop.Group)
	}
	if state, _ := c2cSlotRow(t, db, order.ID, g.a[1].ID); state != string(reservations.StateConfirmed) {
		t.Errorf("the person's slot row is %q, want confirmed at dispatch (unchanged)", state)
	}
}

// TestC2cPin_AFlatChildOfALanedGroupIsACandidateAtRelease: "the chooser
// considers every child of the group" (RULINGS-C2c-run-through §3.2). G has
// lanes A and B and one flat child F. The swap waits at G's spot pencilled in
// A; A holds another robot, B is full, F is free. The release takes F.
//
// RED AT BASE (56a702c6): the release passed a flat child over ("A FLAT CHILD
// OF A LANED GROUP is passed over: the robot stands at a lane's wait"), so the
// swap waited lane-occupied with F empty.
func TestC2cPin_AFlatChildOfALanedGroupIsACandidateAtRelease(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcherWithResolver(t, db)
	g := c2cTwoLaneGroup(t, db, "C2CFC", 2, 2)
	flat := &nodes.Node{Name: "C2CFC-F", Enabled: true, ParentID: &g.grp.ID}
	testutil.MustNoErr(t, db.CreateNode(flat), "create the group's flat child")
	press, press2 := lineNode(t, db, "C2CFC-PRESS"), lineNode(t, db, "C2CFC-PRESS2")
	elsewhere, away := lineNode(t, db, "C2CFC-EMPTYSRC"), lineNode(t, db, "C2CFC-AWAY")
	testdb.CreateBinAtNode(t, db, "", elsewhere.ID, "C2CFC-EMPTY")
	fillLaneSlots(t, db, sd.Payload.Code, "C2CFC-B", g.b...)
	c2cOccupy(t, db, "c2cfc-in-a", g.laneA, away)
	swap := c2cAtSpot(t, db, "c2cfc-swap", sd.Payload.Code, 0,
		c2cSwapPlan(g, press.Name, g.a[1].Name, elsewhere.Name, press2.Name, true))
	c2cPaper(t, db, swap.ID, g.a[1])

	c2cGatePass(d, g)

	after, steps := c2cReload(t, db, swap)
	_, drop := c2cInteriorDrop(steps)
	if drop.Node != flat.Name || after.WaitIndex < 2 {
		t.Fatalf("the interior drop is at %q at wait index %d (cause %q), want it released onto the group's "+
			"flat child %s: A is taken, B is full, and the chooser considers every child of the group",
			drop.Node, after.WaitIndex, after.QueueCause, flat.Name)
	}
	if state, _ := c2cSlotRow(t, db, swap.ID, flat.ID); state != string(reservations.StateConfirmed) {
		t.Errorf("the swap's row on %s is %q, want confirmed: the release takes the chosen child as armor",
			flat.Name, state)
	}
	for _, lane := range []*nodes.Node{g.laneA, g.laneB} {
		if occ, err := reservations.OccupantsOf(db.DB, lane.ID); err == nil && slices.Contains(occ, swap.ID) {
			t.Errorf("the swap occupies %s: a drop onto a flat child enters no lane", lane.Name)
		}
	}
	if err := d.assertEachWaitGatesItsEntry(steps); err != nil {
		t.Errorf("the released plan no longer holds: %v", err)
	}
}

// TestC2cPin_ARefusedOccupancyAtTheAppendGivesTheSlotBack is the occupancy pin
// the orc asked for (RULINGS-C2c-run-through §3.2): lane occupancy is taken at
// the tail append, not in the release transaction, so the append's occupancy
// can be refused AFTER the release transaction took the chosen slot as armor.
// The order must not strand: it ends released into the lane, or back to
// waiting with a cause and its slot given back.
//
// The refusal is real, not simulated: a trigger on this test's database
// refuses the occupancy row for the swap (the take's INSERT fails, as a
// database refusing it would). The release then appends nothing, and the swap
// must be back at G's spot with a cause, holding its slot as PAPER again (no
// hard claim, its row pending), so neither a stranger nor its own next pass
// meets an armor nobody is driving to. With the trigger gone, the next pass
// releases it.
//
// RED AT BASE (56a702c6): the release transaction's armor stayed after the
// refused append — claimed_by set and the row confirmed on a slot no robot was
// driving to — until a later pass happened to re-choose the same slot.
func TestC2cPin_ARefusedOccupancyAtTheAppendGivesTheSlotBack(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	d, backend := newTestDispatcherWithResolver(t, db)
	g := c2cTwoLaneGroup(t, db, "C2COC", 2, 2)
	press, press2 := lineNode(t, db, "C2COC-PRESS"), lineNode(t, db, "C2COC-PRESS2")
	elsewhere := lineNode(t, db, "C2COC-EMPTYSRC")
	testdb.CreateBinAtNode(t, db, "", elsewhere.ID, "C2COC-EMPTY")
	swap := c2cAtSpot(t, db, "c2coc-swap", sd.Payload.Code, 0,
		c2cSwapPlan(g, press.Name, g.a[1].Name, elsewhere.Name, press2.Name, true))
	c2cPaper(t, db, swap.ID, g.a[1])

	_, err := db.Exec(fmt.Sprintf(`CREATE FUNCTION c2c_refuse_occupancy_%[1]d() RETURNS trigger AS $$
		BEGIN
		  IF NEW.resource_kind = 'occupancy' AND NEW.order_id = %[1]d THEN
		    RAISE EXCEPTION 'test: the lane refuses order %[1]d''s occupancy';
		  END IF;
		  RETURN NEW;
		END $$ LANGUAGE plpgsql`, swap.ID))
	testutil.MustNoErr(t, err, "create the refusal")
	_, err = db.Exec(fmt.Sprintf(`CREATE TRIGGER c2c_refuse_occupancy_%[1]d BEFORE INSERT ON reservations
		FOR EACH ROW EXECUTE FUNCTION c2c_refuse_occupancy_%[1]d()`, swap.ID))
	testutil.MustNoErr(t, err, "install the refusal")
	appends := len(backend.ReleaseCalls())

	c2cGatePass(d, g)

	after, steps := c2cReload(t, db, swap)
	_, drop := c2cInteriorDrop(steps)
	var red []string
	if !IsGateStaged(after) || after.WaitIndex != 1 {
		red = append(red, fmt.Sprintf("the swap is %s at wait index %d, want it still at G's spot: its append "+
			"was refused", after.Status, after.WaitIndex))
	}
	if after.QueueCause == "" {
		red = append(red, "the swap waits with no cause: a refused append is a wait, and the row says why")
	}
	if n := len(backend.ReleaseCalls()) - appends; n != 0 {
		red = append(red, fmt.Sprintf("%d append(s) reached the fleet after the occupancy was refused", n))
	}
	slot, err := db.GetNodeByDotName(drop.Node)
	testutil.MustNoErr(t, err, "read the chosen slot")
	if slot.ClaimedBy != nil {
		red = append(red, fmt.Sprintf("%s is still claimed by order %d after the refused append: the release's "+
			"armor must be given back", slot.Name, *slot.ClaimedBy))
	}
	if state, tag := c2cSlotRow(t, db, swap.ID, slot.ID); state != string(reservations.StatePending) ||
		tag != reservations.GroupRoomTag {
		red = append(red, fmt.Sprintf("the swap's row on %s is %q/%q, want pending/%s: back to the paper it "+
			"held before the release", slot.Name, state, tag, reservations.GroupRoomTag))
	}
	if len(red) != 0 {
		t.Fatalf("a refused occupancy at the append: %v", red)
	}

	_, err = db.Exec(fmt.Sprintf(`DROP TRIGGER c2c_refuse_occupancy_%d ON reservations`, swap.ID))
	testutil.MustNoErr(t, err, "lift the refusal")
	c2cGatePass(d, g)
	if again, _ := c2cReload(t, db, swap); again.WaitIndex < 2 {
		t.Fatalf("with the refusal lifted the swap is still at wait index %d (cause %q): the next pass must "+
			"release it", again.WaitIndex, again.QueueCause)
	}
}
