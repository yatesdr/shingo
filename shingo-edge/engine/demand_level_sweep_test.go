package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/orders"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// demand_level_sweep_test.go — the level keeper's own tests.
//
// THE BLOCK THAT CAUSED 2026-07-21 HAS NEVER HAD A TEST AT ITS OWN GRAIN.
// consumption_fixture_test.go drives the real handleCounterDelta on a virtual
// clock and contains no order-related identifier at all; across wiring_test.go
// and wiring_concurrent_tick_test.go every CreateOrder is setup. No assertion
// anywhere observes an order PRODUCED by the replenishment decision. So these
// are not additional coverage on a tested path — they are the first assertions
// that watch the decision itself.
//
// AND THE SIM CANNOT STAND IN FOR THEM. The Springfield spam population is
// unreachable by construction on this tree: the fixtures' markets refill, and
// the 2ae75147 park replaced the terminal skip for filler legs. A green soak
// proves circulation, not restraint. The anti-spam property lives here or
// nowhere.

// keeperFixture seeds one consume cell that wants material, with Core answering
// that its position is bare.
//
// single_robot + a bare position is the shape that mints exactly one plain move
// per ask, which is what makes "how many orders did this produce" a countable
// question. The claim is armed (auto_reorder) and its level is 25.
func keeperFixture(t *testing.T) (*Engine, *store.DB, int64, int64) {
	t.Helper()
	db := testEngineDB(t)
	eng := testEngine(t, db)

	procID, err := db.CreateProcess("KEEPER-PROC", "cell level keeper", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: procID, CoreNodeName: "ALN_003", Code: "A03", Name: "ALN_003", Sequence: 1, Enabled: true,
	})
	testutil.MustNoErr(t, err, "create node")
	styleID, err := db.CreateStyle("KEEPER-STYLE", "", procID)
	testutil.MustNoErr(t, err, "create style")
	testutil.MustNoErr(t, db.SetActiveStyle(procID, &styleID), "set active style")

	claimID, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: "ALN_003", Role: protocol.ClaimRoleConsume,
		SwapMode: protocol.SwapModeSingleRobot, PayloadCode: "PANEL-B",
		UOPCapacity: 40, ReorderPoint: 25, AutoReorder: domain.Ptr(true),
		InboundSource: "SYN_MARKET", OutboundDestination: "SYN_MARKET",
		InboundStaging: "SLN_005", OutboundStaging: "SLN_006",
	})
	testutil.MustNoErr(t, err, "upsert claim")

	_, err = db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "ensure runtime")

	eng.coreClient = NewCoreClient(headOccupancyStub(t, false).URL)
	return eng, db, nodeID, claimID
}

// setLevel writes the cached count with NO bin bound and NO order pointers —
// the W3 state exactly. Deliberately not routed through a tick: the whole point
// of the keeper is that it works when nothing is ticking.
func setLevel(t *testing.T, db *store.DB, nodeID, claimID int64, remaining int) {
	t.Helper()
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, nil, remaining), "seed level")
}

func keeperClaim(t *testing.T, db *store.DB, nodeID int64) *processes.NodeClaim {
	t.Helper()
	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "get node")
	claim := findActiveClaim(db, node)
	if claim == nil {
		t.Fatal("fixture has no active claim")
	}
	return claim
}

// ── 1. THE STALE-FLAG TRAP ────────────────────────────────────────────────
//
// THIS IS THE TEST THAT KEEPS THE FIX FROM BECOMING THE BUG, and it is written
// before the sweep for that reason.
//
// below_reorder_since is written ON TRANSITION ONLY, by four writers that all
// live inside evaluateCellLevel/evaluateProduceLevel. So the column does not
// mean "this claim is below its level". It means "this claim WAS below its
// level the last time anyone looked" — and once the evaluation moves off the
// PLC tick, "the last time anyone looked" can be arbitrarily long ago.
//
// A sweep that keys its ASK on that flag orders parts for a cell that already
// has a full bin, once per period, for the whole length of any line stoppage:
// a break, a shift change, a changeover, a weekend. CanAcceptOrders does not
// stop it (a terminal order reads as acceptable) and guardPositionSpokenFor
// does not (it guards the downgrade branch only). That is a spam generator,
// and it fires when everything is healthy.
//
// So the ask must be derived from the COUNT, which the delivery handler writes
// (wiring_delivered.go) and which needs no tick to read.
func TestLevelSweep_StaleFlagNeverOrders(t *testing.T) {
	t.Parallel()
	eng, db, nodeID, claimID := keeperFixture(t)
	claim := keeperClaim(t, db, nodeID)

	// The cell was below its level, so the falling edge is stamped.
	setLevel(t, db, nodeID, claimID, 10)
	if below, _ := eng.evaluateCellLevel(claim, 10); !below {
		t.Fatal("fixture should start below its reorder point")
	}
	stamped, err := db.GetClaimBelowReorderSince(claimID)
	testutil.MustNoErr(t, err, "read falling edge")
	if stamped == nil {
		t.Fatal("fixture drifted: the falling edge must be stamped for this trap to mean anything")
	}

	// A bin lands. The count is written by the delivery handler; NOTHING TICKS
	// after it, so no evaluator has run and the flag is still set.
	setLevel(t, db, nodeID, claimID, 40)

	before := countOrders(t, db)
	for i := 0; i < 10; i++ {
		eng.sweepCellLevels()
	}
	if after := countOrders(t, db); after != before {
		t.Fatalf("order count %d → %d across ten sweeps of a FULL cell: the sweep is keyed on "+
			"below_reorder_since, which only records where the level was when something last "+
			"looked. This is the spam generator, and it fires on a healthy plant.", before, after)
	}

	// And the sweep must have MAINTAINED the flag rather than merely ignored
	// it. Deleting the tick blocks deletes the column's only writers; if the
	// sweep does not take that job over, CellLevelStillBreached (the close
	// pass's whole predicate) reads a value nothing updates any more.
	if stamped, err := db.GetClaimBelowReorderSince(claimID); err != nil || stamped != nil {
		t.Errorf("below_reorder_since = %v (err %v) after the cell recovered — the sweep must own "+
			"this column now that the tick blocks are gone", stamped, err)
	}
}

// The band is the same trap one step in, and it is where reading the RETURN
// VALUE of evaluateCellLevel instead of the count would still get it wrong.
//
// Inside the hysteresis band the evaluator reports `below` from the FLAG — that
// is what the band is for, and it is correct for deciding whether the episode
// is still running. It is not the ask predicate. The tick never ordered at 26
// against a level of 25, and neither may the sweep.
func TestLevelSweep_InsideTheHysteresisBandNeverOrders(t *testing.T) {
	t.Parallel()
	eng, db, nodeID, claimID := keeperFixture(t)
	claim := keeperClaim(t, db, nodeID)

	setLevel(t, db, nodeID, claimID, 10)
	eng.evaluateCellLevel(claim, 10)

	// 26 is above the level of 25 but inside the 10% margin, so the flag stays
	// set and evaluateCellLevel still reports `below`.
	setLevel(t, db, nodeID, claimID, 26)

	before := countOrders(t, db)
	eng.sweepCellLevels()
	if after := countOrders(t, db); after != before {
		t.Fatalf("order count %d → %d for a cell at 26 against a level of 25: the ask is being taken "+
			"from evaluateCellLevel's sticky `below`, not from the count", before, after)
	}
}

// ── 2. ONE ASK, NOT TEN ───────────────────────────────────────────────────
//
// W3: the cell is at zero, both runtime order pointers are nil, no bin is
// bound, and NOTHING IS TICKING — a real machine cannot cycle an empty input,
// so the PLC gate correctly stops the counter at exactly the moment the demand
// exists. The old evaluator lived on the tick, so the one ask taken at the
// crossing was the only ask that would ever be taken. If it was refused, the
// cell sat starved forever.
//
// The keeper must ask. Once.
func TestLevelSweep_AsksOnceForAWedgedCell(t *testing.T) {
	t.Parallel()
	eng, db, nodeID, claimID := keeperFixture(t)
	setLevel(t, db, nodeID, claimID, 0)

	eng.sweepCellLevels()
	if got := countOrders(t, db); got != 1 {
		t.Fatalf("first sweep of a wedged cell minted %d orders, want exactly 1 — this is W3: a cell "+
			"at zero with a stopped counter had no way left to ask", got)
	}

	for i := 0; i < 5; i++ {
		eng.sweepCellLevels()
	}
	if got := countOrders(t, db); got != 1 {
		t.Fatalf("order count = %d after six sweeps, want 1 — the level is still breached every "+
			"period, so without the live-order dedup this is 2026-07-21 on a timer", got)
	}
}

// ── 3. THE DEDUP SEES A SYNCHRONOUS CREATE ────────────────────────────────
//
// THIS IS THE ASSERTION THAT WOULD HAVE CAUGHT THE ORIGIN-COLUMN DESIGN BEFORE
// IT SHIPPED. Edge's orders.Create inserts eleven columns and origin_id is not
// among them; the only Edge writer of that column is Core's projection upsert.
// So a dedup counting an episode's orders by origin returns ZERO for an order
// Edge created seconds ago — the 2026-08-03 duplicate shape rebuilt one level
// down.
//
// No Core round trip, no projection, no outbox drain between the two sweeps.
// Whatever the dedup reads must be a column Edge itself wrote.
func TestLevelSweep_DedupSeesItsOwnCreateWithNoCoreRoundTrip(t *testing.T) {
	t.Parallel()
	eng, db, nodeID, claimID := keeperFixture(t)
	setLevel(t, db, nodeID, claimID, 0)

	eng.sweepCellLevels()
	if got := countOrders(t, db); got != 1 {
		t.Fatalf("first sweep minted %d orders, want 1", got)
	}

	// The order Edge just made carries no origin_id. Assert that, so the test
	// keeps its point if someone ever populates the column: origin grain would
	// still be the wrong dedup, because it is blind to a bin already inbound
	// from a changeover, an operator push or the A/B tail, and because the
	// episode key is process-grain — two nodes on one process sharing a payload
	// would shadow each other, one served and the other suppressed.
	var originID any
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT origin_id FROM orders`).Scan(&originID), "read origin_id")
	if originID != nil {
		t.Logf("origin_id is now populated on Edge-created orders (%v) — the dedup must still not "+
			"depend on it", originID)
	}

	eng.sweepCellLevels()
	if got := countOrders(t, db); got != 1 {
		t.Fatalf("second sweep minted a duplicate (count=%d): the dedup cannot see an order this "+
			"same process created a moment ago", got)
	}
}

// ── 4. TERMINAL DOES NOT REOPEN UNBOUNDEDLY ───────────────────────────────
//
// A skipped order is a real, unserved demand, so the next sweep SHOULD ask
// again — that is the whole difference between this and the tick, which got one
// chance and lost it. What must not happen is the third sweep asking a third
// time while the second ask is still live.
func TestLevelSweep_ReasksAfterTerminalButNotOnTopOfTheReask(t *testing.T) {
	t.Parallel()
	eng, db, nodeID, claimID := keeperFixture(t)
	setLevel(t, db, nodeID, claimID, 0)

	eng.sweepCellLevels()
	if got := countOrders(t, db); got != 1 {
		t.Fatalf("first sweep minted %d orders, want 1", got)
	}

	var firstID int64
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT id FROM orders`).Scan(&firstID), "read order id")
	testutil.MustNoErr(t, db.UpdateOrderStatus(firstID, string(orders.StatusSkipped)), "skip the order")

	eng.sweepCellLevels()
	if got := countOrders(t, db); got != 2 {
		t.Fatalf("order count = %d after the first ask was SKIPPED, want 2 — the cell is still at "+
			"zero and nothing is coming; refusing to re-ask is how a cell starves", got)
	}

	eng.sweepCellLevels()
	if got := countOrders(t, db); got != 2 {
		t.Fatalf("order count = %d, want 2 — the re-ask is live, so the sweep after it must be silent", got)
	}
}

// ── 5. RECOVERY IS QUIET ──────────────────────────────────────────────────
//
// The healthy ending, end to end: the bin lands, the keeper clears the falling
// edge, the close pass reads that column and ends the episode, and nothing
// speaks again.
func TestLevelSweep_RecoveryClearsTheEdgeAndClosesTheEpisode(t *testing.T) {
	t.Parallel()
	eng, db, nodeID, claimID := keeperFixture(t)
	claim := keeperClaim(t, db, nodeID)

	setLevel(t, db, nodeID, claimID, 0)
	eng.evaluateCellLevel(claim, 0)
	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "get node")
	if _, _, err := eng.openCellEpisode(node.ProcessID, claim,
		protocol.EpisodeTriggerAutoreorder, 1, 0, false); err != nil {
		t.Fatalf("open episode: %v", err)
	}
	key := protocol.CellEpisodeKey("KEEPER-PROC", "PANEL-B", protocol.ClaimRoleConsume)
	if _, err := db.GetOpenDemandOrigin(key); err != nil {
		t.Fatalf("fixture drifted: the episode must be open before recovery, got %v", err)
	}

	// The bin lands and the count is written. One combined pass: the keeper
	// clears the edge, then the close pass reads the column it just wrote.
	setLevel(t, db, nodeID, claimID, 40)
	before := countOrders(t, db)
	eng.reconcileDemand()

	if stamped, err := db.GetClaimBelowReorderSince(claimID); err != nil || stamped != nil {
		t.Errorf("below_reorder_since = %v (err %v), want cleared on the rising edge", stamped, err)
	}
	if _, err := db.GetOpenDemandOrigin(key); err == nil {
		t.Error("the episode is still open on a recovered cell — the close pass reads a column the " +
			"keeper had already refreshed in this same pass, so it had everything it needed")
	}

	for i := 0; i < 10; i++ {
		eng.reconcileDemand()
	}
	if after := countOrders(t, db); after != before {
		t.Errorf("order count %d → %d across ten passes over a recovered cell: recovery must be silent", before, after)
	}
}

// ── 6. FAIL-OPEN CARRIES OVER ─────────────────────────────────────────────
//
// The close pass's posture, stated at demand_reconciler.go: a read failure is
// NOT evidence about the plant. It must neither close an episode nor mint an
// order — the next pass re-decides. The keeper inherits it, and this is the arm
// where it bites: the live-order dedup. A dedup that cannot see the orders
// table must not conclude "nothing is coming".
//
// Not parallel: it renames a table out from under the whole DB.
func TestLevelSweep_DedupReadFailureAsksForNothing(t *testing.T) {
	eng, db, nodeID, claimID := keeperFixture(t)
	setLevel(t, db, nodeID, claimID, 0)

	// Take the orders table out from under the dedup query.
	_, err := db.DB.Exec(`ALTER TABLE orders RENAME TO orders_quarantined`)
	testutil.MustNoErr(t, err, "quarantine orders table")

	eng.sweepCellLevels()

	_, err = db.DB.Exec(`ALTER TABLE orders_quarantined RENAME TO orders`)
	testutil.MustNoErr(t, err, "restore orders table")

	if got := countOrders(t, db); got != 0 {
		t.Errorf("the sweep minted %d orders while it could not read what was already in flight — "+
			"a check that cannot see its input must never render as a finding", got)
	}
	if stamped, err := db.GetClaimBelowReorderSince(claimID); err != nil || stamped == nil {
		t.Errorf("below_reorder_since = %v (err %v): the level read SUCCEEDED, so the falling edge "+
			"is a fact and must still be recorded even though the ask could not be decided", stamped, err)
	}
}

// ── 7. THE FLIP SITE'S IMMEDIATE CHECK ────────────────────────────────────
//
// sweepNodeLevelNow is the same decision taken early, and the A/B flip is its
// one caller: a flip is the moment a parked side becomes interesting, and
// waiting a period to notice would be a real regression on an operator-visible
// path. It must make nothing possible that the periodic pass would not.
func TestLevelSweep_ImmediateCheckMatchesThePeriodicPass(t *testing.T) {
	t.Parallel()
	eng, db, nodeID, claimID := keeperFixture(t)
	setLevel(t, db, nodeID, claimID, 0)

	eng.sweepNodeLevelNow(nodeID)
	if got := countOrders(t, db); got != 1 {
		t.Fatalf("immediate check minted %d orders for a cell at zero, want 1", got)
	}
	eng.sweepNodeLevelNow(nodeID)
	eng.sweepCellLevels()
	if got := countOrders(t, db); got != 1 {
		t.Fatalf("order count = %d, want 1 — the immediate check and the periodic pass share one "+
			"dedup or they are two mechanisms again", got)
	}
}

// THE OPT-OUT IS NOW HONOURED WHERE IT WAS NOT.
//
// reorder_point = 0 is the documented opt-out and the legacy default. The
// consume tick refused to fire on it deliberately. The A/B flip tail did not
// even look — it compared remaining <= reorder_point, which is true at zero
// against zero, so a claim explicitly opted out fired from the flip anyway.
//
// This is the behaviour change a plant would notice: one with reorder_point = 0
// claims stops getting A/B-tail orders it was silently getting.
func TestLevelSweep_ReorderPointZeroIsAnOptOut(t *testing.T) {
	t.Parallel()
	eng, db, nodeID, claimID := keeperFixture(t)
	_, err := db.DB.Exec(`UPDATE style_node_claims SET reorder_point = 0 WHERE id = ?`, claimID)
	testutil.MustNoErr(t, err, "opt the claim out")
	setLevel(t, db, nodeID, claimID, 0)

	eng.sweepCellLevels()
	eng.sweepNodeLevelNow(nodeID)
	if got := countOrders(t, db); got != 0 {
		t.Fatalf("an opted-out claim (reorder_point = 0) at zero minted %d orders, want 0 — the old "+
			"flip tail read remaining <= reorder_point, which is true at 0 <= 0", got)
	}
}
