//go:build docker

package service

import (
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingocore/domain"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// TestApplyArrival exercises BinService.ApplyArrival's two-branch contract:
// staged vs. unstaged, claim-release in both. The orchestration body lives
// in this package as of Phase 6.4a (moved from store/completion.go's
// (db *DB).ApplyBinArrival, which has been deleted).
func TestApplyArrival(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := newBinSvc(db)

	bt := &bins.BinType{Code: "AB-BT", Description: "tote"}
	db.CreateBinType(bt)

	startNode := &nodes.Node{Name: "AB-START", Enabled: true}
	db.CreateNode(startNode)

	cases := []struct {
		name      string
		staged    bool
		expiresAt *time.Time
		wantStat  domain.BinStatus
	}{
		{"unstaged arrival", false, nil, "available"},
		{
			"staged arrival",
			true,
			func() *time.Time { tt := time.Now().Add(2 * time.Hour); return &tt }(),
			"staged",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Per-case destination so one case's arrival doesn't leave a bin
			// that the next case's arrival would (correctly) evict.
			destNode := &nodes.Node{Name: "AB-DEST-" + tc.name, Enabled: true}
			testutil.MustNoErr(t, db.CreateNode(destNode), "create dest node")
			bin := &bins.Bin{BinTypeID: bt.ID, Label: "AB-" + tc.name, NodeID: &startNode.ID, Status: "available"}
			testutil.MustNoErr(t, db.CreateBin(bin), "create bin")
			// Claim so we can verify ApplyArrival releases it.
			claimer := testdb.CreateOrder(t, db)
			testdb.ClaimBinForTest(t, db, bin.ID, claimer.ID)

			evicted, err := svc.ApplyArrival(bin.ID, destNode.ID, tc.staged, tc.expiresAt)
			testutil.MustNoErr(t, err, "ApplyArrival")
			if evicted {
				t.Errorf("evicted = true, want false (arrival onto an empty destination must not evict)")
			}

			got, _ := db.GetBin(bin.ID)
			if got.NodeID == nil || *got.NodeID != destNode.ID {
				t.Errorf("NodeID = %v, want %d", got.NodeID, destNode.ID)
			}
			if got.ClaimedBy != nil {
				t.Errorf("ClaimedBy = %v, want nil after arrival", got.ClaimedBy)
			}
			if got.Status != tc.wantStat {
				t.Errorf("Status = %q, want %q", got.Status, tc.wantStat)
			}
			if tc.staged {
				if got.StagedAt == nil {
					t.Error("StagedAt should be set when staged=true")
				}
				if tc.expiresAt != nil {
					if got.StagedExpiresAt == nil {
						t.Error("StagedExpiresAt should be set when expiresAt provided")
					} else {
						// Compare to within a second to allow for round-trip precision.
						diff := got.StagedExpiresAt.Sub(*tc.expiresAt)
						if diff < -time.Second || diff > time.Second {
							t.Errorf("StagedExpiresAt = %v, want ~%v", got.StagedExpiresAt, tc.expiresAt)
						}
					}
				}
			} else {
				if got.StagedAt != nil {
					t.Errorf("StagedAt = %v, want nil for unstaged", got.StagedAt)
				}
				if got.StagedExpiresAt != nil {
					t.Errorf("StagedExpiresAt = %v, want nil for unstaged", got.StagedExpiresAt)
				}
			}
		})
	}
}

// TestApplyArrival_EvictsStaleGhostOnOccupiedPhysicalNode verifies that a
// delivery onto a physical node that shingo still records as holding a
// different bin is proof (RDS faults on delivery to an occupied single-bin
// slot) that the recorded bin is a stale ghost. ApplyArrival must place the
// arriving bin and evict the ghost to _TRANSIT (unclaimed + anomaly), never
// reject the newcomer — and report evicted=true so the caller can alert.
func TestApplyArrival_EvictsStaleGhostOnOccupiedPhysicalNode(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := newBinSvc(db)

	bt := &bins.BinType{Code: "F-BT", Description: "tote"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "create bin type")
	transit, err := db.GetNodeByName(domain.TransitNodeName)
	testutil.MustNoErr(t, err, "lookup _TRANSIT (migration v15)")

	startNode := &nodes.Node{Name: "F-START", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(startNode), "create start node")
	destNode := &nodes.Node{Name: "F-DEST", Enabled: true} // physical: IsSynthetic false
	testutil.MustNoErr(t, db.CreateNode(destNode), "create dest node")

	// Stale ghost: shingo records it at the destination, still claimed.
	ghost := &bins.Bin{BinTypeID: bt.ID, Label: "F-GHOST", NodeID: &destNode.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(ghost), "create ghost bin")
	ghostOrder := testdb.CreateOrder(t, db)
	testdb.ClaimBinForTest(t, db, ghost.ID, ghostOrder.ID)

	// Arriving bin: the real, RDS-verified bin being delivered.
	arriving := &bins.Bin{BinTypeID: bt.ID, Label: "F-ARRIVING", NodeID: &startNode.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(arriving), "create arriving bin")
	arrivingOrder := testdb.CreateOrder(t, db)
	testdb.ClaimBinForTest(t, db, arriving.ID, arrivingOrder.ID)

	evicted, err := svc.ApplyArrival(arriving.ID, destNode.ID, false, nil)
	testutil.MustNoErr(t, err, "ApplyArrival")
	if !evicted {
		t.Fatal("evicted = false, want true (occupied physical destination must evict the stale ghost)")
	}

	// The arriving bin took the slot, unclaimed.
	gotArr, _ := db.GetBin(arriving.ID)
	if gotArr.NodeID == nil || *gotArr.NodeID != destNode.ID {
		t.Errorf("arriving NodeID = %v, want %d (newcomer must be placed, never rejected)", gotArr.NodeID, destNode.ID)
	}
	if gotArr.ClaimedBy != nil {
		t.Errorf("arriving ClaimedBy = %v, want nil", gotArr.ClaimedBy)
	}

	// The ghost was evicted to _TRANSIT, unclaimed, with anomaly_at set.
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

	// And it surfaces in the operator anomaly list.
	anomalies, err := svc.ListAnomalies()
	testutil.MustNoErr(t, err, "ListAnomalies")
	found := false
	for _, b := range anomalies {
		if b.ID == ghost.ID {
			found = true
		}
	}
	if !found {
		t.Error("evicted ghost not in ListAnomalies — operator can't recover it")
	}
}

// TestApplyArrival_SyntheticDestNotEvicted pins the exemption: a synthetic
// destination (LANE/NGRP/_TRANSIT) legitimately holds many bins, so an
// arrival there must not evict the existing occupants.
func TestApplyArrival_SyntheticDestNotEvicted(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := newBinSvc(db)

	bt := &bins.BinType{Code: "F-SYN-BT", Description: "tote"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "create bin type")

	start := &nodes.Node{Name: "F-SYN-START", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(start), "create start")
	syn := &nodes.Node{Name: "F-SYN-DEST", Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(syn), "create synthetic dest")

	occupant := &bins.Bin{BinTypeID: bt.ID, Label: "F-SYN-OCC", NodeID: &syn.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(occupant), "create occupant")
	arriving := &bins.Bin{BinTypeID: bt.ID, Label: "F-SYN-ARR", NodeID: &start.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(arriving), "create arriving")

	evicted, err := svc.ApplyArrival(arriving.ID, syn.ID, false, nil)
	testutil.MustNoErr(t, err, "ApplyArrival")
	if evicted {
		t.Error("evicted = true, want false (synthetic destinations hold many bins; no eviction)")
	}
	gotOcc, _ := db.GetBin(occupant.ID)
	if gotOcc.NodeID == nil || *gotOcc.NodeID != syn.ID {
		t.Errorf("occupant NodeID = %v, want %d (must stay; synthetic node not evicted)", gotOcc.NodeID, syn.ID)
	}
}
