//go:build docker

package uop_test

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"

	"shingocore/internal/testdb"
)

// A RESTORED EDGE'S LEVEL APPLIES AND RE-ANCHORS. The station applied a pile's
// levels at seqs 1-3; then seq 2 arrives with a window that ends after the
// last one applied — an Edge restored from a backup numbering new levels with
// old seqs (SYNTH-round2 S6). The level is the Edge's row as it stands, so it
// applies, and the dedup row moves down to seq 2 and its window so the
// restored Edge's seq 3 applies next instead of being skipped as a duplicate.
//
// This was TestRunningNet_BucketRollbackIsRecordedWithNoBin: a bucket DELTA
// stream that went backward could not say which of its counts Core already
// had, so it was recorded as an edge_rollback exception and not applied. A
// level carries the whole row, so there is nothing to double-count. Flipped by
// brief v7 expected change #1 (Core's mirror equals the Edge's rows after
// every message); the exception row went with AppendBucketUOPException.
func TestBucketLevel_RestoredEdgeAppliesAndReanchors(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := levelService(db)
	node := sd.StorageNode.Name
	const station = "stn-bucket-restore"

	base := time.Now().UTC().Add(-time.Hour)
	mk := func(seq int64, qty int, window time.Time) *protocol.LinesideBucketLevel {
		l := makeBucketLevel(node, "PART-RB", active, qty, 0, seq)
		l.WindowEnd = window
		return l
	}
	for seq := int64(1); seq <= 3; seq++ {
		testutil.MustNoErr(t, svc.ApplyLinesideBucketLevel(station, mk(seq, int(10*seq), base.Add(time.Duration(seq)*time.Minute))), "apply")
	}

	restoredAt := base.Add(10 * time.Minute)
	testutil.MustNoErr(t, svc.ApplyLinesideBucketLevel(station, mk(2, 17, restoredAt)), "restored seq 2")
	if qty, _, _ := pileRow(t, db, node, "PART-RB", active); qty != 17 {
		t.Fatalf("qty = %d, want 17 (the restored Edge's row)", qty)
	}
	var lastSeq int64
	var windowEnd time.Time
	testutil.MustNoErr(t, db.QueryRow(`SELECT last_seq, applied_window_end FROM inventory_delta_dedup
		WHERE station=$1 AND scope_kind=$2`, station, protocol.InvDeltaScopeBucketLevel).Scan(&lastSeq, &windowEnd), "dedup row")
	if lastSeq != 2 || !windowEnd.Equal(restoredAt.Truncate(time.Microsecond)) {
		t.Errorf("dedup = seq %d window %v, want seq 2 window %v (re-anchored)", lastSeq, windowEnd, restoredAt)
	}

	testutil.MustNoErr(t, svc.ApplyLinesideBucketLevel(station, mk(3, 21, restoredAt.Add(time.Minute))), "restored seq 3")
	if qty, _, _ := pileRow(t, db, node, "PART-RB", active); qty != 21 {
		t.Errorf("qty = %d, want 21 (the restored Edge's next level applies)", qty)
	}
}
