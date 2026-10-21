package engine

import (
	"testing"

	"shingoedge/domain"
	"shingoedge/store"
)

// mustFunnelLoader builds a shared loader that takes ONE window at a time, over
// the given windows. Same shape as mustMultiWindowLoader; the only difference is
// the setting under test, so a comparison between the two isolates it.
func mustFunnelLoader(t *testing.T, id string, windows []string, payloads ...string) *domain.Loader {
	t.Helper()
	ws := make([]domain.Window, len(windows))
	for i, w := range windows {
		ws[i] = domain.Window{Node: domain.NodeID(w)}
	}
	ps := make([]domain.PayloadCode, len(payloads))
	thresholds := make(map[domain.PayloadCode]int, len(payloads))
	for i, p := range payloads {
		ps[i] = domain.PayloadCode(p)
		thresholds[domain.PayloadCode(p)] = 100
	}
	l, err := domain.NewSharedWindowLoader(domain.LoaderID(id), domain.RoleProduce, domain.ReplenishmentThreshold,
		ws, ps,
		domain.WithInboundSource("EMPTY-SUPER"),
		domain.WithUOPThreshold(thresholds),
		domain.WithFunnelWindows(true))
	if err != nil {
		t.Fatalf("build funnel loader: %v", err)
	}
	return l
}

// TestFunnelWindows_ProjectedFromCoreCache pins the last hop: the setting Core
// synced down has to survive projectCoreLoader onto the domain aggregate, which
// is the only copy the seam reads. Cached correctly and dropped in projection
// looks exactly like Core never sent it.
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

// TestFunnelWindows_TheLoaderDecides: each loader answers for itself, from
// Core's funnel_windows on its own row. There is no plant-wide key left to
// brake a spreading loader into the funnel (it used to be
// loaders_multi_window: false, which no plant set).
func TestFunnelWindows_TheLoaderDecides(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)

	spreadWins := []string{"FWB-S1", "FWB-S2", "FWB-S3"}
	funnelWins := []string{"FWB-F1", "FWB-F2", "FWB-F3"}
	seedWindowNodes(t, db, "FWB-PROC", append(append([]string{}, spreadWins...), funnelWins...))
	spread := mustMultiWindowLoader(t, "FWB-SPREAD", spreadWins, "P1")
	funnel := mustFunnelLoader(t, "FWB-FUNNEL", funnelWins, "P1")

	if !eng.multiWindowFor(spread) {
		t.Error("a loader set to spread should spread")
	}
	if eng.multiWindowFor(funnel) {
		t.Error("a loader set to funnel should funnel")
	}
}
