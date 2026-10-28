package store

import (
	"path/filepath"
	"testing"
)

// Edge v31 (drop_core_loader_uop_threshold) and v32
// (demand_origins_open_reorder_point) on the shape a plant Edge has the day they
// ship: the Core loader cache carrying uop_threshold, and an open cell episode
// whose level sits in a column named threshold. The threshold is Core's alone;
// the level a cell episode was decided at is the Edge's reorder point.

// plantPreSplit puts an opened database back to the pre-v31 shape, with an open
// episode at level 50, and forgets v31 and v32 so the next boot runs them.
func plantPreSplit(t *testing.T, db *DB) {
	t.Helper()
	for _, stmt := range []string{
		`ALTER TABLE core_loader_positions ADD COLUMN uop_threshold INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE core_loader_payloads ADD COLUMN uop_threshold INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE demand_origins_open RENAME COLUMN reorder_point TO threshold`,
		`INSERT INTO demand_origins_open (episode_key, origin_id, kind, threshold) VALUES ('cell|P|PART|consume', 'o-1', 'cell', 50)`,
		`DELETE FROM schema_migrations WHERE version IN (31, 32)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("plant pre-split shape (%s): %v", stmt, err)
		}
	}
}

func TestV31V32SplitThresholdFromReorderPoint(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "edge.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	plantPreSplit(t, db)
	db.Close()

	up, err := Open(path)
	if err != nil {
		t.Fatalf("boot after the plant shape: %v", err)
	}
	defer up.Close()
	for _, table := range []string{"core_loader_positions", "core_loader_payloads"} {
		if n := columnCount(t, up, table, "uop_threshold"); n != 0 {
			t.Errorf("%s still has uop_threshold after v31: the threshold is Core's alone", table)
		}
	}
	if n := columnCount(t, up, "demand_origins_open", "threshold"); n != 0 {
		t.Error("demand_origins_open still has threshold after v32")
	}
	open, err := up.GetOpenDemandOrigin("cell|P|PART|consume")
	if err != nil || open == nil {
		t.Fatalf("read the open episode after v32: %v, %v", open, err)
	}
	if open.ReorderPoint != 50 {
		t.Errorf("open episode reorder_point = %d, want 50 carried over from threshold", open.ReorderPoint)
	}
}

// A fresh database already has the final shape from the DDL; v31 and v32 change
// nothing on it.
func TestV31V32NoOpOnFreshDB(t *testing.T) {
	t.Parallel()
	db, err := Open(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	for _, c := range []struct {
		table, column string
		want          int
	}{
		{"core_loader_positions", "uop_threshold", 0},
		{"core_loader_payloads", "uop_threshold", 0},
		{"demand_origins_open", "threshold", 0},
		{"demand_origins_open", "reorder_point", 1},
	} {
		if n := columnCount(t, db, c.table, c.column); n != c.want {
			t.Errorf("fresh %s.%s count = %d, want %d", c.table, c.column, n, c.want)
		}
	}
}
