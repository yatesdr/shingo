package engine

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/orders"
	storeorders "shingoedge/store/orders"
)

// reorder_pause_test.go — what ends an order at an armed position, and whether
// the sweep asks again at once: the three writers of a pause (a cancel Core
// sends, a person's abort, a failure), an off from an old Core's abandon, and
// the ends that pause nothing (a skip, the Edge's own aborts).

// sweptOrder is the order the first sweep of a keeperFixture cell at 0 asks.
func sweptOrder(t *testing.T, eng *Engine) *storeorders.Order {
	t.Helper()
	eng.sweepCellLevels()
	if got := countOrders(t, eng.db); got != 1 {
		t.Fatalf("first sweep minted %d orders, want 1", got)
	}
	var id int64
	testutil.MustNoErr(t, eng.db.DB.QueryRow(`SELECT id FROM orders`).Scan(&id), "read order id")
	o, err := eng.db.GetOrder(id)
	testutil.MustNoErr(t, err, "read the order")
	return o
}

// TestReask_AfterEachKindOfEnd: an armed cell at 0 is asked for, the order
// ends, and the next sweep re-asks unless the end held the position. At
// aea6321d every row re-asked (2 orders); the four that hold moved to 1.
func TestReask_AfterEachKindOfEnd(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		end  func(t *testing.T, eng *Engine, o *storeorders.Order)
		hold domain.ReorderHold
	}{
		{name: "Core cancels it", hold: domain.ReorderHoldPaused, end: func(t *testing.T, eng *Engine, o *storeorders.Order) {
			testutil.MustNoErr(t, eng.orderMgr.HandleDispatchReply(o.UUID, orders.ReplyCancelled, "", "", "fleet refused the order"), "Core cancels")
		}},
		{name: "an old Core abandons it", hold: domain.ReorderHoldOff, end: func(t *testing.T, eng *Engine, o *storeorders.Order) {
			testutil.MustNoErr(t, eng.orderMgr.HandleDispatchReply(o.UUID, orders.ReplyCancelled, "", "",
				"abandoned: stuck in dispatched past 1h0m0s"), "Core abandons")
		}},
		{name: "it fails", hold: domain.ReorderHoldPaused, end: func(t *testing.T, eng *Engine, o *storeorders.Order) {
			testutil.MustNoErr(t, eng.orderMgr.HandleDispatchReply(o.UUID, orders.ReplyError, "", "", "no route"), "Core fails it")
		}},
		{name: "a person aborts it on the Orders page", hold: domain.ReorderHoldPaused, end: func(t *testing.T, eng *Engine, o *storeorders.Order) {
			testutil.MustNoErr(t, eng.orderMgr.AbortOrderByPerson(o.ID), "a person aborts")
		}},
		{name: "the Edge aborts it for a changeover", end: func(t *testing.T, eng *Engine, o *storeorders.Order) {
			testutil.MustNoErr(t, eng.orderMgr.AbortOrderWithReason(o.ID, "cancelled at changeover start"), "the Edge aborts")
		}},
		{name: "Core skips it", end: func(t *testing.T, eng *Engine, o *storeorders.Order) {
			testutil.MustNoErr(t, eng.orderMgr.HandleDispatchReply(o.UUID, orders.ReplySkipped, "", "", "no source bin"), "Core skips")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eng, db, nodeID, claimID := keeperFixture(t)
			setLevel(t, db, nodeID, claimID, 0)
			o := sweptOrder(t, eng)
			tc.end(t, eng, o)

			eng.sweepCellLevels()
			want := 2
			if tc.hold != "" {
				want = 1
			}
			if got := countOrders(t, db); got != want {
				t.Fatalf("orders after the end and one sweep = %d, want %d", got, want)
			}
			rt, err := db.GetProcessNodeRuntime(nodeID)
			testutil.MustNoErr(t, err, "read the runtime row")
			if rt.ReorderHold != tc.hold {
				t.Errorf("the position holds %q, want %q", rt.ReorderHold, tc.hold)
			}
		})
	}
}

// TestCoreCancel_AnAbandonedSwapEndsOff: an old Core gives up on one leg of a
// swap ("abandoned: ...") and cancels its partner in the same call stack
// ("coordinated swap ..."), which is a pause by its words. The position ends
// off whichever arrives first: the rank keeps the pause from stepping it down.
func TestCoreCancel_AnAbandonedSwapEndsOff(t *testing.T) {
	t.Parallel()
	const abandoned = "abandoned: operator-gated staging past 4h0m0s"
	const partner = "coordinated swap evac (order 1) cancelled; cancelling supply so it cannot drop onto an un-cleared line"
	for _, tc := range []struct {
		name  string
		order [2]string
	}{
		{name: "the abandon first, as Core sends it", order: [2]string{abandoned, partner}},
		{name: "the partner's first", order: [2]string{partner, abandoned}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eng, db, nodeID, _ := keeperFixture(t)
			for i, reason := range tc.order {
				id, err := db.CreateOrder("swap-leg-"+string(rune('a'+i)), orders.TypeRetrieve, &nodeID, false, 1,
					"ALN_003", "", "SYN_MARKET", protocol.Height{}, false, "PANEL-B", "", "")
				testutil.MustNoErr(t, err, "create a leg")
				testutil.MustNoErr(t, db.UpdateOrderStatus(id, string(orders.StatusStaged)), "stage the leg")
				o, err := db.GetOrder(id)
				testutil.MustNoErr(t, err, "read the leg")
				testutil.MustNoErr(t, eng.orderMgr.HandleDispatchReply(o.UUID, orders.ReplyCancelled, "", "", reason), "Core cancels a leg")
			}
			rt, err := db.GetProcessNodeRuntime(nodeID)
			testutil.MustNoErr(t, err, "read the runtime row")
			if rt.ReorderHold != domain.ReorderHoldOff {
				t.Fatalf("after both cancels the position holds %q, want off", rt.ReorderHold)
			}
		})
	}
}

// TestGo_RequestClearsAPauseAndAnOffNotAPersonsOff: a person's REQUEST clears a
// pause or an off and stamps the go, in one statement; a person's off stays,
// and the go is stamped under it. The sweep's own ask is not a go.
func TestGo_RequestClearsAPauseAndAnOffNotAPersonsOff(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		held  domain.ReorderHold
		stays bool
	}{
		{held: domain.ReorderHoldPaused},
		{held: domain.ReorderHoldOff},
		{held: domain.ReorderHoldPersonOff, stays: true},
	} {
		t.Run(string(tc.held), func(t *testing.T) {
			t.Parallel()
			eng, db, nodeID, claimID := keeperFixture(t)
			setLevel(t, db, nodeID, claimID, 0)
			holdAt(t, eng, nodeID, tc.held, time.Now(), "fixture")
			before := time.Now().Add(-time.Second)
			if _, err := eng.RequestNodeMaterial(nodeID, 1); err != nil {
				t.Fatalf("REQUEST: %v", err)
			}
			rt, err := db.GetProcessNodeRuntime(nodeID)
			testutil.MustNoErr(t, err, "read the runtime row")
			want := domain.ReorderHoldNone
			if tc.stays {
				want = tc.held
			}
			if rt.ReorderHold != want {
				t.Errorf("after REQUEST the position holds %q, want %q", rt.ReorderHold, want)
			}
			if rt.ReorderGoAt.Before(before) {
				t.Errorf("REQUEST stamped the go at %v, want at or after %v", rt.ReorderGoAt, before)
			}
		})
	}

	t.Run("the sweep's ask", func(t *testing.T) {
		t.Parallel()
		eng, db, nodeID, claimID := keeperFixture(t)
		setLevel(t, db, nodeID, claimID, 0)
		holdAt(t, eng, nodeID, domain.ReorderHoldPaused, time.Now().Add(-domain.ReorderPause-time.Minute), "a pause run out")
		eng.sweepCellLevels()
		rt, err := db.GetProcessNodeRuntime(nodeID)
		testutil.MustNoErr(t, err, "read the runtime row")
		if !rt.ReorderGoAt.IsZero() || rt.ReorderHold != domain.ReorderHoldPaused {
			t.Errorf("the sweep's ask wrote a go (go %v, hold %q); only a person's REQUEST is one", rt.ReorderGoAt, rt.ReorderHold)
		}
	})
}

// TestGo_TurningAutoReorderOnInProcesses: a save that turns a claim's
// auto-reorder on clears its position's pause and stamps the go; a save that
// leaves it on writes nothing.
func TestGo_TurningAutoReorderOnInProcesses(t *testing.T) {
	t.Parallel()
	eng, db, nodeID, claimID := keeperFixture(t)
	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "read the node")
	_, err = db.DB.Exec(`UPDATE style_node_claims SET auto_reorder=0 WHERE id=?`, claimID)
	testutil.MustNoErr(t, err, "turn the claim off")

	save := func(on bool) {
		t.Helper()
		claim, err := db.GetStyleNodeClaim(claimID)
		testutil.MustNoErr(t, err, "read the claim")
		cell := domain.Collapse(*claim)
		cell.Advanced = domain.AdvancedOf(claim)
		cell.Advanced.AutoReorder = on
		fp, err := eng.FlowFingerprint(node.ProcessID, claim.StyleID)
		testutil.MustNoErr(t, err, "fingerprint")
		_, err = eng.SaveFlow(node.ProcessID, FlowSaveRequest{
			ToStyleID: claim.StyleID, Fingerprint: fp, Source: domain.ClaimSourceAdmin, CalledBy: "test",
			Cells: []domain.FlowCell{cell},
		})
		testutil.MustNoErr(t, err, "save the flow")
	}

	holdAt(t, eng, nodeID, domain.ReorderHoldPaused, time.Now(), "fixture")
	save(true)
	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "read the runtime row")
	if rt.ReorderHold != domain.ReorderHoldNone || rt.ReorderGoAt.IsZero() {
		t.Fatalf("turning auto-reorder on left hold %q, go %v; want cleared and stamped", rt.ReorderHold, rt.ReorderGoAt)
	}

	holdAt(t, eng, nodeID, domain.ReorderHoldPaused, time.Now(), "again")
	save(true)
	rt, err = db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "read the runtime row")
	if rt.ReorderHold != domain.ReorderHoldPaused {
		t.Errorf("a save that left auto-reorder on cleared the pause (hold %q)", rt.ReorderHold)
	}
}

// TestArming_TheSweepDoesNotAskOnACountThatCannotBeTheClaims: a breached,
// armed cell whose count cannot be its claim's (produce past two packs,
// consume past one) is not asked for; at two packs, or a negative consume
// count, it is. At aea6321d every node asked.
func TestArming_TheSweepDoesNotAskOnACountThatCannotBeTheClaims(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	f := seedWalkerProcess(t, db, "ARMING", []walkerNode{
		{Suffix: "PTWO", Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeSingleRobot,
			PayloadCode: "ARM-P1", Capacity: 100, AutoReorder: true, Remaining: 200},
		{Suffix: "POVER", Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeSingleRobot,
			PayloadCode: "ARM-P2", Capacity: 100, AutoReorder: true, Remaining: 201},
		{Suffix: "CNEG", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeSingleRobot,
			PayloadCode: "ARM-C1", Capacity: 100, ReorderPoint: 25, AutoReorder: true, Remaining: -1650},
		{Suffix: "COVER", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeSingleRobot,
			PayloadCode: "ARM-C2", Capacity: 100, ReorderPoint: 120, AutoReorder: true, Remaining: 101},
	})
	eng := testEngine(t, db)
	bridgeCore(t, eng, headOccupancyStub(t, false).URL)
	holdPlaces(t, eng, everyAs(false))

	proc, err := db.GetProcess(f.ProcessID)
	testutil.MustNoErr(t, err, "get process")
	eng.sweepProcessLevels(proc)

	assertSet(t, "nodes asked for", askedNodes(t, db, f), []string{"CNEG", "PTWO"})
}
