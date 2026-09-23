package dispatch

import (
	"testing"

	"shingo/protocol"

	"shingocore/dispatch/binresolver"
	"shingocore/domain"
	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
	"shingocore/store/reservations"
)

// A NAMED SOURCE FOR A FULL STAYS SCOPED.
//
// A full-intent retrieve used to be scoped by its source in two cases only: an
// NGRP (tier 1) and a dedicated-loader position (tier 2). A LANE or a concrete
// non-loader node passed both, was not node-local so tier 4 skipped it, and
// landed in tier 5's plant-wide FindSourceBinFIFO — which took a full of the
// part from wherever it stood, another cell's supermarket included. Pinned at
// 1bb689bc (4369a0d5); the LANE and concrete cases below now say the new
// verdict, and the blank / NGRP / loader-position cases did not move.
//
// It is tier 3's rule for empties applied to fulls: a scoped need that falls
// through to the plant-wide scan is the Hopkinsville wrong-supermarket pull.

// namedSourceFixture is the production U1's destination (consume window
// SMN_001) plus three candidate sources — a LANE, a concrete non-loader node,
// and an NGRP — and one FULL of PART-A standing in an UNRELATED supermarket,
// which is what the plant-wide scan returns.
func namedSourceFixture() *fakeFinderDB {
	db := newFakeFinderDB()
	db.addNode(&nodes.Node{ID: 10, Name: "SMN_001", Enabled: true})
	db.addConsumeLoaderWindow(1, 10)
	db.addNode(&nodes.Node{ID: 60, Name: "FG-LANE-07", Enabled: true, IsSynthetic: true, NodeTypeCode: protocol.NodeClassLANE})
	db.addNode(&nodes.Node{ID: 61, Name: "FG-DROP-01", Enabled: true})
	db.addNode(&nodes.Node{ID: 40, Name: "FG-BUFFER", Enabled: true, IsSynthetic: true, NodeTypeCode: protocol.NodeClassNGRP})
	other := int64(90)
	grp := int64(91)
	db.addNode(&nodes.Node{ID: grp, Name: "OTHER-CELL-SM", Enabled: true, IsSynthetic: true, NodeTypeCode: protocol.NodeClassNGRP})
	db.addNode(&nodes.Node{ID: other, Name: "OTHER-CELL-SM-01", Enabled: true, ParentID: &grp})
	db.fifoBin = &bins.Bin{ID: 901, PayloadCode: "PART-A", NodeID: &other, UOPRemaining: 100, UOPCapacity: 100, Status: "available"}
	db.addBin(db.fifoBin)
	return db
}

// laneRecordingResolver stands in for the resolver on a LANE source: it records
// the node it was asked to resolve, and answers the way the group scan does
// (the first candidate the filter admits).
type laneRecordingResolver struct {
	filterHonouringResolver
	asked *nodes.Node
}

func (r *laneRecordingResolver) Resolve(n *nodes.Node, mode binresolver.ResolveMode, payload string,
	st binresolver.BinTypeStatement, a reservations.DigAsker, filter binresolver.BinFilter) (*ResolveResult, error) {
	r.asked = n
	return r.filterHonouringResolver.Resolve(n, mode, payload, st, a, filter)
}

// LANE, nothing in it: waits scoped on the lane, never widens.
func TestNamedSourcePin_LaneSourceStaysInTheLane(t *testing.T) {
	t.Parallel()
	db := namedSourceFixture()
	r := &laneRecordingResolver{}
	res := NewSourceFinder(db, r, nil).FindSource(u1Order("FG-LANE-07"), IntentFull)
	if res.Outcome != OutcomeWait || res.QueueCause != CauseFinderGroupEmpty || res.QueueParams.Group != "FG-LANE-07" {
		t.Fatalf("U1 sourcing from a LANE: outcome=%v cause=%q params=%+v bin=%v, want Wait %q on FG-LANE-07",
			res.Outcome, res.QueueCause, res.QueueParams, res.Bin, CauseFinderGroupEmpty)
	}
	if r.asked == nil || r.asked.Name != "FG-LANE-07" {
		t.Errorf("resolver asked about %v, want the lane", r.asked)
	}
	if db.fifoCalls != 0 {
		t.Errorf("FindSourceBinFIFO calls = %d, want 0 — a lane source never widens", db.fifoCalls)
	}
}

// LANE holding a partial and a full: the full, through the resolver, with the
// drain window's fullness filter passed down.
func TestNamedSource_LaneSourceTakesTheFullInTheLane(t *testing.T) {
	t.Parallel()
	db := namedSourceFixture()
	slot := &nodes.Node{ID: 62, Name: "FG-LANE-07-S1", Enabled: true}
	db.addNode(slot)
	r := &laneRecordingResolver{filterHonouringResolver: filterHonouringResolver{cands: []*ResolveResult{
		{Bin: &bins.Bin{ID: 610, PayloadCode: "PART-A", UOPRemaining: 40, UOPCapacity: 100, NodeID: &slot.ID}, Node: slot},
		{Bin: &bins.Bin{ID: 611, PayloadCode: "PART-A", UOPRemaining: 100, UOPCapacity: 100, NodeID: &slot.ID}, Node: slot},
	}}}
	res := NewSourceFinder(db, r, nil).FindSource(u1Order("FG-LANE-07"), IntentFull)
	if res.Outcome != OutcomeFound || res.Bin == nil || res.Bin.ID != 611 {
		t.Fatalf("outcome=%v cause=%q bin=%v, want the lane's full 611", res.Outcome, res.QueueCause, res.Bin)
	}
	if !r.sawFilter {
		t.Error("the lane was resolved without the drain window's fullness filter")
	}
	if db.fifoCalls != 0 {
		t.Errorf("FindSourceBinFIFO calls = %d, want 0", db.fifoCalls)
	}
}

// CONCRETE NODE holding nothing: waits on the node, never widens.
func TestNamedSourcePin_ConcreteSourceStaysOnTheNode(t *testing.T) {
	t.Parallel()
	db := namedSourceFixture()
	res := NewSourceFinder(db, nil, nil).FindSource(u1Order("FG-DROP-01"), IntentFull)
	if res.Outcome != OutcomeWait || res.QueueCause != CauseFinderNodeEmpty || res.QueueParams.Group != "FG-DROP-01" {
		t.Fatalf("U1 sourcing from a concrete node: outcome=%v cause=%q params=%+v bin=%v, want Wait %q on FG-DROP-01",
			res.Outcome, res.QueueCause, res.QueueParams, res.Bin, CauseFinderNodeEmpty)
	}
	if db.fifoCalls != 0 {
		t.Errorf("FindSourceBinFIFO calls = %d, want 0 — a concrete source never widens", db.fifoCalls)
	}
}

// dropNodeWith puts bins on the concrete source FG-DROP-01.
func dropNodeWith(db *fakeFinderDB, bs ...*bins.Bin) {
	id := int64(61)
	for _, b := range bs {
		b.NodeID = &id
		b.Status = "available"
		db.addBin(b)
	}
}

// CONCRETE NODE holding a partial and a full, at a drain window: the full is
// taken — a partial on the node does not hide the full beside it.
func TestNamedSource_ConcreteSourceTakesTheFullBesideAPartial(t *testing.T) {
	t.Parallel()
	db := namedSourceFixture()
	dropNodeWith(db,
		&bins.Bin{ID: 620, PayloadCode: "PART-A", UOPRemaining: 40, UOPCapacity: 100},
		&bins.Bin{ID: 621, PayloadCode: "PART-A", UOPRemaining: 100, UOPCapacity: 100})
	res := NewSourceFinder(db, nil, nil).FindSource(u1Order("FG-DROP-01"), IntentFull)
	if res.Outcome != OutcomeFound || res.Bin == nil || res.Bin.ID != 621 || res.Node == nil || res.Node.Name != "FG-DROP-01" {
		t.Fatalf("outcome=%v cause=%q bin=%v node=%v, want the full 621 on FG-DROP-01",
			res.Outcome, res.QueueCause, res.Bin, res.Node)
	}
	if db.fifoCalls != 0 {
		t.Errorf("FindSourceBinFIFO calls = %d, want 0", db.fifoCalls)
	}
}

// CONCRETE NODE holding only a partial of the part, at a drain window: waits
// for a full. Not Found by the named-node exemption, which is for a person's
// move and not the plant's pull.
func TestNamedSource_ConcreteSourceOnlyAPartialWaitsForAFull(t *testing.T) {
	t.Parallel()
	db := namedSourceFixture()
	dropNodeWith(db, &bins.Bin{ID: 620, PayloadCode: "PART-A", UOPRemaining: 40, UOPCapacity: 100})
	res := NewSourceFinder(db, nil, nil).FindSource(u1Order("FG-DROP-01"), IntentFull)
	if res.Outcome != OutcomeWait || res.QueueCause != CauseFinderNoFullCarrier {
		t.Fatalf("outcome=%v cause=%q bin=%v, want Wait %q", res.Outcome, res.QueueCause, res.Bin, CauseFinderNoFullCarrier)
	}
}

// CONCRETE NODE holding an EMPTY carrier: an empty is not a full of anything.
// The a9a0eb78 empty exemption is for moves; a retrieve needs the part.
func TestNamedSource_ConcreteSourceDoesNotTakeAnEmpty(t *testing.T) {
	t.Parallel()
	db := namedSourceFixture()
	dropNodeWith(db, &bins.Bin{ID: 622, UOPRemaining: 0, UOPCapacity: 100})
	res := NewSourceFinder(db, nil, nil).FindSource(u1Order("FG-DROP-01"), IntentFull)
	if res.Outcome != OutcomeWait || res.QueueCause != CauseFinderNodeEmpty {
		t.Fatalf("outcome=%v cause=%q bin=%v, want Wait %q", res.Outcome, res.QueueCause, res.Bin, CauseFinderNodeEmpty)
	}
}

// BLANK: no source named, plant-wide by design. Must not move.
func TestNamedSourcePin_BlankSourceIsPlantWide(t *testing.T) {
	t.Parallel()
	db := namedSourceFixture()
	res := NewSourceFinder(db, nil, nil).FindSource(u1Order(""), IntentFull)
	if res.Outcome != OutcomeFound || res.Bin == nil || res.Bin.ID != 901 {
		t.Fatalf("blank source: outcome=%v bin=%v, want the plant-wide full 901", res.Outcome, res.Bin)
	}
	if db.fifoCalls != 1 {
		t.Errorf("FindSourceBinFIFO calls = %d, want 1", db.fifoCalls)
	}
}

// NGRP: tier 1 resolves inside the group and, finding nothing, waits scoped;
// the plant-wide scan is never asked. Must not move.
func TestNamedSourcePin_NGRPSourceStaysScoped(t *testing.T) {
	t.Parallel()
	db := namedSourceFixture()
	r := &filterHonouringResolver{} // the group holds nothing
	res := NewSourceFinder(db, r, nil).FindSource(u1Order("FG-BUFFER"), IntentFull)
	if res.Outcome != OutcomeWait || res.QueueCause != CauseFinderGroupEmpty {
		t.Fatalf("NGRP source: outcome=%v cause=%q, want Wait %q", res.Outcome, res.QueueCause, CauseFinderGroupEmpty)
	}
	if db.fifoCalls != 0 {
		t.Errorf("FindSourceBinFIFO calls = %d, want 0 — an NGRP source never widens", db.fifoCalls)
	}
}

// DEDICATED-LOADER POSITION: tier 2 ranks the loader's pool and, finding no
// part, waits scoped; the plant-wide scan is never asked. Must not move.
// (TestReplayUsesLoaderPool pins the Found arm.)
func TestNamedSourcePin_LoaderPositionSourceStaysScoped(t *testing.T) {
	t.Parallel()
	db := namedSourceFixture()
	posID := int64(51)
	db.addNode(&nodes.Node{ID: posID, Name: "FG-HOME-01", Enabled: true})
	db.addDedicatedLoader(2, posID, "PART-A")
	res := NewSourceFinder(db, nil, nil).FindSource(u1Order("FG-HOME-01"), IntentFull)
	if res.Outcome != OutcomeWait || res.QueueCause != CauseFinderPoolEmpty {
		t.Fatalf("loader-position source: outcome=%v cause=%q, want Wait %q", res.Outcome, res.QueueCause, CauseFinderPoolEmpty)
	}
	if db.fifoCalls != 0 {
		t.Errorf("FindSourceBinFIFO calls = %d, want 0 — a loader-position source never widens", db.fifoCalls)
	}
}

// ── READ COUNTS ─────────────────────────────────────────────────────────────
//
// countingFinderDB counts the FinderDB reads a full-intent cascade can make,
// so a path's cost is a pinned number. The resolver's own reads for a LANE
// source are pinned in binresolver (TestResolveRetrieveInLane_ReadsNoMoreThanTheGroupPath).
type countingFinderDB struct {
	*fakeFinderDB
	reads int
}

func (c *countingFinderDB) GetNodeByDotName(n string) (*nodes.Node, error) {
	c.reads++
	return c.fakeFinderDB.GetNodeByDotName(n)
}

func (c *countingFinderDB) GetNode(id int64) (*nodes.Node, error) {
	c.reads++
	return c.fakeFinderDB.GetNode(id)
}

func (c *countingFinderDB) ListBinsByNode(id int64) ([]*bins.Bin, error) {
	c.reads++
	return c.fakeFinderDB.ListBinsByNode(id)
}

func (c *countingFinderDB) ListBinsByNodes(ids []int64) ([]*bins.Bin, error) {
	c.reads++
	return c.fakeFinderDB.ListBinsByNodes(ids)
}

func (c *countingFinderDB) FindSourceBinFIFO(p string, x int64) (*bins.Bin, error) {
	c.reads++
	return c.fakeFinderDB.FindSourceBinFIFO(p, x)
}

func (c *countingFinderDB) LoadBinTypeRule(p string) (domain.BinTypeRule, error) {
	c.reads++
	return c.fakeFinderDB.LoadBinTypeRule(p)
}

func (c *countingFinderDB) GetLoaderHomeByPositionNode(id int64) (*loaders.Home, error) {
	c.reads++
	return c.fakeFinderDB.GetLoaderHomeByPositionNode(id)
}

func (c *countingFinderDB) GetLoader(id int64) (*loaders.Loader, error) {
	c.reads++
	return c.fakeFinderDB.GetLoader(id)
}

func (c *countingFinderDB) ListLoaderHomes(id int64) ([]loaders.Home, error) {
	c.reads++
	return c.fakeFinderDB.ListLoaderHomes(id)
}

func (c *countingFinderDB) MaintainedGroupsSupporting(p string) ([]int64, error) {
	c.reads++
	return c.fakeFinderDB.MaintainedGroupsSupporting(p)
}

func (c *countingFinderDB) NodeIsUnderAny(id int64, roots []int64) (bool, error) {
	c.reads++
	return c.fakeFinderDB.NodeIsUnderAny(id, roots)
}

func (c *countingFinderDB) IsSlotAccessible(id int64) (bool, error) {
	c.reads++
	return c.fakeFinderDB.IsSlotAccessible(id)
}

// The NGRP-sourced drain-window need asks the drain-window question ONCE. At
// 1bb689bc it asked at tier 1 (the filter) and again at the seatbelt, three
// reads each; the count below is 3 lower than at 1bb689bc.
func TestFinderReads_NGRPDrainWindowAsksOnce(t *testing.T) {
	t.Parallel()
	c := &countingFinderDB{fakeFinderDB: namedSourceFixture()}
	slot := &nodes.Node{ID: 41, Name: "FG-BUFFER-01", Enabled: true}
	c.addNode(slot)
	r := &filterHonouringResolver{cands: []*ResolveResult{{
		Bin: &bins.Bin{ID: 34, PayloadCode: "PART-A", UOPRemaining: 100, UOPCapacity: 100, NodeID: &slot.ID}, Node: slot}}}
	res := NewSourceFinder(c, r, nil).FindSource(u1Order("FG-BUFFER"), IntentFull)
	if res.Outcome != OutcomeFound || res.Bin == nil || res.Bin.ID != 34 {
		t.Fatalf("outcome=%v bin=%v, want the group's full 34", res.Outcome, res.Bin)
	}
	t.Logf("NGRP drain-window reads = %d", c.reads)
	if c.reads != wantNGRPReads {
		t.Errorf("reads = %d, want %d", c.reads, wantNGRPReads)
	}
}

// A concrete-sourced full reads what the plant-wide path it replaces read: the
// node's candidate list and the carrier rule stand in for the FIFO scan and the
// found bin's node re-read.
func TestFinderReads_ConcreteSourceUnchanged(t *testing.T) {
	t.Parallel()
	c := &countingFinderDB{fakeFinderDB: namedSourceFixture()}
	dropNodeWith(c.fakeFinderDB, &bins.Bin{ID: 621, PayloadCode: "PART-A", UOPRemaining: 100, UOPCapacity: 100})
	res := NewSourceFinder(c, nil, nil).FindSource(u1Order("FG-DROP-01"), IntentFull)
	t.Logf("concrete reads = %d (outcome %v bin %v)", c.reads, res.Outcome, res.Bin)
	if res.Outcome != OutcomeFound || res.Bin == nil {
		t.Fatalf("outcome=%v bin=%v, want a full", res.Outcome, res.Bin)
	}
	if c.reads != wantConcreteReads {
		t.Errorf("reads = %d, want %d", c.reads, wantConcreteReads)
	}
}

// Measured at 1bb689bc by running these two tests against that tree's finder:
// NGRP 9 (the drain-window question asked twice, 3 reads each), concrete 9 (a
// FIFO scan and the found bin's node in place of the candidate list and the
// carrier rule). The concrete count is 8 since tier 2 stopped looking the
// source name up a second time (the node resolved at the top is passed in).
const (
	wantNGRPReads     = 6
	wantConcreteReads = 8
)
