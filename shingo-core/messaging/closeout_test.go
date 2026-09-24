//go:build docker

package messaging

import (
	"fmt"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/store/nodes"
)

// closeout_test.go — the memory close-out's owner rulings (2026-09-24).
//
// 2a: "the edges should always report even if nothing is happening". The Edge
// reports every seat it runs, an empty one as a row with nothing bound, and
// sends a report every interval even when it has no rows. So a Core carrier at
// a seat the Edge says is empty is caught from the report itself, with no
// station-to-seat map, and an episode closes when its seat reports empty.
// Before the ruling the Edge left an empty seat out and sent nothing when all
// were empty, and Core dropped a row with no payload and returned on a report
// with no rows (the pins these tests inverted).

// emptySeatRow is the row an Edge sends for a seat it runs with nothing bound:
// no carrier, no count, no bucket, and no part to report under.
func emptySeatRow(node string) string {
	return fmt.Sprintf(`{"core_node_name":%q,"payload_code":"","bin_count":0,"bin_uop":0,"bucket_qty":0}`, node)
}

// A STAGED, UNBOUND CARRIER AT A SEAT THE EDGE REPORTS EMPTY opens an
// empty_seat episode naming the carrier. The seat has never been reported with
// anything on it and the plant-claims mirror does not list it: the report
// naming it is what makes it one of the station's seats. Inverts the pin that
// nothing was raised.
func TestCloseout_2a_CoreCarrierAtASeatTheEdgeReportsEmptyIsAnAnomaly(t *testing.T) {
	t.Parallel()
	r := newDivergenceRig(t, "EMPTYSEAT")
	seat := &nodes.Node{Name: "ALN_DV_EMPTYSEAT_UNMIRRORED", Enabled: true}
	testutil.MustNoErr(t, r.db.CreateNode(seat), "create unmirrored seat")
	bin, _ := r.carrier(seat.ID, "BIN-DV-EMPTYSEAT", "PART-A", 150)

	r.report(emptySeatRow(seat.Name))
	d := r.wantOne("empty_seat", bin, -1, 150)
	if d.node != seat.Name {
		t.Errorf("episode names seat %q, want %q", d.node, seat.Name)
	}
	if d.edgeCount.Valid {
		t.Errorf("edge_count = %d, want none: the Edge has no carrier there", d.edgeCount.Int64)
	}
}

// AN EPISODE CLOSES WHEN ITS SEAT REPORTS EMPTY. The Edge's bucket at the seat
// disagreed with Core's mirror; the bucket is used up and the seat reports
// empty, and the episode is recovered. Inverts the pin that it stayed open.
func TestCloseout_2a_EpisodeClosesWhenItsSeatReportsEmpty(t *testing.T) {
	t.Parallel()
	r := newDivergenceRig(t, "SEATEMPTIES")
	r.report(fmt.Sprintf(`{"core_node_name":%q,"payload_code":"PART-A","bin_count":0,"bin_uop":0,"bucket_qty":40}`, r.seat.Name))
	r.wantOne("bucket", 0, 40, 0)

	r.report(emptySeatRow(r.seat.Name))
	if open := r.open(); len(open) != 0 {
		t.Errorf("still open after the seat reported empty: %+v", open)
	}
	if got := r.closed(); got != 1 {
		t.Errorf("%d recovered episode(s), want 1", got)
	}
}

// A REPORT WITH NO ROWS CLOSES THE STATION'S EPISODES. An Edge that runs no
// consume seats says so every interval. Inverts the pin that it was ignored.
func TestCloseout_2a_ReportWithNoRowsClosesTheStationsEpisodes(t *testing.T) {
	t.Parallel()
	r := newDivergenceRig(t, "NOROWS")
	r.report(fmt.Sprintf(`{"core_node_name":%q,"payload_code":"PART-A","bin_count":0,"bin_uop":0,"bucket_qty":40}`, r.seat.Name))
	r.wantOne("bucket", 0, 40, 0)

	r.report()
	if open := r.open(); len(open) != 0 {
		t.Errorf("still open after a report with no rows: %+v", open)
	}
}

// A REPORT WITH NO ROWS THAT IS OLDER THAN THE STATION'S LATEST CHANGES
// NOTHING. It moves no row, so the latest-wins upsert cannot tell it is stale;
// the handler compares it only when it is newer than every row the station has
// stored. A late or redelivered empty report must not close what a current one
// found. Green before and after: before, a report with no rows was ignored.
func TestCloseout_2a_StaleReportWithNoRowsChangesNothing(t *testing.T) {
	t.Parallel()
	r := newDivergenceRig(t, "STALENOROWS")
	r.report(fmt.Sprintf(`{"core_node_name":%q,"payload_code":"PART-A","bin_count":0,"bin_uop":0,"bucket_qty":40}`, r.seat.Name))
	r.wantOne("bucket", 0, 40, 0)

	r.redeliver()
	r.wantOne("bucket", 0, 40, 0)
}
