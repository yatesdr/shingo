//go:build docker

package service

import (
	"context"
	"testing"

	"shingo/protocol"
	"shingocore/store"
	"shingocore/store/plantclaims"
)

// insertLinesideBucket seeds one lineside_buckets row directly, in the given
// state: Core's mirror of one Edge pile.
func insertLinesideBucket(t *testing.T, db *store.DB, node string, state protocol.LinesideBucketState, payload string, qty int) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO lineside_buckets (station, core_node_name, payload_code, state, qty)
		 VALUES ($1,$2,$3,$4,$5)`,
		"test-station", node, payload, string(state), qty,
	); err != nil {
		t.Fatalf("insert lineside bucket %s@%s (%s): %v", payload, node, state, err)
	}
}

// TestSystemUOPForPayload_CountsActivePilesOnly pins the bucket arm's rule:
// on-hand counts a pile while it is active and never once it is stranded.
//
// This was TestSystemUOPForPayload_ExcludesStrandedBuckets, the changeover
// decision of 2026-07-23 computed from the plant-claims mirror: a pile at a
// node whose active style no longer claimed its payload was "stranded" and
// left out, and a pile at a node with no mirror was counted. FLIPPED BY BRIEF
// v7 EXPECTED CHANGE #3: stranded is the state the Edge set at the cutover, so
// the claims no longer enter into it. The 250 below, an active pile of a part
// the node's style does not claim, now counts (it drains, or the cutover would
// have stranded it), and the stranded rows of that part never count, mirrored
// node or not.
func TestSystemUOPForPayload_CountsActivePilesOnly(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewInventoryService(db)

	const (
		node       = "ALN-STRAND"
		unmirrored = "ALN-UNMIRRORED"
		claimed    = "P-ACTIVE"
		unclaimed  = "P-STRANDED"
	)

	// Mirror: node ALN-STRAND runs an active style that consumes P-ACTIVE only.
	if err := plantclaims.ReplaceProcess(db.DB, "PROC-1",
		[]plantclaims.StyleRow{{ProcessID: "PROC-1", StyleID: "STYLE-ACTIVE", ConfigGen: 1, IsActive: true}},
		[]plantclaims.ClaimRow{{
			ProcessID:           "PROC-1",
			StyleID:             "STYLE-ACTIVE",
			CoreNodeName:        node,
			Role:                protocol.ClaimRoleConsume,
			PayloadCode:         claimed,
			AllowedPayloadCodes: []string{claimed},
		}}, 0,
	); err != nil {
		t.Fatalf("seed plant claims: %v", err)
	}

	insertLinesideBucket(t, db, node, protocol.LinesideBucketActive, claimed, 100)
	insertLinesideBucket(t, db, node, protocol.LinesideBucketActive, unclaimed, 250)
	insertLinesideBucket(t, db, node, protocol.LinesideBucketStranded, unclaimed, 40)
	insertLinesideBucket(t, db, unmirrored, protocol.LinesideBucketStranded, unclaimed, 30)

	res, err := svc.SystemUOPForPayload(context.Background(), []string{claimed, unclaimed})
	if err != nil {
		t.Fatalf("SystemUOPForPayload: %v", err)
	}
	got := map[string]int{}
	for _, c := range res.Counts {
		got[c.PayloadCode] = c.BucketUOP
	}
	if got[claimed] != 100 {
		t.Errorf("%s bucket UOP = %d, want 100", claimed, got[claimed])
	}
	if got[unclaimed] != 250 {
		t.Errorf("%s bucket UOP = %d, want 250 (the active pile counts; both stranded rows do not)", unclaimed, got[unclaimed])
	}
}
