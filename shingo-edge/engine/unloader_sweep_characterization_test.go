package engine

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/orders"
)

// Characterisation pins for the unloader auto-push sweep (pushUnloadersViaSeam)
// and its P=1 entry (MaybeCreateUnloaderFullIn). They pin what the sweep does
// today, including its Core read count, so a rewrite of the sweep into one pass
// per loader shows up as the predicted diff and nothing else.

// sweepBinsStub serves Core's /api/telemetry/node-bins one row per requested
// node, each carrying node_name, with a resident payload for the nodes in
// resident. It counts every call. mode "status" answers HTTP 500 instead.
// reverse returns the rows in the reverse of the requested order, so a reader
// that matched rows by position instead of by NodeName would misattribute them.
type sweepBinsStub struct {
	srv      *httptest.Server
	hits     atomic.Int64
	mu       sync.Mutex
	requests [][]string
}

func newSweepBinsStub(t *testing.T, resident map[string]string, mode string, reverse bool) *sweepBinsStub {
	t.Helper()
	s := &sweepBinsStub{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/telemetry/node-bins" {
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
			return
		}
		s.hits.Add(1)
		nodes := strings.Split(r.URL.Query().Get("nodes"), ",")
		s.mu.Lock()
		s.requests = append(s.requests, nodes)
		s.mu.Unlock()
		if mode == "status" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		rows := make([]map[string]any, 0, len(nodes))
		for _, n := range nodes {
			row := map[string]any{"node_name": n, "occupied": false}
			if p, ok := resident[n]; ok {
				row["occupied"] = true
				row["payload_code"] = p
			}
			rows = append(rows, row)
		}
		if reverse {
			for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
				rows[i], rows[j] = rows[j], rows[i]
			}
		}
		_ = json.NewEncoder(w).Encode(rows)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// unloaderInfo builds a shared_window consume loader over windows serving
// payloads, operator replenishment (the sweep's only drained mode), pulling
// from FG-SUPER.
func unloaderInfo(name string, windows, payloads []string) protocol.LoaderInfo {
	pos := make([]protocol.LoaderPosition, len(windows))
	for i, w := range windows {
		pos[i] = protocol.LoaderPosition{CoreNodeName: w, Kind: "window"}
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
		ConfigGen:     1,
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
	stub := newSweepBinsStub(t, nil, "", false)
	eng.coreClient = NewCoreClient(stub.srv.URL)

	counter.Reset()
	eng.pushUnloadersViaSeam()

	if got := stub.hits.Load(); got != 0 {
		t.Errorf("no-inbound unloader: node-bins calls = %d, want 0", got)
	}
	if got := counter.Count(); got != 0 {
		t.Errorf("no-inbound unloader: DB statements = %d, want 0", got)
	}

	// Positive control: the same loader WITH an inbound source reads both, so
	// the zeros above are the gate and not a stub or counter that never counts.
	info.InboundSource = "FG-SUPER"
	info.ConfigGen = 2
	seedCoreLoader(t, eng, info)
	counter.Reset()
	eng.pushUnloadersViaSeam()
	if stub.hits.Load() == 0 || counter.Count() == 0 {
		t.Fatalf("control: node-bins calls = %d, DB statements = %d; want both > 0", stub.hits.Load(), counter.Count())
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
		Replenishment: protocol.LoaderReplenishmentOperator, InboundSource: "FG-SUPER", ConfigGen: 1,
		Positions: []protocol.LoaderPosition{{CoreNodeName: "ZP-P1", PayloadCode: "PART-A", Kind: "position"}},
	})
	ls, err := eng.loaderStore.Loaders(domain.RoleConsume)
	if err != nil || len(ls) != 1 {
		t.Fatalf("dedicated consume loader must project: loaders=%d err=%v", len(ls), err)
	}
	if n := len(ls[0].PayloadSet()); n != 0 {
		t.Fatalf("fixture premise: dedicated loader PayloadSet has %d entries, want 0", n)
	}
	stub := newSweepBinsStub(t, nil, "", false)
	eng.coreClient = NewCoreClient(stub.srv.URL)

	counter.Reset()
	eng.pushUnloadersViaSeam()

	if got := stub.hits.Load(); got != 0 {
		t.Errorf("zero-payload unloader: node-bins calls = %d, want 0", got)
	}
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
			eng.coreClient = NewCoreClient(newSweepBinsStub(t, map[string]string{"W1": "PART-A"}, "", false).srv.URL)

			tc.entry(eng)

			got := fullsByWindow(t, eng, tc.windows)
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("U1s by window = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestUnloaderSweep_UnreachableFiresNothing: Core configured but answering 500
// — no U1 for any payload of the loader, one loader_budget line per payload
// (each occupancy=http_error, to_fire=0), and the call count that the per-payload
// shape costs today: P·(W+1) = 2·(2+1) = 6.
func TestUnloaderSweep_UnreachableFiresNothing(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	windows := []string{"UR-W1", "UR-W2"}
	seedWindowNodes(t, db, "UR-PROC", windows)
	seedCoreLoader(t, eng, unloaderInfo("UR", windows, []string{"PART-A", "PART-B"}))
	stub := newSweepBinsStub(t, nil, "status", false)
	eng.coreClient = NewCoreClient(stub.srv.URL)
	lines := captureBudgetLines(eng)
	var guardBlind atomic.Int64
	eng.debugFn = func(f string, a ...any) {
		if strings.Contains(fmt.Sprintf(f, a...), "the guard could not see") {
			guardBlind.Add(1)
		}
	}

	eng.pushUnloadersViaSeam()

	if got := fullsByWindow(t, eng, windows); len(got) != 0 {
		t.Fatalf("unreachable Core: U1s = %v, want none", got)
	}
	if len(*lines) != 2 {
		t.Fatalf("loader_budget lines = %d, want 2 (one per payload): %v", len(*lines), *lines)
	}
	for i, p := range []string{"PART-A", "PART-B"} {
		l := (*lines)[i]
		if !strings.Contains(l, fmt.Sprintf("payload=%q", p)) || !strings.Contains(l, "occupancy=http_error") || !strings.Contains(l, "to_fire=0") {
			t.Errorf("line %d = %q, want payload=%q occupancy=http_error to_fire=0", i, l, p)
		}
	}
	if got := stub.hits.Load(); got != 6 {
		t.Errorf("node-bins calls = %d, want 6 at base (P=2 · (W=2 guard reads + 1 seam read))", got)
	}
	if got := guardBlind.Load(); got != 4 {
		t.Errorf("guard-could-not-see debug lines = %d, want 4 at base (one per window per payload)", got)
	}
}

// TestUnloaderSweep_NotConfiguredStillFires: with no Core configured the sweep
// keeps pulling — first free window per payload, payloads in config order —
// and logs occupancy=not_configured.
func TestUnloaderSweep_NotConfiguredStillFires(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	windows := []string{"NC-W1", "NC-W2"}
	seedWindowNodes(t, db, "NC-PROC", windows)
	seedCoreLoader(t, eng, unloaderInfo("NC", windows, []string{"PART-A", "PART-B"}))
	eng.coreClient = NewCoreClient("")
	lines := captureBudgetLines(eng)

	eng.pushUnloadersViaSeam()

	want := map[string][]string{"NC-W1": {"PART-A"}, "NC-W2": {"PART-B"}}
	if got := fullsByWindow(t, eng, windows); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("not_configured: U1s = %v, want %v", got, want)
	}
	if len(*lines) != 2 {
		t.Fatalf("loader_budget lines = %d, want 2: %v", len(*lines), *lines)
	}
	for _, l := range *lines {
		if !strings.Contains(l, "occupancy=not_configured") || !strings.Contains(l, "created=1") {
			t.Errorf("line %q, want occupancy=not_configured created=1", l)
		}
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
	eng.coreClient = NewCoreClient(newSweepBinsStub(t, nil, "", false).srv.URL)

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
	eng.coreClient = NewCoreClient(newSweepBinsStub(t, map[string]string{"FF-W1": "PART-Z"}, "", false).srv.URL)
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
	eng.coreClient = NewCoreClient(newSweepBinsStub(t, map[string]string{"OL-W1": "PART-A"}, "", false).srv.URL)
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

// TestUnloaderSweep_SeamMatchesOccupancyByNodeName: the stub answers rows in the
// reverse of the requested order. W2 is occupied, W1 free; the seam still
// attributes the resident to W2 by node_name and routes the U1 to W1.
func TestUnloaderSweep_SeamMatchesOccupancyByNodeName(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	windows := []string{"NN-W1", "NN-W2"}
	seedWindowNodes(t, db, "NN-PROC", windows)
	seedCoreLoader(t, eng, unloaderInfo("NN", windows, []string{"PART-A"}))
	eng.coreClient = NewCoreClient(newSweepBinsStub(t, map[string]string{"NN-W2": "PART-Z"}, "", true).srv.URL)

	eng.pushUnloadersViaSeam()

	want := map[string][]string{"NN-W1": {"PART-A"}}
	if got := fullsByWindow(t, eng, windows); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("U1s = %v, want %v", got, want)
	}
}

// TestUnloaderSweep_GuardReadsFirstRowNotNodeName pins a fixture-shape fact at
// base: the parked-full guard asks for one node at a time and reads bins[0]
// without checking its node_name. A Core answer with no node_name (the shape
// fakeCoreBinServer serves) therefore still suppresses the U1 through the guard.
// A guard that matches by NodeName ignores such a row; the seam's own count
// still charges it to the budget under the empty name.
func TestUnloaderSweep_GuardReadsFirstRowNotNodeName(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	windows := []string{"FR-W1", "FR-W2"}
	seedWindowNodes(t, db, "FR-PROC", windows)
	seedCoreLoader(t, eng, unloaderInfo("FR", windows, []string{"PART-A"}))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{"occupied": true, "payload_code": "PART-A"}})
	}))
	t.Cleanup(srv.Close)
	eng.coreClient = NewCoreClient(srv.URL)

	eng.MaybeCreateUnloaderFullIn("PART-A")

	if got := fullsByWindow(t, eng, windows); len(got) != 0 {
		t.Fatalf("nameless occupied row: U1s = %v, want none at base (guard reads bins[0])", got)
	}
}

// TestUnloaderSweep_NodeBinsCallCount pins the sweep's Core read cost at base.
// Per auto consume loader with an inbound source, per payload p: one guard read
// per node of ReservationTarget(p) plus one seam read — Σ P·(W+1) with W the
// target-node count (all windows when spreading, 1 when funnelled). A threshold
// consume loader and a no-inbound loader cost nothing.
//
//	spread loader: P=2, W=2 → 2·3 = 6
//	funnel loader: P=2, W=1 → 2·2 = 4
//	threshold / no-inbound  → 0
//	sweep total             → 10
//
// MaybeCreateUnloaderFullIn on the spread loader is the P=1 case: 1·3 = 3.
func TestUnloaderSweep_NodeBinsCallCount(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	seedWindowNodes(t, db, "CC-PROC", []string{"SP-W1", "SP-W2", "FU-W1", "FU-W2", "TH-W1", "NI-W1"})
	funnel := unloaderInfo("FU", []string{"FU-W1", "FU-W2"}, []string{"PART-C", "PART-D"})
	funnel.FunnelWindows = true
	threshold := unloaderInfo("TH", []string{"TH-W1"}, []string{"PART-E"})
	threshold.Replenishment = protocol.LoaderReplenishmentThreshold
	noInbound := unloaderInfo("NIB", []string{"NI-W1"}, []string{"PART-F"})
	noInbound.InboundSource = ""
	seedCoreLoader(t, eng,
		unloaderInfo("SP", []string{"SP-W1", "SP-W2"}, []string{"PART-A", "PART-B"}),
		funnel, threshold, noInbound)
	stub := newSweepBinsStub(t, nil, "", false)
	eng.coreClient = NewCoreClient(stub.srv.URL)

	eng.pushUnloadersViaSeam()

	if got := stub.hits.Load(); got != 10 {
		t.Errorf("sweep node-bins calls = %d, want 10 at base", got)
	}
	// Every read names nodes of the spread or funnel loader only.
	stub.mu.Lock()
	var seen []string
	for _, req := range stub.requests {
		seen = append(seen, strings.Join(req, ","))
	}
	stub.mu.Unlock()
	sort.Strings(seen)
	want := []string{"FU-W1", "FU-W1", "FU-W1", "FU-W1", "SP-W1", "SP-W1", "SP-W1,SP-W2", "SP-W1,SP-W2", "SP-W2", "SP-W2"}
	if fmt.Sprint(seen) != fmt.Sprint(want) {
		t.Errorf("node-bins requests = %v, want %v", seen, want)
	}

	// P=1 entry on a fresh engine so the sweep's orders don't change the count.
	db2 := testEngineDB(t)
	eng2 := testEngine(t, db2)
	seedWindowNodes(t, db2, "CC2-PROC", []string{"SP-W1", "SP-W2"})
	seedCoreLoader(t, eng2, unloaderInfo("SP", []string{"SP-W1", "SP-W2"}, []string{"PART-A", "PART-B"}))
	stub2 := newSweepBinsStub(t, nil, "", false)
	eng2.coreClient = NewCoreClient(stub2.srv.URL)
	eng2.MaybeCreateUnloaderFullIn("PART-A")
	if got := stub2.hits.Load(); got != 3 {
		t.Errorf("MaybeCreateUnloaderFullIn node-bins calls = %d, want 3 at base (W=2 guard + 1 seam)", got)
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
	eng.coreClient = NewCoreClient(newSweepBinsStub(t, map[string]string{"FN-W2": "PART-Z"}, "", false).srv.URL)

	eng.pushUnloadersViaSeam()

	want := map[string][]string{"FN-W1": {"PART-A"}, "FN-W2": {"PART-A"}}
	if got := fullsByWindow(t, eng, windows); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("U1s = %v, want %v (the new one at FN-W1)", got, want)
	}
}
