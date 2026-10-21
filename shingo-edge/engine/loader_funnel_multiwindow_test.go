package engine

import (
	"testing"

	"shingoedge/store"
)

// TestFunnelWindows_ProjectedFromCoreCache pins the last hop: the setting Core
// synced down survives projectCoreLoader onto the domain aggregate. No Edge
// decision reads it since every carrier a loader asks for is Core's to place
// (Core funnels from its own row); it is pinned while the aggregate carries it.
func TestFunnelWindows_ProjectedFromCoreCache(t *testing.T) {
	t.Parallel()
	cached := store.CoreLoader{
		LoaderKey: "loader:9001", Role: "produce", Name: "FWP",
		Layout: "shared_window", Replenishment: "threshold",
		FunnelWindows: true,
		Positions:     []store.CoreLoaderPosition{{PositionNode: "FWP-W1"}, {PositionNode: "FWP-W2"}},
		Payloads:      []store.CoreLoaderPayload{{PayloadCode: "P1"}},
	}
	l, err := projectCoreLoader(cached)
	if err != nil {
		t.Fatalf("projectCoreLoader: %v", err)
	}
	if !l.FunnelWindows() {
		t.Error("Core said funnel; the projected loader spreads")
	}

	// And the default direction, which is the one that ships to every existing
	// plant: a cache row that says nothing must project as spread.
	cached.FunnelWindows = false
	l, err = projectCoreLoader(cached)
	if err != nil {
		t.Fatalf("projectCoreLoader (spread): %v", err)
	}
	if l.FunnelWindows() {
		t.Error("cache says spread; the projected loader funnels")
	}
}
