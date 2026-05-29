package engine

import "testing"

// These tests pin tryCreateL1's contract by asserting the returned created
// count and the orders DB — not log strings (the review flagged the existing
// log-scraping tests as brittle). They cover the in-flight clamp and the
// transitional allowlist gate.

// TestTryCreateL1_ClampsToInFlightAndReturnsCreated pins that the chokepoint
// fires count-inFlight and returns exactly how many it created.
func TestTryCreateL1_ClampsToInFlightAndReturnsCreated(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	_, nodeID, _ := seedActiveManualSwapLoader(t, db, "CLAMP-PROC", "CLAMP-LOADER", "PART-Z")
	loader := eng.FindLoaderForPayload("PART-Z")
	if loader == nil {
		t.Fatal("seeded loader not found")
	}

	// 1 wanted, 0 in flight -> 1 created.
	if created, err := eng.tryCreateL1(loader, "PART-Z", L1SideCycle, 1); err != nil || created != 1 {
		t.Fatalf("seed fire: created=%d err=%v, want 1, nil", created, err)
	}
	// 3 wanted, 1 in flight -> 2 created.
	if created, err := eng.tryCreateL1(loader, "PART-Z", L1SideCycle, 3); err != nil || created != 2 {
		t.Errorf("clamp: created=%d err=%v, want 2, nil", created, err)
	}
	// 3 wanted, 3 in flight -> 0 created.
	if created, err := eng.tryCreateL1(loader, "PART-Z", L1SideCycle, 3); err != nil || created != 0 {
		t.Errorf("at cap: created=%d err=%v, want 0, nil", created, err)
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
	if n != 3 {
		t.Errorf("expected 3 in-flight L1 orders, got %d", n)
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
	loader := eng.FindLoaderForPayload("PART-T")
	if loader == nil {
		t.Fatal("seeded loader not found")
	}

	if err := db.SetTransitionalLoader("TR-LOADER", true, "test"); err != nil {
		t.Fatalf("set transitional: %v", err)
	}
	if created, err := eng.tryCreateL1(loader, "PART-T", L1SideCycle, 2); err != nil || created != 0 {
		t.Errorf("L1SideCycle on transitional: created=%d err=%v, want 0, nil", created, err)
	}
	if created, err := eng.tryCreateL1(loader, "PART-T", L1LoopThreshold, 2); err != nil || created != 0 {
		t.Errorf("L1LoopThreshold on transitional: created=%d err=%v, want 0, nil", created, err)
	}
	ords, _ := db.ListActiveOrdersByProcessNode(nodeID)
	for _, o := range ords {
		if o.RetrieveEmpty && o.PayloadCode == "PART-T" {
			t.Fatalf("transitional loader must not auto-create L1s; found order %d", o.ID)
		}
	}

	if err := db.SetTransitionalLoader("TR-LOADER", false, "test"); err != nil {
		t.Fatalf("clear transitional: %v", err)
	}
	if created, err := eng.tryCreateL1(loader, "PART-T", L1SideCycle, 2); err != nil || created != 2 {
		t.Errorf("after clearing transitional: created=%d err=%v, want 2, nil", created, err)
	}
}
