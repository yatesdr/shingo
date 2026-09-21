//go:build docker

package store_test

import (
	"database/sql"
	"testing"
	"time"

	"shingocore/internal/testdb"
	"shingocore/store"
)

// The v119 backfill: reason moves from the metadata JSON the applier has
// always written into a real column, once, for recent rows. Pinned at the
// migration (not the rate query) because the backfill is the one step that
// must never silently do nothing — a plant that skips it reads every
// pre-migration tick as reasonless, and the rate filter then counts NONE of
// its history (the safe direction, but a whole window of velocity gone).

// seedV119Delta writes one pre-v119-shaped delta row: reason only in metadata.
func seedV119Delta(t *testing.T, db *store.DB, binID int64, before, after int, payload, metadata string, at time.Time) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO bin_uop_ledger
		(bin_id, before_uop, after_uop, op, source, payload_code, actor, metadata, applied_at)
		VALUES ($1,$2,$3,'bin_uop_delta','test',$4,'test',$5::jsonb,$6)`,
		binID, before, after, payload, metadata, at.Truncate(time.Microsecond)); err != nil {
		t.Fatalf("seed delta row: %v", err)
	}
}

// TestV119_BackfillPromotesReasonFromMetadata seeds pre-119 rows of every
// shape the backfill must distinguish — recent with a reason, recent with NULL
// metadata, recent with no reason key, older-than-7-days with a reason — then
// re-applies v119 through the migrate path (the v94 pattern) and pins which
// rows carry the column and which keep the empty default.
func TestV119_BackfillPromotesReasonFromMetadata(t *testing.T) {
	db, cfg := testdb.OpenWithConfig(t)
	const bin = 9301

	seedV119Delta(t, db, bin, 100, 99, "PART-A", `{"reason":"consume_tick","delta":-1}`, time.Now().Add(-time.Hour))
	seedV119Delta(t, db, bin, 99, 98, "PART-A", `{"reason":"operator_correction","delta":-1}`, time.Now().Add(-2*time.Hour))
	seedV119Delta(t, db, bin, 98, 97, "PART-A", `{"delta":-1}`, time.Now().Add(-3*time.Hour))
	seedV119Delta(t, db, bin, 97, 96, "PART-A", `{"reason":"capture_reduction","delta":-1}`, time.Now().Add(-8*24*time.Hour))

	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version = 119`); err != nil {
		t.Fatalf("clear v119 row: %v", err)
	}
	migrated, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("re-open to apply v119: %v", err)
	}
	defer migrated.Close()

	// Newest-first, so the assertions name their row by the reason it should
	// carry rather than by an incidental applied_at.
	rows, err := migrated.Query(`SELECT reason, metadata->>'reason'
		FROM bin_uop_ledger WHERE bin_id = $1 ORDER BY applied_at DESC`, bin)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	defer rows.Close()
	var got [][2]string
	for rows.Next() {
		var r string
		var m sql.NullString
		if err := rows.Scan(&r, &m); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, [2]string{r, m.String})
	}
	if len(got) != 4 {
		t.Fatalf("read %d rows, want 4", len(got))
	}
	want := [][2]string{
		{"consume_tick", "consume_tick"},               // recent + reason → promoted
		{"operator_correction", "operator_correction"}, // recent + reason → promoted
		{"", ""},                  // no reason key → stays empty
		{"", "capture_reduction"}, // older than 7 days → untouched
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("row %d (reason, metadata.reason) = %q, want %q", i, got[i], w)
		}
	}
}
