//go:build docker

package dispatch

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// widen_supply_test.go — C(ii): supply-pickup widening in DispatchPreparedComplex.
//
// The disposition change under test: a supply-shaped pickup whose node-local
// pool is dry PARKS the order (waiting_for_material) instead of letting it
// fail terminal downstream — the terminal-skip → swap-peer-cancel →
// autoreorder runaway (Springfield 2026-07-21) collapses into one escapable
// wait. Evac/removal pickups and empty legs stay byte-untouched.
//
// The fixture population that matters: April-legacy complex supply orders
// carry ProcessNode="" (~12% at SPR). With no ProcessNode, isRemovalPickup
// degrades to never-removal, so their pickups are ALL widened — intended,
// because that population is supply-shaped (pickup SMN → dropoff SLN → wait).

func widenNode(t *testing.T, db interface {
	CreateNode(n *nodes.Node) error
}, name string, synthetic bool) {
	t.Helper()
	testutil.MustNoErr(t, db.CreateNode(&nodes.Node{Name: name, Enabled: true, IsSynthetic: synthetic}), "create node "+name)
}

func mkWidenOrder(t *testing.T, db interface {
	CreateOrder(o *orders.Order) error
}, uuid, payload, source, delivery, processNode string, steps []resolvedStep) *orders.Order {
	t.Helper()
	stepsJSON, _ := json.Marshal(steps)
	o := &orders.Order{
		EdgeUUID:     uuid,
		StationID:    "line-1",
		OrderType:    OrderTypeComplex,
		Status:       StatusQueued,
		Quantity:     1,
		PayloadCode:  payload,
		SourceNode:   source,
		DeliveryNode: delivery,
		ProcessNode:  processNode,
		StepsJSON:    string(stepsJSON),
	}
	testutil.MustNoErr(t, db.CreateOrder(o), "create order "+uuid)
	return o
}

// TestWiden_AprilLegacyDryNodeParks is the disposition change itself, on the
// S2 fixture shape: ProcessNode="", pickup at a real-but-dry node. Before
// C(ii) this shape rode through to a terminal no-source fail; now it parks
// with the scoped waiting_for_material reason and stays queued for the
// scanner to retry.
func TestWiden_AprilLegacyDryNodeParks(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	widenNode(t, db, "WPARK-SRC", false)
	widenNode(t, db, "WPARK-LINE", false)

	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: "WPARK-SRC"},
		{Action: protocol.ActionDropoff, Node: "WPARK-LINE"},
		{Action: protocol.ActionWait, Node: "WPARK-LINE"},
	}
	order := mkWidenOrder(t, db, "widen-park", "PART-WP", "WPARK-SRC", "WPARK-LINE", "", steps)

	if err := d.DispatchPreparedComplex(order); err == nil {
		t.Fatal("DispatchPreparedComplex returned nil for a dry supply node — expected the park error (nil would let the scanner clear the queue reason)")
	}

	got, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "re-read order")
	if got.Status != StatusQueued {
		t.Fatalf("status = %q, want queued — a dry supply pool must PARK, never fail terminal", got.Status)
	}
	if got.QueueCode != string(protocol.QueueWaitingForMaterial) {
		t.Errorf("QueueCode = %q, want %q", got.QueueCode, protocol.QueueWaitingForMaterial)
	}
	if got.QueueCause != "finder-node-empty" {
		t.Errorf("QueueCause = %q, want finder-node-empty (the node-local scope tag — plant-wide tiers must not have run)", got.QueueCause)
	}
}

// TestWiden_RewriteReanchorsToGroupNode: a step whose Group carries the anchor
// (the persisted stamp from a prior rewrite) re-derives against the anchor's
// pool each tick. Material at the anchor, none at the currently-named node →
// the step rewrites back and the change PERSISTS (steps_json + source_node),
// which is the §8.2 property: the stamp write must trip the persist trigger.
func TestWiden_RewriteReanchorsToGroupNode(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	widenNode(t, db, "WID-A", false)
	widenNode(t, db, "WID-B", false)
	widenNode(t, db, "WID-LINE", false)
	wa, _ := db.GetNodeByDotName("WID-A")
	testdb.CreateBinAtNode(t, db, "PART-W", wa.ID, "WIDBIN")

	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: "WID-B", Group: "WID-A"},
		{Action: protocol.ActionDropoff, Node: "WID-LINE"},
	}
	order := mkWidenOrder(t, db, "widen-rewrite", "PART-W", "WID-B", "WID-LINE", "", steps)

	if err := d.DispatchPreparedComplex(order); err != nil {
		t.Fatalf("DispatchPreparedComplex: %v", err)
	}

	got, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "re-read order")
	if got.Status == StatusFailed {
		t.Fatalf("order failed: %q", got.Status)
	}
	var persisted []resolvedStep
	testutil.MustNoErr(t, json.Unmarshal([]byte(got.StepsJSON), &persisted), "decode persisted steps")
	if persisted[0].Node != "WID-A" {
		t.Errorf("persisted pickup node = %q, want WID-A (rewritten to the anchor's pool hit)", persisted[0].Node)
	}
	if persisted[0].Group != "WID-A" {
		t.Errorf("persisted pickup group = %q, want WID-A (the anchor stamp must survive for next-tick re-derivation)", persisted[0].Group)
	}
	if got.SourceNode != "WID-A" {
		t.Errorf("source_node = %q, want WID-A (endpoint re-extract persists the widened choice)", got.SourceNode)
	}
}

// TestWidenSupplyPickups_ScopeMatrix pins every skip edge of the widening
// loop directly: dropoffs, empty legs, removal pickups (the evac split), and
// synthetic/unknown anchors are never consulted and never produce a hold —
// those paths keep today's downstream reserve/moot behavior byte-unchanged.
// The April contrast case (same step shape, ProcessNode="") IS widened.
func TestWidenSupplyPickups_ScopeMatrix(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	widenNode(t, db, "WEV-LINE", false) // dry on purpose
	widenNode(t, db, "WEV-AWAY", false)
	widenNode(t, db, "WEV-SYN", true)

	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: "WEV-LINE"},              // removal: pickup at the order's own line
		{Action: protocol.ActionDropoff, Node: "WEV-AWAY"},             // not a pickup
		{Action: protocol.ActionPickup, Node: "WEV-AWAY", Empty: true}, // empty leg
		{Action: protocol.ActionPickup, Node: "WEV-SYN"},               // synthetic anchor
		{Action: protocol.ActionPickup, Node: "WEV-GONE"},              // unknown anchor
	}
	evacOrder := mkWidenOrder(t, db, "widen-scope-evac", "PART-EV", "WEV-LINE", "WEV-AWAY", "WEV-LINE", steps)

	widened, changed, hold := d.widenSupplyPickups(evacOrder, steps)
	if hold != nil {
		t.Fatalf("hold = %+v, want nil — no step in the evac/skip matrix may consult the finder", hold)
	}
	if changed {
		t.Fatal("changed = true, want false — the skip matrix must leave steps byte-identical")
	}
	for i := range steps {
		if widened[i] != steps[i] {
			t.Errorf("step %d mutated: %+v -> %+v", i, steps[i], widened[i])
		}
	}

	// April contrast: the SAME dry-line pickup with ProcessNode="" is no
	// longer a removal (never-removal degradation, intended for the legacy
	// supply-shaped population) — it widens, finds nothing, and holds Wait.
	aprilOrder := mkWidenOrder(t, db, "widen-scope-april", "PART-EV", "WEV-LINE", "WEV-AWAY", "",
		[]resolvedStep{{Action: protocol.ActionPickup, Node: "WEV-LINE"}})
	_, _, aprilHold := d.widenSupplyPickups(aprilOrder, []resolvedStep{{Action: protocol.ActionPickup, Node: "WEV-LINE"}})
	if aprilHold == nil {
		t.Fatal("April-legacy dry pickup produced no hold — the never-removal degradation is the intended widening entry for that population")
	}
	if MapFinderOutcome(*aprilHold) != OutcomeWait {
		t.Fatalf("April-legacy hold outcome = %v, want Wait", aprilHold.Outcome)
	}
}

// TestWidenSupplyPickups_StopsAtWait pins the pre-wait scope. The classic
// single-order swap (pickup A → dropoff LINE → wait → pickup LINE → dropoff A)
// has a post-wait pickup at a line that is EMPTY at dispatch time — its bin
// arrives only when this order's own pre-wait blocks deliver. Post-wait steps
// execute after an Edge release against that future world state, so widening
// must never judge them against current pools (caught live by
// TestSimulator_StagedComplexOrder parking this exact shape).
func TestWidenSupplyPickups_StopsAtWait(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	widenNode(t, db, "WWAIT-SRC", false)
	widenNode(t, db, "WWAIT-LINE", false) // dry: its bin arrives via this order
	ws, _ := db.GetNodeByDotName("WWAIT-SRC")
	testdb.CreateBinAtNode(t, db, "PART-WW", ws.ID, "WWAITBIN")

	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: "WWAIT-SRC"},
		{Action: protocol.ActionDropoff, Node: "WWAIT-LINE"},
		{Action: protocol.ActionWait, Node: "WWAIT-LINE"},
		{Action: protocol.ActionPickup, Node: "WWAIT-LINE"},
		{Action: protocol.ActionDropoff, Node: "WWAIT-SRC"},
	}
	// ProcessNode deliberately blank — the shape must be safe even where
	// isRemovalPickup degrades to never-removal.
	order := mkWidenOrder(t, db, "widen-wait", "PART-WW", "WWAIT-SRC", "WWAIT-LINE", "", steps)

	widened, changed, hold := d.widenSupplyPickups(order, steps)
	if hold != nil {
		t.Fatalf("hold = %+v, want nil — the post-wait line pickup was judged against the current (empty) pool", hold)
	}
	if changed {
		t.Fatal("changed = true, want false — pre-wait pickup already sits at its pool hit")
	}
	if widened[3] != steps[3] {
		t.Errorf("post-wait pickup mutated: %+v", widened[3])
	}
}

// TestWidenSupplyPickups_OwnHoldSkipsFinder pins the owner-aware guard. A
// `sourcing` order holds reservations across scanner retries BY DESIGN, and
// the finder is owner-blind (HasPendingReservation includes the order's own
// hold) — without the guard, the order would park on the exact bin it holds
// and never exit. A hold by ANOTHER order must still park (the material is
// genuinely spoken for).
func TestWidenSupplyPickups_OwnHoldSkipsFinder(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	widenNode(t, db, "WHOLD-A", false)
	widenNode(t, db, "WHOLD-LINE", false)
	wh, _ := db.GetNodeByDotName("WHOLD-A")
	bin := testdb.CreateBinAtNode(t, db, "PART-H", wh.ID, "WHOLDBIN")

	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: "WHOLD-A"},
		{Action: protocol.ActionDropoff, Node: "WHOLD-LINE"},
	}
	owner := mkWidenOrder(t, db, "widen-hold-owner", "PART-H", "WHOLD-A", "WHOLD-LINE", "", steps)
	testutil.MustNoErr(t, reservations.Acquire(db.DB, owner.ID, bin.ID, "test"), "acquire own hold")

	widened, changed, hold := d.widenSupplyPickups(owner, steps)
	if hold != nil {
		t.Fatalf("hold = %+v, want nil — the order self-parked on its own reservation (THE landmine)", hold)
	}
	if changed {
		t.Fatal("changed = true, want false — a held need is the reconcile's property")
	}
	if widened[0] != steps[0] {
		t.Errorf("held step mutated: %+v", widened[0])
	}

	// Same node, different order: the hold belongs to someone else, the pool
	// is effectively dry for this order → park (Wait), do not steal.
	other := mkWidenOrder(t, db, "widen-hold-other", "PART-H", "WHOLD-A", "WHOLD-LINE", "", steps)
	_, _, otherHold := d.widenSupplyPickups(other, steps)
	if otherHold == nil {
		t.Fatal("other order got no hold — a foreign reservation must read as unavailable")
	}
	if MapFinderOutcome(*otherHold) != OutcomeWait {
		t.Fatalf("other order hold outcome = %v, want Wait", otherHold.Outcome)
	}
}
