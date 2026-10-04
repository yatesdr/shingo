package engine

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/orders"
)

// A PRODUCE LINE WITH NO BIN ON IT.
//
// A single-robot swap starts by lifting the line's bin, so on an empty line it
// holds at Core for ever. The request answers an empty line the way the consume
// side does: a plain order brings an empty to the line, from the spot when a
// right spare stands there, else from the claim's inbound source. The empty
// reading is Core's answer to the occupancy read the request already makes.

// liveLineRows is the line's live rows, by type.
func liveLineRows(t *testing.T, eng *Engine, nodeID int64) (complex, plain []domain.Order) {
	t.Helper()
	rows, err := eng.db.ListActiveOrdersByProcessNode(nodeID)
	testutil.MustNoErr(t, err, "rows")
	for _, o := range rows {
		if o.OrderType == orders.TypeComplex {
			complex = append(complex, o)
		} else {
			plain = append(plain, o)
		}
	}
	return complex, plain
}

func TestProduceEmptyLine_SingleRobotRequestDeliversAnEmpty(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		keepStaged  bool
		spot        NodeBinInfo
		uop         int
		wantSource  string
		wantType    protocol.OrderType
		wantRefills int
		wantReturns int
	}{
		{"no spot, count left over", false, NodeBinInfo{}, 30, ksMarket, orders.TypeRetrieve, 0, 0},
		{"no spot, count zero", false, NodeBinInfo{}, 0, ksMarket, orders.TypeRetrieve, 0, 0},
		{"keep-staged, empty spare on the spot", true, NodeBinInfo{Occupied: true}, 0, ksSpot, orders.TypeMove, 1, 0},
		{"keep-staged, spot bare", true, NodeBinInfo{}, 0, ksMarket, orders.TypeRetrieve, 1, 0},
		{"keep-staged, a full on the spot", true, NodeBinInfo{Occupied: true, PayloadCode: ksPart}, 0, ksMarket, orders.TypeRetrieve, 1, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			eng, db, nodeID, claim := seedCell(t, protocol.ClaimRoleProduce, protocol.SwapModeSingleRobot, c.keepStaged,
				map[string]NodeBinInfo{ksLine: {}, ksSpot: c.spot})
			testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, &claim.ID, c.uop), "count")

			res, err := eng.RequestProduceSwap(nodeID)
			testutil.MustNoErr(t, err, "request on an empty line")

			legs, plain := liveLineRows(t, eng, nodeID)
			if len(legs) != 0 {
				t.Fatalf("%d swap legs on an empty line, want none", len(legs))
			}
			var to []domain.Order
			for _, o := range plain {
				if o.DeliveryNode == ksLine {
					to = append(to, o)
				}
			}
			if len(to) != 1 {
				t.Fatalf("deliveries to the line = %d, want 1 (rows %+v)", len(to), plain)
			}
			d := to[0]
			if d.SourceNode != c.wantSource || d.OrderType != c.wantType || d.PayloadCode != "" && d.OrderType == orders.TypeMove {
				t.Errorf("the delivery is a %s %s->%s carrying %q, want a %s from %s",
					d.OrderType, d.SourceNode, d.DeliveryNode, d.PayloadCode, c.wantType, c.wantSource)
			}
			if d.OrderType == orders.TypeRetrieve && !d.RetrieveEmpty {
				t.Errorf("the retrieve to the line asks for a full, want an empty")
			}
			if res == nil || res.Order == nil || res.Order.ID != d.ID {
				t.Errorf("the request returned %+v, want the delivery %d", res, d.ID)
			}
			rt, err := db.GetProcessNodeRuntime(nodeID)
			testutil.MustNoErr(t, err, "runtime")
			if rt.ActiveOrderID == nil || *rt.ActiveOrderID != d.ID {
				t.Errorf("the line's active order is %v, want the delivery %d", rt.ActiveOrderID, d.ID)
			}
			if c.keepStaged {
				if got := readSpotOrders(t, db, nodeID); got.refills != c.wantRefills || got.returns != c.wantReturns {
					t.Errorf("spot refills=%d returns=%d, want %d and %d", got.refills, got.returns, c.wantRefills, c.wantReturns)
				}
			}
		})
	}
}

// A position still being worked reads empty mid-swap: the request is refused
// and nothing is created, as on the consume side.
func TestProduceEmptyLine_PositionStillWorkedRefuses(t *testing.T) {
	t.Parallel()
	eng, _, nodeID, _ := seedCell(t, protocol.ClaimRoleProduce, protocol.SwapModeSingleRobot, false,
		map[string]NodeBinInfo{ksLine: {}})
	// A live order on the line that sits in no runtime slot.
	_, err := eng.orderMgr.CreateRetrieveOrder(&nodeID, true, 1, ksLine, ksMarket, "", "standard", ksPart, false, false,
		orders.Attached("prior"))
	testutil.MustNoErr(t, err, "prior delivery")

	_, err = eng.RequestProduceSwap(nodeID)
	if err == nil || !strings.Contains(err.Error(), "still working this position") {
		t.Fatalf("request = %v, want the still-working refusal", err)
	}
	legs, plain := liveLineRows(t, eng, nodeID)
	if len(legs) != 0 || len(plain) != 1 {
		t.Fatalf("legs=%d plain=%d after the refusal, want 0 and the prior 1", len(legs), len(plain))
	}
}

// TWO-ROBOT IS NOT CHANGED, and this is what it does today on an empty line: with
// parts counted it builds the two-leg swap (whose removal leg then lifts at an
// empty line), and with none counted it refuses.
func TestProduceEmptyLine_TwoRobotIsUnchanged(t *testing.T) {
	t.Parallel()
	t.Run("parts counted: the swap", func(t *testing.T) {
		t.Parallel()
		eng, _, nodeID, _ := seedCell(t, protocol.ClaimRoleProduce, protocol.SwapModeTwoRobot, false,
			map[string]NodeBinInfo{ksLine: {}})
		_, err := eng.RequestProduceSwap(nodeID)
		testutil.MustNoErr(t, err, "request")
		legs, plain := liveLineRows(t, eng, nodeID)
		if len(legs) != 2 || len(plain) != 0 {
			t.Fatalf("legs=%d plain=%d, want the two-leg swap and no plain order", len(legs), len(plain))
		}
	})
	t.Run("none counted: refused", func(t *testing.T) {
		t.Parallel()
		eng, db, nodeID, claim := seedCell(t, protocol.ClaimRoleProduce, protocol.SwapModeTwoRobot, false,
			map[string]NodeBinInfo{ksLine: {}})
		testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, &claim.ID, 0), "count")
		_, err := eng.RequestProduceSwap(nodeID)
		if err == nil || !strings.Contains(err.Error(), "has no parts to finalize") {
			t.Fatalf("request = %v, want the no-parts refusal", err)
		}
		if legs, plain := liveLineRows(t, eng, nodeID); len(legs)+len(plain) != 0 {
			t.Fatalf("legs=%d plain=%d, want nothing", len(legs), len(plain))
		}
	})
}
