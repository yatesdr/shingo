//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
)

// TestDropoffGate_TheSecondShopperGoesOnePassAfterTheSlotClears follows
// TestDropoffGate_TwoShoppersForOneExclusiveDropoffOneGoes to the end: two
// complex orders bound for one declared-exclusive staging dropoff at once.
//
// One goes; the other parks under a dropoff-family cause, holding nothing;
// while the first order's carrier sits on the slot the second stays parked;
// once the slot clears, the second goes within ONE scan pass; both complete.
func TestDropoffGate_TheSecondShopperGoesOnePassAfterTheSlotClears(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	srcX := prNode(t, db, "RC-SRC-X")
	srcY := prNode(t, db, "RC-SRC-Y")
	stage := prNode(t, db, "RC-STAGE")
	away := prNode(t, db, "RC-AWAY")
	binX := testdb.CreateBinAtNode(t, db, sd.Payload.Code, srcX.ID, "RC-X")
	binY := testdb.CreateBinAtNode(t, db, sd.Payload.Code, srcY.ID, "RC-Y")

	for _, s := range []struct{ uuid, src string }{{"rc-x", srcX.Name}, {"rc-y", srcY.Name}} {
		d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
			OrderUUID: s.uuid, PayloadCode: sd.Payload.Code, Quantity: 1,
			Steps: []protocol.ComplexOrderStep{prPick(s.src), prDropExcl(stage.Name)},
		})
	}
	prScanPass(t, d, db, "rc-x", "rc-y")

	x, y := prReloadUUID(t, db, "rc-x"), prReloadUUID(t, db, "rc-y")
	first, second, firstBin, secondBin := x, y, binX, binY
	if x.VendorOrderID == "" {
		first, second, firstBin, secondBin = y, x, binY, binX
	}
	if first.VendorOrderID == "" || second.VendorOrderID != "" {
		t.Fatalf("pass 1: want exactly one order dispatched, got x vendor=%q (%s/%s), y vendor=%q (%s/%s)",
			x.VendorOrderID, x.Status, x.QueueCause, y.VendorOrderID, y.Status, y.QueueCause)
	}
	t.Logf("pass 1: %s dispatched; %s parked %s / %q — %q",
		first.EdgeUUID, second.EdgeUUID, second.QueueCode, second.QueueCause, second.QueueReason)
	refusals := 0
	if isDropoffFamily(second.QueueCause) {
		refusals++
	} else {
		t.Errorf("pass 1: the second order parked under %q, want a dropoff-family cause", second.QueueCause)
	}
	prAssertHoldsNothing(t, db, second, "the order refused at the dropoff gate")

	// The first order's carrier lands on the slot and the order completes.
	testutil.MustNoErr(t, db.MoveBinClearingStaging(firstBin.ID, stage.ID, false), "land the first carrier")
	testdb.SeedOrderStatus(t, db, first.ID, string(StatusConfirmed), "test harness")
	testutil.MustNoErr(t, db.ReleaseOrderHoldings(first.ID), "the first order hands its holds back")

	prScanPass(t, d, db, "rc-x", "rc-y")
	second = prReloadUUID(t, db, second.EdgeUUID)
	if second.VendorOrderID != "" {
		t.Fatalf("pass 2: the second order went while the first order's carrier still sits on %s", stage.Name)
	}
	t.Logf("pass 2 (slot occupied): %s parked %s / %q — %q",
		second.EdgeUUID, second.QueueCode, second.QueueCause, second.QueueReason)
	if isDropoffFamily(second.QueueCause) {
		refusals++
	}

	// The slot clears: the carrier is taken away.
	testutil.MustNoErr(t, db.MoveBinClearingStaging(firstBin.ID, away.ID, false), "clear the slot")
	prScanPass(t, d, db, "rc-x", "rc-y")
	second = prReloadUUID(t, db, second.EdgeUUID)
	if second.VendorOrderID == "" {
		t.Fatalf("pass 3: the slot cleared and the second order did not go within one pass: %s / %q — %q",
			second.Status, second.QueueCause, second.QueueReason)
	}
	t.Logf("pass 3 (slot cleared): %s dispatched as %s", second.EdgeUUID, second.VendorOrderID)

	testutil.MustNoErr(t, db.MoveBinClearingStaging(secondBin.ID, stage.ID, false), "land the second carrier")
	testdb.SeedOrderStatus(t, db, second.ID, string(StatusConfirmed), "test harness")
	testutil.MustNoErr(t, db.ReleaseOrderHoldings(second.ID), "the second order hands its holds back")

	for _, uuid := range []string{"rc-x", "rc-y"} {
		if o := prReloadUUID(t, db, uuid); o.Status != StatusConfirmed {
			t.Errorf("%s ended %s, want confirmed", uuid, o.Status)
		}
	}
	t.Logf("refusal counter (passes the second order spent under a dropoff-family cause): %d", refusals)
	if refusals == 0 {
		t.Error("the refusal counter stayed at 0 — the gate never bound")
	}
}

func isDropoffFamily(cause string) bool {
	switch QueueCause(cause) {
	case CauseDropoffOccupied, CauseDropoffInflight, CauseDropoffCapacity:
		return true
	}
	return false
}
