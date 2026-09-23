package engine

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/store/processes"
)

// TestEmptyOut_EnvelopeNamesNoPart: CLEAR at a Core-owned unloader window
// holding PART-U sends a U2 whose order.request carries payload_code "". A named
// part makes Core judge the carrier against it (tier 4), turns a home removal
// into a pool Drain (tier 2), and gates the destination through
// payloadAllowedAt — see createUnloaderEmptyOut. lookupPayloadMeta does not
// backfill a node with no stored claim, so the envelope carries exactly what
// createUnloaderEmptyOut passed. Red before D2 ("PART-U").
func TestEmptyOut_EnvelopeNamesNoPart(t *testing.T) {
	t.Parallel()
	const window = "D2-UNL-W1"
	srv := fakeCoreBinServer(t, true, "PART-U")
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.coreClient = NewCoreClient(srv.URL)

	procID, err := db.CreateProcess("D2-PROC", "", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: procID, CoreNodeName: window, Code: "D2", Name: window, Sequence: 1, Enabled: true,
	})
	testutil.MustNoErr(t, err, "create node")
	_, err = db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "ensure runtime")
	info := sharedLoaderInfo(window, "consume", "operator", "PART-U", 0, 0)
	info.OutboundDest = "EMPTY-TOTES"
	seedCoreLoader(t, eng, info)

	testutil.MustNoErr(t, eng.ClearBin(nodeID, ""), "ClearBin")

	var moves []protocol.OrderRequest
	for _, m := range findOutboxByType(t, db, protocol.TypeOrderRequest) {
		var env protocol.Envelope
		testutil.MustNoErr(t, json.Unmarshal(m.Payload, &env), "unmarshal envelope")
		var req protocol.OrderRequest
		testutil.MustNoErr(t, env.DecodePayload(&req), "decode order request")
		if req.OrderType == orders.TypeMove && req.SourceNode == window {
			moves = append(moves, req)
		}
	}
	if len(moves) != 1 {
		t.Fatalf("U2 order.request envelopes from %s = %d, want 1", window, len(moves))
	}
	if moves[0].PayloadCode != "" {
		t.Errorf("U2 envelope payload_code = %q, want \"\" — the empty-out is a removal of the "+
			"carrier on the window, never a fetch for the part that was cleared", moves[0].PayloadCode)
	}
}
