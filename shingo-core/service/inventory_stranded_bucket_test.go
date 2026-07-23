//go:build docker

package service

import (
	"context"
	"testing"

	"shingo/protocol"
	"shingocore/store"
	"shingocore/store/plantclaims"
)

// insertLinesideBucket seeds one lineside_buckets row directly. style_id is required
// (BIGINT NOT NULL) but the stranded filter joins on core_node_name + payload_code —
// NOT style_id (buckets carry the numeric edge style id, the mirror carries the style
// name) — so its value here is arbitrary.
func insertLinesideBucket(t *testing.T, db *store.DB, node string, styleID int64, payload string, qty int) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO lineside_buckets (station, core_node_name, pair_key, style_id, part_number, qty, payload_code)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		"test-station", node, "PK", styleID, payload, qty, payload,
	); err != nil {
		t.Fatalf("insert lineside bucket %s@%s: %v", payload, node, err)
	}
}

// TestSystemUOPForPayload_ExcludesStrandedBuckets pins the changeover decision
// (2026-07-23): a bucket captured under a PRIOR style that the node's current style no
// longer consumes is STRANDED and must not count toward on-hand — it inflates the
// payload total and suppresses that payload's replenishment (the Springfield 74576
// case: a 250-qty stranded bucket held the total >= threshold so no empty was sent).
// Active lineside still counts, and a bucket at a node with no active-style mirror is
// left counted (exclude only what we can POSITIVELY prove stranded).
func TestSystemUOPForPayload_ExcludesStrandedBuckets(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewInventoryService(db)

	const (
		node       = "ALN-STRAND"
		unmirrored = "ALN-UNMIRRORED"
		active     = "P-ACTIVE"
		stranded   = "P-STRANDED"
	)

	// Mirror: node ALN-STRAND runs an active style that consumes P-ACTIVE, not P-STRANDED.
	if err := plantclaims.ReplaceProcess(db.DB, "PROC-1",
		[]plantclaims.StyleRow{{ProcessID: "PROC-1", StyleID: "STYLE-ACTIVE", ConfigGen: 1, IsActive: true}},
		[]plantclaims.ClaimRow{{
			ProcessID:           "PROC-1",
			StyleID:             "STYLE-ACTIVE",
			CoreNodeName:        node,
			Role:                protocol.ClaimRoleConsume,
			PayloadCode:         active,
			AllowedPayloadCodes: []string{active},
		}}, 0,
	); err != nil {
		t.Fatalf("seed plant claims: %v", err)
	}

	insertLinesideBucket(t, db, node, 1, active, 100)        // active style consumes it -> counts
	insertLinesideBucket(t, db, node, 2, stranded, 250)      // prior style, node dropped it -> excluded
	insertLinesideBucket(t, db, unmirrored, 3, stranded, 30) // node not in the mirror -> counted (safe default)

	res, err := svc.SystemUOPForPayload(context.Background(), []string{active, stranded})
	if err != nil {
		t.Fatalf("SystemUOPForPayload: %v", err)
	}
	got := map[string]int{}
	for _, c := range res.Counts {
		got[c.PayloadCode] = c.BucketUOP
	}
	if got[active] != 100 {
		t.Errorf("active payload bucket UOP = %d, want 100 (active-style bucket counts)", got[active])
	}
	if got[stranded] != 30 {
		t.Errorf("stranded payload bucket UOP = %d, want 30 (250 excluded at the mirrored node, 30 kept at the unmirrored node)", got[stranded])
	}
}
