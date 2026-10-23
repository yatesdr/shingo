package engine

import (
	"encoding/json"
	"sort"
	"sync/atomic"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
)

// TestRequestNodeMaterial_AWrongCarrierIsClearedThenPrimedThenSwapped: a
// consume press whose line holds a full of its part and whose on-deck holds a
// carrier with no part (wrong for a consume index: ChainReadiness clears it).
// One REQUEST makes, on the press's process node and in this order: the clear,
// a move of that carrier by name to the claim's outbound; the prime, a full of
// the part to the on-deck; then the swap's two legs. The clear names its bin on
// the wire, so Core lifts that carrier or none.
func TestRequestNodeMaterial_AWrongCarrierIsClearedThenPrimedThenSwapped(t *testing.T) {
	t.Parallel()
	eng, db, nodeID := seedCensusPress(t, protocol.ClaimRoleConsume, protocol.SwapModeTwoRobotPressIndex, 30, false)
	var calls atomic.Int32
	bridgeCore(t, eng, censusStub(t, &calls, ksLine, censusDeck).URL)
	holdPlaces(t, eng, func(n string) (protocol.NodeBinInfo, bool) {
		switch n {
		case ksLine:
			return protocol.NodeBinInfo{NodeName: n, Occupied: true, BinID: 90, PayloadCode: ksPart, UOPRemaining: 40, UOPCapacity: 40}, true
		case censusDeck:
			return protocol.NodeBinInfo{NodeName: n, Occupied: true, BinID: 91}, true
		}
		return protocol.NodeBinInfo{NodeName: n}, true
	})

	res, err := eng.RequestNodeMaterial(nodeID, 1)
	testutil.MustNoErr(t, err, "the REQUEST")
	rows, err := db.ListActiveOrdersByProcessNode(nodeID)
	testutil.MustNoErr(t, err, "the press's rows")
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	if len(rows) != 4 {
		t.Fatalf("%d orders on the press, want the clear, the prime and two legs: %+v", len(rows), rows)
	}
	clear, prime := rows[0], rows[1]
	if clear.OrderType != orders.TypeMove || clear.SourceNode != censusDeck || clear.DeliveryNode != ksDest {
		t.Errorf("first: %s %s -> %s, want the clear: a move off %s to %s", clear.OrderType, clear.SourceNode, clear.DeliveryNode, censusDeck, ksDest)
	}
	if prime.OrderType != orders.TypeMove || prime.SourceNode != ksMarket || prime.DeliveryNode != censusDeck {
		t.Errorf("second: %s %s -> %s, want the prime: a full from %s to %s", prime.OrderType, prime.SourceNode, prime.DeliveryNode, ksMarket, censusDeck)
	}
	for _, leg := range rows[2:] {
		if leg.OrderType != orders.TypeComplex {
			t.Errorf("order %d is %s, want the swap's legs last", leg.ID, leg.OrderType)
		}
	}
	if len(res.PrimeOrders) != 2 || res.PrimeOrders[0].ID != clear.ID || res.PrimeOrders[1].ID != prime.ID {
		t.Errorf("the result's chain: %+v, want the clear then the prime", res.PrimeOrders)
	}
	msgs, err := db.ListPendingOutbox(100)
	testutil.MustNoErr(t, err, "outbox")
	named := false
	for _, m := range msgs {
		if m.MsgType != protocol.TypeOrderRequest {
			continue
		}
		var env protocol.Envelope
		testutil.MustNoErr(t, json.Unmarshal(m.Payload, &env), "envelope")
		var req protocol.OrderRequest
		testutil.MustNoErr(t, env.DecodePayload(&req), "request")
		if req.OrderUUID == clear.UUID {
			named = req.BinID == 91
		}
	}
	if !named {
		t.Error("the clear does not name the carrier (bin 91) on the wire")
	}
}
