package service

import (
	"testing"

	"shingoedge/domain"
)

// TestStarvationUsesTheConfiguredThreshold replaces TestLinesideStarved, which
// pinned a quarter-of-a-bin floor: starved when lineside < capacity/4.
//
// That floor was a guess. It had no relationship to the number that actually
// triggers replenishment, so the board could go red while the reorder logic was
// content, or stay calm while it was not — and on a dedicated home the two
// numbers are wildly apart (SMN_024 holds a 216-UOP threshold against bins many
// times that size). The test passed for years because it only ever checked the
// arithmetic it was given, never whether that was the right arithmetic.
//
// The comparison now reads the Core-owned loader aggregate's configured UOP
// threshold for the payload — the same value the demand sweep uses — so the
// board and the replenishment logic cannot disagree about what "low" means.
//
// ZERO MEANS NO POLICY, NOT ZERO PARTS. A loader with no configured threshold is
// not opted into UOP-threshold replenishment, and inventing a floor for it would
// put red cards on boards nobody has configured. Silence is the honest answer
// where nobody has said what low means.
func TestStarvationUsesTheConfiguredThreshold(t *testing.T) {
	t.Parallel()

	const part = domain.PayloadCode("PART-A")

	// A dedicated-positions loader, which is the layout the defect was found on:
	// the threshold rides on the Position, not on a shared per-payload map.
	loader, err := domain.NewDedicatedPositionsLoader(
		domain.LoaderID("loader:test"), "Test Loader",
		domain.RoleProduce, domain.ReplenishmentThreshold,
		[]domain.Position{{Node: domain.NodeID("SMN_TEST"), Payload: part, UOPThreshold: 500}},
	)
	if err != nil {
		t.Fatalf("build loader: %v", err)
	}

	cases := []struct {
		name     string
		lineside int
		want     bool
	}{
		{"below the configured threshold is starved", 499, true},
		{"exactly at the threshold is safe", 500, false},
		{"well above is safe", 2000, false},
		{"empty is starved", 0, true},
	}
	for _, c := range cases {
		got := false
		if t := loader.UOPThresholdFor(part); t > 0 && c.lineside < t {
			got = true
		}
		if got != c.want {
			t.Errorf("%s: lineside=%d against threshold 500 = %v, want %v",
				c.name, c.lineside, got, c.want)
		}
	}

	// A payload the loader carries no threshold for must never trip, however
	// low it reads. This is the arm that keeps an unconfigured board quiet.
	if th := loader.UOPThresholdFor(domain.PayloadCode("PART-UNCONFIGURED")); th != 0 {
		t.Errorf("unconfigured payload returned threshold %d, want 0 — a board with no "+
			"threshold policy must not be given an invented one", th)
	}
}
