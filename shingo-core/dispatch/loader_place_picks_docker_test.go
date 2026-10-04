//go:build docker

package dispatch

import (
	"fmt"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
	"shingocore/store/orders"
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
			o.EdgeUUID, o.StationID, o.OrderType, o.Status = "pick-supply", "test", OrderTypeComplex, "staged"
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
			testutil.MustNoErr(t, db.ConfirmSlotReservation(n(fx).ID, h.ID), "confirm the reservation")
		}
	}
}

// hardClaimedBy puts another order's hard claim on the node.
func hardClaimedBy(n func(fx *pickFixture) *nodes.Node) func(t *testing.T, db *store.DB, fx *pickFixture) {
	return func(t *testing.T, db *store.DB, fx *pickFixture) {
		h := strangerOrder(t, db, "pick-claim-"+n(fx).Name)
		testutil.MustNoErr(t, db.ReserveSlot(n(fx).ID, h.ID), "reserve "+n(fx).Name)
		testutil.MustNoErr(t, db.ConfirmSlotClaim(n(fx).ID, h.ID, nil), "hard-claim "+n(fx).Name)
	}
}

// namedBy points a live order at the node that holds no claim and no
// reservation: it names the node and nothing more.
func namedBy(n func(fx *pickFixture) *nodes.Node) func(t *testing.T, db *store.DB, fx *pickFixture) {
	return func(t *testing.T, db *store.DB, fx *pickFixture) {
		testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.EdgeUUID, o.StationID, o.OrderType, o.Status = "pick-named-"+n(fx).Name, "test", OrderTypeComplex, StatusQueued
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
	{point: "buffer-walk", hold: "none (home in flight)", want: "buffer",
		place: placeHomeSource(evacSteps, inFlightTo(homeOf))},
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
			h := strangerOrder(t, db, "qc-claimer")
			testutil.MustNoErr(t, db.ReserveSlot(hold.ID, h.ID), "reserve the containment node")
			testutil.MustNoErr(t, db.ConfirmSlotClaim(hold.ID, h.ID, nil), "hard-claim the containment node")
		}},
		// Reservation and hard claim only: a live order merely naming the node
		// still diverts.
		{hold: "containment node named by a live order", want: "diverted", set: func(t *testing.T, db *store.DB, hold *nodes.Node) {
			testdb.CreateOrder(t, db, func(o *orders.Order) {
				o.EdgeUUID, o.StationID, o.OrderType, o.Status = "qc-named", "test", OrderTypeComplex, StatusQueued
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
// counts a name as nothing, on both sides: the refill is created, and another
// return placed while the evac waits still takes the home.
func TestSpringfieldIncident_20260805_AWaitingReturnNamingTheHomeHoldsNothing(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	home, _, _, loaderID := springfieldLoaderFixture(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID, o.StationID, o.OrderType, o.Status = "spr-0805-evac", "test", OrderTypeComplex, StatusSourcing
		o.DeliveryNode, o.ProcessNode, o.PayloadCode = home.Name, "SPR-0805-LINE", "PART-X"
	})

	cfg, ok, err := d.LoadReplenishConfig(loaderID)
	if err != nil || !ok {
		t.Fatalf("load replenish config for loader %d: ok=%v err=%v", loaderID, ok, err)
	}
	res, err := d.ReplenishLoader(ReplenishRequest{
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
	if wait := d.placeForDedicatedLoader(ret, steps, nil); wait != "" || ret.DeliveryNode != home.Name {
		t.Fatalf("a return placed while the evac and the refill name %s landed at %s (wait %q); "+
			"for a loader the first to claim goes", home.Name, ret.DeliveryNode, wait)
	}
}
