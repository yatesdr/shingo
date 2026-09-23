package binresolver

import (
	"errors"
	"testing"
	"time"

	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/reservations"
)

// A LANE NAMED AS A RETRIEVE SOURCE resolves inside that lane, through the
// group scan's own ranking, buried check and dig lock
// (GroupResolver.ResolveRetrieveInLane). It used to reach resolveRetrieve,
// which is neither FIFO nor burial-aware, and no production caller could get
// there; the finder now sends LANE sources here.

// countingStore counts every Store read the resolver makes, so the cost of a
// path is a pinned number rather than a claim in a comment.
type countingStore struct {
	*fakeStore
	reads int
}

func (c *countingStore) ListChildNodes(id int64) ([]*nodes.Node, error) {
	c.reads++
	return c.fakeStore.ListChildNodes(id)
}

func (c *countingStore) ListChildNodesUnlocked(id int64, a reservations.DigAsker) ([]*nodes.Node, error) {
	c.reads++
	return c.fakeStore.ListChildNodesUnlocked(id, a)
}

func (c *countingStore) GetNode(id int64) (*nodes.Node, error) {
	c.reads++
	return c.fakeStore.GetNode(id)
}

func (c *countingStore) GetNodeProperty(id int64, key string) string {
	c.reads++
	return c.fakeStore.GetNodeProperty(id, key)
}

func (c *countingStore) FindSourceBinInLane(id int64, p string) (*bins.Bin, error) {
	c.reads++
	return c.fakeStore.FindSourceBinInLane(id, p)
}

func (c *countingStore) FindOldestBuriedBin(id int64, p string) (*bins.Bin, *nodes.Node, error) {
	c.reads++
	return c.fakeStore.FindOldestBuriedBin(id, p)
}

// laneFixture is NGRP 1 with one lane 10, whose mouth slot 11 holds mouth and
// whose slot 12 holds buried (either may be nil).
func laneFixture(mouth, buried *bins.Bin) (*countingStore, *nodes.Node, *nodes.Node) {
	f := newFakeStore()
	grp := ngrpNode(1, "FG-SM")
	lane := laneChild(10, "FG-SM-L1")
	lane.ParentID = &grp.ID
	mouthSlot := &nodes.Node{ID: 11, Name: "FG-SM-L1-S1", Enabled: true, ParentID: &lane.ID}
	deepSlot := &nodes.Node{ID: 12, Name: "FG-SM-L1-S2", Enabled: true, ParentID: &lane.ID}
	for _, n := range []*nodes.Node{grp, lane, mouthSlot, deepSlot} {
		f.nodes[n.ID] = n
	}
	f.children[grp.ID] = []*nodes.Node{lane}
	f.children[lane.ID] = []*nodes.Node{mouthSlot, deepSlot}
	if mouth != nil {
		attachSlot(mouth, mouthSlot)
		f.sourceInLane[lane.ID] = mouth
	}
	if buried != nil {
		attachSlot(buried, deepSlot)
		f.oldestBuried[lane.ID] = laneBuried{bin: buried, slot: deepSlot}
	}
	return &countingStore{fakeStore: f}, grp, lane
}

func laneBin(id int64, uop int, age time.Duration) *bins.Bin {
	at := time.Now().Add(-age)
	return &bins.Bin{ID: id, Status: "available", ManifestConfirmed: true, PayloadCode: "PART-A",
		UOPRemaining: uop, UOPCapacity: 100, LoadedAt: &at}
}

func fullOnly(b *bins.Bin) bool { return b.UOPRemaining >= b.UOPCapacity }

func TestResolveRetrieveInLane_TakesTheMouth(t *testing.T) {
	t.Parallel()
	c, _, lane := laneFixture(laneBin(501, 100, time.Hour), nil)
	res, err := (&DefaultResolver{DB: c}).Resolve(lane, ResolveModeRetrieve, "PART-A", NoBinType, reservations.Anyone, nil)
	if err != nil || res == nil || res.Bin == nil || res.Bin.ID != 501 || res.Node == nil || res.Node.ID != 11 {
		t.Fatalf("res=%v err=%v, want the mouth bin 501 at slot 11", res, err)
	}
}

// The lookpast: a partial at the mouth, an older full behind it, and a caller
// that can only use fulls. The partial loses the comparison and the full is
// dug for, instead of the need waiting forever on the mouth.
func TestResolveRetrieveInLane_DigsForAFullBehindAPartial(t *testing.T) {
	t.Parallel()
	c, _, lane := laneFixture(laneBin(501, 40, time.Hour), laneBin(502, 100, 2*time.Hour))
	_, err := (&DefaultResolver{DB: c}).Resolve(lane, ResolveModeRetrieve, "PART-A", NoBinType, reservations.Anyone, fullOnly)
	var bErr *BuriedError
	if !errors.As(err, &bErr) || bErr.Bin == nil || bErr.Bin.ID != 502 || bErr.LaneID != 10 {
		t.Fatalf("err=%v, want a BuriedError for the full 502 in lane 10", err)
	}
}

// Nothing in the lane: a plain error, never structural — the lane may simply be
// empty now, and the finder queues scoped on it.
func TestResolveRetrieveInLane_EmptyLaneWaits(t *testing.T) {
	t.Parallel()
	c, _, lane := laneFixture(nil, nil)
	_, err := (&DefaultResolver{DB: c}).Resolve(lane, ResolveModeRetrieve, "PART-A", NoBinType, reservations.Anyone, nil)
	var sErr *StructuralError
	if err == nil || errors.As(err, &sErr) {
		t.Fatalf("err=%v, want a plain (non-structural) error", err)
	}
}

// A lane another order is digging is not searched, exactly as the group scan
// drops it from its candidates.
func TestResolveRetrieveInLane_DigHeldLaneIsNotSearched(t *testing.T) {
	t.Parallel()
	c, _, lane := laneFixture(laneBin(501, 100, time.Hour), nil)
	c.lockLaneForDig(lane.ID)
	res, err := (&DefaultResolver{DB: c}).Resolve(lane, ResolveModeRetrieve, "PART-A", NoBinType, reservations.Anyone, nil)
	if err == nil {
		t.Fatalf("res=%v, want the dig-held lane refused", res)
	}
}

// THE READ COUNT, pinned. A lane source costs six reads under the FIFO
// strategy: the two node-property reads getGroupAlgorithm makes (asrs_enabled,
// retrieve_algorithm), the parent's unlocked children (the dig lock), the mouth
// bin, its slot, the oldest buried bin. The same lane as the only child of an
// NGRP costs seven: the group path also lists the group's children in
// DefaultResolver.Resolve. So a LANE source reads no more than the node-group
// path it now matches, and nothing here is an Edge round trip.
func TestResolveRetrieveInLane_ReadsNoMoreThanTheGroupPath(t *testing.T) {
	t.Parallel()
	c, grp, lane := laneFixture(laneBin(501, 100, time.Hour), nil)
	r := &DefaultResolver{DB: c}

	if _, err := r.Resolve(lane, ResolveModeRetrieve, "PART-A", NoBinType, reservations.Anyone, nil); err != nil {
		t.Fatalf("lane resolve: %v", err)
	}
	laneReads := c.reads

	c.reads = 0
	if _, err := r.Resolve(grp, ResolveModeRetrieve, "PART-A", NoBinType, reservations.Anyone, nil); err != nil {
		t.Fatalf("group resolve: %v", err)
	}
	groupReads := c.reads

	if laneReads != 6 {
		t.Errorf("LANE source reads = %d, want 6", laneReads)
	}
	if groupReads != 7 {
		t.Errorf("one-lane NGRP reads = %d, want 7", groupReads)
	}
}
