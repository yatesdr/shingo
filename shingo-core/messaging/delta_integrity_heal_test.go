//go:build docker

package messaging

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/service"
)

// delta_integrity_heal_test.go — the delta-integrity panel against the running
// net (memory close-out §3, 2026-09-24). It lives here for the divergence rig,
// which applies deltas through the real applier.

// A REFUSED-THEN-LANDED DELTA IS COUNTED AS LOST. Seq 1 (-10) names the wrong
// part and is refused: a payload_mismatch_dropped row of -10. Seq 2 (-5) names
// the right part and carries the scope's net -15; nothing was applied before,
// so Core applies all -15 and its ledger row says healed -10. The count is
// whole. PIN: the panel still reports -10 lost for the refused part.
func TestPin_3_RefusedThenLandedDeltaCountsAsLost(t *testing.T) {
	t.Parallel()
	r := newDivergenceRig(t, "HEALDROP")
	bin, epoch := r.carrier(r.seat.ID, "BIN-DV-HEALDROP", "PART-A", 150)
	svc := service.NewInventoryDeltaService(r.db, service.NewBinManifestService(r.db, service.EpochAnnounce{}), service.EpochAnnounce{})
	apply := func(seq int64, payload string, d int, net int64) error {
		n := net
		return svc.ApplyBinUOPDelta(r.station, &protocol.BinUOPDelta{
			Station: r.station, BinID: bin, PayloadCode: payload, Delta: d,
			Reason: protocol.ReasonConsumeTick, SequenceID: seq, Epoch: epoch, Net: &n,
			WindowStart: r.at, WindowEnd: r.at,
		})
	}
	if err := apply(1, "PART-WRONG", -10, -10); err == nil {
		t.Fatal("seq 1 under the wrong part was applied; the pin needs it refused")
	}
	testutil.MustNoErr(t, apply(2, "PART-A", -5, -15), "apply seq 2")
	if got := r.coreCount(bin); got != 135 {
		t.Fatalf("Core reads %d, want 135: the net must have landed the refused -10", got)
	}

	rows, err := r.db.DeltaIntegrityByPayload(r.at.AddDate(0, 0, -1))
	testutil.MustNoErr(t, err, "delta integrity")
	var lost *int
	for i := range rows {
		if rows[i].PayloadCode == "PART-WRONG" {
			lost = &rows[i].UOPLost
		}
	}
	if lost == nil {
		t.Fatalf("no delta-integrity row for the refused part: %+v", rows)
	}
	if *lost != -10 {
		t.Errorf("uop_lost = %d, want -10 (the base counts the refused delta as lost)", *lost)
	}
}
