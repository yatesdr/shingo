package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

// A person's reset in Core (an admin Load Payload or Clear) names what the
// carrier now holds, and the station records it as the carrier at the node, so
// its counts carry that part. Hopkinsville 4x2, 2026-10-26: a line cart was
// restamped in Core from one part to another, the station kept the old part,
// and Core refused every count from it as a payload mismatch.

func identityNode(t *testing.T) (*Engine, int64) {
	t.Helper()
	db := testEngineDB(t)
	pid, err := db.CreateProcess("P", "", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	sid, err := db.CreateOperatorStation(stations.Input{ProcessID: pid, Name: "S"})
	testutil.MustNoErr(t, err, "create station")
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: pid, OperatorStationID: &sid, CoreNodeName: "SYN-LINE", Enabled: true,
	})
	testutil.MustNoErr(t, err, "create node")
	_, err = db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "ensure runtime")
	return testEngine(t, db), nodeID
}

func identityOf(t *testing.T, eng *Engine, nodeID int64) *processes.RuntimeState {
	t.Helper()
	rt, err := eng.db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "get runtime")
	return rt
}

func TestHandleUOPAdjustment_APersonsRestampNamesTheCarrier(t *testing.T) {
	t.Parallel()
	eng, nodeID := identityNode(t)
	bin := int64(17)
	testutil.MustNoErr(t, eng.db.SetProcessNodeRuntimeWithBinAndEpoch(nodeID, nil, &bin, 35, 69), "bind the cart")
	testutil.MustNoErr(t, eng.db.SetProcessNodeRuntimeLinesidePayload(nodeID, "PART-OLD", true, "reconcile"), "the part it last knew")

	eng.HandleUOPAdjustment(protocol.UOPAdjustment{
		BinID: bin, CoreNodeName: "SYN-LINE", NewRemaining: 100, Epoch: 36,
		Actor: protocol.AuditActorUI, PayloadCode: "PART-NEW", PayloadKnown: true,
	})
	rt := identityOf(t, eng, nodeID)
	if string(rt.LinesidePayloadCode) != "PART-NEW" || !rt.LinesidePayloadKnown || rt.LinesideSource != "operator" {
		t.Fatalf("after the restamp the node holds %q (known=%v, source=%q), want PART-NEW, known, from the operator",
			rt.LinesidePayloadCode, rt.LinesidePayloadKnown, rt.LinesideSource)
	}
	if _, part, _ := eng.binAtNode(rt, nil); part != "PART-NEW" {
		t.Errorf("the line's counts carry %q, want PART-NEW", part)
	}

	// A count names nothing, and neither does a lifecycle announcement.
	eng.HandleUOPAdjustment(protocol.UOPAdjustment{
		BinID: bin, CoreNodeName: "SYN-LINE", NewRemaining: 90, Epoch: 36, Actor: protocol.AuditActorUI,
	})
	eng.HandleUOPAdjustment(protocol.UOPAdjustment{
		BinID: bin, CoreNodeName: "SYN-LINE", NewRemaining: 90, Epoch: 37,
	})
	if rt := identityOf(t, eng, nodeID); string(rt.LinesidePayloadCode) != "PART-NEW" || !rt.LinesidePayloadKnown {
		t.Errorf("a count or a lifecycle announcement changed the carrier to %q (known=%v), want PART-NEW kept",
			rt.LinesidePayloadCode, rt.LinesidePayloadKnown)
	}
}

func TestHandleUOPAdjustment_APersonsLoadBindsANamedCarrier(t *testing.T) {
	t.Parallel()
	eng, nodeID := identityNode(t)
	eng.HandleUOPAdjustment(protocol.UOPAdjustment{
		BinID: 24, CoreNodeName: "SYN-LINE", NewRemaining: 100, Epoch: 5,
		Actor: protocol.AuditActorUI, PayloadCode: "PART-NEW", PayloadKnown: true,
	})
	rt := identityOf(t, eng, nodeID)
	if rt.ActiveBinID == nil || *rt.ActiveBinID != 24 {
		t.Fatalf("the load did not bind the staged cart: active bin %v", rt.ActiveBinID)
	}
	if string(rt.LinesidePayloadCode) != "PART-NEW" || !rt.LinesidePayloadKnown {
		t.Errorf("the bound cart is %q (known=%v), want PART-NEW, known", rt.LinesidePayloadCode, rt.LinesidePayloadKnown)
	}
}
