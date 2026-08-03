//go:build docker

package dispatch

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
)

// replenishFixture builds a shared-window loader over `count` windows, plus the
// market its empties come from, and returns the request/config pair pointed at
// it. The loader config is passed to ReplenishLoader directly rather than read
// back through the aggregate, which is what the function's signature asks for
// and what lets these tests state a shape in one place.
func replenishFixture(t *testing.T, db *store.DB, prefix string, count int) (ReplenishRequest, LoaderReplenishConfig, []string) {
	t.Helper()
	testdb.SetupStandardData(t, db)

	source := &nodes.Node{Name: prefix + "-EMPTY-MARKET", Enabled: true}
	if err := db.CreateNode(source); err != nil {
		t.Fatalf("create source: %v", err)
	}

	homes := make([]loaders.Home, 0, count)
	names := map[int64]string{}
	windowNames := make([]string, 0, count)
	for i := range count {
		n := &nodes.Node{Name: prefix + "-W" + string(rune('A'+i)), Enabled: true}
		if err := db.CreateNode(n); err != nil {
			t.Fatalf("create window: %v", err)
		}
		homes = append(homes, loaders.Home{PositionNodeID: n.ID})
		names[n.ID] = n.Name
		windowNames = append(windowNames, n.Name)
	}

	req := ReplenishRequest{
		StationID:      "edge.line1",
		LoaderID:       99,
		PayloadCode:    "PART-R",
		Threshold:      1000,
		CurrentUOP:     0,
		PerBinCapacity: 10,
	}
	cfg := LoaderReplenishConfig{
		Layout:        loaders.LayoutSharedWindow,
		Homes:         homes,
		NodeNames:     names,
		Payloads:      []loaders.Payload{{PayloadCode: "PART-R"}},
		InboundSource: source.Name,
	}
	return req, cfg, windowNames
}

// TestReplenishLoader_NeverMoreThanTheWindowsCanHold is the whole reason this
// path is moving to Core.
//
// The reading asks for a hundred carriers. The loader has three windows. Three
// is the answer, because a window holds one carrier and there is nowhere to put
// the other ninety-seven. The sizing answer alone — what the loop needs — is
// what over-ordered the plant: it is a true statement about demand and a useless
// statement about what to create.
func TestReplenishLoader_NeverMoreThanTheWindowsCanHold(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, nil)
	req, cfg, windows := replenishFixture(t, db, "RNM", 3)

	res, err := d.ReplenishLoader(req, cfg)
	if err != nil {
		t.Fatalf("ReplenishLoader: %v", err)
	}
	if res.Want != 100 {
		t.Errorf("Want = %d, want 100 (the sizing answer is reported unbounded — it is what the loop needs)", res.Want)
	}
	if len(res.Created) != 3 {
		t.Fatalf("created %d orders, want 3 — one per window and no more", len(res.Created))
	}
	seen := map[string]int{}
	for _, o := range res.Created {
		seen[o.DeliveryNode]++
		if o.OrderType != OrderTypeRetrieveEmpty {
			t.Errorf("order type = %s, want retrieve_empty", o.OrderType)
		}
		if o.SourceNode != cfg.InboundSource {
			t.Errorf("source = %q, want the loader's inbound market %q", o.SourceNode, cfg.InboundSource)
		}
		if o.PayloadCode != "" {
			t.Errorf("payload = %q, want blank — an empty carrier is generic until the loader fills it", o.PayloadCode)
		}
		if o.Quantity != 1 {
			t.Errorf("quantity = %d, want 1 — one order, one carrier, one window", o.Quantity)
		}
	}
	for _, w := range windows {
		if seen[w] != 1 {
			t.Errorf("window %s got %d carriers, want exactly 1", w, seen[w])
		}
	}
}

// TestReplenishLoader_FunnelTakesOneWhateverTheAsk pins the bound through the
// shape where it is NOT simply the window count: a three-window loader
// configured to take one window at a time gets one carrier for an ask of a
// hundred, and gets it at its first window.
//
// This is the case that proves the bound is the target list rather than a
// coincidence of the spread shape — remove the target-list iteration and this
// fails where the spread test would not.
func TestReplenishLoader_FunnelTakesOneWhateverTheAsk(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, nil)
	req, cfg, windows := replenishFixture(t, db, "RFT", 3)
	cfg.FunnelWindows = true

	res, err := d.ReplenishLoader(req, cfg)
	if err != nil {
		t.Fatalf("ReplenishLoader: %v", err)
	}
	if res.Want != 100 {
		t.Errorf("Want = %d, want 100", res.Want)
	}
	if len(res.Created) != 1 {
		t.Fatalf("created %d, want 1 — this loader takes one window at a time", len(res.Created))
	}
	if got := res.Created[0].DeliveryNode; got != windows[0] {
		t.Errorf("delivered to %s, want the first window %s", got, windows[0])
	}
}

// TestReplenishLoader_AsksForFewerThanTheWindowsHold covers the other side: when
// the loop needs less than the loader can hold, the ask wins. Without this the
// budget would read as a target rather than a ceiling and every replenishment
// would fill the loader regardless of demand.
func TestReplenishLoader_AsksForFewerThanTheWindowsHold(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, nil)
	req, cfg, _ := replenishFixture(t, db, "RAF", 4)
	req.Threshold = 15 // 15 units, 10 per carrier -> 2 carriers
	req.CurrentUOP = 0

	res, err := d.ReplenishLoader(req, cfg)
	if err != nil {
		t.Fatalf("ReplenishLoader: %v", err)
	}
	if res.Want != 2 {
		t.Errorf("Want = %d, want 2", res.Want)
	}
	if len(res.Created) != 2 {
		t.Errorf("created %d, want 2 — the loader holds 4 but the loop only needs 2", len(res.Created))
	}
}

// TestReplenishLoader_SkipsWindowsThatCannotTakeOne pins the per-window check.
// A window with a carrier already at it is named in HeldBy rather than silently
// dropped, and the carriers that do get created go to the windows that are free.
func TestReplenishLoader_SkipsWindowsThatCannotTakeOne(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, nil)
	req, cfg, windows := replenishFixture(t, db, "RSW", 3)

	// Occupy the first window with a physical bin.
	blocked := windows[0]
	n, err := db.GetNodeByDotName(blocked)
	if err != nil || n == nil {
		t.Fatalf("resolve %s: %v", blocked, err)
	}
	makeLoaderBin(t, db, "PART-R", n.ID, "RSW-BIN", 10, time.Now().Add(-time.Hour))

	res, err := d.ReplenishLoader(req, cfg)
	if err != nil {
		t.Fatalf("ReplenishLoader: %v", err)
	}
	if len(res.Created) != 2 {
		t.Fatalf("created %d, want 2 — one window is occupied", len(res.Created))
	}
	if _, held := res.HeldBy[blocked]; !held {
		t.Errorf("HeldBy = %v, want an entry naming %s — a skipped window must be reported, not just absent", res.HeldBy, blocked)
	}
	for _, o := range res.Created {
		if o.DeliveryNode == blocked {
			t.Errorf("sent a carrier to %s, which already has one", blocked)
		}
	}
}

// TestReplenishLoader_QuietRunsSayWhy pins that every way of creating nothing is
// distinguishable. A caller that cannot tell "already stocked" from "broken"
// either raises false alarms all shift or learns to ignore the real ones.
func TestReplenishLoader_QuietRunsSayWhy(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		prefix string
		adjust func(*ReplenishRequest, *LoaderReplenishConfig)
	}{
		{"at threshold", "RQ1", func(r *ReplenishRequest, _ *LoaderReplenishConfig) { r.CurrentUOP = r.Threshold }},
		{"no per-bin capacity", "RQ2", func(r *ReplenishRequest, _ *LoaderReplenishConfig) { r.PerBinCapacity = 0 }},
		{"fed directly", "RQ3", func(_ *ReplenishRequest, c *LoaderReplenishConfig) { c.InboundSource = "" }},
		{"payload the loader does not carry", "RQ4", func(r *ReplenishRequest, _ *LoaderReplenishConfig) { r.PayloadCode = "PART-NOT-HERE" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testDB(t)
			d, _ := newTestDispatcher(t, db, nil)
			req, cfg, _ := replenishFixture(t, db, tc.prefix, 2)
			tc.adjust(&req, &cfg)

			res, err := d.ReplenishLoader(req, cfg)
			if err != nil {
				t.Fatalf("ReplenishLoader: %v", err)
			}
			if len(res.Created) != 0 {
				t.Errorf("created %d orders, want none", len(res.Created))
			}
			if res.Skipped == "" {
				t.Error("nothing created and no reason given; a caller cannot tell this from a failure")
			}
		})
	}
}

// TestReplenishLoader_BrokenLedgerStillOrders pins the Springfield rule end to
// end rather than only in the arithmetic: a negative reading is sized from zero,
// and ordering CONTINUES. A broken count does not get to size the order and does
// not get to cancel it.
func TestReplenishLoader_BrokenLedgerStillOrders(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, nil)
	req, cfg, _ := replenishFixture(t, db, "RBL", 2)
	req.Threshold = 20
	req.CurrentUOP = -443
	req.PerBinCapacity = 10

	res, err := d.ReplenishLoader(req, cfg)
	if err != nil {
		t.Fatalf("ReplenishLoader: %v", err)
	}
	if res.Want != 2 {
		t.Errorf("Want = %d, want 2 (the gap sized from 0, not from -443 which would ask for 47)", res.Want)
	}
	if len(res.Created) != 2 {
		t.Errorf("created %d, want 2 — a negative reading is when the loop most needs stock", len(res.Created))
	}
}

// TestReplenishLoader_RefusesAnOperatorDrivenLoader is the Core half of a guard
// that already exists on the Edge, where it has been quietly doing real work.
//
// The configuration that reaches here is legal and derivable: a produce loader
// set to operator replenishment with a leftover threshold on one of its
// payloads. Core's registry derivation keeps that threshold and the config
// validation permits the pair, so the monitor fires at such a loader today and
// the Edge silently swallows it. Without this, Core taking over ordering turns a
// signal nobody acts on into carriers arriving at a loader whose whole
// configuration says a person stages it.
func TestReplenishLoader_RefusesAnOperatorDrivenLoader(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, nil)
	req, cfg, _ := replenishFixture(t, db, "ROD", 3)
	cfg.Replenishment = loaders.ReplenishmentOperator

	res, err := d.ReplenishLoader(req, cfg)
	if err != nil {
		t.Fatalf("ReplenishLoader: %v", err)
	}
	if len(res.Created) != 0 {
		t.Errorf("created %d carriers for an operator-driven loader; a person stages that one", len(res.Created))
	}
	if res.Skipped == "" {
		t.Error("refused silently; the reason has to be sayable")
	}
}

// TestReplenishLoader_SourcesFromTheBufferWhenThereIsOne pins the precedence the
// Edge already uses: a configured buffer wins outright over the inbound market,
// with no fallback. Copied rather than chosen — a loader with a buffer would
// otherwise silently start sourcing from somewhere else the day Core took over,
// which is a change to where a robot physically drives.
func TestReplenishLoader_SourcesFromTheBufferWhenThereIsOne(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, nil)
	req, cfg, _ := replenishFixture(t, db, "RBF", 2)
	cfg.BufferDest = "RBF-BUFFER-GROUP"

	res, err := d.ReplenishLoader(req, cfg)
	if err != nil {
		t.Fatalf("ReplenishLoader: %v", err)
	}
	if len(res.Created) == 0 {
		t.Fatal("created nothing")
	}
	for _, o := range res.Created {
		if o.SourceNode != "RBF-BUFFER-GROUP" {
			t.Errorf("source = %q, want the buffer group — it wins over the inbound market", o.SourceNode)
		}
	}

}

// TestReplenishLoader_NoSourceAtAllOrdersNothing: with neither a buffer nor an
// inbound market there is nowhere to pull a carrier from, which is a supported
// configuration (the loader is fed by hand) rather than a fault.
func TestReplenishLoader_NoSourceAtAllOrdersNothing(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, nil)
	req, cfg, _ := replenishFixture(t, db, "RNS", 2)
	cfg.InboundSource = ""
	cfg.BufferDest = ""

	res, err := d.ReplenishLoader(req, cfg)
	if err != nil {
		t.Fatalf("ReplenishLoader: %v", err)
	}
	if len(res.Created) != 0 || res.Skipped == "" {
		t.Errorf("created=%d skipped=%q, want nothing with a reason", len(res.Created), res.Skipped)
	}
}

// TestReplenishLoader_AttachesTheDemandEpisode pins the attribution. An order
// carrying an episode id IS attached by definition, and stamping it orphan would
// fill the bucket that exists to find lost attributions with correctly
// attributed orders — making the real ones unfindable.
func TestReplenishLoader_AttachesTheDemandEpisode(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, nil)
	req, cfg, _ := replenishFixture(t, db, "RAT", 2)
	req.OriginID = "3f2504e0-4f89-11d3-9a0c-0305e82c3301"

	res, err := d.ReplenishLoader(req, cfg)
	if err != nil {
		t.Fatalf("ReplenishLoader: %v", err)
	}
	if len(res.Created) == 0 {
		t.Fatal("created nothing")
	}
	for _, o := range res.Created {
		if o.OriginClass != protocol.OriginClassAttached {
			t.Errorf("origin class = %q, want attached — the order names its episode", o.OriginClass)
		}
		if o.OriginID != req.OriginID {
			t.Errorf("origin id = %q, want %q", o.OriginID, req.OriginID)
		}
	}
}

// TestReplenishLoader_RefusesARequestWithNoLoader: a legacy binding carries no
// loader id, and a request built from one would resolve against empty config and
// create nothing while looking like it tried.
func TestReplenishLoader_RefusesARequestWithNoLoader(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, nil)
	req, cfg, _ := replenishFixture(t, db, "RNL", 2)
	req.LoaderID = 0

	res, err := d.ReplenishLoader(req, cfg)
	if err != nil {
		t.Fatalf("ReplenishLoader: %v", err)
	}
	if len(res.Created) != 0 || res.Skipped == "" {
		t.Errorf("created=%d skipped=%q, want nothing with a reason", len(res.Created), res.Skipped)
	}
}

// TestLoadReplenishConfig_ReadsWhatTheDecisionNeeds covers the assembler: the
// decision is only as good as the configuration handed to it, and every field it
// branches on has to survive the read.
func TestLoadReplenishConfig_ReadsWhatTheDecisionNeeds(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, nil)
	testdb.SetupStandardData(t, db)

	win := &nodes.Node{Name: "LRC-W1", Enabled: true}
	if err := db.CreateNode(win); err != nil {
		t.Fatalf("create window: %v", err)
	}
	id, err := db.CreateLoader(loaders.Loader{
		Name: "LRC-L", Role: loaders.RoleProduce, Layout: loaders.LayoutSharedWindow,
		Replenishment: loaders.ReplenishmentThreshold,
		InboundSource: "LRC-MARKET", BufferDest: "LRC-BUFFER", FunnelWindows: true,
	})
	if err != nil {
		t.Fatalf("create loader: %v", err)
	}
	if err := db.UpsertLoaderHome(loaders.Home{LoaderID: id, PositionNodeID: win.ID}); err != nil {
		t.Fatalf("add window: %v", err)
	}
	if err := db.UpsertLoaderPayload(loaders.Payload{LoaderID: id, PayloadCode: "PART-R", UOPThreshold: 100}); err != nil {
		t.Fatalf("add payload: %v", err)
	}

	cfg, found, err := d.LoadReplenishConfig(id)
	if err != nil || !found {
		t.Fatalf("LoadReplenishConfig: found=%v err=%v", found, err)
	}
	if cfg.Layout != loaders.LayoutSharedWindow || !cfg.FunnelWindows {
		t.Errorf("layout=%q funnel=%v, want shared_window/true", cfg.Layout, cfg.FunnelWindows)
	}
	if cfg.Replenishment != loaders.ReplenishmentThreshold {
		t.Errorf("replenishment = %q; without it the operator-driven refusal cannot fire", cfg.Replenishment)
	}
	if cfg.InboundSource != "LRC-MARKET" || cfg.BufferDest != "LRC-BUFFER" {
		t.Errorf("sources = %q / %q, want LRC-MARKET / LRC-BUFFER", cfg.InboundSource, cfg.BufferDest)
	}
	if len(cfg.Homes) != 1 || len(cfg.Payloads) != 1 {
		t.Errorf("homes=%d payloads=%d, want 1/1", len(cfg.Homes), len(cfg.Payloads))
	}
	if cfg.NodeNames[win.ID] != "LRC-W1" {
		t.Errorf("node names = %v; without the name resolution there is nowhere to deliver", cfg.NodeNames)
	}

	// An absent loader is a normal answer at a decision point, not an error.
	if _, found, err := d.LoadReplenishConfig(999999); err != nil || found {
		t.Errorf("absent loader: found=%v err=%v, want false/nil", found, err)
	}
}

// TestReplenishLoader_DedicatedGoesToTheNamedPosition pins that a dedicated
// loader gets one carrier at the position the reading came from, not at
// whichever position happens to sort first. Two positions carrying the same part
// is the shape that made this matter.
func TestReplenishLoader_DedicatedGoesToTheNamedPosition(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, nil)
	req, cfg, windows := replenishFixture(t, db, "RDP", 2)
	cfg.Layout = loaders.LayoutDedicatedPositions
	cfg.Payloads = nil
	for i := range cfg.Homes {
		cfg.Homes[i].PayloadCode = "PART-R"
	}
	req.MemberNode = windows[1]

	res, err := d.ReplenishLoader(req, cfg)
	if err != nil {
		t.Fatalf("ReplenishLoader: %v", err)
	}
	if len(res.Created) != 1 {
		t.Fatalf("created %d, want 1 — dedicated positions never share a budget", len(res.Created))
	}
	if got := res.Created[0].DeliveryNode; got != windows[1] {
		t.Errorf("delivered to %s, want the named position %s", got, windows[1])
	}
}
