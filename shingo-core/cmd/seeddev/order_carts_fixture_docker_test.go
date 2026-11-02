//go:build docker

package main

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/config"
	"shingocore/domain"
	"shingocore/engine"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/plantspec"
	"shingocore/store"
	"shingocore/store/orders"
)

// order_carts_fixture_docker_test.go — plants/order-carts.yaml, the multi-cart
// fixture, seeded and driven end to end: the dispatcher's intake and allocation,
// the simulator's blocks, the engine's lifts, set-downs, completion and cancel.
//
// The cells are the plant's: PRESS-1's press-index swap (R2 lifts the on-deck
// cart onto the press and an empty onto the back position: two carts, one order)
// and WELD-2's single_robot relay (two carts, four lifts, two of them relays).
// The injected states are the brief's: a CANCEL with the second cart on the
// robot, a set-down's event withheld (finding 6), and a hand move of a cart on
// a robot. The invariant asked after every one: no cart is ever without an order
// on record — each cart's last order (its newest row) is the order that moved
// it.

const ocStation = "edge1.line1"

type ocPlant struct {
	db  *store.DB
	eng *engine.Engine
	sim *simulator.SimulatorBackend
}

func newOCPlant(t *testing.T) *ocPlant {
	t.Helper()
	db := testdb.Open(t)
	plant, err := plantspec.Load("../../../plants/order-carts.yaml")
	testutil.MustNoErr(t, err, "load plants/order-carts.yaml")
	testutil.MustNoErr(t, plant.Validate(), "validate plants/order-carts.yaml")
	testutil.MustNoErr(t, seedCore(db, plant, map[string]int64{}), "seed the multi-cart fixture")
	seedClaimsMirror(t, db, plant)

	sim := simulator.New()
	cfg := config.Defaults()
	cfg.Messaging.StationID = "test-core"
	cfg.Messaging.DispatchTopic = "shingo.dispatch"
	// A no-op log: a started engine's loops can still log after the test ends.
	eng := engine.New(engine.Config{AppConfig: cfg, DB: db, Fleet: sim, LogFunc: func(string, ...any) {}})
	eng.Start()
	t.Cleanup(eng.Stop)
	return &ocPlant{db: db, eng: eng, sim: sim}
}

func (p *ocPlant) env() *protocol.Envelope {
	return &protocol.Envelope{Src: protocol.Address{Role: protocol.RoleEdge, Station: ocStation},
		Dst: protocol.Address{Role: protocol.RoleCore}}
}

func (p *ocPlant) bin(t *testing.T, label string) int64 {
	t.Helper()
	b, err := p.db.GetBinByLabel(label)
	if err != nil || b == nil {
		t.Fatalf("the fixture's bin %s: %v", label, err)
	}
	return b.ID
}

// order issues a complex order the way an Edge sends one and returns it
// dispatched.
func (p *ocPlant) order(t *testing.T, uuid, payload, processNode string, steps ...protocol.ComplexOrderStep) *orders.Order {
	t.Helper()
	p.eng.Dispatcher().HandleComplexOrderRequest(p.env(), &protocol.ComplexOrderRequest{
		OrderUUID: uuid, PayloadCode: payload, Quantity: 1, ProcessNode: processNode, Steps: steps,
	})
	o, err := p.db.GetOrderByUUID(uuid)
	testutil.MustNoErr(t, err, "the order "+uuid)
	if o.VendorOrderID == "" {
		t.Fatalf("%s did not dispatch: %s (%s)", uuid, o.Status, o.QueueReason)
	}
	return o
}

// block completes o's block at node of the given kind, as the fleet reports it.
func (p *ocPlant) block(t *testing.T, o *orders.Order, node string, lift bool) {
	t.Helper()
	for _, b := range p.sim.BlocksForOrder(o.VendorOrderID) {
		if strings.TrimSpace(b.Location) != node {
			continue
		}
		if lift && engine.IsPickupBlock(b.BinTask) || !lift && engine.IsDropoffBlock(b.BinTask) {
			if p.sim.CompleteBlock(o.VendorOrderID, b.BlockID, b.Location, b.BinTask, 0, 0) {
				return
			}
		}
	}
	t.Fatalf("order %d: no %s block at %s to complete", o.ID, map[bool]string{true: "pickup", false: "dropoff"}[lift], node)
}

// finish drives o to FINISHED and confirms it, as the fleet and the station do.
func (p *ocPlant) finish(t *testing.T, o *orders.Order) {
	t.Helper()
	p.sim.DriveState(o.VendorOrderID, "RUNNING")
	p.sim.DriveState(o.VendorOrderID, "FINISHED")
	p.eng.Dispatcher().HandleOrderReceipt(p.env(), &protocol.OrderReceipt{
		OrderUUID: o.EdgeUUID, ReceiptType: "confirmed", FinalCount: 1,
	})
	testdb.RequireOrderStatus(t, p.db, o.EdgeUUID, "confirmed")
}

func (p *ocPlant) cancel(t *testing.T, o *orders.Order) {
	t.Helper()
	p.eng.Dispatcher().HandleOrderCancel(p.env(), &protocol.OrderCancel{OrderUUID: o.EdgeUUID, Reason: "fixture"})
	testdb.RequireOrderStatus(t, p.db, o.EdgeUUID, "cancelled")
}

// at is where a cart is recorded.
func (p *ocPlant) at(t *testing.T, binID int64) string {
	t.Helper()
	b, err := p.db.GetBin(binID)
	testutil.MustNoErr(t, err, "read the cart")
	return b.NodeName
}

// lastOrderIs asserts the invariant: the cart has an order on record, and it is
// want (its newest row).
func (p *ocPlant) lastOrderIs(t *testing.T, label string, binID int64, want *orders.Order) {
	t.Helper()
	got, err := p.db.ListOrdersByBin(binID, 1)
	testutil.MustNoErr(t, err, "the cart's orders")
	if len(got) == 0 {
		t.Fatalf("%s (bin %d) has no order on record", label, binID)
	}
	if got[0].ID != want.ID {
		t.Fatalf("%s (bin %d): its last order is %d, want %d (%s)", label, binID, got[0].ID, want.ID, want.EdgeUUID)
	}
}

// pressIndex runs R1 (the full off the press to its outbound staging) to the
// end, then issues R2: the on-deck empty from PLN_002 onto the press, an empty
// from the press empties onto PLN_002. R2 is returned dispatched, nothing lifted.
func (p *ocPlant) pressIndex(t *testing.T, tag string) (r2 *orders.Order, onDeck, empty int64) {
	t.Helper()
	full := p.bin(t, "BIN-ACT-P1F")
	// R1 carries the press's full: the plant seeds the front cart empty, and an empty off a
	// position a kanban supports goes to that kanban, not to the outbound.
	_, err := p.db.Exec(`UPDATE bins SET payload_code = 'PANEL-A', uop_remaining = 30 WHERE id = $1`, full)
	testutil.MustNoErr(t, err, "the press's full")
	r1 := p.order(t, tag+"-r1", "PANEL-A", "PLN_001",
		protocol.ComplexOrderStep{Action: protocol.ActionPickup, Node: "PLN_001"},
		protocol.ComplexOrderStep{Action: protocol.ActionDropoff, Node: "SLN_015"})
	p.block(t, r1, "PLN_001", true)
	p.block(t, r1, "SLN_015", false)
	p.finish(t, r1)
	if got := p.at(t, full); got != "SLN_015" {
		t.Fatalf("R1's full is recorded at %s, want SLN_015", got)
	}
	onDeck, empty = p.bin(t, "BIN-ACT-P1B"), p.bin(t, "BIN-PEB-01")
	r2 = p.order(t, tag+"-r2", "PANEL-A", "PLN_001",
		protocol.ComplexOrderStep{Action: protocol.ActionPickup, Node: "PLN_002", Empty: true},
		protocol.ComplexOrderStep{Action: protocol.ActionDropoff, Node: "PLN_001"},
		protocol.ComplexOrderStep{Action: protocol.ActionPickup, Node: "PEB_001", Empty: true},
		protocol.ComplexOrderStep{Action: protocol.ActionDropoff, Node: "PLN_002"})
	if n := len(testdb.CartRowBins(t, p.db, r2.ID)); n != 2 {
		t.Fatalf("R2 carries %d cart rows at its dispatch, want 2 (the on-deck cart and the empty)", n)
	}
	return r2, onDeck, empty
}

// TestOrderCartsFixture_ThePressIndexSwapsTwoCarts: R2 lifts the on-deck cart
// onto the press and an empty onto the back position; both land where their
// rows set them down, both rows are kept after the end, and each cart's last
// order is R2.
func TestOrderCartsFixture_ThePressIndexSwapsTwoCarts(t *testing.T) {
	t.Parallel()
	p := newOCPlant(t)
	r2, onDeck, empty := p.pressIndex(t, "oc-pi")
	p.block(t, r2, "PLN_002", true)
	p.block(t, r2, "PLN_001", false)
	p.block(t, r2, "PEB_001", true)
	p.block(t, r2, "PLN_002", false)
	p.finish(t, r2)

	for _, c := range []struct {
		label string
		id    int64
		want  string
	}{{"the on-deck cart", onDeck, "PLN_001"}, {"the empty", empty, "PLN_002"}} {
		if got := p.at(t, c.id); got != c.want {
			t.Errorf("%s is recorded at %s, want %s", c.label, got, c.want)
		}
		p.lastOrderIs(t, c.label, c.id, r2)
	}
	if n := len(testdb.CartRowBins(t, p.db, r2.ID)); n != 2 {
		t.Errorf("R2 has %d cart rows after its end, want both kept", n)
	}
}

// TestOrderCartsFixture_TheRelaySwapLandsBothCarts: WELD-2's single_robot swap
// parks the fresh cart at inbound staging, waits at the line for the station's
// release, lifts the spent one off the line and parks it at outbound staging,
// then re-lifts each: four rows, each re-lift by
// its own row. (The fresh cart is placed at SLN_007 by the fixture: the market's
// lanes are gated, and the gate is not this test's subject.)
func TestOrderCartsFixture_TheRelaySwapLandsBothCarts(t *testing.T) {
	t.Parallel()
	p := newOCPlant(t)
	fresh, spent := p.bin(t, "BIN-PB-S3"), p.bin(t, "BIN-ACT-W2-PB")
	_, err := p.db.DB.Exec(`UPDATE bins SET node_id = (SELECT id FROM nodes WHERE name = 'SLN_007') WHERE id = $1`, fresh)
	testutil.MustNoErr(t, err, "stage the fresh cart")

	swap := p.order(t, "oc-relay", "PANEL-B", "ALN_003",
		protocol.ComplexOrderStep{Action: protocol.ActionPickup, Node: "SLN_007"},
		protocol.ComplexOrderStep{Action: protocol.ActionDropoff, Node: "SLN_003"},
		protocol.ComplexOrderStep{Action: protocol.ActionWait, Node: "ALN_003"},
		protocol.ComplexOrderStep{Action: protocol.ActionPickup, Node: "ALN_003"},
		protocol.ComplexOrderStep{Action: protocol.ActionDropoff, Node: "SLN_004"},
		protocol.ComplexOrderStep{Action: protocol.ActionPickup, Node: "SLN_003"},
		protocol.ComplexOrderStep{Action: protocol.ActionDropoff, Node: "ALN_003"},
		protocol.ComplexOrderStep{Action: protocol.ActionPickup, Node: "SLN_004"},
		protocol.ComplexOrderStep{Action: protocol.ActionDropoff, Node: "SLN_008"})
	if n := len(testdb.CartRowBins(t, p.db, swap.ID)); n != 4 {
		t.Fatalf("the relay swap carries %d rows at its dispatch, want 4 (two lifts of each cart)", n)
	}
	type step struct {
		node string
		lift bool
	}
	for _, s := range []step{{"SLN_007", true}, {"SLN_003", false}} {
		p.block(t, swap, s.node, s.lift)
		p.lastOrderIs(t, "the fresh cart", fresh, swap)
	}
	// At the line's wait: the station releases the rest of the swap.
	p.sim.DriveState(swap.VendorOrderID, "RUNNING")
	p.sim.DriveState(swap.VendorOrderID, "WAITING")
	p.eng.Dispatcher().HandleOrderRelease(p.env(), &protocol.OrderRelease{OrderUUID: swap.EdgeUUID})
	for _, s := range []step{{"ALN_003", true}, {"SLN_004", false},
		{"SLN_003", true}, {"ALN_003", false}, {"SLN_004", true}, {"SLN_008", false}} {
		p.block(t, swap, s.node, s.lift)
		// Between blocks every cart this order lifted has it as its last order.
		p.lastOrderIs(t, "the fresh cart", fresh, swap)
	}
	p.finish(t, swap)

	if got := p.at(t, fresh); got != "ALN_003" {
		t.Errorf("the fresh cart is recorded at %s, want ALN_003 (the line)", got)
	}
	if got := p.at(t, spent); got != "SLN_008" {
		t.Errorf("the spent cart is recorded at %s, want SLN_008", got)
	}
	p.lastOrderIs(t, "the fresh cart", fresh, swap)
	p.lastOrderIs(t, "the spent cart", spent, swap)
}

// TestOrderCartsFixture_ACancelWithBOnTheRobotKeepsBsOrder: R2 sets the on-deck
// cart (A) on the press, lifts the empty (B), and is cancelled with B on the
// robot. A stays on the press, B stays recorded on the robot, and each cart's
// last order is R2: the sweep and the cancel-return find B through its row.
func TestOrderCartsFixture_ACancelWithBOnTheRobotKeepsBsOrder(t *testing.T) {
	t.Parallel()
	p := newOCPlant(t)
	r2, a, b := p.pressIndex(t, "oc-cancel")
	p.block(t, r2, "PLN_002", true)
	p.block(t, r2, "PLN_001", false)
	p.block(t, r2, "PEB_001", true)
	p.cancel(t, r2)

	if got := p.at(t, a); got != "PLN_001" {
		t.Errorf("A is recorded at %s, want PLN_001 (set down before the cancel)", got)
	}
	if got := p.at(t, b); got != domain.TransitNodeName {
		t.Errorf("B is recorded at %s, want %s (on the robot)", got, domain.TransitNodeName)
	}
	p.lastOrderIs(t, "A", a, r2)
	p.lastOrderIs(t, "B", b, r2)
	if n := len(testdb.CartRowBins(t, p.db, r2.ID)); n != 2 {
		t.Errorf("the cancelled R2 has %d cart rows, want both kept", n)
	}
}

// TestOrderCartsFixture_AWithheldSetDownKeepsBothCartsOrder is finding 6's
// state on the fixture: A's set-down event never arrives, B is lifted, and R2
// is cancelled. Both carts are recorded on the robot and each one's last order
// is R2 — neither reads an older order, and neither is without one.
func TestOrderCartsFixture_AWithheldSetDownKeepsBothCartsOrder(t *testing.T) {
	t.Parallel()
	p := newOCPlant(t)
	r2, a, b := p.pressIndex(t, "oc-f6")
	p.block(t, r2, "PLN_002", true)
	// A's set-down at PLN_001 is withheld.
	p.block(t, r2, "PEB_001", true)
	p.cancel(t, r2)

	for _, c := range []struct {
		label string
		id    int64
	}{{"A", a}, {"B", b}} {
		if got := p.at(t, c.id); got != domain.TransitNodeName {
			t.Errorf("%s is recorded at %s, want %s", c.label, got, domain.TransitNodeName)
		}
		p.lastOrderIs(t, c.label, c.id, r2)
	}
}

// TestOrderCartsFixture_AHandMoveOfACartOnTheRobotIsRefused: with B lifted by a
// live R2, a person's move of B is refused (the orphan guard asks B's rows,
// not one cart column), and B stays on the robot with R2 its order.
func TestOrderCartsFixture_AHandMoveOfACartOnTheRobotIsRefused(t *testing.T) {
	t.Parallel()
	p := newOCPlant(t)
	r2, _, b := p.pressIndex(t, "oc-hand")
	p.block(t, r2, "PLN_002", true)
	p.block(t, r2, "PLN_001", false)
	p.block(t, r2, "PEB_001", true)

	cart, err := p.db.GetBin(b)
	testutil.MustNoErr(t, err, "B")
	dest, err := p.db.GetNodeByDotName("SLN_014")
	testutil.MustNoErr(t, err, "a node to move it to")
	if _, err := p.eng.BinService().Move(cart, dest.ID); err == nil {
		t.Fatalf("a hand move of B (on the robot for R2 %d) was accepted", r2.ID)
	}
	if got := p.at(t, b); got != domain.TransitNodeName {
		t.Errorf("B is recorded at %s after the refused move, want %s", got, domain.TransitNodeName)
	}
	p.lastOrderIs(t, "B", b, r2)
}
