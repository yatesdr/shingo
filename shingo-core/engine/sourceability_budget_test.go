//go:build docker

package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/demands"
	"shingocore/store/plantclaims"
)

// sourceability_budget_test.go — what one pass of each recompute path costs in
// STATEMENTS.
//
// THE TWO PATHS HAVE DIFFERENT BUDGETS AND ONE SHARED READER. recomputeAll runs
// off a two-minute timer; recomputeKeys runs off a 300 ms debounce fired by bin
// movement, so its cadence is set by plant traffic. Both call BuildInputs. A
// query added for the full pass therefore lands on the debounced path too, and
// nothing in a behaviour test notices — which is exactly what happened when
// ActiveStyles went into BuildInputs for samples only the full pass reads.
//
// THE NUMBERS BELOW ARE THE MEASUREMENT, NOT AN AMBITION. A change that moves
// one of them must move it in the commit that moves the behaviour, with the new
// formula written beside it, so the diff says what got cheaper or dearer and by
// how much rather than leaving it to be rediscovered.
//
// The counting seam is store.OpenCounting (store/query_count.go) — pgx's own
// QueryTracer, counting where the application hands pgx a statement. Nothing is
// threaded through production code.

// recomputeBudget is one path and the statements a pass over it costs.
type recomputeBudget struct {
	path string
	want int64
	// formula says what the number is made of, so a future reader can tell a
	// legitimate move from a regression without re-deriving it.
	formula string
}

// The pinned costs, measured on this fixture.
//
// BOTH FULL-PASS SHAPES ARE PINNED. With no running style no cell sample is
// taken and RecordTTESamples is never reached, which isolates read cost from
// write cost; with one, the write is on the pass, and that is what a plant
// actually runs. Reading a change's cost off the wrong one of those two
// overstates it by the three statements of a write that was always there.
//
// recomputeKeys is the one that matters. It fires on bin movement, so it is
// paid at whatever rate the plant moves bins, and every statement on it is on
// the path that tells an Edge what it can source. The seventh BuildInputs read
// is ActiveStyles, whose answer only the full pass's samples consume.
var (
	budgetRecomputeAll = recomputeBudget{
		path: "recomputeAll", want: 8,
		formula: "BuildInputs 7 (styles, claims, pool, on-line, line-UOP, rates, active-styles) " +
			"+ 1 verdict-history write, this being the first observation of the style. No sample " +
			"write: with no running style there is no cell sample, so RecordTTESamples is not reached",
	}
	budgetRecomputeAllRunning = recomputeBudget{
		path: "recomputeAll (running style)", want: 11,
		formula: "the no-running-style pass (8) plus the cell sample write: INSERT 1 + prunes 2 " +
			"(samples, drain ledger). THIS is the shape a plant runs, because a plant is always " +
			"running a style",
	}
	budgetRecomputeKeys = recomputeBudget{
		path: "recomputeKeys", want: 7,
		formula: "BuildInputs 7, and nothing else — the debounced path writes nothing in steady " +
			"state. One of the seven is ActiveStyles, which only the full pass's samples read",
	}
)

// seedBudgetFixture builds the smallest plant both paths will walk: one style
// with one claim, one bin that satisfies it, and one monitored binding at a
// loader node so the loader-sample reads have something to do.
//
// running SAYS WHETHER THE STYLE IS THE ONE THE PROCESS IS RUNNING, which
// decides whether a CELL sample is taken and therefore whether the sample write
// is on the pass at all. Both shapes are pinned: false isolates read cost from
// write cost, true is what a plant actually looks like.
//
// SEEDED THROUGH THE PLAIN HANDLE, counted through the counting one. Both reach
// the same database; the split is what keeps fixture writes out of the number.
func seedBudgetFixture(t *testing.T, running bool) (*store.DB, *store.QueryCounter, []plantclaims.ProcessKey) {
	t.Helper()

	db, cfg := testdb.OpenWithConfig(t)
	storageNode, _, _ := setupTestData(t, db)
	createTestBinAtNode(t, db, "BIN-BUD", storageNode.ID, "src")

	testutil.MustNoErr(t, plantclaims.ReplaceProcess(db.DB, "SNFB",
		[]plantclaims.StyleRow{{ProcessID: "SNFB", StyleID: "A", IsActive: running}},
		[]plantclaims.ClaimRow{{ProcessID: "SNFB", StyleID: "A",
			CoreNodeName: storageNode.Name, PayloadCode: "BIN-BUD"}},
		0), "seed mirror")

	// A monitored binding. Its node is deliberately NOT the claim's: that is the
	// shape Q3 measured at Springfield, where no binding sits at a node any style
	// claim names, and it is the shape the loader samples exist to cover.
	_, err := db.SyncDemandRegistry("ST-BUD", []demands.RegistryEntry{{
		StationID: "ST-BUD", CoreNodeName: "LOADER-BUD", Role: protocol.ClaimRoleConsume,
		PayloadCode: "BIN-BUD", ReplenishUOPThreshold: 5,
	}})
	testutil.MustNoErr(t, err, "seed monitored binding")

	cdb, counter, err := store.OpenCounting(cfg)
	testutil.MustNoErr(t, err, "open counting db")
	t.Cleanup(func() { cdb.Close() })

	return cdb, counter, []plantclaims.ProcessKey{{ProcessID: "SNFB", StyleID: "A"}}
}

// TestBudget_RecomputeAll pins the full pass's statement count on a process
// with no running style — read cost, isolated from the sample write.
func TestBudget_RecomputeAll(t *testing.T) {
	t.Parallel()
	cdb, counter, _ := seedBudgetFixture(t, false)

	eng := newUnstartedEngine(t, cdb, simulator.New())
	m := eng.SourceabilityMonitor()
	m.publishFn = nil

	counter.Reset()
	m.recomputeAll()
	got := counter.Count()

	if got != budgetRecomputeAll.want {
		t.Errorf("%s: %d statements, pinned at %d (%s)",
			budgetRecomputeAll.path, got, budgetRecomputeAll.want, budgetRecomputeAll.formula)
	}
}

// TestBudget_RecomputeAllRunningStyle pins the PRODUCTION shape: a process that
// is running one of its styles, so cell samples are taken and the sample write
// is on the pass — which it is on every plant, every pass, and was before B8.
//
// THIS IS THE NUMBER B8's COST SHOULD BE READ FROM. Against the no-running-style
// fixture B8 looks like +6, but three of those are the sample INSERT and the two
// prunes, which that fixture simply never reached at base because it had no cell
// sample to write. A plant always has one. Measured here so the difference is a
// pinned fact rather than an argument.
func TestBudget_RecomputeAllRunningStyle(t *testing.T) {
	t.Parallel()
	cdb, counter, _ := seedBudgetFixture(t, true)

	eng := newUnstartedEngine(t, cdb, simulator.New())
	m := eng.SourceabilityMonitor()
	m.publishFn = nil

	counter.Reset()
	m.recomputeAll()
	got := counter.Count()

	if got != budgetRecomputeAllRunning.want {
		t.Errorf("%s: %d statements, pinned at %d (%s)",
			budgetRecomputeAllRunning.path, got, budgetRecomputeAllRunning.want, budgetRecomputeAllRunning.formula)
	}
}

// TestBudget_RecomputeKeys pins the debounced path's statement count.
//
// THIS IS THE ONE THAT MATTERS. It fires on bin movement, so its cost is paid
// at whatever rate the plant moves bins, and everything it reads is on the path
// that tells an Edge what it can source. A read added here that the verdict
// does not need is both a cost and a new way for the verdict to fail.
func TestBudget_RecomputeKeys(t *testing.T) {
	t.Parallel()
	cdb, counter, keys := seedBudgetFixture(t, false)

	eng := newUnstartedEngine(t, cdb, simulator.New())
	m := eng.SourceabilityMonitor()
	m.publishFn = nil

	// One pass first: the first recompute is the one that finds every verdict
	// "changed" (there was no prior state) and therefore writes history. The
	// steady state is the pass after, and the steady state is what runs on a
	// plant.
	m.recomputeKeys(keys)

	counter.Reset()
	m.recomputeKeys(keys)
	got := counter.Count()

	if got != budgetRecomputeKeys.want {
		t.Errorf("%s: %d statements, pinned at %d (%s)",
			budgetRecomputeKeys.path, got, budgetRecomputeKeys.want, budgetRecomputeKeys.formula)
	}
}
