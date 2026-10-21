package engine

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"shingo/protocol"
	"shingo/shared/loadervectors"
	"shingoedge/domain"
	"shingoedge/orders"
	"shingoedge/store/processes"
)

// Characterisation pins for the unloader auto-push sweep (pushUnloadersViaSeam)
// and its P=1 entry (MaybeCreateUnloaderFullIn). They pin what the sweep does
// today, so a rewrite of the sweep shows up as the predicted diff and nothing
// else. The occupancy they decide from is the bins table's (holdSweep).

// holdSweep holds, as a table eng may decide from, Core's rows for every
// place in eng's store: free, or occupied by the payload resident names.
func holdSweep(t *testing.T, eng *Engine, resident map[string]string) {
	t.Helper()
	holdPlaces(t, eng, func(n string) (NodeBinInfo, bool) {
		p, ok := resident[n]
		return NodeBinInfo{NodeName: n, Occupied: ok, PayloadCode: p}, true
	})
}

// unloaderInfo builds a shared_window consume loader over windows serving
// payloads, operator replenishment (what Core stores for every unloader), pulling
// from FG-SUPER.
func unloaderInfo(name string, windows, payloads []string) protocol.LoaderInfo {
	pos := make([]protocol.LoaderPosition, len(windows))
	for i, w := range windows {
		pos[i] = protocol.LoaderPosition{CoreNodeName: w}
	}
	ps := make([]protocol.LoaderPayloadInfo, len(payloads))
	for i, p := range payloads {
		ps[i] = protocol.LoaderPayloadInfo{PayloadCode: p}
	}
	return protocol.LoaderInfo{
		Name:          name,
		LoaderKey:     "loader:" + name,
		Role:          "consume",
		Layout:        "shared_window",
		Replenishment: protocol.LoaderReplenishmentOperator,
		InboundSource: "FG-SUPER",
		OutboundDest:  "EMPTY-TOTES",
		Positions:     pos,
		Payloads:      ps,
	}
}

// fullsByWindow maps each window to the payloads of the non-terminal U1
// full-ins delivered to it.
func fullsByWindow(t *testing.T, eng *Engine, windows []string) map[string][]string {
	t.Helper()
	list, err := eng.db.ListActiveOrdersByDeliveryNodeSet(windows)
	if err != nil {
		t.Fatalf("ListActiveOrdersByDeliveryNodeSet: %v", err)
	}
	out := map[string][]string{}
	for _, o := range list {
		if !o.RetrieveEmpty {
			out[o.DeliveryNode] = append(out[o.DeliveryNode], o.PayloadCode)
		}
	}
	return out
}

// captureBudgetLines installs a logFn that keeps every loader_budget line.
func captureBudgetLines(eng *Engine) *[]string {
	var mu sync.Mutex
	lines := []string{}
	eng.logFn = func(f string, a ...any) {
		line := fmt.Sprintf(f, a...)
		if strings.HasPrefix(line, "loader_budget ") {
			mu.Lock()
			lines = append(lines, line)
			mu.Unlock()
		}
	}
	return &lines
}

// TestUnloaderSweep_NoInboundSource_ZeroReads: a consume loader with no inbound
// source returns before any read — no Core node-bins call and no Edge DB
// statement for the whole sweep.
func TestUnloaderSweep_NoInboundSource_ZeroReads(t *testing.T) {
	t.Parallel()
	db, counter := testEngineDBCounting(t)
	eng := testEngine(t, db)
	windows := []string{"NI-W1", "NI-W2"}
	seedWindowNodes(t, db, "NI-PROC", windows)
	info := unloaderInfo("NI", windows, []string{"PART-A", "PART-B"})
	info.InboundSource = ""
	seedCoreLoader(t, eng, info)
	holdSweep(t, eng, nil)

	counter.Reset()
	eng.pushUnloadersViaSeam()

	if got := counter.Count(); got != 0 {
		t.Errorf("no-inbound unloader: DB statements = %d, want 0", got)
	}

	// Positive control: the same loader WITH an inbound source reads the order
	// table, so the zero above is the gate and not a counter that never counts.
	info.InboundSource = "FG-SUPER"
	seedCoreLoader(t, eng, info)
	counter.Reset()
	eng.pushUnloadersViaSeam()
	if counter.Count() == 0 {
		t.Fatal("control: DB statements = 0, want > 0")
	}
}

// TestUnloaderSweep_ZeroPayloads_ZeroReads: a consume loader whose shared
// payload set is empty — a dedicated_positions loader, whose payloads live on
// its positions — is offered nothing by the sweep and so reads nothing.
func TestUnloaderSweep_ZeroPayloads_ZeroReads(t *testing.T) {
	t.Parallel()
	db, counter := testEngineDBCounting(t)
	eng := testEngine(t, db)
	seedWindowNodes(t, db, "ZP-PROC", []string{"ZP-P1"})
	seedCoreLoader(t, eng, protocol.LoaderInfo{
		Name: "ZP", LoaderKey: "loader:ZP", Role: "consume", Layout: "dedicated_positions",
		Replenishment: protocol.LoaderReplenishmentOperator, InboundSource: "FG-SUPER",
		Positions: []protocol.LoaderPosition{{CoreNodeName: "ZP-P1", PayloadCode: "PART-A"}},
	})
	ls, err := eng.loaderStore.Loaders(domain.RoleConsume)
	if err != nil || len(ls) != 1 {
		t.Fatalf("dedicated consume loader must project: loaders=%d err=%v", len(ls), err)
	}
	if n := len(ls[0].PayloadSet()); n != 0 {
		t.Fatalf("fixture premise: dedicated loader PayloadSet has %d entries, want 0", n)
	}
	holdSweep(t, eng, nil)

	counter.Reset()
	eng.pushUnloadersViaSeam()

	if got := counter.Count(); got != 0 {
		t.Errorf("zero-payload unloader: DB statements = %d, want 0", got)
	}
}

// TestUnloaderSweep_PayloadSpecificGuard runs the three cases of
// TestCreateUnloaderFullIn_PayloadSpecificGuard through the two public entries:
// MaybeCreateUnloaderFullIn (one payload) and the sweep (every payload of the
// loader). A full of PART-A is resident on W1.
func TestUnloaderSweep_PayloadSpecificGuard(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		windows []string
		entry   func(*Engine)
		want    map[string][]string
	}{
		{
			name: "maybe_create_multi_window_same_payload_suppresses", windows: []string{"W1", "W2"},
			entry: func(e *Engine) { e.MaybeCreateUnloaderFullIn("PART-A") },
			want:  map[string][]string{},
		},
		{
			name: "maybe_create_multi_window_other_payload_routes_to_free_window", windows: []string{"W1", "W2"},
			entry: func(e *Engine) { e.MaybeCreateUnloaderFullIn("PART-B") },
			want:  map[string][]string{"W2": {"PART-B"}},
		},
		{
			name: "maybe_create_single_window_other_payload_suppressed_by_budget", windows: []string{"W1"},
			entry: func(e *Engine) { e.MaybeCreateUnloaderFullIn("PART-B") },
			want:  map[string][]string{},
		},
		{
			name: "sweep_multi_window", windows: []string{"W1", "W2"},
			entry: func(e *Engine) { e.pushUnloadersViaSeam() },
			want:  map[string][]string{"W2": {"PART-B"}},
		},
		{
			name: "sweep_single_window", windows: []string{"W1"},
			entry: func(e *Engine) { e.pushUnloadersViaSeam() },
			want:  map[string][]string{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testEngineDB(t)
			eng := testEngine(t, db)
			seedWindowNodes(t, db, "PSG-PROC", tc.windows)
			seedCoreLoader(t, eng, unloaderInfo("PSG", tc.windows, []string{"PART-A", "PART-B"}))
			holdSweep(t, eng, map[string]string{"W1": "PART-A"})

			tc.entry(eng)

			got := fullsByWindow(t, eng, tc.windows)
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("U1s by window = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestUnloaderSweep_NotActableFiresNothing: a bins table the Edge may not
// decide from (none held) — no U1 for any payload of the loader, one
// loader_budget line per payload (each occupancy=not_actable, to_fire=0), and
// one debug line saying the parked-full guard could not see.
func TestUnloaderSweep_NotActableFiresNothing(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	windows := []string{"UR-W1", "UR-W2"}
	seedWindowNodes(t, db, "UR-PROC", windows)
	seedCoreLoader(t, eng, unloaderInfo("UR", windows, []string{"PART-A", "PART-B"}))
	lines := captureBudgetLines(eng)
	var guardBlind atomic.Int64
	eng.debugFn = func(f string, a ...any) {
		if strings.Contains(fmt.Sprintf(f, a...), "the guard could not see") {
			guardBlind.Add(1)
		}
	}

	eng.pushUnloadersViaSeam()

	if got := fullsByWindow(t, eng, windows); len(got) != 0 {
		t.Fatalf("table not actable: U1s = %v, want none", got)
	}
	if len(*lines) != 2 {
		t.Fatalf("loader_budget lines = %d, want 2 (one per payload): %v", len(*lines), *lines)
	}
	for i, p := range []string{"PART-A", "PART-B"} {
		l := (*lines)[i]
		if !strings.Contains(l, fmt.Sprintf("payload=%q", p)) || !strings.Contains(l, "occupancy=not_actable") || !strings.Contains(l, "to_fire=0") {
			t.Errorf("line %d = %q, want payload=%q occupancy=not_actable to_fire=0", i, l, p)
		}
	}
	if got := guardBlind.Load(); got != 1 {
		t.Errorf("guard-could-not-see debug lines = %d, want 1 (one per unloader)", got)
	}
}

// TestUnloaderSweep_LockHeldAtDecisionPerLoader: every loader_budget decision
// line is written with that loader's budget mutex held (so count, fire and the
// record are one critical section), and a DIFFERENT loader's mutex is free at
// that moment — the per-loader key the re-entrancy rule relies on
// (TestWithLoaderBudget_EmitDuringReservation_NoDeadlock pins the emit side).
func TestUnloaderSweep_LockHeldAtDecisionPerLoader(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	seedWindowNodes(t, db, "LK-PROC", []string{"LKX-W1", "LKY-W1"})
	seedCoreLoader(t, eng,
		unloaderInfo("LKX", []string{"LKX-W1"}, []string{"PART-A"}),
		unloaderInfo("LKY", []string{"LKY-W1"}, []string{"PART-B"}))
	holdSweep(t, eng, nil)

	var checked, bad atomic.Int64
	eng.logFn = func(f string, a ...any) {
		line := fmt.Sprintf(f, a...)
		if !strings.HasPrefix(line, "loader_budget ") {
			return
		}
		self, other := "loader:LKX", "loader:LKY"
		if strings.Contains(line, "loader=loader:LKY ") {
			self, other = other, self
		}
		checked.Add(1)
		if eng.loaderBudgetLock(self).TryLock() {
			eng.loaderBudgetLock(self).Unlock()
			bad.Add(1)
		}
		if !eng.loaderBudgetLock(other).TryLock() {
			bad.Add(1)
		} else {
			eng.loaderBudgetLock(other).Unlock()
		}
	}

	eng.pushUnloadersViaSeam()

	if checked.Load() != 2 {
		t.Fatalf("loader_budget lines seen = %d, want 2", checked.Load())
	}
	if bad.Load() != 0 {
		t.Fatalf("%d lock checks failed: a decision line was written without its own loader's lock, or with another loader's", bad.Load())
	}
	want := map[string][]string{"LKX-W1": {"PART-A"}, "LKY-W1": {"PART-B"}}
	if got := fullsByWindow(t, eng, []string{"LKX-W1", "LKY-W1"}); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("U1s = %v, want %v", got, want)
	}
}

// TestUnloaderSweep_FirstFreeWindowAndSeesEarlierPayload: three windows, W1
// holding a resident of a payload the loader does not serve. PART-A takes the
// first free window (W2); PART-B then sees PART-A's order and takes W3. The
// second payload's decision accounts for the first payload's fire.
func TestUnloaderSweep_FirstFreeWindowAndSeesEarlierPayload(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	windows := []string{"FF-W1", "FF-W2", "FF-W3"}
	seedWindowNodes(t, db, "FF-PROC", windows)
	seedCoreLoader(t, eng, unloaderInfo("FF", windows, []string{"PART-A", "PART-B"}))
	holdSweep(t, eng, map[string]string{"FF-W1": "PART-Z"})
	lines := captureBudgetLines(eng)

	eng.pushUnloadersViaSeam()

	want := map[string][]string{"FF-W2": {"PART-A"}, "FF-W3": {"PART-B"}}
	if got := fullsByWindow(t, eng, windows); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("U1s = %v, want %v", got, want)
	}
	if len(*lines) != 2 {
		t.Fatalf("loader_budget lines = %d, want 2: %v", len(*lines), *lines)
	}
	if l := (*lines)[1]; !strings.Contains(l, "in_flight_total=2") || !strings.Contains(l, "resident=1") || !strings.Contains(l, "targets=[FF-W3]") {
		t.Errorf("second payload's line %q: want in_flight_total=2 resident=1 targets=[FF-W3]", l)
	}
}

// TestUnloaderSweep_OneBudgetLinePerPayloadThatReachesTheSeam: a payload the
// parked-full guard suppresses never reaches withLoaderBudget and so writes no
// loader_budget line; every other payload writes exactly one.
func TestUnloaderSweep_OneBudgetLinePerPayloadThatReachesTheSeam(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	windows := []string{"OL-W1", "OL-W2", "OL-W3"}
	seedWindowNodes(t, db, "OL-PROC", windows)
	seedCoreLoader(t, eng, unloaderInfo("OL", windows, []string{"PART-A", "PART-B", "PART-C"}))
	holdSweep(t, eng, map[string]string{"OL-W1": "PART-A"})
	lines := captureBudgetLines(eng)

	eng.pushUnloadersViaSeam()

	var payloads []string
	for _, l := range *lines {
		for _, p := range []string{"PART-A", "PART-B", "PART-C"} {
			if strings.Contains(l, fmt.Sprintf("payload=%q", p)) {
				payloads = append(payloads, p)
			}
		}
	}
	if fmt.Sprint(payloads) != "[PART-B PART-C]" {
		t.Fatalf("loader_budget lines by payload = %v, want [PART-B PART-C] (PART-A guard-suppressed, no line)", payloads)
	}
	want := map[string][]string{"OL-W2": {"PART-B"}, "OL-W3": {"PART-C"}}
	if got := fullsByWindow(t, eng, windows); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("U1s = %v, want %v", got, want)
	}
}

// TestUnloaderSweep_SeamMatchesOccupancyByNodeName: W2 is occupied, W1 free;
// the seam attributes the resident to W2 by its row's node and routes the U1
// to W1.
func TestUnloaderSweep_SeamMatchesOccupancyByNodeName(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	windows := []string{"NN-W1", "NN-W2"}
	seedWindowNodes(t, db, "NN-PROC", windows)
	seedCoreLoader(t, eng, unloaderInfo("NN", windows, []string{"PART-A"}))
	holdSweep(t, eng, map[string]string{"NN-W2": "PART-Z"})

	eng.pushUnloadersViaSeam()

	want := map[string][]string{"NN-W1": {"PART-A"}}
	if got := fullsByWindow(t, eng, windows); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("U1s = %v, want %v", got, want)
	}
}

// TestUnloaderSweep_FunnelCountsOnlyItsTargetWindow: a funnelled unloader
// (FunnelWindows) budgets one bin at its FIRST window only. An in-flight U1 and
// a resident bin on its SECOND window are outside that target, so they do not
// count against it and the U1 still fires at the first window. A sweep that
// reads the loader's whole window set once must still restrict the count to
// the payload's ReservationTarget nodes to keep this.
func TestUnloaderSweep_FunnelCountsOnlyItsTargetWindow(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	windows := []string{"FN-W1", "FN-W2"}
	seedWindowNodes(t, db, "FN-PROC", windows)
	info := unloaderInfo("FN", windows, []string{"PART-A"})
	info.FunnelWindows = true
	seedCoreLoader(t, eng, info)
	w2, err := db.GetProcessNodeByCoreNodeName("FN-W2")
	if err != nil {
		t.Fatalf("resolve FN-W2: %v", err)
	}
	w2ID := w2.ID
	if _, err := eng.orderMgr.CreateRetrieveOrder(&w2ID, false, 1, "FN-W2", "FG-SUPER", "",
		"standard", "PART-A", false, true, orders.NoDemand()); err != nil {
		t.Fatalf("seed U1 at FN-W2: %v", err)
	}
	holdSweep(t, eng, map[string]string{"FN-W2": "PART-Z"})

	eng.pushUnloadersViaSeam()

	want := map[string][]string{"FN-W1": {"PART-A"}, "FN-W2": {"PART-A"}}
	if got := fullsByWindow(t, eng, windows); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("U1s = %v, want %v (the new one at FN-W1)", got, want)
	}
}

// TestUnloaderSweep_ReplenishmentDoesNotGateTheDrain: the Edge does not read a
// consume loader's stored replenishment. Core refuses threshold on a consume
// loader (service.ErrConsumeThreshold) and neither plant holds one, so the skip
// the sweep and the re-pull gates once kept for it is gone: a consume loader
// stored as threshold is drained by the sweep and by its own re-pulls exactly as
// an operator one is.
func TestUnloaderSweep_ReplenishmentDoesNotGateTheDrain(t *testing.T) {
	t.Parallel()
	entries := map[string]func(eng *Engine, window string){
		"sweep": func(eng *Engine, _ string) { eng.pushUnloadersViaSeam() },
		"own re-pull": func(eng *Engine, window string) {
			eng.rePushOwnUnloader(mustProcessNode(t, eng, window))
		},
		"own re-pull if uncovered": func(eng *Engine, window string) {
			eng.rePushOwnUnloaderIfUncovered(mustProcessNode(t, eng, window), "")
		},
	}
	for name, run := range entries {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, repl := range []string{protocol.LoaderReplenishmentOperator, protocol.LoaderReplenishmentThreshold} {
				db := testEngineDB(t)
				eng := testEngine(t, db)
				windows := []string{"RG-W1"}
				seedWindowNodes(t, db, "RG-PROC", windows)
				info := unloaderInfo("RG", windows, []string{"PART-A"})
				info.Replenishment = repl
				seedCoreLoader(t, eng, info)
				holdSweep(t, eng, nil)

				run(eng, windows[0])

				want := map[string][]string{"RG-W1": {"PART-A"}}
				if got := fullsByWindow(t, eng, windows); fmt.Sprint(got) != fmt.Sprint(want) {
					t.Errorf("replenishment %s: U1s = %v, want %v", repl, got, want)
				}
			}
		})
	}
}

// TestConsumeVectors_Edge runs the Edge's U1 against shared/loadervectors'
// consume cases, through the release commit's entry
// (MaybeCreateUnloaderFullIn), with the bins table holding each case's
// standing carriers and each inbound full a live U1 row. Core runs the same
// cases through ReplenishLoader's side-cycle origin; this half pins what the
// plant ran before the U1 moved there.
func TestConsumeVectors_Edge(t *testing.T) {
	t.Parallel()
	for _, c := range loadervectors.MustLoad().Consume {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			db := testEngineDB(t)
			eng := testEngine(t, db)
			windows := make([]string, len(c.Windows))
			for i, w := range c.Windows {
				windows[i] = w.Node
			}
			seedWindowNodes(t, db, "CV-PROC", windows)
			info := unloaderInfo("CV", windows, c.Payloads)
			info.FunnelWindows = c.FunnelWindows
			seedCoreLoader(t, eng, info)
			rows := map[string]NodeBinInfo{}
			for i, w := range c.Windows {
				switch {
				case w.StandingFull != "":
					rows[w.Node] = NodeBinInfo{Occupied: true, PayloadCode: w.StandingFull, BinID: int64(100 + i)}
				case w.StandingEmpty:
					rows[w.Node] = NodeBinInfo{Occupied: true, BinID: int64(100 + i)}
				}
				if w.InboundFull != "" {
					id := mustProcessNode(t, eng, w.Node).ID
					if _, err := eng.orderMgr.CreateRetrieveOrder(&id, false, 1, w.Node, "FG-SUPER", "",
						"standard", w.InboundFull, false, true, orders.NoDemand()); err != nil {
						t.Fatalf("seed the full inbound to %s: %v", w.Node, err)
					}
				}
			}
			holdCoreRows(t, eng, rows, windows...)
			before := fullsByWindow(t, eng, windows)

			if c.Burst {
				var wg sync.WaitGroup
				for _, p := range c.Events {
					wg.Add(1)
					go func() {
						defer wg.Done()
						eng.MaybeCreateUnloaderFullIn(p)
					}()
				}
				wg.Wait()
			} else {
				for _, p := range c.Events {
					eng.MaybeCreateUnloaderFullIn(p)
				}
			}

			got := newFullsByWindow(before, fullsByWindow(t, eng, windows))
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

// newFullsByWindow is what after holds beyond before, per window.
func newFullsByWindow(before, after map[string][]string) map[string][]string {
	out := map[string][]string{}
	for w, ps := range after {
		if extra := ps[min(len(before[w]), len(ps)):]; len(extra) > 0 {
			out[w] = extra
		}
	}
	return out
}

// mustProcessNode reads the process node at a core node name.
func mustProcessNode(t *testing.T, eng *Engine, coreNode string) *processes.Node {
	t.Helper()
	n, err := eng.db.GetProcessNodeByCoreNodeName(coreNode)
	if err != nil || n == nil {
		t.Fatalf("process node %s: %v", coreNode, err)
	}
	return n
}
