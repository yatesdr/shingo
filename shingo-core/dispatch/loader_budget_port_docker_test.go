//go:build docker

package dispatch

import (
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingo/shared/loadervectors"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// loader_budget_port_docker_test.go — the Edge's never-2N seam, asserted on
// Core.
//
// The Edge decides fulls to a consume loader, the unloader's U1 and the manual
// retrieve at a loader window under its own per-loader lock
// (withLoaderBudget, createUnloaderFullIns). Before that code moves, what its
// pins assert is restated here against ReplenishLoader, the one door it moves
// to. Each test names the Edge pin it ports (shingo-edge/engine:
// loader_reservation_seam_test, loader_budget_lock_pin_test,
// unloader_sweep_characterization_test, side_cycle_pins_test,
// loader_push_characterization_test).
//
// PREDICTIONS. What Core could not do when these were written was written
// before the source that makes it true, and skipped until then: a full pulled
// by a release (lifted when ReplenishLoader took the side-cycle origin), and
// one decision at a time per loader, with the consume set's burst case (lifted
// when the per-loader lock moved into ReplenishLoader). With that lock taken
// out, eight concurrent asks left four live empties at one window.
//
// side_cycle_pins_test's assertions are the Edge's triggers (LOAD, CLEAR, an
// L2's landing re-pushing its own loader); the triggers stay on the Edge and
// ask, so what lands here is the outcome they rely on: a second push with an
// empty in flight orders nothing (TestReplenishLoader_AnOperatorAsksForAsManyAsTheyNamed).

// liveAt counts the live orders of one order type delivering to node.
func liveAt(t *testing.T, db *store.DB, node string, typ protocol.OrderType) int {
	t.Helper()
	var n int
	err := db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM orders WHERE delivery_node = $1 AND order_type = $2 AND status NOT IN (%s)`,
		protocol.TerminalStatusSQLList()), node, string(typ)).Scan(&n)
	testutil.MustNoErr(t, err, "count live orders at "+node)
	return n
}

// liveIDs lists the live orders delivering to any of nodes.
func liveIDs(t *testing.T, db *store.DB, nodes []string) []int64 {
	t.Helper()
	var ids []int64
	for _, n := range nodes {
		rows, err := db.Query(fmt.Sprintf(`SELECT id FROM orders WHERE delivery_node = $1 AND status NOT IN (%s) ORDER BY id`,
			protocol.TerminalStatusSQLList()), n)
		testutil.MustNoErr(t, err, "list live orders at "+n)
		for rows.Next() {
			var id int64
			testutil.MustNoErr(t, rows.Scan(&id), "scan a live order")
			ids = append(ids, id)
		}
		testutil.MustNoErr(t, rows.Err(), "live orders at "+n)
		rows.Close()
	}
	return ids
}

// operatorAsk is a person's ask for count empties of payload at the loader:
// the operator origin.
func operatorAsk(req ReplenishRequest, payload string, count int) ReplenishRequest {
	req.Origin, req.PayloadCode, req.Want = ReplenishByOperator, payload, count
	req.Threshold, req.CurrentUOP, req.PerBinCapacity = 0, 0, 0
	return req
}

// TestPortedBudget_PropNeverExceedsBudget ports
// TestWithLoaderBudget_PropNeverExceedsBudget: a deterministic random run of
// asks for two payloads at a one-window loader, with completions between them.
// Live empties at the window never exceed its budget of one.
func TestPortedBudget_PropNeverExceedsBudget(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, nil)
	req, cfg, windows := replenishFixture(t, db, "PNB", 1)
	cfg.Payloads = []loaders.Payload{{PayloadCode: "P1"}, {PayloadCode: "P2"}}
	cfg.Replenishment = loaders.ReplenishmentOperator
	rng := rand.New(rand.NewSource(20260612))
	asked := 0
	for step := range 200 {
		switch rng.Intn(3) {
		case 0, 1:
			if _, err := d.ReplenishLoader(operatorAsk(req, []string{"P1", "P2"}[rng.Intn(2)], rng.Intn(2)+1), cfg); err != nil {
				t.Fatalf("step %d: ReplenishLoader: %v", step, err)
			}
			asked++
		case 2:
			if ids := liveIDs(t, db, windows); len(ids) > 0 {
				if _, err := db.TerminalizeOrder(ids[rng.Intn(len(ids))], protocol.StatusConfirmed, "test: complete"); err != nil {
					t.Fatalf("step %d: complete: %v", step, err)
				}
			}
		}
		if n := liveAt(t, db, windows[0], OrderTypeRetrieveEmpty); n > 1 {
			t.Fatalf("step %d: %d live empties at a one-window loader, budget 1", step, n)
		}
	}
	if asked == 0 {
		t.Fatal("run asked nothing")
	}
}

// TestPortedBudget_PropOccupancyLive ports TestWithLoaderBudget_PropOccupancyLive:
// a three-window loader, asks of random size, carriers arriving at and leaving
// windows between steps, completions. A: live empties across the windows never
// exceed three. B: no window gains an empty while a carrier stands on it.
func TestPortedBudget_PropOccupancyLive(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, nil)
	req, cfg, windows := replenishFixture(t, db, "POL", 3)
	cfg.Payloads = []loaders.Payload{{PayloadCode: "P1"}, {PayloadCode: "P2"}}
	cfg.Replenishment = loaders.ReplenishmentOperator
	standing := map[string]*bins.Bin{}
	nodeOf := map[string]*nodes.Node{}
	for _, w := range windows {
		n, err := db.GetNodeByDotName(w)
		testutil.MustNoErr(t, err, "resolve "+w)
		nodeOf[w] = n
	}
	rng := rand.New(rand.NewSource(20260801))
	fired, occupiedAsks := 0, 0
	for step := range 300 {
		switch rng.Intn(4) {
		case 0, 1:
			before := map[string]int{}
			for _, w := range windows {
				before[w] = liveAt(t, db, w, OrderTypeRetrieveEmpty)
			}
			if len(standing) > 0 {
				occupiedAsks++
			}
			res, err := d.ReplenishLoader(operatorAsk(req, []string{"P1", "P2"}[rng.Intn(2)], rng.Intn(len(windows))+1), cfg)
			if err != nil {
				t.Fatalf("step %d: ReplenishLoader: %v", step, err)
			}
			fired += len(res.Created)
			for _, w := range windows {
				if after := liveAt(t, db, w, OrderTypeRetrieveEmpty); after > before[w] && standing[w] != nil {
					t.Errorf("step %d: window %s gained an empty (%d -> %d) while a carrier stood on it", step, w, before[w], after)
				}
			}
		case 2:
			w := windows[rng.Intn(len(windows))]
			if b := standing[w]; b != nil {
				testutil.MustNoErr(t, db.DeleteBin(b.ID), "a carrier leaves "+w)
				delete(standing, w)
			} else {
				standing[w] = makeEmptyBin(t, db, nodeOf[w].ID, fmt.Sprintf("POL-%d", step))
			}
		case 3:
			if ids := liveIDs(t, db, windows); len(ids) > 0 {
				if _, err := db.TerminalizeOrder(ids[rng.Intn(len(ids))], protocol.StatusConfirmed, "test: complete"); err != nil {
					t.Fatalf("step %d: complete: %v", step, err)
				}
			}
		}
		total := 0
		for _, w := range windows {
			total += liveAt(t, db, w, OrderTypeRetrieveEmpty)
		}
		if total > len(windows) {
			t.Fatalf("step %d: %d live empties across %d windows", step, total, len(windows))
		}
	}
	if fired == 0 || occupiedAsks == 0 {
		t.Fatalf("run exercised nothing: %d created, %d asks with a carrier standing", fired, occupiedAsks)
	}
}

// TestPortedBudget_AResidentEmptyHoldsTheOnlyWindow ports
// TestWithLoaderBudget_SuppressesWhenWindowHasResidentEmpty (Springfield
// SMN_014, 2026-07-23) and its control _FiresWhenWindowEmpty: an empty standing
// on a one-window loader's window holds it, by name, with no order in flight;
// the same ask at the free window orders one.
func TestPortedBudget_AResidentEmptyHoldsTheOnlyWindow(t *testing.T) {
	t.Parallel()
	for _, resident := range []bool{true, false} {
		t.Run(fmt.Sprintf("resident=%v", resident), func(t *testing.T) {
			t.Parallel()
			db := testDB(t)
			d, _ := newTestDispatcher(t, db, nil)
			req, cfg, windows := replenishFixture(t, db, "RES", 1)
			cfg.Replenishment = loaders.ReplenishmentOperator
			if resident {
				n, err := db.GetNodeByDotName(windows[0])
				testutil.MustNoErr(t, err, "resolve the window")
				makeEmptyBin(t, db, n.ID, "RES-EMPTY")
			}
			res, err := d.ReplenishLoader(operatorAsk(req, req.PayloadCode, 1), cfg)
			testutil.MustNoErr(t, err, "ReplenishLoader")
			want := 1
			if resident {
				want = 0
				if res.HeldBy[windows[0]] == "" {
					t.Errorf("HeldBy = %v, want the window named", res.HeldBy)
				}
			}
			if len(res.Created) != want {
				t.Errorf("created %d, want %d", len(res.Created), want)
			}
		})
	}
}

// sideCycleAsk is a release's U1 asked of Core: ReplenishLoader's side-cycle
// origin, a full of the request's payload, want one.
func sideCycleAsk(d *Dispatcher, req ReplenishRequest, cfg LoaderReplenishConfig) (ReplenishResult, error) {
	req.Origin, req.OrderType, req.Want = ReplenishBySideCycle, OrderTypeRetrieve, 1
	return d.ReplenishLoader(req, cfg)
}

// consumeFixture builds a consume case's unloader on Core: its windows (named
// prefix + the vector's node), the FG market it pulls from, the standing
// carriers and the inbound fulls. It returns the request, the config and the
// map from a real window name back to the vector's.
func consumeFixture(t *testing.T, db *store.DB, prefix string, c loadervectors.ConsumeCase) (ReplenishRequest, LoaderReplenishConfig, map[string]string) {
	t.Helper()
	setupTestData(t, db)
	market := &nodes.Node{Name: prefix + "-FG-MARKET", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(market), "create the FG market")
	cfg := LoaderReplenishConfig{
		Layout:        loaders.LayoutSharedWindow,
		FunnelWindows: c.FunnelWindows,
		NodeNames:     map[int64]string{},
		Replenishment: loaders.ReplenishmentOperator,
		InboundSource: market.Name,
	}
	for _, p := range c.Payloads {
		cfg.Payloads = append(cfg.Payloads, loaders.Payload{PayloadCode: p})
	}
	back := map[string]string{}
	for i, w := range c.Windows {
		n := &nodes.Node{Name: prefix + "-" + w.Node, Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(n), "create window "+w.Node)
		cfg.Homes = append(cfg.Homes, loaders.Home{PositionNodeID: n.ID, SortOrder: i + 1})
		cfg.NodeNames[n.ID] = n.Name
		back[n.Name] = w.Node
		switch {
		case w.StandingFull != "":
			makeLoaderBin(t, db, w.StandingFull, n.ID, n.Name+"-FULL", 10, time.Now().Add(-time.Hour))
		case w.StandingEmpty:
			makeEmptyBin(t, db, n.ID, n.Name+"-EMPTY")
		}
		if w.InboundFull != "" {
			testutil.MustNoErr(t, db.CreateOrder(&orders.Order{
				EdgeUUID: n.Name + "-INBOUND", StationID: "edge.line1", OrderType: OrderTypeRetrieve,
				Status: StatusPending, Quantity: 1, SourceNode: market.Name, DeliveryNode: n.Name,
				PayloadCode: w.InboundFull, SourceIntent: SourceIntentForType(OrderTypeRetrieve),
			}), "the full inbound to "+w.Node)
		}
	}
	return ReplenishRequest{StationID: "edge.line1", LoaderID: 77}, cfg, back
}

// TestConsumeVectors_Core runs shared/loadervectors' consume cases through
// ReplenishLoader's side-cycle origin: the Edge's U1 answers (TestConsumeVectors_Edge),
// asked of Core.
func TestConsumeVectors_Core(t *testing.T) {
	t.Parallel()
	for i, c := range loadervectors.MustLoad().Consume {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			db := testDB(t)
			d, _ := newTestDispatcher(t, db, nil)
			req, cfg, back := consumeFixture(t, db, fmt.Sprintf("CV%d", i), c)
			got := map[string][]string{}
			var mu sync.Mutex
			ask := func(p string) {
				r := req
				r.PayloadCode = p
				res, err := sideCycleAsk(d, r, cfg)
				if err != nil {
					t.Errorf("release of %s: %v", p, err)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				for _, o := range res.Created {
					if o.OrderType != OrderTypeRetrieve {
						t.Errorf("release of %s ordered a %s, want a full (retrieve)", p, o.OrderType)
					}
					got[back[o.DeliveryNode]] = append(got[back[o.DeliveryNode]], o.PayloadCode)
				}
			}
			if c.Burst {
				var wg sync.WaitGroup
				for _, p := range c.Events {
					wg.Add(1)
					go func() { defer wg.Done(); ask(p) }()
				}
				wg.Wait()
			} else {
				for _, p := range c.Events {
					ask(p)
				}
			}
			want := map[string][]string{}
			for w, p := range c.WantNew {
				want[w] = []string{p}
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("new fulls by window = %v, want %v\nwhy this case exists: %s", got, want, c.Why)
			}
		})
	}
}

// TestConsumeVectors_CoverTheBriefsShapes guards the consume set itself: a
// file that lost its burst, its parked full, its full already coming or its
// funnel would pass every case above while pinning less than it appears to.
func TestConsumeVectors_CoverTheBriefsShapes(t *testing.T) {
	t.Parallel()
	var burst, parked, inbound, funnel, multi int
	for _, c := range loadervectors.MustLoad().Consume {
		if c.Burst && len(c.Events) > 1 && len(c.Windows) > 1 {
			burst++
		}
		if c.FunnelWindows {
			funnel++
		}
		if len(c.Windows) > 1 {
			multi++
		}
		for _, w := range c.Windows {
			if w.StandingFull != "" {
				parked++
			}
			if w.InboundFull != "" {
				inbound++
			}
		}
	}
	if burst == 0 || parked == 0 || inbound == 0 || funnel == 0 || multi == 0 {
		t.Errorf("consume vectors: burst=%d parked=%d inbound=%d funnel=%d multi-window=%d; each must be non-zero",
			burst, parked, inbound, funnel, multi)
	}
}

// TestPortedBudget_SideCycleWithNoInboundReadsNothing ports
// TestUnloaderSweep_NoInboundSource_ZeroReads: an unloader fed directly (no
// inbound source) is skipped before any statement, with a reason.
func TestPortedBudget_SideCycleWithNoInboundReadsNothing(t *testing.T) {
	t.Parallel()
	db, cfgDB := testdb.OpenWithConfig(t)
	c := loadervectors.ConsumeCase{Payloads: []string{"PART-A"}, Windows: []loadervectors.ConsumeWindow{{Node: "W1"}, {Node: "W2"}}}
	req, cfg, _ := consumeFixture(t, db, "SNI", c)
	cfg.InboundSource = ""
	cdb, counter, err := store.OpenCounting(cfgDB)
	testutil.MustNoErr(t, err, "counting db")
	t.Cleanup(func() { cdb.Close() })
	d, _ := newTestDispatcher(t, cdb, nil)
	req.PayloadCode = "PART-A"
	counter.Reset()
	res, err := sideCycleAsk(d, req, cfg)
	testutil.MustNoErr(t, err, "ReplenishLoader")
	if n := counter.Count(); n != 0 {
		t.Errorf("a directly fed unloader cost %d statements, want 0: %v", n, counter.Statements())
	}
	if len(res.Created) != 0 || res.Skipped == "" {
		t.Errorf("created %d, skipped %q; want none and a reason", len(res.Created), res.Skipped)
	}
}

// TestPortedBudget_SideCycleIsNotGatedByReplenishment ports
// TestUnloaderSweep_ReplenishmentDoesNotGateTheDrain: a release's full is
// pulled at an unloader stored as operator (what Core stores for every
// unloader) and as threshold alike. The operator-driven refusal is the
// threshold origin's, and a release is not a threshold.
func TestPortedBudget_SideCycleIsNotGatedByReplenishment(t *testing.T) {
	t.Parallel()
	for _, repl := range []string{loaders.ReplenishmentOperator, loaders.ReplenishmentThreshold} {
		t.Run(repl, func(t *testing.T) {
			t.Parallel()
			db := testDB(t)
			d, _ := newTestDispatcher(t, db, nil)
			c := loadervectors.ConsumeCase{Payloads: []string{"PART-A"}, Windows: []loadervectors.ConsumeWindow{{Node: "W1"}}}
			req, cfg, _ := consumeFixture(t, db, "SRG", c)
			cfg.Replenishment = repl
			req.PayloadCode = "PART-A"
			res, err := sideCycleAsk(d, req, cfg)
			testutil.MustNoErr(t, err, "ReplenishLoader")
			if len(res.Created) != 1 {
				t.Errorf("replenishment %s: created %d (skipped %q), want 1", repl, len(res.Created), res.Skipped)
			}
		})
	}
}

// TestPortedBudget_SideCycleFailsClosed ports
// TestCreateUnloaderFullIn_HoldsWhenOccupancyReadFails: a release asked of a
// Core whose reads die orders nothing and says why.
func TestPortedBudget_SideCycleFailsClosed(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, nil)
	c := loadervectors.ConsumeCase{Payloads: []string{"PART-A"}, Windows: []loadervectors.ConsumeWindow{{Node: "W1"}, {Node: "W2"}}}
	req, cfg, _ := consumeFixture(t, db, "SFC", c)
	req.PayloadCode = "PART-A"
	testutil.MustNoErr(t, db.Close(), "close the db")
	res, err := sideCycleAsk(d, req, cfg)
	if err != nil {
		t.Fatalf("a dead read is a reason, not an error: %v", err)
	}
	if len(res.Created) != 0 || (res.Skipped == "" && len(res.HeldBy) == 0) {
		t.Errorf("created %d, skipped %q, held %v; want none and a reason", len(res.Created), res.Skipped, res.HeldBy)
	}
}

// TestPortedBudget_OneDecisionAtATimePerLoader ports the lock the Edge's seam
// tests pin (TestUnloaderSweep_LockHeldAtDecisionPerLoader,
// TestWithLoaderBudget_PropNeverExceedsBudget's serial count->fire): eight asks
// for three empties each, all at once, at a three-window loader, make three
// orders, one per window. Without one decision at a time per loader each ask
// reads three free windows before any inserts.
func TestPortedBudget_OneDecisionAtATimePerLoader(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, nil)
	req, cfg, windows := replenishFixture(t, db, "ODL", 3)
	cfg.Replenishment = loaders.ReplenishmentOperator
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := d.ReplenishLoader(operatorAsk(req, req.PayloadCode, 3), cfg); err != nil {
				t.Errorf("ReplenishLoader: %v", err)
			}
		}()
	}
	wg.Wait()
	for _, w := range windows {
		if n := liveAt(t, db, w, OrderTypeRetrieveEmpty); n != 1 {
			t.Errorf("window %s: %d live empties, want 1", w, n)
		}
	}
}

// TestPortedBudget_TheSideCycleReadIsOneStatement counts the read the side
// cycle adds (orders.ReadPartAtLoader): a release's full-in at a free
// three-window unloader costs exactly one statement more than a person's
// unattributed ask for one carrier at the same windows, which subtracts
// nothing and so reads nothing to subtract.
func TestPortedBudget_TheSideCycleReadIsOneStatement(t *testing.T) {
	t.Parallel()
	cost := func(side bool) int64 {
		db, cfgDB := testdb.OpenWithConfig(t)
		c := loadervectors.ConsumeCase{Payloads: []string{"PART-A"}, Windows: []loadervectors.ConsumeWindow{{Node: "W1"}, {Node: "W2"}, {Node: "W3"}}}
		req, cfg, _ := consumeFixture(t, db, "SOS", c)
		cdb, counter, err := store.OpenCounting(cfgDB)
		testutil.MustNoErr(t, err, "counting db")
		t.Cleanup(func() { cdb.Close() })
		d, _ := newTestDispatcher(t, cdb, nil)
		req.PayloadCode = "PART-A"
		counter.Reset()
		var res ReplenishResult
		if side {
			res, err = sideCycleAsk(d, req, cfg)
		} else {
			req.Origin, req.OrderType, req.Want = ReplenishByOperator, OrderTypeRetrieve, 1
			res, err = d.ReplenishLoader(req, cfg)
		}
		testutil.MustNoErr(t, err, "ReplenishLoader")
		if len(res.Created) != 1 {
			t.Fatalf("side=%v: created %d (skipped %q), want 1", side, len(res.Created), res.Skipped)
		}
		return counter.Count()
	}
	person, release := cost(false), cost(true)
	if release != person+1 {
		t.Errorf("a release's full-in cost %d statements, a person's ask %d; want exactly one more", release, person)
	}
}
