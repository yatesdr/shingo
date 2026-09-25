//go:build docker

package uop_test

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
)

// TestInventoryDelta_LinesideBucketLevel_OnePileIsOneRow is the pin that
// used to be TestInventoryDelta_LinesideBucketDelta_StyleRestampSplitsTheRow.
// It pinned Core's old key (core_node_name, pair_key, style_id, payload_code)
// against the Edge's re-stamp (FINDINGS-lineside-bucket-drift §2): one Edge
// pile captured under style 12 and re-stamped to style 19 landed on Core as
// two rows, the style-19 drain emptied the new row, the next drain was refused
// for a "non-existent bucket", and the style-12 row kept its 100, Core reading
// high.
//
// FLIPPED BY BRIEF v7 EXPECTED CHANGE #1. Core's row is keyed as the Edge
// keys the pile, (node, part, state), and is set to the Edge's level: the same
// sequence of Edge rows is one Core row that equals the Edge's after every
// message, with no split, no refusal and no orphan.
func TestInventoryDelta_LinesideBucketLevel_OnePileIsOneRow(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := levelService(db)
	node := sd.LineNode.Name
	const part = "PART-RESTAMP"

	count := func() int {
		t.Helper()
		var n int
		testutil.MustNoErr(t, db.QueryRow(`SELECT count(*) FROM lineside_buckets
			WHERE core_node_name=$1 AND payload_code=$2`, node, part).Scan(&n), "count rows")
		return n
	}

	// The Edge's row after each change: the first capture (100), the next
	// capture that used to re-stamp the style (120), a drain of 100 (20), a
	// drain of 5 (15; the old key refused this one), and a capture (25).
	for i, lv := range []struct{ qty, drained int }{{100, 0}, {120, 0}, {20, 100}, {15, 5}, {25, 0}} {
		testutil.MustNoErr(t, svc.ApplyLinesideBucketLevel(testStation,
			makeBucketLevel(node, part, active, lv.qty, lv.drained, int64(i+1))), "level")
		qty, _, _ := pileRow(t, db, node, part, active)
		if n := count(); n != 1 || qty != lv.qty {
			t.Fatalf("after level %d: %d row(s) at qty %d, want one row at %d (the Edge's row)", i+1, n, qty, lv.qty)
		}
	}
}
