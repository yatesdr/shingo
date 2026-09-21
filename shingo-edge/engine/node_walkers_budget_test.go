package engine

import (
	"fmt"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store"
)

// node_walkers_budget_test.go — what one pass of each per-node walker costs in
// STATEMENTS.
//
// The edge store is pinned to a single SQLite connection (store.Open sets
// SetMaxOpenConns(1)) on a Raspberry Pi, so every statement one of these
// walkers issues is a statement the operator board's next poll waits behind.
// A walker that issues a fixed number of statements per NODE therefore gets
// slower for the plant exactly as the plant grows — and the growth is
// invisible in a test that asserts on wall time, which is noise on a loaded
// runner. It is not invisible in a count.
//
// THE NUMBERS BELOW ARE THE MEASUREMENT, NOT AN AMBITION. Each table says what
// the walker costs today at 1, 4 and 12 nodes. A change that moves one of them
// must move it in the commit that moves the behaviour, with the new formula
// written in the comment beside it, so the diff says what got cheaper and by
// how much rather than leaving it to be rediscovered.
//
// The counting seam is store.OpenCounting (store/query_count.go) — an existing
// database/sql driver wrapper that counts at the point the application hands a
// statement to SQLite. Nothing is threaded through production code.

// walkerBudget is one process size and the statements a pass over it costs.
type walkerBudget struct {
	nodes int
	want  int64
}

// checkBudget runs one size and reports the count against the pinned number.
func checkBudget(t *testing.T, size walkerBudget, got int64, formula string) {
	t.Helper()
	if got != size.want {
		t.Errorf("%d nodes: %d statements, pinned at %d (%s)", size.nodes, got, size.want, formula)
	}
}

// uniformConsumeNodes builds n identical consume cells, sitting comfortably
// above their reorder point so the sweep walks them and asks for nothing.
//
// ABOVE THE LEVEL ON PURPOSE. A pass that asks mints orders, and an ask is
// per-node work no batching can or should remove — the order is the point. The
// steady state is the one that runs every sixty seconds on a healthy plant, and
// it is the walk alone.
func uniformConsumeNodes(n int, bin *int64) []walkerNode {
	out := make([]walkerNode, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, walkerNode{
			Suffix: fmt.Sprintf("C%02d", i+1), Role: protocol.ClaimRoleConsume,
			SwapMode: protocol.SwapModeSingleRobot, PayloadCode: "SYN-PART-A",
			Capacity: 40, ReorderPoint: 25, AutoReorder: true,
			Remaining: 40, BoundBin: bin, ActivePull: true,
		})
	}
	return out
}

// ── WALKER 1: sweepProcessLevels ──────────────────────────────────────────

// TestLevelSweep_StatementsPerPass pins what one process costs the level sweep
// per period.
//
// sweepCellLevels runs this for EVERY process, so the per-process figure is
// multiplied by the process count before it reaches the connection.
func TestLevelSweep_StatementsPerPass(t *testing.T) {
	t.Parallel()
	// Was 2N+1 — a claim lookup and a runtime read per node. Now 3 flat: the
	// node list, one claims-for-styles read, one runtimes-for-nodes read.
	for _, size := range []walkerBudget{
		{nodes: 1, want: 3},
		{nodes: 4, want: 3},
		{nodes: 12, want: 3},
	} {
		t.Run(fmt.Sprintf("%dnodes", size.nodes), func(t *testing.T) {
			t.Parallel()
			db, counter := testEngineDBCounting(t)
			bin := int64(1)
			f := seedWalkerProcess(t, db, "BUDGETL", uniformConsumeNodes(size.nodes, &bin))
			eng := testEngine(t, db)
			proc, err := db.GetProcess(f.ProcessID)
			testutil.MustNoErr(t, err, "get process")

			counter.Reset()
			eng.sweepProcessLevels(proc)
			checkBudget(t, size, counter.Count(),
				"node list + claims-for-styles + runtimes-for-nodes = 3, independent of node count")

			// The pass asked for nothing, so the count above is the walk and
			// nothing else. Asserted rather than assumed: a fixture that drifted
			// into asking would inflate the budget and hide the walk.
			if got := countOrders(t, db); got != 0 {
				t.Fatalf("the steady-state pass minted %d orders — the fixture is not above its level", got)
			}
		})
	}
}

// ── WALKER 2: strandedMonitor.tick ────────────────────────────────────────

// TestStrandedMonitor_StatementsPerPass pins the parked-ticks scan.
//
// It walks EVERY node on the Edge, not one process's, so this is the whole
// plant's node count and not a cell's.
func TestStrandedMonitor_StatementsPerPass(t *testing.T) {
	t.Parallel()
	// Was 3N+1 — a process read, a claim lookup and a runtime read per node.
	// Now 4 flat, and flat in the PROCESS count too: the per-node process read
	// became one process list.
	for _, size := range []walkerBudget{
		{nodes: 1, want: 4},
		{nodes: 4, want: 4},
		{nodes: 12, want: 4},
	} {
		t.Run(fmt.Sprintf("%dnodes", size.nodes), func(t *testing.T) {
			t.Parallel()
			db, counter := testEngineDBCounting(t)
			bin := int64(1)
			seedWalkerProcess(t, db, "BUDGETS", uniformConsumeNodes(size.nodes, &bin))
			eng := testEngine(t, db)
			sm := &strandedMonitor{eng: eng, states: map[int64]*strandedNodeState{}}

			counter.Reset()
			sm.tick(time.Now())
			checkBudget(t, size, counter.Count(),
				"node list + process list + claims-for-styles + runtimes-for-nodes = 4")
		})
	}
}

// ── WALKER 3: handleCounterDelta ──────────────────────────────────────────

// TestCounterDelta_StatementsPerTickWalk pins the walk alone: a tick carrying a
// style no node on the process claims.
//
// EVERY NODE IS STILL VISITED AND STILL READ — the walk has to read the runtime
// row and resolve the claim before it can find out the style does not match —
// so this is the per-node read cost with no landing on top of it. The tick
// fires on every PLC count, which makes it the hottest of the three.
func TestCounterDelta_StatementsPerTickWalk(t *testing.T) {
	t.Parallel()
	// Was 3N+1 — a runtime read, a process read and a claim lookup per node.
	// Now 4 flat: the node list, one process read, one claims-for-styles read,
	// one runtimes-for-nodes read.
	for _, size := range []walkerBudget{
		{nodes: 1, want: 4},
		{nodes: 4, want: 4},
		{nodes: 12, want: 4},
	} {
		t.Run(fmt.Sprintf("%dnodes", size.nodes), func(t *testing.T) {
			t.Parallel()
			db, counter := testEngineDBCounting(t)
			bin := int64(1)
			f := seedWalkerProcess(t, db, "BUDGETW", uniformConsumeNodes(size.nodes, &bin))
			eng := testEngine(t, db)

			counter.Reset()
			eng.handleCounterDelta(CounterDeltaEvent{
				ProcessID: f.ProcessID, StyleID: f.StyleID + 9999, Delta: 3,
			})
			checkBudget(t, size, counter.Count(),
				"node list + process + claims-for-styles + runtimes-for-nodes = 4")
		})
	}
}

// TestCounterDelta_StatementsPerTickLanding pins the whole tick, walk plus
// landing, for a process where every node takes the count.
//
// The landing itself is irreducibly per-node — each cell's own counter has to
// move — so the interesting figure is the gap between this and the walk above.
func TestCounterDelta_StatementsPerTickLanding(t *testing.T) {
	t.Parallel()
	// Was 3N+1 walk + 3 per landing node. Now a flat 4 walk + the same 3.
	for _, size := range []walkerBudget{
		{nodes: 1, want: 7},
		{nodes: 4, want: 16},
		{nodes: 12, want: 40},
	} {
		t.Run(fmt.Sprintf("%dnodes", size.nodes), func(t *testing.T) {
			t.Parallel()
			db, counter := testEngineDBCounting(t)
			bin := int64(1)
			f := seedWalkerProcess(t, db, "BUDGETT", uniformConsumeNodes(size.nodes, &bin))
			eng := testEngine(t, db)

			counter.Reset()
			eng.handleCounterDelta(CounterDeltaEvent{
				ProcessID: f.ProcessID, StyleID: f.StyleID, Delta: 3,
			})
			checkBudget(t, size, counter.Count(),
				"the flat 4-statement walk plus, per landing node: 1 bucket drain + 1 "+
					"active-bucket visibility read + 1 counter update")

			// Every node took it: the landing count is N, not a subset.
			for suffix, id := range f.NodeIDs {
				rt, err := db.GetProcessNodeRuntime(id)
				testutil.MustNoErr(t, err, "read runtime")
				if rt.RemainingUOPCached != 37 {
					t.Fatalf("node %s = %d, want 37 — every node must take this tick or the "+
						"landing cost is being measured on a subset", suffix, rt.RemainingUOPCached)
				}
			}
		})
	}
}

// TestCounterDelta_StatementsPerPart pins the OTHER multiplier on this path: a
// multi-part claim drains one bucket per allowed payload.
//
// A multi-part claim is one assembly built from several staged parts. The node
// counter is a single integer — one UOP is one assembly — so only the primary
// part's drain changes the arithmetic; the secondaries drain so the board stays
// honest. Each of them is its own statement, and each miss asks the node's
// active buckets whether it should have drained.
func TestCounterDelta_StatementsPerPart(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		parts int
		want  int64
	}{
		{parts: 1, want: 7},
		{parts: 3, want: 9},
		{parts: 6, want: 12},
	} {
		t.Run(fmt.Sprintf("%dparts", tc.parts), func(t *testing.T) {
			t.Parallel()
			db, counter := testEngineDBCounting(t)
			bin := int64(1)
			nodes := uniformConsumeNodes(1, &bin)
			f := seedWalkerProcess(t, db, "BUDGETP", nodes)
			seedAllowedPayloads(t, db, f.ClaimIDs["C01"], tc.parts)
			eng := testEngine(t, db)

			counter.Reset()
			eng.handleCounterDelta(CounterDeltaEvent{
				ProcessID: f.ProcessID, StyleID: f.StyleID, Delta: 3,
			})
			if got := counter.Count(); got != tc.want {
				t.Errorf("%d parts: %d statements, pinned at %d (per part: 1 drain, and ONE "+
					"active-bucket visibility read for the whole tick however many parts missed)",
					tc.parts, got, tc.want)
			}
		})
	}
}

// seedAllowedPayloads widens a claim to n allowed payload codes, the first of
// which is the claim's own primary. Written directly because allowed_payload_codes
// is a JSON column the upsert validator only accepts for some modes, and the
// shape being measured here is the read path's, not the editor's.
func seedAllowedPayloads(t *testing.T, db *store.DB, claimID int64, n int) {
	t.Helper()
	codes := `["SYN-PART-A"`
	for i := 1; i < n; i++ {
		codes += fmt.Sprintf(`,"SYN-PART-%02d"`, i)
	}
	codes += `]`
	_, err := db.DB.Exec(`UPDATE style_node_claims SET allowed_payload_codes=? WHERE id=?`, codes, claimID)
	testutil.MustNoErr(t, err, "widen allowed payloads")
}
