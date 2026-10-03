// The single-robot changeover collector, end to end across the two modules.
//
// A single_robot changeover is two orders. Order A stages the incoming bin on
// the to-claim's inbound staging; order B evacuates the line and then collects
// that staged bin to deliver it. Order B carries the FROM payload — its first
// pickup lifts the outgoing bin off the line, and filtering that for the
// incoming part is ALN_001 — so its staging pickup, which names nothing, is
// judged by Core against the FROM part. On a payload change the staged bin is
// the TO part and is never collected.
//
// This drives the Edge builder's real step list through Core's real complex
// intake and the dispatch the scanner runs, so neither side's half can drift
// from the other's without this failing.
//
//go:build docker

package scenarios

import (
	"testing"

	"shingo/protocol"

	"shingocore/dispatch"
	corenodes "shingocore/store/nodes"
	corepayloads "shingocore/store/payloads"
	coreharness "shingocore/testharness"

	edgeengine "shingoedge/engine"
	"shingoedge/store/processes"
)

func TestScenario_SingleRobotCollectorCollectsTheIncomingPart(t *testing.T) {
	coreDB := coreharness.OpenDB(t)
	coreharness.SetupStandardData(t, coreDB)

	for _, name := range []string{"SRC-LINE", "SRC-STG-IN", "SRC-STG-OUT", "SRC-DST", "SRC-SRC"} {
		if err := coreDB.CreateNode(&corenodes.Node{Name: name, Enabled: true}); err != nil {
			t.Fatalf("create node %s: %v", name, err)
		}
	}
	for _, code := range []string{"SRC-P-FROM", "SRC-P-TO"} {
		if err := coreDB.CreatePayload(&corepayloads.Payload{Code: code, UOPCapacity: 100}); err != nil {
			t.Fatalf("create payload %s: %v", code, err)
		}
	}
	line, err := coreDB.GetNodeByDotName("SRC-LINE")
	if err != nil {
		t.Fatal(err)
	}
	stgIn, err := coreDB.GetNodeByDotName("SRC-STG-IN")
	if err != nil {
		t.Fatal(err)
	}
	// The outgoing bin on the line, and the incoming bin order A staged.
	coreharness.CreateBinAtNode(t, coreDB, "SRC-P-FROM", line.ID, "SRC-OUTGOING")
	incoming := coreharness.CreateBinAtNode(t, coreDB, "SRC-P-TO", stgIn.ID, "SRC-INCOMING")

	claim := func(payload string) *processes.NodeClaim {
		return &processes.NodeClaim{
			CoreNodeName: "SRC-LINE", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeSingleRobot,
			PayloadCode: payload, InboundStaging: "SRC-STG-IN", OutboundStaging: "SRC-STG-OUT",
			InboundSource: "SRC-SRC", OutboundDestination: "SRC-DST",
		}
	}
	from, to := claim("SRC-P-FROM"), claim("SRC-P-TO")
	collector := edgeengine.BuildSwapChangeoverSteps(from, to, "", "").StepsB
	if len(collector) == 0 {
		t.Fatal("fixture drift: the single_robot changeover built no collector")
	}

	backend := coreharness.NewTrackingBackend()
	d := dispatch.NewDispatcher(coreDB, backend, &noopEmitter{}, "core", "shingo.dispatch", nil)
	env := &protocol.Envelope{Src: protocol.Address{Station: "edge.test"}}
	// The order carries the FROM payload, as assignDispatch stamps every
	// evac-bearing changeover order (changeover_planner.go).
	d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID: "src-collector", PayloadCode: from.PayloadCode, Quantity: 1,
		ProcessNode: "SRC-LINE", Steps: collector,
	})
	order, err := coreDB.GetOrderByUUID("src-collector")
	if err != nil {
		t.Fatalf("collector order not created: %v", err)
	}
	if err := d.DispatchPreparedComplex(order); err != nil {
		t.Fatalf("the collector did not dispatch: %v\n"+
			"Its staging pickup is judged against the order's FROM payload while the bin it came for is "+
			"the TO part, so it holds for ever on every payload-changing single_robot changeover", err)
	}
	if b, err := coreDB.GetBin(incoming.ID); err != nil || b.ClaimedBy == nil || *b.ClaimedBy != order.ID {
		t.Errorf("the incoming bin is not claimed by the collector (bin=%+v err=%v)", b, err)
	}
}
