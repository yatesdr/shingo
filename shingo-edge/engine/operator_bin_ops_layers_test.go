package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"shingo/protocol/testutil"
)

// TestClearBin_DeclaredLayersReachCore: the height an operator declares with
// the CLEAR (the panel's "<type> · N high" button) reaches Core as the clear's
// layers; a CLEAR that declares none sends no layers key, as before, so Core
// writes no tote number.
func TestClearBin_DeclaredLayersReachCore(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/telemetry/bin-clear" {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			bodies = append(bodies, body)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "bin_id": 91, "delta_epoch": 2})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	t.Cleanup(srv.Close)
	eng := testEngine(t, testEngineDB(t))
	bridgeCore(t, eng, srv.URL)
	info := sharedLoaderInfo("HLC-L", "consume", "operator", "PART-HL", 0, 0)
	info.InboundSource = ""
	info.OutboundDest = "EMPTY-TOTES"
	nodeID := coreUnloaderWindow(t, eng, "HLC-L", info)

	three, zero := 3, 0
	testutil.MustNoErr(t, eng.ClearBin(nodeID, "TOTE-S", false, &three), "ClearBin at 3 high")
	testutil.MustNoErr(t, eng.ClearBin(nodeID, "TOTE-S", false, &zero), "ClearBin as tops & bottoms")
	testutil.MustNoErr(t, eng.ClearBin(nodeID, "TOTE-S", false, nil), "ClearBin with no height")

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 3 {
		t.Fatalf("bin-clear POSTs = %d, want 3", len(bodies))
	}
	for i, want := range []any{float64(3), float64(0), nil} {
		got, has := bodies[i]["layers"]
		if (want == nil) == has || (want != nil && got != want) {
			t.Errorf("clear %d: layers = %v (present %v), want %v", i, got, has, want)
		}
		if bodies[i]["bin_type_code"] != "TOTE-S" {
			t.Errorf("clear %d: bin_type_code = %v, want TOTE-S", i, bodies[i]["bin_type_code"])
		}
	}
}
