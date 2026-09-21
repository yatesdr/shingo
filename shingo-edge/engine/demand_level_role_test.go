package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// demand_level_role_test.go — WHICH NUMBER EACH ROLE'S CLAIM IS JUDGED AGAINST.
//
// A claim carries two numbers that could serve as its demand level:
// reorder_point and the payload's UOP capacity. Which one applies is a property
// of the ROLE, and until this file nothing in the tree said so in one place:
// evaluateCellLevel read reorder_point, evaluateProduceLevel read capacity, and
// openCellEpisode stamped reorder_point onto the episode for BOTH. That is two
// derivations for one fact, and on a produce claim carrying a reorder point the
// record and the decision therefore named different numbers.
//
// These tests are the characterisation half: every case here holds on the tree
// as it stands and must keep holding afterwards. They are what proves a
// role-aware level does not move a claim that is configured the way every
// deployed claim is configured today — produce with reorder_point = 0 — because
// that is the state both plants are in and the state the change must not touch.
//
// The fixtures are synthetic. Payload codes and node names are shaped like the
// plant's but name nothing in one.

// demandLevelFixture seeds one cell of the given role and returns the claim as a
// tick reads it back — through the database, not as the input struct.
//
// THE READ-BACK IS THE POINT. uop_capacity is a dead column resolved from
// payload_catalog on every claim read (store/internal/capacity), so a fixture
// that asserted against its own input would be asserting about a number the
// engine never sees. upsertClaimRetiredMode writes the catalog row the resolve
// will find; the capacity check below fails loudly rather than letting a test
// about a full bin quietly become a test about an empty one.
func demandLevelFixture(
	t *testing.T, prefix string, role protocol.ClaimRole, capacity, reorderPoint int,
) (*Engine, *store.DB, int64, *processes.NodeClaim) {
	t.Helper()
	db := testEngineDB(t)
	eng := testEngine(t, db)

	procID, err := db.CreateProcess(prefix+"-PROC", "", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	node := prefix + "_CELL"
	_, err = db.CreateProcessNode(processes.NodeInput{
		ProcessID: procID, CoreNodeName: node, Code: "N01", Name: node, Sequence: 1, Enabled: true,
	})
	testutil.MustNoErr(t, err, "create node")
	styleID, err := db.CreateStyle(prefix+"-STYLE", "", procID)
	testutil.MustNoErr(t, err, "create style")
	testutil.MustNoErr(t, db.SetActiveStyle(procID, &styleID), "set active style")

	_, err = upsertClaimRetiredMode(db, processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: node, Role: role,
		SwapMode: protocol.SwapModeSingleRobot, PayloadCode: prefix + "-PART",
		UOPCapacity: capacity, ReorderPoint: reorderPoint, AutoReorder: domain.Ptr(true),
		InboundSource: "SYN_MARKET", OutboundDestination: "SYN_MARKET",
		InboundStaging: prefix + "_STG_IN", OutboundStaging: prefix + "_STG_OUT",
	})
	testutil.MustNoErr(t, err, "upsert claim")

	claim, err := db.GetStyleNodeClaimByNode(styleID, node)
	testutil.MustNoErr(t, err, "read the claim back")
	if claim == nil {
		t.Fatalf("no claim at %s after upserting one", node)
	}
	if claim.UOPCapacity != capacity {
		t.Fatalf("claim capacity read back as %d, want %d — the payload catalog row the resolve "+
			"reads was not seeded, so every level assertion below would be measured against the "+
			"wrong denominator", claim.UOPCapacity, capacity)
	}
	if claim.ReorderPoint != reorderPoint {
		t.Fatalf("claim reorder point read back as %d, want %d", claim.ReorderPoint, reorderPoint)
	}
	return eng, db, procID, claim
}

// episodeThreshold returns the threshold stamped on this cell's open episode.
func episodeThreshold(
	t *testing.T, db *store.DB, procName, payload string, role protocol.ClaimRole,
) int {
	t.Helper()
	open, err := db.GetOpenDemandOrigin(protocol.CellEpisodeKey(procName, payload, role))
	testutil.MustNoErr(t, err, "read the open episode")
	if open == nil {
		t.Fatalf("no open episode at %s|%s|%s", procName, payload, role)
	}
	return open.Threshold
}

// A CONSUME CLAIM IS JUDGED AT ITS REORDER POINT, and the episode records the
// same number the decision used.
//
// This is the half of the pair that already agreed, and it is pinned because a
// role-aware level is exactly the kind of change that fixes one role by moving
// the other. Every deployed consume claim that carries a reorder point decides
// here.
func TestDemandLevel_ConsumeIsJudgedAtItsReorderPoint(t *testing.T) {
	t.Parallel()
	eng, db, procID, claim := demandLevelFixture(t, "CONSRP", protocol.ClaimRoleConsume, 100, 80)

	if below, _ := eng.evaluateCellLevel(claim, 81); below {
		t.Error("81 remaining against a reorder point of 80 is not yet below the level")
	}
	if below, _ := eng.evaluateCellLevel(claim, 80); !below {
		t.Error("80 remaining against a reorder point of 80 IS the level — the predicate is " +
			"at-or-below, and a cell that asks one part late is a cell that runs dry")
	}

	if _, _, err := eng.openCellEpisode(
		procID, claim, protocol.EpisodeTriggerAutoreorder, 1, 80, false,
	); err != nil {
		t.Fatalf("open the consume cell's episode: %v", err)
	}
	if got := episodeThreshold(t, db, "CONSRP-PROC", "CONSRP-PART", protocol.ClaimRoleConsume); got != 80 {
		t.Errorf("episode threshold = %d, want 80 — the record must name the number the decision "+
			"was made against, or the episode surface cannot be read back against the config", got)
	}
}

// A CONSUME CLAIM WITH NO REORDER POINT IS THE DOCUMENTED OPT-OUT, and stays
// one.
//
// reorder_point = 0 is the legacy default on every claim nobody has edited. It
// means "this cell does not auto-order", not "this cell is out of parts at
// zero", and the guard that reads it that way is the reason a plant can roll
// the feature out cell by cell.
func TestDemandLevel_ConsumeWithNoReorderPointStillOptsOut(t *testing.T) {
	t.Parallel()
	eng, db, procID, claim := demandLevelFixture(t, "CONSOPT", protocol.ClaimRoleConsume, 100, 0)

	below, shouldClose := eng.evaluateCellLevel(claim, 0)
	if below || shouldClose {
		t.Errorf("an empty cell on an opted-out claim reported below=%v shouldClose=%v — a zero "+
			"reorder point is silence, not a level at zero", below, shouldClose)
	}
	if stamped, err := db.GetClaimBelowReorderSince(claim.ID); err != nil || stamped != nil {
		t.Errorf("below_reorder_since = %v (err %v) on an opted-out claim, want nothing stamped",
			stamped, err)
	}

	if _, _, err := eng.openCellEpisode(
		procID, claim, protocol.EpisodeTriggerOperator, 1, 0, false,
	); err != nil {
		t.Fatalf("open the consume cell's episode: %v", err)
	}
	if got := episodeThreshold(t, db, "CONSOPT-PROC", "CONSOPT-PART", protocol.ClaimRoleConsume); got != 0 {
		t.Errorf("episode threshold = %d, want 0 — an opted-out consume claim was judged against "+
			"no level at all, and the record says so", got)
	}
}

// A PRODUCE CLAIM WITH NO REORDER POINT IS JUDGED AT CAPACITY — the state every
// produce claim at both plants is in.
//
// THIS IS THE DEPLOY-SAFETY CASE. 0 of 8 Springfield and 0 of 24 Hopkinsville
// produce claims carry a reorder point, so this test is the whole of what those
// plants do on the day a role-aware level ships. Breach at capacity, relief
// below capacity minus the margin, nothing in between.
func TestDemandLevel_ProduceWithNoReorderPointIsJudgedAtCapacity(t *testing.T) {
	t.Parallel()
	eng, _, _, claim := demandLevelFixture(t, "PRODCAP", protocol.ClaimRoleProduce, 100, 0)

	if breached, _ := eng.evaluateProduceLevel(claim, 99); breached {
		t.Error("99 in a bin that holds 100 is not full — a produce cell that evacuates early " +
			"sends away a bin with room left in it")
	}
	if breached, _ := eng.evaluateProduceLevel(claim, 100); !breached {
		t.Error("100 in a bin that holds 100 IS full and must breach")
	}

	// The relief edge runs the other way from the consume side: the count drops
	// after the full bin leaves, and the margin is what stops a tick's wobble at
	// the capacity line from minting an episode per tick.
	margin := eng.cfg.HysteresisMargin(100)
	if _, shouldClose := eng.evaluateProduceLevel(claim, 100-margin); shouldClose {
		t.Errorf("relieved at exactly capacity-margin (%d) — that point is INSIDE the hysteresis "+
			"band, and closing there is what the band exists to prevent", 100-margin)
	}
	if _, shouldClose := eng.evaluateProduceLevel(claim, 100-margin-1); !shouldClose {
		t.Errorf("not relieved at %d, which is clear of capacity %d minus margin %d — the episode "+
			"never closes and the cell holds one open demand forever", 100-margin-1, 100, margin)
	}
}

// A PRODUCE REORDER POINT AT OR ABOVE CAPACITY IS NOT A LEVEL, and the decision
// falls back to capacity.
//
// A bin cannot hold more than its capacity, so "ask when the count reaches 120"
// on a bin that holds 100 is a level that can never be crossed. Honouring it
// literally would mean a produce cell that never asks for its empty at all —
// strictly worse than the behaviour it was meant to improve. Capacity is the
// floor the fallback lands on, and it is what the tree does today by accident
// (the evaluator never read reorder_point) and must keep doing on purpose.
func TestDemandLevel_ProduceReorderPointAtOrAboveCapacityDecidesAtCapacity(t *testing.T) {
	t.Parallel()

	t.Run("above capacity", func(t *testing.T) {
		t.Parallel()
		eng, _, _, claim := demandLevelFixture(t, "PRODOVER", protocol.ClaimRoleProduce, 100, 120)
		if breached, _ := eng.evaluateProduceLevel(claim, 99); breached {
			t.Error("breached at 99 against capacity 100")
		}
		if breached, _ := eng.evaluateProduceLevel(claim, 100); !breached {
			t.Error("a reorder point of 120 on a bin that holds 100 is unreachable; the cell must " +
				"still breach when the bin is full, or it never asks for its empty at all")
		}
	})

	t.Run("exactly capacity", func(t *testing.T) {
		t.Parallel()
		eng, _, _, claim := demandLevelFixture(t, "PRODEQ", protocol.ClaimRoleProduce, 100, 100)
		if breached, _ := eng.evaluateProduceLevel(claim, 99); breached {
			t.Error("breached at 99 against capacity 100")
		}
		if breached, _ := eng.evaluateProduceLevel(claim, 100); !breached {
			t.Error("a reorder point equal to capacity asks nothing earlier than capacity does, " +
				"so it is capacity")
		}
	})
}
