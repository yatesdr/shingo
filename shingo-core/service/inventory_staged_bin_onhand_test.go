//go:build docker

package service

import (
	"context"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/store/nodes"
)

// TestSystemUOPForPayload_StagedBinCountsTowardOnHand is the Core twin of the
// F1a repro (P2-C1). It pins the on-hand-side half of the SNF3 CARRIER-0024
// incident: a bin parked `staged` at a consuming line still counts in full
// toward SystemUOPForPayload, so a stale staged bin whose real parts have
// already been consumed at the line (but whose Core uop_remaining never moved
// — see the Edge twin) keeps the payload's on-hand total at or above threshold.
// The replenishment monitor's fire gate (`total >= threshold → continue`,
// engine/threshold_monitor.go) then suppresses the empty-to-supermarket signal
// exactly while the physical line is running out.
//
// This is CORRECT behavior on day 0 — a freshly staged bin IS inventory — and
// excluding staged wholesale would under-count every healthy delivery (scope
// §4 F3, explicitly out of scope). So this test documents the trap, it does
// not condemn the SUM. It stays GREEN: the Phase-2 work makes the divergence
// visible and correctable (P2-C3/C5/C6/C7), it does not change what counts as
// on-hand.
func TestSystemUOPForPayload_StagedBinCountsTowardOnHand(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewInventoryService(db)

	const payload = "PART-SNF3"

	// A dumping ground node for the bins to sit on (SystemUOPForPayload applies
	// no node filter — location is irrelevant to on-hand, only status is).
	line := &nodes.Node{Name: "ALN_003", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(line), "create line node")

	// The phantom: CARRIER-0024 staged at the line, Core still holds its
	// delivered snapshot of 150 because no delta ever carried its bin id.
	phantom := createTestBin(t, db, line.ID, "CARRIER-0024", payload, 150)
	if _, err := db.Exec(`UPDATE bins SET status='staged' WHERE id=$1`, phantom.ID); err != nil {
		t.Fatalf("stage phantom bin: %v", err)
	}

	// The real remaining on-hand for this payload: one small available bin (14).
	// Total = 150 + 14 = 164, which is what the monitor sees.
	createTestBin(t, db, line.ID, "CARRIER-REAL", payload, 14)

	res, err := svc.SystemUOPForPayload(context.Background(), []string{payload})
	if err != nil {
		t.Fatalf("SystemUOPForPayload: %v", err)
	}
	if len(res.Counts) != 1 {
		t.Fatalf("counts = %d, want 1", len(res.Counts))
	}
	got := res.Counts[0]

	// The staged 150 is fully included: on-hand reads 164 even though the line
	// physically ran down to ~14. A threshold of 160 would never fire.
	if got.BinUOP != 164 {
		t.Errorf("BinUOP = %d, want 164 (staged CARRIER-0024's 150 counts in full alongside the real 14)",
			got.BinUOP)
	}
	if got.TotalUOP != 164 {
		t.Errorf("TotalUOP = %d, want 164 — the staged phantom keeps on-hand >= a 160 threshold, so the empty-to-SM signal stays suppressed while the line starves",
			got.TotalUOP)
	}
}
