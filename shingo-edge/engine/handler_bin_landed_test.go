package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/orders"
)

// TestLanding_RecordsWhatTheDoorwayKnows: a landing records its cart's part at
// the bind (decision 57): the part its order delivered to the node, from the
// delivery; else Core's part for the bin, from the landing (Core's stands
// where it names another part than the order's: it is the bin's record); else
// the part the position's active claim names, assumed, not known. Before, a
// landing whose part Core named was recorded from the landing even when its
// order named the same part, and a cart neither named (placed by hand, an
// order that brought an empty or delivers elsewhere) was recorded unknown and
// left for the flip and the readers to guess.
func TestLanding_RecordsWhatTheDoorwayKnows(t *testing.T) {
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
		{name: "Core names the order's part", named: "PART-X", order: true, wantPart: "PART-X", wantKnown: true, wantSrc: domain.CarrierFromDelivery},
		{name: "Core names another part than the order", named: "PART-Y", order: true, wantPart: "PART-Y", wantKnown: true, wantSrc: domain.CarrierFromLanding},
		{name: "the order brought an empty", order: true, empty: true, wantPart: "PART-OLD", wantSrc: domain.CarrierSource("assumed")},
		{name: "the order delivers elsewhere", order: true, elsewhere: true, wantPart: "PART-OLD", wantSrc: domain.CarrierSource("assumed")},
		{name: "a hand-placed cart: no order, no part", wantPart: "PART-OLD", wantSrc: domain.CarrierSource("assumed")},
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
