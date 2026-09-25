//go:build docker

package sourceability_test

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/store/sourceability"
)

// TestRecordTTESamples_PrunesDrainLedgerPastRetention pins that the drain
// ledger rides pruneTTESamples' sweep: a drain row past the retention horizon
// goes, one inside it stays. The rows come from the real writer
// (ApplyLinesideBucketDelta); only the old row's age is planted.
//
// Stays under the change (brief v7 U4 "Velocity: unchanged"; the ledger loses
// columns, not rows).
func TestRecordTTESamples_PrunesDrainLedgerPastRetention(t *testing.T) {
	w := drainWorld(t)
	node := w.std.LineNode.Name
	applyBucket(t, w, bucketDelta(node, "BIN-A", 50, 1, protocol.ReasonCaptureFill))
	applyBucket(t, w, bucketDelta(node, "BIN-A", -10, 2, protocol.ReasonConsumeDrain))
	applyBucket(t, w, bucketDelta(node, "BIN-A", -5, 3, protocol.ReasonConsumeDrain))

	_, err := w.db.Exec(`UPDATE lineside_drain_ledger SET applied_at = NOW() - interval '100 days'
		WHERE before_qty = 50`)
	testutil.MustNoErr(t, err, "age the first drain row")

	testutil.MustNoErr(t, sourceability.RecordTTESamples(w.db, nil, sourceability.TTERetention), "record/prune")

	if n, before, after := drainRows(t, w); n != 1 || before != 40 || after != 35 {
		t.Errorf("drain ledger = %d row(s) (before=%d after=%d), want only the recent 40->35 row — 100 days is past the horizon",
			n, before, after)
	}
}
