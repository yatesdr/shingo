//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// TestApplyComplexPlan_SameNodeDoublePickupClaimsDistinctBins guards the
// invariant that two pickup steps at the SAME node each claim a DISTINCT bin.
//
// ApplyComplexPlan re-lists the live candidates at each pickup step and claims
// sequentially, so step 1's claim removes that bin from step 2's candidate set.
// The plan, by contrast, predicts the same first-available bin for both
// same-node steps (BuildComplexPlan builds its claimed-map only after
// selection), so an "optimization" that claimed the plan's prediction directly
// would collide and leave the second pickup binless. The existing end-to-end
// path test uses two DIFFERENT nodes, so nothing else covers the same-node case.
func TestApplyComplexPlan_SameNodeDoublePickupClaimsDistinctBins(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)

	// One source node holding two eligible bins of the same payload; a
	// two-pickup order draws both from it.
	dual := &nodes.Node{Name: "DUAL-SRC", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dual), "create dual-source node")

	dualA := &bins.Bin{BinTypeID: 1, Label: "DUAL-A", NodeID: &dual.ID, Status: "available", PayloadCode: bp.Code}
	testutil.MustNoErr(t, db.CreateBin(dualA), "create first bin at dual source")
	dualB := &bins.Bin{BinTypeID: 1, Label: "DUAL-B", NodeID: &dual.ID, Status: "available", PayloadCode: bp.Code}
	testutil.MustNoErr(t, db.CreateBin(dualB), "create second bin at dual source")

	order := &orders.Order{
		EdgeUUID:     "uuid-same-node-pickup",
		StationID:    "line-1",
		OrderType:    OrderTypeComplex,
		Status:       StatusQueued,
		Quantity:     1,
		SourceNode:   dual.Name,
		ProcessNode:  dual.Name,
		DeliveryNode: lineNode.Name,
		PayloadCode:  bp.Code,
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")

	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	// Two pickup steps at the SAME node, each delivered to the line.
	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: dual.Name},
		{Action: protocol.ActionDropoff, Node: lineNode.Name},
		{Action: protocol.ActionPickup, Node: dual.Name},
		{Action: protocol.ActionDropoff, Node: lineNode.Name},
	}
	plan := BuildComplexPlan(steps, d.snapshotPickupBins(steps), bp.Code, order.ProcessNode)
	if err := d.ApplyComplexPlan(order, plan, bp.Code, nil); err != nil {
		t.Fatalf("ApplyComplexPlan: %v", err)
	}

	// Both pickups claimed, and they claimed different bins — the whole point.
	claimed, err := db.ListBinsByClaim(order.ID)
	testutil.MustNoErr(t, err, "list claimed bins")
	if len(claimed) != 2 {
		t.Fatalf("claimed %d bin(s), want 2 — both same-node pickups must claim", len(claimed))
	}
	claimedIDs := map[int64]bool{}
	for _, b := range claimed {
		claimedIDs[b.ID] = true
	}
	if len(claimedIDs) != 2 {
		t.Fatalf("two pickups claimed the same bin: %v", claimedIDs)
	}
	if !claimedIDs[dualA.ID] || !claimedIDs[dualB.ID] {
		t.Errorf("claimed bins = %v, want the two DUAL bins %d and %d", claimedIDs, dualA.ID, dualB.ID)
	}

	// The junction rows tie each pickup STEP to its own bin: two rows, both at
	// the shared node, distinct step indices, distinct bins.
	obs, err := db.ListOrderBins(order.ID)
	testutil.MustNoErr(t, err, "list order_bins")
	if len(obs) != 2 {
		t.Fatalf("order_bins rows = %d, want 2", len(obs))
	}
	stepsSeen := map[int]bool{}
	binsSeen := map[int64]bool{}
	for _, ob := range obs {
		if ob.NodeName != dual.Name {
			t.Errorf("order_bin node = %q, want %q", ob.NodeName, dual.Name)
		}
		stepsSeen[ob.StepIndex] = true
		binsSeen[ob.BinID] = true
	}
	if len(binsSeen) != 2 {
		t.Errorf("order_bins reference %d distinct bin(s), want 2", len(binsSeen))
	}
	if len(stepsSeen) != 2 {
		t.Errorf("order_bins reference %d distinct step index(es), want 2", len(stepsSeen))
	}
}
