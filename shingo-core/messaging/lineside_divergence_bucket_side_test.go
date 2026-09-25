//go:build docker

package messaging

import (
	"fmt"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
)

// plantCoreBucket writes one lineside_buckets row at the rig's seat.
func (r *divergenceRig) plantCoreBucket(station string, state protocol.LinesideBucketState, payload string, qty int) {
	r.t.Helper()
	_, err := r.db.Exec(`INSERT INTO lineside_buckets (station, core_node_name, payload_code, state, qty)
		VALUES ($1, $2, $3, $4, $5)`, station, r.seat.Name, payload, string(state), qty)
	testutil.MustNoErr(r.t, err, "plant core bucket")
}

func bucketRow(node, payload string, qty int) string {
	return fmt.Sprintf(`{"core_node_name":%q,"payload_code":%q,"bin_count":0,"bin_uop":0,"bucket_qty":%d}`, node, payload, qty)
}

// THE BUCKET ARM COMPARES CORE'S ACTIVE PILE, AND IGNORES THE CLAIMS. Core's
// side of a (seat, part) is its active row; a stranded row of the same part is
// not compared (the Edge's report carries active piles only). A part the
// seat's active style does not consume (PART-B) is an active pile and is
// compared.
//
// This was TestLinesideDivergence_BucketArmSumsEveryStyleRowAndIgnoresClaims,
// whose fixture was two style rows for one pile (30 + 10 against an Edge 40).
// FLIPPED BY BRIEF v7 EXPECTED CHANGE #1 as a fixture: one row per (node,
// part, state), so the pile is one active row of 40, and the stranded 10 of
// the same part beside it is the new half of the pin. PART-B stays compared
// (#3: it is an active pile).
func TestLinesideDivergence_BucketArmComparesTheActivePileAndIgnoresClaims(t *testing.T) {
	t.Parallel()
	r := newDivergenceRig(t, "BKTSUM")
	r.plantCoreBucket(r.station, protocol.LinesideBucketActive, "PART-A", 40)
	r.plantCoreBucket(r.station, protocol.LinesideBucketStranded, "PART-A", 10)
	r.plantCoreBucket(r.station, protocol.LinesideBucketActive, "PART-B", 25)

	r.report(bucketRow(r.seat.Name, "PART-A", 40), bucketRow(r.seat.Name, "PART-B", 25))
	if open := r.open(); len(open) != 0 {
		t.Errorf("open divergences = %+v, want none: PART-A active 40 and PART-B 25 both agree", open)
	}

	r.report(bucketRow(r.seat.Name, "PART-A", 40), bucketRow(r.seat.Name, "PART-B", 20))
	r.wantOne("bucket", 0, 20, 25)
}

// THE BUCKET ARM READS THE PILE WHOEVER LAST SENT IT. Core's mirror row is a
// fact about the node; its station is the last reporter, so a row another
// station last sent compares at its qty.
//
// This was TestLinesideDivergence_BucketArmReadsOnlyTheReportingStation
// (`WHERE lb.station = $1`: another station's row compared as 0, and 40 vs 0
// opened a divergence). Flipped by the U4 predicate drop in the checksum's
// bucket arm (store/lineside_divergence.go), not by a numbered change.
func TestLinesideDivergence_BucketArmReadsThePileWhoeverLastSentIt(t *testing.T) {
	t.Parallel()
	r := newDivergenceRig(t, "BKTSTN")
	r.plantCoreBucket("stn-some-other-edge", protocol.LinesideBucketActive, "PART-A", 40)

	r.report(bucketRow(r.seat.Name, "PART-A", 40))
	if open := r.open(); len(open) != 0 {
		t.Errorf("open divergences = %+v, want none: the pile reads 40 whichever station last sent it", open)
	}
}
