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

// delivered_leg_docker_test.go: brief C item 5 (docket R87), data only.
//
// CHARACTERISATION FOR BRIEF C ITEM 5, NOT A RULING. Each test runs one of the
// doors that closes a dig's chapter by cancelling its live legs (the callers of
// splitLiftedLegs) while a leg has set its bin down and is not yet confirmed,
// and asserts what the door does today: the leg's end, its bin's row, the
// events, what reaches the fleet and the station, any order made, and where the
// chapter and the parent go. Where today's answer looks wrong it is still what
// is asserted. The orc and the owner rule on it; a change that fails one of
// these changes the behaviour it records, and the test is re-read against that
// ruling, never loosened to pass.
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

// The statuses a leg passes through: cancelled after its bin landed but before
// its delivered write, cancelled after it, or confirmed.
var (
	landedThenCancelled    = []protocol.Status{StatusPending, StatusDispatched, StatusInTransit, StatusCancelled}
	deliveredThenCancelled = []protocol.Status{StatusPending, StatusDispatched, StatusInTransit, StatusDelivered, StatusCancelled}
	deliveredThenConfirmed = []protocol.Status{StatusPending, StatusDispatched, StatusInTransit, StatusDelivered, StatusConfirmed}
)

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
	// the lane it left, before the delivered write. Recorded: leg 1 is
	// cancelled under the dissolve marker while it reads in_transit, the
	// delivered write and the confirm are both refused, the fleet is asked to
	// stop leg 1's job (which has finished), and the demand re-plans.
	t.Run("landed, not yet marked delivered", func(t *testing.T) {
		t.Parallel()
		r := newTwiceRig(t)
		parent, lane, slots, legs := liftedDig(t, r, "DL-DIS-L", 1)
		leg1, hauler := heldBehindAClaimedWall(t, r, parent, slots, legs)
		stallOrder(t, r, hauler.ID)
		leg1 = landLegBin(t, r, leg1)

		c := cutNow(t, r)
		r.d.RedriveHeldCompoundLegs(lane.ID) // the landing's lane-gate re-drive
		deliveredErr := markLegDelivered(r, leg1)
		confirmErr := autoConfirmLeg(r, leg1)
		testutil.MustNoErr(t, r.d.AdvanceCompoundOrder(parent.ID), "the dissolve marker's re-drive")

		assertLegEnded(t, r, leg1.ID, StatusCancelled, reshuffleDissolveDetail, landedThenCancelled, leg1.DeliveryNode)
		assertDissolved(t, r, c, parent, lane, leg1, legs[1])
		if !IsConcurrentTransition(deliveredErr) || !IsIllegalTransition(confirmErr) {
			t.Errorf("the engine's delivered write returned %v and its confirm %v, want a concurrent "+
				"transition and an illegal in_transit -> confirmed", deliveredErr, confirmErr)
		}
		if n := completionsOf(r.e.mockEmitter, leg1.ID); n != 0 {
			t.Errorf("leg 1 announced completed %d times, want 0: neither of its writes landed", n)
		}
	})

	// Leg 1 lands and is delivered while the hauler still moves, so both its
	// landing and its delivery re-ask leg 2 and hold it. Core then stops before
	// the confirm, so leg 1 stays delivered. The hauler stops; the next to
	// re-ask leg 2 is the lane's floor (lane_floor.go) or any event in the lane.
	// Recorded: leg 1 is cancelled under the dissolve marker from delivered,
	// the fleet is asked to stop its finished job, and the demand re-plans.
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

		assertLegEnded(t, r, leg1.ID, StatusCancelled, reshuffleDissolveDetail, deliveredThenCancelled, leg1.DeliveryNode)
		assertDissolved(t, r, c, parent, lane, leg1, legs[1])
		if n := completionsOf(r.e.mockEmitter, leg1.ID); n != 1 {
			t.Errorf("leg 1 announced completed %d times, want 1: its delivered write", n)
		}
	})
}

// assertDissolved is what both halves of the dissolve record past the leg: the
// dissolve marker emitted for leg 1, a fleet stop for its job, its station told
// it cancelled, leg 2 cancelled too, the lane let go, no order made, and the
// parent back at pending after the marker's re-drive.
func assertDissolved(t *testing.T, r twiceRig, c cut, parent *orders.Order, lane *nodes.Node, leg1, leg2 *orders.Order) {
	t.Helper()
	if got := cancelsOf(r.e.mockEmitter, leg1.ID); !slices.Equal(got, []string{reshuffleDissolveDetail}) {
		t.Errorf("cancels emitted for leg 1: %q, want the dissolve marker once", got)
	}
	if got := c.stopsSince(r); !slices.Equal(got, []string{leg1.VendorOrderID}) {
		t.Errorf("fleet stops since the landing: %v, want leg 1's job %s alone", got, leg1.VendorOrderID)
	}
	told := c.toldAbout(t, r, leg1.EdgeUUID)
	t.Logf("told about leg 1: %v", told)
	if len(told) != 1 || !strings.Contains(told[0], `"status":"cancelled"`) {
		t.Errorf("the station was told %v about leg 1, want one cancelled update", told)
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
	// lane, so leg 1's landing re-drives it, before the delivered write: the
	// door's IsPostDelivery filter reads in_transit and lets leg 1 through, and
	// leg 1 is not carrying, so it is cancelled. Recorded: leg 1 cancelled with
	// the leg-failed marker, both engine writes after it refused, the fleet
	// asked to stop its finished job, and the order failed on the config fault
	// once the chapter closes.
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
		testutil.MustNoErr(t, r.d.AdvanceCompoundOrder(parent.ID), "the leg-failed marker's re-drive")

		assertLegEnded(t, r, leg1.ID, StatusCancelled, reshuffleLegFailedDetail, landedThenCancelled, leg1.DeliveryNode)
		if !IsConcurrentTransition(deliveredErr) || !IsIllegalTransition(confirmErr) {
			t.Errorf("the engine's delivered write returned %v and its confirm %v, want a concurrent "+
				"transition and an illegal in_transit -> confirmed", deliveredErr, confirmErr)
		}
		if got := c.stopsSince(r); !slices.Equal(got, []string{leg1.VendorOrderID}) {
			t.Errorf("fleet stops since the landing: %v, want leg 1's job %s alone", got, leg1.VendorOrderID)
		}
		assertParentFailedOnConfig(t, r, parent, failed)
	})

	// Leg 2's pickup slot is renamed. The leg no longer names a slot of the
	// lane, so leg 1's landing does not reach it; the delivery's own advance
	// does, with leg 1 delivered. Recorded: the filter keeps leg 1 and the
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
// orders page (engine.TerminateOrder's cascade; a fleet stop is the same kind
// of cancel), which re-drives nothing (no marker). Leg 1 delivers, and its own
// advance is the chapter end.
//
// Recorded: the delivery's advance cancels leg 1 with the leg-failed marker,
// the fleet is asked to stop its finished job, the engine's confirm loses its
// compare-and-swap, and the marker's re-drive returns the parent to pending
// with the bare leg-failed sentence, though no leg failed.
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
	testutil.MustNoErr(t, r.d.AdvanceCompoundOrder(parent.ID), "the leg-failed marker's re-drive")

	assertLegEnded(t, r, leg1.ID, StatusCancelled, reshuffleLegFailedDetail, deliveredThenCancelled, leg1.DeliveryNode)
	if !IsConcurrentTransition(confirmErr) {
		t.Errorf("the engine's confirm returned %v, want a concurrent transition", confirmErr)
	}
	if got := cancelsOf(r.e.mockEmitter, leg1.ID); !slices.Equal(got, []string{reshuffleLegFailedDetail}) {
		t.Errorf("cancels emitted for leg 1: %q, want the leg-failed marker once", got)
	}
	if got := c.stopsSince(r); !slices.Equal(got, []string{leg1.VendorOrderID}) {
		t.Errorf("fleet stops since the delivery: %v, want leg 1's job %s alone", got, leg1.VendorOrderID)
	}
	t.Logf("told about leg 1: %v", c.toldAbout(t, r, leg1.EdgeUUID))
	if n := c.ordersSince(t, r); n != 0 {
		t.Errorf("%d order(s) made since the delivery, want none", n)
	}
	if r.d.laneLock.IsLocked(lane.ID) {
		t.Errorf("lane %s is still held after the chapter closed", lane.Name)
	}
	assertRestarted(t, r, parent.ID, reshuffleLegFailedDetail)
}

// TestDeliveredLeg_ParentCancel: the parent's cancel cascade
// (cancelCompoundChildren), through the wire door (HandleOrderCancel: an Edge's
// cancel, the changeover start's cancel of a digging order). The operator's
// Terminate on the orders page is the same cascade (engine.TerminateOrder ->
// CancelOrderWithCascade); its post-delivery guard reads the parent, which is
// reshuffling. The cancel lands while leg 1 is delivered and not confirmed.
//
// Recorded: leg 1 is cancelled with the parent's reason and cause, the fleet
// is asked to stop its finished job and leg 2's, the station is told leg 1
// cancelled, the engine's confirm loses its compare-and-swap, the lane is let
// go and no order is made.
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

	assertLegEnded(t, r, leg1.ID, StatusCancelled, "parent order cancelled: operator abort", deliveredThenCancelled,
		leg1.DeliveryNode)
	if !IsConcurrentTransition(confirmErr) {
		t.Errorf("the engine's confirm returned %v, want a concurrent transition", confirmErr)
	}
	if got := cancelsOf(r.e.mockEmitter, leg1.ID); !slices.Equal(got, []string{"parent order cancelled: operator abort"}) {
		t.Errorf("cancels emitted for leg 1: %q, want the parent's reason once", got)
	}
	if got := c.stopsSince(r); !slices.Equal(got, []string{leg1.VendorOrderID, leg2.VendorOrderID}) {
		t.Errorf("fleet stops since the delivery: %v, want leg 1's finished job %s and leg 2's %s",
			got, leg1.VendorOrderID, leg2.VendorOrderID)
	}
	if got := reloadLeg(t, r, leg2.ID); got.Status != StatusCancelled {
		t.Errorf("leg 2 is %q, want cancelled by the cascade", got.Status)
	}
	told := c.toldAbout(t, r, leg1.EdgeUUID)
	if len(told) != 1 || !strings.Contains(told[0], `"status":"cancelled"`) {
		t.Errorf("the station was told %v about leg 1, want one cancelled update", told)
	}
	assertTold(t, "what the cancel told the station", toldSince(t, r.db, c.outbox), []string{
		`order.update to test: {"detail":"operator abort","order_uuid":"DL-PC-parent","status":"cancelled","version":3}`,
		`order.update to test: {"detail":"parent order cancelled: operator abort","order_uuid":"<uuid>","status":"cancelled","version":5}`,
		`order.update to test: {"detail":"parent order cancelled: operator abort","order_uuid":"<uuid>","status":"cancelled","version":4}`,
		`order.cancelled to test: {"order_uuid":"DL-PC-parent","reason":"operator abort"}`,
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
// Recorded: the repeat finds leg 1 live and not carrying, so it runs the
// cascade again: leg 1 is cancelled with the parent's reason, the fleet is
// asked to stop its finished job, and the station is answered a second time.
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

	assertLegEnded(t, r, leg1.ID, StatusCancelled, "parent order cancelled: operator abort", deliveredThenCancelled,
		leg1.DeliveryNode)
	if !IsConcurrentTransition(confirmErr) {
		t.Errorf("the engine's confirm returned %v, want a concurrent transition", confirmErr)
	}
	if got := c.stopsSince(r); !slices.Equal(got, []string{leg1.VendorOrderID}) {
		t.Errorf("fleet stops since the repeat: %v, want leg 1's job %s alone", got, leg1.VendorOrderID)
	}
	assertTold(t, "what the repeat told the station", toldSince(t, r.db, c.outbox), []string{
		`order.update to test: {"detail":"parent order cancelled: operator abort","order_uuid":"<uuid>","status":"cancelled","version":5}`,
		`order.cancelled to test: {"order_uuid":"DL-CC-parent","reason":"operator abort"}`,
	})
	if got := reloadLeg(t, r, parent.ID); got.Status != StatusCancelled {
		t.Errorf("parent is %q, want cancelled", got.Status)
	}
	if n := c.ordersSince(t, r); n != 0 {
		t.Errorf("%d order(s) made since the repeat, want none", n)
	}
}
