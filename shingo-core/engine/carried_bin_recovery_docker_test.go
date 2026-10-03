//go:build docker

package engine

import (
	"errors"
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

// seedCarried puts an EMPTY bin on a robot's carrier node, the state
// parkOnCarrier leaves behind, with the cancelled order that was taking it to
// PRESS-<robot>. When place is not blank, a produce claim at that press
// declares place as where its empties come from — the declaration the Return
// button sends the bin to. Blank declares nothing, and the button holds.
func seedCarried(t *testing.T, db *store.DB, robotID, place string) *bins.Bin {
	t.Helper()
	carrier := &nodes.Node{Name: bins.CarrierNodePrefix + robotID, IsSynthetic: true, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(carrier), "create carrier node")

	bt := &bins.BinType{Code: "CR-" + robotID, Description: "tote"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "create bin type")
	bin := &bins.Bin{BinTypeID: bt.ID, Label: "carried-" + robotID, NodeID: &carrier.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(bin), "create bin")

	press := "PRESS-" + robotID
	ord := &orders.Order{
		EdgeUUID: "carried-" + robotID, StationID: "edge.test", OrderType: "retrieve",
		Status: protocol.StatusCancelled, Quantity: 1, DeliveryNode: press, ProcessNode: press,
		RobotID: robotID, BinID: &bin.ID,
	}
	testutil.MustNoErr(t, db.CreateOrder(ord), "create prior order")
	_, err := db.DB.Exec(`UPDATE orders SET robot_id=$1, bin_id=$2, status='cancelled' WHERE id=$3`,
		robotID, bin.ID, ord.ID)
	testutil.MustNoErr(t, err, "set robot, bin and terminal status")
	if place != "" {
		seedRoleClaim(t, db, "PROC-"+robotID, press, protocol.ClaimRoleProduce, "", place, "")
	}
	return bin
}

// seedCarriedHome is seedCarried with a store group of one free slot declared
// as the bin's place. It returns the slot the bin is returned into.
func seedCarriedHome(t *testing.T, db *store.DB, robotID string) (*bins.Bin, *nodes.Node) {
	t.Helper()
	grp, slot := storeGroupWithSlot(t, db, robotID)
	return seedCarried(t, db, robotID, grp.Name), slot
}

// dispatchableRobot is a robot the fleet will actually take an order for:
// connected, in the dispatch pool, parked, no faults.
func dispatchableRobot(id string) fleet.RobotStatus {
	return fleet.RobotStatus{VehicleID: id, Connected: true, Available: true}
}

// THE BUTTON IS THE SAME CHOOSER AS THE CANCEL-RETURN WATCH (chooseDeclared).
// A FULL bin goes to the inbound source a claim names for its payload, and the
// order is the pinned unload-only one.
func TestRecoverCarriedBin_AFullBinGoesToItsClaimsSourceGroup(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewTrackingBackend()
	eng := newTestEngine(t, db, backend)

	grp, slot := storeGroupWithSlot(t, db, "RCB-FULL")
	seedClaim(t, db, "PROC-RCB-FULL", "STY", "LINE-RCB-FULL", "RCB-FULL-P", grp.Name, true)
	bin, carrier := seedCancelledCarry(t, db, "AMR-R1", "RCB-FULL-P", "LINE-RCB-FULL")
	cacheRobot(eng, dispatchableRobot("AMR-R1"))

	order, detail, err := eng.RecoverCarriedBin(bin.ID, "operator:test")
	testutil.MustNoErr(t, err, "recover carried bin")

	if order.DeliveryNode != slot.Name {
		t.Errorf("destination = %q, want %s in %s, the group the claim sources RCB-FULL-P from",
			order.DeliveryNode, slot.Name, grp.Name)
	}
	if order.RobotID != "AMR-R1" {
		t.Errorf("order robot = %q, want AMR-R1 — an unpinned recovery order is a job any robot could take, and no other robot has the bin", order.RobotID)
	}
	// The bin's last order is over, so the press is that order's return: the
	// watch's restart guard and the station's notice read it as such.
	if order.RecoversOrderID == nil || *order.RecoversOrderID != carrier.ID {
		t.Errorf("recovers_order_id = %v, want the bin's cancelled order %d", order.RecoversOrderID, carrier.ID)
	}
	want := "AMR-R1 unloads at " + slot.Name + " (returned to " + grp.Name + ", where "
	if !strings.Contains(detail, want) {
		t.Errorf("detail = %q, want it to contain %q — the robot, the node and the declaration that chose it", detail, want)
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
	if !strings.Contains(strings.ToLower(req.Blocks[0].BinTask), "unload") {
		t.Errorf("binTask = %q, want an unload — a load block would tell the robot to lift the bin it is about to set down", req.Blocks[0].BinTask)
	}

	// THE AUDIT ROW IS PART OF THE UNIT, under the button's own verb and the
	// person's name.
	action, actor, audit := lastBinAction(t, db, bin.ID)
	if action != "carried_bin_recovery_ordered" {
		t.Errorf("action = %q — it must be distinguishable from the inference's transit_bin_on_robot, "+
			"because deduced and commanded are different things to read back", action)
	}
	if actor != "operator:test" {
		t.Errorf("actor = %q, want the caller's — a person pressed this", actor)
	}
	if audit != detail {
		t.Errorf("audit detail %q differs from the answer the person was shown %q", audit, detail)
	}
}

// An EMPTY bin goes to the place a claim declares empties of its type go to or
// come from.
func TestRecoverCarriedBin_AnEmptyGoesToItsDeclaredPlace(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())

	bin, slot := seedCarriedHome(t, db, "AMR-R2")
	cacheRobot(eng, dispatchableRobot("AMR-R2"))

	order, detail, err := eng.RecoverCarriedBin(bin.ID, "operator:test")
	testutil.MustNoErr(t, err, "recover carried bin")
	if order.DeliveryNode != slot.Name {
		t.Errorf("destination = %q, want %s, the press's declared empties-in", order.DeliveryNode, slot.Name)
	}
	if !strings.Contains(detail, "a declared place for an empty") {
		t.Errorf("detail = %q, want it to say the place was declared", detail)
	}
}

// NOTHING DECLARED, NOTHING MOVES. The refusal is the watch's hold sentence,
// unchanged, and it names every place tried; no order is created, not even a
// failed one.
func TestRecoverCarriedBin_NoDeclarationHolds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, robot, place, want string
	}{
		{"no claim declares a place", "AMR-R3", "",
			"no claim declares a place for an empty CR-AMR-R3"},
		{"every declared place refuses", "AMR-R3B", "R3B-DOCK",
			"every place an empty CR-AMR-R3B is sourced from refused it (R3B-DOCK: it is a single position, not a store group or a loader position)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testdb.Open(t)
			eng := newTestEngine(t, db, testdb.NewTrackingBackend())
			if tc.place != "" {
				dock := &nodes.Node{Name: tc.place, Enabled: true}
				testutil.MustNoErr(t, db.CreateNode(dock), "create the declared single position")
			}
			bin := seedCarried(t, db, tc.robot, tc.place)
			cacheRobot(eng, dispatchableRobot(tc.robot))
			before := orderCount(t, db)

			_, _, err := eng.RecoverCarriedBin(bin.ID, "operator:test")
			var refusal *CarriedBinNotRecoverable
			if !errors.As(err, &refusal) {
				t.Fatalf("want a refusal, got %v", err)
			}
			if refusal.Reason != tc.want {
				t.Errorf("refusal = %q, want the hold sentence verbatim: %q", refusal.Reason, tc.want)
			}
			if after := orderCount(t, db); after != before {
				t.Errorf("orders %d -> %d — a hold creates nothing", before, after)
			}
		})
	}
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
			bin, _ := seedCarriedHome(t, db, id)
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
	bin, _ := seedCarriedHome(t, db, "AMR-NOTEL")
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
	bin, _ := seedCarriedHome(t, db, "AMR-TWICE")
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

	elsewhere := &nodes.Node{Name: "ELSEWHERE-GUARD", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(elsewhere), "create the node the watch would pick")

	bin, _ := seedCarriedHome(t, db, "AMR-GUARD")
	cacheRobot(eng, dispatchableRobot("AMR-GUARD"))
	if _, _, err := eng.RecoverCarriedBin(bin.ID, "operator:test"); err != nil {
		t.Fatalf("recover carried bin: %v", err)
	}

	// Now the deck reports EMPTY at a DIFFERENT node, which is precisely the
	// race: without the guard the watch places the bin at ELSEWHERE-GUARD while
	// the order is still on its way to the declared slot.
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

// seedRecoveryPayload creates a payload template.
func seedRecoveryPayload(t *testing.T, db *store.DB, code string) *payloads.Payload {
	t.Helper()
	p := &payloads.Payload{Code: code, Description: "recovery test payload", UOPCapacity: 100}
	testutil.MustNoErr(t, db.CreatePayload(p), "create payload "+code)
	return p
}

func orderCount(t *testing.T, db *store.DB) int {
	t.Helper()
	var n int
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT count(*) FROM orders`).Scan(&n), "count orders")
	return n
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
