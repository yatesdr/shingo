package engine

import (
	"errors"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
)

// REQUEST EMPTY BIN ON A SWAP-MODE LINE GOES THROUGH THE PRODUCE REQUEST'S PATH.
//
// It reads the line first and plans what the line needs, exactly as the produce
// request does; only the guards about the parts are left out. These pin what
// that brings to this button, each of which it went without while it built the
// swap blind. The request census pins the orders and Core calls per mode.

// A position still being worked reads empty mid-swap: the button is refused and
// nothing is created, as both requests are.
func TestRequestEmptyBin_PositionStillWorkedRefuses(t *testing.T) {
	t.Parallel()
	eng, _, nodeID, _ := seedCell(t, protocol.ClaimRoleProduce, protocol.SwapModeSingleRobot, false,
		map[string]NodeBinInfo{ksLine: {}})
	// A live order on the line that sits in no runtime slot and is not an empty.
	_, err := eng.orderMgr.CreateMoveOrder(&nodeID, 1, ksMarket, ksLine, false, orders.Attached("prior"))
	testutil.MustNoErr(t, err, "prior delivery")

	_, err = eng.RequestEmptyBin(nodeID, ksPart)
	if err == nil || !strings.Contains(err.Error(), "still working this position") {
		t.Fatalf("empty-bin request = %v, want the still-working refusal", err)
	}
	legs, plain := liveLineRows(t, eng, nodeID)
	if len(legs) != 0 || len(plain) != 1 {
		t.Fatalf("legs=%d plain=%d after the refusal, want 0 and the prior 1", len(legs), len(plain))
	}
}

// A keep-staged line's swap lifts the spare from the spot; the request orders
// the spot's refill, as the produce request does.
func TestRequestEmptyBin_KeepStagedSpotIsRefilled(t *testing.T) {
	t.Parallel()
	eng, db, nodeID, _ := seedCell(t, protocol.ClaimRoleProduce, protocol.SwapModeSingleRobot, true,
		map[string]NodeBinInfo{ksLine: {Occupied: true, PayloadCode: ksPart}, ksSpot: {Occupied: true}})

	_, err := eng.RequestEmptyBin(nodeID, ksPart)
	testutil.MustNoErr(t, err, "empty-bin request")

	legs, _ := liveLineRows(t, eng, nodeID)
	if len(legs) != 1 {
		t.Fatalf("swap legs = %d, want the one single-robot leg", len(legs))
	}
	if got := readSpotOrders(t, db, nodeID); got.refills != 1 || got.returns != 0 {
		t.Errorf("spot refills=%d returns=%d, want 1 and 0", got.refills, got.returns)
	}
}

// A bin a cancelled changeover left on single-robot outbound staging is moved on
// before the swap is built, as at the produce request.
func TestRequestEmptyBin_MovesABinLeftOnOutboundStaging(t *testing.T) {
	t.Parallel()
	eng, _, nodeID, _ := seedCell(t, protocol.ClaimRoleProduce, protocol.SwapModeSingleRobot, false, nil)
	srv, _ := countingNodeBinsStub(t, map[string]NodeBinInfo{
		ksLine: {Occupied: true, PayloadCode: ksPart}, ksOut: {Occupied: true, PayloadCode: "PART-PARKED"}})
	eng.coreClient = NewCoreClient(srv.URL)

	_, err := eng.RequestEmptyBin(nodeID, ksPart)
	testutil.MustNoErr(t, err, "empty-bin request")

	if moves := movesOff(t, eng, nodeID, ksOut); len(moves) != 1 || moves[0].DeliveryNode != ksDest {
		t.Fatalf("moves off outbound staging = %+v, want one to %s", moves, ksDest)
	}
}

// An armed changeover refuses outgoing-style relief at both buttons.
func TestRequestEmptyBin_BlockedForOutgoingStyleDuringChangeover(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, styleID, _ := seedProduceNode(t, db, "two_robot")
	eng := testEngine(t, db)
	otherStyleID, err := db.CreateStyle("CO-TARGET", "target", processID)
	testutil.MustNoErr(t, err, "create target style")
	fromStyle := styleID
	_, err = eng.changeoverService.Create(processID, &fromStyle, otherStyleID, "test", "", nil, nil, nil, nil)
	testutil.MustNoErr(t, err, "create changeover")

	_, err = eng.RequestEmptyBin(nodeID, "WIDGET-A")
	var armed *ChangeoverArmedError
	if !errors.As(err, &armed) {
		t.Fatalf("empty-bin request = %v, want the armed-changeover refusal", err)
	}
	if n := countOrdersByType(t, db, string(protocol.OrderTypeComplex)); n != 0 {
		t.Errorf("complex orders = %d after the refusal, want 0", n)
	}
}

// The empty on a swap-mode line is the claim's, so the button needs no payload
// code: one it never read was required and checked anyway.
func TestRequestEmptyBin_SwapModeNeedsNoPayloadCode(t *testing.T) {
	t.Parallel()
	eng, _, nodeID, _ := seedCell(t, protocol.ClaimRoleProduce, protocol.SwapModeSingleRobot, false,
		map[string]NodeBinInfo{ksLine: {}})

	_, err := eng.RequestEmptyBin(nodeID, "")
	testutil.MustNoErr(t, err, "empty-bin request with no payload code")
	if _, plain := liveLineRows(t, eng, nodeID); len(plain) != 1 || plain[0].PayloadCode != ksPart {
		t.Fatalf("plain orders %+v, want the one empty for the claim's part %s", plain, ksPart)
	}
}
