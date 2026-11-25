//go:build docker

package dispatch

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingo/shared/parkvectors"
	"shingocore/dispatch/plan"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/payloads"
)

// loader_place_picks_docker_test.go — what each of loader placement's choose
// points picks, with nothing held and under each hold it respects.
//
// Loader placement (loader_place.go) chooses a landing node at several points:
//
//   - the home a return lifts its bin at (tryPlaceFromHomeSource);
//   - the home a leg is pointed at, by the in-flight check when a return's home
//     is clear, or by the full capacity gate otherwise (placeForDedicatedLoader);
//   - the carrier's own home, when it does not belong where it was pointed
//     (homeForPayload);
//   - the buffers, in member order, then drain or wait (placeForLoader);
//   - the member a partner's committed lift empties (placeVacated);
//   - quality containment's divert to a concrete node (placeForContainment).
//
// Each row puts one hold on the node the choose point would otherwise take, and
// names where the leg lands. The holds here are the ones placement reads: a bin
// standing on the node, another order on its way there holding a claimed bin,
// the order's own plan delivering there first, a carrier that does not belong,
// and another order's slot reservation or hard claim on it.
//
// A LIVE ORDER THAT ONLY NAMES THE NODE IS NOT A HOLD for a loader, and the
// rows marked "named by a live order" pin that it is not. For a loader the
// first to claim goes (CheckDropoffCapacity's doc): two returns waiting on one
// empty home each name it, and counting the name would keep both off it, and a
// return that gave its home up to a refill merely naming it is link 2 of the
// 2026-08-26 chain (TestSpringfieldIncident_ReturnHoldsHome_ReplenishYields).

// pickFixture is parkFixture with a second buffer after the first in member
// order, so a refused first buffer shows which way the walk goes.
type pickFixture struct {
	home, buffer, buffer2, outbound *nodes.Node
	loaderID                        int64
}

func newPickFixture(t *testing.T, db *store.DB) *pickFixture {
	t.Helper()
	home, buffer, outbound, loaderID := parkFixture(t, db)
	b2 := prNode(t, db, "LX-B2")
	testutil.MustNoErr(t, db.UpsertLoaderHome(store.LoaderHome{LoaderID: loaderID, PositionNodeID: b2.ID,
		Kind: loaders.HomeKindBuffer}), "second buffer")
	return &pickFixture{home: home, buffer: buffer, buffer2: b2, outbound: outbound, loaderID: loaderID}
}

// fill stands a bin on n that nobody is coming for.
func (fx *pickFixture) fill(t *testing.T, db *store.DB, n *nodes.Node, label string) {
	t.Helper()
	makeLoaderBin(t, db, "PART-X", n.ID, label, 4, time.Now().UTC())
}

// landed names where placement left the leg: "wait:<node>" for a wait, else the
// fixture role of its delivery node.
func (fx *pickFixture) landed(o *orders.Order, wait string, own *nodes.Node) string {
	if wait != "" {
		return "wait:" + fx.role(wait, own)
	}
	return fx.role(o.DeliveryNode, own)
}

func (fx *pickFixture) role(name string, own *nodes.Node) string {
	switch {
	case name == fx.home.Name:
		return "home"
	case name == fx.buffer.Name:
		return "buffer"
	case name == fx.buffer2.Name:
		return "buffer2"
	case name == fx.outbound.Name:
		return "outbound"
	case own != nil && name == own.Name:
		return "own-home"
	}
	return name
}

type pickCase struct {
	point string // the choose point, as loader_place.go names it
	hold  string
	// place builds the leg, applies the hold, runs placement, and returns where
	// the leg landed.
	place func(t *testing.T, db *store.DB, d *Dispatcher, fx *pickFixture) string
	want  string
}

// placeHomeSource: a return lifting its bin at the home, pointed at the drain.
func placeHomeSource(steps func(fx *pickFixture) []resolvedStep, hold func(t *testing.T, db *store.DB, fx *pickFixture)) func(*testing.T, *store.DB, *Dispatcher, *pickFixture) string {
	return func(t *testing.T, db *store.DB, d *Dispatcher, fx *pickFixture) string {
		fx.fill(t, db, fx.home, "lifted-at-home")
		if hold != nil {
			hold(t, db, fx)
		}
		o := makeEvacOrder(t, db, "pick-hs", fx.home.Name, fx.outbound.Name)
		wait := d.placeForDedicatedLoader(o, steps(fx), nil)
		return fx.landed(o, wait, nil)
	}
}

func evacSteps(fx *pickFixture) []resolvedStep {
	return simpleEvacSteps(fx.home.Name, fx.outbound.Name)
}

// placeReturn: a swap's return coming back to the home from a line, linked to
// its supply sibling when linked is set.
func placeReturn(linked bool, hold func(t *testing.T, db *store.DB, fx *pickFixture)) func(*testing.T, *store.DB, *Dispatcher, *pickFixture) string {
	return func(t *testing.T, db *store.DB, d *Dispatcher, fx *pickFixture) string {
		line := prNode(t, db, "PICK-LINE")
		if hold != nil {
			hold(t, db, fx)
		}
		ret, steps := parkSwapPair(t, db, fx.home.Name, line.Name, linked)
		wait := d.placeForDedicatedLoader(ret, steps, nil)
		return fx.landed(ret, wait, nil)
	}
}

// placeSupplyWithWait: a leg the capacity gate judges — a supply from staging
// to the home with a wait in its plan and no process node, so its role reads
// as not-a-return.
func placeSupplyWithWait(hold func(t *testing.T, db *store.DB, fx *pickFixture)) func(*testing.T, *store.DB, *Dispatcher, *pickFixture) string {
	return func(t *testing.T, db *store.DB, d *Dispatcher, fx *pickFixture) string {
		staging := prNode(t, db, "PICK-STAGE")
		if hold != nil {
			hold(t, db, fx)
		}
		o := testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.EdgeUUID, o.StationUID, o.OrderType, o.Status = "pick-supply", "test", OrderTypeComplex, "staged"
			o.SourceNode, o.DeliveryNode, o.PayloadCode = staging.Name, fx.home.Name, "PART-X"
		})
		steps := []resolvedStep{vsWait(staging.Name), vsPick(staging.Name), vsDrop(fx.home.Name)}
		wait := d.placeForDedicatedLoader(o, steps, nil)
		return fx.landed(o, wait, nil)
	}
}

// placeMismatch: a return carrying a PART-Y carrier to PART-X's home, whose own
// carrier the sibling lifts. PART-Y's own home is a second pinned member.
func placeMismatch(hold func(t *testing.T, db *store.DB, fx *pickFixture, own *nodes.Node)) func(*testing.T, *store.DB, *Dispatcher, *pickFixture) string {
	return func(t *testing.T, db *store.DB, d *Dispatcher, fx *pickFixture) string {
		own, line := mismatchFixture(t, db, fx.loaderID, "PICK-LINE-MM")
		makeLoaderBin(t, db, "PART-Y", line.ID, "outgoing-carrier", 72, time.Now().UTC())
		fx.fill(t, db, fx.home, "sibling-lifts-this")
		if hold != nil {
			hold(t, db, fx, own)
		}
		ret, steps := parkSwapPair(t, db, fx.home.Name, line.Name, true)
		wait := d.placeForDedicatedLoader(ret, steps, nil)
		return fx.landed(ret, wait, own)
	}
}

// placeVacatedPair: a return whose home holds a carrier nobody lifts and whose
// buffers are both full, while its supply partner's committed lift takes the
// first buffer's only bin.
func placeVacatedPair(hold func(t *testing.T, db *store.DB, fx *pickFixture)) func(*testing.T, *store.DB, *Dispatcher, *pickFixture) string {
	return func(t *testing.T, db *store.DB, d *Dispatcher, fx *pickFixture) string {
		line := prNode(t, db, "PICK-VS-LINE")
		now := time.Now().UTC()
		makeLoaderBin(t, db, "PART-X", fx.home.ID, "vs-home-full", 10, now)
		partial := makeLoaderBin(t, db, "PART-X", fx.buffer.ID, "vs-partial", 4, now.Add(-time.Hour))
		fx.fill(t, db, fx.buffer2, "vs-buffer2-full")
		makeLoaderBin(t, db, "PART-X", line.ID, "vs-resident", 3, now)
		if hold != nil {
			hold(t, db, fx)
		}
		supply, ret, steps := vsPairRows(t, db, "pick-vs", line.Name, fx.buffer.Name, fx.home.Name)
		pc := vsLift(t, db, supply, partial.ID, fx.buffer.Name)
		wait := d.placeForDedicatedLoader(ret, steps, pc)
		return fx.landed(ret, wait, nil)
	}
}

func inFlightTo(n func(fx *pickFixture) *nodes.Node) func(t *testing.T, db *store.DB, fx *pickFixture) {
	return func(t *testing.T, db *store.DB, fx *pickFixture) {
		makeInFlightTo(t, db, "pick-inbound-"+n(fx).Name, n(fx).Name)
	}
}

func binOn(n func(fx *pickFixture) *nodes.Node) func(t *testing.T, db *store.DB, fx *pickFixture) {
	return func(t *testing.T, db *store.DB, fx *pickFixture) {
		fx.fill(t, db, n(fx), "pick-occupant-"+n(fx).Name)
	}
}

func both(a, b func(t *testing.T, db *store.DB, fx *pickFixture)) func(t *testing.T, db *store.DB, fx *pickFixture) {
	return func(t *testing.T, db *store.DB, fx *pickFixture) {
		a(t, db, fx)
		b(t, db, fx)
	}
}

// reservedBy puts another order's slot reservation on the node: pending, or
// confirmed when confirm is set.
func reservedBy(n func(fx *pickFixture) *nodes.Node, confirm bool) func(t *testing.T, db *store.DB, fx *pickFixture) {
	return func(t *testing.T, db *store.DB, fx *pickFixture) {
		h := strangerOrder(t, db, "pick-res-"+n(fx).Name)
		testutil.MustNoErr(t, db.ReserveSlot(n(fx).ID, h.ID), "reserve "+n(fx).Name)
		if confirm {
			testdb.SeedDispatched(t, db, h.ID, "sg-"+h.EdgeUUID) // (C1.5: a confirmed holder is dispatched)
			testutil.MustNoErr(t, db.ConfirmSlotReservation(n(fx).ID, h.ID), "confirm the reservation")
		}
	}
}

// hardClaimedBy puts another order's hard claim on the node.
func hardClaimedBy(n func(fx *pickFixture) *nodes.Node) func(t *testing.T, db *store.DB, fx *pickFixture) {
	return func(t *testing.T, db *store.DB, fx *pickFixture) {
		h := strangerOrder(t, db, "pick-claim-"+n(fx).Name, asDispatched) // (C1.5: a claimant is dispatched)
		testutil.MustNoErr(t, db.ReserveSlot(n(fx).ID, h.ID), "reserve "+n(fx).Name)
		testutil.MustNoErr(t, db.ConfirmSlotClaim(n(fx).ID, h.ID, nil), "hard-claim "+n(fx).Name)
	}
}

// namedBy points a live order at the node that holds no claim and no
// reservation: it names the node and nothing more.
func namedBy(n func(fx *pickFixture) *nodes.Node) func(t *testing.T, db *store.DB, fx *pickFixture) {
	return func(t *testing.T, db *store.DB, fx *pickFixture) {
		testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.EdgeUUID, o.StationUID, o.OrderType, o.Status = "pick-named-"+n(fx).Name, "test", OrderTypeComplex, StatusQueued
			o.DeliveryNode = n(fx).Name
		})
	}
}

func homeOf(fx *pickFixture) *nodes.Node    { return fx.home }
func bufferOf(fx *pickFixture) *nodes.Node  { return fx.buffer }
func buffer2Of(fx *pickFixture) *nodes.Node { return fx.buffer2 }

var loaderPickCases = []pickCase{
	// tryPlaceFromHomeSource: the in-flight count alone, plus the order's own
	// plan delivering to the home first. The bin it lifts is not a hold.
	{point: "home-source", hold: "none", want: "home",
		place: placeHomeSource(evacSteps, nil)},
	{point: "home-source", hold: "in-flight to home", want: "buffer",
		place: placeHomeSource(evacSteps, inFlightTo(homeOf))},
	{point: "home-source", hold: "own plan delivers to home first", want: "buffer",
		place: placeHomeSource(func(fx *pickFixture) []resolvedStep {
			return []resolvedStep{vsPick(fx.home.Name), vsDrop(fx.home.Name), vsDrop(fx.outbound.Name)}
		}, nil)},

	// A return whose home is clear: the in-flight count alone.
	{point: "return-home-clear", hold: "none", want: "home",
		place: placeReturn(true, nil)},
	{point: "return-home-clear", hold: "bin the sibling lifts", want: "home",
		place: placeReturn(true, binOn(homeOf))},
	{point: "return-home-clear", hold: "in-flight to home", want: "buffer",
		place: placeReturn(true, inFlightTo(homeOf))},

	// The full capacity gate on the home: bins and in-flight.
	{point: "home-capacity-gate", hold: "none", want: "home",
		place: placeSupplyWithWait(nil)},
	{point: "home-capacity-gate", hold: "bin on home", want: "buffer",
		place: placeSupplyWithWait(binOn(homeOf))},
	{point: "home-capacity-gate", hold: "in-flight to home", want: "buffer",
		place: placeSupplyWithWait(inFlightTo(homeOf))},
	{point: "home-capacity-gate", hold: "return, carrier nobody lifts", want: "buffer",
		place: placeReturn(false, binOn(homeOf))},

	// homeForPayload: the carrier's own home, by the full capacity gate.
	{point: "own-home", hold: "none", want: "own-home",
		place: placeMismatch(nil)},
	{point: "own-home", hold: "bin on own home", want: "buffer",
		place: placeMismatch(func(t *testing.T, db *store.DB, fx *pickFixture, own *nodes.Node) {
			makeLoaderBin(t, db, "PART-Y", own.ID, "own-home-occupied", 500, time.Now().UTC())
		})},
	{point: "own-home", hold: "in-flight to own home", want: "buffer",
		place: placeMismatch(func(t *testing.T, db *store.DB, fx *pickFixture, own *nodes.Node) {
			makeInFlightTo(t, db, "pick-inbound-own", own.Name)
		})},

	// placeForLoader: buffers in member order by the full capacity gate, then
	// drain when the order points elsewhere, else wait on the home.
	{point: "buffer-walk", hold: "bin on first buffer", want: "buffer2",
		place: placeHomeSource(evacSteps, both(inFlightTo(homeOf), binOn(bufferOf)))},
	{point: "buffer-walk", hold: "in-flight to first buffer", want: "buffer2",
		place: placeHomeSource(evacSteps, both(inFlightTo(homeOf), inFlightTo(bufferOf)))},
	{point: "buffer-walk", hold: "every buffer taken, order points at drain", want: "outbound",
		place: placeHomeSource(evacSteps, both(inFlightTo(homeOf), both(binOn(bufferOf), inFlightTo(buffer2Of))))},
	{point: "buffer-walk", hold: "every buffer taken, order points at home", want: "wait:home",
		place: placeReturn(false, both(binOn(homeOf), both(binOn(bufferOf), binOn(buffer2Of))))},

	// placeVacated: the member a partner's committed lift empties, refused by an
	// in-flight order there.
	{point: "vacated", hold: "none", want: "buffer",
		place: placeVacatedPair(nil)},
	{point: "vacated", hold: "in-flight to the vacated buffer", want: "wait:home",
		place: placeVacatedPair(inFlightTo(bufferOf))},

	// ── Another order's slot reservation or hard claim (owner ruling 2026-10-03:
	// a loader takes the same check as every other chooser). Each of these
	// picked the held node before, and the claim door refused it on every pass.

	// A: a held home sends the bin to a buffer.
	{point: "home-source", hold: "pending reservation on home", want: "buffer",
		place: placeHomeSource(evacSteps, reservedBy(homeOf, false))},
	{point: "home-source", hold: "confirmed reservation on home", want: "buffer",
		place: placeHomeSource(evacSteps, reservedBy(homeOf, true))},
	{point: "return-home-clear", hold: "pending reservation on home", want: "buffer",
		place: placeReturn(true, reservedBy(homeOf, false))},
	{point: "return-home-clear", hold: "hard claim on home", want: "buffer",
		place: placeReturn(true, hardClaimedBy(homeOf))},
	{point: "home-capacity-gate", hold: "pending reservation on home", want: "buffer",
		place: placeSupplyWithWait(reservedBy(homeOf, false))},
	{point: "home-capacity-gate", hold: "hard claim on home", want: "buffer",
		place: placeSupplyWithWait(hardClaimedBy(homeOf))},
	{point: "own-home", hold: "pending reservation on own home", want: "buffer",
		place: placeMismatch(func(t *testing.T, db *store.DB, fx *pickFixture, own *nodes.Node) {
			reservedBy(func(*pickFixture) *nodes.Node { return own }, false)(t, db, fx)
		})},

	// H: a held first buffer picks the second.
	{point: "buffer-walk", hold: "pending reservation on first buffer", want: "buffer2",
		place: placeHomeSource(evacSteps, both(inFlightTo(homeOf), reservedBy(bufferOf, false)))},
	{point: "buffer-walk", hold: "hard claim on first buffer", want: "buffer2",
		place: placeHomeSource(evacSteps, both(inFlightTo(homeOf), hardClaimedBy(bufferOf)))},

	// B: the only free buffer held, the order pointing at the drain: it drains.
	{point: "buffer-walk", hold: "only free buffer reserved, order points at drain", want: "outbound",
		place: placeHomeSource(evacSteps, both(inFlightTo(homeOf), both(binOn(buffer2Of), reservedBy(bufferOf, false))))},

	// C: the only free buffer held, the order pointing at the home: it waits.
	// So does a vacated buffer another order holds.
	{point: "buffer-walk", hold: "only free buffer reserved, order points at home", want: "wait:home",
		place: placeReturn(false, both(binOn(homeOf), both(binOn(buffer2Of), reservedBy(bufferOf, false))))},
	{point: "vacated", hold: "pending reservation on the vacated buffer", want: "wait:home",
		place: placeVacatedPair(reservedBy(bufferOf, false))},

	// ── Named by a live order that holds nothing: NOT a hold for a loader. These
	// land exactly where they did before the ruling (cases D, E and F of the
	// polish-loader report).
	{point: "home-source", hold: "home named by a live order", want: "home",
		place: placeHomeSource(evacSteps, namedBy(homeOf))},
	{point: "return-home-clear", hold: "home named by a live order", want: "home",
		place: placeReturn(true, namedBy(homeOf))},
	{point: "home-capacity-gate", hold: "home named by a live order", want: "home",
		place: placeSupplyWithWait(namedBy(homeOf))},
	{point: "buffer-walk", hold: "only free buffer named by a live order, order points at home", want: "buffer",
		place: placeReturn(false, both(binOn(homeOf), both(binOn(buffer2Of), namedBy(bufferOf))))},
	{point: "buffer-walk", hold: "only free buffer named by a live order, order points at drain", want: "buffer",
		place: placeHomeSource(evacSteps, both(inFlightTo(homeOf), both(binOn(buffer2Of), namedBy(bufferOf))))},
	{point: "vacated", hold: "vacated buffer named by a live order", want: "buffer",
		place: placeVacatedPair(namedBy(bufferOf))},
}

// TestLoaderPlacementPicks pins every loader-placement pick as it stands, so a
// change to how placement tests a node shows exactly which picks moved.
func TestLoaderPlacementPicks(t *testing.T) {
	t.Parallel()
	for i, c := range loaderPickCases {
		c := c
		t.Run(fmt.Sprintf("%02d %s/%s", i, c.point, c.hold), func(t *testing.T) {
			t.Parallel()
			db := testDB(t)
			fx := newPickFixture(t, db)
			d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
			if got := c.place(t, db, d, fx); got != c.want {
				t.Fatalf("%s under %q landed at %s, want %s", c.point, c.hold, got, c.want)
			}
		})
	}
}

// TestContainmentPlacementPicks pins quality containment's divert: a concrete
// containment node takes the leg when the capacity gate passes and parks it
// when a bin stands there or another order is on its way there.
func TestContainmentPlacementPicks(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		hold string
		set  func(t *testing.T, db *store.DB, hold *nodes.Node)
		want string
	}{
		{hold: "none", want: "diverted"},
		{hold: "bin on containment node", want: "parked", set: func(t *testing.T, db *store.DB, hold *nodes.Node) {
			makeLoaderBin(t, db, "PART-QC", hold.ID, "qc-occupant", 4, time.Now().UTC())
		}},
		{hold: "in-flight to containment node", want: "parked", set: func(t *testing.T, db *store.DB, hold *nodes.Node) {
			makeInFlightTo(t, db, "qc-inbound", hold.Name)
		}},
		// G: another order's reservation or hard claim parks instead of diverting.
		{hold: "pending reservation on containment node", want: "parked", set: func(t *testing.T, db *store.DB, hold *nodes.Node) {
			h := strangerOrder(t, db, "qc-reserver")
			testutil.MustNoErr(t, db.ReserveSlot(hold.ID, h.ID), "reserve the containment node")
		}},
		{hold: "hard claim on containment node", want: "parked", set: func(t *testing.T, db *store.DB, hold *nodes.Node) {
			h := strangerOrder(t, db, "qc-claimer", asDispatched) // (C1.5: a claimant is dispatched)
			testutil.MustNoErr(t, db.ReserveSlot(hold.ID, h.ID), "reserve the containment node")
			testutil.MustNoErr(t, db.ConfirmSlotClaim(hold.ID, h.ID, nil), "hard-claim the containment node")
		}},
		// Reservation and hard claim only: a live order merely naming the node
		// still diverts.
		{hold: "containment node named by a live order", want: "diverted", set: func(t *testing.T, db *store.DB, hold *nodes.Node) {
			testdb.CreateOrder(t, db, func(o *orders.Order) {
				o.EdgeUUID, o.StationUID, o.OrderType, o.Status = "qc-named", "test", OrderTypeComplex, StatusQueued
				o.DeliveryNode = hold.Name
			})
		}},
	} {
		c := c
		t.Run(c.hold, func(t *testing.T) {
			t.Parallel()
			db := testDB(t)
			_, fgn, hold := containmentFixture(t, db)
			d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
			if c.set != nil {
				c.set(t, db, hold)
			}
			o := containmentOrder(t, db, "qc-pick", fgn.Name, "PROD-QC", nil)
			got := "diverted"
			if st := d.placeForContainment(o, containmentSteps(fgn.Name, "PROD-QC")); st != nil {
				got = "parked"
			} else if o.DeliveryNode != hold.Name {
				got = "untouched:" + o.DeliveryNode
			}
			if got != c.want {
				t.Fatalf("containment under %q: %s, want %s", c.hold, got, c.want)
			}
		})
	}
}

// A LOST CLAIM NEVER RE-PICKS THE SLOT IT LOST. Placement takes the free home;
// another order then reserves it first, and the claim door refuses this one.
// The next pass must choose somewhere the door accepts. Before placement read
// reservations it chose the home again on every pass, for as long as the other
// order held it.
func TestLoaderPlacement_ALostClaimDoesNotRePickTheSlot(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	fx := newPickFixture(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	fx.fill(t, db, fx.home, "lost-claim-lifted")
	o := makeEvacOrder(t, db, "lost-claim", fx.home.Name, fx.outbound.Name)
	steps := evacSteps(fx)
	if wait := d.placeForDedicatedLoader(o, steps, nil); wait != "" || o.DeliveryNode != fx.home.Name {
		t.Fatalf("precondition: the free home was not picked (landed %s, wait %q)", o.DeliveryNode, wait)
	}
	winner := strangerOrder(t, db, "lost-claim-winner")
	testutil.MustNoErr(t, db.ReserveSlot(fx.home.ID, winner.ID), "the other order reserves the home first")
	if err := db.ReserveSlot(fx.home.ID, o.ID); err == nil {
		t.Fatal("precondition: the claim door accepted a slot another order holds")
	}

	wait := d.placeForDedicatedLoader(o, steps, nil)
	if wait != "" {
		t.Fatalf("the next pass waits on %s; a buffer is free", wait)
	}
	if o.DeliveryNode == fx.home.Name {
		t.Fatalf("the next pass picked %s again, the slot this order just lost to order %d", fx.home.Name, winner.ID)
	}
	n, err := db.GetNodeByDotName(o.DeliveryNode)
	testutil.MustNoErr(t, err, "read the new pick")
	if err := db.ReserveSlot(n.ID, o.ID); err != nil {
		t.Fatalf("the next pass picked %s and the claim door refused it too: %v", n.Name, err)
	}
}

// A NODE WITH NO CLAIM DOOR is not dispatched onto when another order has it
// reserved. The fixture's home is neither storage-classed nor declared
// exclusive, so the complex slot reserve never asks the door about it: before
// placement read reservations, a return was dispatched onto it with the other
// order's reservation standing.
func TestLoaderPlacement_NoClaimDoor_ReservedHomeIsNotTaken(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	fx := newPickFixture(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	if needs := d.allocator.slotNeeds([]resolvedStep{vsDrop(fx.home.Name)}); len(needs) != 0 {
		t.Fatalf("precondition: %s has a claim door (%d slot need(s)); this pin is about a node without one",
			fx.home.Name, len(needs))
	}
	fx.fill(t, db, fx.home, "no-door-lifted")
	holder := strangerOrder(t, db, "no-door-holder")
	testutil.MustNoErr(t, db.ReserveSlot(fx.home.ID, holder.ID), "another order reserves the home")

	o := makeEvacOrder(t, db, "no-door", fx.home.Name, fx.outbound.Name)
	if wait := d.placeForDedicatedLoader(o, evacSteps(fx), nil); wait != "" {
		t.Fatalf("waits on %s; a buffer is free", wait)
	}
	if o.DeliveryNode == fx.home.Name {
		t.Fatalf("placed onto %s, which order %d has reserved; nothing after placement would stop the robot",
			fx.home.Name, holder.ID)
	}
}

// THE SPRINGFIELD 2026-08-05 SPECIMEN (SMN_030, 8h57m). An evac waited on its
// partner naming the empty home as its delivery node and holding nothing, and
// the refill counted it as an ask and was refused all shift. A loader still
// counts a name as nothing: the refill is created. The refill itself holds the
// home from its issue (Need.Window, owner 2026-10-25: the asks hold their
// window), so a return placed while the evac waits goes to the buffer, where it
// took the home beside the refill before and two carriers were bound for it.
func TestSpringfieldIncident_20260805_AWaitingReturnNamingTheHomeHoldsNothing(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	home, buffer, _, loaderID := springfieldLoaderFixture(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID, o.StationUID, o.OrderType, o.Status = "spr-0805-evac", "test", OrderTypeComplex, StatusQueued
		o.QueueCode = string(protocol.QueueWaitingForPartner)
		o.DeliveryNode, o.ProcessNode, o.PayloadCode = home.Name, "SPR-0805-LINE", "PART-X"
	})

	cfg, ok, err := d.LoadReplenishConfig(loaderID)
	if err != nil || !ok {
		t.Fatalf("load replenish config for loader %d: ok=%v err=%v", loaderID, ok, err)
	}
	res, err := d.ReplenishLoader(ReplenishRequest{
		Origin: ReplenishByThreshold, OrderType: OrderTypeRetrieveEmpty,
		StationID: "test", LoaderID: loaderID, PayloadCode: "PART-X", MemberNode: home.Name,
		Threshold: 100, CurrentUOP: 0, PerBinCapacity: 10,
	}, cfg)
	testutil.MustNoErr(t, err, "replenish")
	refilled := false
	for _, o := range res.Created {
		if o != nil && o.DeliveryNode == home.Name {
			refilled = true
		}
	}
	if !refilled {
		t.Fatalf("no refill to %s (held=%v): the waiting evac names it and holds nothing, which is the "+
			"8h57m deadlock", home.Name, res.HeldBy)
	}

	line := prNode(t, db, "SPR-0805-LINE2")
	ret, steps := parkSwapPair(t, db, home.Name, line.Name, true)
	if wait := d.placeForDedicatedLoader(ret, steps, nil); wait != "" || ret.DeliveryNode != buffer.Name {
		t.Fatalf("a return placed while the refill holds %s landed at %s (wait %q), want the buffer %s: "+
			"the refill holds the home from its issue", home.Name, ret.DeliveryNode, wait, buffer.Name)
	}
}

// ── EVERY BUILDER LEG THROUGH THE PARK (brief G B2, 2026-11-25) ─────────────
//
// The picks above hand-build the legs. These take every leg the Edge's step
// builders write for a consume line whose routing runs through a dedicated
// loader (shared/parkvectors, kept current by the Edge's
// TestParkVectors_AreTheBuildersShapes): each is sent through Core's intake as
// the Edge sends it, naming its partner, with its process node and without,
// and one scan pass runs. The pin is where each leg then stands: sent, or its
// status and cause; every drop of its plan after the park, a vacate-stamped one
// marked; and "drain" when the park recorded one. A leg with no pin fails and
// prints the line to add.

// parkWorld is the pick fixture's loader (HOME, two buffers, the outbound
// OUT) with the incoming part's home HOME-B on it, and the line's other nodes,
// keyed by the vectors' role names.
type parkWorld struct {
	fx   *pickFixture
	node map[string]string // role -> node name
	role map[string]string // node name -> role
}

func newParkWorld(t *testing.T, db *store.DB) *parkWorld {
	t.Helper()
	fx := newPickFixture(t, db)
	testutil.MustNoErr(t, db.CreatePayload(&payloads.Payload{Code: "PART-Y", Description: "Y", UOPCapacity: 10}),
		"create PART-Y")
	w := &parkWorld{fx: fx, role: map[string]string{}, node: map[string]string{"HOME": fx.home.Name,
		"BUF": fx.buffer.Name, "BUF2": fx.buffer2.Name, "OUT": fx.outbound.Name}}
	for _, r := range []string{"HOME-B", "HOME2", "LINE", "PAIR", "STAGE", "OSTAGE", "MARKET", "MARKET-B"} {
		w.node[r] = prNode(t, db, "PV-"+r).Name
	}
	testutil.MustNoErr(t, db.UpsertLoaderHome(store.LoaderHome{LoaderID: fx.loaderID,
		PositionNodeID: w.at(t, db, "HOME-B").ID, PayloadCode: "PART-Y", Kind: loaders.HomeKindHome}), "HOME-B")
	for r, n := range w.node {
		w.role[n] = r
	}
	return w
}

func (w *parkWorld) at(t *testing.T, db *store.DB, role string) *nodes.Node {
	t.Helper()
	n, err := db.GetNodeByDotName(w.node[role])
	testutil.MustNoErr(t, err, "node "+role)
	return n
}

// put stands a cart of payload on role's node ("" payload: an empty).
func (w *parkWorld) put(t *testing.T, db *store.DB, role, payload, label string, uop int) *bins.Bin {
	t.Helper()
	if payload == "" {
		return makeEmptyBin(t, db, w.at(t, db, role).ID, label)
	}
	return makeLoaderBin(t, db, payload, w.at(t, db, role).ID, label, uop, time.Now().UTC())
}

// stock stands the walk's world: each part's full on its home and on its
// market, the old cart (a partial) on the line, a cart on the index press's
// back position. The buffers stand free.
func (w *parkWorld) stock(t *testing.T, db *store.DB) {
	t.Helper()
	w.put(t, db, "HOME", "PART-X", "pv-home-full", 10)
	w.put(t, db, "HOME-B", "PART-Y", "pv-homeb-full", 10)
	w.put(t, db, "MARKET", "PART-X", "pv-market-full", 10)
	w.put(t, db, "MARKET-B", "PART-Y", "pv-marketb-full", 10)
	w.put(t, db, "LINE", "PART-X", "pv-line-old", 3)
	w.put(t, db, "PAIR", "PART-X", "pv-pair-cart", 10)
}

// addHome2 makes HOME2 a second home pinned to the line's part.
func (w *parkWorld) addHome2(t *testing.T, db *store.DB) {
	t.Helper()
	testutil.MustNoErr(t, db.UpsertLoaderHome(store.LoaderHome{LoaderID: w.fx.loaderID,
		PositionNodeID: w.at(t, db, "HOME2").ID, PayloadCode: "PART-X", Kind: loaders.HomeKindHome}), "HOME2")
}

func parkLegUUID(name string) string { return "pv-" + strings.ReplaceAll(name, "/", "-") }

// submit sends one leg through Core's intake as the Edge sends it.
func (w *parkWorld) submit(t *testing.T, d *Dispatcher, c parkvectors.Case, sibling string, withProcess bool) string {
	t.Helper()
	var steps []protocol.ComplexOrderStep
	testutil.MustNoErr(t, json.Unmarshal(c.Steps, &steps), c.Name)
	for i := range steps {
		if steps[i].Node == "" {
			continue
		}
		n, ok := w.node[steps[i].Node]
		if !ok {
			t.Fatalf("%s: the vectors name %q, which the park world has no node for", c.Name, steps[i].Node)
		}
		steps[i].Node = n
	}
	pn := ""
	if withProcess {
		pn = w.node[c.ProcessNode]
	}
	uuid := parkLegUUID(c.Name)
	d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{OrderUUID: uuid,
		PayloadCode: c.PayloadCode, Quantity: 1, ProcessNode: pn, SiblingOrderUUID: sibling, Steps: steps})
	return uuid
}

// submitUnit sends a unit's legs in the Edge's creation order, each pair leg
// naming the other.
func (w *parkWorld) submitUnit(t *testing.T, d *Dispatcher, unit []parkvectors.Case, withProcess bool) []string {
	t.Helper()
	uuids := make([]string, len(unit))
	for i, c := range unit {
		sib := ""
		if c.Sibling != "" {
			sib = parkLegUUID(c.Sibling)
		}
		uuids[i] = w.submit(t, d, c, sib, withProcess)
	}
	return uuids
}

// landing is where a leg stands: "sent" when the fleet has it, else its status
// and cause; every drop of its plan, by role, one carrying a vacate stamp
// marked +v(own), +v(partner) or +v(sibling); "drain" when the park recorded
// one for it.
func (w *parkWorld) landing(t *testing.T, db *store.DB, uuid string) string {
	t.Helper()
	o := prReloadUUID(t, db, uuid)
	state := "sent"
	if o.VendorOrderID == "" {
		state = fmt.Sprintf("%s:%s", o.Status, o.QueueCause)
	}
	steps, ok := decodeSteps(o.StepsJSON)
	if !ok {
		t.Fatalf("%s: its stored plan does not read", uuid)
	}
	var drops []string
	for _, s := range steps {
		if s.Action != protocol.ActionDropoff {
			continue
		}
		r, ok := w.role[s.Node]
		if !ok {
			r = s.Node
		}
		if st := plan.StampHonoured(s); st != nil {
			switch {
			case st.Kind != "":
				r += "+v(" + st.Kind + ")"
			case st.Partner != 0:
				r += "+v(partner)"
			default:
				r += "+v(own)"
			}
		}
		drops = append(drops, r)
	}
	out := state + " drops=" + strings.Join(drops, ",")
	acts, err := db.ListRecoveryActions(500)
	testutil.MustNoErr(t, err, "recovery actions")
	for _, a := range acts {
		if a.Action == "loader_park_no_slot" && a.TargetType == "order" && a.TargetID == o.ID {
			out += " drain"
			break
		}
	}
	return out
}

// sentence is the wait's sentence with node names read back as roles.
func (w *parkWorld) sentence(t *testing.T, db *store.DB, uuid string) string {
	t.Helper()
	s := prReloadUUID(t, db, uuid).QueueReason
	for n, r := range w.role {
		s = strings.ReplaceAll(s, n, r)
	}
	return s
}

// parkUnits is the vectors in units the Edge creates together: a pair (its
// supply or Order A first, as the doors create them) or a single leg.
func parkUnits(t *testing.T, cases []parkvectors.Case) [][]parkvectors.Case {
	t.Helper()
	byName := map[string]parkvectors.Case{}
	for _, c := range cases {
		byName[c.Name] = c
	}
	done := map[string]bool{}
	var out [][]parkvectors.Case
	for _, c := range cases {
		if done[c.Name] {
			continue
		}
		done[c.Name] = true
		if c.Sibling == "" {
			out = append(out, []parkvectors.Case{c})
			continue
		}
		sib, ok := byName[c.Sibling]
		if !ok {
			t.Fatalf("%s names a partner %s the vectors do not hold", c.Name, c.Sibling)
		}
		done[sib.Name] = true
		if c.Leg == "B" || c.Leg == "evac" {
			c, sib = sib, c
		}
		out = append(out, []parkvectors.Case{c, sib})
	}
	return out
}

func parkPinKey(name string, withProcess bool) string {
	if withProcess {
		return name + " pn=LINE"
	}
	return name + " pn=none"
}

// TestLoaderPark_EveryBuilderLeg pins where today's park lands every builder
// leg, in the world stock stands (each part's full on its home, the buffers
// free), with the process node the builders' callers send and without it.
//
// cost: 102 leaves, every builder leg (or swap pair) with and without a process
// node, each its own cloned database, loader and stock, sent through Core's
// intake and one dispatch pass. The park reads the loader's members, the bins
// and the in-flight orders through the store (LoaderMembersForPlacement,
// CheckDropoffCapacity), which the plan package's fakes do not stand in for,
// and the pin is the whole pass's answer for each leg.
func TestLoaderPark_EveryBuilderLeg(t *testing.T) {
	t.Parallel()
	cases, err := parkvectors.Cases()
	testutil.MustNoErr(t, err, "the park vectors")
	if len(cases) == 0 {
		t.Fatal("no park vectors: shared/parkvectors/vectors.json is empty")
	}
	seen := map[string]bool{}
	for _, unit := range parkUnits(t, cases) {
		for _, withProcess := range []bool{true, false} {
			unit, withProcess := unit, withProcess
			for _, c := range unit {
				seen[parkPinKey(c.Name, withProcess)] = true
			}
			t.Run(parkPinKey(unit[0].Name, withProcess), func(t *testing.T) {
				t.Parallel()
				db := testDB(t)
				w := newParkWorld(t, db)
				w.stock(t, db)
				for _, c := range unit {
					if c.Leg == "staged-deliver" && c.Config != "home-staging" {
						// The delivery collects what the stage set down there.
						w.put(t, db, "STAGE", "PART-Y", "pv-staged-incoming", 10)
					}
				}
				d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
				uuids := w.submitUnit(t, d, unit, withProcess)
				prScanPass(t, d, db, uuids...)
				for i, c := range unit {
					key := parkPinKey(c.Name, withProcess)
					got := w.landing(t, db, uuids[i])
					want, ok := parkBuilderPins[key]
					if !ok {
						t.Errorf("no pinned placement for %s; today it lands %q. Add:\n\t%q: %q,", key, got, key, got)
						continue
					}
					if got != want {
						t.Errorf("%s lands %q, pinned %q", key, got, want)
					}
				}
			})
		}
	}
	for key := range parkBuilderPins {
		if !seen[key] {
			t.Errorf("pin %q names no leg in shared/parkvectors: the builders no longer write it; delete the pin", key)
		}
	}
}

// parkBuilderPins is today's landing of every builder leg
// (TestLoaderPark_EveryBuilderLeg).
var parkBuilderPins = map[string]string{
	"home-fed/any/changeover/drop-release pn=LINE":             "sent drops=OUT",
	"home-fed/any/changeover/drop-release pn=none":             "sent drops=OUT",
	"home-fed/any/changeover/stage pn=LINE":                    "sent drops=HOME-B", // B23's class: a supply with no wait, set back down where it lifts
	"home-fed/any/changeover/stage pn=none":                    "sent drops=HOME-B", // B23's class: a supply with no wait, set back down where it lifts
	"home-fed/any/changeover/staged-deliver pn=LINE":           "sent drops=LINE",
	"home-fed/any/changeover/staged-deliver pn=none":           "sent drops=LINE",
	"home-fed/any/changeover/tooling pn=LINE":                  "sent drops=OUT,LINE",
	"home-fed/any/changeover/tooling pn=none":                  "sent drops=OUT,LINE",
	"home-fed/sequential/changeover/A pn=LINE":                 "sent drops=OUT,LINE",
	"home-fed/sequential/changeover/A pn=none":                 "sent drops=OUT,LINE",
	"home-fed/sequential/steady/A pn=LINE":                     "sent drops=OUT",
	"home-fed/sequential/steady/A pn=none":                     "sent drops=OUT",
	"home-fed/sequential/steady/backfill pn=LINE":              "sent drops=LINE", // moved by brief H H1 (a): B23 closed, a leg that fills its line is left to go there
	"home-fed/sequential/steady/backfill pn=none":              "sent drops=HOME", // B23's class, kept: with no process node (no builder sends one) legPlacesLineBin cannot read it
	"home-fed/single_robot/changeover/B pn=LINE":               "sent drops=STAGE,OSTAGE,LINE,OUT",
	"home-fed/single_robot/changeover/B pn=none":               "sent drops=STAGE,OSTAGE,LINE,OUT",
	"home-fed/single_robot/steady/A pn=LINE":                   "sent drops=STAGE,OSTAGE,LINE,OUT",
	"home-fed/single_robot/steady/A pn=none":                   "sent drops=STAGE,OSTAGE,LINE,OUT",
	"home-fed/two_robot/changeover/evac pn=LINE":               "sent drops=OUT",
	"home-fed/two_robot/changeover/evac pn=none":               "sent drops=OUT",
	"home-fed/two_robot/changeover/supply pn=LINE":             "sent drops=STAGE,LINE",
	"home-fed/two_robot/changeover/supply pn=none":             "sent drops=STAGE,LINE",
	"home-fed/two_robot/steady/A pn=LINE":                      "sent drops=STAGE,LINE",
	"home-fed/two_robot/steady/A pn=none":                      "sent drops=STAGE,LINE",
	"home-fed/two_robot/steady/B pn=LINE":                      "sent drops=OUT",
	"home-fed/two_robot/steady/B pn=none":                      "sent drops=OUT",
	"home-fed/two_robot_press_index/changeover/evac pn=LINE":   "sent drops=OUT,PAIR",
	"home-fed/two_robot_press_index/changeover/evac pn=none":   "sent drops=OUT,PAIR",
	"home-fed/two_robot_press_index/changeover/supply pn=LINE": "sent drops=LINE",
	"home-fed/two_robot_press_index/changeover/supply pn=none": "sent drops=LINE",
	"home-fed/two_robot_press_index/steady/A pn=LINE":          "sent drops=OUT,PAIR",
	"home-fed/two_robot_press_index/steady/A pn=none":          "sent drops=OUT,PAIR",
	"home-fed/two_robot_press_index/steady/B pn=LINE":          "sent drops=LINE",
	"home-fed/two_robot_press_index/steady/B pn=none":          "sent drops=LINE",

	"home-return/any/changeover/drop-release pn=LINE":             "sent drops=BUF",
	"home-return/any/changeover/drop-release pn=none":             "sent drops=BUF",
	"home-return/any/changeover/stage pn=LINE":                    "sent drops=STAGE",
	"home-return/any/changeover/stage pn=none":                    "sent drops=STAGE",
	"home-return/any/changeover/staged-deliver pn=LINE":           "sent drops=LINE",
	"home-return/any/changeover/staged-deliver pn=none":           "sent drops=LINE",
	"home-return/any/changeover/tooling pn=LINE":                  "sent drops=HOME,LINE", // B24's class: the drop on an occupied HOME is asked nothing
	"home-return/any/changeover/tooling pn=none":                  "sent drops=HOME,LINE", // B24's class: the drop on an occupied HOME is asked nothing
	"home-return/sequential/changeover/A pn=LINE":                 "sent drops=HOME,LINE", // B24: the drop on HOME (holding the old part's spare) is asked nothing
	"home-return/sequential/changeover/A pn=none":                 "sent drops=HOME,LINE", // B24: the drop on HOME (holding the old part's spare) is asked nothing
	"home-return/sequential/steady/A pn=LINE":                     "sent drops=BUF",
	"home-return/sequential/steady/A pn=none":                     "sent drops=BUF",
	"home-return/sequential/steady/backfill pn=LINE":              "sent drops=LINE",
	"home-return/sequential/steady/backfill pn=none":              "sent drops=LINE",
	"home-return/single_robot/changeover/B pn=LINE":               "sent drops=STAGE,OSTAGE,LINE,BUF", // its own earlier lift empties HOME; the gate reads it occupied
	"home-return/single_robot/changeover/B pn=none":               "sent drops=STAGE,OSTAGE,LINE,BUF", // its own earlier lift empties HOME; the gate reads it occupied
	"home-return/single_robot/steady/A pn=LINE":                   "sent drops=STAGE,OSTAGE,LINE,BUF", // its own earlier lift empties HOME; the gate reads it occupied
	"home-return/single_robot/steady/A pn=none":                   "sent drops=STAGE,OSTAGE,LINE,BUF", // its own earlier lift empties HOME; the gate reads it occupied
	"home-return/two_robot/changeover/evac pn=LINE":               "sent drops=BUF",
	"home-return/two_robot/changeover/evac pn=none":               "sent drops=BUF",
	"home-return/two_robot/changeover/supply pn=LINE":             "sent drops=STAGE,LINE",
	"home-return/two_robot/changeover/supply pn=none":             "sent drops=STAGE,LINE",
	"home-return/two_robot/steady/A pn=LINE":                      "sent drops=STAGE,LINE",
	"home-return/two_robot/steady/A pn=none":                      "sent drops=STAGE,LINE",
	"home-return/two_robot/steady/B pn=LINE":                      "sent drops=BUF",
	"home-return/two_robot/steady/B pn=none":                      "sent drops=BUF",
	"home-return/two_robot_press_index/changeover/evac pn=LINE":   "sent drops=HOME,PAIR", // B24's class: the drop on an occupied HOME is asked nothing
	"home-return/two_robot_press_index/changeover/evac pn=none":   "sent drops=HOME,PAIR", // B24's class: the drop on an occupied HOME is asked nothing
	"home-return/two_robot_press_index/changeover/supply pn=LINE": "sent drops=LINE",
	"home-return/two_robot_press_index/changeover/supply pn=none": "sent drops=LINE",
	"home-return/two_robot_press_index/steady/A pn=LINE":          "sent drops=HOME,PAIR", // B24's class: the drop on an occupied HOME is asked nothing
	"home-return/two_robot_press_index/steady/A pn=none":          "sent drops=HOME,PAIR", // B24's class: the drop on an occupied HOME is asked nothing
	"home-return/two_robot_press_index/steady/B pn=LINE":          "sent drops=LINE",
	"home-return/two_robot_press_index/steady/B pn=none":          "sent drops=LINE",

	"home-staging/any/changeover/drop-release pn=LINE":             "sent drops=BUF",
	"home-staging/any/changeover/drop-release pn=none":             "sent drops=BUF",
	"home-staging/any/changeover/staged-deliver pn=LINE":           "sent drops=LINE",      // moved by brief H H1 (a): B23's class closed, a leg that fills its line is left to go there
	"home-staging/any/changeover/staged-deliver pn=none":           "sent drops=HOME-B",    // B23's class, kept: with no process node (no builder sends one) legPlacesLineBin cannot read it
	"home-staging/any/changeover/tooling pn=LINE":                  "sent drops=HOME,LINE", // B24's class: the drop on an occupied HOME is asked nothing
	"home-staging/any/changeover/tooling pn=none":                  "sent drops=HOME,LINE", // B24's class: the drop on an occupied HOME is asked nothing
	"home-staging/sequential/changeover/A pn=LINE":                 "sent drops=HOME,LINE", // B24: the drop on HOME (holding the old part's spare) is asked nothing
	"home-staging/sequential/changeover/A pn=none":                 "sent drops=HOME,LINE", // B24: the drop on HOME (holding the old part's spare) is asked nothing
	"home-staging/sequential/steady/A pn=LINE":                     "sent drops=BUF",
	"home-staging/sequential/steady/A pn=none":                     "sent drops=BUF",
	"home-staging/sequential/steady/backfill pn=LINE":              "sent drops=LINE",            // moved by brief H H1 (a): B23 closed, a leg that fills its line is left to go there
	"home-staging/sequential/steady/backfill pn=none":              "sent drops=HOME",            // B23's class, kept: with no process node (no builder sends one) legPlacesLineBin cannot read it
	"home-staging/single_robot/changeover/B pn=LINE":               "sent drops=OSTAGE,LINE,BUF", // its own earlier lift empties HOME; the gate reads it occupied
	"home-staging/single_robot/changeover/B pn=none":               "sent drops=OSTAGE,LINE,BUF", // its own earlier lift empties HOME; the gate reads it occupied
	"home-staging/single_robot/steady/A pn=LINE":                   "sent drops=OSTAGE,LINE,BUF", // its own earlier lift empties HOME; the gate reads it occupied
	"home-staging/single_robot/steady/A pn=none":                   "sent drops=OSTAGE,LINE,BUF", // its own earlier lift empties HOME; the gate reads it occupied
	"home-staging/two_robot/changeover/evac pn=LINE":               "sent drops=BUF",
	"home-staging/two_robot/changeover/evac pn=none":               "sent drops=BUF",
	"home-staging/two_robot/changeover/supply pn=LINE":             "sent drops=LINE",
	"home-staging/two_robot/changeover/supply pn=none":             "sent drops=LINE",
	"home-staging/two_robot/steady/A pn=LINE":                      "sent drops=LINE",
	"home-staging/two_robot/steady/A pn=none":                      "sent drops=LINE",
	"home-staging/two_robot/steady/B pn=LINE":                      "sent drops=HOME",      // A2's row: HOME, no stamp, so nothing holds its release
	"home-staging/two_robot/steady/B pn=none":                      "sent drops=BUF",       // the wait proxy reads this return as no return
	"home-staging/two_robot_press_index/changeover/evac pn=LINE":   "sent drops=HOME,PAIR", // B24's class: the drop on an occupied HOME is asked nothing
	"home-staging/two_robot_press_index/changeover/evac pn=none":   "sent drops=HOME,PAIR", // B24's class: the drop on an occupied HOME is asked nothing
	"home-staging/two_robot_press_index/changeover/supply pn=LINE": "sent drops=LINE",
	"home-staging/two_robot_press_index/changeover/supply pn=none": "sent drops=LINE",
	"home-staging/two_robot_press_index/steady/A pn=LINE":          "sent drops=HOME,PAIR", // B24's class: the drop on an occupied HOME is asked nothing
	"home-staging/two_robot_press_index/steady/A pn=none":          "sent drops=HOME,PAIR", // B24's class: the drop on an occupied HOME is asked nothing
	"home-staging/two_robot_press_index/steady/B pn=LINE":          "sent drops=LINE",
	"home-staging/two_robot_press_index/steady/B pn=none":          "sent drops=LINE",

	"staged/any/changeover/drop-release pn=LINE":             "sent drops=BUF",
	"staged/any/changeover/drop-release pn=none":             "sent drops=BUF",
	"staged/any/changeover/stage pn=LINE":                    "sent drops=HOME-B", // B23's class: a supply with no wait, set back down where it lifts
	"staged/any/changeover/stage pn=none":                    "sent drops=HOME-B", // B23's class: a supply with no wait, set back down where it lifts
	"staged/any/changeover/staged-deliver pn=LINE":           "sent drops=LINE",
	"staged/any/changeover/staged-deliver pn=none":           "sent drops=LINE",
	"staged/any/changeover/tooling pn=LINE":                  "sent drops=HOME,LINE", // B24's class: the drop on an occupied HOME is asked nothing
	"staged/any/changeover/tooling pn=none":                  "sent drops=HOME,LINE", // B24's class: the drop on an occupied HOME is asked nothing
	"staged/sequential/changeover/A pn=LINE":                 "sent drops=HOME,LINE", // B24: the drop on HOME (holding the old part's spare) is asked nothing
	"staged/sequential/changeover/A pn=none":                 "sent drops=HOME,LINE", // B24: the drop on HOME (holding the old part's spare) is asked nothing
	"staged/sequential/steady/A pn=LINE":                     "sent drops=BUF",
	"staged/sequential/steady/A pn=none":                     "sent drops=BUF",
	"staged/sequential/steady/backfill pn=LINE":              "sent drops=LINE",                  // moved by brief H H1 (a): B23 closed, a leg that fills its line is left to go there
	"staged/sequential/steady/backfill pn=none":              "sent drops=HOME",                  // B23's class, kept: with no process node (no builder sends one) legPlacesLineBin cannot read it
	"staged/single_robot/changeover/B pn=LINE":               "sent drops=STAGE,OSTAGE,LINE,BUF", // its own earlier lift empties HOME; the gate reads it occupied
	"staged/single_robot/changeover/B pn=none":               "sent drops=STAGE,OSTAGE,LINE,BUF", // its own earlier lift empties HOME; the gate reads it occupied
	"staged/single_robot/steady/A pn=LINE":                   "sent drops=STAGE,OSTAGE,LINE,BUF", // its own earlier lift empties HOME; the gate reads it occupied
	"staged/single_robot/steady/A pn=none":                   "sent drops=STAGE,OSTAGE,LINE,BUF", // its own earlier lift empties HOME; the gate reads it occupied
	"staged/two_robot/changeover/evac pn=LINE":               "sent drops=BUF",
	"staged/two_robot/changeover/evac pn=none":               "sent drops=BUF",
	"staged/two_robot/changeover/supply pn=LINE":             "sent drops=STAGE,LINE",
	"staged/two_robot/changeover/supply pn=none":             "sent drops=STAGE,LINE",
	"staged/two_robot/steady/A pn=LINE":                      "sent drops=STAGE,LINE",
	"staged/two_robot/steady/A pn=none":                      "sent drops=STAGE,LINE",
	"staged/two_robot/steady/B pn=LINE":                      "sent drops=HOME",
	"staged/two_robot/steady/B pn=none":                      "sent drops=BUF",       // the wait proxy reads this return as no return
	"staged/two_robot_press_index/changeover/evac pn=LINE":   "sent drops=HOME,PAIR", // B24's class: the drop on an occupied HOME is asked nothing
	"staged/two_robot_press_index/changeover/evac pn=none":   "sent drops=HOME,PAIR", // B24's class: the drop on an occupied HOME is asked nothing
	"staged/two_robot_press_index/changeover/supply pn=LINE": "sent drops=LINE",
	"staged/two_robot_press_index/changeover/supply pn=none": "sent drops=LINE",
	"staged/two_robot_press_index/steady/A pn=LINE":          "sent drops=HOME,PAIR", // B24's class: the drop on an occupied HOME is asked nothing
	"staged/two_robot_press_index/steady/A pn=none":          "sent drops=HOME,PAIR", // B24's class: the drop on an occupied HOME is asked nothing
	"staged/two_robot_press_index/steady/B pn=LINE":          "sent drops=LINE",
	"staged/two_robot_press_index/steady/B pn=none":          "sent drops=LINE",
}

// clear takes every cart off role's node.
func (w *parkWorld) clear(t *testing.T, db *store.DB, role string) {
	t.Helper()
	bs, err := db.ListBinsByNode(w.at(t, db, role).ID)
	testutil.MustNoErr(t, err, "bins on "+role)
	for _, b := range bs {
		testutil.MustNoErr(t, db.DeleteBin(b.ID), "clear "+role)
	}
}

// fillBuffers stands a cart nobody lifts on both buffers.
func (w *parkWorld) fillBuffers(t *testing.T, db *store.DB) {
	t.Helper()
	w.put(t, db, "BUF", "PART-X", "pv-buf-full", 4)
	w.put(t, db, "BUF2", "PART-X", "pv-buf2-full", 4)
}

// manualOrderCase is the Manual Order page's complex order
// (shingo-edge/www/static/js/pages/manual-order.js, createOrder; the
// production page sends the same): fetch at the staging node, set it down
// there, a bare wait, lift it again, deliver. It has no Go builder; with no
// process node picked it reaches Core with none (handlers_api_orders.go,
// PressOf), the one door that does.
func manualOrderCase(t *testing.T, name, staging, production string) parkvectors.Case {
	t.Helper()
	steps, err := json.Marshal([]protocol.ComplexOrderStep{{Action: protocol.ActionPickup, Node: staging},
		{Action: protocol.ActionDropoff, Node: staging}, {Action: protocol.ActionWait},
		{Action: protocol.ActionPickup, Node: staging}, {Action: protocol.ActionDropoff, Node: production}})
	testutil.MustNoErr(t, err, "marshal "+name)
	return parkvectors.Case{Name: name, Config: "manual", Leg: "manual", PayloadCode: "PART-X", ProcessNode: "LINE",
		Steps: steps}
}

// parkRow is one of brief G's rows, in a world of its own: the walk's world,
// then world's changes.
type parkRow struct {
	name   string
	legs   []string // vector case names (a pair in creation order), or
	manual []string // a Manual Order page order: its staging and production nodes
	world  func(t *testing.T, db *store.DB, w *parkWorld)
	// noProcess sends the legs with no process node.
	noProcess bool
	// refuseLast: the fleet refuses the last leg's create in the first pass, and
	// a second pass sends it alone (its partner committed a pass earlier).
	refuseLast bool
	// want is each leg's landing (landing) after the pass; under refuseLast the
	// last leg's after the first pass, then after the second.
	want []string
	// sentence is the first leg's wait sentence, node names read as roles.
	sentence string
}

var parkBriefRows = []parkRow{
	{name: "WELD-5: two-robot through its home with staging, every buffer full",
		legs:  []string{"staged/two_robot/steady/A", "staged/two_robot/steady/B"},
		world: func(t *testing.T, db *store.DB, w *parkWorld) { w.fillBuffers(t, db) },
		want:  []string{"sent drops=STAGE,LINE", "sent drops=HOME"}},
	{name: "WELD-5: the return's create refused, sent alone a pass later, the full not yet lifted",
		legs:       []string{"staged/two_robot/steady/A", "staged/two_robot/steady/B"},
		refuseLast: true,
		want:       []string{"sent drops=STAGE,LINE", "queued:fleet-error drops=HOME", "sent drops=HOME"}},
	{name: "A's line: home as source, staging and destination, every buffer full",
		legs:  []string{"home-staging/two_robot/steady/A", "home-staging/two_robot/steady/B"},
		world: func(t *testing.T, db *store.DB, w *parkWorld) { w.fillBuffers(t, db) },
		want:  []string{"sent drops=LINE", "sent drops=HOME"}},
	{name: "A's line: a buffer holding an empty, the other full",
		legs: []string{"home-staging/two_robot/steady/A", "home-staging/two_robot/steady/B"},
		world: func(t *testing.T, db *store.DB, w *parkWorld) {
			w.put(t, db, "BUF", "", "pv-buf-empty", 0)
			w.put(t, db, "BUF2", "PART-X", "pv-buf2-full", 4)
		},
		want: []string{"sent drops=LINE", "sent drops=HOME"}},
	{name: "A's line: the return's create refused, sent alone a pass later",
		legs:       []string{"home-staging/two_robot/steady/A", "home-staging/two_robot/steady/B"},
		refuseLast: true,
		want:       []string{"sent drops=LINE", "queued:fleet-error drops=HOME", "sent drops=HOME"}},
	{name: "Sequential removal through its home, no buffer free",
		legs:     []string{"staged/sequential/steady/A"},
		world:    func(t *testing.T, db *store.DB, w *parkWorld) { w.fillBuffers(t, db) },
		want:     []string{"pending:loader-park-no-slot drops=HOME"},
		sentence: "Waiting for a slot at HOME — empty one of HOME, BUF, BUF2 and it goes"},
	{name: "Sequential removal, two homes (the second bare), no buffer free",
		legs: []string{"staged/sequential/steady/A"},
		world: func(t *testing.T, db *store.DB, w *parkWorld) {
			w.addHome2(t, db)
			w.fillBuffers(t, db)
		},
		want:     []string{"pending:loader-park-no-slot drops=HOME"},
		sentence: "Waiting for a slot at HOME — empty one of HOME, BUF, BUF2, HOME2 and it goes"},
	{name: "Sequential removal, two homes (the second bare), a buffer free",
		legs:  []string{"staged/sequential/steady/A"},
		world: func(t *testing.T, db *store.DB, w *parkWorld) { w.addHome2(t, db) },
		want:  []string{"sent drops=BUF"}},
	{name: "Sequential backfill after its removal landed in a buffer (B23)",
		legs: []string{"staged/sequential/steady/backfill"},
		world: func(t *testing.T, db *store.DB, w *parkWorld) {
			w.clear(t, db, "LINE")
			w.put(t, db, "BUF", "", "pv-removed-empty", 0)
		},
		want: []string{"sent drops=LINE"}}, // brief H H1 (a): was HOME, set back down on the home it lifts from
	{name: "Part change through two homes, the old home bare",
		legs:  []string{"staged/sequential/changeover/A"},
		world: func(t *testing.T, db *store.DB, w *parkWorld) { w.clear(t, db, "HOME") },
		want:  []string{"sent drops=HOME,LINE"}},
	{name: "Two-robot part change through two homes, the old home bare",
		legs:  []string{"staged/two_robot/changeover/supply", "staged/two_robot/changeover/evac"},
		world: func(t *testing.T, db *store.DB, w *parkWorld) { w.clear(t, db, "HOME") },
		want:  []string{"sent drops=STAGE,LINE", "sent drops=HOME"}},
	{name: "Manual Order page: staged at STAGE, delivered onto HOME, no process node",
		manual:    []string{"STAGE", "HOME"},
		world:     func(t *testing.T, db *store.DB, w *parkWorld) { w.put(t, db, "STAGE", "PART-X", "pv-manual-cart", 10) },
		noProcess: true,
		want:      []string{"sent drops=STAGE,BUF"}},
	{name: "Manual Order page: staged at HOME, delivered to the line, no process node",
		manual:    []string{"HOME", "LINE"},
		noProcess: true,
		want:      []string{"sent drops=HOME,LINE"}},
}

// TestLoaderPark_TheBriefsRows pins where today's park lands the rows brief G
// names, each in a world of its own: the two-robot lines through a home (with
// staging, and A's line with none) including a partner committed a pass
// earlier; the Sequential rows behind B23; the part change behind B24; the two
// homes; the one door that sends no process node.
func TestLoaderPark_TheBriefsRows(t *testing.T) {
	t.Parallel()
	cases, err := parkvectors.Cases()
	testutil.MustNoErr(t, err, "the park vectors")
	byName := map[string]parkvectors.Case{}
	for _, c := range cases {
		byName[c.Name] = c
	}
	for _, row := range parkBriefRows {
		row := row
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			var unit []parkvectors.Case
			for _, n := range row.legs {
				c, ok := byName[n]
				if !ok {
					t.Fatalf("the park vectors hold no %s", n)
				}
				unit = append(unit, c)
			}
			if row.manual != nil {
				unit = append(unit, manualOrderCase(t, "manual/"+row.manual[0]+"-"+row.manual[1], row.manual[0],
					row.manual[1]))
			}
			db := testDB(t)
			w := newParkWorld(t, db)
			w.stock(t, db)
			if row.world != nil {
				row.world(t, db, w)
			}
			backend := &failCreateFor{MockTrackingBackend: testdb.NewTrackingBackend()}
			if row.refuseLast {
				backend.uuid = parkLegUUID(unit[len(unit)-1].Name)
			}
			d, _ := newTestDispatcher(t, db, backend)
			uuids := w.submitUnit(t, d, unit, !row.noProcess)
			prScanPass(t, d, db, uuids...)
			var got []string
			for _, u := range uuids {
				got = append(got, w.landing(t, db, u))
			}
			if row.refuseLast {
				backend.uuid = ""
				prScanPass(t, d, db, uuids[len(uuids)-1])
				got = append(got, w.landing(t, db, uuids[len(uuids)-1]))
			}
			if strings.Join(got, " | ") != strings.Join(row.want, " | ") {
				t.Errorf("lands\n\t%q\npinned\n\t%q", got, row.want)
			}
			if row.sentence != "" {
				if s := w.sentence(t, db, uuids[0]); s != row.sentence {
					t.Errorf("waits saying %q, pinned %q", s, row.sentence)
				}
			}
		})
	}
}
