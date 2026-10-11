//go:build docker

package dispatch

import (
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

// c2c_paper_room_pins_docker_test.go — C2c (Lane C), P4 of DESIGN-C2c §9: rank
// and commitment take paper room (the brief's fourth proof line).
//
// Ruling 2: between plan and release a planned drop holds one unit of room in
// its group, as paper — a pending reservation, the same row count as today's
// slot row. "It is taken by rank or yields to a committed robot, as paper
// does." D13 F1: the paper is a pending `slot` row on the chosen slot tagged
// reserved_by='groupRoom'. D13 F2: commitment wins over rank on a staged
// robot's paper — an unsent plan never takes it, whatever its rank.
//
// THE FIXTURE: X holds the paper on S, the one slot of G any chooser could
// offer. At the base the store choosers never offer a stranger's pending slot
// (SlotTakeableSQL, store/nodes/lanes.go), whatever the asker's rank or state.

// c2cPaperHolder is X: a never-sent order whose plan drops into G at S, holding
// S's paper. priority ranks it.
func c2cPaperHolder(t *testing.T, db *store.DB, uuid, payload string, priority int, g c2cGroup, s *nodes.Node) *orders.Order {
	t.Helper()
	x := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = uuid
		o.OrderType = OrderTypeComplex
		o.PayloadCode = payload
		o.DeliveryNode = s.Name
		o.Status = protocol.StatusQueued
	})
	testutil.MustNoErr(t, db.UpdateOrderStepsJSON(x.ID, fmt.Sprintf(
		`[{"action":%q,"node":%q},{"action":%q,"node":%q,"group":%q}]`,
		protocol.ActionPickup, strings.ToUpper(uuid)+"-SRC", protocol.ActionDropoff, s.Name, g.grp.Name)),
		"X's plan")
	if priority != 0 {
		_, err := db.Exec(`UPDATE orders SET priority = $1 WHERE id = $2`, priority, x.ID)
		testutil.MustNoErr(t, err, "rank X")
	}
	c2cPaper(t, db, x.ID, s)
	return x
}

// c2cAskAtAcquiring is Y: a swap whose interior drop names G, through the real
// intake with the resolver production has, then one dispatch pass. The shape is
// TestC2bPin_AnInteriorGroupDropWaitsForAFreeChild's.
func c2cAskAtAcquiring(t *testing.T, d *Dispatcher, db *store.DB, prefix, payload string, binTypeID int64,
	priority int, g c2cGroup) *orders.Order {
	t.Helper()
	press, press2, cell := lineNode(t, db, prefix+"-PRESS"), lineNode(t, db, prefix+"-PRESS2"), lineNode(t, db, prefix+"-CELL")
	prResident(t, db, press, binTypeID, payload, prefix+"-FULL")
	testdb.CreateBinAtNode(t, db, "", cell.ID, prefix+"-EMPTY")
	uuid := strings.ToLower(prefix) + "-y"
	d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID: uuid, PayloadCode: payload, Quantity: 1, Priority: priority, ProcessNode: press.Name,
		Steps: []protocol.ComplexOrderStep{
			prPick(press.Name), prDrop(g.grp.Name),
			{Action: protocol.ActionPickup, Node: cell.Name, Empty: true}, prDrop(press2.Name),
		},
	})
	if o := prReloadUUID(t, db, uuid); o.Priority != priority {
		_, err := db.Exec(`UPDATE orders SET priority = $1 WHERE id = $2`, priority, o.ID)
		testutil.MustNoErr(t, err, "rank Y")
	}
	prScanPass(t, d, db, uuid)
	return prReloadUUID(t, db, uuid)
}

// c2cSlotHolder is the order holding S's live slot row, or 0.
func c2cSlotHolder(t *testing.T, db *store.DB, s *nodes.Node) int64 {
	t.Helper()
	var holder int64
	err := db.DB.QueryRow(`SELECT order_id FROM reservations
		WHERE node_id = $1 AND resource_kind = 'slot' AND state <> 'released' LIMIT 1`, s.ID).Scan(&holder)
	if err != nil {
		return 0
	}
	return holder
}

// c2cOneFreeSlotGroup is G with exactly one free slot S: B's mouth. A is full
// and B1 holds a bin.
func c2cOneFreeSlotGroup(t *testing.T, db *store.DB, prefix, payload string) (c2cGroup, *nodes.Node) {
	t.Helper()
	g := c2cTwoLaneGroup(t, db, prefix, 2, 2)
	testdb.CreateBinAtNode(t, db, payload, g.a[0].ID, prefix+"-A0")
	testdb.CreateBinAtNode(t, db, payload, g.a[1].ID, prefix+"-A1")
	testdb.CreateBinAtNode(t, db, payload, g.b[1].ID, prefix+"-B1")
	return g, g.b[0]
}

// TestC2cPin_RankTakesPaperRoom is P4 (a). X is queued and never sent, holding
// the paper on S; Y outranks X (higher priority) and asks G at acquiring.
// Ruling 2 ("taken by rank"): Y's chooser is offered S, Y takes it, X's row is
// gone, and X's wait names Y (slot-taken, the HolderNote store.TakeSlotByRank
// already writes for a dig leg; DESIGN-C2c §7.2 generalises that door).
//
// RED AT BASE, by reading: Y's intake and acquiring resolve of G both pass over
// S (a stranger's pending slot is untakeable whatever the rank), so Y parks
// under ngrp-resolve and X keeps S.
func TestC2cPin_RankTakesPaperRoom(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcherWithResolver(t, db)
	g, s := c2cOneFreeSlotGroup(t, db, "C2CP4A", sd.Payload.Code)
	x := c2cPaperHolder(t, db, "c2cp4a-x", sd.Payload.Code, 0, g, s)

	y := c2cAskAtAcquiring(t, d, db, "C2CP4A", sd.Payload.Code, sd.BinType.ID, 100, g)

	var red []string
	if y.QueueCause == string(CauseNGRPResolve) {
		red = append(red, fmt.Sprintf("Y (priority 100) waits under %q although S is only an outranked plan's paper",
			y.QueueCause))
	}
	if h := c2cSlotHolder(t, db, s); h != y.ID {
		red = append(red, fmt.Sprintf("S's slot row is held by order %d, want Y (%d)", h, y.ID))
	}
	if n := c2bSlotRows(t, db, x.ID); n != 0 {
		red = append(red, fmt.Sprintf("the outranked X still holds %d slot row(s)", n))
	}
	xa, err := db.GetOrder(x.ID)
	testutil.MustNoErr(t, err, "reload X")
	if xa.QueueCause != "slot-taken" || !strings.Contains(xa.QueueReason, fmt.Sprint(y.ID)) {
		red = append(red, fmt.Sprintf("X's wait is %q (%q), want slot-taken naming Y (order %d)",
			xa.QueueCause, xa.QueueReason, y.ID))
	}
	if len(red) != 0 {
		t.Fatalf("P4 (a): %v", red)
	}
}

// TestC2cPin_AnEqualRankNeverTakesAnOlderPlansPaper is P4 (a′), the control:
// the same shape with Y at X's rank and younger. DemandRank.Outranks breaks a
// priority tie for the incumbent (older wins; the loser never steals back,
// SYNTH round 1 Fork 2, Rule 2), so X keeps S and Y waits.
//
// GREEN AT BASE (nothing pending is offered) and GREEN AFTER (the rank refuses).
func TestC2cPin_AnEqualRankNeverTakesAnOlderPlansPaper(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcherWithResolver(t, db)
	g, s := c2cOneFreeSlotGroup(t, db, "C2CP4A2", sd.Payload.Code)
	x := c2cPaperHolder(t, db, "c2cp4a2-x", sd.Payload.Code, 0, g, s)

	y := c2cAskAtAcquiring(t, d, db, "C2CP4A2", sd.Payload.Code, sd.BinType.ID, 0, g)

	if h := c2cSlotHolder(t, db, s); h != x.ID {
		t.Fatalf("S's slot row is held by order %d, want the older X (%d): an equal rank never takes", h, x.ID)
	}
	if y.QueueCause != string(CauseNGRPResolve) {
		t.Fatalf("the younger Y is %s under %q, want it waiting under %q", y.Status, y.QueueCause, CauseNGRPResolve)
	}
}

// TestC2cPin_ACommittedRobotTakesAnUnsentPlansPaper is P4 (b). Z is dispatched
// and staged at G's spot; its pencilled slot A0 filled while it drove, and G
// has no free slot but S, which X holds as paper and X was never sent. X
// OUTRANKS Z here on purpose: the take is by commitment, not by rank. Ruling 2
// ("yields to a committed robot") and the model ("a plan never blocks a
// committed robot", HANDOFF §5): Z is released into S on the pass, X's row is
// gone, and X's wait says so (the same door as the rank take, DESIGN-C2c §7.2,
// with the note "a committed robot") — X re-plans on its next pass.
//
// RED AT BASE, by reading: Z's release finds nothing in A (full; A0's own slot
// is taken, so acceptanceDigNeeded answers no), the widen resolves G and S is
// untakeable (a stranger's pending row), so Z waits under
// gate-rebind-unavailable holding the press's bin.
func TestC2cPin_ACommittedRobotTakesAnUnsentPlansPaper(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcherWithResolver(t, db)
	g, s := c2cOneFreeSlotGroup(t, db, "C2CP4B", sd.Payload.Code)
	x := c2cPaperHolder(t, db, "c2cp4b-x", sd.Payload.Code, 100, g, s)
	press, press2 := lineNode(t, db, "C2CP4B-PRESS"), lineNode(t, db, "C2CP4B-PRESS2")
	elsewhere := lineNode(t, db, "C2CP4B-EMPTYSRC")
	testdb.CreateBinAtNode(t, db, "", elsewhere.ID, "C2CP4B-EMPTY")
	z := c2cAtSpot(t, db, "c2cp4b-z", sd.Payload.Code, 0,
		c2cSwapPlan(g, press.Name, g.a[0].Name, elsewhere.Name, press2.Name, true))

	c2cGatePass(d, g)

	after, steps := c2cReload(t, db, z)
	_, drop := c2cInteriorDrop(steps)
	var red []string
	if drop.Node != s.Name || after.WaitIndex < 2 {
		red = append(red, fmt.Sprintf("Z's drop is at %q at wait index %d (cause %q), want it released into %s: "+
			"a committed robot is not blocked by an unsent plan's paper", drop.Node, after.WaitIndex,
			after.QueueCause, s.Name))
	}
	if n := c2bSlotRows(t, db, x.ID); n != 0 {
		red = append(red, fmt.Sprintf("the unsent X still holds %d slot row(s)", n))
	}
	xa, err := db.GetOrder(x.ID)
	testutil.MustNoErr(t, err, "reload X")
	if xa.QueueCause != "slot-taken" || !strings.Contains(xa.QueueReason, fmt.Sprint(z.ID)) {
		red = append(red, fmt.Sprintf("X's wait is %q (%q), want slot-taken naming Z (order %d) so it re-plans",
			xa.QueueCause, xa.QueueReason, z.ID))
	}
	if len(red) != 0 {
		t.Fatalf("P4 (b): %v", red)
	}
}

// TestC2cPin_AnUnsentPlanNeverTakesAStagedRobotsPaper is P4 (b′), D13 F2's
// control. X is dispatched and standing at G's spot (committed), holding the
// paper on S; Y is unsent and OUTRANKS X. F2: commitment wins — a staged
// order's paper is not offered by rank to an unsent plan. Y waits; X keeps S.
//
// GREEN AT BASE (nothing pending is offered) and GREEN AFTER (F2).
func TestC2cPin_AnUnsentPlanNeverTakesAStagedRobotsPaper(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcherWithResolver(t, db)
	g, s := c2cOneFreeSlotGroup(t, db, "C2CP4B2", sd.Payload.Code)
	xPress, xPress2 := lineNode(t, db, "C2CP4B2-XPRESS"), lineNode(t, db, "C2CP4B2-XPRESS2")
	xEmpty := lineNode(t, db, "C2CP4B2-XEMPTYSRC")
	testdb.CreateBinAtNode(t, db, "", xEmpty.ID, "C2CP4B2-XEMPTY")
	xPlan := c2cSwapPlan(g, xPress.Name, s.Name, xEmpty.Name, xPress2.Name, true)
	xPlan[2].WaitLane = g.laneB.ID // S is B's mouth: the wait names the lane its entry is in
	x := c2cAtSpot(t, db, "c2cp4b2-x", sd.Payload.Code, 0, xPlan)
	c2cPaper(t, db, x.ID, s)

	y := c2cAskAtAcquiring(t, d, db, "C2CP4B2", sd.Payload.Code, sd.BinType.ID, 100, g)

	if h := c2cSlotHolder(t, db, s); h != x.ID {
		t.Fatalf("S's slot row is held by order %d, want the committed X (%d): F2, an unsent plan never takes "+
			"a staged robot's paper, whatever its rank", h, x.ID)
	}
	if y.VendorOrderID != "" || !protocol.IsAcquiring(y.Status) {
		t.Fatalf("the unsent Y was sent (%s, vendor %q) with G's only room held by a committed robot",
			y.Status, y.VendorOrderID)
	}
	xa, err := db.GetOrder(x.ID)
	testutil.MustNoErr(t, err, "reload X")
	if xa.QueueCause == "slot-taken" {
		t.Fatalf("the committed X was told its slot was taken (%q)", xa.QueueReason)
	}
}
