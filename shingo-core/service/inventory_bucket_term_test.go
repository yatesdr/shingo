//go:build docker

package service

import (
	"context"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/store/nodes"
	"shingocore/store/plantclaims"
)

// TestSystemUOPForPayload_BucketArmIsTheActivePilePlusBins pins the bucket
// arm: TotalUOP = BinUOP + the payload's ACTIVE piles.
//
// This was TestSystemUOPForPayload_BucketArmSumsEveryStyleRowPlusBins, which
// pinned the old arm summing every row of the payload whatever its style_id,
// so the two rows a style re-stamp left for one Edge pile both counted (40 +
// 100 + 20). FLIPPED BY BRIEF v7 EXPECTED CHANGE #1: one row per (node, part,
// state), so there is no second style row; the pile that was re-stamped is one
// active row, and what a cutover left is a stranded row of the same payload at
// the same node, which does not count.
func TestSystemUOPForPayload_BucketArmIsTheActivePilePlusBins(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewInventoryService(db)
	const payload = "P-SPLIT"

	line := &nodes.Node{Name: "ALN-SPLIT", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(line), "create node")
	createTestBin(t, db, line.ID, "BIN-SPLIT", payload, 40)
	insertLinesideBucket(t, db, line.Name, protocol.LinesideBucketActive, payload, 20)
	insertLinesideBucket(t, db, line.Name, protocol.LinesideBucketStranded, payload, 100)

	res, err := svc.SystemUOPForPayload(context.Background(), []string{payload})
	testutil.MustNoErr(t, err, "SystemUOPForPayload")
	got := res.Counts[0]
	if got.BinUOP != 40 || got.BucketUOP != 20 || got.TotalUOP != 60 {
		t.Errorf("counts = bins %d buckets %d total %d, want 40 / 20 / 60 (the stranded 100 does not count)",
			got.BinUOP, got.BucketUOP, got.TotalUOP)
	}
}

// TestSystemUOPForPayload_AllowedPayloadKeepsABucketCounted pinned the second
// arm of the claims-derived stranded rule: a bucket whose part is only in the
// active claim's allowed_payload_codes (not its payload_code) is covered, so it
// counts.
//
// Stays green under the change: the pile is active and counts. Brief v7 #3
// replaced the claims rule with the state column, so the claims seeded here no
// longer enter into it (TestSystemUOPForPayload_CountsActivePilesOnly).
func TestSystemUOPForPayload_AllowedPayloadKeepsABucketCounted(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewInventoryService(db)
	const (
		node    = "ALN-ALLOWED"
		primary = "P-PRIMARY"
		alt     = "P-ALT"
	)
	testutil.MustNoErr(t, plantclaims.ReplaceProcess(db.DB, "PROC-ALLOWED",
		[]plantclaims.StyleRow{{ProcessID: "PROC-ALLOWED", StyleID: "S-ACTIVE", ConfigGen: 1, IsActive: true}},
		[]plantclaims.ClaimRow{{
			ProcessID: "PROC-ALLOWED", StyleID: "S-ACTIVE", CoreNodeName: node,
			Role: protocol.ClaimRoleConsume, PayloadCode: primary,
			AllowedPayloadCodes: []string{primary, alt},
		}}, 0), "seed plant claims")
	insertLinesideBucket(t, db, node, protocol.LinesideBucketActive, alt, 7)

	res, err := svc.SystemUOPForPayload(context.Background(), []string{alt})
	testutil.MustNoErr(t, err, "SystemUOPForPayload")
	if got := res.Counts[0].BucketUOP; got != 7 {
		t.Errorf("BucketUOP = %d, want 7 (an allowed payload of the active claim is not stranded)", got)
	}
}
