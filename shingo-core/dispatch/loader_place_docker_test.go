//go:build docker

package dispatch

import (
	"encoding/json"
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

// (g) REGRESSION GUARD: a two-robot swap supply leg whose SOURCE is a dedicated home
// (the real dedicated-loader supply shape: picks fresh bin FROM home → stages →
// delivers to line) must not be rerouted. Pattern A must exit when steps contain
// a wait — the staging wait embedded in the two-robot supply chain is the gate.
// Without this guard, Pattern A overwrites DeliveryNode=line with the home, making
// the order circular (pickup=home, deliver=home) and Core skips it (SPR-2026-06-23).
func TestPlaceForDedicatedLoader_SupplyFromHome_Untouched(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	home, _, outbound, _ := parkFixture(t, db)
	line := &nodes.Node{Name: "ALN-LINE", Enabled: true}
	if err := db.CreateNode(line); err != nil {
		t.Fatalf("create line: %v", err)
	}
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	// Supply leg: source = home (dedicated home, IS a loader home), delivery = line.
	// Steps mirror the two-robot swap shape: pickup home → stage → wait → pickup → drop line.
	supply := &orders.Order{
		EdgeUUID: "supply-from-home-1", StationID: "test", OrderType: protocol.OrderTypeComplex, Status: "staged",
		Quantity: 1, SourceNode: home.Name, DeliveryNode: line.Name, PayloadCode: "PART-X",
	}
	if err := db.CreateOrder(supply); err != nil {
		t.Fatalf("create supply order: %v", err)
	}
	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: home.Name},
		{Action: protocol.ActionDropoff, Node: outbound.Name},
		{Action: protocol.ActionWait, Node: outbound.Name},
		{Action: protocol.ActionPickup, Node: outbound.Name},
		{Action: protocol.ActionDropoff, Node: line.Name},
	}
	d.placeForDedicatedLoader(supply, steps)

	if supply.DeliveryNode != line.Name {
		t.Fatalf("supply-from-home leg DeliveryNode = %q, want UNCHANGED line %q (Pattern A must not touch supply legs)", supply.DeliveryNode, line.Name)
	}
}

// Supply leg with a staging wait step delivering to a home that already holds a
// physical bin must route to buffer — not attempt delivery to the occupied home and
// fault on arrival (mirrors the SPR-2026-06-23 / order-2237 incident).
func TestPlaceForDedicatedLoader_SupplyWithWait_HomeOccupied_RoutesBuffer(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	home, buffer, outbound, _ := parkFixture(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	// Physical bin already at the home; no in-flight orders (the case where
	// in-flight-only would falsely clear the home as available).
	makeLoaderBin(t, db, "PART-X", home.ID, "home-occupied", 100, time.Now().UTC())

	staging := &nodes.Node{Name: "LX-STAGE2", Enabled: true}
	if err := db.CreateNode(staging); err != nil {
		t.Fatalf("create staging: %v", err)
	}
	supply := &orders.Order{
		EdgeUUID: "supply-wait-1", StationID: "test", OrderType: protocol.OrderTypeComplex, Status: "staged",
		Quantity: 1, SourceNode: staging.Name, DeliveryNode: home.Name, PayloadCode: "PART-X",
	}
	if err := db.CreateOrder(supply); err != nil {
		t.Fatalf("create supply order: %v", err)
	}
	steps := []resolvedStep{
		{Action: protocol.ActionWait, Node: staging.Name},
		{Action: protocol.ActionPickup, Node: staging.Name},
		{Action: protocol.ActionDropoff, Node: home.Name},
	}
	d.placeForDedicatedLoader(supply, steps)

	if supply.DeliveryNode != buffer.Name {
		t.Fatalf("DeliveryNode = %q, want BUFFER %q (home physically occupied, supply+wait must not deliver there)",
			supply.DeliveryNode, buffer.Name)
	}
	_ = outbound // fixture requires it; not used by this case
}

// Supply leg with wait step delivering to a free home (no physical bin, no
// in-flight) must still route directly to the home.
func TestPlaceForDedicatedLoader_SupplyWithWait_HomeFree_RoutesHome(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	home, _, outbound, _ := parkFixture(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	staging := &nodes.Node{Name: "LX-STAGE3", Enabled: true}
	if err := db.CreateNode(staging); err != nil {
		t.Fatalf("create staging: %v", err)
	}
	supply := &orders.Order{
		EdgeUUID: "supply-wait-free-1", StationID: "test", OrderType: protocol.OrderTypeComplex, Status: "staged",
		Quantity: 1, SourceNode: staging.Name, DeliveryNode: home.Name, PayloadCode: "PART-X",
	}
	if err := db.CreateOrder(supply); err != nil {
		t.Fatalf("create supply order: %v", err)
	}
	steps := []resolvedStep{
		{Action: protocol.ActionWait, Node: staging.Name},
		{Action: protocol.ActionPickup, Node: staging.Name},
		{Action: protocol.ActionDropoff, Node: home.Name},
	}
	d.placeForDedicatedLoader(supply, steps)

	if supply.DeliveryNode != home.Name {
		t.Fatalf("DeliveryNode = %q, want HOME %q (home free, supply+wait should route home)",
			supply.DeliveryNode, home.Name)
	}
	_ = outbound
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

// FALLTHROUGH: Pattern A declining must not end placement.
//
// Regression for the defect where Pattern A's lookup failures each returned
// from placeForDedicatedLoader outright, so Pattern B was unreachable for any
// order whose SOURCE is not a dedicated-loader home — even when its DELIVERY
// node was one.
//
// Shape: a return leg that lifts a bin from a LINE node and delivers it to the
// loader home, with a restock already in flight to that home. Pattern A is
// entered (SourceNode set, no wait step) and declines (the line is not a home).
// Pattern B must then run and route the bin to the BUFFER, because the home is
// spoken for.
//
// Before the fix this asserted-on value stayed at the home — i.e. the order was
// left committed to a node another bin was already inbound to, which is the
// two-bins-on-one-node family. Pattern A never even looked at it.
func TestPlaceForDedicatedLoader_NonHomeSource_FallsThroughToPatternB(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	home, buffer, _, _ := parkFixture(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	line := &nodes.Node{Name: "LX-LINE-FT", Enabled: true}
	if err := db.CreateNode(line); err != nil {
		t.Fatalf("create line node: %v", err)
	}

	// The home is already spoken for by an inbound restock.
	makeInFlightTo(t, db, "restock-ft-1", home.Name)

	o := &orders.Order{
		EdgeUUID: "park-fallthrough-1", StationID: "test", OrderType: protocol.OrderTypeMove,
		Status: "staged", Quantity: 1,
		SourceNode: line.Name, DeliveryNode: home.Name, PayloadCode: "PART-X",
	}
	if err := db.CreateOrder(o); err != nil {
		t.Fatalf("create return order: %v", err)
	}

	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: line.Name},
		{Action: protocol.ActionDropoff, Node: home.Name},
	}
	d.placeForDedicatedLoader(o, steps)

	if o.DeliveryNode != buffer.Name {
		t.Fatalf("DeliveryNode = %q, want BUFFER %q — Pattern A declined (source is not a home) "+
			"so Pattern B must place this; leaving it at the home commits two bins to one node",
			o.DeliveryNode, buffer.Name)
	}
}

// Pattern A OWNING the placement must still short-circuit: when the source IS a
// home, Pattern B must not also run and re-decide. Guards the other direction of
// the handled-helper contract.
func TestPlaceForDedicatedLoader_HomeSource_PatternAStillShortCircuits(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	home, _, outbound, _ := parkFixture(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	evac := makeEvacOrder(t, db, "park-shortcircuit-1", home.Name, outbound.Name)
	d.placeForDedicatedLoader(evac, simpleEvacSteps(home.Name, outbound.Name))

	if evac.DeliveryNode != home.Name {
		t.Fatalf("DeliveryNode = %q, want HOME %q — Pattern A owned this and the home is free",
			evac.DeliveryNode, home.Name)
	}
}

// ── The SMN_016 / SMN_035 regression (Springfield, 2026-08-26) ───────────────
//
// A swap RETURN leg carries a station wait — the robot dwells at the line until
// the operator releases it — so the old hasWaitStep proxy read it as a supply
// leg, ran the physical gate against its own home, saw the carrier its sibling
// was about to lift, and yielded to a buffer. The home was then unclaimed, the
// replenishment loop filled it seconds after the supply leg cleared it, and the
// returning carrier had nowhere to land. Its record was evicted as a ghost.
//
// parkSwapPair builds that exact shape: a supply leg lifting from the home to
// the line, and its return sibling coming back to the home, both with the line
// as ProcessNode.
func parkSwapPair(t *testing.T, db *store.DB, home, line string, linkSibling bool) (ret *orders.Order, retSteps []resolvedStep) {
	t.Helper()
	supplySteps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: home},
		{Action: protocol.ActionDropoff, Node: line},
	}
	raw, err := json.Marshal(supplySteps)
	if err != nil {
		t.Fatalf("marshal supply steps: %v", err)
	}
	supply := &orders.Order{
		EdgeUUID: "park-swap-supply", StationID: "test", OrderType: protocol.OrderTypeComplex, Status: "staged",
		Quantity: 1, SourceNode: home, DeliveryNode: line, ProcessNode: line,
		PayloadCode: "PART-X", StepsJSON: string(raw),
	}
	if err := db.CreateOrder(supply); err != nil {
		t.Fatalf("create supply leg: %v", err)
	}
	retSteps = []resolvedStep{
		{Action: protocol.ActionWait, Node: line},
		{Action: protocol.ActionPickup, Node: line},
		{Action: protocol.ActionDropoff, Node: home},
	}
	ret = &orders.Order{
		EdgeUUID: "park-swap-return", StationID: "test", OrderType: protocol.OrderTypeComplex, Status: "staged",
		Quantity: 1, SourceNode: line, DeliveryNode: home, ProcessNode: line, PayloadCode: "PART-X",
	}
	if linkSibling {
		ret.SiblingOrderUUID = supply.EdgeUUID
	}
	if err := db.CreateOrder(ret); err != nil {
		t.Fatalf("create return leg: %v", err)
	}
	return ret, retSteps
}

// The regression itself: the carrier standing on the home is the one the supply
// sibling lifts, so the return must HOLD the home. Holding it is what makes the
// order in-flight to the home, which is what makes loader_replenish's gate yield
// — the whole mechanism that failed on 2026-08-26.
func TestPlaceForDedicatedLoader_ReturnWithWait_SiblingLifting_HoldsHome(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	home, buffer, _, _ := parkFixture(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	line := &nodes.Node{Name: "LX-LINE", Enabled: true}
	if err := db.CreateNode(line); err != nil {
		t.Fatalf("create line node: %v", err)
	}
	// The carrier the supply sibling is about to lift off the home.
	makeLoaderBin(t, db, "PART-X", home.ID, "sibling-lifts-this", 100, time.Now().UTC())

	ret, retSteps := parkSwapPair(t, db, home.Name, line.Name, true)
	d.placeForDedicatedLoader(ret, retSteps)

	if ret.DeliveryNode != home.Name {
		t.Fatalf("DeliveryNode = %q, want HOME %q — the bin on the home is the one the sibling lifts, "+
			"so the return must hold the home rather than yield it to the replenishment loop (buffer was %q)",
			ret.DeliveryNode, home.Name, buffer.Name)
	}
}

// The other half of the distinction the old evac branch could not draw: a home
// holding a carrier NOBODY is coming for is genuinely occupied, and routing a
// robot at it faults on arrival. That one still goes to buffer.
func TestPlaceForDedicatedLoader_ReturnWithWait_ForeignCarrier_RoutesBuffer(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	home, buffer, _, _ := parkFixture(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	line := &nodes.Node{Name: "LX-LINE2", Enabled: true}
	if err := db.CreateNode(line); err != nil {
		t.Fatalf("create line node: %v", err)
	}
	makeLoaderBin(t, db, "PART-X", home.ID, "foreign-carrier", 100, time.Now().UTC())

	// No sibling link: nothing vouches for the carrier standing on the home.
	ret, retSteps := parkSwapPair(t, db, home.Name, line.Name, false)
	d.placeForDedicatedLoader(ret, retSteps)

	if ret.DeliveryNode != buffer.Name {
		t.Fatalf("DeliveryNode = %q, want BUFFER %q — no sibling lifts the carrier on the home, "+
			"so it is a real occupant and the return must not be driven at it",
			ret.DeliveryNode, buffer.Name)
	}
}

// An unreadable role (no ProcessNode) must keep the previous behaviour verbatim
// rather than collapsing into "supply". Live traffic has carried a ProcessNode on
// every complex order since 2026-05-04, so this covers the 193 historical rows and
// any future regression that stops stamping it.
func TestPlaceForDedicatedLoader_ReturnWithWait_UnknownRole_KeepsProxyBehaviour(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	home, buffer, _, _ := parkFixture(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	line := &nodes.Node{Name: "LX-LINE3", Enabled: true}
	if err := db.CreateNode(line); err != nil {
		t.Fatalf("create line node: %v", err)
	}
	makeLoaderBin(t, db, "PART-X", home.ID, "unknown-role-occupant", 100, time.Now().UTC())

	ret, retSteps := parkSwapPair(t, db, home.Name, line.Name, true)
	ret.ProcessNode = "" // role unreadable — falls back to the wait-step proxy
	d.placeForDedicatedLoader(ret, retSteps)

	if ret.DeliveryNode != buffer.Name {
		t.Fatalf("DeliveryNode = %q, want BUFFER %q — with no ProcessNode the leg must move exactly as "+
			"the wait-step proxy moved it, not be reclassified", ret.DeliveryNode, buffer.Name)
	}
}
