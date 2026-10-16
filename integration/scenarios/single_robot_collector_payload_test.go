// The single-robot changeover's fetch, end to end across the two modules.
//
// A single_robot changeover is one order (since 2026-10-16): it fetches the
// incoming bin and parks it on the to-claim's inbound staging, evacuates the
// line, and collects the parked bin by relay to deliver it. The order carries
// the FROM payload — a pickup lifts the outgoing bin off the line, and
// filtering that for the incoming part is ALN_001 — so its fetch must name the
// TO part or Core judges it against the FROM part and finds nothing on a
// payload change. (Until 2026-10-16 the same question sat on a second order's
// collect at staging, which named the TO part and so could be given a stray.)
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
	src, err := coreDB.GetNodeByDotName("SRC-SRC")
	if err != nil {
		t.Fatal(err)
	}
	// The outgoing bin on the line, and the incoming bin at its source.
	coreharness.CreateBinAtNode(t, coreDB, "SRC-P-FROM", line.ID, "SRC-OUTGOING")
	incoming := coreharness.CreateBinAtNode(t, coreDB, "SRC-P-TO", src.ID, "SRC-INCOMING")

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
		t.Fatal("fixture drift: the single_robot changeover built no order")
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
		t.Fatalf("the order did not dispatch: %v\n"+
			"Its fetch is judged against the order's FROM payload while the bin it came for is "+
			"the TO part, so it holds for ever on every payload-changing single_robot changeover", err)
	}
	if b, err := coreDB.GetBin(incoming.ID); err != nil || b.ClaimedBy == nil || *b.ClaimedBy != order.ID {
		t.Errorf("the incoming bin at its source is not claimed by the order (bin=%+v err=%v)", b, err)
	}
}
