package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
)

// demand_level_produce_early_test.go — A PRESS MUST BE ABLE TO ASK FOR ITS
// EMPTY BEFORE THE BIN IS FULL.
//
// A produce cell is judged at capacity and nowhere else: evaluateProduceLevel
// reads UOPCapacity and ignores reorder_point entirely. So the earliest moment
// a press can ask for its empty carrier is the moment the bin it is filling has
// no room left — and then it waits out the whole delivery leg holding a full
// bin it cannot add to. Hopkinsville totes to a press run a p50 of 1135 s on
// that leg.
//
// reorder_point is the field that says "ask this early", it is already on the
// claim, it is already offered by the claim editor on a produce row, and the
// produce path never read it. openCellEpisode meanwhile stamped it as the
// episode's threshold for BOTH roles, so the record already named a number the
// decision had never looked at.
//
// These tests state the behaviour the level ought to have. They are RED on the
// tree as written; the commit after this one is what makes them green. Their
// consume siblings and the produce-with-no-reorder-point case are in
// demand_level_role_test.go and are green throughout — nothing here asks for a
// change to a claim configured the way every deployed one is.

// THE DECISION EDGE. Capacity 100, reorder point 80: the press asks at 80.
func TestProduceEarlyAsk_BreachesAtTheReorderPointNotAtCapacity(t *testing.T) {
	t.Parallel()
	eng, _, _, claim := demandLevelFixture(t, "EARLY", protocol.ClaimRoleProduce, 100, 80)

	if breached, _ := eng.evaluateProduceLevel(claim, 79); breached {
		t.Error("79 against a reorder point of 80 is not yet the level")
	}
	if breached, _ := eng.evaluateProduceLevel(claim, 80); !breached {
		t.Error("a produce claim with reorder point 80 on a bin that holds 100 must breach at 80. " +
			"Judged at capacity it cannot ask until 100, which is the press asking for its empty " +
			"at the one moment it can no longer use the bin it has, and then waiting out the whole " +
			"delivery leg holding it")
	}
}

// THE RELIEF EDGE IS THE MIRROR, and the margin is taken from the level that
// was breached — not from capacity.
//
// Stated separately because a change that fixes the breach and leaves the
// relief reading capacity gives a band that straddles the wrong number: the
// episode would open at 80 and refuse to close until the count fell below 90,
// which is above the level it opened at. The band has to sit under the level.
func TestProduceEarlyAsk_RelievesBelowTheReorderPointMinusItsOwnMargin(t *testing.T) {
	t.Parallel()
	eng, _, _, claim := demandLevelFixture(t, "EARLYREL", protocol.ClaimRoleProduce, 100, 80)

	if breached, _ := eng.evaluateProduceLevel(claim, 80); !breached {
		t.Fatal("the claim must breach at 80 before its relief edge can be asked about")
	}

	margin := eng.cfg.HysteresisMargin(80)
	if _, shouldClose := eng.evaluateProduceLevel(claim, 80-margin); shouldClose {
		t.Errorf("relieved at exactly level-margin (%d) — that point is inside the band", 80-margin)
	}
	if _, shouldClose := eng.evaluateProduceLevel(claim, 80-margin-1); !shouldClose {
		t.Errorf("not relieved at %d, which is clear of level 80 minus its own margin %d. A band "+
			"measured off capacity instead would hold the episode open until the count fell below "+
			"%d, above the level the episode opened at", 80-margin-1, margin,
			100-eng.cfg.HysteresisMargin(100))
	}
}

// AND THE SWEEP HAS TO ASK. The evaluator stamping a falling edge is the
// record; the order is the behaviour the press feels.
//
// sweepNodeLevel recomputes `breached` inline rather than taking the
// evaluator's return, because inside the hysteresis band the evaluator's first
// value answers "is this episode still running" and not "should we order". That
// recompute is a second derivation of the level and it reads UOPCapacity
// directly, so a produce claim can breach in the evaluator and still not be
// asked for.
func TestProduceEarlyAsk_SweepPlacesTheOrderAtTheReorderPoint(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	f := seedWalkerProcess(t, db, "EARLYSWEEP", []walkerNode{
		{Suffix: "EARLY", Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeSingleRobot,
			PayloadCode: "SYN-PART-E", Capacity: 100, ReorderPoint: 80, AutoReorder: true,
			Remaining: 80},
		{Suffix: "BELOW", Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeSingleRobot,
			PayloadCode: "SYN-PART-F", Capacity: 100, ReorderPoint: 80, AutoReorder: true,
			Remaining: 79},
	})
	eng := testEngine(t, db)
	eng.coreClient = NewCoreClient(headOccupancyStub(t, false).URL)

	proc, err := db.GetProcess(f.ProcessID)
	testutil.MustNoErr(t, err, "get process")
	eng.sweepProcessLevels(proc)

	assertSet(t, "nodes asked for", askedNodes(t, db, f), []string{"EARLY"})
	assertSet(t, "claims stamped with a falling edge", stampedClaims(t, db, f), []string{"EARLY"})
}

// THE EPISODE RECORDS THE LEVEL THE DECISION USED. Capacity 6, no reorder
// point: the cell was judged at 6 and the row must say 6.
//
// It said 0, because openCellEpisode stamped claim.ReorderPoint for both roles
// while the produce evaluator judged against capacity. A produce episode
// therefore reported a threshold nothing had ever compared anything to, and on
// the ordinary produce claim — the one with no reorder point, which is every
// deployed one — that threshold was literally zero.
func TestProduceEarlyAsk_EpisodeRecordsCapacityWhenThereIsNoReorderPoint(t *testing.T) {
	t.Parallel()
	eng, db, procID, claim := demandLevelFixture(t, "RECCAP", protocol.ClaimRoleProduce, 6, 0)

	if _, _, err := eng.openCellEpisode(
		procID, claim, protocol.EpisodeTriggerAutoreorder, 1, 6, false,
	); err != nil {
		t.Fatalf("open the produce cell's episode: %v", err)
	}
	if got := episodeThreshold(t, db, "RECCAP-PROC", "RECCAP-PART", protocol.ClaimRoleProduce); got != 6 {
		t.Errorf("episode threshold = %d, want 6 — this cell was judged full at 6 and the record "+
			"must name that number. A stored 0 reads as a threshold of zero, which is a statement "+
			"about the level that nobody made", got)
	}
}

// AND AN UNREACHABLE REORDER POINT IS RECORDED AS WHAT IT DECIDED AT.
//
// 120 on a bin that holds 100 can never be crossed, so the decision falls back
// to capacity — and the episode has to say 100, not 120. Stamping the
// configured-but-unreachable number would put the two back into disagreement in
// the one case where the configuration is wrong and the row is the only place
// anybody would notice.
func TestProduceEarlyAsk_EpisodeRecordsCapacityWhenTheReorderPointIsUnreachable(t *testing.T) {
	t.Parallel()
	eng, db, procID, claim := demandLevelFixture(t, "RECOVER", protocol.ClaimRoleProduce, 100, 120)

	if _, _, err := eng.openCellEpisode(
		procID, claim, protocol.EpisodeTriggerAutoreorder, 1, 100, false,
	); err != nil {
		t.Fatalf("open the produce cell's episode: %v", err)
	}
	if got := episodeThreshold(t, db, "RECOVER-PROC", "RECOVER-PART", protocol.ClaimRoleProduce); got != 100 {
		t.Errorf("episode threshold = %d, want 100 — a reorder point of 120 on a bin that holds "+
			"100 is not the level anything was judged against", got)
	}
}

// THE CONSUME SIDE IS UNTOUCHED BY ALL OF IT. Same capacity and same reorder
// point, the other role: judged at 80 before and after, recorded as 80 before
// and after.
//
// Green on both trees. It is here rather than only in the characterisation file
// because a role-aware level is the kind of change that fixes one role by
// moving the other, and the consume half is where every deployed reorder point
// actually lives.
func TestProduceEarlyAsk_ConsumeIsUnmoved(t *testing.T) {
	t.Parallel()
	eng, db, procID, claim := demandLevelFixture(t, "UNMOVED", protocol.ClaimRoleConsume, 100, 80)

	if below, _ := eng.evaluateCellLevel(claim, 81); below {
		t.Error("81 against a consume reorder point of 80 is not below")
	}
	if below, _ := eng.evaluateCellLevel(claim, 80); !below {
		t.Error("80 against a consume reorder point of 80 is the level")
	}
	if _, _, err := eng.openCellEpisode(
		procID, claim, protocol.EpisodeTriggerAutoreorder, 1, 80, false,
	); err != nil {
		t.Fatalf("open the consume cell's episode: %v", err)
	}
	if got := episodeThreshold(t, db, "UNMOVED-PROC", "UNMOVED-PART", protocol.ClaimRoleConsume); got != 80 {
		t.Errorf("consume episode threshold = %d, want 80", got)
	}
}
