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

// TestSystemUOPForPayload_BucketArmSumsEveryStyleRowPlusBins pins the bucket
// arm's shape at the base: TotalUOP = BinUOP + the SUM over every
// lineside_buckets row of the payload, whatever its style_id, so the two rows a
// style re-stamp leaves for one Edge pile both count.
//
// Flips under brief v7 expected change #1: one row per pile, so the second
// (style-19) row cannot exist and the bucket term reads the Edge's pile.
func TestSystemUOPForPayload_BucketArmSumsEveryStyleRowPlusBins(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewInventoryService(db)
	const payload = "P-SPLIT"

	line := &nodes.Node{Name: "ALN-SPLIT", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(line), "create node")
	createTestBin(t, db, line.ID, "BIN-SPLIT", payload, 40)
	insertLinesideBucket(t, db, line.Name, 12, payload, 100) // pre-re-stamp row
	insertLinesideBucket(t, db, line.Name, 19, payload, 20)  // post-re-stamp row, same pile

	res, err := svc.SystemUOPForPayload(context.Background(), []string{payload})
	testutil.MustNoErr(t, err, "SystemUOPForPayload")
	got := res.Counts[0]
	if got.BinUOP != 40 || got.BucketUOP != 120 || got.TotalUOP != 160 {
		t.Errorf("counts = bins %d buckets %d total %d, want 40 / 120 / 160 (both style rows sum)",
			got.BinUOP, got.BucketUOP, got.TotalUOP)
	}
}

// TestSystemUOPForPayload_AllowedPayloadKeepsABucketCounted pins the second
// arm of the claims-derived stranded rule: a bucket whose part is only in the
// active claim's allowed_payload_codes (not its payload_code) is covered, so it
// counts. TestSystemUOPForPayload_ExcludesStrandedBuckets covers the
// payload_code arm and the unmirrored-node default.
//
// Stays under the change (the pile is active and counts; brief v7 #3 replaces
// the rule with a state, which this bucket does not have).
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
	insertLinesideBucket(t, db, node, 1, alt, 7)

	res, err := svc.SystemUOPForPayload(context.Background(), []string{alt})
	testutil.MustNoErr(t, err, "SystemUOPForPayload")
	if got := res.Counts[0].BucketUOP; got != 7 {
		t.Errorf("BucketUOP = %d, want 7 (an allowed payload of the active claim is not stranded)", got)
	}
}
