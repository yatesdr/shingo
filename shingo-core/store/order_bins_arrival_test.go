//go:build docker

package store_test

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/domain"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// TestApplyMultiBinArrival_EvictsStaleGhostOnOccupiedPhysicalNode is the
// multi-bin twin of TestApplyArrival_EvictsStaleGhostOnOccupiedPhysicalNode:
// a delivery cannot physically complete onto an occupied slot, so a different
// bin still recorded at a per-step destination is a stale ghost. The arrival
// must place the arriving bin and evict the ghost to _TRANSIT (unclaimed +
// anomaly), return its id, and never reject the newcomer.
func TestApplyMultiBinArrival_EvictsStaleGhostOnOccupiedPhysicalNode(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	bt := &bins.BinType{Code: "MB-BT", Description: "tote"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "create bin type")
	transit, err := db.GetNodeByName(domain.TransitNodeName)
	testutil.MustNoErr(t, err, "lookup _TRANSIT (migration v15)")

	startNode := &nodes.Node{Name: "MB-START", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(startNode), "create start node")
	// Two physical destinations (IsSynthetic false). destA holds a stale ghost;
	// destB is empty, to prove only the occupied node triggers an eviction.
	destA := &nodes.Node{Name: "MB-DEST-A", Enabled: true}
	destB := &nodes.Node{Name: "MB-DEST-B", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(destA), "create destA")
	testutil.MustNoErr(t, db.CreateNode(destB), "create destB")

	// Stale ghost: shingo records it at destA, still claimed.
	ghost := &bins.Bin{BinTypeID: bt.ID, Label: "MB-GHOST", NodeID: &destA.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(ghost), "create ghost bin")
	ghostOrder := testdb.CreateOrder(t, db)
	testdb.ClaimBinForTest(t, db, ghost.ID, ghostOrder.ID)

	// Two arriving bins, one per destination, both claimed by the same order.
	arrA := &bins.Bin{BinTypeID: bt.ID, Label: "MB-ARR-A", NodeID: &startNode.ID, Status: "available"}
	arrB := &bins.Bin{BinTypeID: bt.ID, Label: "MB-ARR-B", NodeID: &startNode.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(arrA), "create arrA")
	testutil.MustNoErr(t, db.CreateBin(arrB), "create arrB")
	order := testdb.CreateOrder(t, db)
	testdb.ClaimBinForTest(t, db, arrA.ID, order.ID)
	testdb.ClaimBinForTest(t, db, arrB.ID, order.ID)

	evicted, err := db.ApplyMultiBinArrival([]orders.BinArrivalInstruction{
		{BinID: arrA.ID, ToNodeID: destA.ID}, // collides with the ghost
		{BinID: arrB.ID, ToNodeID: destB.ID}, // empty destination
	})
	testutil.MustNoErr(t, err, "ApplyMultiBinArrival")

	// Exactly the ghost was reported evicted.
	if len(evicted) != 1 || evicted[0] != ghost.ID {
		t.Fatalf("evicted = %v, want [%d] (only the occupied destination evicts)", evicted, ghost.ID)
	}

	// Both arriving bins took their slots, unclaimed.
	for _, p := range []struct {
		bin, node int64
		label     string
	}{{arrA.ID, destA.ID, "arrA"}, {arrB.ID, destB.ID, "arrB"}} {
		got, _ := db.GetBin(p.bin)
		if got.NodeID == nil || *got.NodeID != p.node {
			t.Errorf("%s NodeID = %v, want %d (newcomer must be placed, never rejected)", p.label, got.NodeID, p.node)
		}
		if got.ClaimedBy != nil {
			t.Errorf("%s ClaimedBy = %v, want nil after arrival", p.label, got.ClaimedBy)
		}
	}

	// The ghost was evicted to _TRANSIT, unclaimed, with anomaly_at set, and
	// surfaces on the operator anomaly list.
	gotGhost, _ := db.GetBin(ghost.ID)
	if gotGhost.NodeID == nil || *gotGhost.NodeID != transit.ID {
		t.Errorf("ghost NodeID = %v, want %d (_TRANSIT)", gotGhost.NodeID, transit.ID)
	}
	if gotGhost.ClaimedBy != nil {
		t.Errorf("ghost ClaimedBy = %v, want nil (must unclaim so it surfaces in ListAnomalies)", gotGhost.ClaimedBy)
	}
	if gotGhost.AnomalyAt == nil {
		t.Error("ghost AnomalyAt = nil, want set")
	}
	anomalies, err := db.ListAnomalousTransitBins()
	testutil.MustNoErr(t, err, "ListAnomalousTransitBins")
	found := false
	for _, b := range anomalies {
		if b.ID == ghost.ID {
			found = true
		}
	}
	if !found {
		t.Error("evicted ghost not in anomalous-transit list — operator can't recover it")
	}
}

// TestApplyMultiBinArrival_SyntheticDestNotEvicted pins the exemption for the
// multi-bin path: a synthetic destination (LANE/NGRP) legitimately holds many
// bins, so an arrival there must not evict the existing occupants.
func TestApplyMultiBinArrival_SyntheticDestNotEvicted(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	bt := &bins.BinType{Code: "MB-SYN-BT", Description: "tote"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "create bin type")

	start := &nodes.Node{Name: "MB-SYN-START", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(start), "create start")
	syn := &nodes.Node{Name: "MB-SYN-DEST", Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(syn), "create synthetic dest")

	occupant := &bins.Bin{BinTypeID: bt.ID, Label: "MB-SYN-OCC", NodeID: &syn.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(occupant), "create occupant")
	arriving := &bins.Bin{BinTypeID: bt.ID, Label: "MB-SYN-ARR", NodeID: &start.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(arriving), "create arriving")
	order := testdb.CreateOrder(t, db)
	testdb.ClaimBinForTest(t, db, arriving.ID, order.ID)

	evicted, err := db.ApplyMultiBinArrival([]orders.BinArrivalInstruction{
		{BinID: arriving.ID, ToNodeID: syn.ID},
	})
	testutil.MustNoErr(t, err, "ApplyMultiBinArrival")
	if len(evicted) != 0 {
		t.Errorf("evicted = %v, want none (synthetic destinations hold many bins)", evicted)
	}
	gotOcc, _ := db.GetBin(occupant.ID)
	if gotOcc.NodeID == nil || *gotOcc.NodeID != syn.ID {
		t.Errorf("occupant NodeID = %v, want %d (must stay; synthetic node not evicted)", gotOcc.NodeID, syn.ID)
	}
}

// TestApplyMultiBinArrival_EvictsMultipleGhostsAtOneNode covers a pre-existing
// double-occupancy: two stale records at one physical node are both evicted
// when a newcomer arrives there.
func TestApplyMultiBinArrival_EvictsMultipleGhostsAtOneNode(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	bt := &bins.BinType{Code: "MB-MG-BT", Description: "tote"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "create bin type")
	transit, err := db.GetNodeByName(domain.TransitNodeName)
	testutil.MustNoErr(t, err, "lookup _TRANSIT")

	start := &nodes.Node{Name: "MB-MG-START", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(start), "create start")
	dest := &nodes.Node{Name: "MB-MG-DEST", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dest), "create dest")

	ghost1 := &bins.Bin{BinTypeID: bt.ID, Label: "MB-MG-G1", NodeID: &dest.ID, Status: "available"}
	ghost2 := &bins.Bin{BinTypeID: bt.ID, Label: "MB-MG-G2", NodeID: &dest.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(ghost1), "create ghost1")
	testutil.MustNoErr(t, db.CreateBin(ghost2), "create ghost2")
	arriving := &bins.Bin{BinTypeID: bt.ID, Label: "MB-MG-ARR", NodeID: &start.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(arriving), "create arriving")
	order := testdb.CreateOrder(t, db)
	testdb.ClaimBinForTest(t, db, arriving.ID, order.ID)

	evicted, err := db.ApplyMultiBinArrival([]orders.BinArrivalInstruction{
		{BinID: arriving.ID, ToNodeID: dest.ID},
	})
	testutil.MustNoErr(t, err, "ApplyMultiBinArrival")
	if len(evicted) != 2 {
		t.Fatalf("evicted %d ghosts, want 2", len(evicted))
	}
	for _, g := range []*bins.Bin{ghost1, ghost2} {
		got, _ := db.GetBin(g.ID)
		if got.NodeID == nil || *got.NodeID != transit.ID {
			t.Errorf("ghost %s NodeID = %v, want %d (_TRANSIT)", g.Label, got.NodeID, transit.ID)
		}
		if got.AnomalyAt == nil {
			t.Errorf("ghost %s AnomalyAt = nil, want set", g.Label)
		}
	}
	gotArr, _ := db.GetBin(arriving.ID)
	if gotArr.NodeID == nil || *gotArr.NodeID != dest.ID {
		t.Errorf("arriving NodeID = %v, want %d (newcomer placed)", gotArr.NodeID, dest.ID)
	}
}
