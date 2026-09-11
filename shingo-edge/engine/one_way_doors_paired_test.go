package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// one_way_doors_paired_test.go — census 6: the doors that minted leg A with no
// sibling.
//
// Core's pair rule (shingo-core/dispatch/complex_pair.go) admits both legs of a
// coordinated swap in one pass or neither, and it can only do that for a pair it
// can SEE. It sees a pair when the leg it is dispatching names its partner. The
// produce door and the changeover applier mint both uuids before either create,
// so each leg names the other from its first moment (paired_leg_uuid_test.go).
// Three doors did not: the consume REQUEST (and the level keeper's consume arm,
// which is the same function), and REQUEST EMPTY BIN on a swap-mode cell. They
// created leg A with sibling "" and only leg B named A, so on Core's own intake
// pass leg A was a SOLO order and went to the fleet before B existed — 31 of the
// 57 pairs in the 2026-09-10 lane-stress run, every one of them a consume pair.
//
// DEFECT PINS. Each fails at bcbde0d2 in assertMutuallyPaired: leg A goes out
// with no sibling.

// seedLineSwapClaim creates a CONSUME cell whose active claim is a multi-leg swap
// mode — the population the consume REQUEST builds a pair for.
func seedLineSwapClaim(t *testing.T, db *store.DB, prefix string, mode protocol.SwapMode) int64 {
	t.Helper()
	processID, err := db.CreateProcess(prefix+"-PROC", "", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: prefix + "-LINE", Code: prefix, Name: prefix + " line", Enabled: true,
	})
	testutil.MustNoErr(t, err, "create node")
	styleID, err := db.CreateStyle(prefix+"-STYLE", "", processID)
	testutil.MustNoErr(t, err, "create style")
	testutil.MustNoErr(t, db.SetActiveStyle(processID, &styleID), "set active style")
	_, err = db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID:             styleID,
		CoreNodeName:        prefix + "-LINE",
		Role:                protocol.ClaimRoleConsume,
		SwapMode:            mode,
		PayloadCode:         "PART-C",
		UOPCapacity:         100,
		InboundSource:       prefix + "-MARKET",
		InboundStaging:      prefix + "-IN-STAGE",
		OutboundStaging:     prefix + "-OUT-STAGE",
		OutboundDestination: prefix + "-MARKET",
		PairedCoreNode:      prefix + "-BACK",
	})
	testutil.MustNoErr(t, err, "upsert claim")
	_, err = db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "ensure runtime")
	return nodeID
}

func TestConsumeRequest_BothLegsGoOutPaired(t *testing.T) {
	t.Parallel()
	for _, mode := range []protocol.SwapMode{protocol.SwapModeTwoRobot, protocol.SwapModeTwoRobotPressIndex} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			db := testEngineDB(t)
			nodeID := seedLineSwapClaim(t, db, "OWC", mode)
			eng := testEngine(t, db)

			res, err := eng.RequestNodeMaterial(nodeID, 1)
			testutil.MustNoErr(t, err, "consume REQUEST")
			if res.OrderA == nil || res.OrderB == nil {
				t.Fatalf("fixture: the REQUEST did not build a two-leg swap (order=%v a=%v b=%v)",
					res.Order, res.OrderA, res.OrderB)
			}
			assertMutuallyPaired(t, complexRequestsOnTheWire(t, db))
		})
	}
}

// TestLevelKeeperConsume_BothLegsGoOutPaired is the same door reached the way the
// plant reaches it most: the level sweep asking on a consume cell below its
// reorder point. It is the only consume door any automated sim run drives.
func TestLevelKeeperConsume_BothLegsGoOutPaired(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	nodeID := seedLineSwapClaim(t, db, "OWL", protocol.SwapModeTwoRobot)
	eng := testEngine(t, db)

	res, err := eng.requestNodeMaterialFor(nodeID, 1, protocol.EpisodeTriggerAutoreorder)
	testutil.MustNoErr(t, err, "level-keeper ask")
	if res.OrderA == nil || res.OrderB == nil {
		t.Fatalf("fixture: the ask did not build a two-leg swap")
	}
	assertMutuallyPaired(t, complexRequestsOnTheWire(t, db))
}

// TestRequestEmptyBin_BothLegsGoOutPaired: REQUEST EMPTY BIN on a produce cell
// whose claim is a swap mode reuses the swap dispatch, so it builds the same pair
// and had the same one-way first leg.
func TestRequestEmptyBin_BothLegsGoOutPaired(t *testing.T) {
	t.Parallel()
	for _, mode := range []protocol.SwapMode{protocol.SwapModeTwoRobot, protocol.SwapModeTwoRobotPressIndex} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			db := testEngineDB(t)
			nodeID, _, _ := seedSwapClaim(t, db, mode, "")
			eng := testEngine(t, db)

			if _, err := eng.RequestEmptyBin(nodeID, "WIDGET-A"); err != nil {
				t.Fatalf("REQUEST EMPTY BIN: %v", err)
			}
			reqs := complexRequestsOnTheWire(t, db)
			if len(reqs) != 2 {
				t.Fatalf("fixture: REQUEST EMPTY BIN put %d complex legs on the wire, want the two-leg swap", len(reqs))
			}
			assertMutuallyPaired(t, reqs)
		})
	}
}
