//go:build docker

package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/plantclaims"
)

// A recovery that lands a bin at a line tells that line, and only that line: the
// Edge that models the node binds the bin when order.delivered names it. The
// broadcast to station "" that used to serve this told every Edge; a recovery to
// storage tells no one (B3 close-out, stop 5). The destination is main's one
// chooser (chooseDeclared) since rebase 4; these pins drive it through a claim's
// declared source group.

// recoveryDelivered drives a recovery order's delivery through the engine's
// delivered handler and returns the order.delivered notices it queued, by
// station.
func recoveryDelivered(t *testing.T, eng *Engine, db *store.DB, orderID int64) map[string]int {
	t.Helper()
	var mark int
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM outbox`).Scan(&mark), "outbox mark")
	// The row is delivered before the handler runs, as the fleet's status path
	// leaves it.
	_, err := db.DB.Exec(`UPDATE orders SET status = $1 WHERE id = $2`, protocol.StatusDelivered, orderID)
	testutil.MustNoErr(t, err, "mark delivered")
	o, err := db.GetOrder(orderID)
	testutil.MustNoErr(t, err, "reload recovery")
	eng.handleOrderDelivered(o)
	rows, err := db.DB.Query(`SELECT COALESCE(station_id, '') FROM outbox WHERE id > $1 AND msg_type = $2`,
		mark, protocol.TypeOrderDelivered)
	testutil.MustNoErr(t, err, "read notices")
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var st string
		testutil.MustNoErr(t, rows.Scan(&st), "scan")
		out[st]++
	}
	return out
}

// declaredChildFor is a recovery's destination as main's chooser finds it
// (chooseDeclared): a node group a consume claim names as its line's inbound
// source, with one free child, which is a storage slot (STOR) when stor is set
// and a plain position otherwise. The cancelled order carrying the bin is the
// claim's line's.
func declaredChildFor(t *testing.T, eng *Engine, db *store.DB, prefix string, stor bool) (*nodes.Node, int64) {
	t.Helper()
	grp, child := storeGroupWithSlot(t, db, prefix)
	if !stor {
		_, err := db.DB.Exec(`UPDATE nodes SET node_type_id = NULL WHERE id = $1`, child.ID)
		testutil.MustNoErr(t, err, "make the child a plain position")
	}
	seedClaim(t, db, "PROC-"+prefix, "STY", "LINE-"+prefix, prefix+"-P", grp.Name, true)
	bin, _ := seedCancelledCarry(t, db, "AMR-"+prefix, prefix+"-P", "LINE-"+prefix)
	cacheRobot(eng, dispatchableRobot("AMR-"+prefix))
	return child, bin.ID
}

// recoverInto runs the Return button on the bin and checks the chooser took it
// to want.
func recoverInto(t *testing.T, eng *Engine, db *store.DB, binID int64, want *nodes.Node) (projectsTo string, notices map[string]int) {
	t.Helper()
	order, _, err := eng.RecoverCarriedBin(binID, "operator:test")
	testutil.MustNoErr(t, err, "recover carried bin")
	o, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "reload order")
	if o.DeliveryNode != want.Name {
		t.Fatalf("fixture: recovery delivers to %q, want the declared group's child %q", o.DeliveryNode, want.Name)
	}
	if !o.StationUnaware {
		t.Errorf("recovery row station_unaware=false, want true: no Edge asked for it")
	}
	return o.ProjectsTo, recoveryDelivered(t, eng, db, order.ID)
}

// TestB3D3_RecoveryToALineTellsThatLine: a recovery whose bin lands on a
// position (not a storage slot), on a plant with one Edge, projects to that
// Edge; its delivery sends one order.delivered, to it.
func TestB3D3_RecoveryToALineTellsThatLine(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())
	registerActiveEdge(t, db, "edge.test")
	child, binID := declaredChildFor(t, eng, db, "B3RL", false)
	projects, got := recoverInto(t, eng, db, binID, child)
	if projects != "edge.test" {
		t.Errorf("recovery projects_to = %q, want edge.test (the line's station hears its material notices)", projects)
	}
	if len(got) != 1 || got["edge.test"] != 1 {
		t.Errorf("order.delivered notices by station = %v, want exactly one, to edge.test", got)
	}
}

// TestB3D3_RecoveryToStorageTellsNoOne: a recovery into a storage slot (STOR)
// projects nowhere, though the plant has one Edge and the cancelled order was
// a station's: no Edge models a storage slot, so its delivery sends nothing.
func TestB3D3_RecoveryToStorageTellsNoOne(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())
	registerActiveEdge(t, db, "edge.test")
	child, binID := declaredChildFor(t, eng, db, "B3RS", true)
	projects, got := recoverInto(t, eng, db, binID, child)
	if projects != "" || len(got) != 0 {
		t.Errorf("recovery projects_to = %q, notices = %v; want none", projects, got)
	}
}

// twoEdgePark registers two Edges and drives a recovery onto a position no
// order named, returning the recovery's delivered notices by station.
// node_stations assigns the node to edge.a; claim, when set, makes edge.b's
// process claim it in the keeper's mirror.
func twoEdgePark(t *testing.T, prefix string, claim bool) (projectsTo string, notices map[string]int) {
	t.Helper()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())
	registerActiveEdge(t, db, "edge.a")
	registerActiveEdge(t, db, "edge.b")
	child, binID := declaredChildFor(t, eng, db, prefix, false)
	testutil.MustNoErr(t, db.AssignNodeToStation(child.ID, "edge.a"), "node_stations")
	if claim {
		_, err := db.ReplacePlantClaimsFromStation("PROC-B-"+prefix, "edge.b",
			[]plantclaims.StyleRow{{ProcessID: "PROC-B-" + prefix, StyleID: "S1", ConfigGen: 1}},
			[]plantclaims.ClaimRow{{ProcessID: "PROC-B-" + prefix, StyleID: "S1", CoreNodeName: child.Name,
				Role: protocol.ClaimRoleConsume}}, 0)
		testutil.MustNoErr(t, err, "claims mirror")
	}
	return recoverInto(t, eng, db, binID, child)
}

// TestB3D3_RecoveryNoOrderNamed_TellsTheStationThatClaimsIt: with two Edges, a
// position no order named asks the claims mirror, and node_stations (an admin
// allow-list, here naming the other station) is not asked.
func TestB3D3_RecoveryNoOrderNamed_TellsTheStationThatClaimsIt(t *testing.T) {
	t.Parallel()
	projects, got := twoEdgePark(t, "B3RC", true)
	if projects != "edge.b" {
		t.Errorf("recovery projects_to = %q, want edge.b (the station whose process claims the node)", projects)
	}
	if len(got) != 1 || got["edge.b"] != 1 {
		t.Errorf("order.delivered notices by station = %v, want exactly one, to edge.b", got)
	}
}

// TestB3D3_RecoveryNoOrderNamed_NoClaim_TellsNoOne: with two Edges and no claim
// on the node, nobody is told; a guess would bind a bin at an Edge that does not
// hold it.
func TestB3D3_RecoveryNoOrderNamed_NoClaim_TellsNoOne(t *testing.T) {
	t.Parallel()
	projects, got := twoEdgePark(t, "B3RN", false)
	if projects != "" || len(got) != 0 {
		t.Errorf("recovery projects_to = %q, notices = %v; want none", projects, got)
	}
}
