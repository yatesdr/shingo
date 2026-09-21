package engine

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// node_walkers_decision_test.go — what the three per-node walkers DECIDE.
//
// Three loops walk a process's nodes once a period or once a tick: the level
// sweep (demand_reconciler.go), the parked-ticks monitor
// (uop_stranded_monitor.go) and the counter tick (wiring_counter_delta.go).
// Each of them reads the claim and the runtime row one node at a time, and the
// batching work that follows replaces those per-node reads with set-returning
// ones.
//
// A BATCHED READ IS ONLY THE SAME READ IF THE DECISIONS COME OUT THE SAME, and
// the decisions are the thing nobody had written down. Each walker's skip
// ladder is a list of reasons in a fixed order — no claim, wrong role, a
// loader, a parked side, a style that does not match — and every one of them is
// a plant behaviour somebody depends on. These tests name the whole ladder per
// walker and assert the resulting set of nodes: which were acted on, which were
// passed over, and what was stamped. They are written to pass BEFORE the
// batching and to keep passing after it, so a diff in them is a defect in the
// batching and not a judgement call.
//
// The fixtures are synthetic: node names are the plant's own naming shape
// (ALN_nnn, PLN_nnn) but no part number, tag or host from any plant appears.

// walkerNode describes one node of a seeded process in the terms the walkers
// actually branch on. Everything here is a branch somebody's loop tests.
type walkerNode struct {
	// Suffix names the node; the seeded core node name is prefix + "_" + Suffix.
	Suffix string
	// NoClaim seeds the node with no style_node_claims row at all, which is the
	// first rung of every walker's ladder.
	NoClaim bool
	// NoRuntime skips the runtime row, which is the OTHER absence — a node the
	// claim knows about that has never been ensured.
	NoRuntime bool

	Role         protocol.ClaimRole
	SwapMode     protocol.SwapMode
	PayloadCode  string
	Capacity     int
	ReorderPoint int
	AutoReorder  bool

	// Paired + ActivePull make an A/B side. A parked side is Paired != "" with
	// ActivePull false — the state the tick skips and the level sweep does not.
	Paired     string
	ActivePull bool

	Remaining int
	BoundBin  *int64
	Pending   int
}

// walkerFixture is one seeded process and the ids the assertions need.
type walkerFixture struct {
	ProcessID int64
	StyleID   int64
	Prefix    string
	NodeIDs   map[string]int64
	ClaimIDs  map[string]int64
	Names     map[string]string
}

func (f *walkerFixture) node(t *testing.T, suffix string) int64 {
	t.Helper()
	id, ok := f.NodeIDs[suffix]
	if !ok {
		t.Fatalf("fixture has no node %q", suffix)
	}
	return id
}

// seedWalkerProcess creates one process, one style and one node per spec.
//
// ONE STYLE, because all three walkers resolve the claim through the process's
// ACTIVE style (store.ActiveStyleFirst) and a second style would be testing the
// changeover fallback rather than the walk. The changeover fallback has its own
// tests (TestFindActiveClaim_PostCutover).
func seedWalkerProcess(t *testing.T, db *store.DB, prefix string, specs []walkerNode) *walkerFixture {
	t.Helper()
	procID, err := db.CreateProcess(prefix+"-PROC", prefix+" walkers", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	styleID, err := db.CreateStyle(prefix+"-STYLE", "", procID)
	testutil.MustNoErr(t, err, "create style")
	testutil.MustNoErr(t, db.SetActiveStyle(procID, &styleID), "set active style")

	f := &walkerFixture{
		ProcessID: procID, StyleID: styleID, Prefix: prefix,
		NodeIDs:  map[string]int64{},
		ClaimIDs: map[string]int64{},
		Names:    map[string]string{},
	}
	for i, spec := range specs {
		name := prefix + "_" + spec.Suffix
		nodeID, err := db.CreateProcessNode(processes.NodeInput{
			ProcessID: procID, CoreNodeName: name, Code: fmt.Sprintf("N%02d", i+1),
			Name: name, Sequence: i + 1, Enabled: true,
		})
		testutil.MustNoErr(t, err, "create node "+name)
		f.NodeIDs[spec.Suffix] = nodeID
		f.Names[spec.Suffix] = name

		var claimID int64
		if !spec.NoClaim {
			in := processes.NodeClaimInput{
				StyleID: styleID, CoreNodeName: name, Role: spec.Role,
				SwapMode: spec.SwapMode, PayloadCode: spec.PayloadCode,
				UOPCapacity: spec.Capacity, ReorderPoint: spec.ReorderPoint,
				AutoReorder: domain.Ptr(spec.AutoReorder),
				// The routing each mode's validator requires. Named
				// synthetically; no plant node is referenced.
				InboundSource: "SYN_MARKET", OutboundDestination: "SYN_MARKET",
				PairedCoreNode: spec.Paired,
			}
			// Sequential A/B is the mode that PAIRS, and its validator refuses
			// the staging fields single_robot requires. Routing is per mode, so
			// the seed follows the mode rather than setting every field.
			if spec.SwapMode == protocol.SwapModeSingleRobot {
				in.InboundStaging = prefix + "_STG_IN"
				in.OutboundStaging = prefix + "_STG_OUT"
			}
			claimID, err = upsertClaimRetiredMode(db, in)
			testutil.MustNoErr(t, err, "upsert claim "+name)
			f.ClaimIDs[spec.Suffix] = claimID
		}
		if spec.NoRuntime {
			continue
		}
		_, err = db.EnsureProcessNodeRuntime(nodeID)
		testutil.MustNoErr(t, err, "ensure runtime "+name)
		var claimRef *int64
		if claimID != 0 {
			claimRef = &claimID
		}
		testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(nodeID, claimRef, spec.BoundBin, spec.Remaining),
			"seed runtime "+name)
		// ALWAYS WRITTEN, never left to the default. active_pull is
		// `INTEGER NOT NULL DEFAULT 1`, so a runtime row nobody has flipped
		// reads as the ACTIVE side of an A/B pair. A fixture that only wrote
		// the true case would seed every "parked" node as active and the tick
		// tests would pass for the wrong reason.
		testutil.MustNoErr(t, db.SetActivePull(nodeID, spec.ActivePull), "set active pull "+name)
		if spec.Pending != 0 {
			testutil.MustNoErr(t, db.AddPendingUOPDelta(nodeID, spec.Pending), "seed pending "+name)
		}
	}
	return f
}

// askedNodes returns the suffixes of the nodes an order was minted for, sorted.
// The sweep stamps process_node_id itself at create time, which is why it is
// also the sweep's own dedup key.
func askedNodes(t *testing.T, db *store.DB, f *walkerFixture) []string {
	t.Helper()
	rows, err := db.DB.Query(`SELECT process_node_id FROM orders WHERE process_node_id IS NOT NULL`)
	testutil.MustNoErr(t, err, "read asked nodes")
	defer rows.Close()
	bySuffix := map[int64]string{}
	for suffix, id := range f.NodeIDs {
		bySuffix[id] = suffix
	}
	var out []string
	for rows.Next() {
		var id int64
		testutil.MustNoErr(t, rows.Scan(&id), "scan process_node_id")
		out = append(out, bySuffix[id])
	}
	testutil.MustNoErr(t, rows.Err(), "iterate asked nodes")
	sort.Strings(out)
	return out
}

// stampedClaims returns the suffixes whose claim carries a below_reorder_since,
// sorted. The column records the falling edge and is what the close pass reads.
func stampedClaims(t *testing.T, db *store.DB, f *walkerFixture) []string {
	t.Helper()
	var out []string
	for suffix, claimID := range f.ClaimIDs {
		stamped, err := db.GetClaimBelowReorderSince(claimID)
		testutil.MustNoErr(t, err, "read below_reorder_since")
		if stamped != nil {
			out = append(out, suffix)
		}
	}
	sort.Strings(out)
	return out
}

func assertSet(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s = [%s], want [%s]", what, strings.Join(got, ","), strings.Join(want, ","))
	}
}

// ── WALKER 1: THE LEVEL SWEEP ─────────────────────────────────────────────

// levelSweepLadder is every rung of sweepNodeLevel's decision, on one process.
//
// Read it as the specification it is:
//
//	NOCLAIM    no style_node_claims row       — walked, skipped, nothing stamped
//	NORUNTIME  claim but no runtime row       — walked, skipped, nothing stamped
//	LOADER     manual_swap                    — skipped BEFORE the evaluators, so
//	                                            not even the falling edge is
//	                                            recorded; loader replenishment is
//	                                            Core-owned
//	OPTOUT     consume, reorder_point = 0     — the documented opt-out; returns
//	                                            before the evaluator, so no stamp
//	NOCAP      produce, capacity 0            — the produce mirror of the opt-out
//	PARKED     consume, paired, ActivePull    — NOT SKIPPED. The tick skips a
//	           false                            parked side because it is not
//	                                            consuming; the level does not care,
//	                                            and this is the cell the removed
//	                                            A/B flip tail existed to reorder for
//	ARMEDOFF   consume, auto_reorder off      — STAMPS but never asks. The flag
//	                                            governs whether we ACT, not whether
//	                                            it happened
//	ASK        consume, armed, below          — stamps and asks
//	FULL       consume, armed, above          — neither
//	PRODFULL   produce, armed, at capacity    — stamps and asks (the evacuate side)
func levelSweepLadder(prefix string) []walkerNode {
	bin := int64(1)
	return []walkerNode{
		{Suffix: "NOCLAIM", NoClaim: true, Remaining: 0},
		{Suffix: "NORUNTIME", NoRuntime: true, Role: protocol.ClaimRoleConsume,
			SwapMode: protocol.SwapModeSingleRobot, PayloadCode: "SYN-PART-A",
			Capacity: 40, ReorderPoint: 25, AutoReorder: true},
		{Suffix: "LOADER", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeManualSwap,
			PayloadCode: "SYN-PART-A", Capacity: 40, ReorderPoint: 25, AutoReorder: true, Remaining: 0},
		{Suffix: "OPTOUT", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeSingleRobot,
			PayloadCode: "SYN-PART-A", Capacity: 40, ReorderPoint: 0, AutoReorder: true, Remaining: 0},
		{Suffix: "NOCAP", Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeSingleRobot,
			PayloadCode: "SYN-PART-NOCAT", Capacity: 0, ReorderPoint: 0, AutoReorder: true, Remaining: 999},
		{Suffix: "PARKED", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeSequential,
			PayloadCode: "SYN-PART-A", Capacity: 40, ReorderPoint: 25, AutoReorder: true,
			Paired: prefix + "_ASK", ActivePull: false, Remaining: 0},
		{Suffix: "ARMEDOFF", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeSingleRobot,
			PayloadCode: "SYN-PART-A", Capacity: 40, ReorderPoint: 25, AutoReorder: false, Remaining: 0},
		{Suffix: "ASK", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeSingleRobot,
			PayloadCode: "SYN-PART-A", Capacity: 40, ReorderPoint: 25, AutoReorder: true, Remaining: 0},
		{Suffix: "FULL", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeSingleRobot,
			PayloadCode: "SYN-PART-A", Capacity: 40, ReorderPoint: 25, AutoReorder: true,
			Remaining: 40, BoundBin: &bin},
		{Suffix: "PRODFULL", Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeSingleRobot,
			PayloadCode: "SYN-PART-B", Capacity: 30, ReorderPoint: 0, AutoReorder: true, Remaining: 30},
	}
}

// TestLevelSweep_DecisionLadder pins the whole set in one pass: which nodes the
// sweep asked for, and which claims it stamped a falling edge on.
//
// The two sets are deliberately different, and that difference IS the
// behaviour: ARMEDOFF stamps and does not ask, LOADER and OPTOUT neither.
func TestLevelSweep_DecisionLadder(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	f := seedWalkerProcess(t, db, "LADDER", levelSweepLadder("LADDER"))
	eng := testEngine(t, db)
	eng.coreClient = NewCoreClient(headOccupancyStub(t, false).URL)

	proc, err := db.GetProcess(f.ProcessID)
	testutil.MustNoErr(t, err, "get process")
	eng.sweepProcessLevels(proc)

	assertSet(t, "nodes asked for", askedNodes(t, db, f),
		[]string{"ASK", "PARKED", "PRODFULL"})
	assertSet(t, "claims stamped with a falling edge", stampedClaims(t, db, f),
		[]string{"ARMEDOFF", "ASK", "PARKED", "PRODFULL"})

	// And it is stable: a second pass over the same state asks for nothing more,
	// because every ask it made is still in flight at its own node.
	eng.sweepProcessLevels(proc)
	assertSet(t, "nodes asked for after a second pass", askedNodes(t, db, f),
		[]string{"ASK", "PARKED", "PRODFULL"})
}

// TestLevelSweep_ParkedSideIsNotSkipped isolates the one rung whose comment
// carries the most weight: demand_reconciler.go says a parked A/B side is asked
// for on the same terms as any other node, and that sentence is what made the
// old flip tail removable.
//
// It is stated separately from the ladder because it is the rung a batching
// change is most likely to break by accident — every other walker in this file
// DOES skip a parked side, so a shared helper that "skips parked nodes" would
// look right and silently stop reordering for a drained parked cell.
func TestLevelSweep_ParkedSideIsNotSkipped(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	f := seedWalkerProcess(t, db, "PARK", []walkerNode{
		{Suffix: "A", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeSequential,
			PayloadCode: "SYN-PART-A", Capacity: 40, ReorderPoint: 25, AutoReorder: true,
			Paired: "PARK_B", ActivePull: true, Remaining: 40},
		{Suffix: "B", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeSequential,
			PayloadCode: "SYN-PART-A", Capacity: 40, ReorderPoint: 25, AutoReorder: true,
			Paired: "PARK_A", ActivePull: false, Remaining: 0},
	})
	eng := testEngine(t, db)
	eng.coreClient = NewCoreClient(headOccupancyStub(t, false).URL)

	proc, err := db.GetProcess(f.ProcessID)
	testutil.MustNoErr(t, err, "get process")
	eng.sweepProcessLevels(proc)

	assertSet(t, "nodes asked for", askedNodes(t, db, f), []string{"B"})
}

// ── WALKER 2: THE PARKED-TICKS MONITOR ────────────────────────────────────

// strandedLadder is the stranded monitor's own decision set, which is NOT the
// level sweep's:
//
//	NOCLAIM    no claim                   — cleared, state dropped
//	PRODUCE    produce role               — cleared; only consume cells count
//	                                        parts down against a bound bin
//	LOADER     manual_swap                — cleared
//	BOUND      consume, a bin bound       — cleared by the growth rule itself
//	FLAT       consume, unbound, pending  — never escalates; an idle line is not
//	           not growing                  an alarm
//	CLIMB      consume, unbound, pending  — fires once per window
//	           rising every scan
//	PARKED     consume, unbound, climbing — FIRES. The monitor does not read
//	           on a parked A/B side         active_pull at all, so a parked side
//	                                        alarms on the same terms as any other
func TestStrandedMonitor_DecisionLadder(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	bound := int64(7)
	f := seedWalkerProcess(t, db, "STRAND", []walkerNode{
		{Suffix: "NOCLAIM", NoClaim: true},
		{Suffix: "PRODUCE", Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeSingleRobot,
			PayloadCode: "SYN-PART-B", Capacity: 30},
		{Suffix: "LOADER", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeManualSwap,
			PayloadCode: "SYN-PART-A", Capacity: 40},
		{Suffix: "BOUND", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeSingleRobot,
			PayloadCode: "SYN-PART-A", Capacity: 40, BoundBin: &bound},
		{Suffix: "FLAT", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeSingleRobot,
			PayloadCode: "SYN-PART-A", Capacity: 40},
		{Suffix: "CLIMB", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeSingleRobot,
			PayloadCode: "SYN-PART-A", Capacity: 40},
		{Suffix: "PARKED", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeSequential,
			PayloadCode: "SYN-PART-A", Capacity: 40, Paired: "STRAND_CLIMB", ActivePull: false},
	})
	eng := testEngine(t, db)
	var fired []string
	eng.Events.SubscribeTypes(func(evt Event) {
		if a, ok := evt.Payload.(UOPStrandedEvent); ok {
			fired = append(fired, a.CoreNodeName)
		}
	}, EventUOPStranded)

	// FLAT holds a constant non-zero pending; BOUND, CLIMB and PARKED climb.
	testutil.MustNoErr(t, db.AddPendingUOPDelta(f.node(t, "FLAT"), 5), "seed flat pending")
	sm := &strandedMonitor{eng: eng, states: map[int64]*strandedNodeState{}}
	base := time.Now()
	for i := 0; i < strandedWindow+2; i++ {
		for _, s := range []string{"BOUND", "CLIMB", "PARKED"} {
			testutil.MustNoErr(t, db.AddPendingUOPDelta(f.node(t, s), 3), "climb "+s)
		}
		sm.tick(base.Add(time.Duration(i) * time.Minute))
	}

	sort.Strings(fired)
	assertSet(t, "nodes that raised a parked-ticks alarm", fired,
		[]string{"STRAND_CLIMB", "STRAND_PARKED"})

	var lit []string
	for suffix, name := range f.Names {
		if eng.StrandedAlarmDetail(name) != "" {
			lit = append(lit, suffix)
		}
	}
	sort.Strings(lit)
	assertSet(t, "nodes with a lit chip", lit, []string{"CLIMB", "PARKED"})
}

// ── WALKER 3: THE COUNTER TICK ────────────────────────────────────────────

// tickOutcome is one node's post-tick runtime, which is where the counter
// walk's decision is visible: a node that took the tick moved its cached count,
// a node that held it moved pending instead, and a node that declined moved
// neither.
type tickOutcome struct {
	Remaining int
	Pending   int
}

func tickOutcomes(t *testing.T, db *store.DB, f *walkerFixture) map[string]tickOutcome {
	t.Helper()
	out := map[string]tickOutcome{}
	for suffix, id := range f.NodeIDs {
		rt, err := db.GetProcessNodeRuntime(id)
		if err != nil || rt == nil {
			continue
		}
		out[suffix] = tickOutcome{Remaining: rt.RemainingUOPCached, Pending: int(rt.PendingUOPDelta)}
	}
	return out
}

// TestCounterDelta_DecisionLadder pins which nodes a counter tick lands on.
//
//	NOCLAIM    no claim                    — no_claim, untouched
//	NORUNTIME  no runtime row              — no_runtime, untouched
//	LOADER     manual_swap                 — skipped: a forklift-loaded bin's
//	                                         count is an operator declaration and
//	                                         no PLC tag is tied to it
//	CONSUME    consume, paired, ACTIVE     — decremented
//	UNBOUND    consume, no bin bound       — HELD in pending, count unmoved
//	PRODUCE    produce, bound              — incremented
//	PARKEDCON  consume, paired, not active — SKIPPED (inactive_consume). This is
//	                                         the rung that differs from the level
//	                                         sweep, and the difference is the point
//	PARKEDPRO  produce, paired, not active — SKIPPED (inactive_produce)
//
// CONSUME IS PAIRED WITH PARKEDCON DELIBERATELY. The A/B fallthrough net only
// stands down when a PAIRED consume side took the tick (pairedConsumeHandled),
// so with an unpaired active consume node the net fires and PARKEDCON absorbs
// the count instead of being skipped. That behaviour has its own test below;
// here the pair is closed so the skip rung is the one being read.
func TestCounterDelta_DecisionLadder(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	b1, b2, b3, b4 := int64(11), int64(12), int64(13), int64(14)
	f := seedWalkerProcess(t, db, "TICK", []walkerNode{
		{Suffix: "NOCLAIM", NoClaim: true, Remaining: 100, ActivePull: true},
		{Suffix: "NORUNTIME", NoRuntime: true, Role: protocol.ClaimRoleConsume,
			SwapMode: protocol.SwapModeSingleRobot, PayloadCode: "SYN-PART-A", Capacity: 40},
		{Suffix: "LOADER", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeManualSwap,
			PayloadCode: "SYN-PART-A", Capacity: 40, Remaining: 100, BoundBin: &b1, ActivePull: true},
		{Suffix: "CONSUME", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeSequential,
			PayloadCode: "SYN-PART-A", Capacity: 40, Remaining: 100, BoundBin: &b2,
			Paired: "TICK_PARKEDCON", ActivePull: true},
		{Suffix: "UNBOUND", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeSingleRobot,
			PayloadCode: "SYN-PART-A", Capacity: 40, Remaining: 100, ActivePull: true},
		{Suffix: "PRODUCE", Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeSingleRobot,
			PayloadCode: "SYN-PART-B", Capacity: 30, Remaining: 100, BoundBin: &b3, ActivePull: true},
		{Suffix: "PARKEDCON", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeSequential,
			PayloadCode: "SYN-PART-A", Capacity: 40, Remaining: 100, BoundBin: &b4,
			Paired: "TICK_CONSUME", ActivePull: false},
		{Suffix: "PARKEDPRO", Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeSequential,
			PayloadCode: "SYN-PART-B", Capacity: 30, Remaining: 100,
			Paired: "TICK_PRODUCE", ActivePull: false},
	})
	eng := testEngine(t, db)

	eng.handleCounterDelta(CounterDeltaEvent{ProcessID: f.ProcessID, StyleID: f.StyleID, Delta: 5})

	got := tickOutcomes(t, db, f)
	want := map[string]tickOutcome{
		"NOCLAIM":   {Remaining: 100, Pending: 0},
		"LOADER":    {Remaining: 100, Pending: 0},
		"CONSUME":   {Remaining: 95, Pending: 0},
		"UNBOUND":   {Remaining: 100, Pending: 5},
		"PRODUCE":   {Remaining: 105, Pending: 0},
		"PARKEDCON": {Remaining: 100, Pending: 0},
		"PARKEDPRO": {Remaining: 100, Pending: 0},
	}
	for suffix, w := range want {
		if g := got[suffix]; g != w {
			t.Errorf("node %s after the tick = %+v, want %+v", suffix, g, w)
		}
	}
	if _, ok := got["NORUNTIME"]; ok {
		t.Error("NORUNTIME materialised a runtime row: the counter walk reads, it must not ensure")
	}
}

// TestCounterDelta_StyleMismatchTakesNothing: a tick carrying a style no claim
// on this process names lands nowhere and moves nothing.
//
// The walk still visits every node — it has to read the claim to learn the
// style does not match — so this is the case a batched read must not
// accidentally fix by filtering the claims query on the tick's style. The claim
// that governs a node is the one under the process's ACTIVE style; comparing it
// to the tick's style is a separate question, and a query that conflated them
// would silently start attributing ticks to a style's claims during a
// changeover.
func TestCounterDelta_StyleMismatchTakesNothing(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	bin := int64(21)
	f := seedWalkerProcess(t, db, "MISMATCH", []walkerNode{
		{Suffix: "A", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeSingleRobot,
			PayloadCode: "SYN-PART-A", Capacity: 40, Remaining: 100, BoundBin: &bin},
	})
	eng := testEngine(t, db)

	eng.handleCounterDelta(CounterDeltaEvent{ProcessID: f.ProcessID, StyleID: f.StyleID + 999, Delta: 5})

	if got := tickOutcomes(t, db, f)["A"]; got.Remaining != 100 || got.Pending != 0 {
		t.Errorf("node A after a mis-styled tick = %+v, want the seeded 100/0 — a tick for a style "+
			"this claim does not name must move nothing", got)
	}
}

// TestCounterDelta_ABFallthroughPicksTheFirstParkedSide: when NO active-pull
// consume side existed, the first parked one found takes the count as the
// safety net ("count to lineside storage").
//
// FIRST FOUND, in the node walk's own order, which is ListProcessNodesByProcess
// ORDER BY sequence, name. A batched read that reordered the walk would change
// which side of an A/B pair absorbs a fallthrough tick — a silent change of
// which carrier a count is charged to.
func TestCounterDelta_ABFallthroughPicksTheFirstParkedSide(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	b1, b2 := int64(31), int64(32)
	f := seedWalkerProcess(t, db, "FALL", []walkerNode{
		{Suffix: "A", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeSequential,
			PayloadCode: "SYN-PART-A", Capacity: 40, Remaining: 100, BoundBin: &b1,
			Paired: "FALL_B", ActivePull: false},
		{Suffix: "B", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeSequential,
			PayloadCode: "SYN-PART-A", Capacity: 40, Remaining: 100, BoundBin: &b2,
			Paired: "FALL_A", ActivePull: false},
	})
	eng := testEngine(t, db)

	eng.handleCounterDelta(CounterDeltaEvent{ProcessID: f.ProcessID, StyleID: f.StyleID, Delta: 4})

	got := tickOutcomes(t, db, f)
	if got["A"].Remaining != 96 {
		t.Errorf("node A (sequence 1) = %+v, want remaining 96 — the fallthrough charges the FIRST "+
			"parked side the walk found", got["A"])
	}
	if got["B"].Remaining != 100 {
		t.Errorf("node B = %+v, want the seeded 100 — only one side absorbs a fallthrough tick", got["B"])
	}
}

// TestCounterDelta_MultiPartDrainsEachBucketOnce pins the per-part half of the
// counter tick: which bucket drains, by how much, and what is left for the bin.
//
// The node counter is a single integer — one UOP is one assembly — so only the
// PRIMARY part's drain reduces what flows to the bin. The secondaries drain so
// the board stays honest and change no arithmetic. A part with no bucket drains
// nothing and is not an error; it is the ordinary state of a secondary that has
// not been pulled this cycle.
//
// It is here because the diagnostic that reports an unexpected miss reads the
// node's active buckets ONCE per tick rather than once per miss, and this is the
// test that says the drains themselves did not move with it.
func TestCounterDelta_MultiPartDrainsEachBucketOnce(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	bin := int64(41)
	f := seedWalkerProcess(t, db, "MULTI", []walkerNode{
		{Suffix: "CELL", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeSingleRobot,
			PayloadCode: "SYN-PART-A", Capacity: 40, Remaining: 100, BoundBin: &bin, ActivePull: true},
	})
	claimID := f.ClaimIDs["CELL"]
	_, err := db.DB.Exec(`UPDATE style_node_claims SET allowed_payload_codes=? WHERE id=?`,
		`["SYN-PART-A","SYN-PART-B","SYN-PART-C"]`, claimID)
	testutil.MustNoErr(t, err, "widen allowed payloads")

	nodeID := f.node(t, "CELL")
	// A holds less than the tick; B holds more; C has no bucket at all.
	_, err = db.CaptureLinesideBucket(nodeID, "", f.StyleID, "SYN-PART-A", 2)
	testutil.MustNoErr(t, err, "capture A")
	_, err = db.CaptureLinesideBucket(nodeID, "", f.StyleID, "SYN-PART-B", 5)
	testutil.MustNoErr(t, err, "capture B")

	eng := testEngine(t, db)
	eng.handleCounterDelta(CounterDeltaEvent{ProcessID: f.ProcessID, StyleID: f.StyleID, Delta: 3})

	// A drained its whole 2 and the row went with it; the remaining 1 unit is
	// what reaches the bin.
	if b, err := db.GetActiveLinesideBucket(nodeID, f.StyleID, "SYN-PART-A"); err == nil && b != nil {
		t.Errorf("bucket A still holds %d, want the row gone — it held 2 against a tick of 3", b.Qty)
	}
	b, err := db.GetActiveLinesideBucket(nodeID, f.StyleID, "SYN-PART-B")
	testutil.MustNoErr(t, err, "read bucket B")
	if b == nil || b.Qty != 2 {
		t.Errorf("bucket B = %v, want qty 2 — a secondary drains the full tick independently", b)
	}
	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "read runtime")
	if rt.RemainingUOPCached != 99 {
		t.Errorf("node counter = %d, want 99 — only the PRIMARY's drain reduces what reaches the "+
			"bin, so 3 ticks less the 2 that came off the rack is 1", rt.RemainingUOPCached)
	}
}
