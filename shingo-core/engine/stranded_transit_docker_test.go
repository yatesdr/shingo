//go:build docker

package engine

import (
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingo/protocol/testutil"
	"shingocore/fleet"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// A bin the robot picked up sits at _TRANSIT with claimed_by set. Cancel the
// order and the claim is released without touching node_id, so the bin is at
// _TRANSIT, unclaimed, and nobody knows where it is. These pin what the
// inference does about that.

// seedStranded creates a bin parked at _TRANSIT and an order that owned it,
// i.e. the state terminalisation leaves behind.
func seedStranded(t *testing.T, db *store.DB, robotID string) (*bins.Bin, *orders.Order) {
	t.Helper()
	var transitID int64
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT id FROM nodes WHERE name='_TRANSIT'`).Scan(&transitID),
		"lookup _TRANSIT")

	bt := &bins.BinType{Code: "ST-" + robotID, Description: "tote"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "create bin type")

	bin := &bins.Bin{BinTypeID: bt.ID, Label: "stranded-" + robotID, NodeID: &transitID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(bin), "create bin")

	// TERMINAL, because that is the state the inference actually runs in: the
	// hook fires from the terminal event handler, after TerminalizeOrderWithReason
	// has flipped the status and released the claim. A fixture that left the
	// order live would be testing a world that cannot happen — and would trip
	// BinService.Move's orphaned-order guard, which is right to refuse it.
	ord := &orders.Order{
		EdgeUUID: "stranded-" + robotID, StationID: "edge.test", OrderType: "retrieve",
		Status: protocol.StatusCancelled, Quantity: 1, DeliveryNode: "DELV.1",
		RobotID: robotID, BinID: &bin.ID,
	}
	testutil.MustNoErr(t, db.CreateOrder(ord), "create order")
	_, err := db.DB.Exec(`UPDATE orders SET robot_id=$1, bin_id=$2, status='cancelled' WHERE id=$3`,
		robotID, bin.ID, ord.ID)
	testutil.MustNoErr(t, err, "set robot, bin and terminal status on order")
	// The TERMINAL HISTORY ROW, because the sweep dates the bin from it. Written
	// at "now" so the default fixture is a freshly stranded bin — the case the
	// inference is for. strandOrderAt backdates it for the cases it is not.
	strandOrderAt(t, db, ord, clock.Now().UTC())
	// AND THE PICKUP ROW. A bin at _TRANSIT got there by being picked up, so an
	// order without an `in_transit` row is not a state the plant can produce —
	// and branch A now measures from it (Engine.pickupWithin), failing closed
	// when it is absent. A fixture missing it would make every branch-A test
	// pass or fail for the wrong reason.
	pickupOrderAt(t, db, ord, clock.Now().UTC())
	return bin, ord
}

// strandOrderAt sets when the order terminalised, by writing (or moving) its
// terminal history row. The sweep reads that row and nothing else — see
// Engine.terminalWithin for why not orders.updated_at.
func strandOrderAt(t *testing.T, db *store.DB, ord *orders.Order, at time.Time) {
	t.Helper()
	_, err := db.DB.Exec(`DELETE FROM order_history WHERE order_id=$1 AND status=$2`,
		ord.ID, string(ord.Status))
	testutil.MustNoErr(t, err, "clear terminal history")
	_, err = db.DB.Exec(
		`INSERT INTO order_history (order_id, status, detail, created_at) VALUES ($1,$2,$3,$4)`,
		ord.ID, string(ord.Status), "test terminalisation", at)
	testutil.MustNoErr(t, err, "write terminal history")
}

// pickupOrderAt sets when the bin left its source, by writing (or moving) the
// order's `in_transit` history row. Branch A reads that row and nothing else —
// see Engine.pickupWithin for why the terminal row cannot bound the same thing.
func pickupOrderAt(t *testing.T, db *store.DB, ord *orders.Order, at time.Time) {
	t.Helper()
	_, err := db.DB.Exec(`DELETE FROM order_history WHERE order_id=$1 AND status=$2`,
		ord.ID, string(protocol.StatusInTransit))
	testutil.MustNoErr(t, err, "clear pickup history")
	_, err = db.DB.Exec(
		`INSERT INTO order_history (order_id, status, detail, created_at) VALUES ($1,$2,$3,$4)`,
		ord.ID, string(protocol.StatusInTransit), "test pickup", at)
	testutil.MustNoErr(t, err, "write pickup history")
}

// cacheRobot writes a robot into the engine's cache, which is what the
// inference reads. Same-package test, so it takes the lock rather than
// reaching for a setter that exists only for tests.
func cacheRobot(e *Engine, r fleet.RobotStatus) {
	e.robotsMu.Lock()
	defer e.robotsMu.Unlock()
	e.robotsCache[r.VehicleID] = r
}

func binNodeName(t *testing.T, db *store.DB, binID int64) string {
	t.Helper()
	b, err := db.GetBin(binID)
	testutil.MustNoErr(t, err, "get bin")
	return b.NodeName
}

// Branch A: the robot is parked with an empty deck at a station that resolves
// to a node. That is where it put the bin, and the operator has nothing to do.
//
// IT PASSES VIA THE SIMULATOR, AND PROVES NOTHING ABOUT THE PLANT. The fixture
// publishes a Core node name as the robot's station, which is what
// fleet/simulator does (parity.go) and what the resolver's IDENTITY path reads.
// No live Springfield `robot_station` value has ever been a node name — nine
// distinct values on 2026-08-24, all map furniture — so this test was green
// through every month the plant's branch A resolved nothing at all. The plant's
// case is the SCENE ALIAS, pinned in stranded_placement_docker_test.go.
func TestStrandedTransit_BranchA_PlacesTheBinAtTheRobotsStation(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newUnstartedEngine(t, db, simulator.New())

	dest := &nodes.Node{Name: "DROP-A", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dest), "create dest node")

	bin, ord := seedStranded(t, db, "AMR-A")
	cacheRobot(eng, fleet.RobotStatus{
		VehicleID: "AMR-A", Connected: true, JackState: 3, LiftHeight: -0.0001,
		CurrentStation: "DROP-A", LastStation: "DROP-A", X: 12.5, Y: 3.25,
	})

	eng.inferStrandedTransitBin(ord.ID)

	if got := binNodeName(t, db, bin.ID); got != "DROP-A" {
		t.Errorf("bin is at %q, want DROP-A — an empty deck at a known station is a placement", got)
	}
	// The audit says a machine did this, not an operator.
	var actor, action string
	testutil.MustNoErr(t, db.DB.QueryRow(
		`SELECT actor, action FROM recovery_actions WHERE target_type='bin' AND target_id=$1
		 ORDER BY id DESC LIMIT 1`, bin.ID).Scan(&actor, &action),
		"read recovery action")
	if actor != "system:inferred" {
		t.Errorf("recovery actor = %q, want system:inferred — an inferred placement must be readable as one", actor)
	}
	if action != "transit_anomaly_recover" {
		t.Errorf("recovery action = %q", action)
	}
}

// The empty-node guard is NOT bypassed. A robot parked at a node something else
// already occupies means the inference is stale, and forcing it would put two
// bins in one slot.
func TestStrandedTransit_OccupiedNodeFallsToAnomaly(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newUnstartedEngine(t, db, simulator.New())

	dest := &nodes.Node{Name: "DROP-FULL", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dest), "create dest node")

	bin, ord := seedStranded(t, db, "AMR-C")

	// Something is already there.
	resident := &bins.Bin{BinTypeID: bin.BinTypeID, Label: "resident", NodeID: &dest.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(resident), "create resident bin")
	cacheRobot(eng, fleet.RobotStatus{
		VehicleID: "AMR-C", Connected: true, JackState: 3, LiftHeight: -0.0001,
		CurrentStation: "DROP-FULL", LastStation: "DROP-FULL",
	})

	eng.inferStrandedTransitBin(ord.ID)

	if got := binNodeName(t, db, bin.ID); got != "_TRANSIT" {
		t.Errorf("bin was forced into an occupied node (%q) — the guard must hold", got)
	}
	b, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "get bin")
	if !strings.Contains(b.AnomalyNote, "DROP-FULL") {
		t.Errorf("the note must name the node it could not use: %q", b.AnomalyNote)
	}
}

// A deck mid-travel is not an answer. Neither placing the bin nor leaving it on
// the robot is true, so the inference declines rather than guessing.
func TestStrandedTransit_MovingDeckIsNotAnAnswer(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newUnstartedEngine(t, db, simulator.New())

	dest := &nodes.Node{Name: "DROP-MID", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dest), "create dest node")

	bin, ord := seedStranded(t, db, "AMR-D")
	cacheRobot(eng, fleet.RobotStatus{
		VehicleID: "AMR-D", Connected: true, JackState: 2, LiftHeight: 0.03,
		CurrentStation: "DROP-MID", LastStation: "DROP-MID",
	})

	eng.inferStrandedTransitBin(ord.ID)

	if got := binNodeName(t, db, bin.ID); got != "_TRANSIT" {
		t.Errorf("a deck mid-unload must not produce a placement, bin went to %q", got)
	}
	b, _ := db.GetBin(bin.ID)
	if !strings.Contains(b.AnomalyNote, "not at rest") {
		t.Errorf("the note must say why it declined: %q", b.AnomalyNote)
	}
}

// A bin that reached its destination is not this function's business.
func TestStrandedTransit_IgnoresABinThatIsNotStranded(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newUnstartedEngine(t, db, simulator.New())

	dest := &nodes.Node{Name: "DROP-DONE", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dest), "create dest node")

	bin, ord := seedStranded(t, db, "AMR-E")
	// The bin actually arrived.
	_, err := db.DB.Exec(`UPDATE bins SET node_id=$1 WHERE id=$2`, dest.ID, bin.ID)
	testutil.MustNoErr(t, err, "move bin to destination")

	cacheRobot(eng, fleet.RobotStatus{VehicleID: "AMR-E", Connected: true, JackState: 3})
	eng.inferStrandedTransitBin(ord.ID)

	b, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "get bin")
	if b.AnomalyAt != nil {
		t.Error("a bin that reached its destination must not be stamped anomalous")
	}
}

// ── Branch B: the bin is still on the deck ──────────────────────────────────

// A loaded deck means the bin is not lost and not at a station — it is on that
// robot. It lives on the robot's own synthetic node until the deck reports
// empty, so nothing can source from it and nothing reports it missing.
func TestStrandedTransit_BranchB_LoadedDeckParksTheBinOnTheRobot(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newUnstartedEngine(t, db, simulator.New())

	bin, ord := seedStranded(t, db, "AMR-03")
	cacheRobot(eng, fleet.RobotStatus{
		VehicleID: "AMR-03", Connected: true, JackState: 1, JackIsFull: true, IsLoaded: true,
		LiftHeight: 0.0601, LastStation: "SMN_024",
	})

	eng.inferStrandedTransitBin(ord.ID)

	if got := binNodeName(t, db, bin.ID); got != "_ROBOT:AMR-03" {
		t.Fatalf("bin is at %q, want _ROBOT:AMR-03", got)
	}
	// Synthetic, so every finder in Core already excludes it: a bin on a deck
	// must not be re-claimed or sourced from.
	node, err := db.GetNodeByName("_ROBOT:AMR-03")
	testutil.MustNoErr(t, err, "get carrier node")
	if !node.IsSynthetic {
		t.Error("the carrier node must be synthetic — otherwise the finders can source from a robot's deck")
	}
	// NOT an anomaly. _TRANSIT + no claim is the anomaly definition, and a bin
	// whose location we know perfectly well is not lost.
	b, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "get bin")
	if b.AnomalyAt != nil {
		t.Error("a bin on a known robot's deck must not be reported as an anomaly")
	}
	var actor string
	testutil.MustNoErr(t, db.DB.QueryRow(
		`SELECT actor FROM recovery_actions WHERE target_type='bin' AND target_id=$1
		 AND action='transit_bin_on_robot' ORDER BY id DESC LIMIT 1`, bin.ID).Scan(&actor),
		"read carrier audit row")
	if actor != "system:inferred" {
		t.Errorf("carrier audit actor = %q", actor)
	}
}

// The watch: once the deck reports empty at a station we can name, the bin is
// placed there.
func TestStrandedTransit_CarriedBinIsPlacedWhenTheJackUnloads(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newUnstartedEngine(t, db, simulator.New())

	dest := &nodes.Node{Name: "DROP-B", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dest), "create dest node")

	bin, ord := seedStranded(t, db, "AMR-13")
	cacheRobot(eng, fleet.RobotStatus{
		VehicleID: "AMR-13", Connected: true, JackState: 1, JackIsFull: true, IsLoaded: true, LiftHeight: 0.0601,
	})
	eng.inferStrandedTransitBin(ord.ID)
	if got := binNodeName(t, db, bin.ID); got != "_ROBOT:AMR-13" {
		t.Fatalf("setup: bin is at %q, want the carrier node", got)
	}

	// Still loaded: the sweep leaves it alone rather than guessing.
	eng.sweepCarriedBins()
	if got := binNodeName(t, db, bin.ID); got != "_ROBOT:AMR-13" {
		t.Fatalf("a still-loaded deck must not place the bin, went to %q", got)
	}

	// The robot drives to DROP-B and sets the bin down.
	cacheRobot(eng, fleet.RobotStatus{
		VehicleID: "AMR-13", Connected: true, JackState: 3, LiftHeight: -0.0001,
		CurrentStation: "DROP-B", LastStation: "DROP-B",
	})
	eng.sweepCarriedBins()

	if got := binNodeName(t, db, bin.ID); got != "DROP-B" {
		t.Errorf("bin is at %q, want DROP-B — the jack unloaded there", got)
	}
}

// The deck empties somewhere Core cannot name — a charging bay, a waypoint. The
// bin becomes an anomaly carrying the position, which is the operator's map pin.
func TestStrandedTransit_CarriedBinUnloadedNowhereKnownBecomesAnAnomaly(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newUnstartedEngine(t, db, simulator.New())

	bin, ord := seedStranded(t, db, "AMR-14")
	cacheRobot(eng, fleet.RobotStatus{
		VehicleID: "AMR-14", Connected: true, JackState: 1, JackIsFull: true, IsLoaded: true, LiftHeight: 0.0601,
	})
	eng.inferStrandedTransitBin(ord.ID)

	cacheRobot(eng, fleet.RobotStatus{
		VehicleID: "AMR-14", Connected: true, JackState: 3, LiftHeight: -0.0001,
		CurrentStation: "CHARGER-2", LastStation: "CHARGER-2", X: 55.5, Y: 2.5,
	})
	eng.sweepCarriedBins()

	b, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "get bin")
	if b.AnomalyAt == nil {
		t.Error("a bin set down at an unknown point is an anomaly")
	}
	for _, want := range []string{"x=55.50", "CHARGER-2"} {
		if !strings.Contains(b.AnomalyNote, want) {
			t.Errorf("anomaly note %q is missing %q — the operator needs the pin", b.AnomalyNote, want)
		}
	}
}

// ── The sweep: the guarantee, not the fast path ─────────────────────────────

// Two bins already stranded before any of this ran — the backlog the first
// sweep after deploy clears. One resolvable, one not; they must end in
// different places, and neither may be left as a bare "lost".
func TestStrandedSweep_PlacesRecentWork(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newUnstartedEngine(t, db, simulator.New())

	dest := &nodes.Node{Name: "DROP-SWEEP", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dest), "create dest node")

	// Both bins are in the state terminalisation leaves behind: at _TRANSIT
	// with the claim already released. The events that would have triggered the
	// fast path are long gone.
	resolvable, _ := seedStranded(t, db, "AMR-S1")
	lost, _ := seedStranded(t, db, "AMR-S2")

	cacheRobot(eng, fleet.RobotStatus{
		VehicleID: "AMR-S1", Connected: true, JackState: 3, LiftHeight: -0.0001,
		CurrentStation: "DROP-SWEEP", LastStation: "DROP-SWEEP",
	})
	cacheRobot(eng, fleet.RobotStatus{
		VehicleID: "AMR-S2", Connected: true, JackState: 3, LiftHeight: -0.0001,
		CurrentStation: "NOWHERE-WE-KNOW", LastStation: "NOWHERE-WE-KNOW", X: 71.25, Y: 4.5,
	})

	eng.sweepStrandedBins()

	if got := binNodeName(t, db, resolvable.ID); got != "DROP-SWEEP" {
		t.Errorf("resolvable bin is at %q, want DROP-SWEEP — the sweep must place what it can", got)
	}
	if got := binNodeName(t, db, lost.ID); got != "_TRANSIT" {
		t.Errorf("unresolvable bin moved to %q — it must stay put", got)
	}
	b, err := db.GetBin(lost.ID)
	testutil.MustNoErr(t, err, "get lost bin")
	if b.AnomalyAt == nil {
		t.Error("the unresolvable bin must be stamped anomalous")
	}
	if !strings.Contains(b.AnomalyNote, "x=71.25") {
		t.Errorf("the unresolvable bin must carry the robot's position: %q", b.AnomalyNote)
	}
	// And the placement is auditable as a machine's.
	var actor string
	testutil.MustNoErr(t, db.DB.QueryRow(
		`SELECT actor FROM recovery_actions WHERE target_type='bin' AND target_id=$1
		 ORDER BY id DESC LIMIT 1`, resolvable.ID).Scan(&actor), "read recovery action")
	if actor != "system:inferred" {
		t.Errorf("sweep placement actor = %q, want system:inferred", actor)
	}
}

// A bin on a carrier node is not an anomaly and the sweep must not treat it as
// one. ListAnomalies no longer returns the carrier nodes at all, and the sweep
// skips them besides; this pins both.
func TestStrandedSweep_DoesNotTreatACarriedBinAsLost(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newUnstartedEngine(t, db, simulator.New())

	bin, ord := seedStranded(t, db, "AMR-S3")
	cacheRobot(eng, fleet.RobotStatus{
		VehicleID: "AMR-S3", Connected: true, JackState: 1, JackIsFull: true, IsLoaded: true, LiftHeight: 0.0601,
	})
	eng.inferStrandedTransitBin(ord.ID)
	if got := binNodeName(t, db, bin.ID); got != "_ROBOT:AMR-S3" {
		t.Fatalf("setup: bin is at %q", got)
	}

	eng.sweepStrandedBins()

	if got := binNodeName(t, db, bin.ID); got != "_ROBOT:AMR-S3" {
		t.Errorf("the sweep moved a carried bin to %q", got)
	}
	b, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "get bin")
	if b.AnomalyAt != nil {
		t.Error("a bin riding a known robot must not be stamped anomalous by the sweep")
	}
}

// THE SWEEP IS A BACKSTOP, NOT A BACKFILL. A bin whose order ended three hours
// ago is not something the robot's CURRENT position can speak to: the robot has
// run other jobs since, and an empty node under it now has nothing to do with
// where that bin was set down. The sweep must decline rather than place, even
// though every other condition (deck empty, parked, station resolves, node free)
// is satisfied — which is exactly what makes this the dangerous case.
func TestStrandedSweep_DeclinesABinOlderThanTheWindow(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newUnstartedEngine(t, db, simulator.New())

	dest := &nodes.Node{Name: "DROP-STALE", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dest), "create dest node")

	stale, staleOrd := seedStranded(t, db, "AMR-OLD")
	strandOrderAt(t, db, staleOrd, clock.Now().UTC().Add(-3*time.Hour))

	// Everything the inference wants, so only the AGE can be what stops it.
	cacheRobot(eng, fleet.RobotStatus{
		VehicleID: "AMR-OLD", Connected: true, JackState: 3, LiftHeight: -0.0001,
		CurrentStation: "DROP-STALE", LastStation: "DROP-STALE",
	})

	eng.sweepStrandedBins()

	if got := binNodeName(t, db, stale.ID); got != "_TRANSIT" {
		t.Errorf("a bin stranded 3h ago was placed at %q — the sweep must not backfill history, "+
			"because the robot's position now says nothing about where that bin went", got)
	}
}

// The window is for _TRANSIT bins only. A bin riding a deck is dated by the
// JACK, not by an order that ended long ago: however stale the order, a deck
// that reports empty has set its bin down and that is a fact about now.
func TestStrandedSweep_CarrierBinIsRecheckedDespiteAStaleOrder(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newUnstartedEngine(t, db, simulator.New())

	dest := &nodes.Node{Name: "DROP-CARRIED", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dest), "create dest node")

	bin, ord := seedStranded(t, db, "AMR-RIDE")
	// Park it on the deck first, while the order is still fresh.
	cacheRobot(eng, fleet.RobotStatus{
		VehicleID: "AMR-RIDE", Connected: true, JackState: 1, JackIsFull: true, IsLoaded: true, LiftHeight: 0.0601,
	})
	eng.inferStrandedTransitBin(ord.ID)
	if got := binNodeName(t, db, bin.ID); got != "_ROBOT:AMR-RIDE" {
		t.Fatalf("setup: bin is at %q, want the carrier node", got)
	}

	// Now age the order well past the window and unload the deck.
	strandOrderAt(t, db, ord, clock.Now().UTC().Add(-6*time.Hour))
	cacheRobot(eng, fleet.RobotStatus{
		VehicleID: "AMR-RIDE", Connected: true, JackState: 3, LiftHeight: -0.0001,
		CurrentStation: "DROP-CARRIED", LastStation: "DROP-CARRIED",
	})

	eng.sweepStrandedBins()

	if got := binNodeName(t, db, bin.ID); got != "DROP-CARRIED" {
		t.Errorf("carried bin is at %q, want DROP-CARRIED — the age window is for _TRANSIT bins, "+
			"not for a deck that just reported empty", got)
	}
	// And the carrier node is retired once nothing is on it.
	var n int
	testutil.MustNoErr(t, db.DB.QueryRow(
		`SELECT count(*) FROM nodes WHERE name='_ROBOT:AMR-RIDE'`).Scan(&n), "count carrier node")
	if n != 0 {
		t.Errorf("carrier node survived with no bins on it — it would accumulate one row per vehicle")
	}
}

// A robot that is DRIVING resolves to a station it merely passed, because
// CurrentStation is empty while it moves and ResolveRobotStation falls back to
// LastStation. The jack being at rest says nothing about the vehicle under it.
func TestStrandedTransit_MovingRobotIsNotAPlacement(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newUnstartedEngine(t, db, simulator.New())

	dest := &nodes.Node{Name: "DROP-PASSED", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dest), "create dest node")

	bin, ord := seedStranded(t, db, "AMR-MOVING")
	cacheRobot(eng, fleet.RobotStatus{
		VehicleID: "AMR-MOVING", Connected: true, JackState: 3, LiftHeight: -0.0001,
		Busy:        true, // under way
		LastStation: "DROP-PASSED", X: 44.5, Y: 9.75,
	})

	eng.inferStrandedTransitBin(ord.ID)

	if got := binNodeName(t, db, bin.ID); got != "_TRANSIT" {
		t.Errorf("bin was placed at %q from a MOVING robot's last station — "+
			"that is a node it drove past, not one it stopped at", got)
	}
	b, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "get bin")
	if !strings.Contains(b.AnomalyNote, "under way") {
		t.Errorf("the note must say why it declined: %q", b.AnomalyNote)
	}
}
