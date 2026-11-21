//go:build docker

package store_test

import (
	"slices"
	"strings"
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/schema"
)

// TestSchemaDrift_ADatabaseBornOnTheBaselineHasNone: the boot's drift report
// finds nothing on a database this build made. A report that found something
// here would be a parser of the snapshot or of the catalog that disagrees with
// pg_dump, and every plant would wear its warning.
func TestSchemaDrift_ADatabaseBornOnTheBaselineHasNone(t *testing.T) {
	t.Parallel()
	_, cfg := testdb.OpenWithConfig(t)
	db, err := store.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if drift := db.DriftAtBoot(); len(drift) != 0 {
		t.Fatalf("a database born on the baseline drifts:\n  %s", strings.Join(drift, "\n  "))
	}
}

// TestSchemaDrift_NamesAHandDroppedIndexAnAddedColumnAndAMissingTable: each
// hand change to the schema is exactly one line of the boot's report, naming
// its object, and the boot goes on. A dropped table is one line, not one per
// column it took with it.
func TestSchemaDrift_NamesAHandDroppedIndexAnAddedColumnAndAMissingTable(t *testing.T) {
	t.Parallel()
	raw, cfg := testdb.OpenWithConfig(t)
	for _, q := range []string{
		`DROP INDEX idx_orders_status`,
		`ALTER TABLE orders ADD COLUMN zz_hand_added integer`,
		`DROP TABLE tte_samples`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	db, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("the boot refused a drifted database; the report refuses nothing: %v", err)
	}
	defer db.Close()
	want := []string{
		"schema drift: column orders.zz_hand_added is in the database and not in this build's schema",
		"schema drift: index idx_orders_status is missing (this build ships it)",
		"schema drift: table tte_samples is missing (this build ships it)",
	}
	if got := db.DriftAtBoot(); !slices.Equal(got, want) {
		t.Fatalf("the boot's drift report:\n  got  %q\n  want %q", got, want)
	}
}

// TestV185_AHandDroppedIndexIsDriftNotARerun: v185's verify asserts only what
// a re-run of v185 restores, so an index dropped by hand after it ran does not
// make the next boot re-run it (which would take every table's lock and still
// not bring the index back, every boot). The boot's drift report names it.
func TestV185_AHandDroppedIndexIsDriftNotARerun(t *testing.T) {
	t.Parallel()
	raw, cfg := testdb.OpenWithConfig(t)
	var before string
	if err := raw.QueryRow(`SELECT applied_at::text FROM schema_migrations WHERE version = 185`).Scan(&before); err != nil {
		t.Fatalf("read v185's row: %v", err)
	}
	if _, err := raw.Exec(`DROP INDEX idx_mission_telemetry_station_uid`); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var after string
	if err := raw.QueryRow(`SELECT applied_at::text FROM schema_migrations WHERE version = 185`).Scan(&after); err != nil {
		t.Fatalf("read v185's row after the boot: %v", err)
	}
	if after != before {
		t.Errorf("the boot re-ran v185 over a dropped index (applied_at %s -> %s): its verify asserts what a re-run cannot restore", before, after)
	}
	want := "schema drift: index idx_mission_telemetry_station_uid is missing (this build ships it)"
	if got := db.DriftAtBoot(); !slices.Equal(got, []string{want}) {
		t.Errorf("the boot's drift report %q, want only %q", got, want)
	}
}

// TestSchemaPredicates_ReadSchemaPublicOnly: a same-named copy of a v184 table
// in another schema (CREATE TABLE pre185.orders AS TABLE public.orders, made
// before the deploy) is not Core's. v185's verify reads public's columns and
// tables only, so the next two boots do not re-run v185 over it (a re-run
// would rename public.orders.station_id, which is gone, and fail every boot);
// a forced re-run leaves the copies alone, a partitioned copy's index
// included; and the drift report, which reads public, names none of it.
func TestSchemaPredicates_ReadSchemaPublicOnly(t *testing.T) {
	t.Parallel()
	raw, cfg := testdb.OpenWithConfig(t)
	const copyIndex = "cell_part_events_2026_06_cell_id_recorded_at_idx"
	for _, q := range []string{
		`CREATE SCHEMA pre185`,
		`CREATE TABLE pre185.orders (id bigint, station_id text, projects_to text)`,
		`CREATE TABLE pre185.edge_registry (id bigint, station_uid text, station_id text CONSTRAINT edge_registry_station_id_key UNIQUE)`,
		`CREATE TABLE pre185.cell_targets (cell_id text)`,
		`CREATE TABLE pre185.downtime_events (id bigint)`,
		`CREATE TABLE pre185.cell_part_events (station_uid text, recorded_at timestamptz) PARTITION BY RANGE (recorded_at)`,
		`CREATE TABLE pre185.cell_part_events_2026_06 PARTITION OF pre185.cell_part_events FOR VALUES FROM ('2026-06-01') TO ('2026-07-01')`,
		`CREATE INDEX ` + copyIndex + ` ON pre185.cell_part_events_2026_06 (station_uid, recorded_at)`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for _, c := range []struct {
		name string
		got  bool
	}{
		{"ColumnExists(orders, station_id)", schema.ColumnExists(raw, "orders", "station_id")},
		{"!ColumnAbsent(orders, station_id)", !schema.ColumnAbsent(raw, "orders", "station_id")},
		{"TableExists(cell_targets)", schema.TableExists(raw, "cell_targets")},
		{"!TableAbsent(downtime_events)", !schema.TableAbsent(raw, "downtime_events")},
		{"IndexExists(" + copyIndex + ")", schema.IndexExists(raw, copyIndex)},
		{"!IndexAbsent(" + copyIndex + ")", !schema.IndexAbsent(raw, copyIndex)},
		{"ConstraintExists(edge_registry, edge_registry_station_id_key)", schema.ConstraintExists(raw, "edge_registry", "edge_registry_station_id_key")},
	} {
		if c.got {
			t.Errorf("%s is true: it read schema pre185's copy as Core's", c.name)
		}
	}

	appliedAt := func() string {
		t.Helper()
		var at string
		if err := raw.QueryRow(`SELECT applied_at::text FROM schema_migrations WHERE version = 185`).Scan(&at); err != nil {
			t.Fatalf("read v185's row: %v", err)
		}
		return at
	}
	before := appliedAt()
	for boot := 1; boot <= 2; boot++ {
		db, err := store.Open(cfg)
		if err != nil {
			t.Fatalf("boot %d beside schema pre185: %v", boot, err)
		}
		drift := db.DriftAtBoot()
		db.Close()
		if after := appliedAt(); after != before {
			t.Fatalf("boot %d re-ran v185 (applied_at %s -> %s): its verify read schema pre185's copy", boot, before, after)
		}
		if len(drift) != 0 {
			t.Errorf("boot %d's drift report names another schema's objects:\n  %s", boot, strings.Join(drift, "\n  "))
		}
	}

	// A forced re-run (the row gone, as the runner's self-heal leaves it)
	// changes nothing outside public.
	if _, err := raw.Exec(`DELETE FROM schema_migrations WHERE version = 185`); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("v185 re-run beside schema pre185: %v", err)
	}
	db.Close()
	var copyCol, copyIdx bool
	if err := raw.QueryRow(`SELECT
		EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'pre185' AND table_name = 'orders' AND column_name = 'station_id'),
		EXISTS (SELECT 1 FROM pg_indexes WHERE schemaname = 'pre185' AND indexname = $1)`, copyIndex).Scan(&copyCol, &copyIdx); err != nil {
		t.Fatal(err)
	}
	if !copyCol || !copyIdx {
		t.Errorf("the re-run changed schema pre185: orders.station_id there %v, index %s there %v; want both", copyCol, copyIndex, copyIdx)
	}
}
