//go:build docker

package store_test

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/domain"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// order_trip_docker_test.go: where an order's bin is on its trip (domain.Trip),
// asked through store.DB.OrderTrips, the one reader of the question the doors
// that end a live order ask (final brief 1b).

// tripRig is one database with the places the cases stand their carts on: a
// pickup (SRC), the line (LINE), a staging spot (STG), an outbound spot (OUT),
// a slot named only under its group (GRP.SLOT) and _TRANSIT.
type tripRig struct {
	db                                 *store.DB
	src, line, stg, out, slot, transit *nodes.Node
	group                              string
}

func newTripRig(t *testing.T, db *store.DB) tripRig {
	t.Helper()
	sd := testdb.SetupStandardData(t, db)
	node := func(name string, parent *int64) *nodes.Node {
		n := &nodes.Node{Name: name, Enabled: true, ParentID: parent}
		testutil.MustNoErr(t, db.CreateNode(n), "create node "+name)
		return n
	}
	transit, err := db.GetNodeByName(domain.TransitNodeName)
	testutil.MustNoErr(t, err, "find _TRANSIT")
	group := testdb.MarkedGroup(t, db, "TRIP-GRP")
	return tripRig{db: db, src: sd.StorageNode, line: sd.LineNode, stg: node("TRIP-STG", nil),
		out: node("TRIP-OUT", nil), slot: node("TRIP-SLOT", group), transit: transit, group: "TRIP-GRP"}
}

// order writes an order with the plan steps (none: the plan its endpoints
// imply, SRC to LINE) at status.
func (r tripRig) order(t *testing.T, uuid string, status protocol.Status, steps string) *orders.Order {
	t.Helper()
	return testdb.CreateOrder(t, r.db, func(o *orders.Order) {
		o.EdgeUUID, o.Status, o.StepsJSON = uuid, status, steps
		o.SourceNode, o.DeliveryNode = r.src.Name, r.line.Name
	})
}

// cart stands a bin on node at, writes the order's row for it (lifted at
// pick, set down at drop, -1 for an open lift) and has the order claim it
// when claimed.
func (r tripRig) cart(t *testing.T, o *orders.Order, label string, at *nodes.Node, pick, drop int, claimed bool) {
	t.Helper()
	b := testdb.CreateBinAtNode(t, r.db, "PART-A", at.ID, label)
	if drop < 0 {
		if _, err := r.db.Exec(`INSERT INTO order_bins (order_id, bin_id, pick_step) VALUES ($1, $2, $3)`,
			o.ID, b.ID, pick); err != nil {
			t.Fatalf("open lift row for %s: %v", label, err)
		}
	} else {
		testdb.SeedCartRow(t, r.db, o.ID, b.ID, pick, drop)
	}
	if claimed {
		testdb.ClaimBinForTest(t, r.db, b.ID, o.ID)
	}
}

// relay is the single-robot swap's plan: the fresh cart (A) is lifted at SRC
// and parked at STG, the line's cart (B) is lifted at LINE and set down at
// OUT, and A is lifted again at STG and set down at LINE, its final drop.
const relay = `[{"action":"pickup","node":"STORAGE-A1"},{"action":"dropoff","node":"TRIP-STG"},` +
	`{"action":"pickup","node":"LINE1-IN"},{"action":"dropoff","node":"TRIP-OUT"},` +
	`{"action":"pickup","node":"TRIP-STG"},{"action":"dropoff","node":"LINE1-IN"}]`

// relayCarts writes the relay's three rows (A 0-1, B 2-3, A 4-5) with A at a
// and B at b.
func (r tripRig) relayCarts(t *testing.T, o *orders.Order, a, b *nodes.Node, aClaimed, bClaimed bool) {
	t.Helper()
	bin := func(label string, at *nodes.Node, claimed bool) int64 {
		x := testdb.CreateBinAtNode(t, r.db, "PART-A", at.ID, label)
		if claimed {
			testdb.ClaimBinForTest(t, r.db, x.ID, o.ID)
		}
		return x.ID
	}
	binA, binB := bin(o.EdgeUUID+"-A", a, aClaimed), bin(o.EdgeUUID+"-B", b, bClaimed)
	testdb.SeedCartRow(t, r.db, o.ID, binA, 0, 1)
	testdb.SeedCartRow(t, r.db, o.ID, binB, 2, 3)
	testdb.SeedCartRow(t, r.db, o.ID, binA, 4, 5)
}

// TestOrderTrips_EveryAnswer: the reader's answer for each shape a door meets,
// the relay among them, and what it costs: one statement for every order the
// rows cannot answer, whatever their number, and none when the rows answer
// all of them.
func TestOrderTrips_EveryAnswer(t *testing.T) {
	t.Parallel()
	db, cfg := testdb.OpenWithConfig(t)
	r := newTripRig(t, db)

	type tripCase struct {
		name    string
		want    domain.Trip
		fromRow bool
		o       *orders.Order
	}
	var cases []tripCase
	add := func(name string, want domain.Trip, fromRow bool, o *orders.Order) {
		cases = append(cases, tripCase{name, want, fromRow, o})
	}

	o := r.order(t, "trip-at-pickup", protocol.StatusInTransit, "")
	r.cart(t, o, "TRIP-PICKUP", r.src, 0, 1, true)
	add("its claimed cart still at its pickup", domain.TripNotLifted, false, o)

	o = r.order(t, "trip-carried", protocol.StatusInTransit, "")
	r.cart(t, o, "TRIP-CARRIED", r.transit, 0, 1, true)
	add("its claimed cart at _TRANSIT", domain.TripCarried, false, o)

	o = r.order(t, "trip-landed", protocol.StatusInTransit, "")
	r.cart(t, o, "TRIP-LANDED", r.line, 0, 1, false)
	add("landed: its cart at its drop, the claim released, the order still in_transit", domain.TripSetDown, false, o)

	o = r.order(t, "trip-at-drop-claimed", protocol.StatusInTransit, "")
	r.cart(t, o, "TRIP-AT-DROP-CLAIMED", r.line, 0, 1, true)
	add("its cart at its drop with the order's claim still on it", domain.TripNotLifted, false, o)

	o = r.order(t, "trip-dotted", protocol.StatusInTransit,
		`[{"action":"pickup","node":"STORAGE-A1"},{"action":"dropoff","node":"`+r.group+`.TRIP-SLOT"}]`)
	r.cart(t, o, "TRIP-DOTTED", r.slot, 0, 1, false)
	add("landed at a drop the plan names under its group (GRP.SLOT)", domain.TripSetDown, false, o)

	o = r.order(t, "trip-open-lift", protocol.StatusStaged, `[{"action":"pickup","node":"STORAGE-A1"},{"action":"wait"}]`)
	r.cart(t, o, "TRIP-OPEN", r.src, 0, -1, false)
	add("an open lift (no drop planned) not lifted: no final drop is not every final drop", domain.TripNotLifted, false, o)

	o = r.order(t, "trip-relay-carrying", protocol.StatusInTransit, relay)
	r.relayCarts(t, o, r.stg, r.transit, true, true)
	add("relay: A parked at STG with its claim, B in the forks", domain.TripCarried, false, o)

	o = r.order(t, "trip-relay-mid", protocol.StatusInTransit, relay)
	r.relayCarts(t, o, r.stg, r.out, false, false)
	add("relay: A at STG and B at OUT, both unclaimed: A's final drop is LINE", domain.TripNotLifted, false, o)

	o = r.order(t, "trip-relay-done", protocol.StatusInTransit, relay)
	r.relayCarts(t, o, r.line, r.out, false, false)
	add("relay: A at LINE and B at OUT, both unclaimed", domain.TripSetDown, false, o)

	o = r.order(t, "trip-delivered", protocol.StatusDelivered, "")
	r.cart(t, o, "TRIP-DELIVERED", r.src, 0, 1, true)
	add("delivered, whatever its bins say", domain.TripSetDown, true, o)

	add("no cart", domain.TripNotLifted, true, r.order(t, "trip-no-cart", protocol.StatusInTransit, ""))

	// Each order as a door holds it: read back with its carts.
	var all, rowsOnly []*orders.Order
	for i, c := range cases {
		fresh, err := db.GetOrder(c.o.ID)
		testutil.MustNoErr(t, err, "reload "+c.name)
		cases[i].o = fresh
		all = append(all, fresh)
		if c.fromRow {
			rowsOnly = append(rowsOnly, fresh)
		}
	}

	cdb, counter, err := store.OpenCounting(cfg)
	testutil.MustNoErr(t, err, "counting db")
	t.Cleanup(func() { cdb.Close() })

	counter.Reset()
	trips, err := cdb.OrderTrips(all)
	testutil.MustNoErr(t, err, "ask every case at once")
	if n := counter.Count(); n != 1 {
		t.Errorf("%d orders asked at once cost %d statements, want 1: %v", len(all), n, counter.Statements())
	}
	for _, c := range cases {
		got, ok := trips[c.o.ID]
		if !ok || got != c.want {
			t.Errorf("%s: %v (answered %v), want %v", c.name, got, ok, c.want)
		}
		counter.Reset()
		one, err := cdb.OrderTrips([]*orders.Order{c.o})
		testutil.MustNoErr(t, err, "ask "+c.name+" alone")
		want := int64(1)
		if c.fromRow {
			want = 0
		}
		if n := counter.Count(); n != want || one[c.o.ID] != c.want {
			t.Errorf("%s alone: %v in %d statements, want %v in %d", c.name, one[c.o.ID], n, c.want, want)
		}
	}

	counter.Reset()
	trips, err = cdb.OrderTrips(rowsOnly)
	testutil.MustNoErr(t, err, "ask the rows-only cases")
	if n := counter.Count(); n != 0 || len(trips) != len(rowsOnly) {
		t.Errorf("%d orders their rows answer: %d answers in %d statements, want %d in 0", len(rowsOnly),
			len(trips), n, len(rowsOnly))
	}
}
