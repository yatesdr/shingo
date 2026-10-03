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
// points picks today, with nothing held and under each hold it already respects.
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
// the order's own plan delivering there first, a carrier that does not belong.

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
