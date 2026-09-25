//go:build docker

package uop_test

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/service"
	"shingocore/uop"
)

// TestInventoryDelta_LinesideBucketDelta_StyleRestampSplitsTheRow pins today's
// Core key (core_node_name, pair_key, style_id, payload_code) against the
// Edge's re-stamp (FINDINGS-lineside-bucket-drift §2): one Edge pile of one
// part at one seat, captured under style 12 and then re-stamped to style 19,
// lands on Core as TWO rows. The drain under style 19 empties the new row and
// the next drain is refused for a "non-existent bucket" while the style-12 row
// still holds its 100, and the refusal leaves the seq unconsumed so the next
// message at that seq applies.
//
// Flips under brief v7 expected change #1 (one row per (node, part, state), set
// to the Edge's level: no split, no refusal, no orphan).
func TestInventoryDelta_LinesideBucketDelta_StyleRestampSplitsTheRow(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db, service.EpochAnnounce{}), service.EpochAnnounce{})
	node := sd.LineNode.Name
	const part = "PART-RESTAMP"

	rows := func() map[int64]int {
		t.Helper()
		r, err := db.Query(`SELECT style_id, qty FROM lineside_buckets
			WHERE core_node_name=$1 AND payload_code=$2`, node, part)
		testutil.MustNoErr(t, err, "list rows")
		defer r.Close()
		out := map[int64]int{}
		for r.Next() {
			var style int64
			var qty int
			testutil.MustNoErr(t, r.Scan(&style, &qty), "scan")
			out[style] = qty
		}
		testutil.MustNoErr(t, r.Err(), "rows")
		return out
	}

	// The pile's first capture, under style 12.
	testutil.MustNoErr(t, svc.ApplyLinesideBucketDelta(testStation,
		makeBucketDelta(node, "L1|U1", 12, part, 100, 1, protocol.ReasonCaptureFill)), "capture style 12")
	// The same pile, re-stamped to style 19 by the next capture. The scope key
	// carries the style, so this is seq 1 of a new scope.
	testutil.MustNoErr(t, svc.ApplyLinesideBucketDelta(testStation,
		makeBucketDelta(node, "L1|U1", 19, part, 20, 1, protocol.ReasonCaptureFill)), "capture style 19")
	if got := rows(); len(got) != 2 || got[12] != 100 || got[19] != 20 {
		t.Fatalf("rows = %v, want two rows {12:100 19:20} for one Edge pile", got)
	}

	// Drain the style-19 row to zero: it is GC'd, the style-12 row stays.
	testutil.MustNoErr(t, svc.ApplyLinesideBucketDelta(testStation,
		makeBucketDelta(node, "L1|U1", 19, part, -20, 2, protocol.ReasonConsumeDrain)), "drain style 19 to 0")
	if got := rows(); len(got) != 1 || got[12] != 100 {
		t.Fatalf("rows after the drain = %v, want only {12:100}", got)
	}

	// The next drain names style 19, which Core no longer has: refused.
	err := svc.ApplyLinesideBucketDelta(testStation,
		makeBucketDelta(node, "L1|U1", 19, part, -5, 3, protocol.ReasonConsumeDrain))
	if err == nil || !strings.Contains(err.Error(), "non-existent bucket") {
		t.Fatalf("drain of a GC'd style row: err = %v, want the non-existent-bucket refusal", err)
	}
	if got := rows(); len(got) != 1 || got[12] != 100 {
		t.Errorf("rows after the refusal = %v, want {12:100} untouched (Core reads 100 high)", got)
	}

	// The refusal rolled its claim back: seq 3 is still free and applies.
	testutil.MustNoErr(t, svc.ApplyLinesideBucketDelta(testStation,
		makeBucketDelta(node, "L1|U1", 19, part, 10, 3, protocol.ReasonCaptureFill)), "seq 3 after the refusal")
	if got := rows(); len(got) != 2 || got[12] != 100 || got[19] != 10 {
		t.Errorf("rows = %v, want {12:100 19:10} (the refused -5 is not carried: no net on these messages)", got)
	}
}
