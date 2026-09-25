//go:build docker

package messaging

import (
	"fmt"
	"testing"

	"shingo/protocol/testutil"
)

// plantCoreBucket writes one lineside_buckets row at the rig's seat.
func (r *divergenceRig) plantCoreBucket(station string, styleID int64, payload string, qty int) {
	r.t.Helper()
	_, err := r.db.Exec(`INSERT INTO lineside_buckets (station, core_node_name, pair_key, style_id, payload_code, qty)
		VALUES ($1, $2, 'PK', $3, $4, $5)`, station, r.seat.Name, styleID, payload, qty)
	testutil.MustNoErr(r.t, err, "plant core bucket")
}

func bucketRow(node, payload string, qty int) string {
	return fmt.Sprintf(`{"core_node_name":%q,"payload_code":%q,"bin_count":0,"bin_uop":0,"bucket_qty":%d}`, node, payload, qty)
}

// THE BUCKET ARM COMPARES THE SUM OF EVERY STYLE ROW, AND IGNORES THE STRANDED
// RULE. Core's side of a (seat, part) is SUM(qty) over the station's rows, so
// the two rows a style re-stamp leaves (30 + 10) agree with an Edge pile of 40.
// A part the seat's active style does not consume (PART-B: claims-stranded, so
// SystemUOPForPayload leaves it out of on-hand) is still compared here.
//
// Flips under brief v7 expected change #1 as a fixture (two rows for one pile
// cannot exist); the comparison of Core's active qty against the Edge's pile
// stays. PART-B stays compared (it is an active pile after the change, #3).
func TestLinesideDivergence_BucketArmSumsEveryStyleRowAndIgnoresClaims(t *testing.T) {
	t.Parallel()
	r := newDivergenceRig(t, "BKTSUM")
	r.plantCoreBucket(r.station, 12, "PART-A", 30)
	r.plantCoreBucket(r.station, 19, "PART-A", 10)
	r.plantCoreBucket(r.station, 12, "PART-B", 25)

	r.report(bucketRow(r.seat.Name, "PART-A", 40), bucketRow(r.seat.Name, "PART-B", 25))
	if open := r.open(); len(open) != 0 {
		t.Errorf("open divergences = %+v, want none: PART-A 30+10 = 40 and PART-B 25 both agree", open)
	}

	r.report(bucketRow(r.seat.Name, "PART-A", 40), bucketRow(r.seat.Name, "PART-B", 20))
	r.wantOne("bucket", 0, 20, 25)
}

// THE BUCKET ARM READS ONLY THE REPORTING STATION'S ROWS
// (store/lineside_divergence.go, `WHERE lb.station = $1`). A row another
// station last stamped compares as 0.
//
// Flips under brief v7 U4 ("the checksum's bucket arm ... minus the lb.station
// predicate"). Not one of the seven numbered changes: inert while each plant
// runs one station.
func TestLinesideDivergence_BucketArmReadsOnlyTheReportingStation(t *testing.T) {
	t.Parallel()
	r := newDivergenceRig(t, "BKTSTN")
	r.plantCoreBucket("stn-some-other-edge", 12, "PART-A", 40)

	r.report(bucketRow(r.seat.Name, "PART-A", 40))
	r.wantOne("bucket", 0, 40, 0)
}
