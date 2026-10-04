// A produce keep-staged changeover that changes the carrier type, end to end
// across the two modules.
//
// The spot holds an EMPTY of the carrier type the line is leaving: the
// outgoing style's spare, which the changeover-start reconcile is sending back.
// Until it goes, the incoming style's supply must not take it to the line.
// Edge cannot tell carrier types apart — it holds no carrier rule — so the
// supply's spot pickup names the incoming part and is never Empty, and Core
// judges the bin against that part's carrier rule. This drives the Edge
// builder's real step list through Core's complex intake and dispatch: the
// supply holds while the old-type empty stands there, and takes the new-type
// empty once one does.
//
// The steady keep-staged builders are used: a changeover's supply for a
// keep-staged to-claim is the same tail lifting the spare with the same
// spotPickup (engine changeoverDispatch); only its wait's release purpose
// differs, and Core's allocator does not read that.
//
//go:build docker

package scenarios

import (
	"testing"

	"shingo/protocol"

	"shingocore/dispatch"
	corebins "shingocore/store/bins"
	corenodes "shingocore/store/nodes"
	coreorders "shingocore/store/orders"
	corepayloads "shingocore/store/payloads"
	coreharness "shingocore/testharness"

	edgeengine "shingoedge/engine"
	"shingoedge/store/processes"
)

func TestScenario_KeepStagedProduceSupplyNeverTakesTheOldCarrier(t *testing.T) {
	for _, mode := range []protocol.SwapMode{protocol.SwapModeTwoRobot, protocol.SwapModeSingleRobot} {
		t.Run(string(mode), func(t *testing.T) {
			p := "KSP" + string(mode[0]) // node-name prefix, one per subtest database
			coreDB := coreharness.OpenDB(t)
			coreharness.SetupStandardData(t, coreDB)
			node := func(name string) *corenodes.Node {
				n := &corenodes.Node{Name: p + "-" + name, Enabled: true}
				if err := coreDB.CreateNode(n); err != nil {
					t.Fatalf("node %s: %v", name, err)
				}
				return n
			}
			line, spot, outStage, dest := node("LINE"), node("SPOT"), node("OUT-STAGE"), node("DEST")
			node("SRC")
			oldT := &corebins.BinType{Code: p + "-OLDT"}
			newT := &corebins.BinType{Code: p + "-NEWT"}
			for _, bt := range []*corebins.BinType{oldT, newT} {
				if err := coreDB.CreateBinType(bt); err != nil {
					t.Fatalf("bin type %s: %v", bt.Code, err)
				}
			}
			pOld := &corepayloads.Payload{Code: p + "-POLD", UOPCapacity: 50}
			pNew := &corepayloads.Payload{Code: p + "-PNEW", UOPCapacity: 50}
			for _, pl := range []*corepayloads.Payload{pOld, pNew} {
				if err := coreDB.CreatePayload(pl); err != nil {
					t.Fatalf("payload %s: %v", pl.Code, err)
				}
			}
			mustNil(t, coreDB.SetPayloadBinTypes(pOld.ID, []int64{oldT.ID}), "rule old")
			mustNil(t, coreDB.SetPayloadBinTypes(pNew.ID, []int64{newT.ID}), "rule new")

			// The press holds the outgoing part's bin; the spot holds an empty of the
			// outgoing carrier type.
			lineBin := &corebins.Bin{BinTypeID: oldT.ID, Label: p + "-LINE-BIN", NodeID: &line.ID, Status: "staged"}
			mustNil(t, coreDB.CreateBin(lineBin), "line bin")
			mustNil(t, coreDB.SetBinManifest(lineBin.ID, `{"items":[]}`, pOld.Code, 50), "line manifest")
			mustNil(t, coreDB.ConfirmBinManifest(lineBin.ID, ""), "line confirm")
			oldEmpty := &corebins.Bin{BinTypeID: oldT.ID, Label: p + "-OLD-EMPTY", NodeID: &spot.ID, Status: "available"}
			mustNil(t, coreDB.CreateBin(oldEmpty), "old-type empty")

			to := &processes.NodeClaim{
				CoreNodeName: line.Name, Role: protocol.ClaimRoleProduce, SwapMode: mode, PayloadCode: pNew.Code,
				InboundStaging: spot.Name, OutboundStaging: outStage.Name, InboundSource: p + "-SRC",
				OutboundDestination: dest.Name, KeepStaged: true,
			}
			d := dispatch.NewDispatcher(coreDB, coreharness.NewTrackingBackend(), &noopEmitter{}, "core", "shingo.dispatch", nil)
			env := &protocol.Envelope{Src: protocol.Address{Station: "edge.test"}}
			var legs []string
			submit := func(uuid, sibling, payload string, steps []protocol.ComplexOrderStep) {
				d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
					OrderUUID: uuid, PayloadCode: payload, Quantity: 1, ProcessNode: line.Name,
					SiblingOrderUUID: sibling, Steps: steps,
				})
				legs = append(legs, uuid)
			}
			supplyUUID := p + "-supply"
			if mode == protocol.SwapModeTwoRobot {
				a, b := edgeengine.BuildTwoRobotSwapSteps(to)
				// The changeover's two-robot supply goes out with a blank payload,
				// back-filled from the incoming style; the evac carries the outgoing.
				submit(supplyUUID, p+"-evac", pNew.Code, a)
				submit(p+"-evac", supplyUUID, pOld.Code, b)
			} else {
				// The single-robot order B carries the outgoing payload (its first
				// pickup lifts the press's bin); its spot pickup names the incoming.
				submit(supplyUUID, "", pOld.Code, edgeengine.BuildSingleSwapSteps(to))
			}
			pass := func() {
				for _, uuid := range legs {
					o, err := coreDB.GetOrderByUUID(uuid)
					mustNil(t, err, "order "+uuid)
					_ = d.DispatchPreparedComplex(o)
				}
			}
			supply := func() *coreorders.Order {
				o, err := coreDB.GetOrderByUUID(supplyUUID)
				mustNil(t, err, "supply")
				return o
			}

			pass()
			pass()
			if s := supply(); s.VendorOrderID != "" {
				t.Fatalf("the supply went to the fleet with an outgoing-type empty on the spot (status %q)", s.Status)
			}
			if b, err := coreDB.GetBin(oldEmpty.ID); err != nil || b.ClaimedBy != nil {
				t.Fatalf("the outgoing-type empty is claimed (bin %+v, err %v): it would be delivered to the press", b, err)
			}

			// The return lifts the old empty; a refill lands a new-type empty.
			mustNil(t, coreDB.DeleteBin(oldEmpty.ID), "the return takes the old empty away")
			newEmpty := &corebins.Bin{BinTypeID: newT.ID, Label: p + "-NEW-EMPTY", NodeID: &spot.ID, Status: "available"}
			mustNil(t, coreDB.CreateBin(newEmpty), "new-type empty")

			pass()
			pass()
			s := supply()
			if s.VendorOrderID == "" {
				t.Fatalf("the supply did not go with a new-type empty on the spot: status %q (%s) %q",
					s.Status, s.QueueCause, s.QueueReason)
			}
			if b, err := coreDB.GetBin(newEmpty.ID); err != nil || b.ClaimedBy == nil || *b.ClaimedBy != s.ID {
				t.Errorf("the new-type empty is not the supply's (bin %+v, err %v)", b, err)
			}
		})
	}
}

func mustNil(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}
