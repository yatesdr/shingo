//go:build docker

package www

import (
	"net/http"
	"strings"
	"testing"

	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
)

// TestBinMove_RefusesMovingABinToWhereItAlreadyIs pins that asking to move a bin
// to the node it is already sitting on is refused, and says so.
//
// The engineer's door has always refused this. The operator's door never checked.
// The plant has five other places that refuse it, and the wire protocol reserves
// a terminal code for it, so the judgement is settled everywhere except here.
//
// The occupancy gate does catch most of it incidentally — the bin is at the
// destination, so the destination counts as occupied — but the message it gives
// is "that spot is taken", which sends someone to go clear a node that does not
// need clearing. The specific check runs first so the specific message wins.
func TestBinMove_RefusesMovingABinToWhereItAlreadyIs(t *testing.T) {
	t.Parallel()
	sim := simulator.New()
	h, db := testHandlersWithSim(t, sim)
	sd := testdb.SetupStandardData(t, db)

	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-SAMENODE")

	resp, status := submitRetrieveSpecificTo(t, h, bin.Label, sd.StorageNode.Name)

	if status == http.StatusOK {
		t.Fatalf("moving a bin to its own node returned 200 — a robot would be dispatched to carry a bin nowhere. body=%+v", resp)
	}
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want %d: nothing about the plant is in the way, the request itself is the problem", status, http.StatusBadRequest)
	}
	if !strings.Contains(resp.Error, bin.Label) || !strings.Contains(resp.Error, sd.StorageNode.Name) {
		t.Errorf("message %q should name the bin and the node so the operator can see what they asked for", resp.Error)
	}
	if strings.Contains(strings.ToLower(resp.Error), "occupied") {
		t.Errorf("message %q is the occupancy gate's — it tells someone to clear a node whose only occupant is the bin they were moving", resp.Error)
	}
}

// TestBinMove_RefusesSameNodeEvenInsideALane is the hole the incidental block
// leaves open.
//
// The occupancy gate defers on synthetic lane nodes — lane depth is the
// planners' business, so it reports "not blocked" and moves on. A bin sitting in
// a lane therefore passes the gate, and before this check the request went
// straight to the fleet with no guard at all. This is the case that made the
// explicit check necessary rather than merely tidier.
func TestBinMove_RefusesSameNodeEvenInsideALane(t *testing.T) {
	t.Parallel()
	sim := simulator.New()
	h, db := testHandlersWithSim(t, sim)
	sd := testdb.SetupStandardData(t, db)

	laneType, err := db.GetNodeTypeByCode("LANE")
	if err != nil {
		t.Fatalf("get LANE node type: %v", err)
	}
	lane := &nodes.Node{Name: "SAMENODE-LANE", IsSynthetic: true, NodeTypeID: &laneType.ID, Enabled: true}
	if err := db.CreateNode(lane); err != nil {
		t.Fatalf("create lane: %v", err)
	}
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, lane.ID, "BIN-SAMENODE-LANE")

	resp, status := submitRetrieveSpecificTo(t, h, bin.Label, lane.Name)

	if status == http.StatusOK {
		t.Fatalf("same-node move inside a lane returned 200 — the occupancy gate defers on lanes, so nothing stopped this. body=%+v", resp)
	}
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", status, http.StatusBadRequest)
	}
}
