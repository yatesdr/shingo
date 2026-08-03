package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
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

// TestClearBin_TakesTheEpochCoreReturns closes the consume-side clear route.
//
// Clearing a carrier for reuse starts a new life for it on Core: Core bumps the
// version stamp and hands the new one straight back in the reply. The Edge threw
// the whole reply away and kept the old stamp, so from that moment every count it
// reported for that carrier was discarded — the same loss the admin-adjustment
// path had, arriving through a different door.
func TestClearBin_TakesTheEpochCoreReturns(t *testing.T) {
	t.Parallel()

	const clearedBin = int64(4242)
	const boundEpoch, wantEpoch = int64(7), int64(8)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/telemetry/bin-clear" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "ok", "bin_id": clearedBin, "delta_epoch": wantEpoch,
			})
			return
		}
		// node-bins: the window holds the carrier about to be cleared.
		_ = json.NewEncoder(w).Encode([]map[string]any{{"occupied": true, "payload_code": "PART-CE"}})
	}))
	defer srv.Close()

	db := testEngineDB(t)
	nodeID, _ := seedManualSwapClaim(t, db, "CLR-EPOCH", "consume", "PART-CE", "EMPTY-TOTES")

	eng := testEngine(t, db)
	eng.SetInventoryDeltaSink(&fakeDeltaSink{db: db})
	eng.coreClient = NewCoreClient(srv.URL)

	rt, err := db.EnsureProcessNodeRuntime(nodeID)
	if err != nil || rt == nil {
		t.Fatalf("ensure runtime: %v", err)
	}
	bin := clearedBin
	if err := db.SetProcessNodeRuntimeWithBinAndEpoch(nodeID, rt.ActiveClaimID, &bin, boundEpoch, 25); err != nil {
		t.Fatalf("bind the carrier at its current stamp: %v", err)
	}

	testutil.MustNoErr(t, eng.ClearBin(nodeID, ""), "ClearBin")

	rt, err = db.GetProcessNodeRuntime(nodeID)
	if err != nil || rt == nil {
		t.Fatalf("read runtime: %v", err)
	}
	if rt.ActiveBinEpoch != wantEpoch {
		t.Errorf("epoch = %d, want %d — Core bumped the stamp on the clear and sent it back "+
			"in the same reply; keeping the old one means every count reported for this "+
			"carrier from here on is discarded", rt.ActiveBinEpoch, wantEpoch)
	}
	if rt.RemainingUOPCached != 0 {
		t.Errorf("remaining = %d, want 0 — the clear still zeroes the count", rt.RemainingUOPCached)
	}
}

// TestClearBin_IgnoresTheEpochWhenADifferentCarrierIsBound guards the direction
// that would corrupt: Core resolves which carrier to clear from its own view of
// the node, so a reply can name a carrier the Edge is not pointing at. Adopting
// that stamp would put one carrier's generation on another's counts.
func TestClearBin_IgnoresTheEpochWhenADifferentCarrierIsBound(t *testing.T) {
	t.Parallel()

	const boundBin, clearedBin = int64(5001), int64(5002)
	const boundEpoch = int64(7)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/telemetry/bin-clear" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "ok", "bin_id": clearedBin, "delta_epoch": 99,
			})
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{"occupied": true, "payload_code": "PART-CE2"}})
	}))
	defer srv.Close()

	db := testEngineDB(t)
	nodeID, _ := seedManualSwapClaim(t, db, "CLR-EPOCH-X", "consume", "PART-CE2", "EMPTY-TOTES")

	eng := testEngine(t, db)
	eng.SetInventoryDeltaSink(&fakeDeltaSink{db: db})
	eng.coreClient = NewCoreClient(srv.URL)

	rt, err := db.EnsureProcessNodeRuntime(nodeID)
	if err != nil || rt == nil {
		t.Fatalf("ensure runtime: %v", err)
	}
	bin := boundBin
	if err := db.SetProcessNodeRuntimeWithBinAndEpoch(nodeID, rt.ActiveClaimID, &bin, boundEpoch, 25); err != nil {
		t.Fatalf("bind a different carrier: %v", err)
	}

	testutil.MustNoErr(t, eng.ClearBin(nodeID, ""), "ClearBin")

	rt, err = db.GetProcessNodeRuntime(nodeID)
	if err != nil || rt == nil {
		t.Fatalf("read runtime: %v", err)
	}
	if rt.ActiveBinEpoch != boundEpoch {
		t.Errorf("epoch = %d, want %d — the reply named carrier %d and this node is holding %d; "+
			"the stamp belongs to a carrier that is not here", rt.ActiveBinEpoch, boundEpoch, clearedBin, boundBin)
	}
}
