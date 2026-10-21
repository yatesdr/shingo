package engine

import (
	"testing"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/orders"
)

// blankInboundLoaderInfo builds a shared_window loader with NO inbound source —
// the press/forklift-fed shape: the operator feeds the windows directly, so there
// is nothing to auto-pull from. OutboundDest is still set (the empty-out target).
func blankInboundLoaderInfo(coreNode, role, payload string) protocol.LoaderInfo {
	return protocol.LoaderInfo{
		Name:          coreNode,
		LoaderKey:     "loader:" + coreNode,
		Role:          role,
		Layout:        "shared_window",
		Replenishment: "threshold",
		InboundSource: "", // press/forklift-fed: no source to pull from
		OutboundDest:  "EMPTY-TOTES",
		Positions:     []protocol.LoaderPosition{{CoreNodeName: coreNode}},
		Payloads:      []protocol.LoaderPayloadInfo{{PayloadCode: payload}},
	}
}

// TestCreateUnloaderFullIn_NoInboundSkipsAutoPull pins the press-fed-drain gate:
// a consume loader with a blank inbound_source asks Core for no U1 (there is no
// supermarket to pull a full from — the press delivers directly; Core would
// give it none).
// This is the fix for the queued-forever retrieve orders at the Supermarket
// Unloader, whose inbound ("Supermarket Area") held zero bins.
func TestCreateUnloaderFullIn_NoInboundSkipsAutoPull(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	_, nodeID, _ := seedLoaderNode(t, db, "DRAIN-PROC", "DRAIN-NODE")
	seedCoreLoader(t, eng, blankInboundLoaderInfo("DRAIN-NODE", "consume", "PART-D"))

	l, err := eng.loaderStore.LoaderForPayload(domain.PayloadCode("PART-D"), domain.RoleConsume)
	if err != nil || l == nil {
		t.Fatalf("resolve consume loader: loader=%v err=%v", l, err)
	}
	eng.offerUnloaderFulls(l, []domain.PayloadCode{"PART-D"})
	assertNoAskSent(t, eng)

	ords, err := db.ListActiveOrdersByProcessNode(nodeID)
	if err != nil {
		t.Fatalf("list orders: %v", err)
	}
	for _, o := range ords {
		if o.OrderType == orders.TypeRetrieve {
			t.Errorf("blank-inbound unloader must not auto-pull a full; found retrieve order %d", o.ID)
		}
	}
}

// TestTryCreateL1_NoInboundSkips is the symmetric loader gate: a produce loader
// with neither a buffer dest nor an inbound source has no empties to pull, so the
// automatic L1 fires nothing (the operator stages empties at the window directly).
func TestTryCreateL1_NoInboundSkips(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	_, nodeID, _ := seedLoaderNode(t, db, "DL-PROC", "DL-NODE")
	seedCoreLoader(t, eng, blankInboundLoaderInfo("DL-NODE", "produce", "PART-DL"))

	loader := resolveLoader(t, eng, "PART-DL") // produce-role resolve
	if created, err := eng.stageOperatorEmpty(loader, "PART-DL", 1, "", orders.Origin{}); err != nil || created != 0 {
		t.Errorf("blank-inbound loader: created=%d err=%v, want 0, nil", created, err)
	}
	ords, _ := db.ListActiveOrdersByProcessNode(nodeID)
	for _, o := range ords {
		if o.RetrieveEmpty {
			t.Errorf("blank-inbound loader must not auto-create an L1; found order %d", o.ID)
		}
	}
}

// TestRequestFullBin_PullsFromTheUnloadersInboundSource pins where the
// operator's REQUEST FULL at an unloader window pulls from: the unloader's own
// inbound source, Core's copy of it (the ask names none and does not ask for
// any source), which a blank would turn into Core's global FIFO scan.
func TestRequestFullBin_PullsFromTheUnloadersInboundSource(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	core := answerLoaderAsks(t, eng)
	nodeID := coreUnloaderWindow(t, eng, "RFS-W1", protocol.LoaderInfo{
		Name: "RFS", LoaderKey: "loader:RFS", Role: "consume", Layout: "shared_window",
		Replenishment: protocol.LoaderReplenishmentOperator, InboundSource: "FG-SUPER",
		OutboundDest: "EMPTY-TOTES",
		Positions:    []protocol.LoaderPosition{{CoreNodeName: "RFS-W1"}},
		Payloads:     []protocol.LoaderPayloadInfo{{PayloadCode: "PART-A"}},
	})
	holdSweep(t, eng, nil)

	order, err := eng.RequestFullBin(nodeID, "PART-A")
	if err != nil || order == nil {
		t.Fatalf("RequestFullBin = %+v, %v; want one full ordered", order, err)
	}
	if order.SourceNode != "FG-SUPER" {
		t.Errorf("full ordered from %q, want the unloader's inbound source FG-SUPER", order.SourceNode)
	}
	ask := core.lastAsk()
	if ask.Direction != protocol.LoaderRetrieveFull || ask.Origin != protocol.LoaderAskOperator ||
		ask.SourceNode != "" || ask.AnySource || !ask.SkipAutoConfirm || ask.MemberNode != "RFS-W1" {
		t.Errorf("ask = %+v, want a person's full at RFS-W1 from the unloader's own source, kept off Core's sweep", ask)
	}
}

// assertNoAskSent fails when eng's outbox holds an ask: the Edge asked Core
// for something it should not have.
func assertNoAskSent(t *testing.T, eng *Engine) {
	t.Helper()
	msgs, err := eng.db.ListPendingOutbox(100)
	if err != nil {
		t.Fatalf("list outbox: %v", err)
	}
	for _, m := range msgs {
		if m.MsgType == protocol.SubjectEdgeAsk {
			t.Errorf("outbox holds an ask; want none")
		}
	}
}
