//go:build docker

package service

import (
	"context"
	"testing"

	"shingo/protocol"
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

// A STAGED CARRIER THAT TAKES COUNTS IS BOUND AND CONSUMING, NOT A PHANTOM.
// PIN (round-1 seat-count S0 P0b), green before and after lane A of the memory
// build. Core's `staged` at a seat is the ordinary state of a carrier the Edge
// has bound: its accepted deltas land on bins.uop_remaining, and the total the
// threshold monitor decides off moves with them. Lane A makes that total the
// only one every fire path reads, so this is the property the decision stands
// on; round 1 found all six staged carriers at Springfield's seats in exactly
// this state.
func TestSystemUOPForPayload_StagedCarrierTakesAcceptedDeltas(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewInventoryService(db)
	deltas := NewInventoryDeltaService(db, NewBinManifestService(db, EpochAnnounce{}), EpochAnnounce{})

	const payload = "PART-STAGED-CONSUMING"
	line := &nodes.Node{Name: "ALN_P0B", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(line), "create line node")
	carrier := createTestBin(t, db, line.ID, "CARRIER-P0B", payload, 150)
	if _, err := db.Exec(`UPDATE bins SET status='staged' WHERE id=$1`, carrier.ID); err != nil {
		t.Fatalf("stage carrier: %v", err)
	}
	var epoch int64
	testutil.MustNoErr(t, db.QueryRow(`SELECT delta_epoch FROM bins WHERE id=$1`, carrier.ID).Scan(&epoch), "read epoch")

	testutil.MustNoErr(t, deltas.ApplyBinUOPDelta("stn-p0b", &protocol.BinUOPDelta{
		Station: "stn-p0b", BinID: carrier.ID, PayloadCode: payload, Delta: -10,
		Reason: protocol.ReasonConsumeTick, SequenceID: 1, Epoch: epoch,
	}), "apply consume delta to the staged carrier")

	var remaining int
	testutil.MustNoErr(t, db.QueryRow(`SELECT uop_remaining FROM bins WHERE id=$1`, carrier.ID).Scan(&remaining), "read uop")
	if remaining != 140 {
		t.Errorf("staged carrier uop_remaining = %d after a -10 delta, want 140", remaining)
	}
	res, err := svc.SystemUOPForPayload(context.Background(), []string{payload})
	testutil.MustNoErr(t, err, "SystemUOPForPayload")
	if len(res.Counts) != 1 || res.Counts[0].TotalUOP != 140 {
		t.Errorf("SystemUOPForPayload = %+v, want a total of 140 — the staged carrier counts, at its consumed level", res.Counts)
	}
}
