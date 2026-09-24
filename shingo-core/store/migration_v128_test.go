//go:build docker

package store_test

import (
	"testing"
	"time"

	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/schema"
)

func v128Shape(t *testing.T, q schema.Querier) (index bool, dedup bool, payload bool) {
	t.Helper()
	if err := q.QueryRow(`SELECT EXISTS (
		SELECT 1 FROM pg_index i
		JOIN pg_class c ON c.oid = i.indexrelid
		JOIN pg_class p ON p.oid = i.indrelid
		WHERE p.relname = 'cell_part_events' AND c.relname = 'uq_cell_part_events_tick' AND i.indisunique)`).Scan(&index); err != nil {
		t.Fatalf("probe index: %v", err)
	}
	if err := q.QueryRow(`SELECT to_regclass('production_tick_dedup') IS NOT NULL`).Scan(&dedup); err != nil {
		t.Fatalf("probe dedup table: %v", err)
	}
	payload = schema.ColumnExists(q, "cell_part_events", "payload_code")
	return
}

// TestV128_TickKeyOnCellPartEvents: the dedup key moves onto cell_part_events
// as a unique index on the partitioned parent (legal because it carries the
// partition key, recorded_at), the side table production_tick_dedup is gone,
// and so is the never-written payload_code column.
func TestV128_TickKeyOnCellPartEvents(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	index, dedup, payload := v128Shape(t, db.DB)
	if !index {
		t.Error("unique index uq_cell_part_events_tick missing on cell_part_events")
	}
	if dedup {
		t.Error("production_tick_dedup still exists")
	}
	if payload {
		t.Error("cell_part_events.payload_code still exists")
	}
}

// TestV128_ToleratesADuplicateTick: a plant database holding two rows with the
// same (cell_id, edge_snapshot_id, recorded_at) must not stop Core booting on
// the index build. The migration keeps the lowest id of each duplicate group
// and builds the index.
func TestV128_ToleratesADuplicateTick(t *testing.T) {
	t.Parallel()
	db, cfg := testdb.OpenWithConfig(t)
	at := time.Now().UTC().Truncate(time.Second)
	if err := db.EnsureHeartbeatPartitions(at); err != nil {
		t.Fatal(err)
	}
	// Back to the pre-v128 shape: no unique index.
	if _, err := db.Exec(`DROP INDEX IF EXISTS uq_cell_part_events_tick`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := db.Exec(`INSERT INTO cell_part_events (cell_id, recorded_at, edge_snapshot_id, delta)
			VALUES ('stn-dup', $1, 9, 1)`, at); err != nil {
			t.Fatalf("seed duplicate %d: %v", i, err)
		}
	}
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version = 128`); err != nil {
		t.Fatal(err)
	}
	migrated, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("re-open to apply v128 over a duplicate: %v", err)
	}
	defer migrated.Close()
	var n int
	if err := migrated.QueryRow(`SELECT count(*) FROM cell_part_events WHERE cell_id='stn-dup'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("rows after v128 = %d, want 1 (duplicate collapsed)", n)
	}
	if index, _, _ := v128Shape(t, migrated.DB); !index {
		t.Error("index not built over the collapsed rows")
	}
}
