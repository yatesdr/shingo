//go:build docker

package store_test

import (
	"database/sql"
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/schema"
)

// v131Shape reports the parts of v131's shape a test checks.
func v131Shape(t *testing.T, q schema.Querier) (state, styleGone, pairGone, key, ledgerTrimmed bool) {
	t.Helper()
	state = schema.ColumnExists(q, "lineside_buckets", "state")
	styleGone = schema.ColumnAbsent(q, "lineside_buckets", "style_id")
	pairGone = schema.ColumnAbsent(q, "lineside_buckets", "pair_key")
	if err := q.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = $1
		AND pg_get_constraintdef(oid) = 'UNIQUE (core_node_name, payload_code, state)')`,
		store.LinesideBucketsUniqueConstraintV131).Scan(&key); err != nil {
		t.Fatalf("probe key: %v", err)
	}
	ledgerTrimmed = true
	for _, c := range []string{"station", "pair_key", "style_id", "reason"} {
		if !schema.ColumnAbsent(q, "lineside_drain_ledger", c) {
			ledgerTrimmed = false
		}
	}
	return
}

// TestV131_PileMirrorShape: a migrated database holds the level wire's mirror
// — state in, style and pair out, keyed (core_node_name, payload_code, state),
// the state CHECK refusing anything but the two states — and the drain ledger
// without its four unread columns. v132 dropped demand_origins.used_edge_reports.
func TestV131_PileMirrorShape(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	state, styleGone, pairGone, key, ledgerTrimmed := v131Shape(t, db.DB)
	if !state || !styleGone || !pairGone || !key || !ledgerTrimmed {
		t.Errorf("shape: state=%v style gone=%v pair gone=%v key=%v ledger trimmed=%v, want all true",
			state, styleGone, pairGone, key, ledgerTrimmed)
	}
	if _, err := db.Exec(`INSERT INTO lineside_buckets (station, core_node_name, payload_code, state, qty)
		VALUES ('S', 'N', 'P', 'inactive', 1)`); err == nil {
		t.Error("state 'inactive' was accepted; the CHECK allows only active and stranded")
	}
	if !schema.ColumnAbsent(db.DB, "demand_origins", "used_edge_reports") {
		t.Error("demand_origins.used_edge_reports survived v132")
	}
}

// TestV131_TruncatesAndReshapesAPlantDatabase: v131 run over the pre-v131
// shape, holding delta-fed rows. The mirror is emptied and reshaped (the Edge
// re-seeds it at boot), the retired "bucket" dedup rows go while a bin's stay,
// the drain ledger loses its columns and keeps its rows, and an open bucket
// report_divergence episode closes with the reason while another class stays
// open.
func TestV131_TruncatesAndReshapesAPlantDatabase(t *testing.T) {
	t.Parallel()
	db, cfg := testdb.OpenWithConfig(t)
	exec := func(stmt string, args ...any) {
		t.Helper()
		if _, err := db.Exec(stmt, args...); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	// Back to the pre-v131 shape, with rows in it.
	exec(`ALTER TABLE lineside_buckets DROP CONSTRAINT ` + store.LinesideBucketsUniqueConstraintV131)
	exec(`ALTER TABLE lineside_buckets DROP COLUMN state,
		ADD COLUMN pair_key TEXT NOT NULL DEFAULT '', ADD COLUMN style_id BIGINT NOT NULL DEFAULT 0`)
	exec(`ALTER TABLE lineside_buckets ADD CONSTRAINT ` + store.LinesideBucketsUniqueConstraintV105 +
		` UNIQUE (core_node_name, pair_key, style_id, payload_code)`)
	exec(`INSERT INTO lineside_buckets (station, core_node_name, pair_key, style_id, payload_code, qty)
		VALUES ('ALN', 'ALN_001', 'PK', 12, 'PART-A', 125), ('ALN', 'ALN_001', 'PK', 19, 'PART-A', 715)`)
	exec(`ALTER TABLE lineside_drain_ledger ADD COLUMN station TEXT NOT NULL DEFAULT 'ALN',
		ADD COLUMN pair_key TEXT NOT NULL DEFAULT 'PK', ADD COLUMN style_id BIGINT NOT NULL DEFAULT 19,
		ADD COLUMN reason TEXT NOT NULL DEFAULT 'consume_drain'`)
	exec(`INSERT INTO lineside_drain_ledger (node_id, payload_code, before_qty, after_qty) VALUES (1, 'PART-A', 10, 9)`)
	exec(`INSERT INTO inventory_delta_dedup (station, scope_kind, scope_key, last_seq)
		VALUES ('ALN', 'bucket', 'ALN_001|PK|19|PART-A', 42), ('ALN', 'bin', '7', 5)`)
	exec(`INSERT INTO bin_uop_exception (kind, bin_id, payload_code, actor, occurred_at, op, detail)
		VALUES ('report_divergence', NULL, 'PART-A', 'ALN', NOW(), 'bucket', '{"key":"bucket|ALN_001|PART-A|"}'),
		       ('report_divergence', NULL, 'PART-A', 'ALN', NOW(), 'count', '{"key":"count|ALN_001|PART-A|7"}')`)
	exec(`DELETE FROM schema_migrations WHERE version = 131`)

	migrated, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("re-open to apply v131 over the pre-v131 shape: %v", err)
	}
	defer migrated.Close()
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := migrated.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}

	if n := count(`SELECT count(*) FROM lineside_buckets`); n != 0 {
		t.Errorf("lineside_buckets holds %d row(s) after v131, want 0 (truncated; the Edge re-seeds it)", n)
	}
	state, styleGone, pairGone, key, ledgerTrimmed := v131Shape(t, migrated.DB)
	if !state || !styleGone || !pairGone || !key || !ledgerTrimmed {
		t.Errorf("shape: state=%v style gone=%v pair gone=%v key=%v ledger trimmed=%v, want all true",
			state, styleGone, pairGone, key, ledgerTrimmed)
	}
	if n := count(`SELECT count(*) FROM lineside_drain_ledger`); n != 1 {
		t.Errorf("drain ledger rows = %d, want 1 (columns go, rows stay)", n)
	}
	if n := count(`SELECT count(*) FROM inventory_delta_dedup WHERE scope_kind = 'bucket'`); n != 0 {
		t.Errorf("%d retired bucket dedup row(s) survived, want 0", n)
	}
	if n := count(`SELECT count(*) FROM inventory_delta_dedup WHERE scope_kind = 'bin'`); n != 1 {
		t.Errorf("bin dedup rows = %d, want 1 (untouched)", n)
	}

	var recovered sql.NullTime
	var reason sql.NullString
	if err := migrated.QueryRow(`SELECT recovered_at, detail->>'closed_reason' FROM bin_uop_exception
		WHERE kind = 'report_divergence' AND op = 'bucket'`).Scan(&recovered, &reason); err != nil {
		t.Fatalf("read bucket episode: %v", err)
	}
	if !recovered.Valid || reason.String == "" {
		t.Errorf("bucket episode: recovered=%v reason=%q, want closed with a reason", recovered, reason.String)
	}
	if n := count(`SELECT count(*) FROM bin_uop_exception
		WHERE kind = 'report_divergence' AND op = 'count' AND recovered_at IS NULL`); n != 1 {
		t.Errorf("open count episodes = %d, want 1 (v131 closes bucket episodes only)", n)
	}
}
