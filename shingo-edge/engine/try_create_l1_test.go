package engine

import (
	"shingoedge/orders"
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

// These tests pin fireThresholdL1's contract by asserting the returned created
// count and the orders DB — not log strings (the review flagged the existing
// log-scraping tests as brittle). They cover the in-flight clamp and the
// transitional allowlist gate.

// TestTryCreateL1_BoundedByNodeWindowCapAndReturnsCreated pins the post-PR-0
// chokepoint contract: fireThresholdL1 fires (desired - inFlight) for the payload
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
	seedCoreLoader(t, eng, sharedLoaderInfo("CLAMP-LOADER", "produce", "threshold", "PART-Z", 0, 0))
	loader := resolveLoader(t, eng, "PART-Z")

	// Want 1, window empty -> 1 created.
	if created, err := eng.stageOperatorEmpty(loader, "PART-Z", 1, "", orders.Origin{}); err != nil || created != 1 {
		t.Fatalf("seed fire: created=%d err=%v, want 1, nil", created, err)
	}
	// Want 3, but the one-window loader already holds its empty -> node cap fires 0.
	if created, err := eng.stageOperatorEmpty(loader, "PART-Z", 3, "", orders.Origin{}); err != nil || created != 0 {
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

// TestOperatorPushIsNotSuppressedAtAnOperatorDrivenLoader.
//
// THIS TEST CHANGED SIDES. It used to pin the opposite: that the automatic
// threshold source fired nothing at an operator-driven loader. That source no
// longer exists on the Edge — Core decides a loader's automatic replenishment
// now — and the guarantee moved with it, to ReplenishLoader, which refuses an
// operator-driven loader by name ("a person stages it, so no carriers are
// ordered automatically"). Deleting this test would have lost the other half of
// the pair, which is what is asserted here instead.
//
// The half that stays on the Edge: the OPERATOR'S OWN PUSH must fire at an
// operator-driven loader. That is the entire point of an operator-driven
// loader — a person stages it, and this is the path they stage it through.
// Suppressing it here would leave such a loader with no supply at all from
// either side, which is a stopped line rather than a cautious one.
func TestOperatorPushIsNotSuppressedAtAnOperatorDrivenLoader(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	_, nodeID, _ := seedActiveManualSwapLoader(t, db, "TR-PROC", "TR-LOADER", "PART-T")

	// Operator-driven in the aggregate: replenishment=operator.
	seedCoreLoader(t, eng, sharedLoaderInfo("TR-LOADER", "produce", "operator", "PART-T", 0, 0))
	// Resolve AFTER seeding: the create path reads the projected aggregate
	// snapshot, so the loader must be (re)resolved to observe it.
	loader := resolveLoader(t, eng, "PART-T")
	// Asks for two, bounded to one by the single-window node cap.
	if created, err := eng.stageOperatorEmpty(loader, "PART-T", 2, "", orders.Origin{}); err != nil || created != 1 {
		t.Errorf("operator push at an operator-driven loader: created=%d err=%v, want 1, nil — "+
			"this is the path a person stages the loader through; suppressing it leaves the "+
			"loader with no supply from either side", created, err)
	}
	ords, _ := db.ListActiveOrdersByProcessNode(nodeID)
	var n int
	for _, o := range ords {
		if o.RetrieveEmpty && o.PayloadCode == "PART-T" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("in-flight empties = %d, want 1", n)
	}

	// And at a threshold loader it still fires — the push is not gated on
	// replenishment mode in either direction.
	seedCoreLoader(t, eng, sharedLoaderInfo("TR-LOADER", "produce", "threshold", "PART-T", 0, 0))
	loader = resolveLoader(t, eng, "PART-T")
	if created, err := eng.stageOperatorEmpty(loader, "PART-T", 2, "", orders.Origin{}); err != nil || created != 0 {
		t.Errorf("second push while one is already in flight: created=%d err=%v, want 0, nil "+
			"(the window is taken)", created, err)
	}
}
