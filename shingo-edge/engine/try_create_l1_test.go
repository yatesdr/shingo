package engine

import (
	"testing"

	"shingoedge/domain"
)

// resolveLoader resolves a *domain.Loader for a payload through the engine's
// LoaderStore (the Core-owned aggregate), the same way the hot path does.
func resolveLoader(t *testing.T, eng *Engine, payload string) *domain.Loader {
	t.Helper()
	l, err := eng.loaderStore.LoaderForPayload(domain.PayloadCode(payload), domain.RoleProduce, true)
	if err != nil || l == nil {
		t.Fatalf("resolve loader for %s: loader=%v err=%v", payload, l, err)
	}
	return l
}

// These tests pin tryCreateL1's contract by asserting the returned created
// count and the orders DB — not log strings (the review flagged the existing
// log-scraping tests as brittle). They cover the in-flight clamp and the
// transitional allowlist gate.

// TestTryCreateL1_BoundedByNodeWindowCapAndReturnsCreated pins the post-PR-0
// chokepoint contract: tryCreateL1 fires (desired - inFlight) for the payload
// BUT never lets total in-flight empties at the core node exceed the window's
// physical slot count (manualSwapWindowSlots). At a one-window loader that means
// at most one empty inbound at a time — a desired > 1 is serialized over the
// fill/release cycle, not queued at the window. The per-payload in-flight guard
// remains as the dedup contract; the node cap is the dominant bound.
func TestTryCreateL1_BoundedByNodeWindowCapAndReturnsCreated(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	_, nodeID, _ := seedActiveManualSwapLoader(t, db, "CLAMP-PROC", "CLAMP-LOADER", "PART-Z")
	seedCoreLoader(t, eng, sharedLoaderInfo("CLAMP-LOADER", "produce", "auto", "PART-Z", 0, 0))
	loader := resolveLoader(t, eng, "PART-Z")

	// Want 1, window empty -> 1 created.
	if created, err := eng.tryCreateL1(loader, "PART-Z", L1SideCycle, 1, ""); err != nil || created != 1 {
		t.Fatalf("seed fire: created=%d err=%v, want 1, nil", created, err)
	}
	// Want 3, but the one-window loader already holds its empty -> node cap fires 0.
	if created, err := eng.tryCreateL1(loader, "PART-Z", L1SideCycle, 3, ""); err != nil || created != 0 {
		t.Errorf("node cap: created=%d err=%v, want 0, nil (window already holds 1)", created, err)
	}

	ords, err := db.ListActiveOrdersByProcessNode(nodeID)
	if err != nil {
		t.Fatalf("list orders: %v", err)
	}
	n := 0
	for _, o := range ords {
		if o.RetrieveEmpty && o.PayloadCode == "PART-Z" {
			n++
		}
	}
	if n != manualSwapWindowSlots {
		t.Errorf("expected %d in-flight L1 order(s) (window cap), got %d", manualSwapWindowSlots, n)
	}
}

// TestTryCreateL1_TransitionalSuppressesAutomaticSourcesOnly pins the
// allowlist gate: on a transitional loader the market-accounting sources fire
// nothing, and clearing the flag restores them. (L1LoaderPush, added with
// MaybePushLoader, is the source that must NOT be suppressed — covered there.)
func TestTryCreateL1_TransitionalSuppressesAutomaticSourcesOnly(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	_, nodeID, _ := seedActiveManualSwapLoader(t, db, "TR-PROC", "TR-LOADER", "PART-T")

	// Transitional in the aggregate: replenishment=operator.
	seedCoreLoader(t, eng, sharedLoaderInfo("TR-LOADER", "produce", "operator", "PART-T", 0, 0))
	// Resolve AFTER seeding: tryCreateL1 reads loader.IsTransitional() (the projected
	// aggregate snapshot), so the loader must be (re)resolved to observe it.
	// Representative of production, where each demand signal re-resolves the loader.
	loader := resolveLoader(t, eng, "PART-T")
	if created, err := eng.tryCreateL1(loader, "PART-T", L1SideCycle, 2, ""); err != nil || created != 0 {
		t.Errorf("L1SideCycle on transitional: created=%d err=%v, want 0, nil", created, err)
	}
	if created, err := eng.tryCreateL1(loader, "PART-T", L1LoopThreshold, 2, ""); err != nil || created != 0 {
		t.Errorf("L1LoopThreshold on transitional: created=%d err=%v, want 0, nil", created, err)
	}
	ords, _ := db.ListActiveOrdersByProcessNode(nodeID)
	for _, o := range ords {
		if o.RetrieveEmpty && o.PayloadCode == "PART-T" {
			t.Fatalf("transitional loader must not auto-create L1s; found order %d", o.ID)
		}
	}

	// Clear transitional: re-seed with replenishment=auto.
	seedCoreLoader(t, eng, sharedLoaderInfo("TR-LOADER", "produce", "auto", "PART-T", 0, 0))
	loader = resolveLoader(t, eng, "PART-T") // re-resolve so the snapshot reflects the cleared flag
	// After clearing the flag the source fires again, bounded by the one-window
	// node cap to a single empty (was 2 pre-PR-0; minStock is now reached over
	// the fill/release cycle rather than by queuing both empties at the window).
	if created, err := eng.tryCreateL1(loader, "PART-T", L1SideCycle, 2, ""); err != nil || created != 1 {
		t.Errorf("after clearing transitional: created=%d err=%v, want 1, nil", created, err)
	}
}
