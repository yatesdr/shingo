//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/payloads"
)

// depthlessLaneFixture builds a lane whose slots are depth 1 and depth 2, plus a
// THIRD child that has no depth at all and holds a bin.
//
// plantspec rejects Depth <= 0 for a spec'd slot, so this geometry should not
// exist — but nothing in the runtime path prevents it (ListLaneSlots filters
// nothing), and the two readings of it did not agree. AuditLaneDepths exists to
// make it loud at boot; this fixture is what makes it loud in a test.
func depthlessLaneFixture(t *testing.T, db *store.DB, prefix string) (lane, shallow, target, depthless *nodes.Node, bp *payloads.Payload) {
	t.Helper()
	grpType, err := db.GetNodeTypeByCode("NGRP")
	testutil.MustNoErr(t, err, "get NGRP type")
	lanType, err := db.GetNodeTypeByCode("LANE")
	testutil.MustNoErr(t, err, "get LANE type")

	bp = &payloads.Payload{Code: prefix + "-P"}
	testutil.MustNoErr(t, db.CreatePayload(bp), "create payload")

	grp := &nodes.Node{Name: prefix + "-GRP", NodeTypeID: &grpType.ID, Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(grp), "create group")

	lane = &nodes.Node{Name: prefix + "-LANE", NodeTypeID: &lanType.ID, ParentID: &grp.ID, Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(lane), "create lane")
	lane, err = db.GetNode(lane.ID)
	testutil.MustNoErr(t, err, "reload lane")

	d1, d2 := 1, 2
	shallow = &nodes.Node{Name: prefix + "-S1", ParentID: &lane.ID, Enabled: true, Depth: &d1}
	testutil.MustNoErr(t, db.CreateNode(shallow), "create depth-1 slot")
	target = &nodes.Node{Name: prefix + "-S2", ParentID: &lane.ID, Enabled: true, Depth: &d2}
	testutil.MustNoErr(t, db.CreateNode(target), "create depth-2 slot")

	depthless = &nodes.Node{Name: prefix + "-X", ParentID: &lane.ID, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(depthless), "create depth-less child")
	createTestBinAtNode(t, db, bp.Code, depthless.ID, prefix+"-BIN-X")
	return
}

// TestReachability_NullDepthSiblingReadTheSameBothWays pins D1.
//
// "A slot is reachable iff no occupied slot sits strictly shallower in the same
// lane" had two implementations that answered differently on one lane. Every SQL
// spelling guarded `sib.depth IS NOT NULL` and so IGNORED a depth-less sibling.
// The Go loop did not: it read depths through GetSlotDepth, which reports 0 for
// NULL, and 0 is shallower than everything — so a depth-less occupied child was
// a blocker to it and invisible to the SQL.
//
// The consequence was not theoretical. On a lane holding one such child,
// IsSlotAccessible called every slot reachable while findBuriedBlockers reported
// a blocker; laneGateRetrieveCause then parked the retrieve as
// "lane-target-buried" and PlanReshuffle scheduled a move for a bin the SQL side
// did not think was in the way.
//
// SQL's reading is canonical — no depth means "not a depth-ordered slot, ignore
// it" — because it was the majority spelling and it already matched
// IsSlotAccessible's own Go guard. The assertion here is AGREEMENT, not a
// particular verdict: what was broken was that one sentence had two answers.
func TestReachability_NullDepthSiblingReadTheSameBothWays(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, _, target, depthless, _ := depthlessLaneFixture(t, db, "ND")

	accessible, err := db.IsSlotAccessible(target.ID)
	if err != nil {
		t.Fatalf("IsSlotAccessible: %v", err)
	}
	blockers, err := findBuriedBlockers(db, target.ID)
	if err != nil {
		t.Fatalf("findBuriedBlockers: %v", err)
	}

	if accessible != (len(blockers) == 0) {
		t.Fatalf("the two spellings disagree: IsSlotAccessible(%s)=%v but findBuriedBlockers returned %d blocker(s).\n"+
			"The depth-less child %s holds a bin. SQL ignores it (sib.depth IS NOT NULL); the Go loop used to see it "+
			"as depth 0 and therefore in front of everything. One sentence, two answers.",
			target.Name, accessible, len(blockers), depthless.Name)
	}
	if !accessible {
		t.Fatalf("IsSlotAccessible(%s) = false, want true — nothing DEPTH-ORDERED sits in front of the depth-2 slot; "+
			"the only occupied child of the lane is the depth-less %s, which is not a depth-ordered slot at all",
			target.Name, depthless.Name)
	}
}

// TestReachability_RealBlockerStillFound is the other half of D1's fixture: with
// the SAME depth-less occupant present, a genuine depth-1 blocker must still be
// reported. The ruling ignores depth-less siblings; it must not make the reader
// blind to the ones that count.
func TestReachability_RealBlockerStillFound(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, shallow, target, _, bp := depthlessLaneFixture(t, db, "NDB")
	createTestBinAtNode(t, db, bp.Code, shallow.ID, "NDB-BIN-S1")

	accessible, err := db.IsSlotAccessible(target.ID)
	if err != nil {
		t.Fatalf("IsSlotAccessible: %v", err)
	}
	blockers, err := findBuriedBlockers(db, target.ID)
	if err != nil {
		t.Fatalf("findBuriedBlockers: %v", err)
	}

	if accessible {
		t.Fatalf("IsSlotAccessible(%s) = true, want false — %s is occupied and sits in front of it", target.Name, shallow.Name)
	}
	if len(blockers) != 1 {
		t.Fatalf("findBuriedBlockers returned %d blocker(s), want exactly 1 (%s) — the depth-less child must not be counted",
			len(blockers), shallow.Name)
	}
	if blockers[0].slot.ID != shallow.ID {
		t.Fatalf("blocker slot = %s, want %s", blockers[0].slot.Name, shallow.Name)
	}
}
