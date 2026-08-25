package engine

import (
	"testing"

	"shingoedge/domain"
	"shingoedge/orders"
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
	l, err := domain.NewSharedWindowLoader(domain.LoaderID(id), id, domain.RoleProduce, domain.ReplenishmentThreshold,
		ws, ps,
		domain.WithInboundSource("EMPTY-SUPER"),
		domain.WithUOPThreshold(thresholds),
		domain.WithFunnelWindows(true))
	if err != nil {
		t.Fatalf("build funnel loader: %v", err)
	}
	return l
}

// TestFunnelWindows_TwoLoadersOnePlantDisagree is the whole point of moving this
// setting off the plant-wide config key and onto the loader.
//
// Two shared-window loaders, same plant, same engine, same config: one spreads
// its empties across its three windows, the other keeps a single carrier at its
// first window. Before the setting moved, this shape could not be expressed —
// one boolean answered for every loader in the plant, so the second loader's
// operator either imposed the funnel on the first or went without.
func TestFunnelWindows_TwoLoadersOnePlantDisagree(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)

	spreadWins := []string{"FW-S1", "FW-S2", "FW-S3"}
	funnelWins := []string{"FW-F1", "FW-F2", "FW-F3"}
	seedWindowNodes(t, db, "FW-PROC", append(append([]string{}, spreadWins...), funnelWins...))

	spread := mustMultiWindowLoader(t, "FW-SPREAD", spreadWins, "P1")
	funnel := mustFunnelLoader(t, "FW-FUNNEL", funnelWins, "P1")

	// Ask each for three. The spreader has room for three; the funnel has room
	// for one, whatever it is asked for.
	created, err := eng.stageOperatorEmpty(spread, "P1", 3, "", orders.Origin{})
	if err != nil || created != 3 {
		t.Fatalf("spreading loader, demand of 3: created=%d err=%v, want 3", created, err)
	}
	created, err = eng.stageOperatorEmpty(funnel, "P1", 3, "", orders.Origin{})
	if err != nil || created != 1 {
		t.Fatalf("funnelling loader, demand of 3: created=%d err=%v, want 1", created, err)
	}

	total, per := windowCounts(t, db, spreadWins)
	if total != 3 {
		t.Errorf("spreading loader in-flight = %d, want 3", total)
	}
	for _, w := range spreadWins {
		if per[w] != 1 {
			t.Errorf("spreading loader window %s holds %d, want 1", w, per[w])
		}
	}

	total, per = windowCounts(t, db, funnelWins)
	if total != 1 {
		t.Errorf("funnelling loader in-flight = %d, want 1", total)
	}
	if per[funnelWins[0]] != 1 {
		t.Errorf("funnelling loader: first window holds %d, want 1 (the funnel target)", per[funnelWins[0]])
	}
	for _, w := range funnelWins[1:] {
		if per[w] != 0 {
			t.Errorf("funnelling loader: window %s holds %d, want 0 — nothing goes past the first", w, per[w])
		}
	}

	// And the funnelling loader stays at one until its window clears.
	created, err = eng.stageOperatorEmpty(funnel, "P1", 3, "", orders.Origin{})
	if err != nil || created != 0 {
		t.Errorf("funnelling loader, second demand: created=%d err=%v, want 0", created, err)
	}
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

// TestFunnelWindows_ConfigKeyIsABrakeNotAnAccelerator pins the deprecated
// plant-wide key to exactly one power: it can still force every loader to funnel
// (so a site that set `loaders_multi_window: false` does not silently start
// spreading), and it cannot force a funnelling loader to spread.
//
// The asymmetry is the safety property. A deprecated key that could turn
// behaviour ON would let a stale config override the loader that a person
// configured deliberately.
func TestFunnelWindows_ConfigKeyIsABrakeNotAnAccelerator(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)

	spreadWins := []string{"FWB-S1", "FWB-S2", "FWB-S3"}
	funnelWins := []string{"FWB-F1", "FWB-F2", "FWB-F3"}
	seedWindowNodes(t, db, "FWB-PROC", append(append([]string{}, spreadWins...), funnelWins...))
	spread := mustMultiWindowLoader(t, "FWB-SPREAD", spreadWins, "P1")
	funnel := mustFunnelLoader(t, "FWB-FUNNEL", funnelWins, "P1")

	// Unset: every loader decides for itself.
	eng.cfg.LoadersMultiWindow = nil
	if !eng.multiWindowFor(spread) {
		t.Error("key unset: spreading loader should spread")
	}
	if eng.multiWindowFor(funnel) {
		t.Error("key unset: funnelling loader should funnel")
	}

	// Explicit true: still every loader for itself. The key cannot lift a
	// loader's own funnel setting.
	on := true
	eng.cfg.LoadersMultiWindow = &on
	if !eng.multiWindowFor(spread) {
		t.Error("key true: spreading loader should spread")
	}
	if eng.multiWindowFor(funnel) {
		t.Error("key true: the key must not override a loader configured to funnel")
	}

	// Explicit false: the brake. Everything funnels, including the loader that
	// asked to spread — that is the behaviour a site that set the key already has.
	off := false
	eng.cfg.LoadersMultiWindow = &off
	if eng.multiWindowFor(spread) {
		t.Error("key false: the plant-wide brake must funnel every loader")
	}
	if eng.multiWindowFor(funnel) {
		t.Error("key false: funnelling loader should funnel")
	}
}
