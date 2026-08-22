//go:build docker

package engine

import (
	"strings"
	"testing"

	"shingo/protocol"
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

	ord := &orders.Order{
		EdgeUUID: "stranded-" + robotID, StationID: "edge.test", OrderType: "retrieve",
		Status: protocol.StatusInTransit, Quantity: 1, DeliveryNode: "DELV.1",
		RobotID: robotID, BinID: &bin.ID,
	}
	testutil.MustNoErr(t, db.CreateOrder(ord), "create order")
	_, err := db.DB.Exec(`UPDATE orders SET robot_id=$1, bin_id=$2 WHERE id=$3`, robotID, bin.ID, ord.ID)
	testutil.MustNoErr(t, err, "set robot and bin on order")
	return bin, ord
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
func TestStrandedTransit_BranchA_PlacesTheBinAtTheRobotsStation(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newUnstartedEngine(t, db, simulator.New())

	dest := &nodes.Node{Name: "DROP-A", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dest), "create dest node")

	bin, ord := seedStranded(t, db, "AMR-A")
	cacheRobot(eng, fleet.RobotStatus{
		VehicleID: "AMR-A", JackState: 3, LiftHeight: -0.0001,
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

// Branch C, the loaded deck. Until the bin has somewhere to live (unit 13) it
// stays at _TRANSIT, but the note says why rather than "lost".
func TestStrandedTransit_LoadedDeckStaysAtTransitAndSaysSo(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newUnstartedEngine(t, db, simulator.New())

	bin, ord := seedStranded(t, db, "AMR-B")
	cacheRobot(eng, fleet.RobotStatus{
		VehicleID: "AMR-B", JackState: 1, JackIsFull: true, IsLoaded: true,
		LiftHeight: 0.0601, CurrentStation: "", LastStation: "SMN_024", X: 40.1, Y: 8.75,
	})

	eng.inferStrandedTransitBin(ord.ID)

	if got := binNodeName(t, db, bin.ID); got != "_TRANSIT" {
		t.Errorf("bin moved to %q — a bin still on the deck is not at a station", got)
	}
	b, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "get bin")
	if b.AnomalyAt == nil {
		t.Error("a stranded bin must be stamped anomalous")
	}
	if b.AnomalyNote == "" {
		t.Fatal("the anomaly must carry where the robot was")
	}
	for _, want := range []string{"deck", "AMR-B", "x=40.10", "last station SMN_024"} {
		if !strings.Contains(b.AnomalyNote, want) {
			t.Errorf("anomaly note %q is missing %q", b.AnomalyNote, want)
		}
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
		VehicleID: "AMR-C", JackState: 3, LiftHeight: -0.0001,
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
		VehicleID: "AMR-D", JackState: 2, LiftHeight: 0.03,
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

	cacheRobot(eng, fleet.RobotStatus{VehicleID: "AMR-E", JackState: 3})
	eng.inferStrandedTransitBin(ord.ID)

	b, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "get bin")
	if b.AnomalyAt != nil {
		t.Error("a bin that reached its destination must not be stamped anomalous")
	}
}
