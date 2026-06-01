package engine

import "testing"

// TestMaybePushLoader_StagesOneEmptyForTransitionalLoaderOnly pins the
// loader-side opportunistic staging: a transitional loader gets exactly one
// empty staged (idempotent while in flight); a non-transitional loader gets
// none (its empties come from the threshold/legacy paths).
func TestMaybePushLoader_StagesOneEmptyForTransitionalLoaderOnly(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	_, nodeID, _ := seedActiveManualSwapLoader(t, db, "PUSH-PROC", "PUSH-LOADER", "PART-P")

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

	// Not transitional → no opportunistic staging.
	eng.MaybePushLoader(nodeID)
	if got := countEmpties(); got != 0 {
		t.Fatalf("non-transitional loader must not auto-stage, got %d", got)
	}

	// Mark transitional → one empty staged.
	if err := db.SetTransitionalLoader("PUSH-LOADER", true, "test"); err != nil {
		t.Fatalf("set transitional: %v", err)
	}
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

// TestSweepPushLoaders_OnlyTransitionalProduceLoaders pins that the startup
// sweep stages for transitional produce loaders and skips ordinary ones.
func TestSweepPushLoaders_OnlyTransitionalProduceLoaders(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	_, transNode, _ := seedActiveManualSwapLoader(t, db, "T-PROC", "T-LOADER", "PART-A")
	_, plainNode, _ := seedActiveManualSwapLoader(t, db, "P-PROC", "P-LOADER", "PART-B")
	if err := db.SetTransitionalLoader("T-LOADER", true, "test"); err != nil {
		t.Fatalf("set transitional: %v", err)
	}

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
	if got := countEmpties(transNode); got != 1 {
		t.Errorf("transitional loader: want 1 staged empty, got %d", got)
	}
	if got := countEmpties(plainNode); got != 0 {
		t.Errorf("non-transitional loader: want 0 staged, got %d", got)
	}
}
