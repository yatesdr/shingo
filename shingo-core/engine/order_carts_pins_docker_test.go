//go:build docker

package engine

// order_carts_pins_docker_test.go — U0 of the order-carts stream: an order's
// carts are order_bins rows, one per lift, kept after the order ends. Each pin
// names what it asserts at main f8f22f19 (red or green) in its header; the
// predictions are in the design folder's PINS.md
// (press6-buffer-design-2026-10-30). The rows are read with raw SQL so every
// pin compiles at the pre-change tree.

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/domain"
	"shingocore/fleet"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// cartsSwap is the two-cart swap the stream is about, dispatched through the
// real intake on the simulator: pick A at P, drop A at W, pick B at S, drop B at
// P. P is the process node, so the allocator claims A at P and B at S and
// orders.bin_id names A for the order's whole life.
type cartsSwap struct {
	db    *store.DB
	eng   *Engine
	sim   *simulator.SimulatorBackend
	uuid  string
	order *orders.Order
	a, b  *bins.Bin
	p     *nodes.Node // the process node: A's pick, B's drop
	w     *nodes.Node // A's drop
	s     *nodes.Node // B's pick
}

func newCartsSwap(t *testing.T, uuid string) *cartsSwap {
	t.Helper()
	return newCartsSwapOn(t, testDB(t), uuid)
}

// newCartsSwapOn is newCartsSwap on a database the caller opened (a cost pin
// opens its own, to count through a second handle on it).
func newCartsSwapOn(t *testing.T, db *store.DB, uuid string) *cartsSwap {
	t.Helper()
	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	s, p, _, _, w, bp := setupProductionNodes(t, db)
	c := &cartsSwap{db: db, eng: eng, sim: sim, uuid: uuid, p: p, w: w, s: s}
	c.a = createTestBinAtNode(t, db, bp.Code, p.ID, "CART-A")
	c.b = createTestBinAtNode(t, db, bp.Code, s.ID, "CART-B")
	eng.Dispatcher().HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID:   uuid,
		PayloadCode: bp.Code,
		Quantity:    1,
		ProcessNode: p.Name,
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: p.Name},
			{Action: "dropoff", Node: w.Name},
			{Action: "pickup", Node: s.Name},
			{Action: "dropoff", Node: p.Name},
		},
	})
	c.order = testdb.RequireOrder(t, db, uuid)
	if c.order.VendorOrderID == "" {
		t.Fatalf("the swap did not dispatch (status %s)", c.order.Status)
	}
	return c
}

// block completes the swap's block at node for the given kind (pickup when
// lift, else dropoff), as the fleet reports a FINISHED block mid-flight.
func (c *cartsSwap) block(t *testing.T, node string, lift bool) {
	t.Helper()
	for _, b := range c.sim.BlocksForOrder(c.order.VendorOrderID) {
		if strings.TrimSpace(b.Location) != node {
			continue
		}
		if lift && IsPickupBlock(b.BinTask) || !lift && IsDropoffBlock(b.BinTask) {
			if !c.sim.CompleteBlock(c.order.VendorOrderID, b.BlockID, b.Location, b.BinTask, 0, 0) {
				t.Fatalf("block %s @ %s was not emitted", b.BlockID, b.Location)
			}
			return
		}
	}
	t.Fatalf("no %s block at %s in the swap's plan", map[bool]string{true: "pickup", false: "dropoff"}[lift], node)
}

func (c *cartsSwap) cancel(t *testing.T) {
	t.Helper()
	c.eng.Dispatcher().HandleOrderCancel(testEnvelope(), &protocol.OrderCancel{OrderUUID: c.uuid, Reason: "pin"})
	testdb.RequireOrderStatus(t, c.db, c.uuid, "cancelled")
}

func (c *cartsSwap) confirm(t *testing.T) {
	t.Helper()
	c.sim.DriveState(c.order.VendorOrderID, "RUNNING")
	c.sim.DriveState(c.order.VendorOrderID, "FINISHED")
	c.eng.Dispatcher().HandleOrderReceipt(testEnvelope(), &protocol.OrderReceipt{
		OrderUUID: c.uuid, ReceiptType: "confirmed", FinalCount: 1,
	})
	testdb.RequireOrderStatus(t, c.db, c.uuid, "confirmed")
}

// TestOrderCartsPin_ASwapKeepsBothCartsRowsAfterConfirm: a two-cart swap that
// completes keeps a row for A and a row for B. RED at main: the terminal
// release deletes the junction (store/orders.go:1711) and the completion net
// defers DeleteOrderBins (wiring_completion.go:790-792).
func TestOrderCartsPin_ASwapKeepsBothCartsRowsAfterConfirm(t *testing.T) {
	t.Parallel()
	c := newCartsSwap(t, "carts-confirm")
	c.block(t, c.p.Name, true)
	c.block(t, c.w.Name, false)
	c.block(t, c.s.Name, true)
	c.confirm(t)

	if got := binNodeName(t, c.db, c.a.ID); got != c.w.Name {
		t.Fatalf("A is recorded at %s, want %s (its drop)", got, c.w.Name)
	}
	if got := binNodeName(t, c.db, c.b.ID); got != c.p.Name {
		t.Fatalf("B is recorded at %s, want %s (its drop)", got, c.p.Name)
	}
	rows := testdb.CartRowBins(t, c.db, c.order.ID)
	if !slices.Contains(rows, c.a.ID) || !slices.Contains(rows, c.b.ID) {
		t.Fatalf("the confirmed swap's carts are %v, want rows for A (%d) and B (%d)", rows, c.a.ID, c.b.ID)
	}
}

// TestOrderCartsPin_ASwapKeepsBothCartsRowsAfterCancel: cancelled with B on the
// robot, the swap still has A's row and B's row. RED at main (the terminal
// release deletes them).
func TestOrderCartsPin_ASwapKeepsBothCartsRowsAfterCancel(t *testing.T) {
	t.Parallel()
	c := newCartsSwap(t, "carts-cancel")
	c.block(t, c.p.Name, true)
	c.block(t, c.w.Name, false)
	c.block(t, c.s.Name, true)
	c.cancel(t)

	rows := testdb.CartRowBins(t, c.db, c.order.ID)
	if !slices.Contains(rows, c.a.ID) || !slices.Contains(rows, c.b.ID) {
		t.Fatalf("the cancelled swap's carts are %v, want rows for A (%d) and B (%d)", rows, c.a.ID, c.b.ID)
	}
}

// TestOrderCartsPin_CancelWithBOnTheRobotFindsBsOrder: cancelled with B on the
// robot, B's last order is the swap, on the swap's robot. RED at main: the
// cart's last order is read from orders.bin_id (lastClaimingOrder →
// ListOrdersByBin), which names A, so B reads the older order that brought it
// to S.
func TestOrderCartsPin_CancelWithBOnTheRobotFindsBsOrder(t *testing.T) {
	t.Parallel()
	c := newCartsSwap(t, "carts-b-on-robot")
	older := testdb.CreateOrder(t, c.db, func(o *orders.Order) {
		o.OrderType, o.Status, o.BinID, o.DeliveryNode = protocol.OrderTypeMove, protocol.StatusConfirmed, &c.b.ID, c.s.Name
	})
	c.block(t, c.p.Name, true)
	c.block(t, c.w.Name, false)
	c.block(t, c.s.Name, true)
	c.cancel(t)

	if got := binNodeName(t, c.db, c.b.ID); got != domain.TransitNodeName {
		t.Fatalf("B is recorded at %s, want %s (on the robot)", got, domain.TransitNodeName)
	}
	ord, _, ok := c.eng.lastClaimingOrder(c.b.ID)
	if !ok || ord.ID != c.order.ID {
		got := int64(0)
		if ok {
			got = ord.ID
		}
		t.Fatalf("B's last order = %d (the older order is %d), want the swap %d", got, older.ID, c.order.ID)
	}
}

// TestOrderCartsPin_TwoCartsInFlightKeepTheirOrder is finding 6's state: A's
// set-down at W never recorded, B lifted, then CANCEL. Both carts stand on
// _TRANSIT and each keeps the swap as its last order. RED at main for B (its
// last order is the older one). The brief runs it ×10 (-count=10).
func TestOrderCartsPin_TwoCartsInFlightKeepTheirOrder(t *testing.T) {
	t.Parallel()
	c := newCartsSwap(t, "carts-two-in-flight")
	testdb.CreateOrder(t, c.db, func(o *orders.Order) {
		o.OrderType, o.Status, o.BinID, o.DeliveryNode = protocol.OrderTypeMove, protocol.StatusConfirmed, &c.b.ID, c.s.Name
	})
	c.block(t, c.p.Name, true)
	c.block(t, c.s.Name, true) // A's drop at W lost
	c.cancel(t)

	for name, id := range map[string]int64{"A": c.a.ID, "B": c.b.ID} {
		if got := binNodeName(t, c.db, id); got != domain.TransitNodeName {
			t.Fatalf("%s is recorded at %s, want %s", name, got, domain.TransitNodeName)
		}
		ord, _, ok := c.eng.lastClaimingOrder(id)
		if !ok || ord.ID != c.order.ID {
			t.Fatalf("%s's last order is not the swap %d (ok=%v)", name, c.order.ID, ok)
		}
	}
}

// TestOrderCartsPin_ReapplyMovesNothingOnALandedSwap: a confirmed two-cart swap
// whose completion was never stamped, with A at W and B at P. The repair finds
// both carts where their drops put them and moves nothing. RED at main: the
// repair reads orders.bin_id (A) and order.DeliveryNode (P), moves A onto P and
// evicts B (recovery_service.go:39-62, store/recovery PlaceBinTx).
func TestOrderCartsPin_ReapplyMovesNothingOnALandedSwap(t *testing.T) {
	t.Parallel()
	c := newCartsSwap(t, "carts-reapply")
	c.block(t, c.p.Name, true)
	c.block(t, c.w.Name, false)
	c.block(t, c.s.Name, true)
	c.confirm(t)
	_, err := c.db.DB.Exec(`UPDATE orders SET completed_at=NULL WHERE id=$1`, c.order.ID)
	testutil.MustNoErr(t, err, "unstamp the completion")

	err = c.eng.ReapplyOrderCompletion(c.order.ID, "pin")
	if got := binNodeName(t, c.db, c.a.ID); got != c.w.Name {
		t.Fatalf("after the repair (err=%v) A is at %s, want %s", err, got, c.w.Name)
	}
	if got := binNodeName(t, c.db, c.b.ID); got != c.p.Name {
		t.Fatalf("after the repair (err=%v) B is at %s, want %s", err, got, c.p.Name)
	}
}

// TestOrderCartsPin_APickupAfterTheEndLiftsItsRowsCart: A lifted and set down,
// the swap cancelled before B's lift, then the pickup block at S arrives late
// (a Core cancel untracks nothing; rds/poller.go diffs blocks before the state
// check). The lift is recorded where the cart physically is — B on _TRANSIT,
// unclaimed — and the sweep owns it. RED at main: the junction is gone, so the
// fallback names A (bin_id), which is not at S: "not lifting it", and B stays
// recorded at S while it rides the robot.
func TestOrderCartsPin_APickupAfterTheEndLiftsItsRowsCart(t *testing.T) {
	t.Parallel()
	c := newCartsSwap(t, "carts-late-pickup")
	c.block(t, c.p.Name, true)
	c.block(t, c.w.Name, false)
	c.cancel(t)
	c.eng.handleBlockCompleted(BlockCompletedEvent{
		OrderID: c.order.ID, VendorOrderID: c.order.VendorOrderID,
		BlockID: c.uuid + "-late", Location: c.s.Name, BinTask: "JackLoad",
	})

	b, err := c.db.GetBin(c.b.ID)
	testutil.MustNoErr(t, err, "read B")
	if b.NodeName != domain.TransitNodeName {
		t.Fatalf("after the late pickup at %s, B is recorded at %s, want %s", c.s.Name, b.NodeName, domain.TransitNodeName)
	}
	if b.ClaimedBy != nil {
		t.Fatalf("B is claimed by %d after its order ended, want unclaimed (the sweep owns it)", *b.ClaimedBy)
	}
}

// TestOrderCartsPin_AnUnreadableCartsReadFailsClosed: the settle cannot read the
// order's carts, and it moves nothing. RED at main: `orderBins, _ :=`
// (wiring_completion.go:244) reads an error as "no rows", takes the single-cart
// path and moves bin_id's cart (A) to the delivery node.
func TestOrderCartsPin_AnUnreadableCartsReadFailsClosed(t *testing.T) {
	t.Parallel()
	c := newCartsSwap(t, "carts-unreadable")
	c.block(t, c.p.Name, true)
	c.block(t, c.w.Name, false)
	c.block(t, c.s.Name, true)
	_, err := c.db.DB.Exec(`ALTER TABLE order_bins RENAME TO order_bins_unreadable`)
	testutil.MustNoErr(t, err, "make the carts unreadable")
	c.sim.DriveState(c.order.VendorOrderID, "RUNNING")
	c.sim.DriveState(c.order.VendorOrderID, "FINISHED")

	if got := binNodeName(t, c.db, c.a.ID); got == c.p.Name {
		t.Fatalf("with the carts unreadable the settle moved A (bin_id) onto %s", got)
	}
}

// TestOrderCartsPin_TheGuardRefusesAHandMoveOfBOnTheRobot: B rides the live
// swap's robot; a hand move and a retire are refused, naming the swap. RED at
// main: the guard asks orders.bin_id (ActiveByBinID), which names A, so B passes
// (service/bin_service.go guardNoOrphanedOrder).
func TestOrderCartsPin_TheGuardRefusesAHandMoveOfBOnTheRobot(t *testing.T) {
	t.Parallel()
	c := newCartsSwap(t, "carts-guard")
	c.block(t, c.p.Name, true)
	c.block(t, c.w.Name, false)
	c.block(t, c.s.Name, true)
	spare := &nodes.Node{Name: "SPARE-SLOT", Enabled: true}
	testutil.MustNoErr(t, c.db.CreateNode(spare), "create a free node")

	b, err := c.db.GetBin(c.b.ID)
	testutil.MustNoErr(t, err, "read B")
	if _, err := c.eng.BinService().MoveByHand(b, spare.ID); err == nil || !strings.Contains(err.Error(), "committed to order") {
		t.Fatalf("a hand move of B on the robot = %v, want refused as committed to the swap %d", err, c.order.ID)
	}
	if err := c.eng.BinService().Retire(c.b.ID); err == nil || !strings.Contains(err.Error(), "committed to order") {
		t.Fatalf("a retire of B on the robot = %v, want refused as committed to the swap %d", err, c.order.ID)
	}
}

// TestOrderCartsPin_ALostStoreEventStillResolvesTheNextSetDown is the
// single_robot swap with its outbound-staging store event lost: the fresh
// cart F is re-lifted at inbound staging while the old cart O, never recorded
// as set down, still stands on _TRANSIT. The set-down at the line is F's, by
// its lift's drop step. RED at main: resolveDropoffBin counts two claimed carts
// on _TRANSIT and declines, so F is not recorded on the line.
func TestOrderCartsPin_ALostStoreEventStillResolvesTheNextSetDown(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	storage, line, inStage, outStage, outDest, bp := setupProductionNodes(t, db)
	fresh := createTestBinAtNode(t, db, bp.Code, storage.ID, "CART-FRESH")
	old := createTestBinAtNode(t, db, bp.Code, line.ID, "CART-OLD")
	eng.Dispatcher().HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID: "carts-single-robot", PayloadCode: bp.Code, Quantity: 1, ProcessNode: line.Name,
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: storage.Name},   // 0 lift F
			{Action: "dropoff", Node: inStage.Name},  // 1 F at inbound staging
			{Action: "wait", Node: line.Name},        // 2
			{Action: "pickup", Node: line.Name},      // 3 lift O
			{Action: "dropoff", Node: outStage.Name}, // 4 O at outbound staging: event lost
			{Action: "pickup", Node: inStage.Name},   // 5 re-lift F
			{Action: "dropoff", Node: line.Name},     // 6 F on the line
			{Action: "pickup", Node: outStage.Name},  // 7 re-lift O
			{Action: "dropoff", Node: outDest.Name},  // 8 O out
		},
	})
	order := testdb.RequireOrder(t, db, "carts-single-robot")
	if order.VendorOrderID == "" {
		t.Fatalf("the single_robot swap did not dispatch: %s (%s)", order.Status, order.QueueReason)
	}
	c := &cartsSwap{db: db, eng: eng, sim: sim, uuid: "carts-single-robot", order: order}
	c.block(t, storage.Name, true)
	c.block(t, inStage.Name, false)
	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "WAITING")
	eng.Dispatcher().HandleOrderRelease(testEnvelope(), &protocol.OrderRelease{OrderUUID: "carts-single-robot"})
	c.block(t, line.Name, true)
	c.block(t, inStage.Name, true)
	c.block(t, line.Name, false)

	if got := binNodeName(t, db, fresh.ID); got != line.Name {
		t.Fatalf("F is recorded at %s after the set-down at the line, want %s", got, line.Name)
	}
	if got := binNodeName(t, db, old.ID); got != domain.TransitNodeName {
		t.Fatalf("O is recorded at %s, want %s (its set-down was never reported)", got, domain.TransitNodeName)
	}
}

// TestOrderCartsPin_PlacementIntentIsTheCartsOwnDrop: the swap cancelled with
// A set down at W; where A was going is W, its own drop, not the order's
// delivery node P. RED at main: placementIntent reads the last order's
// delivery_node (stranded_transit.go:385-393).
func TestOrderCartsPin_PlacementIntentIsTheCartsOwnDrop(t *testing.T) {
	t.Parallel()
	c := newCartsSwap(t, "carts-intent")
	c.block(t, c.p.Name, true)
	c.block(t, c.w.Name, false)
	c.block(t, c.s.Name, true)
	c.cancel(t)

	if got := c.eng.placementIntent(c.a.ID); got != c.w.Name {
		t.Fatalf("A's placement intent = %q, want %q (its own drop)", got, c.w.Name)
	}
	if got := c.eng.placementIntent(c.b.ID); got != c.p.Name {
		t.Fatalf("B's placement intent = %q, want %q (its own drop)", got, c.p.Name)
	}
}

// TestOrderCartsPin_LiftProofIsTheCartsOwnLift: A lifted by the swap's robot,
// the swap cancelled, and B put on _TRANSIT by something other than a lift (a
// ghost eviction, a hand move): B's lift proof is B's own pick, which never
// happened, so the sweep names the swap and says no pickup is on record for
// B. RED at main: B's last order is not the swap at all (bin_id names A).
func TestOrderCartsPin_LiftProofIsTheCartsOwnLift(t *testing.T) {
	t.Parallel()
	c := newCartsSwap(t, "carts-lift-proof")
	c.block(t, c.p.Name, true)
	c.cancel(t)
	var transitID int64
	testutil.MustNoErr(t, c.db.DB.QueryRow(`SELECT id FROM nodes WHERE name=$1`, domain.TransitNodeName).Scan(&transitID), "find _TRANSIT")
	_, err := c.db.DB.Exec(`UPDATE bins SET node_id=$1, claimed_by=NULL WHERE id=$2`, transitID, c.b.ID)
	testutil.MustNoErr(t, err, "put B on _TRANSIT without a lift")

	ord, _, ok := c.eng.lastClaimingOrder(c.b.ID)
	if !ok || ord.ID != c.order.ID {
		t.Fatalf("B's last order is not the swap %d (ok=%v)", c.order.ID, ok)
	}
	c.eng.placeStrandedBin(c.b.ID, "", fleet.RobotStatus{}, false, false)
	var note string
	testutil.MustNoErr(t, c.db.DB.QueryRow(`SELECT COALESCE(anomaly_note,'') FROM bins WHERE id=$1`, c.b.ID).Scan(&note), "read B's note")
	if !strings.Contains(note, "no robot pickup on record") {
		t.Fatalf("B's anomaly note = %q, want it to say no pickup is on record for B", note)
	}
}

// TestOrderCartsPin_TheBinsFeedShipsTheCartThatDropsAtTheWatchedNode: the
// live swap delivers B to P; the inbound read the bins feed ships to the Edge
// (NodeBinsInbound.BinType) names B's type for P, not A's. RED at main: the read
// joins COALESCE(bin_id, named_bin_id), which is A (orders.go:1796).
func TestOrderCartsPin_TheBinsFeedShipsTheCartThatDropsAtTheWatchedNode(t *testing.T) {
	t.Parallel()
	c := newCartsSwap(t, "carts-feed")
	other := &bins.BinType{Code: "CARTS-OTHER", Description: "the other type"}
	testutil.MustNoErr(t, c.db.CreateBinType(other), "create B's type")
	_, err := c.db.DB.Exec(`UPDATE bins SET bin_type_id=$1 WHERE id=$2`, other.ID, c.b.ID)
	testutil.MustNoErr(t, err, "give B its own type")

	in, err := orders.ListInFlightByDeliveryNodes(c.db.DB, []string{c.p.Name})
	testutil.MustNoErr(t, err, "the inbound read")
	if len(in) != 1 || in[0].OrderID != c.order.ID {
		t.Fatalf("inbound to %s = %+v, want the swap %d once", c.p.Name, in, c.order.ID)
	}
	if in[0].BinTypeCode != other.Code {
		t.Fatalf("the feed ships %q for %s, want %q (B, the cart that drops there)", in[0].BinTypeCode, c.p.Name, other.Code)
	}
}

// TestOrderCartsPin_ASingleClaimComplexOrderWritesItsRow: a complex order that
// claims one cart writes that cart's row at its confirm, with the cart's pick
// and drop. RED at main: the allocator writes rows only for len(claimed) > 1
// (allocator.go:1176).
func TestOrderCartsPin_ASingleClaimComplexOrderWritesItsRow(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	storage, line, _, _, _, bp := setupProductionNodes(t, db)
	cart := createTestBinAtNode(t, db, bp.Code, storage.ID, "CART-ONE")
	eng.Dispatcher().HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID: "carts-single-claim", PayloadCode: bp.Code, Quantity: 1, ProcessNode: line.Name,
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: storage.Name},
			{Action: "dropoff", Node: line.Name},
		},
	})
	o := testdb.RequireOrder(t, db, "carts-single-claim")
	if o.VendorOrderID == "" {
		t.Fatalf("the order did not dispatch: %s (%s)", o.Status, o.QueueReason)
	}
	if !testdb.HasCartRow(t, db, o.ID, cart.ID) {
		t.Fatalf("the single-claim complex order has no row for its cart %d (rows %v)", cart.ID, testdb.CartRowBins(t, db, o.ID))
	}
}

// TestOrderCartsPin_TheCancelReturnRecoversBThroughItsRow: cancelled with B on
// the robot, the deck loaded and the robot free: the return is the swap's (a
// return order recovering the swap, or a hold naming it). RED at main: the
// cancel return reads B's orders by bin_id (cancel_return.go:83), finds the
// older confirmed order, and stands down.
func TestOrderCartsPin_TheCancelReturnRecoversBThroughItsRow(t *testing.T) {
	t.Parallel()
	c := newCartsSwap(t, "carts-return")
	testdb.CreateOrder(t, c.db, func(o *orders.Order) {
		o.OrderType, o.Status, o.BinID, o.DeliveryNode = protocol.OrderTypeMove, protocol.StatusConfirmed, &c.b.ID, c.s.Name
	})
	c.block(t, c.p.Name, true)
	c.block(t, c.w.Name, false)
	c.block(t, c.s.Name, true)
	_, err := c.db.DB.Exec(`UPDATE orders SET robot_id='AMR-CARTS' WHERE id=$1`, c.order.ID)
	testutil.MustNoErr(t, err, "the swap's robot")
	c.cancel(t)
	robot := loadedDispatchable("AMR-CARTS")
	cacheRobot(c.eng, robot)
	b, err := c.db.GetBin(c.b.ID)
	testutil.MustNoErr(t, err, "read B")

	c.eng.maybeReturnCarriedBin(b, "AMR-CARTS", robot, true)

	var returns, held int
	testutil.MustNoErr(t, c.db.DB.QueryRow(`SELECT count(*) FROM orders WHERE recovers_order_id=$1`, c.order.ID).Scan(&returns), "returns")
	testutil.MustNoErr(t, c.db.DB.QueryRow(`SELECT count(*) FROM recovery_actions
		WHERE target_type='bin' AND target_id=$1 AND detail LIKE $2`, c.b.ID, fmt.Sprintf("order %d cancelled%%", c.order.ID)).Scan(&held), "holds")
	if returns+held == 0 {
		t.Fatalf("the cancel return did nothing for B: no return recovers the swap %d and no hold names it", c.order.ID)
	}
}

// TestOrderCartsPin_ALateFinishedOnACancelledSwapMovesNothing: the fleet's
// FINISHED for the swap arrives after the swap was cancelled with B on the
// robot. Nothing is settled: A stays at W, B stays on _TRANSIT for the sweep.
// GREEN at main (the claim guard refuses an ended order's cargo), and it stays
// green when the settle reads the order's rows.
func TestOrderCartsPin_ALateFinishedOnACancelledSwapMovesNothing(t *testing.T) {
	t.Parallel()
	c := newCartsSwap(t, "carts-late-finished")
	c.block(t, c.p.Name, true)
	c.block(t, c.w.Name, false)
	c.block(t, c.s.Name, true)
	c.cancel(t)
	o, err := c.db.GetOrder(c.order.ID)
	testutil.MustNoErr(t, err, "reload the swap")

	c.eng.handleOrderDelivered(o)

	if got := binNodeName(t, c.db, c.a.ID); got != c.w.Name {
		t.Errorf("a late FINISHED moved A to %s, want it left at %s", got, c.w.Name)
	}
	if got := binNodeName(t, c.db, c.b.ID); got != domain.TransitNodeName {
		t.Errorf("a late FINISHED moved B to %s, want it left on %s", got, domain.TransitNodeName)
	}
}
