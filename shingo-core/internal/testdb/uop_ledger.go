package testdb

import (
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingocore/store"
)

// SeedAudit writes one bin_uop_ledger row directly, in the applier's shape: an
// ordinary delta that applied (before != after, reason consume_tick).
func SeedAudit(t *testing.T, db *store.DB, binID int64, before, after int, op, payload string, at time.Time) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO bin_uop_ledger
		(bin_id, before_uop, after_uop, op, source, payload_code, actor, metadata, applied_at)
		VALUES ($1,$2,$3,$4,'test',$5,'test','{"reason":"consume_tick"}',$6)`,
		binID, before, after, op, payload, at)
	testutil.MustNoErr(t, err, "seed audit row")
}
