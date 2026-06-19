//go:build docker

package dispatch

import (
	"sync"
	"testing"
	"time"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// parkFixture wires a dedicated consume loader for PART-X with a pinned home, an
// explicit buffer slot, and an outbound (drain) node, and returns them. Mirrors
// dedicatedLoaderFixture but adds the outbound node the evac drains to.
func parkFixture(t *testing.T, db *store.DB) (home, buffer, outbound *nodes.Node, loaderID int64) {
	t.Helper()
	// setupTestData seeds the DEFAULT bin type (needed by makeLoaderBin);
	// dedicatedLoaderFixture creates the PART-X payload + the loader (home + buffer).
	setupTestData(t, db)
	home, buffer = dedicatedLoaderFixture(t, db, "consume")
	h, err := db.GetLoaderHomeByPositionNode(home.ID)
	if err != nil || h == nil {
		t.Fatalf("resolve loader home: %v", err)
	}
	loaderID = h.LoaderID
	outbound = &nodes.Node{Name: "LX-OUT", Enabled: true}
	if err := db.CreateNode(outbound); err != nil {
		t.Fatalf("create outbound: %v", err)
	}
	return home, buffer, outbound, loaderID
}

// makeEvacOrder inserts a changeover-evac order returning a partial from the home,
// initially draining to outbound (DeliveryNode=outbound), and returns it.
func makeEvacOrder(t *testing.T, db *store.DB, uuid, home, outbound string) *orders.Order {
	t.Helper()
	o := &orders.Order{
		EdgeUUID: uuid, StationID: "test", OrderType: protocol.OrderTypeMove, Status: "staged",
		Quantity: 1, SourceNode: home, DeliveryNode: outbound, PayloadCode: "PART-X",
	}
	if err := db.CreateOrder(o); err != nil {
		t.Fatalf("create evac order: %v", err)
	}
	return o
}

// makeInFlightTo inserts an active (non-terminal, non-queued) order delivering to
// node — a restock the park must observe via the Core in-flight authority.
func makeInFlightTo(t *testing.T, db *store.DB, uuid, node string) {
	t.Helper()
	o := &orders.Order{
		EdgeUUID: uuid, StationID: "test", OrderType: protocol.OrderTypeRetrieveEmpty, Status: "in_transit",
		Quantity: 1, DeliveryNode: node, PayloadCode: "PART-X",
	}
	if err := db.CreateOrder(o); err != nil {
		t.Fatalf("create in-flight order: %v", err)
	}
}

func simpleEvacSteps(home, outbound string) []resolvedStep {
	return []resolvedStep{
		{Action: protocol.ActionPickup, Node: home},
		{Action: protocol.ActionDropoff, Node: outbound},
	}
}

// (b) HOME: no restock in-flight to the home → the returning partial goes HOME.
func TestPlaceForDedicatedLoader_HomeFree_ReturnsHome(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	home, _, outbound, _ := parkFixture(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	evac := makeEvacOrder(t, db, "park-home-1", home.Name, outbound.Name)
	d.placeForDedicatedLoader(evac, simpleEvacSteps(home.Name, outbound.Name))

	if evac.DeliveryNode != home.Name {
		t.Fatalf("DeliveryNode = %q, want HOME %q (home is free)", evac.DeliveryNode, home.Name)
	}
}

// (a) BUFFER: a restock is in-flight to the home → the home is not free → the
// partial parks in a free buffer slot.
func TestPlaceForDedicatedLoader_RestockInFlight_ParksBuffer(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	home, buffer, outbound, _ := parkFixture(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	makeInFlightTo(t, db, "restock-1", home.Name) // a fresh bin committed to the home
	evac := makeEvacOrder(t, db, "park-buf-1", home.Name, outbound.Name)
	d.placeForDedicatedLoader(evac, simpleEvacSteps(home.Name, outbound.Name))

	if evac.DeliveryNode != buffer.Name {
		t.Fatalf("DeliveryNode = %q, want BUFFER %q (restock in-flight to home)", evac.DeliveryNode, buffer.Name)
	}
}

// Single-robot swap: the SAME order delivers the new style to the home, so the
// returning partial must yield to the buffer even with nothing else in-flight (the
// in-flight count excludes the order's own row — the step-scan catches it).
func TestPlaceForDedicatedLoader_SelfDeliversHome_ParksBuffer(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	home, buffer, outbound, _ := parkFixture(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	evac := makeEvacOrder(t, db, "park-self-1", home.Name, outbound.Name)
	// Steps include a dropoff to the home (the new-style delivery) before the final
	// outbound dropoff — the single-robot-swap shape.
	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: home.Name},
		{Action: protocol.ActionDropoff, Node: home.Name}, // new style delivered to home (same order)
		{Action: protocol.ActionDropoff, Node: outbound.Name},
	}
	d.placeForDedicatedLoader(evac, steps)

	if evac.DeliveryNode != buffer.Name {
		t.Fatalf("DeliveryNode = %q, want BUFFER %q (same order delivers new bin to home)", evac.DeliveryNode, buffer.Name)
	}
}

// (e) BUFFER FULL → DRAIN: home not free AND the buffer slot already holds a bin →
// drain (DeliveryNode left at outbound). Never double-commit a buffer slot.
func TestPlaceForDedicatedLoader_BufferFull_Drains(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	home, buffer, outbound, _ := parkFixture(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	makeInFlightTo(t, db, "restock-2", home.Name)                                  // home not free
	makeLoaderBin(t, db, "PART-X", buffer.ID, "buf-occupied", 4, time.Now().UTC()) // buffer occupied
	evac := makeEvacOrder(t, db, "park-drain-1", home.Name, outbound.Name)
	d.placeForDedicatedLoader(evac, simpleEvacSteps(home.Name, outbound.Name))

	if evac.DeliveryNode != outbound.Name {
		t.Fatalf("DeliveryNode = %q, want DRAIN/outbound %q (home not free, buffer full)", evac.DeliveryNode, outbound.Name)
	}
}

// (f) REGRESSION GUARD: a two-robot swap's SUPPLY leg (source is staging, NOT a
// loader home; it delivers a fresh bin TO the home) is left completely untouched —
// the park never re-places it and never gates it. Proves the supply leg can't be
// caught by this path (the 2b05dce/ALN_003 deadlock stays closed).
func TestPlaceForDedicatedLoader_SupplyLeg_Untouched(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	home, _, _, _ := parkFixture(t, db)
	staging := &nodes.Node{Name: "LX-STAGE", Enabled: true}
	if err := db.CreateNode(staging); err != nil {
		t.Fatalf("create staging: %v", err)
	}
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	// Supply leg: source = staging (not a home), delivery = home.
	supply := &orders.Order{
		EdgeUUID: "supply-1", StationID: "test", OrderType: protocol.OrderTypeMove, Status: "staged",
		Quantity: 1, SourceNode: staging.Name, DeliveryNode: home.Name, PayloadCode: "PART-X",
	}
	if err := db.CreateOrder(supply); err != nil {
		t.Fatalf("create supply order: %v", err)
	}
	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: staging.Name},
		{Action: protocol.ActionDropoff, Node: home.Name},
	}
	d.placeForDedicatedLoader(supply, steps)

	if supply.DeliveryNode != home.Name {
		t.Fatalf("supply leg DeliveryNode = %q, want UNCHANGED %q (park must not touch a supply leg)", supply.DeliveryNode, home.Name)
	}
}

// The release-time link: after placeForDedicatedLoader rewrites DeliveryNode, the
// existing patchRedirectSegments must overlay it onto the final dropoff step so the
// fleet follows the park choice. This is why no Edge step change is needed — Core is
// the single authority and the existing redirect overlay carries it.
func TestPlaceForDedicatedLoader_RedirectCarriesParkToFinalDropoff(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	home, buffer, outbound, _ := parkFixture(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	makeInFlightTo(t, db, "restock-redir", home.Name) // forces buffer
	evac := makeEvacOrder(t, db, "park-redir-1", home.Name, outbound.Name)
	d.placeForDedicatedLoader(evac, simpleEvacSteps(home.Name, outbound.Name))
	if evac.DeliveryNode != buffer.Name {
		t.Fatalf("precondition: DeliveryNode = %q, want buffer %q", evac.DeliveryNode, buffer.Name)
	}

	// Simulate the released final segment (pickup home, dropoff outbound) and apply
	// the existing redirect overlay.
	segment := simpleEvacSteps(home.Name, outbound.Name)
	d.patchRedirectSegments(segment, evac, false)
	if segment[1].Node != buffer.Name {
		t.Fatalf("final dropoff = %q, want buffer %q — patchRedirectSegments must carry the park choice to the fleet", segment[1].Node, buffer.Name)
	}
}

// (d) NEVER-2N RACE: a restock committed in-flight to the home, and N partial-returns
// resolving concurrently — every one must yield to the buffer (none to the home).
// Run under -race.
func TestPlaceForDedicatedLoader_Race_RestockInFlight_AllYieldBuffer(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	home, buffer, outbound, _ := parkFixture(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	makeInFlightTo(t, db, "restock-race", home.Name)

	const n = 8
	evacs := make([]*orders.Order, n)
	for i := 0; i < n; i++ {
		evacs[i] = makeEvacOrder(t, db, "park-race-"+string(rune('a'+i)), home.Name, outbound.Name)
	}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(o *orders.Order) {
			defer wg.Done()
			d.placeForDedicatedLoader(o, simpleEvacSteps(home.Name, outbound.Name))
		}(evacs[i])
	}
	wg.Wait()

	for _, e := range evacs {
		if e.DeliveryNode == home.Name {
			t.Fatalf("order %s landed at HOME while a restock was in-flight — never-2N violated", e.EdgeUUID)
		}
		// buffer or drain are both safe (≤1 at the single buffer slot; the rest drain).
		if e.DeliveryNode != buffer.Name && e.DeliveryNode != outbound.Name {
			t.Fatalf("order %s DeliveryNode = %q, want buffer or drain", e.EdgeUUID, e.DeliveryNode)
		}
	}
}
