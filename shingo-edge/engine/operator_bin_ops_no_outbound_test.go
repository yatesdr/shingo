package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// operator_bin_ops_no_outbound_test.go — what the operator is told when a CLEAR
// at an unloader window cannot send the carrier anywhere.
//
// Two ways that happens. Core refuses the clear before doing it (a stage 1
// with no destination: "this station has nowhere to send the cart"), and the
// refusal has to reach the operator as a refusal with nothing done on the Edge.
// Or the clear commits and the Edge then finds no outbound for the U2
// (createUnloaderEmptyOut's !ok branch). That branch used to return silently:
// the operator saw a successful CLEAR while the carrier sat in the window with
// no move owed. It now says so.

// TestClearBin_NoOutboundTellsTheOperator: an unloader with no outbound clears
// on Core, creates no U2, and returns an error that says the bin WAS cleared
// and names the window — so a retry is not the operator's next move.
func TestClearBin_NoOutboundTellsTheOperator(t *testing.T) {
	t.Parallel()
	eng := testEngine(t, testEngineDB(t))
	core := newClearCaptureCore(t)
	eng.coreClient = NewCoreClient(core.srv.URL)
	info := sharedLoaderInfo("NOB-W1", "consume", "operator", "PART-HL", 0, 0)
	info.InboundSource = ""
	info.OutboundDest = ""
	nodeID := coreUnloaderWindow(t, eng, "NOB-W1", info)

	err := eng.ClearBin(nodeID, "")

	clears, _ := core.snapshot()
	if len(clears) != 1 {
		t.Fatalf("bin-clear POSTs = %d, want 1 (the clear itself still happens)", len(clears))
	}
	if moves := scMovesFrom(t, eng.db, "NOB-W1"); len(moves) != 0 {
		t.Errorf("U2s leaving NOB-W1 = %d, want 0 (there is nowhere to send it)", len(moves))
	}
	if err == nil {
		t.Fatal("ClearBin with no outbound returned nil — the operator is told the carrier left when it did not")
	}
	if msg := err.Error(); !strings.Contains(msg, "cleared") || !strings.Contains(msg, "NOB-W1") {
		t.Errorf("error = %q, want it to say the bin was cleared and name NOB-W1", msg)
	}
}

// TestClearBin_CoreRefusalReachesTheOperator: a clear Core refuses (409, before
// clearing) comes back as ClearBin's error with Core's own sentence, and the
// Edge creates no U2. Characterises the tree: this path already surfaced Core's
// refusal; the pin holds it while the stage-1 409 is added on Core.
func TestClearBin_CoreRefusalReachesTheOperator(t *testing.T) {
	t.Parallel()
	eng := testEngine(t, testEngineDB(t))
	const refusal = "this station has nowhere to send the cart"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/telemetry/bin-clear" {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": refusal})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	t.Cleanup(srv.Close)
	eng.coreClient = NewCoreClient(srv.URL)
	info := sharedLoaderInfo("NOB-R1", "consume", "operator", "PART-HL", 0, 0)
	info.InboundSource = ""
	info.OutboundDest = "EMPTY-TOTES"
	nodeID := coreUnloaderWindow(t, eng, "NOB-R1", info)

	err := eng.ClearBin(nodeID, "")
	if err == nil || !strings.Contains(err.Error(), refusal) {
		t.Fatalf("ClearBin error = %v, want Core's refusal %q", err, refusal)
	}
	if moves := scMovesFrom(t, eng.db, "NOB-R1"); len(moves) != 0 {
		t.Errorf("U2s leaving NOB-R1 = %d, want 0 (Core refused the clear)", len(moves))
	}
}
