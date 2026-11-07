package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/orders"
)

// TestLanding_AnUnnamedCartTakesItsOrdersPart: a landing that carries no part
// but names its order records the part that order delivered to the node, from
// the delivery; a landing that names its part keeps it, and one whose order
// brought an empty, delivers elsewhere or is not named stays unknown. Before,
// every unnamed landing was unknown, and the flip and the readers had to guess
// the cart's part.
func TestLanding_AnUnnamedCartTakesItsOrdersPart(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name      string
		named     string // the landing's part, "" for none
		order     bool   // the landing names an order
		empty     bool   // the order brought an empty
		elsewhere bool   // the order delivers to another node
		wantPart  string
		wantKnown bool
		wantSrc   domain.CarrierSource
	}{
		{name: "the order delivered it here", order: true, wantPart: "PART-X", wantKnown: true, wantSrc: domain.CarrierFromDelivery},
		{name: "the landing names its part", named: "PART-Y", order: true, wantPart: "PART-Y", wantKnown: true, wantSrc: domain.CarrierFromLanding},
		{name: "the order brought an empty", order: true, empty: true, wantSrc: domain.CarrierFromLanding},
		{name: "the order delivers elsewhere", order: true, elsewhere: true, wantSrc: domain.CarrierFromLanding},
		{name: "no order named", wantSrc: domain.CarrierFromLanding},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			db := testEngineDB(t)
			eng := testEngine(t, db)
			_, nodeID, _, _, _, _ := seedChangeoverScenario(t, db)
			uuid := ""
			if row.order {
				uuid = "order-landed"
				delivery := "CO-NODE"
				if row.elsewhere {
					delivery = "ELSEWHERE"
				}
				_, err := db.CreateOrder(uuid, orders.TypeRetrieve, &nodeID, row.empty, 1, delivery, "", "", protocol.Height{},
					false, "PART-X", "", "")
				testutil.MustNoErr(t, err, "the order")
			}
			eng.HandleBinLanded(protocol.BinLanded{BinID: 9901, CoreNodeName: "CO-NODE", Count: 30, Epoch: 1,
				PayloadCode: row.named, PayloadKnown: row.named != "", OrderUUID: uuid, PlacedSeq: 1 << 40})
			rt := runtimeOf(t, eng, nodeID)
			if rt.ActiveBinID == nil || *rt.ActiveBinID != 9901 {
				t.Fatalf("the landing bound %v, want bin 9901", rt.ActiveBinID)
			}
			if string(rt.LinesidePayloadCode) != row.wantPart || rt.LinesidePayloadKnown != row.wantKnown ||
				rt.LinesideSource != string(row.wantSrc) {
				t.Errorf("the seat reads %q known %t from %q, want %q known %t from %q", rt.LinesidePayloadCode,
					rt.LinesidePayloadKnown, rt.LinesideSource, row.wantPart, row.wantKnown, row.wantSrc)
			}
		})
	}
}
