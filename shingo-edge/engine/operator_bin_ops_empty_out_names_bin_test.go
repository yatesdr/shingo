package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/store/processes"
)

// operator_bin_ops_empty_out_names_bin_test.go — the unloader's empty-out names
// the carrier it moves: the bin the CLEAR cleared, or the bin PUSH EMPTY read
// on the window. Core then lifts that bin and no other, and types the order by
// it before dispatch claims it (orders.named_bin_id).

const (
	emptyOutReadBinID    = 41 // what the node-bins read sees on the window
	emptyOutClearedBinID = 42 // what the clear answers it cleared
)

// emptyOutCoreServer answers the two reads the empty-out's doors make, each
// with its own bin id, so a test can tell which one the order names.
func emptyOutCoreServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/telemetry/node-bins":
			var rows []map[string]any
			for _, node := range strings.Split(r.URL.Query().Get("nodes"), ",") {
				rows = append(rows, map[string]any{"node_name": node, "occupied": true, "bin_id": emptyOutReadBinID})
			}
			_ = json.NewEncoder(w).Encode(rows)
		case "/api/telemetry/bin-clear":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "bin_id": emptyOutClearedBinID,
				"cleared_payload_code": "PART-U"})
		default:
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// emptyOutMoves returns the move order.requests from the window in the outbox.
func emptyOutMoves(t *testing.T, eng *Engine, window string) []protocol.OrderRequest {
	t.Helper()
	var moves []protocol.OrderRequest
	for _, m := range findOutboxByType(t, eng.db, protocol.TypeOrderRequest) {
		var env protocol.Envelope
		testutil.MustNoErr(t, json.Unmarshal(m.Payload, &env), "unmarshal envelope")
		var req protocol.OrderRequest
		testutil.MustNoErr(t, env.DecodePayload(&req), "decode order request")
		if req.OrderType == orders.TypeMove && req.SourceNode == window {
			moves = append(moves, req)
		}
	}
	return moves
}

// emptyOutWindow is a Core-owned unloader window with an outbound.
func emptyOutWindow(t *testing.T, window string) (*Engine, int64) {
	t.Helper()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.coreClient = stubCoreClient(emptyOutCoreServer(t).URL)
	procID, err := db.CreateProcess(window+"-PROC", "", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: procID, CoreNodeName: window, Code: window, Name: window, Sequence: 1, Enabled: true,
	})
	testutil.MustNoErr(t, err, "create node")
	_, err = db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "ensure runtime")
	info := sharedLoaderInfo(window, "consume", "operator", "PART-U", 0, 0)
	info.OutboundDest = "EMPTY-TOTES"
	seedCoreLoader(t, eng, info)
	return eng, nodeID
}

// TestEmptyOut_ClearNamesTheBinItCleared: the CLEAR's empty-out names the bin
// the clear answered, the carrier whose life it just ended.
func TestEmptyOut_ClearNamesTheBinItCleared(t *testing.T) {
	t.Parallel()
	const window = "EON-W1"
	eng, nodeID := emptyOutWindow(t, window)
	testutil.MustNoErr(t, eng.ClearBin(nodeID, "", false), "ClearBin")

	moves := emptyOutMoves(t, eng, window)
	if len(moves) != 1 {
		t.Fatalf("empty-out order.requests from %s = %d, want 1", window, len(moves))
	}
	if moves[0].BinID != emptyOutClearedBinID {
		t.Errorf("the CLEAR's empty-out names bin %d, want %d, the bin the clear cleared",
			moves[0].BinID, emptyOutClearedBinID)
	}
	if moves[0].PayloadCode != "" {
		t.Errorf("the empty-out names part %q, want none", moves[0].PayloadCode)
	}
}

// TestEmptyOut_PushEmptyNamesTheBinItRead: PUSH EMPTY's empty-out names the bin
// its own node-bins read found empty on the window.
func TestEmptyOut_PushEmptyNamesTheBinItRead(t *testing.T) {
	t.Parallel()
	const window = "EON-W2"
	eng, nodeID := emptyOutWindow(t, window)
	testutil.MustNoErr(t, eng.PushEmptyOut(nodeID), "PushEmptyOut")

	moves := emptyOutMoves(t, eng, window)
	if len(moves) != 1 {
		t.Fatalf("empty-out order.requests from %s = %d, want 1", window, len(moves))
	}
	if moves[0].BinID != emptyOutReadBinID {
		t.Errorf("PUSH EMPTY's empty-out names bin %d, want %d, the bin its read found on the window",
			moves[0].BinID, emptyOutReadBinID)
	}
	if moves[0].PayloadCode != "" {
		t.Errorf("the empty-out names part %q, want none", moves[0].PayloadCode)
	}
}
