//go:build docker

package dispatch

import (
	"errors"
	"fmt"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/payloads"
)

// dugLaneFixture builds the geometry that exposes D3:
//
//	GRP-<p>
//	└── LANE-<p>: S1 (depth 1, EMPTY) · S2 (depth 2, BLOCKER) · S3 (depth 3, TARGET)
//
// The dig wants S3 and must lift the blocker out of S2. S1 is empty AND
// reachable — nothing occupied sits shallower than it — so Pass 2 will happily
// offer it as a shuffle slot. It is the one slot in the plant that must not be
// offered: parking the blocker there moves it from depth 2 to depth 1, leaving
// it in front of the very target the dig is uncovering.
//
// The group has no direct children, so Pass 1 finds nothing and Pass 2 is
// reached — which is the pressure the brief asks for.
func dugLaneFixture(t *testing.T, db *store.DB, prefix string, extraLanes int) (grp, lane *nodes.Node, slots []*nodes.Node, bp *payloads.Payload) {
	t.Helper()
	grpType, err := db.GetNodeTypeByCode("NGRP")
	testutil.MustNoErr(t, err, "get NGRP type")
	lanType, err := db.GetNodeTypeByCode("LANE")
	testutil.MustNoErr(t, err, "get LANE type")

	bp = &payloads.Payload{Code: prefix + "-P"}
	testutil.MustNoErr(t, db.CreatePayload(bp), "create payload")

	grp = &nodes.Node{Name: prefix + "-GRP", NodeTypeID: &grpType.ID, Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(grp), "create group")

	mkLane := func(name string) (*nodes.Node, []*nodes.Node) {
		l := &nodes.Node{Name: name, NodeTypeID: &lanType.ID, ParentID: &grp.ID, Enabled: true, IsSynthetic: true}
		testutil.MustNoErr(t, db.CreateNode(l), "create "+name)
		var out []*nodes.Node
		for d := 1; d <= 3; d++ {
			depth := d
			s := &nodes.Node{Name: fmt.Sprintf("%s-S%d", name, d), ParentID: &l.ID, Enabled: true, Depth: &depth}
			testutil.MustNoErr(t, db.CreateNode(s), "create slot")
			out = append(out, s)
		}
		reloaded, _ := db.GetNode(l.ID)
		return reloaded, out
	}

	lane, slots = mkLane(prefix + "-LANE")
	createTestBinAtNode(t, db, bp.Code, slots[1].ID, prefix+"-BIN-BLOCKER") // depth 2
	createTestBinAtNode(t, db, bp.Code, slots[2].ID, prefix+"-BIN-TARGET")  // depth 3

	for i := range extraLanes {
		mkLane(fmt.Sprintf("%s-ALT%d", prefix, i+1))
	}

	grp, _ = db.GetNode(grp.ID)
	return
}

// TestFindShuffleSlots_MustNotParkInTheDugLane pins D3.
//
// findShuffleSlots takes laneID — the lane being dug — but Pass 2 never compared
// it against anything. The parameter was read once, at the top, to look up the
// per-lane property override it used to carry, and then the loop iterated EVERY
// LANE child of the group including the one the dig is emptying. So a blocker
// lifted out of lane L could be parked back into another slot of lane L.
//
// It survived because the dig holds the lane exclusively, so nothing else
// competes for those slots and the plan "works" in the sense that no two orders
// collide. It is still wrong on its own terms — here the blocker would move from
// depth 2 to depth 1, ending up in front of the target the dig exists to reach —
// and it stops being survivable at all when lane concurrency is relaxed, which
// is why it lands now rather than then.
func TestFindShuffleSlots_MustNotParkInTheDugLane(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	grp, lane, slots, _ := dugLaneFixture(t, db, "D3", 0)

	got, err := findShuffleSlots(db, lane.ID, grp.ID, 1)

	for _, s := range got {
		if s.ParentID != nil && *s.ParentID == lane.ID {
			t.Fatalf("findShuffleSlots offered %s, which is a slot of %s — the lane being dug.\n"+
				"The blocker at depth 2 would be parked at depth 1, in front of the depth-3 target the dig is "+
				"uncovering. laneID is a parameter of this function and Pass 2 never compared it to anything.",
				s.Name, lane.Name)
		}
	}
	if err == nil {
		t.Fatalf("want ErrNoShuffleSlot: the only free reachable slot in the plant is %s, inside the dug lane, "+
			"so there is nowhere to park — and nowhere-to-park is congestion that waits, not a plan that succeeds",
			slots[0].Name)
	}
	if !errors.Is(err, ErrNoShuffleSlot) {
		t.Fatalf("error = %v, want ErrNoShuffleSlot — a dig with nowhere to park must WAIT and retry, never fail "+
			"terminally (planBuriedReshuffle maps every other error here to the terminal codeReshuffle)", err)
	}
}

// TestFindShuffleSlots_StillParksInAnotherLane is the control. The dug-lane
// guard must exclude one lane, not disable Pass 2 — a blocker still parks in a
// sibling lane, which is the whole point of the pass.
func TestFindShuffleSlots_StillParksInAnotherLane(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	grp, lane, _, _ := dugLaneFixture(t, db, "D3ALT", 1)

	got, err := findShuffleSlots(db, lane.ID, grp.ID, 1)
	if err != nil {
		t.Fatalf("findShuffleSlots: %v — an empty sibling lane is a legitimate place to park a blocker", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d shuffle slots, want 1", len(got))
	}
	if got[0].ParentID == nil || *got[0].ParentID == lane.ID {
		t.Fatalf("shuffle slot = %s (parent %v), want a slot in the sibling lane", got[0].Name, got[0].ParentID)
	}
	// Deepest-first within the chosen lane: the sibling's depth-3 slot, not its
	// mouth. Filling shallow-first is what strands the deeper empties.
	if got[0].Depth == nil || *got[0].Depth != 3 {
		t.Errorf("shuffle slot depth = %v, want 3 — Pass 2 reverse-iterates so the DEEPEST empty is taken first",
			got[0].Depth)
	}
}
