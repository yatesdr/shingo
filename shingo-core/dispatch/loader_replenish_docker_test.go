//go:build docker

package dispatch

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
	"shingocore/store/payloads"
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
		// CONVERTED (D1 Lane 0, item 11): this asserted the returned order's
		// payload was blank. That was the in-memory struct only: the row got the
		// loader's part in a second write after the birth, and a scanner pass
		// could read the row between the two. The part is now in the INSERT, so
		// the order and its row both carry it from birth. The carrier is still
		// generic: the station's projection carries no payload
		// (TestPinCoreAsk_TheLoaderAskIsBornWithItsPart).
		if o.PayloadCode != req.PayloadCode {
			t.Errorf("payload = %q, want the loader's part %q — the ask is the loader's demand for it, written at birth", o.PayloadCode, req.PayloadCode)
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

// TestReplenishLoader_SourcesFromTheInboundGroup pins the one answer to where a
// loader's empties come from. There is no override and no second field: every
// order this path creates names the loader's inbound_source, on both layouts.
//
// A dedicated loader is asserted alongside the shared-window one because that is
// the layout the retired staging field was offered on — it is the case where a
// second source used to be reachable, and the case a reader will come here to
// check.
func TestReplenishLoader_SourcesFromTheInboundGroup(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, nil)
	req, cfg, _ := replenishFixture(t, db, "RBF", 2)

	// The dedicated arm needs its OWN positions, and cannot reuse the shared
	// fixture's. Two independent reasons, each of which makes the arm create
	// nothing:
	//
	//   - DeliveryTargets matches a dedicated position on
	//     h.PayloadCode == in.PayloadCode, and the fixture's homes are unpinned
	//     windows. Flipping the layout field alone selects no target.
	//   - Re-running against the same nodes hits "window-order-open": the shared
	//     pass left a live carrier request on every one of them.
	//
	// A second replenishFixture call is not the way out either — it re-runs
	// SetupStandardData and collides on nodes_name_key. So the positions are
	// built here, pinned to the payload, reusing the fixture's source group.
	//
	// All three failure modes look identical from the outside: an empty
	// res.Created. A test that only checked the sources of the orders it managed
	// to make would pass on an empty set, which is what the Fatalf below is for.
	dedicated := cfg
	dedicated.Layout = loaders.LayoutDedicatedPositions
	dedicated.Homes = nil
	dedicated.NodeNames = map[int64]string{}
	for i := range 2 {
		n := &nodes.Node{Name: "RBD-POS-" + string(rune('A'+i)), Enabled: true}
		if err := db.CreateNode(n); err != nil {
			t.Fatalf("create dedicated position: %v", err)
		}
		dedicated.Homes = append(dedicated.Homes, loaders.Home{
			PositionNodeID: n.ID, PayloadCode: req.PayloadCode,
		})
		dedicated.NodeNames[n.ID] = n.Name
	}

	for _, tc := range []struct {
		name string
		cfg  LoaderReplenishConfig
	}{
		{loaders.LayoutSharedWindow, cfg},
		{loaders.LayoutDedicatedPositions, dedicated},
	} {
		res, err := d.ReplenishLoader(req, tc.cfg)
		if err != nil {
			t.Fatalf("ReplenishLoader %s: %v", tc.name, err)
		}
		if len(res.Created) == 0 {
			t.Fatalf("%s: created nothing (held by %v, skipped %q), so the source assertion below proves nothing",
				tc.name, res.HeldBy, res.Skipped)
		}
		for _, o := range res.Created {
			if o.SourceNode != tc.cfg.InboundSource {
				t.Errorf("%s: source = %q, want the inbound group %q", tc.name, o.SourceNode, tc.cfg.InboundSource)
			}
		}
	}
}

// TestReplenishLoader_NoSourceAtAllOrdersNothing: with no inbound market there is
// nowhere to pull a carrier from, which is a supported configuration (the loader
// is fed by hand) rather than a fault.
func TestReplenishLoader_NoSourceAtAllOrdersNothing(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, nil)
	req, cfg, _ := replenishFixture(t, db, "RNS", 2)
	cfg.InboundSource = ""

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
		InboundSource: "LRC-MARKET", FunnelWindows: true,
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
	if cfg.InboundSource != "LRC-MARKET" {
		t.Errorf("source = %q, want LRC-MARKET", cfg.InboundSource)
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

// TestReplenishLoader_DoesNotReAskForWhatThisDemandAlreadyOrdered is
// Springfield 2026-08-03, reduced to two calls.
//
// The empty market is dry, so the order the first call creates cannot source and
// sits in `queued`. The second call is the same demand asking the same question
// about the same window — and it must not create a second carrier. It did, about
// once a minute for three and a half hours, because the only guard was a
// capacity check whose in-flight count could not see the order it had just made:
// an order that has not sourced holds no claimed bin.
//
// Nothing here needs the market to actually be dry: an order that has not been
// dispatched yet is in exactly the same state as one that cannot be, and it is
// the STATUS the old guard was blind to, not the reason for it.
func TestReplenishLoader_DoesNotReAskForWhatThisDemandAlreadyOrdered(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, nil)
	req, cfg, windows := replenishFixture(t, db, "RDA", 1)
	req.OriginID = "6f1d3a9c-0b52-4c8e-9a71-2d5f8e0c4b31"
	req.OriginClass = string(protocol.OriginClassAttached)
	req.Threshold = 10 // one carrier covers it
	req.CurrentUOP = 0

	first, err := d.ReplenishLoader(req, cfg)
	if err != nil {
		t.Fatalf("first ReplenishLoader: %v", err)
	}
	if len(first.Created) != 1 {
		t.Fatalf("first call created %d, want 1", len(first.Created))
	}

	second, err := d.ReplenishLoader(req, cfg)
	if err != nil {
		t.Fatalf("second ReplenishLoader: %v", err)
	}
	if len(second.Created) != 0 {
		t.Fatalf("second call created %d carriers for a demand that already has one outstanding — this is the duplicate-order bug", len(second.Created))
	}
	// The run reports what it counted, in the sentence a person reads — a quiet
	// run must be distinguishable from a broken one.
	if !strings.Contains(second.Skipped, "already has 1 carrier(s) outstanding") {
		t.Errorf("Skipped = %q, want it to name the 1 carrier already outstanding", second.Skipped)
	}
	if second.Skipped == "" && second.HeldBy[windows[0]] == "" {
		t.Errorf("second call explained nothing: Skipped=%q HeldBy=%v", second.Skipped, second.HeldBy)
	}
}

// TestReplenishLoader_SubtractsOutstandingFromTheAsk is the case a per-window
// check alone does NOT cover, and the reason the fix is a subtraction rather
// than a filter.
//
// Three windows, a loop that needs one carrier. The first call creates one at
// the first window. If the guard were only "skip windows this episode has
// already asked for", the second call would find windows two and three free and
// create there — one episode, three carriers, for an ask of one. The ask itself
// has to shrink by what is already coming.
func TestReplenishLoader_SubtractsOutstandingFromTheAsk(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, nil)
	req, cfg, _ := replenishFixture(t, db, "RSO", 3)
	req.OriginID = "b2c7e415-8d63-4f0a-91be-7c3a6d90e528"
	req.OriginClass = string(protocol.OriginClassAttached)
	req.Threshold = 10 // 10 units, 10 per carrier -> exactly 1
	req.CurrentUOP = 0

	first, err := d.ReplenishLoader(req, cfg)
	if err != nil {
		t.Fatalf("first ReplenishLoader: %v", err)
	}
	if len(first.Created) != 1 {
		t.Fatalf("first call created %d, want 1 — the loop needs one carrier", len(first.Created))
	}

	second, err := d.ReplenishLoader(req, cfg)
	if err != nil {
		t.Fatalf("second ReplenishLoader: %v", err)
	}
	if len(second.Created) != 0 {
		t.Fatalf("second call created %d at the loader's other windows; the ask must shrink by what is already coming, not just avoid the window it went to", len(second.Created))
	}
}

// TestReplenishLoader_UnattributedRequestIsNotBounded pins the deliberate
// exception. Without an origin there is no way to tell this request's
// outstanding orders from any other request's, so no bound is applied — the
// alternative is suppressing a legitimate ask on someone else's evidence.
//
// It is a real shape: a legacy binding, or any caller that has not opened an
// episode, still reaches here.
func TestReplenishLoader_UnattributedRequestIsNotBounded(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, nil)
	req, cfg, _ := replenishFixture(t, db, "RUB", 2)
	req.Threshold = 10
	req.CurrentUOP = 0
	// No OriginID, deliberately.

	first, err := d.ReplenishLoader(req, cfg)
	if err != nil {
		t.Fatalf("first ReplenishLoader: %v", err)
	}
	if len(first.Created) != 1 {
		t.Fatalf("first call created %d, want 1", len(first.Created))
	}

	// The window it used is now occupied by an in-flight order of its own, so the
	// SECOND window is the one still free. The point is that the episode bound did
	// not fire: the request is still sized by the reading alone.
	second, err := d.ReplenishLoader(req, cfg)
	if err != nil {
		t.Fatalf("second ReplenishLoader: %v", err)
	}
	if second.Skipped != "" {
		t.Errorf("Skipped = %q; an unattributed request must not be bounded by an episode it does not have", second.Skipped)
	}
}

// TestReplenishLoader_WindowSpokenForByAnotherDemand pins that the per-window
// question is asked of the WINDOW, not of the asker.
//
// Two payloads at one shared-window loader are two separate demand episodes, and
// neither can see the other's orders. An episode-scoped check would let both put
// a carrier on the same window — which is the one thing "one order per window"
// exists to prevent — and the capacity check cannot catch it either, because the
// first order has not sourced, holds no claimed bin, and so is invisible to the
// in-flight count.
func TestReplenishLoader_WindowSpokenForByAnotherDemand(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, nil)
	req, cfg, windows := replenishFixture(t, db, "RWS", 1)
	req.Threshold = 10
	req.CurrentUOP = 0
	req.OriginID = "3a8f5c21-6e94-4d17-b0c5-9f2e7a41d6b8"
	req.OriginClass = string(protocol.OriginClassAttached)

	if first, err := d.ReplenishLoader(req, cfg); err != nil {
		t.Fatalf("first ReplenishLoader: %v", err)
	} else if len(first.Created) != 1 {
		t.Fatalf("first call created %d, want 1", len(first.Created))
	}

	// A DIFFERENT demand — different episode, same loader, same window.
	other := req
	other.OriginID = "c47b90e2-1f58-4a63-8d0e-5b6c2a97f314"

	second, err := d.ReplenishLoader(other, cfg)
	if err != nil {
		t.Fatalf("second ReplenishLoader: %v", err)
	}
	if len(second.Created) != 0 {
		t.Fatalf("a second demand put %d more carriers on a window that already has one coming", len(second.Created))
	}
	if second.HeldBy[windows[0]] == "" {
		t.Errorf("HeldBy = %v, want an entry naming %s", second.HeldBy, windows[0])
	}
}

// TestReplenishLoader_FailsClosedWhenTheEpisodeReadErrors pins the sizing
// guard's fail-CLOSED posture. The whole point of the bound is that creating
// MORE carriers when you cannot read how many are already outstanding is the
// failure the read exists to prevent — flip this to fail-open and Springfield
// 2026-08-03 comes straight back, only now under a broken read instead of a
// dry market.
//
// Forced with a malformed origin_id: origin_id is a UUID column, so a non-UUID
// string makes CountLiveByOrigin ERROR rather than match nothing. That is the
// exact failure the blank-guard in episodeOutstanding is built around, and
// reaching it with malformed-but-nonblank input is the same error class without
// relying on the guard. No store seam or connection teardown needed.
func TestReplenishLoader_FailsClosedWhenTheEpisodeReadErrors(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, nil)
	req, cfg, _ := replenishFixture(t, db, "RFC", 3)
	req.Threshold = 10 // one carrier covers it
	req.CurrentUOP = 0
	req.OriginID = "not-a-uuid" // errors against the origin_id UUID column
	req.OriginClass = string(protocol.OriginClassAttached)

	res, err := d.ReplenishLoader(req, cfg)
	if err != nil {
		t.Fatalf("ReplenishLoader returned an error; fail-closed returns a result with Skipped set, not an error: %v", err)
	}
	if len(res.Created) != 0 {
		t.Fatalf("created %d carrier(s) with an unreadable episode count; fail-closed must order nothing", len(res.Created))
	}
	if res.Skipped == "" {
		t.Error("Skipped is empty — a failed episode read must set Skipped with the reason, not pass silently")
	}
}

// TestReplenishLoader_FailsClosedWhenAnyReadDies pins the whole function's
// fail-CLOSED posture against a dead store: whatever guard's read fails first,
// no order is created and a reason is surfaced. That is the union of the two
// fail-closed sites in ReplenishLoader (the episode read → Skipped; the
// per-window count → HeldBy "window-check-failed") plus CheckDropoffCapacity's
// own fail-closed. Each is individually untestable without a fault-injection
// seam, because on a closed connection an earlier read always fails first;
// this test asserts the property that actually matters for the order-spam
// regression — a broken read can never produce a carrier.
//
// The per-window "window-check-failed" branch is the one that cannot be
// isolated here: it only fires on a read that errors AFTER CheckDropoffCapacity
// on the same node succeeded, i.e. a transient inter-read failure, which a
// real database cannot be coerced into. Pinning it in isolation would require
// a store fault-injection seam (a fake *store.DB or an error-injecting
// decorator), deliberately not added in this pass.
func TestReplenishLoader_FailsClosedWhenAnyReadDies(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, nil)
	req, cfg, windows := replenishFixture(t, db, "RWC", 3)
	req.Threshold = 10
	req.CurrentUOP = 0
	// A well-formed episode so the failure under test is the dead connection,
	// not the malformed-origin guard pinned in the test above.
	req.OriginID = "11111111-1111-1111-1111-111111111111"
	req.OriginClass = string(protocol.OriginClassAttached)

	// Kill the connection. The episode read errors first and fail-closes the
	// whole call; on a live connection with the episode read patched, the
	// window-loop reads would fail instead. Either way: no carrier is created.
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	res, err := d.ReplenishLoader(req, cfg)
	if err != nil {
		t.Fatalf("ReplenishLoader returned an error; fail-closed returns a result with a reason, not an error: %v", err)
	}
	if len(res.Created) != 0 {
		t.Fatalf("created %d carrier(s) against a dead connection; fail-closed was bypassed — the order-spam regression path is open", len(res.Created))
	}
	// Something must say why nothing was created. On a dead connection the
	// episode guard trips first (Skipped); with that patched, the windows would
	// be held. The contract is the union: at least one reason is surfaced.
	if res.Skipped == "" && len(res.HeldBy) == 0 {
		t.Errorf("neither Skipped nor HeldBy is set on a dead read (%+v); a fail-closed run must say why it ordered nothing", res)
	}
	_ = windows
}

// ── THE REPLENISH EMPTY RIDES A CARRIER THE LOADER'S PART MAY RIDE ──────────
//
// r7 demo run, 2026-10-10 (TRACE-r7-demo-before-stranded.md in the C2c run
// folder): the loader-replenish ask is born with no payload — an empty carrier
// is generic — and a loader with no declared carrier mix (no bin_loader_quotas,
// every loader in the demo plant) asked its market for ANY empty. Twice in 23
// pulls that was a STANDARD-SM carrier for a loader whose parts (STUD, BRKT)
// ride STANDARD only. The Edge loaded it, the carrier rule then refused it as a
// source for good, and its 40 UOP held the loader's threshold total satisfied:
// the loader never refilled and five orders stranded.
//
// The rule these pin: with no declared mix, the carrier types are the ones the
// loader's own payloads may ride (payload_bin_types). Exactly those; a payload
// with no rows lets any carrier through.

// carrierRuleLoader builds a no-quota shared-window loader at one window whose
// one payload rides `std` only, with its inbound market an NGRP of two slots.
// It returns the window, the market and the two slots.
func carrierRuleLoader(t *testing.T, db *store.DB, prefix string) (window, market *nodes.Node, slots [2]*nodes.Node, std, sm *bins.BinType) {
	t.Helper()
	setupTestData(t, db)
	grpType, err := db.GetNodeTypeByCode("NGRP")
	testutil.MustNoErr(t, err, "NGRP type")
	market = &nodes.Node{Name: prefix + "-MKT", IsSynthetic: true, NodeTypeID: &grpType.ID, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(market), "create the market")
	for i := range slots {
		s := &nodes.Node{Name: fmt.Sprintf("%s-MKT-%d", prefix, i+1), Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(s), "create a market slot")
		testutil.MustNoErr(t, db.SetNodeParent(s.ID, market.ID), "parent the slot")
		slots[i] = s
	}
	window = &nodes.Node{Name: prefix + "-WIN", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(window), "create the loader window")

	std = &bins.BinType{Code: prefix + "-STANDARD"}
	testutil.MustNoErr(t, db.CreateBinType(std), "create STANDARD")
	sm = &bins.BinType{Code: prefix + "-STANDARD-SM"}
	testutil.MustNoErr(t, db.CreateBinType(sm), "create STANDARD-SM")
	part := &payloads.Payload{Code: prefix + "-STUD", UOPCapacity: 40}
	testutil.MustNoErr(t, db.CreatePayload(part), "create the part")
	testutil.MustNoErr(t, db.SetPayloadBinTypes(part.ID, []int64{std.ID}), "the part rides STANDARD only")

	loaderID, err := db.CreateLoader(store.Loader{
		Name: prefix + "-LOADER", Role: "produce", Layout: loaders.LayoutSharedWindow,
		Replenishment: "threshold", InboundSource: market.Name, OutboundDest: market.Name,
	})
	testutil.MustNoErr(t, err, "create the loader")
	testutil.MustNoErr(t, db.UpsertLoaderHome(store.LoaderHome{LoaderID: loaderID, PositionNodeID: window.ID}),
		"the window is the loader's home")
	testutil.MustNoErr(t, db.UpsertLoaderPayload(store.LoaderPayload{LoaderID: loaderID, PayloadCode: part.Code, UOPThreshold: 40}),
		"the loader stages the part")
	// No quotas: no declared carrier mix, which is every loader in the demo plant.
	return window, market, slots, std, sm
}

// emptyCarrier puts an empty carrier of one type on a node.
func emptyCarrier(t *testing.T, db *store.DB, bt *bins.BinType, node *nodes.Node, label string) *bins.Bin {
	t.Helper()
	b := &bins.Bin{BinTypeID: bt.ID, Label: label, NodeID: &node.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(b), "create "+label)
	return b
}

// replenishAskPick admits the loader's empty pull exactly as the replenish loop
// births it (AdmitCoreAsk, no payload) and asks the finder for its carrier.
func replenishAskPick(t *testing.T, d *Dispatcher, db *store.DB, market, window *nodes.Node) SourceResult {
	t.Helper()
	order, err := d.AdmitCoreAsk(CoreAskSpec{
		UUIDPrefix: "core-l1-", StationID: "edge.line1",
		SourceNode: market.Name, DeliveryNode: window.Name,
	})
	testutil.MustNoErr(t, err, "admit the replenish ask")
	o, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "read the ask back")
	if o.PayloadCode != "" {
		t.Fatalf("the ask was born carrying payload %q; this pin is about the generic empty", o.PayloadCode)
	}
	return d.planner.finder.FindSource(o, IntentEmpty)
}

// TestReplenishAsk_NoMixTakesACarrierItsPartMayRide: an accessible STANDARD-SM
// empty ranks ahead of a STANDARD one; the STUD loader's ask takes the STANDARD.
func TestReplenishAsk_NoMixTakesACarrierItsPartMayRide(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	window, market, slots, std, sm := carrierRuleLoader(t, db, "RCR")
	wrong := emptyCarrier(t, db, sm, slots[0], "RCR-SM") // lower id: ranked first
	right := emptyCarrier(t, db, std, slots[1], "RCR-STD")

	d, _ := newTestDispatcher(t, db, nil)
	res := replenishAskPick(t, d, db, market, window)
	if res.Outcome != OutcomeFound || res.Bin == nil {
		t.Fatalf("the ask did not find a carrier (outcome %v, cause %q); a STANDARD empty stands in the market",
			res.Outcome, res.QueueCause)
	}
	if res.Bin.ID == wrong.ID {
		t.Fatalf("the ask took %s (%s) for a loader whose part rides %s only: the Edge will load it and the "+
			"carrier rule will refuse it as a source for good", wrong.Label, sm.Code, std.Code)
	}
	if res.Bin.ID != right.ID {
		t.Errorf("the ask took bin %d, want the STANDARD empty %d", res.Bin.ID, right.ID)
	}
}

// TestReplenishAsk_NoMixWaitsRatherThanTakeTheWrongCarrier: with only a
// STANDARD-SM empty in the market, the STUD loader's ask waits under the typed
// cause instead of taking it.
func TestReplenishAsk_NoMixWaitsRatherThanTakeTheWrongCarrier(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	window, market, slots, _, sm := carrierRuleLoader(t, db, "RCW")
	wrong := emptyCarrier(t, db, sm, slots[0], "RCW-SM")

	d, _ := newTestDispatcher(t, db, nil)
	res := replenishAskPick(t, d, db, market, window)
	if res.Outcome == OutcomeFound {
		t.Fatalf("the ask took bin %d (%s is %s, which the loader's part may not ride); it must wait",
			res.Bin.ID, wrong.Label, sm.Code)
	}
	if res.Outcome != OutcomeWait || res.QueueCause != CauseFinderNoEmptyOfType {
		t.Errorf("outcome %v cause %q, want a wait under %q — the market has an empty, just not one this loader can use",
			res.Outcome, res.QueueCause, CauseFinderNoEmptyOfType)
	}
}

// TestReplenishAsk_NoMixCarrierSetIsTheUnionAndAnyAbsorbs pins the set arm and
// the "any" arm of the same rule. A second staged payload that rides
// STANDARD-SM makes the set {STANDARD, STANDARD-SM}, searched as one, so the
// first-ranked SM empty is now a carrier the loader can fill. A third payload
// with no payload_bin_types rows constrains nothing: the window takes any.
func TestReplenishAsk_NoMixCarrierSetIsTheUnionAndAnyAbsorbs(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	window, market, slots, std, sm := carrierRuleLoader(t, db, "RCU")
	smBin := emptyCarrier(t, db, sm, slots[0], "RCU-SM")
	emptyCarrier(t, db, std, slots[1], "RCU-STD")
	home, err := db.GetLoaderHomeByPositionNode(window.ID)
	testutil.MustNoErr(t, err, "read the home")

	panel := &payloads.Payload{Code: "RCU-PANEL", UOPCapacity: 30}
	testutil.MustNoErr(t, db.CreatePayload(panel), "create the second part")
	testutil.MustNoErr(t, db.SetPayloadBinTypes(panel.ID, []int64{sm.ID}), "it rides STANDARD-SM only")
	testutil.MustNoErr(t, db.UpsertLoaderPayload(store.LoaderPayload{LoaderID: home.LoaderID, PayloadCode: panel.Code}),
		"the loader stages it too")

	_, carriers, err := db.GetLoaderHomeCarriers(window.ID)
	testutil.MustNoErr(t, err, "read the carriers")
	if strings.Join(carriers, ",") != std.Code+","+sm.Code {
		t.Fatalf("carriers = %v, want [%s %s] — the union of what the staged parts may ride", carriers, std.Code, sm.Code)
	}
	d, _ := newTestDispatcher(t, db, nil)
	if res := replenishAskPick(t, d, db, market, window); res.Outcome != OutcomeFound || res.Bin.ID != smBin.ID {
		t.Errorf("outcome %v bin %v, want the first-ranked SM empty %d — the set admits it", res.Outcome, res.Bin, smBin.ID)
	}

	testutil.MustNoErr(t, db.UpsertLoaderPayload(store.LoaderPayload{LoaderID: home.LoaderID, PayloadCode: "RCU-UNDESCRIBED"}),
		"a staged part nobody described")
	if _, carriers, err = db.GetLoaderHomeCarriers(window.ID); err != nil || carriers != nil {
		t.Errorf("carriers = %v (%v), want nil — a part with no rows constrains nothing, so any carrier", carriers, err)
	}
}

// TestReplenishLoader_AnOperatorAsksForAsManyAsTheyNamed: the operator origin
// (Want set) is sized as asked — no threshold arithmetic — at a loader a person
// stages (operator-driven), bounded by the windows like every ask: three asked at
// two windows makes two, and a window that cannot take one is held by name.
func TestReplenishLoader_AnOperatorAsksForAsManyAsTheyNamed(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, nil)
	req, cfg, _ := replenishFixture(t, db, "ROP", 2)
	cfg.Replenishment = loaders.ReplenishmentOperator
	req.Threshold, req.CurrentUOP, req.PerBinCapacity = 0, 0, 0
	req.Want = 3

	res, err := d.ReplenishLoader(req, cfg)
	if err != nil {
		t.Fatalf("ReplenishLoader: %v", err)
	}
	if res.Want != 3 || len(res.Created) != 2 {
		t.Errorf("operator ask of 3 at 2 windows: want %d created %d (skipped %q), want 3 and 2", res.Want, len(res.Created), res.Skipped)
	}

	again, err := d.ReplenishLoader(req, cfg)
	if err != nil {
		t.Fatalf("ReplenishLoader again: %v", err)
	}
	if len(again.Created) != 0 || len(again.HeldBy) != 2 {
		t.Errorf("the same ask with both windows taken: created %d held %v, want 0 and both windows named", len(again.Created), again.HeldBy)
	}
}
