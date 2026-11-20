package engine

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/plc"
	"shingoedge/store"
	"shingoedge/store/processes"
)

var updateTickWalkGolden = flag.Bool("update-tick-walk-golden", false,
	"rewrite testdata/tick-walk.golden.txt")

// TestTickWalk_GoldenOverEveryClaimShape pins what the counter walk charges,
// shape by shape, as the bin deltas it emits, the pile drains, every seat's
// count and hold, and the ledger's open rows after each stroke.
//
// THE GOLDEN MUST NOT MOVE when bins count pieces (the claim's pieces per
// cycle, the consume draw) or when produce spots of one part share a step.
// Every fixture here runs a part whose pieces per cycle is 1 (no catalog
// manifest, one manifest line at 1, or a kit, which keeps today's arithmetic)
// and no two produce spots of one style run one part on one counter. Those
// are the cells that count exactly as they did, so a diff here is a defect in
// that work, not a judgement call: regenerate only for a named change to the
// walk itself.
//
// Rendered twice over two fresh databases and asserted equal first, so a
// time, a map order or an id that leaks into the text fails as itself.
func TestTickWalk_GoldenOverEveryClaimShape(t *testing.T) {
	t.Parallel()
	got := renderTickWalk(t)
	if again := renderTickWalk(t); again != got {
		t.Fatalf("the tick walk's rendering is not deterministic:\n--- first\n%s\n--- second\n%s", got, again)
	}
	path := filepath.Join("testdata", "tick-walk.golden.txt")
	if *updateTickWalkGolden {
		testutil.MustNoErr(t, os.WriteFile(path, []byte(got), 0o644), "write golden")
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no %s (%v); regenerate with go test ./engine -run TestTickWalk_GoldenOverEveryClaimShape -update-tick-walk-golden", path, err)
	}
	if !bytes.Equal(bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n")), []byte(got)) {
		t.Errorf("%s moved: a stroke charged a seat, a pile or the ledger differently.\n--- got\n%s", path, got)
	}
}

// golden is one database's walk: the engine, a recording sink, and the
// scenarios' nodes by id for the rendering.
type golden struct {
	t     *testing.T
	db    *store.DB
	eng   *Engine
	sink  *fakeDeltaSink
	names map[int64]string
	out   strings.Builder
	// seen is how many of the sink's calls the last step already printed.
	seenBins, seenPiles int
}

// gwSpot is one position of a scenario: its claim under the scenario's
// style, and its seat.
type gwSpot struct {
	name, part, paired string
	role               protocol.ClaimRole
	mode               protocol.SwapMode
	capacity           int
	bin                int64 // 0: no cart bound
	count              int
	dark               bool // a parked A/B side
}

// gwCell is one seeded process: its id, its style and its positions in walk
// order.
type gwCell struct {
	pid, style int64
	nodes      map[string]int64
	claims     map[string]int64
	order      []string
}

func renderTickWalk(t *testing.T) string {
	t.Helper()
	db := testEngineDB(t)
	g := &golden{t: t, db: db, eng: testEngine(t, db), names: map[int64]string{}}
	g.sink = &fakeDeltaSink{db: db}
	g.eng.SetInventoryDeltaSink(g.sink)

	g.loneProduce()
	g.strictProduce()
	g.lhrh()
	g.abPair()
	g.pressIndex()
	g.consumePile()
	g.consumeKit()
	g.stationCounter()
	g.heldAndOffPlan()
	return g.out.String()
}

// cell seeds a process and one style, active, with spots in sequence order.
func (g *golden) cell(prefix string, spots ...gwSpot) *gwCell {
	g.t.Helper()
	pid, err := g.db.CreateProcess(prefix+"-PROC", prefix, "", "", false)
	testutil.MustNoErr(g.t, err, "process "+prefix)
	style, err := g.db.CreateStyle(prefix+"-STYLE", "", pid)
	testutil.MustNoErr(g.t, err, "style "+prefix)
	testutil.MustNoErr(g.t, g.db.SetActiveStyle(pid, &style), "active "+prefix)
	c := &gwCell{pid: pid, style: style, nodes: map[string]int64{}, claims: map[string]int64{}}
	for i, s := range spots {
		nodeID, err := g.db.CreateProcessNode(processes.NodeInput{ProcessID: pid, CoreNodeName: s.name, Code: s.name,
			Name: s.name, Sequence: i + 1, Enabled: true})
		testutil.MustNoErr(g.t, err, "node "+s.name)
		c.nodes[s.name], c.order = nodeID, append(c.order, s.name)
		g.names[nodeID] = s.name
		c.claims[s.name] = g.claim(style, s)
		_, err = g.db.EnsureProcessNodeRuntime(nodeID)
		testutil.MustNoErr(g.t, err, "runtime "+s.name)
		claimID := c.claims[s.name]
		var bin *int64
		if s.bin != 0 {
			bin = binPtr(s.bin)
		}
		testutil.MustNoErr(g.t, g.db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, bin, s.count), "seat "+s.name)
		testutil.MustNoErr(g.t, g.db.SetActivePull(nodeID, !s.dark), "pull "+s.name)
	}
	return c
}

// claim upserts one spot's claim under style, with the routing its mode's
// validator asks for. Names are synthetic.
func (g *golden) claim(style int64, s gwSpot) int64 {
	g.t.Helper()
	in := processes.NodeClaimInput{StyleID: style, CoreNodeName: s.name, Role: s.role, SwapMode: s.mode,
		PayloadCode: s.part, UOPCapacity: s.capacity, InboundSource: "SYN_MARKET", OutboundDestination: "SYN_MARKET",
		PairedCoreNode: s.paired}
	switch s.mode {
	case protocol.SwapModeSingleRobot:
		in.InboundStaging, in.OutboundStaging = s.name+"_STG_IN", s.name+"_STG_OUT"
	case protocol.SwapModeTwoRobotPressIndex:
		in.InboundStaging, in.OutboundStaging = "IN_"+s.name, "OUT_"+s.name
	}
	id, err := upsertClaimRetiredMode(g.db, in)
	testutil.MustNoErr(g.t, err, "claim "+s.name)
	return id
}

// manifest gives a part's catalog row its manifest lines.
func (g *golden) manifest(part string, lines ...protocol.ManifestItem) {
	g.t.Helper()
	b, err := json.Marshal(lines)
	testutil.MustNoErr(g.t, err, "manifest "+part)
	res, err := g.db.DB.Exec(`UPDATE payload_catalog SET manifest=? WHERE code=?`, string(b), part)
	testutil.MustNoErr(g.t, err, "manifest "+part)
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		g.t.Fatalf("manifest %s: %d catalog rows (%v), want 1", part, n, err)
	}
}

// tick strokes the process's production counter and prints what moved.
func (g *golden) tick(c *gwCell, label string, ev CounterDeltaEvent) {
	g.t.Helper()
	ev.ProcessID = c.pid
	if ev.StyleID == 0 {
		ev.StyleID = c.style
	}
	g.eng.handleCounterDelta(ev)
	g.print(c, label)
}

// print renders one step: the sink's new bin deltas and pile drains, every
// seat of the cell in walk order, and the ledger's open rows.
func (g *golden) print(c *gwCell, label string) {
	g.t.Helper()
	fmt.Fprintf(&g.out, "-- %s\n", label)
	g.sink.mu.Lock()
	bins, piles := g.sink.binCalls[g.seenBins:], g.sink.bucketCalls[g.seenPiles:]
	g.seenBins, g.seenPiles = len(g.sink.binCalls), len(g.sink.bucketCalls)
	for _, b := range bins {
		fmt.Fprintf(&g.out, "   bin %d %s %+d %s epoch=%d\n", b.BinID, b.PayloadCode, b.Delta, b.Reason, b.Epoch)
	}
	for _, p := range piles {
		fmt.Fprintf(&g.out, "   pile %s %s %s drained=%d\n", g.names[p.NodeID], p.PayloadCode, p.State, p.Drained)
	}
	g.sink.mu.Unlock()
	for _, name := range c.order {
		rt, err := g.db.GetProcessNodeRuntime(c.nodes[name])
		testutil.MustNoErr(g.t, err, "seat "+name)
		bin := "-"
		if rt.ActiveBinID != nil {
			bin = fmt.Sprint(*rt.ActiveBinID)
		}
		fmt.Fprintf(&g.out, "   seat %s bin=%s count=%d hold=%d holdPart=%q\n", name, bin, rt.RemainingUOPCached,
			rt.PendingUOPDelta, rt.PendingUOPPayload)
	}
	holds, err := g.db.ListOpenCatidHolds(c.pid)
	testutil.MustNoErr(g.t, err, "ledger")
	for _, h := range holds {
		at := "cell"
		if h.ProcessNodeID != 0 {
			at = g.names[h.ProcessNodeID]
		}
		fmt.Fprintf(&g.out, "   ledger %s catid=%q part=%q %s qty=%d strokes=%d\n", at, h.CATID, h.PayloadCode,
			h.Reason, h.Qty, h.Strokes)
	}
}

func (g *golden) section(title string) { fmt.Fprintf(&g.out, "\n== %s\n", title) }

// loneProduce: one produce spot, a bound bin, counting on past its pack.
func (g *golden) loneProduce() {
	g.section("lone produce")
	c := g.cell("GLP", gwSpot{name: "GLP_1", part: "SYN-LONE", role: protocol.ClaimRoleProduce,
		mode: protocol.SwapModeSingleRobot, capacity: 12, bin: 101, count: 10})
	g.tick(c, "stroke 1", CounterDeltaEvent{Delta: 1})
	g.tick(c, "stroke 3 (past the pack, no strict pack)", CounterDeltaEvent{Delta: 3})
}

// strictProduce: one produce spot under strict pack: the bin stops at the
// pack and the rest is held for the next bin.
func (g *golden) strictProduce() {
	g.section("lone produce, strict pack")
	c := g.cell("GSP", gwSpot{name: "GSP_1", part: "SYN-STRICT", role: protocol.ClaimRoleProduce,
		mode: protocol.SwapModeSingleRobot, capacity: 12, bin: 111, count: 10})
	_, err := g.db.DB.Exec(`UPDATE style_node_claims SET strict_pack=1 WHERE id=?`, c.claims["GSP_1"])
	testutil.MustNoErr(g.t, err, "strict pack")
	g.tick(c, "stroke 5 (2 fit, 3 held)", CounterDeltaEvent{Delta: 5})
}

// lhrh: an LH/RH die, two produce spots of two parts: each takes the whole
// step.
func (g *golden) lhrh() {
	g.section("LH/RH, two produce spots of two parts")
	c := g.cell("GLR",
		gwSpot{name: "GLR_LH", part: "SYN-LH", role: protocol.ClaimRoleProduce, mode: protocol.SwapModeSingleRobot,
			capacity: 100, bin: 121, count: 0},
		gwSpot{name: "GLR_RH", part: "SYN-RH", role: protocol.ClaimRoleProduce, mode: protocol.SwapModeSingleRobot,
			capacity: 100, bin: 122, count: 0})
	g.tick(c, "stroke 1", CounterDeltaEvent{Delta: 1})
	g.tick(c, "stroke 2", CounterDeltaEvent{Delta: 2})
}

// abPair: a sequential consume pair, one side lit: the lit side takes the
// stroke. Both dark: the first in node order is the fall-through.
func (g *golden) abPair() {
	g.section("A/B consume pair")
	c := g.cell("GAB",
		gwSpot{name: "GAB_A", part: "SYN-AB", paired: "GAB_B", role: protocol.ClaimRoleConsume,
			mode: protocol.SwapModeSequential, capacity: 50, bin: 131, count: 50},
		gwSpot{name: "GAB_B", part: "SYN-AB", paired: "GAB_A", role: protocol.ClaimRoleConsume,
			mode: protocol.SwapModeSequential, capacity: 50, bin: 132, count: 50, dark: true})
	g.tick(c, "stroke 4, A lit", CounterDeltaEvent{Delta: 4})
	testutil.MustNoErr(g.t, g.db.SetActivePull(c.nodes["GAB_A"], false), "A dark")
	g.tick(c, "stroke 3, both dark (fall-through)", CounterDeltaEvent{Delta: 3})
}

// pressIndex: a press-index front names its back position and is never a
// pair: it takes the stroke beside a sequential pair whose sides are dark.
func (g *golden) pressIndex() {
	g.section("press-index front beside a dark pair")
	c := g.cell("GPI",
		gwSpot{name: "GPI_F", part: "SYN-PI", paired: "GPI_BACK", role: protocol.ClaimRoleProduce,
			mode: protocol.SwapModeTwoRobotPressIndex, capacity: 40, bin: 141, count: 5},
		gwSpot{name: "GPI_QA", part: "SYN-PIQ", paired: "GPI_QB", role: protocol.ClaimRoleConsume,
			mode: protocol.SwapModeSequential, capacity: 40, bin: 142, count: 40, dark: true},
		gwSpot{name: "GPI_QB", part: "SYN-PIQ", paired: "GPI_QA", role: protocol.ClaimRoleConsume,
			mode: protocol.SwapModeSequential, capacity: 40, bin: 143, count: 40, dark: true})
	g.tick(c, "stroke 2", CounterDeltaEvent{Delta: 2})
}

// consumePile: a consume spot with a lineside pile, its part's catalog row
// one manifest line at pieces per cycle 1: the pile drains first, the rest
// is the cart's. Then a spot with no cart: the stroke is logged.
func (g *golden) consumePile() {
	g.section("consume with a lineside pile; consume with no cart")
	c := g.cell("GCP",
		gwSpot{name: "GCP_1", part: "SYN-PILE", role: protocol.ClaimRoleConsume, mode: protocol.SwapModeSingleRobot,
			capacity: 40, bin: 151, count: 40},
		gwSpot{name: "GCP_NOCART", part: "SYN-PILE2", role: protocol.ClaimRoleConsume, mode: protocol.SwapModeSingleRobot,
			capacity: 40, count: 0})
	g.manifest("SYN-PILE", protocol.ManifestItem{PartNumber: "SYN-PILE", PartsPerCycle: 1})
	_, err := g.db.CaptureLinesideBucket(c.nodes["GCP_1"], "SYN-PILE", 7)
	testutil.MustNoErr(g.t, err, "pile")
	g.tick(c, "stroke 10 (7 off the pile, 3 off the cart)", CounterDeltaEvent{Delta: 10, CATID: "9100"})
	g.tick(c, "stroke 5", CounterDeltaEvent{Delta: 5, CATID: "9100"})
}

// consumeKit: a consume spot whose part is a kit, two manifest lines at 2
// and 3: a kit keeps today's arithmetic, one per stroke.
func (g *golden) consumeKit() {
	g.section("consume of a kit (two manifest lines)")
	c := g.cell("GKT", gwSpot{name: "GKT_1", part: "SYN-KIT", role: protocol.ClaimRoleConsume,
		mode: protocol.SwapModeSingleRobot, capacity: 30, bin: 161, count: 30})
	g.manifest("SYN-KIT", protocol.ManifestItem{PartNumber: "SYN-KIT-L", PartsPerCycle: 2},
		protocol.ManifestItem{PartNumber: "SYN-KIT-R", PartsPerCycle: 3})
	g.tick(c, "stroke 4", CounterDeltaEvent{Delta: 4})
}

// stationCounter: two consume spots counted at a station counter, one at the
// cell counter. The cell's stroke leaves the bound spots to their counter;
// the station's charges each of them.
func (g *golden) stationCounter() {
	g.section("station counter")
	c := g.cell("GST",
		gwSpot{name: "GST_1", part: "SYN-ST1", role: protocol.ClaimRoleConsume, mode: protocol.SwapModeSingleRobot,
			capacity: 40, bin: 171, count: 40},
		gwSpot{name: "GST_2", part: "SYN-ST2", role: protocol.ClaimRoleConsume, mode: protocol.SwapModeSingleRobot,
			capacity: 40, bin: 172, count: 40},
		gwSpot{name: "GST_CELL", part: "SYN-ST1", role: protocol.ClaimRoleConsume, mode: protocol.SwapModeSingleRobot,
			capacity: 40, bin: 173, count: 40})
	lc, err := g.db.CreateLineCounter(c.pid, "load station", "plc", "MES_Cell.Station_Counter_01", "")
	testutil.MustNoErr(g.t, err, "station counter")
	for _, n := range []string{"GST_1", "GST_2"} {
		testutil.MustNoErr(g.t, g.db.SetNodeLineCounter(c.nodes[n], &lc), "bind "+n)
	}
	g.tick(c, "cell stroke 2", CounterDeltaEvent{Delta: 2})
	g.eng.handleLineCounterDelta(LineCounterDeltaEvent{ProcessID: c.pid, CounterID: lc, StyleID: c.style,
		Kind: plc.TickToday, Delta: 3})
	g.print(c, "station stroke 3")
}

// heldAndOffPlan: two styles on one cell, carts recorded as A's part and as
// B's. A held stroke draws no cart; an off-plan stroke of B charges B's cart
// and holds the share at A's; a today's stroke of A charges A's cart and logs
// the share at B's; the held draw's release charges A's cart and holds the
// share at B's again.
func (g *golden) heldAndOffPlan() {
	g.section("held, off-plan, today's and a release")
	c := g.cell("GHO",
		gwSpot{name: "GHO_1", part: "SYN-HO-A", role: protocol.ClaimRoleConsume, mode: protocol.SwapModeSingleRobot,
			capacity: 40, bin: 181, count: 40},
		gwSpot{name: "GHO_2", part: "SYN-HO-A", role: protocol.ClaimRoleConsume, mode: protocol.SwapModeSingleRobot,
			capacity: 40, bin: 182, count: 40})
	styleB, err := g.db.CreateStyle("GHO-STYLE-B", "", c.pid)
	testutil.MustNoErr(g.t, err, "style B")
	for _, n := range c.order {
		g.claim(styleB, gwSpot{name: n, part: "SYN-HO-B", role: protocol.ClaimRoleConsume,
			mode: protocol.SwapModeSingleRobot, capacity: 40})
	}
	testutil.MustNoErr(g.t, g.db.SetProcessNodeRuntimeLinesidePayload(c.nodes["GHO_1"], "SYN-HO-A", true, "test"), "cart A")
	testutil.MustNoErr(g.t, g.db.SetProcessNodeRuntimeLinesidePayload(c.nodes["GHO_2"], "SYN-HO-B", true, "test"), "cart B")

	g.tick(c, "held stroke 3", CounterDeltaEvent{Delta: 3, CATID: "9001", Kind: plc.TickHeld})
	g.tick(c, "off-plan stroke 2 of B", CounterDeltaEvent{StyleID: styleB, Delta: 2, CATID: "9002", Kind: plc.TickOffPlan})
	g.tick(c, "today's stroke 1 of A", CounterDeltaEvent{Delta: 1, CATID: "9003", Kind: plc.TickToday})

	holds, err := g.db.ListOpenCatidHolds(c.pid)
	testutil.MustNoErr(g.t, err, "ledger")
	for i := range holds {
		if holds[i].ProcessNodeID == 0 && holds[i].Reason == holdReasonHeld {
			g.eng.chargeHold(&holds[i], c.style)
			g.print(c, "the held draw released under A")
			return
		}
	}
	g.t.Fatalf("no held draw on the cell to release: %+v", holds)
}
