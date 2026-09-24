//go:build docker

package messaging

import (
	"fmt"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/service"
	"shingocore/store/nodes"
)

// closeout_pins_test.go — the memory close-out's owner rulings (2026-09-24),
// pinned at the tree before the change.
//
// 2a: "the edges should always report even if nothing is happening". Today an
// Edge ships no row for a seat with nothing bound and no bucket, and no report
// at all when every seat is like that; Core drops a row with no payload and
// returns on a report with no rows. So a Core carrier at a seat the Edge runs
// empty is invisible unless that seat once reported something, and an episode
// whose seat went empty never closes.
//
// §3: the delta-integrity panel counts a payload-mismatch drop as lost even
// after the running net lands the same units on the next accepted message.

// emptySeatRow is the row an Edge sends for a seat it runs with nothing bound:
// no carrier, no count, no bucket, and no part to report under.
func emptySeatRow(node string) string {
	return fmt.Sprintf(`{"core_node_name":%q,"payload_code":"","bin_count":0,"bin_uop":0,"bucket_qty":0}`, node)
}

// A STAGED, UNBOUND CARRIER AT A SEAT THE EDGE RUNS, AND THE EDGE SAYS THE
// SEAT IS EMPTY. The seat has never been reported with anything on it and the
// plant-claims mirror does not list it, so today nothing makes it one of the
// station's seats: Core drops the row (no payload) and opens nothing. PIN;
// the change opens an empty_seat episode naming the carrier.
func TestPin_2a_CoreCarrierAtASeatTheEdgeReportsEmptyRaisesNothing(t *testing.T) {
	t.Parallel()
	r := newDivergenceRig(t, "EMPTYSEAT")
	seat := &nodes.Node{Name: "ALN_DV_EMPTYSEAT_UNMIRRORED", Enabled: true}
	testutil.MustNoErr(t, r.db.CreateNode(seat), "create unmirrored seat")
	r.carrier(seat.ID, "BIN-DV-EMPTYSEAT", "PART-A", 150)

	r.report(emptySeatRow(seat.Name))
	if open := r.open(); len(open) != 0 {
		t.Errorf("opened %+v; at the base an empty-seat row is dropped and nothing is compared", open)
	}
}

// AN EPISODE WHOSE SEAT NOW REPORTS EMPTY STAYS OPEN. The Edge's bucket at the
// seat disagreed with Core's mirror; the bucket is then used up and the carrier
// taken away, so the seat reports empty. Today that row is dropped, the report
// moves no row, and nothing is compared: the episode stays open for good.
// PIN; the change closes it.
func TestPin_2a_EpisodeStaysOpenWhenItsSeatReportsEmpty(t *testing.T) {
	t.Parallel()
	r := newDivergenceRig(t, "SEATEMPTIES")
	r.report(fmt.Sprintf(`{"core_node_name":%q,"payload_code":"PART-A","bin_count":0,"bin_uop":0,"bucket_qty":40}`, r.seat.Name))
	r.wantOne("bucket", 0, 40, 0)

	r.report(emptySeatRow(r.seat.Name))
	r.wantOne("bucket", 0, 40, 0)
	if got := r.closed(); got != 0 {
		t.Errorf("%d episode(s) closed; at the base an empty-seat report compares nothing", got)
	}
}

// A REPORT WITH NO ROWS IS IGNORED. An Edge that runs no consume seats now says
// so every interval; today the handler returns on it, so the station's open
// episodes stay open. PIN; the change closes them.
func TestPin_2a_ReportWithNoRowsClosesNothing(t *testing.T) {
	t.Parallel()
	r := newDivergenceRig(t, "NOROWS")
	r.report(fmt.Sprintf(`{"core_node_name":%q,"payload_code":"PART-A","bin_count":0,"bin_uop":0,"bucket_qty":40}`, r.seat.Name))
	r.wantOne("bucket", 0, 40, 0)

	r.report()
	r.wantOne("bucket", 0, 40, 0)
}

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
