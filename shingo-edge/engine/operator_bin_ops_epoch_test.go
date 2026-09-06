package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store"
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
	err := eng.LoadBin(nodeID, "PART-A", declaredUOP(100), manifest)

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

// declaredUOP is a count somebody took, as LoadBin wants it: a pointer, so
// that "they declared this many" and "nobody declared one" are different
// values rather than the same integer read two ways.
func declaredUOP(n int64) *int64 { return &n }

// loadBinWireProbe stands in for Core on the bin-load call and records the
// request exactly as it arrived, so a test can tell a count that was DECLARED
// ZERO from one that was never declared at all. Decoding into BinLoadRequest is
// what makes that visible: the field is a pointer, so absence is nil and a
// declared zero is a pointer to zero.
type loadBinWireProbe struct {
	got          *BinLoadRequest
	manifestHits int
	resolvedUOP  int
}

func (p *loadBinWireProbe) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/bin-load"):
			var req BinLoadRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			p.got = &req
			_ = json.NewEncoder(w).Encode(BinLoadResponse{
				Status: "ok", BinID: 42, PayloadCode: "PART-A",
				UOPRemaining: p.resolvedUOP, DeltaEpoch: 1,
			})
		case strings.HasSuffix(r.URL.Path, "/manifest"):
			p.manifestHits++
			_ = json.NewEncoder(w).Encode(PayloadManifestResponse{
				UOPCapacity: 40,
				Items:       []ManifestItem{{PartNumber: "PN-1", PartsPerCycle: 5}},
			})
		default:
			_ = json.NewEncoder(w).Encode([]NodeBinInfo{{NodeName: "LOADER", Occupied: true, PayloadCode: ""}})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func loadBinTestEngine(t *testing.T, srv *httptest.Server) (*Engine, *store.DB, int64) {
	t.Helper()
	db := testEngineDB(t)
	_, nodeID, _ := seedActiveManualSwapLoader(t, db, "SNF2", "LOADER", "PART-A")
	eng := testEngine(t, db)
	eng.SetInventoryDeltaSink(&fakeDeltaSink{db: db})
	eng.coreClient = NewCoreClient(srv.URL)
	return eng, db, nodeID
}

// TestLoadBin_UndeclaredCountTravelsAsAbsence: nobody declared a count, so the
// field is omitted and Core answers from the payload's standard pack.
//
// It also pins that the Edge does NOT look the template up itself. It used to,
// and that was the defect's first half: FetchPayloadManifest reports every
// failure as a nil result, so the lookup produced 0 whenever it did not work,
// and 0 was the same value the wire used for "undeclared" — one number, three
// meanings, and no way back.
func TestLoadBin_UndeclaredCountTravelsAsAbsence(t *testing.T) {
	t.Parallel()

	probe := &loadBinWireProbe{resolvedUOP: 40}
	eng, _, nodeID := loadBinTestEngine(t, probe.server(t))

	_ = eng.LoadBin(nodeID, "PART-A", nil,
		[]protocol.IngestManifestItem{{PartNumber: "PN-1", Quantity: 250}})

	if probe.got == nil {
		t.Fatal("Core never received a bin-load request")
	}
	if probe.got.UOPCount != nil {
		t.Errorf("uop_count = %d, want it ABSENT — an undeclared count is a question for Core, "+
			"and sending a number makes it indistinguishable from one somebody counted",
			*probe.got.UOPCount)
	}
	if probe.manifestHits != 0 {
		t.Errorf("Edge fetched the payload template %d time(s); it must not — Core owns that "+
			"resolution now, and a second answer here is the one with no way to report failure",
			probe.manifestHits)
	}
}

// TestLoadBin_DeclaredCountTravelsAsGiven: the operator counted, so their
// number goes on the wire untouched. This is the operator-authorship rule at
// the wire: a count somebody took is not improved by anything downstream.
func TestLoadBin_DeclaredCountTravelsAsGiven(t *testing.T) {
	t.Parallel()

	probe := &loadBinWireProbe{resolvedUOP: 250}
	eng, _, nodeID := loadBinTestEngine(t, probe.server(t))

	declared := int64(250)
	_ = eng.LoadBin(nodeID, "PART-A", &declared,
		[]protocol.IngestManifestItem{{PartNumber: "PN-1", Quantity: 250}})

	if probe.got == nil || probe.got.UOPCount == nil {
		t.Fatal("uop_count was absent; the operator declared 250")
	}
	if *probe.got.UOPCount != declared {
		t.Errorf("uop_count = %d, want %d", *probe.got.UOPCount, declared)
	}
}

// TestLoadBin_DeclaredZeroIsNotAbsence is the state the old wire could not
// carry. Somebody counted the carrier and it held nothing; that is an answer,
// and it must not arrive looking like the question.
func TestLoadBin_DeclaredZeroIsNotAbsence(t *testing.T) {
	t.Parallel()

	probe := &loadBinWireProbe{resolvedUOP: 0}
	eng, _, nodeID := loadBinTestEngine(t, probe.server(t))

	declared := int64(0)
	_ = eng.LoadBin(nodeID, "PART-A", &declared,
		[]protocol.IngestManifestItem{{PartNumber: "PN-1", Quantity: 250}})

	if probe.got == nil {
		t.Fatal("Core never received a bin-load request")
	}
	if probe.got.UOPCount == nil {
		t.Fatal("uop_count was ABSENT, want a declared 0 — omitting it asks Core for the " +
			"standard pack, which is how a bin counted as empty comes back full")
	}
	if *probe.got.UOPCount != 0 {
		t.Errorf("uop_count = %d, want 0", *probe.got.UOPCount)
	}
}

// TestLoadBin_SeatsTheCountCoreResolved is the regression pin for the seam
// defect: Core resolves an undeclared count from the standard pack and returns
// what it wrote, and THAT is what the node must be seated with.
//
// Seating the request's own value instead left Core holding a full carrier and
// the Edge holding a starved node, for the same bin, at the same moment — the
// policy-number-in-a-measurement-field shape the capacity seed was deleted for,
// arriving by a different door.
func TestLoadBin_SeatsTheCountCoreResolved(t *testing.T) {
	t.Parallel()

	const coreResolved = 4500
	probe := &loadBinWireProbe{resolvedUOP: coreResolved}
	eng, db, nodeID := loadBinTestEngine(t, probe.server(t))

	// Undeclared: the request carries nothing, so the only count in the system
	// is the one Core sends back.
	_ = eng.LoadBin(nodeID, "PART-A", nil,
		[]protocol.IngestManifestItem{{PartNumber: "PN-1", Quantity: 250}})

	rt, err := db.GetProcessNodeRuntime(nodeID)
	if err != nil || rt == nil {
		t.Fatalf("read runtime: %v", err)
	}
	if rt.RemainingUOPCached != coreResolved {
		t.Errorf("remaining = %d, want %d — Core resolved and wrote %d for this bin; "+
			"seating anything else puts the Edge and the ledger into disagreement about "+
			"a carrier both of them can see", rt.RemainingUOPCached, coreResolved, coreResolved)
	}
}
