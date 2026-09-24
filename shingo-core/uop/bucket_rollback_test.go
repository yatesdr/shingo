//go:build docker

package uop_test

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"

	"shingocore/internal/testdb"
	"shingocore/uop"
)

// A BUCKET STREAM THAT WENT BACKWARD IS RECORDED. The station applied bucket
// seqs 1-3; then seq 2 arrives again with a window that ends after the last one
// applied — an Edge restored from a backup counting the same seqs again
// (SYNTH-round2 S6). For a bin that is an edge_rollback exception and a new
// generation; a bucket has no generation, so it is the exception only, with no
// bin: payload, station and node on the row and in its detail. The message is
// not applied either way.
//
// Verify-red at the base (lane N's applier): the skip was returned as "not
// recorded: bucket scopes have no exception row", because bin_uop_exception.
// bin_id was NOT NULL until migration 127.
func TestRunningNet_BucketRollbackIsRecordedWithNoBin(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := netTestService(db)
	node := sd.StorageNode.Name
	const station = "stn-bucket-rollback"

	base := time.Now().UTC().Add(-time.Hour)
	mk := func(seq int64, window time.Time) *protocol.LinesideBucketDelta {
		d := makeBucketDelta(node, "L1|U1", 100, "PART-RB", 10, seq, protocol.ReasonCaptureFill)
		net := 10 * seq
		d.Net = &net
		d.WindowStart, d.WindowEnd = window.Add(-5*time.Second), window
		return d
	}
	for seq := int64(1); seq <= 3; seq++ {
		testutil.MustNoErr(t, svc.ApplyLinesideBucketDelta(station, mk(seq, base.Add(time.Duration(seq)*time.Minute))), "apply")
	}

	err := svc.ApplyLinesideBucketDelta(station, mk(2, base.Add(10*time.Minute)))
	if !errors.Is(err, uop.ErrInventoryDeltaSkipped) {
		t.Fatalf("err = %v, want ErrInventoryDeltaSkipped (a rollback is not applied)", err)
	}

	var (
		bin              sql.NullInt64
		payload, actor   string
		op, detailNode   string
		detailSeq, lastQ int64
	)
	if err := db.QueryRow(`SELECT bin_id, payload_code, actor, op,
		       detail->>'core_node_name', (detail->>'sequence_id')::bigint, (detail->>'last_seq')::bigint
		FROM bin_uop_exception WHERE kind = 'edge_rollback' AND actor = $1`, station).
		Scan(&bin, &payload, &actor, &op, &detailNode, &detailSeq, &lastQ); err != nil {
		t.Fatalf("no edge_rollback exception for the bucket stream: %v", err)
	}
	if bin.Valid || payload != "PART-RB" || op != "edge_rollback" || detailNode != node || detailSeq != 2 || lastQ != 3 {
		t.Errorf("exception = bin %v payload %q op %q node %q seq %d last %d; want no bin, PART-RB, edge_rollback, %s, 2, 3",
			bin, payload, op, detailNode, detailSeq, lastQ, node)
	}
	var qty int
	testutil.MustNoErr(t, db.QueryRow(`SELECT qty FROM lineside_buckets WHERE core_node_name=$1 AND payload_code='PART-RB'`, node).Scan(&qty), "read bucket")
	if qty != 30 {
		t.Errorf("bucket qty = %d, want 30 (the rolled-back message is not applied)", qty)
	}
}
