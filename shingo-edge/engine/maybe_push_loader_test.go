package engine

import "testing"

// TestMaybePushLoader_StagesOneEmptyForOperatorDrivenLoaderOnly pins the
// loader-side opportunistic staging: an operator-driven loader gets exactly one
// empty staged (idempotent while in flight); a (configured) threshold loader gets
// none (its empties come from the threshold path).
func TestMaybePushLoader_StagesOneEmptyForOperatorDrivenLoaderOnly(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	_, nodeID, _ := seedActiveManualSwapLoader(t, db, "PUSH-PROC", "PUSH-LOADER", "PART-P")
	// Threshold-driven WITH a configured threshold → no operator push.
	seedCoreLoader(t, eng, sharedLoaderInfo("PUSH-LOADER", "produce", "threshold", "PART-P", 0, 100))

	countEmpties := func() int {
		ords, err := db.ListActiveOrdersByProcessNode(nodeID)
		if err != nil {
			t.Fatalf("list orders: %v", err)
		}
		n := 0
		for _, o := range ords {
			if o.RetrieveEmpty {
				n++
			}
		}
		return n
	}

	// Configured threshold loader → no opportunistic staging (Core's threshold
	// monitor supplies it).
	eng.MaybePushLoader(nodeID)
	if got := countEmpties(); got != 0 {
		t.Fatalf("configured threshold loader must not auto-stage, got %d", got)
	}

	// Mark operator-driven (replenishment=operator) → one empty staged.
	seedCoreLoader(t, eng, sharedLoaderInfo("PUSH-LOADER", "produce", "operator", "PART-P", 0, 0))
	eng.MaybePushLoader(nodeID)
	if got := countEmpties(); got != 1 {
		t.Fatalf("transitional loader should stage exactly 1 empty, got %d", got)
	}
	// Idempotent while one is in flight.
	eng.MaybePushLoader(nodeID)
	if got := countEmpties(); got != 1 {
		t.Errorf("must not stage a 2nd empty while one is in flight, got %d", got)
	}

	// The staged empty is payload-AGNOSTIC: a generic carrier with no payload
	// tag. The operator binds the real payload at LoadBin; an opportunistic
	// stage has no payload-specific demand to name.
	ords, _ := db.ListActiveOrdersByProcessNode(nodeID)
	for _, o := range ords {
		if o.RetrieveEmpty && o.PayloadCode != "" {
			t.Errorf("expected staged empty to be payload-agnostic (blank), got %q", o.PayloadCode)
		}
	}
}

// TestSweepPushLoaders_OnlyOperatorStagedLoaders pins that the startup sweep
// stages for operator-driven produce loaders and skips configured threshold ones.
func TestSweepPushLoaders_OnlyOperatorStagedLoaders(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	_, opNode, _ := seedActiveManualSwapLoader(t, db, "T-PROC", "T-LOADER", "PART-A")
	_, thrNode, _ := seedActiveManualSwapLoader(t, db, "P-PROC", "P-LOADER", "PART-B")
	// T-LOADER operator-driven; P-LOADER threshold-driven WITH a threshold configured.
	seedCoreLoader(t, eng,
		sharedLoaderInfo("T-LOADER", "produce", "operator", "PART-A", 0, 0),
		sharedLoaderInfo("P-LOADER", "produce", "threshold", "PART-B", 0, 100))

	eng.SweepPushLoaders()

	countEmpties := func(nodeID int64) int {
		ords, _ := db.ListActiveOrdersByProcessNode(nodeID)
		n := 0
		for _, o := range ords {
			if o.RetrieveEmpty {
				n++
			}
		}
		return n
	}
	if got := countEmpties(opNode); got != 1 {
		t.Errorf("operator-driven loader: want 1 staged empty, got %d", got)
	}
	if got := countEmpties(thrNode); got != 0 {
		t.Errorf("configured threshold loader: want 0 staged, got %d", got)
	}
}

// TestMaybePushLoader_ThresholdWithoutThresholdFallsBackToStaging pins the
// fallback: a loader set to replenishment=threshold but with NO threshold
// configured would be silently starved (Core never signals it), so it falls back
// to operator staging — exactly one empty staged.
func TestMaybePushLoader_ThresholdWithoutThresholdFallsBackToStaging(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	_, nodeID, _ := seedActiveManualSwapLoader(t, db, "FB-PROC", "FB-LOADER", "PART-F")
	// threshold mode, but uop_threshold=0 → misconfigured → fall back to staging.
	seedCoreLoader(t, eng, sharedLoaderInfo("FB-LOADER", "produce", "threshold", "PART-F", 0, 0))

	eng.MaybePushLoader(nodeID)

	n := 0
	ords, _ := db.ListActiveOrdersByProcessNode(nodeID)
	for _, o := range ords {
		if o.RetrieveEmpty {
			n++
		}
	}
	if n != 1 {
		t.Errorf("threshold-with-no-threshold must fall back to staging 1 empty, got %d", n)
	}
}
