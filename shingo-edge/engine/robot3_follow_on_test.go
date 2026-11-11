package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	ordermgr "shingoedge/orders"
	"shingoedge/store"
	"shingoedge/store/orders"
	"shingoedge/store/processes"
)

// ROBOT 3 (refill_by own_robot): neither robot of an index press's pair goes to
// the market. Robot 2's lift of the back is the one trigger, and it orders the
// back its next carrier as a plain retrieve naming the press, confirmed by its
// placement and attached to Robot 2's own origin.

// robot3Pair seeds an index press, sets its refill, and makes its steady pair
// as the produce door does: both legs minted together, linked, attached to one
// episode. It returns the engine, the store, the press's node and the two legs.
func robot3Pair(t *testing.T, refill protocol.RefillBy) (*Engine, *store.DB, int64, *orders.Order, *orders.Order) {
	t.Helper()
	db := testEngineDB(t)
	nodeID, node, _ := seedSwapClaim(t, db, protocol.SwapModeTwoRobotPressIndex, "")
	setRefill(t, db, refill)
	claim := requestedClaimAtNode(db, node)
	eng := testEngine(t, db)
	d, err := BuildSwapDispatch(node, claim)
	if err != nil || d == nil || d.StepsB == nil {
		t.Fatalf("no pair: %v", err)
	}
	uuidA, uuidB := ordermgr.NewOrderUUID(), ordermgr.NewOrderUUID()
	origin := ordermgr.Attached("EP-R3")
	r1, err := eng.orderMgr.CreateComplexOrderPaired(&nodeID, 1, "", claim.CoreNodeName, d.StepsA, true, "", uuidB, uuidA, origin)
	testutil.MustNoErr(t, err, "R1")
	r2, err := eng.orderMgr.CreateComplexOrderPaired(&nodeID, 1, "", claim.CoreNodeName, d.StepsB, true, "", uuidA, uuidB, origin)
	testutil.MustNoErr(t, err, "R2")
	testutil.MustNoErr(t, db.LinkOrderSiblings(r1.ID, r2.ID), "link")
	r2, err = db.GetOrder(r2.ID)
	testutil.MustNoErr(t, err, "R2 reread")
	return eng, db, nodeID, r1, r2
}

func setRefill(t *testing.T, db *store.DB, refill protocol.RefillBy) {
	t.Helper()
	_, err := db.DB.Exec(`UPDATE style_node_claims SET refill_by = ? WHERE core_node_name = 'PRESS'`, string(refill))
	testutil.MustNoErr(t, err, "refill_by")
}

// followOnsTo is the live plain orders bound for node.
func followOnsTo(t *testing.T, db *store.DB, nodeID int64, node string) []orders.Order {
	t.Helper()
	rows, err := db.ListActiveOrdersByProcessNode(nodeID)
	testutil.MustNoErr(t, err, "rows")
	var out []orders.Order
	for _, o := range rows {
		if o.OrderType != protocol.OrderTypeComplex && o.DeliveryNode == node {
			out = append(out, o)
		}
	}
	return out
}

// TestRobot3_ThePairGoesToNoMarketAndOwesTheBackOne: with Robot 3 neither leg
// fetches; Robot 2 lifts the back and nothing sets down there, so the plan
// owes the back one follow-on and counts it, and a held partial's pick is
// refused by name.
func TestRobot3_ThePairGoesToNoMarketAndOwesTheBackOne(t *testing.T) {
	t.Parallel()
	for _, third := range []string{"", "INDEX-C"} {
		claim := &processes.NodeClaim{CoreNodeName: "PRESS", Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeTwoRobotPressIndex,
			PayloadCode: "WIDGET-A", InboundSource: "MARKET-EMPTIES", OutboundDestination: "MARKET",
			PairedCoreNode: "INDEX-B", SecondPairedCoreNode: third, RefillBy: protocol.RefillByOwnRobot}
		d, err := BuildSwapDispatch(&processes.Node{Name: "PRESS", CoreNodeName: "PRESS"}, claim)
		if err != nil || d == nil {
			t.Fatalf("no dispatch: %v", err)
		}
		for _, s := range append(append([]protocol.ComplexOrderStep{}, d.StepsA...), d.StepsB...) {
			if s.Node == "MARKET-EMPTIES" {
				t.Errorf("third %q: a leg of the pair goes to the market: %+v", third, s)
			}
		}
		ext := claim.ExtensionPositions()
		back := ext[len(ext)-1]
		if got := followOnPositions(claim.Positions(), d.StepsA, d.StepsB); len(got) != 1 || got[0] != back || d.FollowOns != 1 {
			t.Errorf("third %q: owed %v (FollowOns %d), want the back %s once", third, got, d.FollowOns, back)
		}
		if err := nameRefillBin(d, claim, 42); err == nil || !strings.Contains(err.Error(), "fetches no carrier") {
			t.Errorf("third %q: a held partial's pick = %v, want refused: the pair fetches nothing", third, err)
		}
	}
}

// TestRobot3_RobotTwosLiftOrdersTheBackItsCarrierOnce: the lift orders one
// carrier, a plain empty retrieve to the back on the press's row, naming the
// press, auto-confirmed, attached to Robot 2's origin; a second pickup message
// orders nothing more; a faulted Robot 2 leaves it, and it still reads as
// coming to the back, so a REQUEST does not prime the back again.
func TestRobot3_RobotTwosLiftOrdersTheBackItsCarrierOnce(t *testing.T) {
	t.Parallel()
	eng, db, nodeID, _, r2 := robot3Pair(t, protocol.RefillByOwnRobot)
	eng.HandleBinPickedUp(r2.UUID, 501, "INDEX-B")

	got := followOnsTo(t, db, nodeID, "INDEX-B")
	if len(got) != 1 {
		t.Fatalf("%d orders to the back after Robot 2's lift, want 1", len(got))
	}
	f := got[0]
	if !isRetrieve(f.OrderType) || !f.RetrieveEmpty || f.SourceNode != "MARKET-EMPTIES" || !f.AutoConfirm ||
		f.OriginID != "EP-R3" || f.OriginClass != string(protocol.OriginClassAttached) {
		t.Errorf("the follow-on: %+v, want an empty retrieved from MARKET-EMPTIES, auto-confirmed, attached to EP-R3", f)
	}
	if press := wireProcessNode(t, db, f.UUID); press != "PRESS" {
		t.Errorf("the follow-on names press %q on the wire, want PRESS", press)
	}

	eng.HandleBinPickedUp(r2.UUID, 501, "INDEX-B")
	if n := len(followOnsTo(t, db, nodeID, "INDEX-B")); n != 1 {
		t.Errorf("a second pickup message: %d orders to the back, want still 1", n)
	}

	testutil.MustNoErr(t, db.UpdateOrderStatus(r2.ID, string(protocol.StatusFaulted)), "Robot 2 faults")
	if n := len(followOnsTo(t, db, nodeID, "INDEX-B")); n != 1 {
		t.Errorf("after Robot 2 faulted: %d orders to the back, want the follow-on left", n)
	}
	placing, err := db.LiveOrderPlacing("INDEX-B")
	testutil.MustNoErr(t, err, "placing")
	if placing["INDEX-B"] != orders.PlacingCarrier {
		t.Errorf("after Robot 2 faulted the back reads %v, want a carrier coming (a REQUEST primes nothing more)", placing["INDEX-B"])
	}
}

// TestRobot3_TheStepsDecideNotTheSetting: an edit between the dispatch and the
// lift neither doubles the back nor starves it. A pair built for Robot 3 still
// orders its one carrier after the press is set back to Robot 1; a pair built
// for Robot 1 orders none after the press is set to Robot 3, because its
// Robot 1 sets the back's carrier down itself.
func TestRobot3_TheStepsDecideNotTheSetting(t *testing.T) {
	t.Parallel()
	eng, db, nodeID, _, r2 := robot3Pair(t, protocol.RefillByOwnRobot)
	setRefill(t, db, protocol.RefillByClearingRobot)
	eng.HandleBinPickedUp(r2.UUID, 502, "INDEX-B")
	if n := len(followOnsTo(t, db, nodeID, "INDEX-B")); n != 1 {
		t.Errorf("a Robot 3 pair after an edit to Robot 1: %d orders to the back, want its 1", n)
	}

	eng, db, nodeID, _, r2 = robot3Pair(t, protocol.RefillByClearingRobot)
	setRefill(t, db, protocol.RefillByOwnRobot)
	eng.HandleBinPickedUp(r2.UUID, 503, "INDEX-B")
	if n := len(followOnsTo(t, db, nodeID, "INDEX-B")); n != 0 {
		t.Errorf("a Robot 1 pair after an edit to Robot 3: %d orders to the back, want none (Robot 1 refills it)", n)
	}
}

// TestRobot3_ACancelledChangeoverAbortsTheCarrierNotYetFlown: a changeover's
// cancel aborts a carrier coming to the back that no robot has taken, and
// leaves one already moving and an order to the line.
func TestRobot3_ACancelledChangeoverAbortsTheCarrierNotYetFlown(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	nodeID, node, _ := seedSwapClaim(t, db, protocol.SwapModeTwoRobotPressIndex, "")
	setRefill(t, db, protocol.RefillByOwnRobot)
	claim := requestedClaimAtNode(db, node)
	eng := testEngine(t, db)
	waiting, err := eng.carrierToPosition(nodeID, claim, "INDEX-B", 1, ordermgr.Attached("EP-CO"))
	testutil.MustNoErr(t, err, "a carrier not yet flown")
	moving, err := eng.carrierToPosition(nodeID, claim, "INDEX-B", 1, ordermgr.Attached("EP-CO"))
	testutil.MustNoErr(t, err, "a carrier moving")
	testutil.MustNoErr(t, db.UpdateOrderStatus(moving.ID, string(protocol.StatusInTransit)), "moving")
	line, err := eng.orderMgr.CreateRetrieveOrder(ordermgr.RetrieveSpec{Process: &nodeID, Delivery: "PRESS",
		Source: "MARKET-EMPTIES", Empty: true, Quantity: 1, Origin: ordermgr.Attached("EP-CO")})
	testutil.MustNoErr(t, err, "an order to the line")

	eng.abortCarriersComingBehindFronts([]processes.NodeTask{{ProcessNodeID: nodeID, FromClaimID: &claim.ID}})

	for _, c := range []struct {
		name    string
		id      int64
		aborted bool
	}{{"the carrier not yet flown", waiting.ID, true}, {"the moving carrier", moving.ID, false}, {"the order to the line", line.ID, false}} {
		o, err := db.GetOrder(c.id)
		testutil.MustNoErr(t, err, c.name)
		if got := ordermgr.IsTerminal(o.Status); got != c.aborted {
			t.Errorf("%s: status %s, aborted = %v, want %v", c.name, o.Status, got, c.aborted)
		}
	}
}

// wireProcessNode is the process_node an order.request in the outbox carries.
func wireProcessNode(t *testing.T, db *store.DB, uuid string) string {
	t.Helper()
	msgs, err := db.ListPendingOutbox(200)
	testutil.MustNoErr(t, err, "outbox")
	for _, m := range msgs {
		if m.MsgType != protocol.TypeOrderRequest {
			continue
		}
		var env protocol.Envelope
		var req protocol.OrderRequest
		if json.Unmarshal(m.Payload, &env) == nil && env.DecodePayload(&req) == nil && req.OrderUUID == uuid {
			return req.ProcessNode
		}
	}
	t.Fatalf("no order.request for %s", uuid)
	return ""
}
