//go:build docker

package dispatch

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// delivered_leg_docker_test.go: brief C item 5 (docket R87), ruled in the final
// brief's 1c (docket section 25, B21).
//
// Each test runs one of the doors that closes a dig's chapter by cancelling its
// live legs (the callers of splitLiftedLegs) while a leg has set its bin down
// and is not yet confirmed, and asserts what the door does: the leg's end, its
// bin's row, the events, what reaches the fleet and the station, any order
// made, and where the chapter and the parent go. They were characterisations
// of the doors cancelling such a leg; the ruling (a leg is finished when it
// puts its bin down, and it confirms) turned each into a pin of the door
// keeping it, and every flipped assertion is named in that commit. A change
// that fails one of these changes the behaviour it records, and the test is
// re-read against the ruling, never loosened to pass.
//
// THE LEG IS DELIVERED AS THE ENGINE DELIVERS IT (engine/wiring_vendor_status.go,
// the delivered arm), in two halves, because a door can run in either:
//
//   - LANDED (landLegBin): handleOrderDelivered sets the bin down at the leg's
//     drop and releases its claim, then emits the bin's move, whose lane-gate
//     subscriber re-drives the parent of any leg still awaiting the fleet in the
//     lane the bin left (RedriveHeldCompoundLegs). All of that runs before the
//     status write: the leg still reads in_transit.
//   - DELIVERED (markLegDelivered): MarkDelivered, whose fireCompleted advances
//     the parent in line (HandleChildOrderComplete) with the leg delivered, and
//     only after that the engine's own confirm (autoConfirmLeg). A Core stopped
//     between the two never confirms the leg on restart (a delivered order is
//     not re-tracked), so it stays delivered until the reconciliation sweep.
//
// This package has no engine, so each test makes the engine's calls itself, in
// the engine's order, and says at each step which call it stands for.

// landLegBin is the first half of a dig leg's delivery: the robot has set its
// bin down. As handleOrderDelivered's settle does it: the leg's slot claims
// released, then the multi-bin arrival, a handoff that also releases the bin's
// claim (store.ApplyMultiBinArrival). A leg still dwelling is released first,
// which is where a dig leg learns its drop (releaseDwell), and a leg the fleet
// has not yet reported moving is put in transit, as RUNNING would have.
func landLegBin(t *testing.T, r twiceRig, leg *orders.Order) *orders.Order {
	t.Helper()
	leg = releaseDwell(t, r.d, r.db, leg)
	if leg.Status != StatusInTransit {
		testutil.MustNoErr(t, r.d.lifecycle.MarkInTransit(leg, "", "fleet", protocol.OrderUpdate{}),
			"the fleet reports the leg running")
	}
	drop, err := r.db.GetNodeByDotName(leg.DeliveryNode)
	testutil.MustNoErr(t, err, "resolve the leg's drop "+leg.DeliveryNode)
	testutil.MustNoErr(t, r.db.UnclaimOrderSlots(leg.ID), "release the leg's slot claims")
	_, err = r.db.ApplyMultiBinArrival(leg.ID, []orders.BinArrivalInstruction{
		{BinID: *testdb.OnlyCart(t, leg), ToNodeID: drop.ID}}, nil)
	testutil.MustNoErr(t, err, "set the leg's bin down at its drop")
	return reloadLeg(t, r, leg.ID)
}

// markLegDelivered is the second half's write, MarkDelivered, on the copy the
// engine holds. It returns the write's error: a door that ran in the first half
// may have ended the leg under it.
func markLegDelivered(r twiceRig, leg *orders.Order) error {
	return r.d.lifecycle.MarkDelivered(leg, "fleet", protocol.OrderUpdate{Detail: "fleet state: FINISHED"})
}

// autoConfirmLeg is the engine's confirm of a delivered compound child, the
// delivered arm's last call, on the same copy.
func autoConfirmLeg(r twiceRig, leg *orders.Order) error {
	_, err := r.d.lifecycle.ConfirmReceipt(leg, leg.Requester(), "auto_confirm_internal", 0)
	return err
}

func reloadLeg(t *testing.T, r twiceRig, id int64) *orders.Order {
	t.Helper()
	o, err := r.db.GetOrder(id)
	testutil.MustNoErr(t, err, fmt.Sprintf("reload order %d", id))
	return o
}

// The statuses a kept leg passes through: delivered, then confirmed. (Before
// final brief 1c the doors cancelled it, after its bin landed or after its
// delivered write.)
var deliveredThenConfirmed = []protocol.Status{StatusPending, StatusDispatched, StatusInTransit, StatusDelivered, StatusConfirmed}

// historyOf is an order's statuses, oldest first.
func historyOf(t *testing.T, r twiceRig, id int64) []protocol.Status {
	t.Helper()
	hist, err := r.db.ListOrderHistory(id)
	testutil.MustNoErr(t, err, "read the order's history")
	var out []protocol.Status
	for _, h := range hist {
		out = append(out, h.Status)
	}
	return out
}

// assertLegEnded asserts a leg's end, the statuses it passed through, and its
// bin's row: where the bin is and that nobody holds it. It logs the whole
// record for the report.
func assertLegEnded(t *testing.T, r twiceRig, legID int64, status protocol.Status, detail string,
	history []protocol.Status, binAt string) {
	t.Helper()
	got := reloadLeg(t, r, legID)
	bin, err := r.db.GetBin(*testdb.OnlyCart(t, got))
	testutil.MustNoErr(t, err, "read the leg's bin")
	steps := historyOf(t, r, legID)
	t.Logf("record: order %d %s (%q) completed=%v history=%v; bin %d at %s claimed_by=%v",
		got.ID, got.Status, got.ErrorDetail, got.CompletedAt != nil, steps, bin.ID, bin.NodeName, bin.ClaimedBy)
	if got.Status != status || got.ErrorDetail != detail {
		t.Errorf("leg %d is %q (%q), want %q (%q)", got.ID, got.Status, got.ErrorDetail, status, detail)
	}
	if !slices.Equal(steps, history) {
		t.Errorf("leg %d passed through %v, want %v", got.ID, steps, history)
	}
	if (got.CompletedAt != nil) != (status == StatusConfirmed) {
		t.Errorf("leg %d completed_at set=%v at %q", got.ID, got.CompletedAt != nil, got.Status)
	}
	if bin.NodeName != binAt || bin.ClaimedBy != nil {
		t.Errorf("leg %d's bin %d is at %s claimed by %v, want at %s and unclaimed", got.ID, bin.ID,
			bin.NodeName, bin.ClaimedBy, binAt)
	}
}

// cancelsOf lists the cancel reasons emitted for one order, in order.
func cancelsOf(e *mockEmitter, orderID int64) []string {
	var out []string
	for _, c := range e.cancelled {
		if c.orderID == orderID {
			out = append(out, c.reason)
		}
	}
	return out
}

// completionsOf counts the completed events emitted for one order: the
// delivered write's, then the confirm's.
func completionsOf(e *mockEmitter, orderID int64) int {
	n := 0
	for _, c := range e.completed {
		if c.orderID == orderID {
			n++
		}
	}
	return n
}

// cut is where a door is measured from: the outbox's last row, the last
// order's id, and the fleet stops so far.
type cut struct {
	outbox, order int64
	stops         int
}

func cutNow(t *testing.T, r twiceRig) cut {
	t.Helper()
	var id int64
	testutil.MustNoErr(t, r.db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM orders`).Scan(&id), "last order id")
	return cut{outbox: outboxMark(t, r.db), order: id, stops: len(r.backend.CancelRequests())}
}

// stopsSince lists the vendor jobs the fleet was asked to stop since c.
func (c cut) stopsSince(r twiceRig) []string {
	return slices.Clone(r.backend.CancelRequests()[c.stops:])
}

// ordersSince counts the orders written since c.
func (c cut) ordersSince(t *testing.T, r twiceRig) int {
	t.Helper()
	var n int
	testutil.MustNoErr(t, r.db.QueryRow(`SELECT count(*) FROM orders WHERE id > $1`, c.order).Scan(&n), "count new orders")
	return n
}

// toldAbout lists what the station was told since c about the order with edge
// uuid uuid, rendered as toldSince renders it. It matches the uuid before the
// rendering writes a minted one as <uuid>.
func (c cut) toldAbout(t *testing.T, r twiceRig, uuid string) []string {
	t.Helper()
	msgs, err := r.db.ListPendingOutbox(500)
	testutil.MustNoErr(t, err, "list outbox")
	var out []string
	for _, m := range msgs {
		if m.ID <= c.outbox {
			continue
		}
		var env protocol.Envelope
		testutil.MustNoErr(t, json.Unmarshal(m.Payload, &env), "unmarshal envelope")
		var body struct {
			OrderUUID string `json:"order_uuid"`
		}
		if json.Unmarshal(env.Payload, &body) != nil || body.OrderUUID != uuid {
			continue
		}
		out = append(out, fmt.Sprintf("%s to %s: %s", m.MsgType, m.DstStation, normalizeTold(env.Payload)))
	}
	return out
}

// liftedDig plans planDigToDissolve's dig over `blockers` blockers and lifts
// leg 1's blocker, as the fleet's pickup does: leg 1 is carrying, dwelling in
// the lane it dug. It returns the parent, the lane, its slots shallowest first
// and the legs in sequence order.
func liftedDig(t *testing.T, r twiceRig, prefix string, blockers int) (*orders.Order, *nodes.Node, []*nodes.Node, []*orders.Order) {
	t.Helper()
	parent, lane, slots, legs := planDigToDissolve(t, r.db, r.d, prefix, blockers)
	transit, err := r.db.GetNodeByDotName("_TRANSIT")
	testutil.MustNoErr(t, err, "resolve _TRANSIT")
	liftBin(t, r.db, r.d, legs[0], slots[0], transit)
	return parent, lane, slots, legs
}

// heldBehindAClaimedWall keeps leg 2 unsent past leg 1's release. A bin stands
// in front of leg 2's pickup, claimed by an order that is still moving (a
// hauler on its way to take it out), so the release's wake re-asks leg 2 and
// holds it rather than dissolving (handleStaleDigLeg's claimed arm; without the
// wall the wake sends leg 2 at once). Leg 1 is released from its dwell, which
// gives it its drop. Returns leg 1, reloaded, and the hauler.
func heldBehindAClaimedWall(t *testing.T, r twiceRig, parent *orders.Order, slots []*nodes.Node, legs []*orders.Order) (*orders.Order, *orders.Order) {
	t.Helper()
	wall := testdb.CreateBinAtNode(t, r.db, parent.PayloadCode, slots[0].ID, parent.EdgeUUID+"-WALL")
	hauler := testdb.CreateOrder(t, r.db, func(o *orders.Order) {
		o.EdgeUUID = parent.EdgeUUID + "-hauler"
		o.Status, o.VendorOrderID = protocol.StatusDispatched, "sg-"+parent.EdgeUUID+"-hauler"
	})
	testdb.ClaimBinInDugLaneForTest(t, r.db, wall.ID, hauler.ID)
	leg1 := releaseDwell(t, r.d, r.db, legs[0])
	next := reloadLeg(t, r, legs[1].ID)
	if next.VendorOrderID != "" || QueueCause(next.QueueCause) != CauseLaneTargetBuried {
		t.Fatalf("fixture: leg 2 after leg 1's release: vendor=%q cause=%q, want it held unsent behind the wall",
			next.VendorOrderID, next.QueueCause)
	}
	return leg1, hauler
}

// stallOrder makes an order read as stopped: nothing has advanced it for two
// stall windows (claimantIsMoving).
func stallOrder(t *testing.T, r twiceRig, id int64) {
	t.Helper()
	_, err := r.db.DB.Exec(`UPDATE orders SET updated_at=$2 WHERE id=$1`, id, clock.Now().UTC().Add(-2*claimantStallWindow))
	testutil.MustNoErr(t, err, "stall the order")
}

// TestDeliveredLeg_Dissolve: the dissolve (dissolveCompound), reached on the
// stale-plan trigger (handleStaleDigLeg): leg 2's pickup is walled by a bin
// whose claimant has stopped moving. One blocker, so leg 1 unburies and leg 2
// retrieves the target.
func TestDeliveredLeg_Dissolve(t *testing.T) {
	t.Parallel()

	// The hauler stops before leg 1 sets its bin down, so the first to re-ask
	// leg 2 is leg 1's own landing: its bin's move re-drives the held legs of
	// the lane it left, before the delivered write. Final brief 1c: the dissolve
	// keeps leg 1, which reads in_transit with its bin down, and holds the lane
	// and the parent; its delivered write and confirm land, the fleet is not
	// asked to stop its finished job, and the confirm's re-drive lets the lane
	// go and the demand re-plans. (It was cancelled under the dissolve marker,
	// both writes refused, its finished job stopped.)
	t.Run("landed, not yet marked delivered", func(t *testing.T) {
		t.Parallel()
		r := newTwiceRig(t)
		parent, lane, slots, legs := liftedDig(t, r, "DL-DIS-L", 1)
		leg1, hauler := heldBehindAClaimedWall(t, r, parent, slots, legs)
		stallOrder(t, r, hauler.ID)
		leg1 = landLegBin(t, r, leg1)

		c := cutNow(t, r)
		r.d.RedriveHeldCompoundLegs(lane.ID) // the landing's lane-gate re-drive
		assertHeldForTheKeptLeg(t, r, parent, lane, legs[1])
		deliveredErr := markLegDelivered(r, leg1)
		confirmErr := autoConfirmLeg(r, leg1)
		testutil.MustNoErr(t, r.d.AdvanceCompoundOrder(parent.ID), "the confirm's re-drive")

		assertLegEnded(t, r, leg1.ID, StatusConfirmed, "", deliveredThenConfirmed, leg1.DeliveryNode)
		assertDissolved(t, r, c, parent, lane, leg1, legs[1])
		if deliveredErr != nil || confirmErr != nil {
			t.Errorf("the engine's delivered write returned %v and its confirm %v, want both to land", deliveredErr, confirmErr)
		}
		if n := completionsOf(r.e.mockEmitter, leg1.ID); n != 2 {
			t.Errorf("leg 1 announced completed %d times, want 2: its delivered write and its confirm", n)
		}
	})

	// Leg 1 lands and is delivered while the hauler still moves, so both its
	// landing and its delivery re-ask leg 2 and hold it. Core then stops before
	// the confirm, so leg 1 stays delivered. The hauler stops; the next to
	// re-ask leg 2 is the lane's floor (lane_floor.go) or any event in the lane.
	// Final brief 1c: the dissolve keeps leg 1 and holds the lane and the parent
	// until the reconciliation sweep confirms it (5 to 10 min in life); the
	// fleet is not asked to stop its finished job; the sweep's confirm lets the
	// lane go and the demand re-plans. (It was cancelled under the dissolve
	// marker from delivered, its finished job stopped.)
	t.Run("delivered, not yet confirmed", func(t *testing.T) {
		t.Parallel()
		r := newTwiceRig(t)
		parent, lane, slots, legs := liftedDig(t, r, "DL-DIS-D", 1)
		leg1, hauler := heldBehindAClaimedWall(t, r, parent, slots, legs)
		leg1 = landLegBin(t, r, leg1)
		r.d.RedriveHeldCompoundLegs(lane.ID) // the landing's re-drive: the hauler still moves
		testutil.MustNoErr(t, markLegDelivered(r, leg1), "the delivered write")
		r.d.HandleChildOrderComplete(leg1) // the delivery's own advance: still held
		if got := reloadLeg(t, r, legs[1].ID); got.Status != StatusPending || got.VendorOrderID != "" {
			t.Fatalf("fixture: leg 2 is %q vendor=%q with the hauler moving, want held", got.Status, got.VendorOrderID)
		}
		stallOrder(t, r, hauler.ID)

		c := cutNow(t, r)
		r.d.RedriveHeldCompoundLegs(lane.ID) // the floor, or the lane's next event
		testutil.MustNoErr(t, r.d.AdvanceCompoundOrder(parent.ID), "the dissolve marker's re-drive")
		assertHeldForTheKeptLeg(t, r, parent, lane, legs[1])
		if got := reloadLeg(t, r, leg1.ID); got.Status != StatusDelivered {
			t.Errorf("leg 1 is %q (%q) before the sweep, want delivered", got.Status, got.ErrorDetail)
		}
		_, err := r.d.lifecycle.ConfirmReceipt(reloadLeg(t, r, leg1.ID), leg1.Requester(), "auto_confirm_timeout", 0)
		testutil.MustNoErr(t, err, "the reconciliation sweep's confirm")
		testutil.MustNoErr(t, r.d.AdvanceCompoundOrder(parent.ID), "the confirm's re-drive")

		assertLegEnded(t, r, leg1.ID, StatusConfirmed, "", deliveredThenConfirmed, leg1.DeliveryNode)
		assertDissolved(t, r, c, parent, lane, leg1, legs[1])
		if n := completionsOf(r.e.mockEmitter, leg1.ID); n != 2 {
			t.Errorf("leg 1 announced completed %d times, want 2: its delivered write and the sweep's confirm", n)
		}
	})
}

// assertHeldForTheKeptLeg: the dissolve kept leg 1, which has set its bin down
// and not yet confirmed, so leg 2 is cancelled under the dissolve marker while
// the lane stays held and the parent stays reshuffling until leg 1's confirm.
func assertHeldForTheKeptLeg(t *testing.T, r twiceRig, parent *orders.Order, lane *nodes.Node, leg2 *orders.Order) {
	t.Helper()
	if got := reloadLeg(t, r, leg2.ID); got.Status != StatusCancelled || got.ErrorDetail != reshuffleDissolveDetail {
		t.Errorf("leg 2 is %q (%q) at the dissolve, want cancelled under the dissolve marker", got.Status, got.ErrorDetail)
	}
	if !r.d.laneLock.IsLocked(lane.ID) {
		t.Errorf("lane %s was let go at the dissolve, before leg 1 confirmed", lane.Name)
	}
	if got := reloadLeg(t, r, parent.ID); got.Status != StatusReshuffling {
		t.Errorf("parent is %q before leg 1 confirmed, want reshuffling", got.Status)
	}
}

// assertDissolved is what both halves of the dissolve record past the leg once
// leg 1 has confirmed (final brief 1c): no cancel emitted for leg 1 and no
// fleet stop for its finished job, its station told it confirmed and never
// cancelled, leg 2 cancelled under the dissolve marker, the lane let go, no
// order made, and the parent back at pending after the confirm's re-drive.
func assertDissolved(t *testing.T, r twiceRig, c cut, parent *orders.Order, lane *nodes.Node, leg1, leg2 *orders.Order) {
	t.Helper()
	if got := cancelsOf(r.e.mockEmitter, leg1.ID); len(got) != 0 {
		t.Errorf("cancels emitted for leg 1: %q, want none: it had set its bin down", got)
	}
	if got := c.stopsSince(r); len(got) != 0 {
		t.Errorf("fleet stops since the landing: %v, want none: leg 1's job had finished and leg 2 was never sent", got)
	}
	told := c.toldAbout(t, r, leg1.EdgeUUID)
	t.Logf("told about leg 1: %v", told)
	if len(told) == 0 || !strings.Contains(told[len(told)-1], `"status":"confirmed"`) ||
		slices.ContainsFunc(told, func(m string) bool { return strings.Contains(m, `"status":"cancelled"`) }) {
		t.Errorf("the station was told %v about leg 1, want it confirmed and never cancelled", told)
	}
	if got := reloadLeg(t, r, leg2.ID); got.Status != StatusCancelled || got.ErrorDetail != reshuffleDissolveDetail {
		t.Errorf("leg 2 is %q (%q), want cancelled under the dissolve marker", got.Status, got.ErrorDetail)
	}
	if r.d.laneLock.IsLocked(lane.ID) {
		t.Errorf("lane %s is still held after the dissolve", lane.Name)
	}
	if n := c.ordersSince(t, r); n != 0 {
		t.Errorf("%d order(s) made since the landing, want none", n)
	}
	assertRestarted(t, r, parent.ID, reshuffleDissolveDetail)
}

// assertRestarted: the parent is back at pending, and the row that put it
// there says why.
func assertRestarted(t *testing.T, r twiceRig, parentID int64, why string) {
	t.Helper()
	after := reloadLeg(t, r, parentID)
	restart := ""
	if h, err := r.db.LatestOrderHistoryForStatus(parentID, StatusPending); err == nil && h != nil {
		restart = h.Detail
	}
	t.Logf("parent %d: %q history=%v, back to pending with %q", after.ID, after.Status, historyOf(t, r, parentID), restart)
	if after.Status != StatusPending || restart != why {
		t.Errorf("parent is %q, last back to pending with %q; want pending with %q", after.Status, restart, why)
	}
}

// TestDeliveredLeg_FailureDoor: the failure door (HandleChildOrderFailure),
// reached when the advance meets a leg it cannot build and ends it through the
// lifecycle's failure door (failLegThatCannotBeBuilt). One blocker; leg 2 is
// held behind a claimed wall past leg 1's release, then a node it names is
// renamed under the open dig, so the next advance cannot build it. The engine's
// failure subscriber is played here as engineReDrive plays it.
func TestDeliveredLeg_FailureDoor(t *testing.T) {
	t.Parallel()

	// Leg 2's drop (the line) is renamed. Its pickup still names a slot of the
	// lane, so leg 1's landing re-drives it, before the delivered write: leg 1
	// reads in_transit with its bin down. Final brief 1c: the splitter keeps it
	// as it keeps a delivered one (the door's IsPostDelivery filter, which let
	// this half through, is folded into it), both engine writes land, the fleet
	// is not asked to stop its finished job, and the order fails on the config
	// fault once the chapter closes. (It was cancelled with the leg-failed
	// marker, both writes refused, its finished job stopped.)
	t.Run("landed, not yet marked delivered", func(t *testing.T) {
		t.Parallel()
		r := newTwiceRig(t)
		parent, lane, slots, legs := liftedDig(t, r, "DL-FD-L", 1)
		leg1, _ := heldBehindAClaimedWall(t, r, parent, slots, legs)
		renameNode(t, r, legs[1].DeliveryNode)
		leg1 = landLegBin(t, r, leg1)

		c := cutNow(t, r)
		r.d.RedriveHeldCompoundLegs(lane.ID) // the landing's re-drive: leg 2 cannot be built
		failed := reloadLeg(t, r, legs[1].ID)
		engineReDrive(t, r.d, r.e.mockEmitter, parent, failed)
		deliveredErr := markLegDelivered(r, leg1)
		confirmErr := autoConfirmLeg(r, leg1)
		testutil.MustNoErr(t, r.d.AdvanceCompoundOrder(parent.ID), "the confirm's re-drive")

		assertLegEnded(t, r, leg1.ID, StatusConfirmed, "", deliveredThenConfirmed, leg1.DeliveryNode)
		if deliveredErr != nil || confirmErr != nil {
			t.Errorf("the engine's delivered write returned %v and its confirm %v, want both to land", deliveredErr, confirmErr)
		}
		if got := cancelsOf(r.e.mockEmitter, leg1.ID); len(got) != 0 {
			t.Errorf("cancels emitted for leg 1: %q, want none", got)
		}
		if got := c.stopsSince(r); len(got) != 0 {
			t.Errorf("fleet stops since the landing: %v, want none: leg 1's job had finished", got)
		}
		assertParentFailedOnConfig(t, r, parent, failed)
	})

	// Leg 2's pickup slot is renamed. The leg no longer names a slot of the
	// lane, so leg 1's landing does not reach it; the delivery's own advance
	// does, with leg 1 delivered. Recorded: the splitter keeps leg 1 (its row
	// reads delivered; the door's own filter kept it before the fold) and the
	// engine's confirm then confirms it; the order fails on the config fault.
	t.Run("delivered, not yet confirmed", func(t *testing.T) {
		t.Parallel()
		r := newTwiceRig(t)
		parent, lane, slots, legs := liftedDig(t, r, "DL-FD-D", 1)
		leg1, _ := heldBehindAClaimedWall(t, r, parent, slots, legs)
		renameNode(t, r, legs[1].SourceNode)
		leg1 = landLegBin(t, r, leg1)
		r.d.RedriveHeldCompoundLegs(lane.ID) // the landing's re-drive: leg 2 is not in it
		if got := reloadLeg(t, r, legs[1].ID); protocol.IsTerminal(got.Status) {
			t.Fatalf("fixture: leg 2 is %q after the landing's re-drive, want it unreached", got.Status)
		}
		testutil.MustNoErr(t, markLegDelivered(r, leg1), "the delivered write")

		c := cutNow(t, r)
		r.d.HandleChildOrderComplete(leg1) // the delivery's own advance: leg 2 cannot be built
		failed := reloadLeg(t, r, legs[1].ID)
		engineReDrive(t, r.d, r.e.mockEmitter, parent, failed)
		confirmErr := autoConfirmLeg(r, leg1)
		testutil.MustNoErr(t, r.d.AdvanceCompoundOrder(parent.ID), "the confirm's re-drive")

		testutil.MustNoErr(t, confirmErr, "the engine's confirm")
		assertLegEnded(t, r, leg1.ID, StatusConfirmed, "", deliveredThenConfirmed, leg1.DeliveryNode)
		if got := cancelsOf(r.e.mockEmitter, leg1.ID); len(got) != 0 {
			t.Errorf("cancels emitted for leg 1: %q, want none", got)
		}
		if n := completionsOf(r.e.mockEmitter, leg1.ID); n != 2 {
			t.Errorf("leg 1 announced completed %d times, want 2: its delivered write and its confirm", n)
		}
		if got := c.stopsSince(r); len(got) != 0 {
			t.Errorf("fleet stops since the delivery: %v, want none", got)
		}
		assertParentFailedOnConfig(t, r, parent, failed)
	})
}

// renameNode renames a node under an open dig, as a person editing the layout
// does (TestCompound_UnknownNodeOnTheLastLeg_FailsTheOrderWithTheLegsSentence).
func renameNode(t *testing.T, r twiceRig, name string) {
	t.Helper()
	n, err := r.db.GetNodeByDotName(name)
	testutil.MustNoErr(t, err, "resolve the node "+name)
	n.Name = name + "-RENAMED"
	testutil.MustNoErr(t, r.db.UpdateNode(n), "rename the node")
}

// assertParentFailedOnConfig: the leg the advance could not build failed on a
// config fault, and the order fails with its sentence once the chapter closes
// (chapterStoppedShort's config carve-out).
func assertParentFailedOnConfig(t *testing.T, r twiceRig, parent, failed *orders.Order) {
	t.Helper()
	after := reloadLeg(t, r, parent.ID)
	t.Logf("leg 2 %d: %q (%q); parent %d: %q (%q) history=%v", failed.ID, failed.Status, failed.ErrorDetail,
		after.ID, after.Status, after.ErrorDetail, historyOf(t, r, parent.ID))
	if failed.Status != StatusFailed || after.Status != StatusFailed || after.ErrorDetail != failed.ErrorDetail {
		t.Errorf("leg 2 is %q (%q) and the order %q (%q), want both failed with the leg's sentence",
			failed.Status, failed.ErrorDetail, after.Status, after.ErrorDetail)
	}
}

// TestDeliveredLeg_ChapterEnd: the chapter end's close-the-rest arm
// (chapterStoppedShort), reached when a leg was cancelled without the
// chapter-end marker. One blocker: leg 1 lifts and is released, and the
// release's wake sends leg 2 behind it. A person then ends leg 2 from the
// orders page (engine.CancelOrder's cascade; a fleet stop is the same kind
// of cancel), which re-drives nothing (no marker). Leg 1 delivers, and its own
// advance is the chapter end.
//
// Final brief 1c: the delivery's advance keeps leg 1, which has set its bin
// down, and waits for it; the engine's confirm lands, the fleet is not asked to
// stop its finished job, and the confirm's re-drive returns the parent to
// pending naming leg 2 as the leg that was cancelled. (Leg 1 was cancelled
// with the leg-failed marker, its finished job stopped, the confirm refused,
// and the row read the bare leg-failed sentence, though no leg failed.)
func TestDeliveredLeg_ChapterEnd(t *testing.T) {
	t.Parallel()
	r := newTwiceRig(t)
	parent, lane, _, legs := liftedDig(t, r, "DL-CE", 1)
	leg1 := releaseDwell(t, r.d, r.db, legs[0])
	leg2 := reloadLeg(t, r, legs[1].ID)
	if leg2.VendorOrderID == "" {
		t.Fatalf("fixture: leg 2 was not sent at leg 1's release (cause %q)", leg2.QueueCause)
	}
	r.d.CancelOrderWithCascade(leg2, leg2.Requester(), "cancelled by test-operator",
		CancelCause{Code: protocol.TermOperatorCancelled, Actor: "test-operator"})
	leg1 = landLegBin(t, r, leg1)
	r.d.RedriveHeldCompoundLegs(lane.ID) // the landing's re-drive: no leg awaits the fleet
	if got := reloadLeg(t, r, leg1.ID); got.Status != StatusInTransit {
		t.Fatalf("fixture: leg 1 is %q after its landing's re-drive, want in_transit", got.Status)
	}
	testutil.MustNoErr(t, markLegDelivered(r, leg1), "the delivered write")

	c := cutNow(t, r)
	r.d.HandleChildOrderComplete(leg1) // the delivery's own advance: the chapter end
	confirmErr := autoConfirmLeg(r, leg1)
	testutil.MustNoErr(t, r.d.AdvanceCompoundOrder(parent.ID), "the confirm's re-drive")

	assertLegEnded(t, r, leg1.ID, StatusConfirmed, "", deliveredThenConfirmed, leg1.DeliveryNode)
	if confirmErr != nil {
		t.Errorf("the engine's confirm returned %v, want it to land", confirmErr)
	}
	if got := cancelsOf(r.e.mockEmitter, leg1.ID); len(got) != 0 {
		t.Errorf("cancels emitted for leg 1: %q, want none: it had set its bin down", got)
	}
	if got := c.stopsSince(r); len(got) != 0 {
		t.Errorf("fleet stops since the delivery: %v, want none: leg 1's job had finished", got)
	}
	t.Logf("told about leg 1: %v", c.toldAbout(t, r, leg1.EdgeUUID))
	if n := c.ordersSince(t, r); n != 0 {
		t.Errorf("%d order(s) made since the delivery, want none", n)
	}
	if r.d.laneLock.IsLocked(lane.ID) {
		t.Errorf("lane %s is still held after the chapter closed", lane.Name)
	}
	assertRestarted(t, r, parent.ID, fmt.Sprintf(
		"reshuffle dissolved: dig leg %d was cancelled (cancelled by test-operator); the demand re-plans", leg2.ID))
}

// TestDeliveredLeg_ParentCancel: the parent's cancel cascade
// (cancelCompoundChildren), through the wire door (HandleOrderCancel: an Edge's
// cancel, the changeover start's cancel of a digging order). The operator's
// Cancel Order on the orders page is the same cascade (engine.CancelOrder ->
// CancelOrderWithCascade); its post-delivery guard reads the parent, which is
// reshuffling. The cancel lands while leg 1 is delivered and not confirmed.
//
// Final brief 1c: leg 1, which has set its bin down, is kept and the engine's
// confirm lands; the fleet is asked to stop leg 2's job alone; the station is
// told leg 1 confirmed, never cancelled; the parent is cancelled, the lane let
// go and no order made. (Leg 1 was cancelled with the parent's reason and
// cause, its finished job stopped, the station told it cancelled, the confirm
// refused.)
func TestDeliveredLeg_ParentCancel(t *testing.T) {
	t.Parallel()
	r := newTwiceRig(t)
	parent, lane, _, legs := liftedDig(t, r, "DL-PC", 1)
	leg1 := landLegBin(t, r, legs[0])
	testutil.MustNoErr(t, markLegDelivered(r, leg1), "the delivered write")
	r.d.HandleChildOrderComplete(leg1) // the delivery's own advance
	leg2 := reloadLeg(t, r, legs[1].ID)

	c := cutNow(t, r)
	env := &protocol.Envelope{ID: "env-DL-PC", Type: protocol.TypeOrderCancel,
		Src: protocol.Address{Role: protocol.RoleEdge, Station: parent.Requester()},
		Dst: protocol.Address{Role: protocol.RoleCore, Station: "core"}}
	r.d.HandleOrderCancel(env, &protocol.OrderCancel{OrderUUID: parent.EdgeUUID, Reason: "operator abort"})
	confirmErr := autoConfirmLeg(r, leg1)

	assertLegEnded(t, r, leg1.ID, StatusConfirmed, "", deliveredThenConfirmed, leg1.DeliveryNode)
	if confirmErr != nil {
		t.Errorf("the engine's confirm returned %v, want it to land", confirmErr)
	}
	if got := cancelsOf(r.e.mockEmitter, leg1.ID); len(got) != 0 {
		t.Errorf("cancels emitted for leg 1: %q, want none: it had set its bin down", got)
	}
	if got := c.stopsSince(r); !slices.Equal(got, []string{leg2.VendorOrderID}) {
		t.Errorf("fleet stops since the delivery: %v, want leg 2's job %s alone", got, leg2.VendorOrderID)
	}
	if got := reloadLeg(t, r, leg2.ID); got.Status != StatusCancelled {
		t.Errorf("leg 2 is %q, want cancelled by the cascade", got.Status)
	}
	told := c.toldAbout(t, r, leg1.EdgeUUID)
	if len(told) != 1 || !strings.Contains(told[0], `"status":"confirmed"`) {
		t.Errorf("the station was told %v about leg 1, want one confirmed update", told)
	}
	assertTold(t, "what the cancel told the station", toldSince(t, r.db, c.outbox), []string{
		`order.update to test: {"detail":"operator abort","order_uuid":"DL-PC-parent","status":"cancelled","version":3}`,
		`order.update to test: {"detail":"parent order cancelled: operator abort","order_uuid":"<uuid>","status":"cancelled","version":4}`,
		`order.cancelled to test: {"order_uuid":"DL-PC-parent","reason":"operator abort"}`,
		`order.update to test: {"detail":"receipt: auto_confirm_internal, count: 0","order_uuid":"<uuid>","status":"confirmed","version":5}`,
	})
	if got := reloadLeg(t, r, parent.ID); got.Status != StatusCancelled {
		t.Errorf("parent is %q, want cancelled", got.Status)
	}
	if r.d.laneLock.IsLocked(lane.ID) {
		t.Errorf("lane %s is still held after the parent's cancel", lane.Name)
	}
	if n := c.ordersSince(t, r); n != 0 {
		t.Errorf("%d order(s) made since the cancel, want none", n)
	}
}

// TestDeliveredLeg_CutCascade: a cancel the parent already took, delivered
// again (finishCutCascade). Once the inbox row is written after the handler, a
// crash after a whole first run delivers the cancel again; the first run kept
// leg 1 because it was carrying its bin (TestRunTwice_CancelCutInsideTheCascade,
// its last case). Leg 1 then sets its bin down and is delivered, its own advance
// finds the parent cancelled, and the cancel arrives again before the confirm.
//
// Final brief 1c: the repeat finds leg 1 live with its bin set down, so it has
// nothing to cancel and does nothing: no fleet stop, no second answer; the
// engine's confirm lands and the station is told leg 1 confirmed. (It ran the
// cascade again: leg 1 cancelled with the parent's reason, its finished job
// stopped, the station answered order.cancelled a second time.)
func TestDeliveredLeg_CutCascade(t *testing.T) {
	t.Parallel()
	r := newTwiceRig(t)
	parent, _, _, legs := liftedDig(t, r, "DL-CC", 1)
	env := &protocol.Envelope{ID: "env-DL-CC", Type: protocol.TypeOrderCancel,
		Src: protocol.Address{Role: protocol.RoleEdge, Station: parent.Requester()},
		Dst: protocol.Address{Role: protocol.RoleCore, Station: "core"}}
	req := &protocol.OrderCancel{OrderUUID: parent.EdgeUUID, Reason: "operator abort"}
	r.d.HandleOrderCancel(env, req)
	if got := reloadLeg(t, r, legs[0].ID); protocol.IsTerminal(got.Status) {
		t.Fatalf("fixture: the first cancel ended leg 1 (%q); it is carrying its bin and is kept", got.Status)
	}
	leg1 := landLegBin(t, r, reloadLeg(t, r, legs[0].ID))
	testutil.MustNoErr(t, markLegDelivered(r, leg1), "the delivered write")
	r.d.HandleChildOrderComplete(leg1) // the delivery's own advance: the parent is cancelled

	c := cutNow(t, r)
	r.d.HandleOrderCancel(env, req) // the same cancel, delivered again
	confirmErr := autoConfirmLeg(r, leg1)

	assertLegEnded(t, r, leg1.ID, StatusConfirmed, "", deliveredThenConfirmed, leg1.DeliveryNode)
	if confirmErr != nil {
		t.Errorf("the engine's confirm returned %v, want it to land", confirmErr)
	}
	if got := c.stopsSince(r); len(got) != 0 {
		t.Errorf("fleet stops since the repeat: %v, want none: leg 1's job had finished", got)
	}
	assertTold(t, "what the repeat and the confirm told the station", toldSince(t, r.db, c.outbox), []string{
		`order.update to test: {"detail":"receipt: auto_confirm_internal, count: 0","order_uuid":"<uuid>","status":"confirmed","version":5}`,
	})
	if got := reloadLeg(t, r, parent.ID); got.Status != StatusCancelled {
		t.Errorf("parent is %q, want cancelled", got.Status)
	}
	if n := c.ordersSince(t, r); n != 0 {
		t.Errorf("%d order(s) made since the repeat, want none", n)
	}
}

// fourLegDig is the owner's example up to leg 2's set-down: a dig over three
// blockers, four legs (three unburies, then the retrieve). Leg 1 has set its
// blocker down at its parking and confirmed; leg 2 is carrying its blocker,
// dwelling in the lane; leg 3 and the retrieve have not started. It returns
// the parent, the lane, its slots shallowest first and the legs in sequence
// order, reloaded.
func fourLegDig(t *testing.T, r twiceRig, prefix string) (*orders.Order, *nodes.Node, []*nodes.Node, []*orders.Order) {
	t.Helper()
	parent, lane, slots, legs := liftedDig(t, r, prefix, 3)
	leg1 := landLegBin(t, r, legs[0]) // released (the wake sends leg 2), then set down
	r.d.RedriveHeldCompoundLegs(lane.ID)
	testutil.MustNoErr(t, markLegDelivered(r, leg1), "leg 1's delivered write")
	r.d.HandleChildOrderComplete(leg1)
	testutil.MustNoErr(t, autoConfirmLeg(r, leg1), "leg 1's confirm")
	r.d.HandleChildOrderComplete(reloadLeg(t, r, leg1.ID))
	leg2 := reloadLeg(t, r, legs[1].ID)
	if leg2.VendorOrderID == "" {
		t.Fatalf("fixture: leg 2 was not sent at leg 1's release (cause %q)", leg2.QueueCause)
	}
	transit, err := r.db.GetNodeByDotName("_TRANSIT")
	testutil.MustNoErr(t, err, "resolve _TRANSIT")
	liftBin(t, r.db, r.d, leg2, slots[1], transit)
	legs = legsOf(t, r.db, parent.ID)
	if len(legs) != 4 || legs[0].Status != StatusConfirmed || protocol.IsTerminal(legs[1].Status) ||
		protocol.IsTerminal(legs[2].Status) || legs[2].VendorOrderID != "" || legs[3].Status != StatusPending {
		t.Fatalf("fixture: legs %v, want leg 1 confirmed, leg 2 carrying, legs 3 and 4 not started", statusesOf(legs))
	}
	return parent, lane, slots, legs
}

func statusesOf(legs []*orders.Order) []string {
	var out []string
	for _, l := range legs {
		out = append(out, fmt.Sprintf("%d:%s vendor=%q", l.ID, l.Status, l.VendorOrderID))
	}
	return out
}

// assertOwnersExample is the end the owner gave for his example: legs 1 and 2
// confirmed, legs 3 and 4 cancelled, and no cancel sent for leg 2, whose job
// finished when it put its bin down.
func assertOwnersExample(t *testing.T, r twiceRig, c cut, legs []*orders.Order) {
	t.Helper()
	for i, want := range []protocol.Status{StatusConfirmed, StatusConfirmed, StatusCancelled, StatusCancelled} {
		got := reloadLeg(t, r, legs[i].ID)
		t.Logf("record: leg %d (%d) %s (%q) history=%v", i+1, got.ID, got.Status, got.ErrorDetail, historyOf(t, r, got.ID))
		if got.Status != want {
			t.Errorf("leg %d is %q (%q), want %q: a leg that has put its bin down finishes and confirms; "+
				"legs not started, or driving to their pickup, cancel", i+1, got.Status, got.ErrorDetail, want)
		}
	}
	if got := cancelsOf(r.e.mockEmitter, legs[1].ID); len(got) != 0 {
		t.Errorf("cancels emitted for leg 2: %q, want none", got)
	}
	if slices.Contains(c.stopsSince(r), legs[1].VendorOrderID) {
		t.Errorf("the fleet was asked to stop leg 2's job %s, which finished when it set its bin down (stops %v)",
			legs[1].VendorOrderID, c.stopsSince(r))
	}
}

// TestDeliveredLeg_FourLegDigClosedAsLeg2SetsDown is the owner's example (final
// brief 1c): "a leg is finished when it puts its bin down, and it
// auto-confirms; legs not started, or driving to their pickup, all cancel; a
// leg carrying its bin sets it down first". A four-leg dig closed as leg 2
// sets down ends with legs 1 and 2 confirmed and legs 3 and 4 cancelled, at
// each door that can close a dig then: the parent's cascade, the dissolve,
// the chapter end and the cut cascade. Two doors cannot reach it: the failure
// door's failed leg ends failed, not cancelled (TestDeliveredLeg_FailureDoor
// keeps its set-down sibling), and a home clear's end has its own two legs
// (TestDeliveredLeg_EndClear).
func TestDeliveredLeg_FourLegDigClosedAsLeg2SetsDown(t *testing.T) {
	t.Parallel()
	cancelEnv := func(parent *orders.Order) *protocol.Envelope {
		return &protocol.Envelope{ID: "env-" + parent.EdgeUUID, Type: protocol.TypeOrderCancel,
			Src: protocol.Address{Role: protocol.RoleEdge, Station: parent.Requester()},
			Dst: protocol.Address{Role: protocol.RoleCore, Station: "core"}}
	}

	// A station cancels the dig while leg 2's bin is down at its parking and
	// its delivered write has not landed: the landed half, which still reads
	// in_transit. Leg 3 is driving to its pickup.
	t.Run("the parent's cascade, leg 2 landed", func(t *testing.T) {
		t.Parallel()
		r := newTwiceRig(t)
		parent, lane, _, legs := fourLegDig(t, r, "FL-PC")
		leg2 := landLegBin(t, r, legs[1]) // released (the wake sends leg 3), then set down
		r.d.RedriveHeldCompoundLegs(lane.ID)
		if got := reloadLeg(t, r, legs[2].ID); got.VendorOrderID == "" {
			t.Fatalf("fixture: leg 3 was not sent at leg 2's release (cause %q)", got.QueueCause)
		}

		c := cutNow(t, r)
		r.d.HandleOrderCancel(cancelEnv(parent), &protocol.OrderCancel{OrderUUID: parent.EdgeUUID, Reason: "operator abort"})
		if err := markLegDelivered(r, leg2); err != nil {
			t.Errorf("leg 2's delivered write after the cancel: %v", err)
		}
		if err := autoConfirmLeg(r, leg2); err != nil {
			t.Errorf("leg 2's confirm after the cancel: %v", err)
		}

		legs = legsOf(t, r.db, parent.ID)
		assertOwnersExample(t, r, c, legs)
		if got := c.stopsSince(r); !slices.Equal(got, []string{legs[2].VendorOrderID}) {
			t.Errorf("fleet stops since the cancel: %v, want leg 3's job %s alone", got, legs[2].VendorOrderID)
		}
		if got := reloadLeg(t, r, parent.ID); got.Status != StatusCancelled {
			t.Errorf("parent is %q, want cancelled", got.Status)
		}
		if r.d.laneLock.IsLocked(lane.ID) {
			t.Errorf("lane %s is still held after the parent's cancel", lane.Name)
		}
	})

	// Leg 3's pickup is walled by a bin whose claimant has stopped moving, and
	// leg 2's landing re-asks it: the plan went stale. The dissolve keeps leg 2
	// and, with it, the lane and the parent until its confirm.
	t.Run("the dissolve, leg 2 landed", func(t *testing.T) {
		t.Parallel()
		r := newTwiceRig(t)
		parent, lane, slots, legs := fourLegDig(t, r, "FL-DIS")
		wall := testdb.CreateBinAtNode(t, r.db, parent.PayloadCode, slots[1].ID, "FL-DIS-WALL")
		hauler := testdb.CreateOrder(t, r.db, func(o *orders.Order) {
			o.EdgeUUID = "FL-DIS-hauler"
			o.Status, o.VendorOrderID = protocol.StatusDispatched, "sg-FL-DIS-hauler"
		})
		testdb.ClaimBinInDugLaneForTest(t, r.db, wall.ID, hauler.ID)
		leg2 := releaseDwell(t, r.d, r.db, legs[1])
		if got := reloadLeg(t, r, legs[2].ID); got.VendorOrderID != "" || QueueCause(got.QueueCause) != CauseLaneTargetBuried {
			t.Fatalf("fixture: leg 3 after leg 2's release: vendor=%q cause=%q, want it held unsent behind the wall",
				got.VendorOrderID, got.QueueCause)
		}
		stallOrder(t, r, hauler.ID)
		leg2 = landLegBin(t, r, leg2)

		c := cutNow(t, r)
		r.d.RedriveHeldCompoundLegs(lane.ID) // the landing's re-drive: leg 3's plan is stale
		if !r.d.laneLock.IsLocked(lane.ID) {
			t.Errorf("the dissolve let lane %s go before leg 2 confirmed", lane.Name)
		}
		if got := reloadLeg(t, r, parent.ID); got.Status != StatusReshuffling {
			t.Errorf("parent is %q before leg 2 confirmed, want reshuffling", got.Status)
		}
		if err := markLegDelivered(r, leg2); err != nil {
			t.Errorf("leg 2's delivered write after the dissolve: %v", err)
		}
		if err := autoConfirmLeg(r, leg2); err != nil {
			t.Errorf("leg 2's confirm after the dissolve: %v", err)
		}
		testutil.MustNoErr(t, r.d.AdvanceCompoundOrder(parent.ID), "the confirm's re-drive")

		legs = legsOf(t, r.db, parent.ID)
		assertOwnersExample(t, r, c, legs)
		for _, l := range legs[2:] {
			if l.ErrorDetail != reshuffleDissolveDetail {
				t.Errorf("leg %d is %q (%q), want cancelled under the dissolve marker", l.ID, l.Status, l.ErrorDetail)
			}
		}
		if got := c.stopsSince(r); len(got) != 0 {
			t.Errorf("fleet stops since the landing: %v, want none: leg 3 was never sent and leg 2's job finished", got)
		}
		if r.d.laneLock.IsLocked(lane.ID) {
			t.Errorf("lane %s is still held after leg 2 confirmed and the chapter closed", lane.Name)
		}
		assertRestarted(t, r, parent.ID, reshuffleDissolveDetail)
	})

	// A person ends the retrieve from the orders page as leg 2 sets down (a
	// cancel with no chapter-end marker, which re-drives nothing); leg 2's own
	// delivery is the chapter end. Leg 3 is driving to its pickup.
	t.Run("the chapter end, leg 2 delivered", func(t *testing.T) {
		t.Parallel()
		r := newTwiceRig(t)
		parent, lane, _, legs := fourLegDig(t, r, "FL-CE")
		leg2 := landLegBin(t, r, legs[1]) // released (the wake sends leg 3), then set down
		r.d.RedriveHeldCompoundLegs(lane.ID)
		if got := reloadLeg(t, r, legs[2].ID); got.VendorOrderID == "" {
			t.Fatalf("fixture: leg 3 was not sent at leg 2's release (cause %q)", got.QueueCause)
		}
		r.d.CancelOrderWithCascade(legs[3], legs[3].Requester(), "cancelled by test-operator",
			CancelCause{Code: protocol.TermOperatorCancelled, Actor: "test-operator"})
		testutil.MustNoErr(t, markLegDelivered(r, leg2), "leg 2's delivered write")

		c := cutNow(t, r)
		r.d.HandleChildOrderComplete(leg2) // the delivery's own advance: the chapter end
		if err := autoConfirmLeg(r, leg2); err != nil {
			t.Errorf("leg 2's confirm after the chapter end: %v", err)
		}
		testutil.MustNoErr(t, r.d.AdvanceCompoundOrder(parent.ID), "the confirm's re-drive")

		legs = legsOf(t, r.db, parent.ID)
		assertOwnersExample(t, r, c, legs)
		if legs[2].ErrorDetail != reshuffleLegFailedDetail {
			t.Errorf("leg 3 is %q (%q), want cancelled under the chapter-end marker", legs[2].Status, legs[2].ErrorDetail)
		}
		if got := c.stopsSince(r); !slices.Equal(got, []string{legs[2].VendorOrderID}) {
			t.Errorf("fleet stops since the delivery: %v, want leg 3's job %s alone", got, legs[2].VendorOrderID)
		}
		if r.d.laneLock.IsLocked(lane.ID) {
			t.Errorf("lane %s is still held after the chapter closed", lane.Name)
		}
		assertRestarted(t, r, parent.ID, fmt.Sprintf(
			"reshuffle dissolved: dig leg %d was cancelled (cancelled by test-operator); the demand re-plans", legs[3].ID))
	})

	// A station cancels the dig while leg 2 carries its bin: leg 2 is kept and
	// legs 3 and 4 go. Leg 2 sets its bin down and is delivered, and the same
	// cancel comes again (a redelivery, or a second press) before the confirm.
	t.Run("the cut cascade, leg 2 delivered", func(t *testing.T) {
		t.Parallel()
		r := newTwiceRig(t)
		parent, _, _, legs := fourLegDig(t, r, "FL-CC")
		req := &protocol.OrderCancel{OrderUUID: parent.EdgeUUID, Reason: "operator abort"}
		r.d.HandleOrderCancel(cancelEnv(parent), req)
		if got := reloadLeg(t, r, legs[1].ID); protocol.IsTerminal(got.Status) {
			t.Fatalf("fixture: the first cancel ended leg 2 (%q); it is carrying its bin and is kept", got.Status)
		}
		leg2 := landLegBin(t, r, reloadLeg(t, r, legs[1].ID))
		testutil.MustNoErr(t, markLegDelivered(r, leg2), "leg 2's delivered write")
		r.d.HandleChildOrderComplete(leg2) // the delivery's own advance: the parent is cancelled

		c := cutNow(t, r)
		r.d.HandleOrderCancel(cancelEnv(parent), req) // the same cancel, delivered again
		if err := autoConfirmLeg(r, leg2); err != nil {
			t.Errorf("leg 2's confirm after the repeat: %v", err)
		}

		legs = legsOf(t, r.db, parent.ID)
		assertOwnersExample(t, r, c, legs)
		if got := c.stopsSince(r); len(got) != 0 {
			t.Errorf("fleet stops since the repeat: %v, want none", got)
		}
		for _, m := range toldSince(t, r.db, c.outbox) {
			if strings.HasPrefix(m, protocol.TypeOrderCancelled) {
				t.Errorf("the repeat answered the station again: %s", m)
			}
		}
	})
}
