//go:build docker

package engine

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// lane_burial_claim_clear_redrive_docker_test.go — the burial guard's named
// releaser, at the level where a releaser is a thing that exists.
//
// The guard refuses a store slot in front of a hard-claimed bin. Two tests
// already pin the halves of that on their own:
//
//   - store/lane_queries_burial_test.go TestBurialGuard_ClearedClaimReopensTheLane
//     — the refusal is a pure function of bins.claimed_by, so the instant the
//     claim goes the lane is open again. No retry counter, no state to unwind.
//     Its own doc says "the event that carries this to a parked order is
//     asserted separately"; this file is that.
//   - dispatch/binresolver/group_resolver_burial_test.go
//     TestBurialDivert_AllLanesClosedParksUnderTheExistingShape — when EVERY
//     lane refuses there is nowhere to divert to and the group parks the order.
//
// Neither says what un-parks it. A refusal with no named releaser is a wedge,
// and "it is a pure function of live state" is only half an argument — something
// still has to make the system LOOK at that state again. This asks which:
// the claim-clear EVENT, or the 60-second periodic sweep.
//
// It must be the event, for the same reason it must be for lane occupancy
// (lane_unification_redrive_docker_test.go, whose shape this follows). A hard
// claim is written immediately before the fleet call and cleared at the end of
// the job; a store that arrives while one is outstanding would otherwise sit for
// up to a minute after the lane reopened, on the highest-traffic path in the
// system, with a slot standing empty. The assertion window below is seconds
// against that 60s backstop, so a pass is attributable to the subscription and
// not to the ticker.

// closedGroup builds an NGRP with n LANEs of two depth-ordered slots each, and
// hard-claims a bin in every lane's DEEP slot — the state a bin is in between
// ConfirmForDispatch and arrival, which is exactly what the guard respects.
//
// Geometry: with the deep slot claimed, the only remaining slot in each lane is
// IN FRONT of a bin a robot is already en route to, so every lane closes and the
// group has nothing to offer. Two lanes rather than one because the row is
// "ALL candidate lanes carry hard claims" — one lane cannot distinguish a closed
// group from a group with a closed lane and no sibling.
//
// Returns the group, the lanes, and the claim-holding order per lane.
func closedGroup(t *testing.T, db *store.DB, prefix, payloadCode string, n int) (grp *nodes.Node, lanes []*nodes.Node, holders []*orders.Order) {
	t.Helper()
	grpType, err := db.GetNodeTypeByCode(protocol.NodeClassNGRP)
	testutil.MustNoErr(t, err, "get NGRP type")
	laneType, err := db.GetNodeTypeByCode(protocol.NodeClassLANE)
	testutil.MustNoErr(t, err, "get LANE type")

	grp = &nodes.Node{Name: prefix + "-GRP", NodeTypeID: &grpType.ID, Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(grp), "create group")

	for i := 1; i <= n; i++ {
		lane := &nodes.Node{
			Name: fmt.Sprintf("%s-L%d", prefix, i), NodeTypeID: &laneType.ID,
			ParentID: &grp.ID, Enabled: true, IsSynthetic: true,
		}
		testutil.MustNoErr(t, db.CreateNode(lane), "create lane")
		var deep *nodes.Node
		for d := 1; d <= 2; d++ {
			depth := d
			s := &nodes.Node{Name: fmt.Sprintf("%s-L%d-S%d", prefix, i, d), ParentID: &lane.ID, Enabled: true, Depth: &depth}
			testutil.MustNoErr(t, db.CreateNode(s), "create slot")
			deep = s
		}
		bin := testdb.CreateBinAtNode(t, db, payloadCode, deep.ID, fmt.Sprintf("%s-DEEP-%d", prefix, i))
		holder := testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.Status = "in_transit"
			o.EdgeUUID = fmt.Sprintf("%s-holder-%d", strings.ToLower(prefix), i)
		})
		testdb.ClaimBinForTest(t, db, bin.ID, holder.ID)

		lanes = append(lanes, lane)
		holders = append(holders, holder)
	}
	return grp, lanes, holders
}

// requireLaneClosedByClaim asserts the guard is what is refusing this lane —
// not a full lane, not an unreachable deep slot. Both of those refuse too, and
// a fixture that drifted into one of them would park the order for a reason
// that has nothing to do with claims, leaving the release below proving nothing.
func requireLaneClosedByClaim(t *testing.T, db *store.DB, lane *nodes.Node) {
	t.Helper()
	got, err := db.FindStoreSlotInLane(lane.ID)
	if err == nil {
		t.Fatalf("lane %s offered slot %s — the guard is not refusing it and this test is vacuous",
			lane.Name, got.Name)
	}
	if !errors.Is(err, nodes.ErrLaneClosedByClaim) {
		t.Fatalf("lane %s refused with %v, want ErrLaneClosedByClaim — a full lane or a stranded "+
			"deep slot refuses as well, and neither of those reopens when a claim clears", lane.Name, err)
	}
}

// TestBurialGuard_ClaimClearEventRedrivesAParkedStore.
//
// A store into a group whose every lane is closed by a hard claim. It parks —
// the group has nowhere to put the bin and the divert has no sibling left to
// divert to. Then ONE claim holder is cancelled, which releases its claim in the
// SAME transaction as the status write (store/orders.go TerminalizeOrder →
// `UPDATE bins SET claimed_by=NULL`), and the terminal event re-drives the
// scanner.
//
// TWO LANES, ONE RELEASE, and the second lane is the attribution. It stays
// closed throughout and is re-asserted closed after the release, so the only
// place the order can land is the lane whose claim went — which is checked, by
// name, on the dropoff the order dispatched with. A test that only asserted
// "dispatched" would also pass if the group had quietly found room somewhere the
// guard never refused.
//
// The parked shape is asserted as a precondition rather than assumed: `queued`
// with the ngrp-resolve cause and the GROUP's name in the operator sentence. If
// the order dispatched at intake the release proves nothing, and if it parked
// under some other cause it is not this guard's park.
//
// MUTATION A (verified): remove EventOrderCancelled from the triggerFulfillment
// subscriptions in wiring.go. The order stays queued and this test times out —
// the sweep would eventually take it, 60 seconds later. (Shared with
// TestUnification_LaneClearingEventRedrivesAParkedPlainOrder: one subscription
// carries both releasers, occupancy and claim.)
//
// MUTATION B (verified): delete the `UPDATE bins SET claimed_by=NULL` statement
// from TerminalizeOrder (store/orders.go). It fires on the FIXTURE check between
// the terminal and the emit — "lane BGRD-L1 is still refusing after its claim
// cleared" — rather than on the wait, and that is the right place for it: the
// lane never reopened, so an eventual timeout would report a missing event when
// what actually went missing is the state the event announces. That is the half
// MUTATION A cannot reach; the two failures are deliberately distinguishable.
func TestBurialGuard_ClaimClearEventRedrivesAParkedStore(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	grp, lanes, holders := closedGroup(t, db, "BGRD", sd.Payload.Code, 2)
	for _, l := range lanes {
		requireLaneClosedByClaim(t, db, l)
	}

	// The bin the store carries, at the line it is being pushed out of.
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.LineNode.ID, "BGRD-STORED")

	eng := newTestEngine(t, db, simulator.New())
	// Intake runs the scanner synchronously (EventOrderQueued → RunOnce), so the
	// order has already been offered the group once by the time this returns.
	eng.Dispatcher().HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID:   "bgrd-store",
		PayloadCode: sd.Payload.Code,
		Quantity:    1,
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: sd.LineNode.Name},
			{Action: "dropoff", Node: grp.Name},
		},
	})

	parked, err := db.GetOrderByUUID("bgrd-store")
	if err != nil || parked == nil {
		t.Fatalf("reload parked order: %v", err)
	}
	if parked.Status != protocol.StatusQueued {
		t.Fatalf("status = %q, want queued — the store found room in a group whose every lane is "+
			"closed by a claim, so the guard is not reaching this path and the rest of this test "+
			"is vacuous", parked.Status)
	}
	if parked.QueueCause != "ngrp-resolve" {
		t.Errorf("queue_cause = %q, want ngrp-resolve", parked.QueueCause)
	}
	if !strings.Contains(parked.QueueReason, grp.Name) {
		t.Errorf("queue_reason = %q, want the group's name in it — the operator sentence is built "+
			"from the resolver's message and the classifier reads the group out of it by substring",
			parked.QueueReason)
	}

	// ── One claim clears ──────────────────────────────────────────────────
	// Cancelling releases the holder's claim in the same transaction as the
	// status write; the event is what tells the scanner to look again.
	if _, err := db.TerminalizeOrder(holders[0].ID, protocol.StatusCancelled, "test: claim holder cancelled"); err != nil {
		t.Fatalf("terminalize claim holder: %v", err)
	}
	if _, err := db.FindStoreSlotInLane(lanes[0].ID); err != nil {
		t.Fatalf("fixture: lane %s is still refusing after its claim cleared (%v) — TerminalizeOrder "+
			"should have released claimed_by in its own transaction", lanes[0].Name, err)
	}
	// The sibling stays shut, so there is exactly one place this order can go.
	requireLaneClosedByClaim(t, db, lanes[1])

	eng.Events.Emit(Event{Type: EventOrderCancelled, Payload: OrderCancelledEvent{
		OrderID:  holders[0].ID,
		EdgeUUID: holders[0].EdgeUUID,
	}})

	// Seconds, against a 60s sweep: this is the event's doing.
	released := waitForStatus(t, db, "bgrd-store", protocol.StatusDispatched, 10*time.Second)

	if !strings.HasPrefix(released.DeliveryNode, lanes[0].Name+"-") {
		t.Errorf("dropoff = %q, want a slot in %s — the reopened lane is the only one the guard "+
			"stopped refusing, so a bin landing anywhere else means the order was re-driven by "+
			"something other than the claim clearing", released.DeliveryNode, lanes[0].Name)
	}
	// The park sentence has to go, and it is POLLED rather than read once: the
	// status write lands inside the fleet handover and the queue-reason clear
	// comes after it, so a reader that arrives on `dispatched` can genuinely be
	// looking at a row mid-dispatch. Reading it once was flaky in exactly that
	// window. What is not acceptable is the sentence SURVIVING — a live job
	// showing the operator a stale wait — so the poll is bounded and the failure
	// says which of the two it is.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if o, rErr := db.GetOrderByUUID("bgrd-store"); rErr == nil && o != nil && o.QueueReason == "" {
			released = o
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if released.QueueReason != "" {
		t.Errorf("queue_reason = %q, still set 5s after dispatch — a dispatched order carrying its "+
			"park sentence shows the operator a stale wait on a live job", released.QueueReason)
	}

	// END-OF-SCENARIO LEDGER SWEEP. The cancelled holder must be left holding
	// nothing — not the bin claim this test watched, and not the confirmed
	// reservation underneath it, which is the half that would keep the bin
	// unfindable while the lane read open. See testdb.AssertNoOrphanedHolds.
	testdb.AssertNoOrphanedHolds(t, db)
}
