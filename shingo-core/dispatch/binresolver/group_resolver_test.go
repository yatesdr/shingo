package binresolver

import (
	"errors"
	"testing"
	"time"

	"shingocore/store"
)

// --- FIFO ------------------------------------------------------------------

// FIFO picks the globally oldest accessible bin across all lanes.
func TestFIFO_PicksOldestAcrossLanes(t *testing.T) {
	f := newFakeStore()
	group := ngrpNode(1, "grp")

	laneA := laneChild(10, "lane-A")
	laneB := laneChild(11, "lane-B")
	f.children[group.ID] = []*store.Node{laneA, laneB}

	slotA := slotInLane(100, "A1")
	slotB := slotInLane(101, "B1")
	f.nodes[slotA.ID] = slotA
	f.nodes[slotB.ID] = slotB

	now := time.Now()
	// Lane-B has the older bin.
	binA := availBin(1000, "P1", now)
	binB := availBin(1001, "P1", now.Add(-1*time.Hour))
	attachSlot(binA, slotA)
	attachSlot(binB, slotB)
	f.sourceInLane[laneA.ID] = binA
	f.sourceInLane[laneB.ID] = binB

	gr := &GroupResolver{DB: f, LaneLock: NewLaneLock()}
	got, err := gr.ResolveRetrieve(group, "P1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Bin != binB || got.Node != slotB {
		t.Fatalf("FIFO should pick lane-B's older bin; got bin=%d node=%s", got.Bin.ID, got.Node.Name)
	}
}

// FIFO returns BuriedError when a buried bin is older than any accessible bin.
func TestFIFO_BuriedOlderThanAccessibleTriggersReshuffle(t *testing.T) {
	f := newFakeStore()
	group := ngrpNode(1, "grp")
	lane := laneChild(10, "L")
	f.children[group.ID] = []*store.Node{lane}

	accSlot := slotInLane(100, "acc")
	buriedSlot := slotInLane(101, "buried")
	f.nodes[accSlot.ID] = accSlot
	f.nodes[buriedSlot.ID] = buriedSlot

	now := time.Now()
	// Accessible bin is 10 minutes old; buried bin is 2 hours old.
	acc := availBin(1000, "P1", now.Add(-10*time.Minute))
	attachSlot(acc, accSlot)
	buried := availBin(1001, "P1", now.Add(-2*time.Hour))
	attachSlot(buried, buriedSlot)

	f.sourceInLane[lane.ID] = acc
	f.oldestBuried[lane.ID] = laneBuried{bin: buried, slot: buriedSlot}

	gr := &GroupResolver{DB: f, LaneLock: NewLaneLock()}
	_, err := gr.ResolveRetrieve(group, "P1")
	var bErr *BuriedError
	if !errors.As(err, &bErr) {
		t.Fatalf("expected *BuriedError, got %T: %v", err, err)
	}
	if bErr.Bin != buried || bErr.LaneID != lane.ID {
		t.Fatalf("BuriedError points to wrong bin/lane: %+v", bErr)
	}
}

// FIFO skips locked lanes in phase 1 and phase 2 — the burial check
// must not fire for a locked lane, otherwise a second concurrent order
// would steal a bin mid-reshuffle.
func TestFIFO_SkipsLockedLanes(t *testing.T) {
	f := newFakeStore()
	group := ngrpNode(1, "grp")
	locked := laneChild(10, "locked")
	open := laneChild(11, "open")
	f.children[group.ID] = []*store.Node{locked, open}

	slot := slotInLane(100, "slot")
	f.nodes[slot.ID] = slot

	now := time.Now()
	// The locked lane holds the older bin; with the lock in place it
	// must be ignored entirely.
	olderInLocked := availBin(1000, "P1", now.Add(-1*time.Hour))
	attachSlot(olderInLocked, slotInLane(999, "locked-slot"))
	f.sourceInLane[locked.ID] = olderInLocked

	newerInOpen := availBin(1001, "P1", now)
	attachSlot(newerInOpen, slot)
	f.sourceInLane[open.ID] = newerInOpen

	ll := NewLaneLock()
	ll.TryLock(locked.ID, 999)

	gr := &GroupResolver{DB: f, LaneLock: ll}
	got, err := gr.ResolveRetrieve(group, "P1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Bin != newerInOpen {
		t.Fatal("FIFO must skip locked lane even when it holds the older bin")
	}
}

// --- COST ------------------------------------------------------------------

// COST only triggers a reshuffle when NO accessible bin exists.
func TestCOST_PrefersAccessibleOverOlderBuried(t *testing.T) {
	f := newFakeStore()
	group := ngrpNode(1, "grp")
	f.setProp(group.ID, "retrieve_algorithm", RetrieveCOST)

	lane := laneChild(10, "L")
	f.children[group.ID] = []*store.Node{lane}

	accSlot := slotInLane(100, "acc")
	buriedSlot := slotInLane(101, "buried")
	f.nodes[accSlot.ID] = accSlot
	f.nodes[buriedSlot.ID] = buriedSlot

	now := time.Now()
	acc := availBin(1000, "P1", now.Add(-10*time.Minute))
	attachSlot(acc, accSlot)
	buried := availBin(1001, "P1", now.Add(-5*time.Hour))
	attachSlot(buried, buriedSlot)

	f.sourceInLane[lane.ID] = acc
	// Burial fixture exists, but COST should not even look at it when
	// an accessible bin is present.
	f.buriedAny[lane.ID] = laneBuried{bin: buried, slot: buriedSlot}

	gr := &GroupResolver{DB: f, LaneLock: NewLaneLock()}
	got, err := gr.ResolveRetrieve(group, "P1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Bin != acc {
		t.Fatalf("COST must prefer accessible bin; got bin %d", got.Bin.ID)
	}
}

// COST falls back to BuriedError only when no accessible bin is found.
func TestCOST_FallsBackToBuriedWhenNoAccessible(t *testing.T) {
	f := newFakeStore()
	group := ngrpNode(1, "grp")
	f.setProp(group.ID, "retrieve_algorithm", RetrieveCOST)

	lane := laneChild(10, "L")
	f.children[group.ID] = []*store.Node{lane}

	buriedSlot := slotInLane(100, "b")
	f.nodes[buriedSlot.ID] = buriedSlot
	buried := availBin(999, "P1", time.Now())
	f.buriedAny[lane.ID] = laneBuried{bin: buried, slot: buriedSlot}
	// No source bin in lane -> fake returns error, resolver treats as
	// "no accessible" and falls through to the burial scan.

	gr := &GroupResolver{DB: f, LaneLock: NewLaneLock()}
	_, err := gr.ResolveRetrieve(group, "P1")
	var bErr *BuriedError
	if !errors.As(err, &bErr) {
		t.Fatalf("expected *BuriedError, got %T: %v", err, err)
	}
	if bErr.Bin != buried {
		t.Fatalf("BuriedError does not reference expected bin: %+v", bErr)
	}
}

// --- FAVL ------------------------------------------------------------------

// FAVL returns the first available bin and never surfaces a BuriedError.
func TestFAVL_FirstAvailableNoReshuffle(t *testing.T) {
	f := newFakeStore()
	group := ngrpNode(1, "grp")
	f.setProp(group.ID, "retrieve_algorithm", RetrieveFAVL)

	laneA := laneChild(10, "A")
	laneB := laneChild(11, "B")
	f.children[group.ID] = []*store.Node{laneA, laneB}

	slotA := slotInLane(100, "a-slot")
	f.nodes[slotA.ID] = slotA

	now := time.Now()
	// A has the newer bin, B has an older bin. FAVL iterates in child
	// order, so it should return A's bin regardless of age.
	binA := availBin(1000, "P1", now)
	attachSlot(binA, slotA)
	binB := availBin(1001, "P1", now.Add(-10*time.Hour))

	f.sourceInLane[laneA.ID] = binA
	f.sourceInLane[laneB.ID] = binB
	// Burial fixture — FAVL must ignore it.
	f.oldestBuried[laneA.ID] = laneBuried{
		bin:  availBin(9999, "P1", now.Add(-48*time.Hour)),
		slot: slotInLane(999, "deep"),
	}

	gr := &GroupResolver{DB: f, LaneLock: NewLaneLock()}
	got, err := gr.ResolveRetrieve(group, "P1")
	// Any error here — BuriedError included — counts as failure: the
	// whole point of FAVL is to skip the burial-detection code path.
	if err != nil {
		t.Fatalf("FAVL must not surface any error (incl. BuriedError); got %T: %v", err, err)
	}
	if got.Bin != binA {
		t.Fatalf("FAVL should return first-iterated lane's bin; got %d", got.Bin.ID)
	}
}

// --- LKND ------------------------------------------------------------------

// LKND prefers a candidate that already holds a matching payload.
func TestLKND_PrefersConsolidationOverEmptier(t *testing.T) {
	f := newFakeStore()
	group := ngrpNode(1, "grp")
	// LKND is the default; setting it explicitly documents intent.
	f.setProp(group.ID, "store_algorithm", StoreLKND)

	empty := directChild(10, "empty")
	match := directChild(11, "match")
	f.children[group.ID] = []*store.Node{empty, match}
	f.binCounts[empty.ID] = 0
	f.binCounts[match.ID] = 0
	// 'match' already holds a matching bin — consolidation wins.
	f.bins[match.ID] = []*store.Bin{availBin(100, "P1", time.Now())}

	gr := &GroupResolver{DB: f, LaneLock: NewLaneLock()}
	got, err := gr.ResolveStore(group, "P1", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Node != match {
		t.Fatalf("LKND should prefer consolidation candidate, got %s", got.Node.Name)
	}
}

// LKND falls back to emptiest when no consolidation candidate exists.
func TestLKND_EmptiestWhenNoMatch(t *testing.T) {
	f := newFakeStore()
	group := ngrpNode(1, "grp")
	// Two lanes, different counts, neither holds a matching bin.
	laneA := laneChild(10, "A")
	laneB := laneChild(11, "B")
	f.children[group.ID] = []*store.Node{laneA, laneB}
	slotA := slotInLane(100, "a-open")
	slotB := slotInLane(101, "b-open")
	f.storeSlot[laneA.ID] = slotA
	f.storeSlot[laneB.ID] = slotB
	f.laneBinCounts[laneA.ID] = 3
	f.laneBinCounts[laneB.ID] = 1
	// No bins anywhere -> hasMatch stays false for both, emptier wins.

	gr := &GroupResolver{DB: f, LaneLock: NewLaneLock()}
	got, err := gr.ResolveStore(group, "P1", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Node != slotB {
		t.Fatalf("LKND tie-break must pick emptier lane; got %s", got.Node.Name)
	}
}

// LKND skips lanes whose effective payload set excludes the request.
func TestLKND_SkipsLaneWithPayloadMismatch(t *testing.T) {
	f := newFakeStore()
	group := ngrpNode(1, "grp")
	restricted := laneChild(10, "restricted")
	open := laneChild(11, "open")
	f.children[group.ID] = []*store.Node{restricted, open}
	f.storeSlot[restricted.ID] = slotInLane(100, "r-slot")
	f.storeSlot[open.ID] = slotInLane(101, "o-slot")
	f.laneBinCounts[restricted.ID] = 0
	f.laneBinCounts[open.ID] = 0

	// 'restricted' accepts only payload OTHER; the request is for P1.
	f.effPayloads[restricted.ID] = []*store.Payload{payload("OTHER")}

	gr := &GroupResolver{DB: f, LaneLock: NewLaneLock()}
	got, err := gr.ResolveStore(group, "P1", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Node.Name != "o-slot" {
		t.Fatalf("LKND should skip restricted lane; got %s", got.Node.Name)
	}
}

// LKND skips lanes/direct-children whose bin-type whitelist excludes the request.
func TestLKND_SkipsBinTypeMismatch(t *testing.T) {
	f := newFakeStore()
	group := ngrpNode(1, "grp")
	restricted := directChild(10, "wrong-type")
	open := directChild(11, "ok")
	f.children[group.ID] = []*store.Node{restricted, open}
	f.binCounts[restricted.ID] = 0
	f.binCounts[open.ID] = 0
	// Restricted only allows bin type 99; request is for type 7.
	f.effBinTypes[restricted.ID] = []*store.BinType{binType(99)}

	want := int64(7)
	gr := &GroupResolver{DB: f, LaneLock: NewLaneLock()}
	got, err := gr.ResolveStore(group, "", &want)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Node != open {
		t.Fatalf("LKND should skip bin-type-mismatched child; got %s", got.Node.Name)
	}
}

// --- DPTH ------------------------------------------------------------------

// DPTH prefers lanes over direct children, even when the direct child
// has room.
func TestDPTH_LanesBeatDirectChildren(t *testing.T) {
	f := newFakeStore()
	group := ngrpNode(1, "grp")
	f.setProp(group.ID, "store_algorithm", StoreDPTH)

	direct := directChild(10, "direct")
	lane := laneChild(11, "lane")
	f.children[group.ID] = []*store.Node{direct, lane}
	f.binCounts[direct.ID] = 0
	laneSlot := slotInLane(100, "lane-slot")
	f.storeSlot[lane.ID] = laneSlot

	gr := &GroupResolver{DB: f, LaneLock: NewLaneLock()}
	got, err := gr.ResolveStore(group, "P1", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Node != laneSlot {
		t.Fatalf("DPTH should pick the lane slot, got %s", got.Node.Name)
	}
}

// DPTH falls back to a direct child when every lane is full.
func TestDPTH_FallsBackToDirectChild(t *testing.T) {
	f := newFakeStore()
	group := ngrpNode(1, "grp")
	f.setProp(group.ID, "store_algorithm", StoreDPTH)

	lane := laneChild(10, "full-lane")
	direct := directChild(11, "direct")
	f.children[group.ID] = []*store.Node{lane, direct}
	// No storeSlot fixture for 'full-lane' -> FindStoreSlotInLane errors.
	f.binCounts[direct.ID] = 0

	gr := &GroupResolver{DB: f, LaneLock: NewLaneLock()}
	got, err := gr.ResolveStore(group, "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Node != direct {
		t.Fatalf("DPTH should fall back to direct child; got %s", got.Node.Name)
	}
}

// --- classifyEmptyGroup ----------------------------------------------------

// An NGRP with only disabled children returns StructuralError.
func TestClassifyEmpty_NoEnabledChildren_Structural(t *testing.T) {
	f := newFakeStore()
	group := ngrpNode(1, "grp")
	f.children[group.ID] = []*store.Node{disabledChild(10, "off")}

	gr := &GroupResolver{DB: f, LaneLock: NewLaneLock()}
	_, err := gr.ResolveRetrieve(group, "P1")
	var sErr *StructuralError
	if !errors.As(err, &sErr) {
		t.Fatalf("expected *StructuralError, got %T: %v", err, err)
	}
	if sErr.Reason == "" {
		t.Fatal("StructuralError must carry a reason string")
	}
}

// An NGRP whose enabled children all restrict payloads away from the
// request returns StructuralError — the group cannot satisfy this
// payload regardless of inventory.
func TestClassifyEmpty_NoChildAcceptsPayload_Structural(t *testing.T) {
	f := newFakeStore()
	group := ngrpNode(1, "grp")
	child := directChild(10, "fixed")
	f.children[group.ID] = []*store.Node{child}
	// Child only accepts payload OTHER; requester asks for P1.
	f.effPayloads[child.ID] = []*store.Payload{payload("OTHER")}

	gr := &GroupResolver{DB: f, LaneLock: NewLaneLock()}
	_, err := gr.ResolveRetrieve(group, "P1")
	var sErr *StructuralError
	if !errors.As(err, &sErr) {
		t.Fatalf("expected *StructuralError, got %T: %v", err, err)
	}
}

// When children exist and accept the payload but no bins are present,
// the error must be transient — a buried bin is absent and a fresh
// delivery could make the group satisfiable.
func TestClassifyEmpty_Transient_WhenGroupStructurallyCapable(t *testing.T) {
	f := newFakeStore()
	group := ngrpNode(1, "grp")
	child := directChild(10, "cap")
	f.children[group.ID] = []*store.Node{child}
	// Empty effective payloads = "no restriction" -> capable.
	// No bins at the child -> nothing to retrieve.

	gr := &GroupResolver{DB: f, LaneLock: NewLaneLock()}
	_, err := gr.ResolveRetrieve(group, "P1")
	if err == nil {
		t.Fatal("expected transient error when no bin available")
	}
	var sErr *StructuralError
	if errors.As(err, &sErr) {
		t.Fatalf("capable-but-empty group must not return StructuralError, got: %v", err)
	}
	var bErr *BuriedError
	if errors.As(err, &bErr) {
		t.Fatalf("capable-but-empty group must not return BuriedError, got: %v", err)
	}
}
