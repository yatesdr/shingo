//go:build docker

package store_test

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/schema"
)

// TestV144_ProcessStylesStationID: process_styles gains a nullable station_id —
// the station that published the process's claims, where the sourceability feed
// sends its verdicts. Existing rows keep NULL (the station is unknown until the
// Edge next publishes), and NULL means "every station", as before.
func TestV144_ProcessStylesStationID(t *testing.T) {
	t.Parallel()
	db, cfg := testdb.OpenWithConfig(t)
	if !schema.ColumnExists(db.DB, "process_styles", "station_id") {
		t.Fatal("process_styles.station_id missing")
	}

	// Back to the pre-v144 shape, with a row written by an old Core.
	if _, err := db.Exec(`ALTER TABLE process_styles DROP COLUMN station_id`); err != nil {
		t.Fatalf("drop station_id: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO process_styles (process_id, style_id) VALUES ('OLD-P', 'S1')`); err != nil {
		t.Fatalf("seed a pre-v144 row: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version = 144`); err != nil {
		t.Fatalf("forget v144: %v", err)
	}
	migrated, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("re-run migrations: %v", err)
	}
	defer migrated.Close()

	var station *string
	if err := migrated.DB.QueryRow(`SELECT station_id FROM process_styles WHERE process_id = 'OLD-P'`).Scan(&station); err != nil {
		t.Fatalf("read the pre-v144 row: %v", err)
	}
	if station != nil {
		t.Errorf("the pre-v144 row has station %q, want NULL (unknown until its Edge republishes)", *station)
	}
}
