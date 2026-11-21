//go:build docker

package messaging

import (
	"reflect"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/domain"
	"shingocore/service"
	"shingocore/store/audit"
)

// delta_integrity_heal_test.go — the delta-integrity panel against the running
// net (memory close-out §3, 2026-09-24). It lives here for the divergence rig,
// which applies deltas through the real applier.

// A REFUSED-THEN-LANDED DELTA IS RECOVERED, NOT LOST. Seq 1 (-10) names the
// wrong part and is refused: a payload_mismatch_dropped row of -10. Seq 2 (-5)
// names the right part and carries the scope's net -15; nothing was applied
// before, so Core applies all -15 and its ledger row says healed -10. The panel
// shows the -10 as recovered, and 0 lost. Inverts the pin that counted it lost.
func TestCloseout_3_RefusedThenLandedDeltaIsRecovered(t *testing.T) {
	t.Parallel()
	r := newDivergenceRig(t, "HEALDROP")
	bin, epoch := r.carrier(r.seat.ID, "BIN-DV-HEALDROP", "PART-A", 150)
	svc := service.NewInventoryDeltaService(r.db, service.NewBinManifestService(r.db, service.EpochAnnounce{}), service.EpochAnnounce{})
	apply := func(seq int64, payload string, d int, net int64) error {
		n := net
		return svc.ApplyBinUOPDelta(r.station, &protocol.BinUOPDelta{
			BinID: bin, PayloadCode: payload, Delta: d,
			Reason: protocol.ReasonConsumeTick, SequenceID: seq, Epoch: epoch, Net: &n,
			WindowStart: r.at, WindowEnd: r.at,
		})
	}
	if err := apply(1, "PART-WRONG", -10, -10); err == nil {
		t.Fatal("seq 1 under the wrong part was applied; the case needs it refused")
	}
	testutil.MustNoErr(t, apply(2, "PART-A", -5, -15), "apply seq 2")
	if got := r.coreCount(bin); got != 135 {
		t.Fatalf("Core reads %d, want 135: the net must have landed the refused -10", got)
	}

	rows, err := r.db.DeltaIntegrityByPayload(r.at.AddDate(0, 0, -1))
	testutil.MustNoErr(t, err, "delta integrity")
	var got *domain.DeltaIntegrity
	for i := range rows {
		if rows[i].PayloadCode == "PART-WRONG" {
			got = &rows[i]
		}
	}
	if got == nil {
		t.Fatalf("no delta-integrity row for the refused part: %+v", rows)
	}
	if got.UOPLost != 0 || got.UOPRecovered != -10 {
		t.Errorf("uop_lost = %d, uop_recovered = %d; want 0 lost and -10 recovered", got.UOPLost, got.UOPRecovered)
	}
	if got.DropRows != 1 || got.PayloadMismatchRows != 1 {
		t.Errorf("drop_rows = %d, payload_mismatch_rows = %d; the drop itself still happened and is still counted",
			got.DropRows, got.PayloadMismatchRows)
	}
}

// TestCycleTime_PinACycleIsTheSendingStations pins the station the cycle-time
// page attributes each applied tick to (audit.ListCycleEvents, the read
// AuditService makes, keyed by domain.BuildCycleSeries): the station on the
// count message's envelope, taken through the real door (HandleBinUOPDelta);
// the body names no station. Two Edges tick two carriers at one
// seat. Ticks come only from an Edge's counts, never from an order, so the
// five order kinds of the actor split do not apply: no Core actor and no
// Core-born path sends a BinUOPDelta.
//
// Red when a tick is attributed to anything but its envelope's station (the
// read taking a column the station has left), or when a station's ticks are
// lost.
func TestCycleTime_PinACycleIsTheSendingStations(t *testing.T) {
	t.Parallel()
	r := newDivergenceRig(t, "CYCLE")
	since := time.Now().Add(-time.Hour)
	first, second := r.station, "stn-bbbb2222"
	binA, epochA := r.carrier(r.seat.ID, "BIN-DV-CYCLE-A", "PART-A", 150)
	binB, epochB := r.carrier(r.seat.ID, "BIN-DV-CYCLE-B", "PART-A", 150)
	tick := func(station string, bin, epoch, seq int64) {
		t.Helper()
		net := -seq
		r.svc.HandleBinUOPDelta(linesideEnvelope(station), &protocol.BinUOPDelta{
			BinID: bin, PayloadCode: "PART-A", Delta: -1,
			Reason: protocol.ReasonConsumeTick, SequenceID: seq, Epoch: epoch, Net: &net,
			WindowStart: r.at, WindowEnd: r.at,
		})
	}
	for seq := int64(1); seq <= 3; seq++ {
		tick(first, binA, epochA, seq)
	}
	for seq := int64(1); seq <= 2; seq++ {
		tick(second, binB, epochB, seq)
	}
	if got := r.coreCount(binA) + r.coreCount(binB); got != 295 {
		t.Fatalf("fixture: Core reads %d on the two carriers, want 295 (every tick applied)", got)
	}

	events, _, err := audit.ListCycleEvents(r.db.DB, since, 500)
	testutil.MustNoErr(t, err, "list cycle events")
	got := map[string]int{}
	for _, e := range events {
		got[e.Station]++
	}
	if want := map[string]int{first: 3, second: 2}; !reflect.DeepEqual(got, want) {
		t.Errorf("cycle ticks by station = %v, want %v", got, want)
	}
	series, unattributable := domain.BuildCycleSeries(events)
	gaps := map[string]int{}
	for _, s := range series {
		gaps[s.Key.Station] = len(s.Gaps)
	}
	if want := map[string]int{first: 2, second: 1}; !reflect.DeepEqual(gaps, want) || unattributable != 0 {
		t.Errorf("cycle series gaps by station = %v (unattributable %d), want %v and none", gaps, unattributable, want)
	}
}
