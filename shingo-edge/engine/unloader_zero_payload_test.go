package engine

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingoedge/domain"
)

// TestUnloaderSweep_ZeroPayloadSharedWindow_ZeroReads: a shared_window
// unloader that declares no payloads (a two-stage unloader's stage-2 window) is
// offered nothing by the sweep or by a per-payload push, so it reads nothing
// and orders nothing, and an operator full request there is refused. The
// inbound source is SET, so the zeros are the payload exit and not the
// no-inbound one (TestUnloaderSweep_NoInboundSource_ZeroReads).
func TestUnloaderSweep_ZeroPayloadSharedWindow_ZeroReads(t *testing.T) {
	t.Parallel()
	db, counter := testEngineDBCounting(t)
	eng := testEngine(t, db)
	nodeID := coreUnloaderWindow(t, eng, "ZPS-W1", protocol.LoaderInfo{
		Name: "ZPS", LoaderKey: "loader:ZPS", Role: "consume", Layout: "shared_window",
		Replenishment: protocol.LoaderReplenishmentOperator, InboundSource: "FG-SUPER",
		OutboundDest: "EMPTY-TOTES", ConfigGen: 1,
		Positions: []protocol.LoaderPosition{{CoreNodeName: "ZPS-W1", Kind: "window"}},
	})
	ls, err := eng.loaderStore.Loaders(domain.RoleConsume)
	if err != nil || len(ls) != 1 || len(ls[0].PayloadSet()) != 0 {
		t.Fatalf("fixture: want one zero-payload consume loader, got %d (err %v)", len(ls), err)
	}
	stub := newSweepBinsStub(t, nil, "", false)
	eng.coreClient = NewCoreClient(stub.srv.URL)

	counter.Reset()
	eng.pushUnloadersViaSeam()
	eng.MaybeCreateUnloaderFullIn("PART-A")
	if got := stub.hits.Load(); got != 0 {
		t.Errorf("node-bins calls = %d, want 0", got)
	}
	if got := counter.Count(); got != 0 {
		t.Errorf("DB statements = %d, want 0", got)
	}

	if _, err := eng.RequestFullBin(nodeID, "PART-A"); err == nil || !strings.Contains(err.Error(), "not in allowed list") {
		t.Errorf("RequestFullBin at a zero-payload unloader = %v, want the allowed-list refusal", err)
	}
	active, err := db.ListActiveOrdersByProcessNode(nodeID)
	if err != nil {
		t.Fatalf("list orders: %v", err)
	}
	if len(active) != 0 {
		t.Errorf("orders at the window = %d, want 0", len(active))
	}
}
