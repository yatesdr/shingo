package engine

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/service"
	"shingoedge/store/catalog"
)

// Tier 1 gates — "one read-model" (collapse the dual loader-resolution layer).
// These guard the two correctness bugs the refactor closes plus the prerequisites
// the deletion depends on. See TIER1-BRIEF.md.

// Gate 1 (BUG-1, never-2N mutex hole): the operator path (RequestEmptyBin) and the
// automatic path (tryCreateL1 via the demand resolver) must lock the SAME per-loader
// mutex for one physical loader, or the seam doesn't mutually exclude them. The seam
// keys loaderResvLock on string(loader.ID()); pre-fix the operator path built its loader
// from the claim (ID = node NAME) while the demand path used the aggregate (ID = token),
// so the two fired through different mutexes. This counts the distinct reservation-lock
// keys used for one loader after driving BOTH paths — must be exactly 1. Deterministic
// (no race): pre-fix it observes 2 keys, post-fix 1.
func TestTier1_BUG1_OperatorAndDemandShareLockKey(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)

	// Single-window real-node loader: node "PLK_X1", aggregate token "loader:PLK_X1".
	nodeID := seedCapManualSwap(t, db, "PROC-X1", "PLK_X1", protocol.ClaimRoleProduce, []string{"PART-X"}, 2, false)
	seedCoreLoader(t, eng, sharedLoaderInfo("PLK_X1", "produce", "auto", "PART-X", 2, 0))

	// Operator path: RequestEmptyBin resolves the loader and reserves through the seam.
	if _, err := eng.RequestEmptyBin(nodeID, "PART-X"); err != nil {
		t.Fatalf("RequestEmptyBin: %v", err)
	}
	// Automatic path: the demand resolver + tryCreateL1 reserve through the same seam.
	dl, _ := eng.findLoaderForDemand("PLK_X1", "PART-X")
	if dl == nil {
		t.Fatal("findLoaderForDemand(PLK_X1, PART-X) = nil")
	}
	if _, err := eng.tryCreateL1(dl, "PART-X", L1SideCycle, 1, ""); err != nil {
		t.Fatalf("tryCreateL1: %v", err)
	}

	var keys []string
	eng.loaderResv.Range(func(k, _ any) bool { keys = append(keys, k.(string)); return true })
	if len(keys) != 1 {
		t.Errorf("reservation lock used %d distinct keys %v for one physical loader; want 1 "+
			"(operator + automatic paths must share the loader_key token, or never-2N is not enforced — BUG-1)", len(keys), keys)
	}
	if len(keys) == 1 && keys[0] != "loader:PLK_X1" {
		t.Errorf("lock key = %q, want the loader_key token loader:PLK_X1 (not the node name)", keys[0])
	}
}

// Gate 2 (BUG-2, dead C-push skip): the legacy DemandSignal path
// (MaybeCreateLoaderEmptyIn) must SKIP a payload that opted into UOP-threshold C-push,
// deferring it to HandleLoopBelowThreshold. Pre-fix the skip read a node-keyed cache
// lookup with the loader_key token, which never matched → the skip was dead → the
// bin-count path fired anyway. Seed BOTH a bin-count floor (min_stock=2) and a UOP
// threshold (>0); with no core client the system count is 0 (< floor), so the bin-count
// path WOULD fire if the skip were dead. The process node exists so a (buggy) fire can
// actually create an order — making the zero-order assertion non-vacuous.
func TestTier1_BUG2_ThresholdOptedPayloadSkipsBinCount(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)

	nodeID := seedCapManualSwap(t, db, "PROC-TH", "TH-LOADER", protocol.ClaimRoleProduce, []string{"PART-T"}, 2, false)
	seedCoreLoader(t, eng, sharedLoaderInfo("TH-LOADER", "produce", "auto", "PART-T", 2, 100))

	eng.MaybeCreateLoaderEmptyIn("TH-LOADER", "PART-T")

	if got := capActiveOrders(t, db, nodeID, true); len(got) != 0 {
		t.Errorf("threshold-opted payload fired %d bin-count L1(s) via the DemandSignal path; "+
			"want 0 — the C-push skip must hold (BUG-2)", len(got))
	}
}

// Gate 3 (prerequisite A landed): the per-payload UOP threshold and the transitional
// flag must reach the *domain.Loader snapshot through projectCoreLoader and survive a
// SetCoreLoaders refresh. Without the new uopThreshold projection the threshold silently
// reads 0 and BUG-2's fix inverts to "threshold-opted but bin-count-only."
func TestTier1_SnapshotCarriesThresholdAndTransitional(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)

	seedCoreLoader(t, eng,
		sharedLoaderInfo("AUTO-LDR", "produce", "auto", "PART-A", 2, 150),
		sharedLoaderInfo("OP-LDR", "produce", "operator", "PART-B", 1, 0),
	)

	a, err := eng.loaders().LoaderForPayload("PART-A", domain.RoleProduce, true)
	if err != nil || a == nil {
		t.Fatalf("resolve AUTO-LDR: %v", err)
	}
	if got := a.UOPThresholdFor("PART-A"); got != 150 {
		t.Errorf("UOPThresholdFor(PART-A) = %d, want 150 (threaded off the cache)", got)
	}
	if a.IsTransitional() {
		t.Error("replenishment=auto loader must not be transitional")
	}

	op, err := eng.loaders().LoaderForPayload("PART-B", domain.RoleProduce, true)
	if err != nil || op == nil {
		t.Fatalf("resolve OP-LDR: %v", err)
	}
	if !op.IsTransitional() {
		t.Error("replenishment=operator loader must be transitional")
	}
	if got := op.UOPThresholdFor("PART-B"); got != 0 {
		t.Errorf("UOPThresholdFor(PART-B) = %d, want 0 (none seeded)", got)
	}

	// Survives a refresh: the field is re-threaded, not stale.
	seedCoreLoader(t, eng, sharedLoaderInfo("AUTO-LDR", "produce", "auto", "PART-A", 2, 222))
	a2, _ := eng.loaders().LoaderForPayload("PART-A", domain.RoleProduce, true)
	if a2 == nil || a2.UOPThresholdFor("PART-A") != 222 {
		t.Errorf("after refresh UOPThresholdFor(PART-A) = %v, want 222", a2)
	}
}

// Gate 4 (dedicated layout preserved): the path RequestEmptyBin/the push now resolve
// through — e.loaders().LoaderAt — must keep a dedicated loader's dedicated_positions
// layout and route a member-named reservation to THAT position (O2). The deleted shim
// flattened every loader to shared_window; this pins the model the aggregate preserves.
func TestTier1_DedicatedLoaderKeepsLayoutViaAggregate(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)

	seedCoreLoader(t, eng, protocol.LoaderInfo{
		Name: "DECK", LoaderKey: "loader:DECK", Role: "produce", Layout: "dedicated_positions",
		Replenishment: "auto", ConfigGen: 1,
		Positions: []protocol.LoaderPosition{
			{CoreNodeName: "D1", PayloadCode: "STUD", Kind: "dedicated", MinStock: 2},
			{CoreNodeName: "D2", PayloadCode: "STUD", Kind: "dedicated", MinStock: 2},
		},
	})

	l, err := eng.loaders().LoaderAt("D2", domain.RoleProduce)
	if err != nil || l == nil {
		t.Fatalf("LoaderAt(D2) = %v, %v", l, err)
	}
	if !l.IsDedicated() {
		t.Errorf("layout = %v, want dedicated_positions (the shim flattened to shared_window)", l.Layout())
	}
	// O2 member routing: a reservation naming D2 routes to D2, not first-match D1.
	nodes, budget := l.ReservationTarget("D2", "STUD", eng.multiWindowEnabled())
	if len(nodes) != 1 || nodes[0] != "D2" || budget != 1 {
		t.Errorf("ReservationTarget(member=D2) = %v/%d, want [D2]/1", nodes, budget)
	}
}

// Gate 6 (calculator capacity from catalog): after the shim deletion, the threshold
// calculator must source per-bin capacity from the payload catalog (not the deleted
// manual_swap shim claim, where it was synthesized as 0). Assert the result echoes the
// catalog's non-zero UOPCapacity. Pre-fix this annotation was silently 0.
func TestTier1_CalculatorCapacityFromCatalog(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.catalogService = service.NewCatalogService(db) // testEngine leaves it nil; production sets it in engine.New

	testutil.MustNoErr(t, db.UpsertPayloadCatalog(&catalog.CatalogEntry{
		ID: 1, Name: "Calc Part", Code: "CALC-PART", UOPCapacity: 345,
	}), "upsert catalog")

	res, err := eng.CalculateThresholdForLoader(CalculateInput{
		CoreNodeName:   "ANY",
		PayloadCode:    "CALC-PART",
		DateRangeStart: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		DateRangeEnd:   time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC),
		SafetyFactor:   1.0,
		CycleSeconds:   10,
	})
	if err != nil {
		t.Fatalf("CalculateThresholdForLoader: %v", err)
	}
	if res.Inputs.BinCapacityUOP != 345 {
		t.Errorf("BinCapacityUOP = %d, want 345 (sourced from the payload catalog)", res.Inputs.BinCapacityUOP)
	}
}
