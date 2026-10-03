package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// WHO KEEPS A CELL SHUT, BY THE DURABLE ROW.
//
// guardPositionSpokenFor's arm 2 and the level sweep's dedup both read the
// line's non-terminal orders through orderWorksTheCell. Every population below
// leaves the line's runtime slots EMPTY — the moments arm 2 exists for, when
// the pointer says nothing — and every one of them must keep the cell shut:
// each either still holds or still owes the line a bin.
//
// The keep-staged exemption (a plain refill bound for the claim's spot) is the
// ONLY row allowed to stop shutting the cell, and only for a keep-staged claim.
// Every row here is a population that exemption must never admit.

type cellRow struct {
	name string
	make func(t *testing.T, eng *Engine, db *store.DB, nodeID int64, claim *processes.NodeClaim)
}

func complexRow(uuid string, steps func(*processes.NodeClaim) []protocol.ComplexOrderStep, delivery func(*processes.NodeClaim) string) func(*testing.T, *Engine, *store.DB, int64, *processes.NodeClaim) {
	return func(t *testing.T, _ *Engine, db *store.DB, nodeID int64, claim *processes.NodeClaim) {
		t.Helper()
		mkSwapLeg(t, db, nodeID, uuid, steps(claim), delivery(claim))
	}
}

var cellShuttingRows = []cellRow{
	{"single_robot swap leg, delivery = OutboundDestination", complexRow("pop-single",
		BuildSingleSwapSteps, func(c *processes.NodeClaim) string { return c.OutboundDestination })},
	{"two_robot evac before departure, delivery blank", complexRow("pop-evac",
		func(c *processes.NodeClaim) []protocol.ComplexOrderStep { _, b := BuildTwoRobotSwapSteps(c); return b },
		func(*processes.NodeClaim) string { return "" })},
	{"release order", complexRow("pop-release",
		BuildReleaseSteps, func(c *processes.NodeClaim) string { return c.OutboundDestination })},
	{"fallback staging order (complex, delivery = InboundStaging)", complexRow("pop-stage",
		BuildStageSteps, func(c *processes.NodeClaim) string { return c.InboundStaging })},
	{"move off the line", func(t *testing.T, eng *Engine, _ *store.DB, nodeID int64, c *processes.NodeClaim) {
		t.Helper()
		_, err := eng.orderMgr.CreateMoveOrder(&nodeID, 1, c.CoreNodeName, c.OutboundDestination, true,
			orders.Attached("pop-move"))
		testutil.MustNoErr(t, err, "create move off the line")
	}},
}

// spotBoundRetrieve is the order shape a keep-staged refill takes: a plain
// retrieve attributed to the line, delivering to the line's inbound staging.
func spotBoundRetrieve(t *testing.T, eng *Engine, nodeID int64, c *processes.NodeClaim) {
	t.Helper()
	_, err := eng.orderMgr.CreateRetrieveOrder(&nodeID, c.Role == protocol.ClaimRoleProduce, 1,
		c.InboundStaging, c.InboundSource, "", "standard", c.PayloadCode, true, false, orders.Attached("spot-refill"))
	testutil.MustNoErr(t, err, "create spot-bound retrieve")
}

func assertCellShut(t *testing.T, eng *Engine, db *store.DB, nodeID int64, why string) {
	t.Helper()
	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "node")
	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "runtime")
	if rt.ActiveOrderID != nil || rt.StagedOrderID != nil {
		t.Fatalf("fixture drift: runtime slots must be nil to isolate arm 2 (%s)", why)
	}
	if gerr := eng.guardPositionSpokenFor(node, rt, keeperClaim(t, db, nodeID)); gerr == nil {
		t.Errorf("guardPositionSpokenFor admitted a downgrade while %s", why)
	}
	before := countOrders(t, db)
	eng.sweepCellLevels()
	if after := countOrders(t, db); after != before {
		t.Errorf("the level sweep asked (orders %d -> %d) while %s", before, after, why)
	}
}

func TestGuardPositionSpokenFor_ArmTwoRefusesEveryNonLinePopulation(t *testing.T) {
	t.Parallel()
	for _, row := range cellShuttingRows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			eng, db, nodeID, claimID := keeperFixture(t)
			setLevel(t, db, nodeID, claimID, 0)
			row.make(t, eng, db, nodeID, keeperClaim(t, db, nodeID))
			assertCellShut(t, eng, db, nodeID, row.name+" is live at the line")
		})
	}
}

// A plain retrieve bound for the line's inbound staging works the cell for a
// claim that keeps no spare: nothing distinguishes it from any other delivery
// the line is owed.
func TestSpotBoundRetrieve_ShutsTheCellForAnOrdinaryClaim(t *testing.T) {
	t.Parallel()
	eng, db, nodeID, claimID := keeperFixture(t)
	setLevel(t, db, nodeID, claimID, 0)
	spotBoundRetrieve(t, eng, nodeID, keeperClaim(t, db, nodeID))
	assertCellShut(t, eng, db, nodeID, "a plain retrieve to inbound staging is queued")
}

// THE WAIT NODE, which nothing pinned. The full two-robot supply holds at the
// inbound staging node for the swap release; the changeover supply holds at
// the same node for the ready release. Keep-staged's short tail waits on the
// same node, so the release doors and cellSetFor see one node in both shapes.
func TestTwoRobotSupply_WaitsAtInboundStaging(t *testing.T) {
	t.Parallel()
	for _, role := range []protocol.ClaimRole{protocol.ClaimRoleConsume, protocol.ClaimRoleProduce} {
		claim := goldenClaim(role, protocol.SwapModeTwoRobot, "P-FROM", "", false)
		supply, _ := BuildTwoRobotSwapSteps(claim)
		assertOneStationWait(t, supply, claim.InboundStaging, "swap", string(role)+" steady supply")

		to := goldenClaim(role, protocol.SwapModeTwoRobot, "P-TO", "", false)
		d := BuildSwapChangeoverSteps(claim, to, "", "")
		if d.Roles == nil {
			t.Fatalf("%s: two_robot changeover lost its role-declared legs", role)
		}
		assertOneStationWait(t, d.Roles.supply.steps, to.InboundStaging, "ready", string(role)+" changeover supply")
	}
}

func assertOneStationWait(t *testing.T, steps []protocol.ComplexOrderStep, node, purpose, what string) {
	t.Helper()
	var waits []protocol.ComplexOrderStep
	for _, s := range steps {
		if s.Action == protocol.ActionWait {
			waits = append(waits, s)
		}
	}
	if len(waits) != 1 {
		t.Fatalf("%s: %d waits, want 1: %+v", what, len(waits), steps)
	}
	if waits[0].Node != node || waits[0].Purpose != purpose || waits[0].WaitKind != waitKindStation {
		t.Errorf("%s: wait = (%q, %q, %q), want (%q, %q, %q)", what,
			waits[0].Node, waits[0].Purpose, waits[0].WaitKind, node, purpose, waitKindStation)
	}
}

// THE SAME ROWS FOR A KEEP-STAGED CELL. The keep-staged exemption (isSpotRefill)
// excuses a plain order bound for the claim's spot and nothing else: every
// population above still keeps the cell shut, including the complex fallback
// staging order that delivers to the same node.
func TestGuardPositionSpokenFor_ArmTwoRefusesEveryNonLinePopulation_KeepStaged(t *testing.T) {
	t.Parallel()
	for _, row := range cellShuttingRows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			eng, db, nodeID, claimID := keeperFixture(t)
			setLevel(t, db, nodeID, claimID, 0)
			markKeepStaged(t, db, claimID)
			row.make(t, eng, db, nodeID, keeperClaim(t, db, nodeID))
			assertCellShut(t, eng, db, nodeID, row.name+" is live at a keep-staged line")
		})
	}
}

// THE ROW THAT FLIPS. For a keep-staged claim a plain retrieve bound for its
// spot is a refill: it brings the line no bin, so it neither refuses the line's
// REQUEST nor silences the level keeper. Before isSpotRefill it did both, for
// the whole trip, and for ever with a dry market (E20).
func TestSpotBoundRetrieve_LeavesAKeepStagedCellOpen(t *testing.T) {
	t.Parallel()
	eng, db, nodeID, claimID := keeperFixture(t)
	setLevel(t, db, nodeID, claimID, 0)
	markKeepStaged(t, db, claimID)
	claim := keeperClaim(t, db, nodeID)
	spotBoundRetrieve(t, eng, nodeID, claim)

	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "node")
	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "runtime")
	if gerr := eng.guardPositionSpokenFor(node, rt, claim); gerr != nil {
		t.Errorf("a refill bound for the spot refused the line's downgrade: %v", gerr)
	}
	before := countOrders(t, db)
	eng.sweepCellLevels()
	if after := countOrders(t, db); after == before {
		t.Errorf("the level sweep asked for nothing while the only order at the line was a refill bound for the spot")
	}
}

// markKeepStaged sets the flag straight on the row: these tests are about the
// two readers of orderWorksTheCell, not the config door that admits the flag.
func markKeepStaged(t *testing.T, db *store.DB, claimID int64) {
	t.Helper()
	_, err := db.DB.Exec(`UPDATE style_node_claims SET keep_staged=1 WHERE id=?`, claimID)
	testutil.MustNoErr(t, err, "keep_staged")
}
