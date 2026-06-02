package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"shingo/protocol"
)

// TestRegression_LoadBin_SeedsActiveBinEpochFromCoreResponse pins the fix in
// 6630c85: engine.LoadBin must thread Core's LoadBin DeltaEpoch into the runtime
// write rather than drop it. Pre-fix, manually-loaded bins landed at epoch 0, so
// Core rejected their BinUOPDeltas via the epoch-aware dedup guard.
//
// This exercises the fallback (ManualLoad) path — no L1 retrieve_empty is in
// flight — and asserts the epoch reached the inventory-delta sink. Downstream L2
// side-cycle creation is out of scope; ManualLoad is invoked before any L2 work,
// so the assertion doesn't depend on LoadBin's overall return.
func TestRegression_LoadBin_SeedsActiveBinEpochFromCoreResponse(t *testing.T) {
	t.Parallel()

	const wantEpoch = 7
	// Core stub: POST = LoadBin (returns the delta epoch); GET = FetchNodeBins
	// (an occupied, empty bin so LoadBin's occupancy gate passes).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_ = json.NewEncoder(w).Encode(BinLoadResponse{
				Status: "ok", BinID: 42, PayloadCode: "PART-A", UOPRemaining: 100, DeltaEpoch: wantEpoch,
			})
			return
		}
		_ = json.NewEncoder(w).Encode([]NodeBinInfo{{NodeName: "LOADER", Occupied: true, PayloadCode: ""}})
	}))
	defer srv.Close()

	db := testEngineDB(t)
	_, nodeID, _ := seedActiveManualSwapLoader(t, db, "SNF2", "LOADER", "PART-A")

	eng := testEngine(t, db)
	sink := &fakeDeltaSink{db: db}
	eng.SetInventoryDeltaSink(sink)
	eng.coreClient = NewCoreClient(srv.URL)

	manifest := []protocol.IngestManifestItem{{PartNumber: "PN-1", Quantity: 100, Description: "x"}}
	err := eng.LoadBin(nodeID, "PART-A", 100, manifest)

	if len(sink.manualLoadCalls) != 1 {
		t.Fatalf("expected exactly 1 ManualLoad call, got %d (LoadBin err=%v)", len(sink.manualLoadCalls), err)
	}
	if got := sink.manualLoadCalls[0].Epoch; got != wantEpoch {
		t.Errorf("ManualLoad epoch = %d, want %d — Core's LoadBin DeltaEpoch must seed active_bin_epoch, not 0", got, wantEpoch)
	}
}
