package engine

import (
	"testing"

	"shingoedge/service"
)

// fakePreflightPoster reports every requested payload as missing, with no
// network call — lets the test drive the StartProcessChangeover preflight
// path deterministically.
type fakePreflightPoster struct{}

func (fakePreflightPoster) Available() bool { return true }

func (fakePreflightPoster) PreflightInventory(station string, payloads []string) (*service.PreflightCoreResult, error) {
	// Report everything the caller asked about as not-in-stock.
	return &service.PreflightCoreResult{Missing: payloads}, nil
}

// TestStartChangeover_MissingStock_StartsWithAdvisory pins the 2026-06-04
// behavior change: a changeover whose required payloads have zero available
// supermarket bins must START — the supply legs queue as "Awaiting Stock"
// and self-heal when the operator loads the material — instead of being
// hard-refused at the preflight gate. This guards against re-introducing the
// refusal, which is the Springfield NF SPOT 3 recurrence (operator had the
// part on the floor, just not yet entered into Shingo).
func TestStartChangeover_MissingStock_StartsWithAdvisory(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, _, toStyleID := seedAddNodeScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	// Wire a preflight that reports the to-style payload as not-in-stock, and
	// a non-empty core client so the gate actually runs (Available() == true).
	eng.coreClient = NewCoreClient("http://test-core")
	eng.preflightChecker = service.NewPreflightChecker(db, fakePreflightPoster{}, "test.station")

	co, err := eng.StartProcessChangeover(processID, toStyleID, "test", "missing-stock advisory")
	if err != nil {
		t.Fatalf("changeover must START despite missing stock, got error: %v", err)
	}
	if co == nil || co.ID == 0 {
		t.Fatal("expected a created changeover, got nil/zero")
	}
	if co.State.IsTerminal() {
		t.Errorf("changeover should be live, got terminal state %q", co.State)
	}
	if len(co.AwaitingStock) == 0 {
		t.Fatal("expected AwaitingStock advisory to be populated when stock is missing")
	}
	found := false
	for _, p := range co.AwaitingStock {
		if p == "PART-ADD" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected AwaitingStock to include PART-ADD, got %v", co.AwaitingStock)
	}
}
