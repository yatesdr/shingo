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

			// placedByOrder is the CLAIMER, which is what a handoff is and what
			// every production caller passes: an order delivering the bin it owns.
			// It used to be 0 here, which no longer means "clear whatever claim is
			// there" — the unclaim is owner-scoped, because an unscoped one erased
			// other orders' claims on the rig.
			evicted, err := svc.ApplyArrival(bin.ID, destNode.ID, tc.staged, tc.expiresAt, claimer.ID)
			testutil.MustNoErr(t, err, "ApplyArrival")
			if len(evicted) > 0 {
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
// different bin is proof (a delivery cannot physically complete onto an
// occupied slot, so the completed delivery proves the slot was empty) that the
// recorded bin is a stale ghost. ApplyArrival must place the arriving bin and
// evict the ghost to _TRANSIT with an anomaly mark, never reject the newcomer —
// and report evicted=true so the caller can alert.
//
// This is the DEAD-CLAIM case, and only here is the claim cleared too. Ownership
// and position are separate facts; the live-holder contract is pinned by
// TestApplyArrival_GhostEvictionKeepsALiveHolderClaim.
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

	// Stale ghost: shingo records it at the destination, holding a DEAD claim —
	// its order already went terminal, so nothing is coming back for this bin.
	// That is what makes it an orphan the operator must recover, and it is the
	// only case in which the eviction may clear ownership. The live-holder case
	// is the opposite contract and has its own test below.
	ghost := &bins.Bin{BinTypeID: bt.ID, Label: "F-GHOST", NodeID: &destNode.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(ghost), "create ghost bin")
	ghostOrder := testdb.CreateOrder(t, db)
	testdb.ClaimBinForTest(t, db, ghost.ID, ghostOrder.ID)
	if _, err := db.Exec(`UPDATE orders SET status='confirmed' WHERE id=$1`, ghostOrder.ID); err != nil {
		t.Fatalf("terminalize ghost holder: %v", err)
	}

	// Arriving bin: the real, RDS-verified bin being delivered.
	arriving := &bins.Bin{BinTypeID: bt.ID, Label: "F-ARRIVING", NodeID: &startNode.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(arriving), "create arriving bin")
	arrivingOrder := testdb.CreateOrder(t, db)
	testdb.ClaimBinForTest(t, db, arriving.ID, arrivingOrder.ID)

	evicted, err := svc.ApplyArrival(arriving.ID, destNode.ID, false, nil, arrivingOrder.ID)
	testutil.MustNoErr(t, err, "ApplyArrival")
	// The IDENTITY of what was displaced, not just that something was. A bare
	// bool was all this returned until the caller needed to write the evicted
	// bin's own audit row — an eviction that leaves no trace on its victim is
	// how CARRIER-0003's journal came to name an order from the night before
	// (Springfield 2026-08-26). See engine.noteEvictedGhosts.
	if len(evicted) != 1 || evicted[0] != ghost.ID {
		t.Fatalf("evicted = %v, want exactly [%d] (the occupied destination's stale ghost, by id)",
			evicted, ghost.ID)
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
		t.Errorf("ghost ClaimedBy = %v, want nil — its holder is terminal, so the claim is dead "+
			"and clearing it is what surfaces the bin in ListAnomalies", gotGhost.ClaimedBy)
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

	evicted, err := svc.ApplyArrival(arriving.ID, syn.ID, false, nil, 0)
	testutil.MustNoErr(t, err, "ApplyArrival")
	if len(evicted) > 0 {
		t.Error("evicted = true, want false (synthetic destinations hold many bins; no eviction)")
	}
	gotOcc, _ := db.GetBin(occupant.ID)
	if gotOcc.NodeID == nil || *gotOcc.NodeID != syn.ID {
		t.Errorf("occupant NodeID = %v, want %d (must stay; synthetic node not evicted)", gotOcc.NodeID, syn.ID)
	}
}

// TestApplyArrival_GhostEvictionKeepsALiveHolderClaim is the ownership half of
// the eviction contract, and the fix for the stranding chain in PLAN §R.5.
//
// The eviction's licence is "a completed delivery proves this slot was empty".
// That is a statement about POSITION. It says nothing about who owns the bin
// whose position was wrong — and a bin claimed by a LIVE order is one a robot
// may be carrying at that moment.
//
// Nulling that claim broke the carrier: its own arrival then read as a teleport
// to the arrival guard ("this bin is not claimed by me"), the guard refused it,
// the bin stranded at _TRANSIT owned by nobody, and the order confirmed anyway
// — reporting a delivery it never made. On the rig one bin was refused twice,
// for two different orders, 76 seconds apart.
//
// So: the position is evicted, the live claim survives, and the bin does NOT go
// on the anomalies page — it is not an orphan awaiting an operator, it is owned
// by an order that is still coming for it. The dead-holder case above is the one
// that gets unclaimed and listed.
//
// Mutation: restore the unconditional `claimed_by=NULL` in EvictStaleGhostBinsTx
// and the ClaimedBy assertion below fires.
func TestApplyArrival_GhostEvictionKeepsALiveHolderClaim(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := newBinSvc(db)

	bt := &bins.BinType{Code: "L-BT", Description: "tote"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "create bin type")
	transit, err := db.GetNodeByName(domain.TransitNodeName)
	testutil.MustNoErr(t, err, "lookup _TRANSIT")

	startNode := &nodes.Node{Name: "L-START", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(startNode), "create start node")
	destNode := &nodes.Node{Name: "L-DEST", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(destNode), "create dest node")

	// The occupant, claimed by an order that is STILL LIVE — the carrier.
	carried := &bins.Bin{BinTypeID: bt.ID, Label: "L-CARRIED", NodeID: &destNode.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(carried), "create carried bin")
	carrier := testdb.CreateOrder(t, db) // status 'queued' — non-terminal
	testdb.ClaimBinForTest(t, db, carried.ID, carrier.ID)

	arriving := &bins.Bin{BinTypeID: bt.ID, Label: "L-ARRIVING", NodeID: &startNode.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(arriving), "create arriving bin")
	arrivingOrder := testdb.CreateOrder(t, db)
	testdb.ClaimBinForTest(t, db, arriving.ID, arrivingOrder.ID)

	evicted, err := svc.ApplyArrival(arriving.ID, destNode.ID, false, nil, arrivingOrder.ID)
	testutil.MustNoErr(t, err, "ApplyArrival")
	if len(evicted) == 0 {
		t.Fatal("evicted = false, want true — the occupied physical destination must still be reconciled")
	}

	got, _ := db.GetBin(carried.ID)
	// The POSITION is still evicted: that half of the contract is unchanged.
	if got.NodeID == nil || *got.NodeID != transit.ID {
		t.Errorf("carried NodeID = %v, want %d (_TRANSIT) — the position must still be corrected", got.NodeID, transit.ID)
	}
	if got.AnomalyAt == nil {
		t.Error("carried AnomalyAt = nil, want set — the position was wrong and that is worth a mark")
	}
	// THE ASSERTION THE OLD CODE FAILED.
	if got.ClaimedBy == nil {
		t.Fatalf("carried ClaimedBy = nil, want order %d — the eviction wiped a LIVE claim, which is "+
			"what made the holder's own arrival look like a teleport and stranded the bin (PLAN §R.5)", carrier.ID)
	}
	if *got.ClaimedBy != carrier.ID {
		t.Errorf("carried ClaimedBy = %d, want %d", *got.ClaimedBy, carrier.ID)
	}

	// And it is NOT an operator anomaly: a live order still owns it and is coming
	// back for it. Listing it would ask a human to recover a bin nothing lost.
	anomalies, err := svc.ListAnomalies()
	testutil.MustNoErr(t, err, "ListAnomalies")
	for _, b := range anomalies {
		if b.ID == carried.ID {
			t.Error("a bin with a live holder is on the anomalies page — it is owned, not orphaned")
		}
	}
}

// TestListAnomalies_SeesAStrayOnAnySyntheticNode pins the widening: the page
// filtered on the name _TRANSIT, and a bin stranded on a DIFFERENT synthetic
// node was invisible to it.
//
// A node group is not somewhere a bin can physically be, so a bin recorded on one
// with no owner is a stray by construction — but with no anomaly stamp and no
// _TRANSIT row it appeared on no page, was covered by no floor, and would be
// handed out by no selector. Observed on the rig as bin 37 at SYN_COMP, unowned
// and unseen for an entire run.
//
// The absence of anomaly_at is the point of the fixture: requiring the stamp
// would re-hide exactly this shape, because the stamp is written by the paths
// that KNOW they stranded something and this bin's problem is that nobody did.
func TestListAnomalies_SeesAStrayOnAnySyntheticNode(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := newBinSvc(db)

	bt := &bins.BinType{Code: "STRAY-BT", Description: "tote"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "create bin type")

	grp := &nodes.Node{Name: "STRAY-SYN-GROUP", Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(grp), "create synthetic group")

	stray := &bins.Bin{BinTypeID: bt.ID, Label: "STRAY-BIN", NodeID: &grp.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(stray), "create stray bin")
	// Unclaimed and UNSTAMPED — no anomaly_at, exactly bin 37's shape.

	anomalies, err := svc.ListAnomalies()
	testutil.MustNoErr(t, err, "ListAnomalies")
	for _, b := range anomalies {
		if b.ID == stray.ID {
			return
		}
	}
	t.Errorf("a bin unclaimed on synthetic node %q is not on the anomalies page — nothing lists it, "+
		"no floor covers it, and no selector will hand it out, so it is stranded silently", grp.Name)
}

// TestApplyArrival_DoesNotClearAnotherOrdersClaim is the integrity defect that
// failed two swaps and cancelled their partners on the lane-stress rig
// 2026-08-13.
//
// ── THE SPECIMEN ──────────────────────────────────────────────────────────
//
// Dig leg order 9 carried blocker bin 6 out of LSD_011 and set it down at
// LSD_010. A blocker in a parking slot is an ordinary reachable bin of an
// ordinary style — which is exactly what a demand resolves onto — and order 1
// had already claimed it. Order 9's arrival cleared that claim, because the
// unclaim was `WHERE id=$1` and nothing else.
//
// Everything after that followed: order 1 picked up its OWN bin with no claim on
// it, so its intermediate set-down found "0 bins in transit under this claim" and
// never recorded, and at final delivery the ledger check refused a robot carrying
// a bin nobody owned. Order 1 failed on cargo_ledger_mismatch and took its
// two-robot swap partner (order 7) with it. Twice in one window.
//
// The ledger check is right and the failure is honest — Core genuinely could not
// identify what the robot was carrying. What produced the state it refused was
// one unscoped UPDATE.
//
// MUTATION: drop the claimed_by predicate from applyArrival's unclaim. The
// foreign claim is cleared again and the assertion fires — which is the rig's
// own failure, one layer upstream of where it surfaced.
func TestApplyArrival_DoesNotClearAnotherOrdersClaim(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := newBinSvc(db)

	bt := &bins.BinType{Code: "FC-BT", Description: "tote"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "create bin type")
	start := &nodes.Node{Name: "FC-START", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(start), "create start node")
	dest := &nodes.Node{Name: "FC-DEST", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dest), "create dest node")

	bin := &bins.Bin{BinTypeID: bt.ID, Label: "FC-BIN", NodeID: &start.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(bin), "create bin")

	// THE DEMAND owns the bin. THE DIG LEG is putting it down. Two unrelated
	// orders, which is the whole fixture: a dig parks a blocker, and the bin it
	// parks is the most reachable one of its style in the group.
	demand := testdb.CreateOrder(t, db)
	digLeg := testdb.CreateOrder(t, db)
	testdb.ClaimBinForTest(t, db, bin.ID, demand.ID)

	if _, err := svc.ApplyArrival(bin.ID, dest.ID, false, nil, digLeg.ID); err != nil {
		t.Fatalf("ApplyArrival: %v", err)
	}

	got, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "reload the bin")

	// THE BIN IS PLACED EITHER WAY. The placement is not in question.
	if got.NodeID == nil || *got.NodeID != dest.ID {
		t.Fatalf("bin is at %v, want %d — the placement itself must be unaffected", got.NodeID, dest.ID)
	}
	// AND ITS OWNER STILL OWNS IT.
	if got.ClaimedBy == nil || *got.ClaimedBy != demand.ID {
		t.Fatalf("bin %d is claimed by %v after order %d placed it, want order %d. A handoff gives up "+
			"THIS order's claim, not whoever's is there: order %d never let go of this bin, and a bin "+
			"whose owner has been erased is one the delivery ledger cannot identify — which fails the "+
			"order, and its two-robot swap partner with it",
			bin.ID, got.ClaimedBy, digLeg.ID, demand.ID, demand.ID)
	}

	// AND THE PLACER'S OWN CLAIM IS STILL GIVEN UP, which is the half that must
	// not regress: scoping the unclaim must not turn into never unclaiming.
	own := &bins.Bin{BinTypeID: bt.ID, Label: "FC-OWN", NodeID: &start.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(own), "create the placer's own bin")
	dest2 := &nodes.Node{Name: "FC-DEST-2", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dest2), "create second dest")
	testdb.ClaimBinForTest(t, db, own.ID, digLeg.ID)

	if _, err := svc.ApplyArrival(own.ID, dest2.ID, false, nil, digLeg.ID); err != nil {
		t.Fatalf("ApplyArrival for the placer's own bin: %v", err)
	}
	after, err := db.GetBin(own.ID)
	testutil.MustNoErr(t, err, "reload the placer's own bin")
	if after.ClaimedBy != nil {
		t.Errorf("the placing order's own claim survived its delivery (claimed_by = %v). A handoff IS "+
			"the end of that order's ownership, and a claim that outlives it blocks re-reservation "+
			"until the order terminalizes", after.ClaimedBy)
	}
}
