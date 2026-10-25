package dispatch

import (
	"testing"

	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// AN EMPTY THAT NAMES A CONCRETE SOURCE STAYS ON IT (plan.Finder tier 4's
// namedEmpty), as a full that names one does (namedFull, the pins beside this
// file). Before it, an empty need naming one physical, non-loader node passed
// every scoped tier and took an empty from anywhere in the plant. It now takes
// the empties standing on that node, bound as the reserve binds them
// (binresolver.BindableEmpties), or waits there.

// namedEmptyNeed is an empty need naming the concrete node FG-DROP-01 of the
// named-source fixture, set down on setDown.
func namedEmptyNeed(setDown string) SourceNeed {
	return SourceNeed{SourceNode: "FG-DROP-01", SetDownNode: setDown, Intent: IntentEmpty}
}

// namedEmptyFixture is the named-source fixture with an empty standing in the
// unrelated supermarket, which the plant-wide empty scan returns, and two
// set-down nodes: OUTSIDE wants a two-high stack, CORE a fed press's 0.
func namedEmptyFixture() *fakeFinderDB {
	db := namedSourceFixture()
	db.globalEmpty = &bins.Bin{ID: 902, NodeID: db.fifoBin.NodeID, Status: "available"}
	db.addNode(&nodes.Node{ID: 70, Name: "OUTSIDE", Enabled: true})
	db.addNode(&nodes.Node{ID: 71, Name: "CORE", Enabled: true})
	db.wantAt = map[string]int{"OUTSIDE": 2, "CORE": 0}
	return db
}

func topsAndBottomsCart(id int64) *bins.Bin {
	zero, eight := 0, 8
	return &bins.Bin{ID: id, TotesOnCart: &zero, TotesPerLayer: &eight}
}

func plainEmpty(id int64) *bins.Bin { return &bins.Bin{ID: id} }

// TestNamedEmpty_ConcreteSourceHoldingNothingWaits: the node holds nothing and
// the plant holds an empty: the need waits on the node and the plant-wide scan
// is never asked.
func TestNamedEmpty_ConcreteSourceHoldingNothingWaits(t *testing.T) {
	t.Parallel()
	db := namedEmptyFixture()
	res := NewSourceFinder(db, nil, nil).FindSourceForNeed(namedEmptyNeed("OUTSIDE"))
	if res.Outcome != OutcomeWait || res.QueueCause != CauseFinderNodeEmpty || res.QueueParams.Group != "FG-DROP-01" {
		t.Fatalf("outcome=%v cause=%q params=%+v bin=%v, want Wait %q on FG-DROP-01",
			res.Outcome, res.QueueCause, res.QueueParams, res.Bin, CauseFinderNodeEmpty)
	}
	if db.globalEmptyCalls != 0 || db.typedGlobalCalls != 0 {
		t.Errorf("plant-wide empty scans = %d + %d, want 0: a concrete source never widens", db.globalEmptyCalls, db.typedGlobalCalls)
	}
}

// TestNamedEmpty_WhatTheNodeHolds: what an empty need naming FG-DROP-01 takes
// there, by what stands on it and where it is set down. Tops & bottoms binds
// where the set-down height admits it (a fed press's core) or where the node is
// a line position; anywhere else it waits. The line-position read is made only
// when tops & bottoms stands there.
func TestNamedEmpty_WhatTheNodeHolds(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name      string
		standing  []*bins.Bin
		setDown   string
		line      bool
		took      int64 // 0: waits on the node
		lineReads int
	}{
		{"an empty, set down on an outside", []*bins.Bin{plainEmpty(630)}, "OUTSIDE", false, 630, 0},
		{"tops & bottoms, set down on an outside", []*bins.Bin{topsAndBottomsCart(631)}, "OUTSIDE", false, 0, 1},
		{"tops & bottoms, set down on a fed core", []*bins.Bin{topsAndBottomsCart(631)}, "CORE", false, 631, 1},
		{"tops & bottoms, no height said", []*bins.Bin{topsAndBottomsCart(631)}, "", false, 0, 1},
		{"tops & bottoms on a line position, set down on an outside", []*bins.Bin{topsAndBottomsCart(631)}, "OUTSIDE", true, 631, 1},
		{"tops & bottoms beside an empty, set down on an outside", []*bins.Bin{topsAndBottomsCart(631), plainEmpty(630)}, "OUTSIDE", false, 630, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			db := namedEmptyFixture()
			dropNodeWith(db, c.standing...)
			if c.line {
				db.linePositions = map[string]bool{"FG-DROP-01": true}
			}
			res := NewSourceFinder(db, nil, nil).FindSourceForNeed(namedEmptyNeed(c.setDown))
			var got int64
			if res.Outcome == OutcomeFound && res.Bin != nil {
				got = res.Bin.ID
				if res.Node == nil || res.Node.Name != "FG-DROP-01" {
					t.Errorf("found on %v, want FG-DROP-01", res.Node)
				}
			} else if res.Outcome != OutcomeWait || res.QueueCause != CauseFinderNodeEmpty {
				t.Errorf("outcome=%v cause=%q, want Found or Wait %q", res.Outcome, res.QueueCause, CauseFinderNodeEmpty)
			}
			if got != c.took {
				t.Errorf("took %d, want %d", got, c.took)
			}
			if db.lineReads != c.lineReads {
				t.Errorf("line-position reads = %d, want %d", db.lineReads, c.lineReads)
			}
			if db.globalEmptyCalls != 0 || db.typedGlobalCalls != 0 {
				t.Errorf("plant-wide empty scans = %d + %d, want 0", db.globalEmptyCalls, db.typedGlobalCalls)
			}
		})
	}
}

// TestNamedSource_ADisabledSourceWaits: a switched-off node is dead to
// automation (helpers.NodeEnabledSQL, the 2026-09-14 ruling), so a retrieve
// naming one takes nothing standing there — a confirmed full or an empty — and
// waits on the node, never widening plant-wide. Rule 6's table asks the full at
// every reader (TestRule6_OneAvailabilityTable, tier4_named_full); this is the
// empty beside it, which the table does not ask.
func TestNamedSource_ADisabledSourceWaits(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		standing *bins.Bin
		need     func(db *fakeFinderDB) SourceResult
	}{
		{"a full", &bins.Bin{ID: 640, PayloadCode: "PART-A", UOPRemaining: 100, UOPCapacity: 100, ManifestConfirmed: true},
			func(db *fakeFinderDB) SourceResult {
				return NewSourceFinder(db, nil, nil).FindSource(u1Order("FG-DROP-01"), IntentFull)
			}},
		{"an empty", plainEmpty(641), func(db *fakeFinderDB) SourceResult {
			return NewSourceFinder(db, nil, nil).FindSourceForNeed(namedEmptyNeed("OUTSIDE"))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			db := namedEmptyFixture()
			db.nodesByName["FG-DROP-01"].Enabled = false
			dropNodeWith(db, c.standing)
			res := c.need(db)
			if res.Outcome != OutcomeWait || res.QueueCause != CauseFinderNodeEmpty || res.QueueParams.Group != "FG-DROP-01" {
				t.Fatalf("outcome=%v cause=%q params=%+v bin=%v, want Wait %q on FG-DROP-01",
					res.Outcome, res.QueueCause, res.QueueParams, res.Bin, CauseFinderNodeEmpty)
			}
			if db.fifoCalls != 0 || db.globalEmptyCalls != 0 || db.typedGlobalCalls != 0 {
				t.Errorf("plant-wide scans = %d + %d + %d, want 0: a named source never widens",
					db.fifoCalls, db.globalEmptyCalls, db.typedGlobalCalls)
			}
		})
	}
}
