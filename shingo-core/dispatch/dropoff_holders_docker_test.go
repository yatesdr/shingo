//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/reservations"
)

// dropoff_holders_docker_test.go — the dropoff gate counts orders that HOLD a
// claimed bin bound for a node, not orders in a status. Helpers are
// pair_rule_helpers_docker_test.go's.

// ── census 17: two born-`sourcing` orders to one exclusive dropoff ──────────

// TestDropoffGate_TwoShoppersForOneExclusiveDropoffOneGoes: two complex orders,
// each able to source, both finishing at the same declared-exclusive staging
// node. The gate exists to let exactly one of them go at a time. It must not let
// NEITHER go.
//
// DEFECT PIN. Fails at bcbde0d2: both are born `sourcing`, InFlightForDropoffSQL
// counts every non-terminal order except `queued`, so each counts the other as
// already on its way and both park on dropoff-inflight with nothing that will
// ever release either (ochre-marten F3; SYNTH B2). Before the birth-rung move the
// `queued` exclusion was the tie-break; the fix counts only what an order
// physically holds.
func TestDropoffGate_TwoShoppersForOneExclusiveDropoffOneGoes(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	srcX := prNode(t, db, "PR17-SRC-X")
	srcY := prNode(t, db, "PR17-SRC-Y")
	stage := prNode(t, db, "PR17-STAGE")
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, srcX.ID, "PR17-X")
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, srcY.ID, "PR17-Y")

	for _, s := range []struct{ uuid, src string }{{"pr17-x", srcX.Name}, {"pr17-y", srcY.Name}} {
		d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
			OrderUUID: s.uuid, PayloadCode: sd.Payload.Code, Quantity: 1,
			Steps: []protocol.ComplexOrderStep{prPick(s.src), prDropExcl(stage.Name)},
		})
	}
	prScanPass(t, d, db, "pr17-x", "pr17-y")

	x, y := prReloadUUID(t, db, "pr17-x"), prReloadUUID(t, db, "pr17-y")
	went, waited := x, y
	if x.VendorOrderID == "" {
		went, waited = y, x
	}
	if went.VendorOrderID == "" {
		t.Fatalf("NEITHER order went to %s: x %q/%q, y %q/%q. Two shoppers each counted the other as "+
			"already on its way — a mutual wait with no tie-break and no releaser",
			stage.Name, x.Status, x.QueueCause, y.Status, y.QueueCause)
	}
	if waited.VendorOrderID != "" {
		t.Fatalf("BOTH orders went to the one exclusive node %s — the gate let two carriers at one slot",
			stage.Name)
	}
	if waited.QueueCode != string(protocol.QueueWaitingForSlot) || !protocol.IsAcquiring(waited.Status) {
		t.Errorf("the second order is %q under %q/%q, want an acquiring order waiting for the slot",
			waited.Status, waited.QueueCode, waited.QueueCause)
	}
	prAssertHoldsNothing(t, db, waited, "the order refused at the dropoff gate")
}

// TestDropoffGate_APlanIsNotAHolder pins the other half of the holder count: a
// reservation is a plan, and an order that has only planned is not bringing a bin.
// The gate refuses a second carrier once the first order has CLAIMED its bin —
// not while it merely holds a reservation on it.
//
// COVERAGE PIN for the holder re-home. MUTATION: count a pending reservation as a
// holder — the gate refuses behind a plan, which is the two-shoppers wait again.
func TestDropoffGate_APlanIsNotAHolder(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	stage := prNode(t, db, "PRPLAN-STAGE")
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "PRPLAN-BIN")
	planner := prLegRow(t, db, "prplan-a", "", "", stage.Name, sd.StorageNode.Name, sd.Payload.Code,
		prResolved(prPick(sd.StorageNode.Name), prDropExcl(stage.Name)))
	testutil.MustNoErr(t, reservations.Acquire(db.DB, planner.ID, planner.ID, bin.ID, "test"), "reserve")

	if blocked, block := CheckDropoffCapacity(db, stage.Name, 0); blocked {
		t.Fatalf("the gate refused %s (%s) behind an order that has only RESERVED its bin", stage.Name, block.Cause)
	}
	// The plan becomes a hold: ClaimBinForTest reserves and claims in one step,
	// so the bare reservation goes first.
	testutil.MustNoErr(t, reservations.Release(db.DB, planner.ID, bin.ID), "drop the plan")
	testdb.ClaimBinForTest(t, db, bin.ID, planner.ID)
	if blocked, block := CheckDropoffCapacity(db, stage.Name, 0); !blocked || block.Cause != CauseDropoffInflight {
		t.Fatalf("the gate let a second carrier toward %s after the first order claimed its bin "+
			"(blocked=%t cause=%q)", stage.Name, blocked, block.Cause)
	}
}

// ── extra (synth): the gate still counts a robot that IS coming ────────────

// TestDropoffGate_RefusesBehindTheABPressRefillingRobot is the other side of the
// re-home: counting holders instead of statuses must not stop counting an order
// that is on its way. A sequential (A/B press) position's changeover Order A is a
// round trip — it lifts the old carrier and brings the new style's back to the
// same position — so while it is out, anything else aimed at that position is a
// second carrier for one slot. The gate must refuse it at dispatch, naming one
// inbound order: the refilling robot.
//
// COVERAGE PIN for the holder re-home. Passes at bcbde0d2 (Order A is
// `in_transit`, which the status count includes). MUTATION: count only bins
// present at the node — the position reads empty, the move is admitted, and this
// fails on the first assertion.
func TestDropoffGate_RefusesBehindTheABPressRefillingRobot(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	pos := prNode(t, db, "PRAB-POS")
	out := prNode(t, db, "PRAB-OUT")
	inb := prNode(t, db, "PRAB-IN")
	carrier := testdb.CreateBinAtNode(t, db, sd.Payload.Code, inb.ID, "PRAB-NEW-CARRIER")

	// Order A, mid-trip: it has already lifted the old carrier off the position
	// and holds the new style's carrier it is bringing back.
	orderA := prLegRow(t, db, "prab-order-a", "", pos.Name, pos.Name, pos.Name, sd.Payload.Code,
		prResolved(prWait(pos.Name), prPick(pos.Name), prDrop(out.Name), prPick(inb.Name), prDrop(pos.Name)))
	testdb.ClaimBinForTest(t, db, carrier.ID, orderA.ID)
	testdb.SeedOrderStatus(t, db, orderA.ID, string(StatusInTransit), "")

	blocked, block := CheckDropoffCapacity(db, pos.Name, 0)
	if !blocked {
		t.Fatalf("the gate admits a second carrier to %s while Order A (%d) is on its way back to it with one. "+
			"That is the A/B press double-supply, one layer down from the Edge interlock", pos.Name, orderA.ID)
	}
	if block.Cause != CauseDropoffInflight || block.Params.InboundOrders != 1 {
		t.Errorf("refusal is %q with %d inbound, want %q naming exactly one inbound order (Order A)",
			block.Cause, block.Params.InboundOrders, CauseDropoffInflight)
	}

	// And a plain delivery aimed at the position is held at dispatch, not sent.
	d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID: "prab-move", OrderType: OrderTypeMove, PayloadCode: sd.Payload.Code,
		SourceNode: sd.StorageNode.Name, DeliveryNode: pos.Name, Quantity: 1,
	})
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "PRAB-MOVE-BIN")
	if mv := dispatchSimpleViaScanner(t, d, db, "prab-move"); mv.VendorOrderID != "" {
		t.Fatalf("the move to %s was dispatched (%s) behind the refilling robot", pos.Name, mv.VendorOrderID)
	}
}
