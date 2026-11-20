//go:build docker

package reconciliation_test

import (
	"strings"
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reconciliation"
)

// TestClaimInsideForeignDugLane_NamesTheNodeInItsDetailNotAsAStation: the
// lane-coordination anomaly (an order's claim inside a lane another order's dig
// holds) names the claimed node in its detail sentence and leaves the station
// blank. It used to put the node's name in the station field, which the
// Diagnostics page renders under "Station"; a node is not a station, and the
// field is station_uid. The rows are written directly: reservations.Acquire
// refuses to create this pair, which is why the anomaly exists only as a count.
// Red if the anomaly fills StationUID again, or stops naming the node.
func TestClaimInsideForeignDugLane_NamesTheNodeInItsDetailNotAsAStation(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	lane := &nodes.Node{Name: "DUG-LANE", Enabled: true}
	if err := nodes.Create(db.DB, lane); err != nil {
		t.Fatalf("create lane: %v", err)
	}
	slot := &nodes.Node{Name: "DUG-LANE-S1", Enabled: true}
	if err := nodes.Create(db.DB, slot); err != nil {
		t.Fatalf("create slot: %v", err)
	}
	if _, err := db.DB.Exec(`UPDATE nodes SET parent_id = $1 WHERE id = $2`, lane.ID, slot.ID); err != nil {
		t.Fatalf("put the slot in the lane: %v", err)
	}
	bin := testdb.CreateBinAtNode(t, db, "DUG-PART", slot.ID, "DUG-BIN")
	holder := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "dug-holder" })
	digger := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "dug-digger" })
	// Both at dispatched: a confirmed hold is a dispatched order's (testdb's
	// hold-at-rest check refuses one on a queued order).
	testdb.SeedDispatched(t, db, holder.ID, "sg-dug-holder")
	testdb.SeedDispatched(t, db, digger.ID, "sg-dug-digger")
	if _, err := db.DB.Exec(`INSERT INTO reservations (order_id, bin_id, state, resource_kind)
		VALUES ($1, $2, 'confirmed', 'bin')`, holder.ID, bin.ID); err != nil {
		t.Fatalf("holder's claim: %v", err)
	}
	if _, err := db.DB.Exec(`INSERT INTO reservations (order_id, node_id, state, resource_kind, mode)
		VALUES ($1, $2, 'confirmed', 'mouth', 'dig')`, digger.ID, lane.ID); err != nil {
		t.Fatalf("digger's lane hold: %v", err)
	}

	anomalies, err := reconciliation.ListAnomalies(db.DB)
	if err != nil {
		t.Fatalf("list anomalies: %v", err)
	}
	var found *reconciliation.Anomaly
	for _, a := range anomalies {
		if a.Issue == "claim_inside_foreign_dug_lane" && a.OrderID != nil && *a.OrderID == holder.ID {
			found = a
		}
	}
	if found == nil {
		t.Fatalf("no claim_inside_foreign_dug_lane anomaly for order %d; got %d anomalies", holder.ID, len(anomalies))
	}
	if found.StationUID != "" {
		t.Errorf("the anomaly's station is %q, want blank: it is the claimed node's name, and a node is not a station", found.StationUID)
	}
	if !strings.Contains(found.Detail, slot.Name) || !strings.Contains(found.Detail, lane.Name) {
		t.Errorf("the anomaly's detail %q should name the node %s and the lane %s", found.Detail, slot.Name, lane.Name)
	}
}
