//go:build docker

package engine

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/fleet"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/payloads"
)

// A bin riding a robot's deck has, until now, had exactly one way off: wait for
// that robot to unload somewhere Core can name. These pin the second way — a
// vehicle-pinned unload-only order — and, as much, pin what it REFUSES to do.

// seedCarried puts a bin on a robot's carrier node, the state parkOnCarrier
// leaves behind, and gives it a terminal order so the "where was it going"
// tier has something to read.
func seedCarried(t *testing.T, db *store.DB, robotID, wasGoingTo string) *bins.Bin {
	t.Helper()
	carrier := &nodes.Node{Name: bins.CarrierNodePrefix + robotID, IsSynthetic: true, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(carrier), "create carrier node")

	bt := &bins.BinType{Code: "CR-" + robotID, Description: "tote"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "create bin type")
	bin := &bins.Bin{BinTypeID: bt.ID, Label: "carried-" + robotID, NodeID: &carrier.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(bin), "create bin")

	ord := &orders.Order{
		EdgeUUID: "carried-" + robotID, StationID: "edge.test", OrderType: "retrieve",
		Status: protocol.StatusCancelled, Quantity: 1, DeliveryNode: wasGoingTo,
		RobotID: robotID, BinID: &bin.ID,
	}
	testutil.MustNoErr(t, db.CreateOrder(ord), "create prior order")
	_, err := db.DB.Exec(`UPDATE orders SET robot_id=$1, bin_id=$2, status='cancelled' WHERE id=$3`,
		robotID, bin.ID, ord.ID)
	testutil.MustNoErr(t, err, "set robot, bin and terminal status")
	return bin
}

// dispatchableRobot is a robot the fleet will actually take an order for:
// connected, in the dispatch pool, parked, no faults.
func dispatchableRobot(id string) fleet.RobotStatus {
	return fleet.RobotStatus{VehicleID: id, Connected: true, Available: true}
}

// TIER 1 — where it was going. The order that was carrying this bin named a
// destination and somebody wanted the bin there; it is the best answer
// available and the recovery order should use it unchanged.
func TestRecoverCarriedBin_Tier1_UsesTheOriginalDestination(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewTrackingBackend()
	eng := newTestEngine(t, db, backend)

	dest := &nodes.Node{Name: "WAS-GOING-HERE", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dest), "create dest")
	bin := seedCarried(t, db, "AMR-R1", "WAS-GOING-HERE")
	cacheRobot(eng, dispatchableRobot("AMR-R1"))

	order, _, err := eng.RecoverCarriedBin(bin.ID, "operator:test")
	testutil.MustNoErr(t, err, "recover carried bin")

	if order.DeliveryNode != "WAS-GOING-HERE" {
		t.Errorf("destination = %q, want WAS-GOING-HERE (tier 1)", order.DeliveryNode)
	}
	if order.RobotID != "AMR-R1" {
		t.Errorf("order robot = %q, want AMR-R1 — an unpinned recovery order is a job any robot could take, and no other robot has the bin", order.RobotID)
	}

	// THE ORDER IS UNLOAD-ONLY AND PINNED, which is the whole shape. Read off
	// the fleet request rather than the order row: the row could carry the
	// right intent and still hand the fleet a two-step plan.
	reqs := backend.CreateRequests()
	if len(reqs) != 1 {
		t.Fatalf("fleet create requests = %d, want 1", len(reqs))
	}
	req := reqs[0]
	if req.Vehicle != "AMR-R1" {
		t.Errorf("CreateOrderRequest.Vehicle = %q, want AMR-R1 — without the pin the fleet sends any robot to unload a bin it is not carrying", req.Vehicle)
	}
	if len(req.Blocks) != 1 {
		t.Fatalf("blocks = %d (%+v), want exactly one — the robot already has the bin, so there is nothing to pick up", len(req.Blocks), req.Blocks)
	}
	if req.Blocks[0].Location != "WAS-GOING-HERE" {
		t.Errorf("block location = %q, want WAS-GOING-HERE", req.Blocks[0].Location)
	}
	if !strings.Contains(strings.ToLower(req.Blocks[0].BinTask), "unload") {
		t.Errorf("binTask = %q, want an unload — a load block would tell the robot to lift the bin it is about to set down", req.Blocks[0].BinTask)
	}

	// THE AUDIT ROW IS PART OF THE UNIT. A bin that moves has to be
	// explainable, and the tier is the answer to "why there".
	var action, actor, detail string
	testutil.MustNoErr(t, db.DB.QueryRow(
		`SELECT action, actor, detail FROM recovery_actions WHERE target_type='bin' AND target_id=$1
		 ORDER BY id DESC LIMIT 1`, bin.ID).Scan(&action, &actor, &detail),
		"read recovery action")
	if action != "carried_bin_recovery_ordered" {
		t.Errorf("action = %q — it must be distinguishable from the inference's transit_bin_on_robot, "+
			"because deduced and commanded are different things to read back", action)
	}
	if actor != "operator:test" {
		t.Errorf("actor = %q, want the caller's — a person pressed this", actor)
	}
	if !strings.Contains(detail, "tier 1") || !strings.Contains(detail, "AMR-R1") {
		t.Errorf("detail = %q, want the robot and the tier that chose the destination", detail)
	}
}

// TIER 2 — where a bin of its kind belongs. No usable original destination, so
// a free storage slot that accepts this payload.
func TestRecoverCarriedBin_Tier2_FallsToAFreeStorageSlot(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())

	payload := seedRecoveryPayload(t, db, "RECOV-P2")
	slot := seedStorageSlotFor(t, db, "STOR-FREE-2", payload)

	// The prior order's destination is OCCUPIED, so tier 1 declines. This is
	// the case tier 2 exists for, and using a node that simply does not exist
	// would not distinguish "tier 1 declined" from "tier 1 never ran".
	taken := &nodes.Node{Name: "TAKEN-2", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(taken), "create occupied dest")
	blocker := &bins.Bin{BinTypeID: seedBinType(t, db, "BLK-2"), Label: "blocker-2", NodeID: &taken.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(blocker), "create blocking bin")

	bin := seedCarried(t, db, "AMR-R2", "TAKEN-2")
	_, perr := db.DB.Exec(`UPDATE bins SET payload_code=$1 WHERE id=$2`, "RECOV-P2", bin.ID)
	testutil.MustNoErr(t, perr, "stamp payload on carried bin")
	cacheRobot(eng, dispatchableRobot("AMR-R2"))

	order, _, err := eng.RecoverCarriedBin(bin.ID, "operator:test")
	testutil.MustNoErr(t, err, "recover carried bin")
	if order.DeliveryNode != slot.Name {
		t.Errorf("destination = %q, want %s — tier 1's node is occupied, so a free storage slot for the payload",
			order.DeliveryNode, slot.Name)
	}
	var detail string
	testutil.MustNoErr(t, db.DB.QueryRow(
		`SELECT detail FROM recovery_actions WHERE target_type='bin' AND target_id=$1 ORDER BY id DESC LIMIT 1`,
		bin.ID).Scan(&detail), "read recovery action")
	if !strings.Contains(detail, "tier 2") {
		t.Errorf("detail = %q, want tier 2 named", detail)
	}
}

// TIER 3 — where the robot already is. No original destination and no storage
// slot; unloading where the robot is parked is the weakest answer and still far
// better than a bin nothing can reach.
func TestRecoverCarriedBin_Tier3_FallsToTheRobotsStation(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())

	here := &nodes.Node{Name: "PARKED-HERE-3", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(here), "create station node")

	// No prior destination at all, and a payload nothing has a slot for.
	bin := seedCarried(t, db, "AMR-R3", "")
	robot := dispatchableRobot("AMR-R3")
	robot.CurrentStation = "PARKED-HERE-3"
	cacheRobot(eng, robot)

	order, _, err := eng.RecoverCarriedBin(bin.ID, "operator:test")
	testutil.MustNoErr(t, err, "recover carried bin")
	if order.DeliveryNode != "PARKED-HERE-3" {
		t.Errorf("destination = %q, want PARKED-HERE-3 (tier 3)", order.DeliveryNode)
	}
}

// A ROBOT UNDER WAY IS NOT PARKED ANYWHERE, so tier 3 must decline for it —
// ResolveRobotStation falls back to LastStation, which for a moving robot is a
// node it PASSED. Unloading there is a bin left in an aisle.
func TestRecoverCarriedBin_Tier3_DeclinesForAMovingRobot(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())

	passed := &nodes.Node{Name: "PASSED-BY-3B", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(passed), "create station node")

	bin := seedCarried(t, db, "AMR-R3B", "")
	robot := dispatchableRobot("AMR-R3B")
	robot.Busy = true
	robot.LastStation = "PASSED-BY-3B"
	cacheRobot(eng, robot)

	if _, _, err := eng.RecoverCarriedBin(bin.ID, "operator:test"); err == nil {
		t.Fatal("want a refusal: a moving robot's last station is somewhere it drove past")
	} else if !strings.Contains(err.Error(), "nowhere to put it") {
		t.Errorf("refusal = %q, want the no-destination reason", err.Error())
	}
	assertNoRecoveryOrder(t, db, bin.ID)
}

// DISPATCHABLE ROBOTS ONLY. A pinned order does not fall through to another
// robot — it sits in the fleet's queue holding the bin's claim, which is worse
// than the state it was fixing.
func TestRecoverCarriedBin_RefusesUndispatchableRobots(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		mutate func(*fleet.RobotStatus)
		want   string
	}{
		{"out of the dispatch pool", func(r *fleet.RobotStatus) { r.Available = false }, "not dispatchable"},
		{"offline", func(r *fleet.RobotStatus) { r.Connected = false }, "offline"},
		{"emergency stop", func(r *fleet.RobotStatus) { r.Emergency = true }, "emergency"},
		{"error state", func(r *fleet.RobotStatus) { r.IsError = true }, "error state"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testdb.Open(t)
			eng := newTestEngine(t, db, testdb.NewTrackingBackend())
			id := "AMR-ND-" + strings.ReplaceAll(tc.name, " ", "")
			dest := &nodes.Node{Name: "DEST-" + id, Enabled: true}
			testutil.MustNoErr(t, db.CreateNode(dest), "create dest")
			bin := seedCarried(t, db, id, dest.Name)
			robot := dispatchableRobot(id)
			tc.mutate(&robot)
			cacheRobot(eng, robot)

			_, _, err := eng.RecoverCarriedBin(bin.ID, "operator:test")
			if err == nil {
				t.Fatal("want a refusal — the order would sit in the fleet queue forever")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal = %q, want it to name %q", err.Error(), tc.want)
			}
			assertNoRecoveryOrder(t, db, bin.ID)
		})
	}
}

// A robot Core has never heard from is the same answer for a different reason:
// nothing is known about whether it would take the order.
func TestRecoverCarriedBin_RefusesWithNoTelemetry(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())
	dest := &nodes.Node{Name: "DEST-NOTEL", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dest), "create dest")
	bin := seedCarried(t, db, "AMR-NOTEL", "DEST-NOTEL")
	// deliberately not cached

	if _, _, err := eng.RecoverCarriedBin(bin.ID, "operator:test"); err == nil ||
		!strings.Contains(err.Error(), "no telemetry") {
		t.Fatalf("want a no-telemetry refusal, got %v", err)
	}
	assertNoRecoveryOrder(t, db, bin.ID)
}

// A BIN AT _TRANSIT IS NOT THIS FUNCTION'S BUSINESS. Its location is unknown —
// the robot may have set it down hours ago — so pinning an unload to the robot
// that last carried it is a guess wearing an order's clothes. That population
// belongs to the A/B/C inference.
func TestRecoverCarriedBin_RefusesABinNotOnADeck(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())
	bin, _ := seedStranded(t, db, "AMR-TRANS")
	cacheRobot(eng, dispatchableRobot("AMR-TRANS"))

	if _, _, err := eng.RecoverCarriedBin(bin.ID, "operator:test"); err == nil ||
		!strings.Contains(err.Error(), "not on a robot's deck") {
		t.Fatalf("want a not-on-a-deck refusal, got %v", err)
	}
}

// IDEMPOTENT. A second press, or a sweep firing while the first order runs,
// must not put two unloads on one deck.
func TestRecoverCarriedBin_SecondCallIsRefused(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())
	dest := &nodes.Node{Name: "DEST-TWICE", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dest), "create dest")
	bin := seedCarried(t, db, "AMR-TWICE", "DEST-TWICE")
	cacheRobot(eng, dispatchableRobot("AMR-TWICE"))

	first, _, err := eng.RecoverCarriedBin(bin.ID, "operator:test")
	testutil.MustNoErr(t, err, "first recovery")
	_, _, err = eng.RecoverCarriedBin(bin.ID, "operator:test")
	if err == nil {
		t.Fatal("want a refusal on the second call")
	}
	if !strings.Contains(err.Error(), "already in flight") {
		t.Errorf("refusal = %q, want it to name the live order", err.Error())
	}
	_ = first
}

// THE CARRIER-NODE GUARD. While a recovery order is running, the jack watch
// must stand down: it would place the bin at whatever station the tick resolves
// to, racing the order's own arrival handling into a second placement.
func TestSweepCarriedBins_StandsDownForALiveRecoveryOrder(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())

	dest := &nodes.Node{Name: "DEST-GUARD", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dest), "create dest")
	elsewhere := &nodes.Node{Name: "ELSEWHERE-GUARD", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(elsewhere), "create the node the watch would pick")

	bin := seedCarried(t, db, "AMR-GUARD", "DEST-GUARD")
	cacheRobot(eng, dispatchableRobot("AMR-GUARD"))
	if _, _, err := eng.RecoverCarriedBin(bin.ID, "operator:test"); err != nil {
		t.Fatalf("recover carried bin: %v", err)
	}

	// Now the deck reports EMPTY at a DIFFERENT node, which is precisely the
	// race: without the guard the watch places the bin at ELSEWHERE-GUARD while
	// the order is still on its way to DEST-GUARD.
	robot := dispatchableRobot("AMR-GUARD")
	robot.JackState = 3
	robot.LiftHeight = -0.0001
	robot.CurrentStation = "ELSEWHERE-GUARD"
	cacheRobot(eng, robot)

	eng.sweepCarriedBins()

	if got := binNodeName(t, db, bin.ID); got != bins.CarrierNodePrefix+"AMR-GUARD" {
		t.Errorf("bin moved to %q during a live recovery order — the order's arrival is the placement, "+
			"and two placements is how a bin ends up recorded somewhere it is not", got)
	}
}

// ── helpers ──────────────────────────────────────────────────────────────

// seedRecoveryPayload creates a payload template the storage slot can be
// linked to. Tier 2 requires an EXPLICIT node_payloads link — a node that has
// not been told it accepts this payload is not a candidate — so both halves
// have to exist for the tier to fire at all.
func seedRecoveryPayload(t *testing.T, db *store.DB, code string) *payloads.Payload {
	t.Helper()
	p := &payloads.Payload{Code: code, Description: "recovery test payload", UOPCapacity: 100}
	testutil.MustNoErr(t, db.CreatePayload(p), "create payload "+code)
	return p
}

// seedStorageSlotFor creates an empty STOR-typed node linked to the payload.
func seedStorageSlotFor(t *testing.T, db *store.DB, name string, p *payloads.Payload) *nodes.Node {
	t.Helper()
	storType, err := db.GetNodeTypeByCode("STOR")
	if err != nil || storType == nil {
		storType = &nodes.NodeType{Code: "STOR", Name: "Storage Slot"}
		if cerr := db.CreateNodeType(storType); cerr != nil {
			existing, rerr := db.GetNodeTypeByCode("STOR")
			testutil.MustNoErr(t, rerr, "resolve STOR node type")
			storType = existing
		}
	}
	n := &nodes.Node{Name: name, Enabled: true, NodeTypeID: &storType.ID}
	testutil.MustNoErr(t, db.CreateNode(n), "create storage slot "+name)
	testutil.MustNoErr(t, db.AssignPayloadToNode(n.ID, p.ID), "link payload to slot")
	return n
}

func seedBinType(t *testing.T, db *store.DB, code string) int64 {
	t.Helper()
	bt := &bins.BinType{Code: code, Description: "tote"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "create bin type "+code)
	return bt.ID
}

func assertNoRecoveryOrder(t *testing.T, db *store.DB, binID int64) {
	t.Helper()
	var n int
	testutil.MustNoErr(t, db.DB.QueryRow(
		`SELECT count(*) FROM orders WHERE bin_id=$1 AND source_intent='on_deck'`, binID).Scan(&n),
		"count recovery orders")
	if n != 0 {
		t.Errorf("%d recovery order(s) created for bin %d — a refusal must leave nothing behind", n, binID)
	}
}
