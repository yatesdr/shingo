//go:build docker

package schemadump_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"shingocore/internal/schemadump"
	"shingocore/store"
)

// v185Seed is a v184 database's rows of every class migration 185 moves, and
// of the classes it must leave alone, written with raw SQL against the v184
// names (no Go writer: those are renamed by v185's own commit).
const v185Seed = `
INSERT INTO orders (id, edge_uuid, station_id, projects_to) VALUES
  (1, 'u-edge', 'uid-a', ''),
  (2, 'u-op', 'core-operator', ''),
  (3, 'u-direct', 'core-direct', ''),
  (4, 'u-test', 'core-test', ''),
  (5, '', '', 'uid-b');
INSERT INTO mission_telemetry (order_id, station_id) VALUES (1, 'uid-a'), (2, 'core-operator'), (3, 'core-direct'), (4, 'core-test'), (5, '');
INSERT INTO bin_uop_ledger (id, bin_id, after_uop, op, actor, station) VALUES
  (1, 1, 1, 'bin_uop_delta', 'uid-a', ''),
  (2, 1, 1, 'stale_epoch_dropped', 'uid-a', ''),
  (3, 1, 1, 'payload_bound_first_delta', 'uid-a', ''),
  (4, 1, 1, 'payload_rebound_with_inventory', 'uid-a', ''),
  (5, 1, 1, 'payload_mismatch_dropped', 'uid-a', ''),
  (6, 1, 1, 'after_lift', 'uid-a', ''),
  (7, 1, 1, 'edge_rollback', 'uid-a', 'uid-a'),
  (8, 1, 1, 'lifted', 'system', 'core-operator'),
  (9, 1, 1, 'lifted', 'system', 'uid-a'),
  (10, 1, 1, 'cycle_count', 'alice', ''),
  (11, 1, 1, 'bin_uop_delta', '', '');
INSERT INTO bin_uop_exception (id, kind, op, actor, occurred_at, detail) VALUES
  (1, 'negative_crossing', 'bin_uop_delta', 'uid-a', now(), NULL),
  (2, 'stale_epoch', 'stale_epoch_dropped', 'uid-a', now(), NULL),
  (3, 'payload_mismatch', 'payload_mismatch_dropped', 'uid-a', now(), NULL),
  (4, 'edge_rollback', 'edge_rollback', 'uid-a', now(), NULL),
  (5, 'boundary', 'edge_rollback', 'uid-a', now(), NULL),
  (6, 'report_divergence', 'count_short', 'uid-a', now(), NULL),
  (7, 'boundary', 'cycle_count', 'system', now(), NULL),
  (8, 'negative_crossing', 'cycle_count', 'alice', now(), NULL),
  (9, 'hold_dropped', 'hold_dropped', 'edge:loader', now(), '{"station": "uid-a", "count": 3}');
INSERT INTO audit_log (id, entity_type, entity_id, action, new_value) VALUES
  (1, 'payload', 7, 'floor_create', '{"code":"P1","station":"uid-a"}'),
  (2, 'payload', 7, 'update', '{"station":"uid-a"}'),
  (3, 'payload', 8, 'floor_create', '{"station": "uid-a", broken');
INSERT INTO edge_registry (id, station_uid, station_id) VALUES
  (1, 'uid-r1', 'uid-r1'),
  (2, '', 'uid-r2'),
  (3, 'uid-r4', 'legacy-x'),
  (4, '', 'uid-r4'),
  (5, '', '');
INSERT INTO bin_uop_delta_daily (day, bin_id, epoch_seq, actor, ticks, consumed, added) VALUES ('2026-06-02', 1, 0, 'uid-a', 1, 1, 0);
CREATE TABLE cell_part_events_2026_06 PARTITION OF cell_part_events FOR VALUES FROM ('2026-06-01') TO ('2026-07-01');
INSERT INTO cell_part_events (cell_id, recorded_at, edge_snapshot_id) VALUES ('uid-a', '2026-06-02', 1);
SELECT setval('orders_id_seq', 100), setval('edge_registry_id_seq', 100), setval('bin_uop_ledger_id_seq', 100),
       setval('bin_uop_exception_id_seq', 100), setval('audit_log_id_seq', 100);
`

// v185Want is each row's columns after v185, as one query per check: the
// query's single text column, rows in order, must equal want.
var v185Want = []struct {
	name, query string
	want        []string
}{
	{"orders: the station's uid stays, an actor moves to requested_by", `SELECT id || ':' || station_uid || ':' || requested_by || ':' || projects_to_station_uid FROM orders ORDER BY id`,
		[]string{"1:uid-a::", "2::core-operator:", "3::core-direct:", "4::core-test:", "5:::uid-b"}},
	{"mission_telemetry likewise", `SELECT order_id || ':' || station_uid || ':' || requested_by FROM mission_telemetry ORDER BY order_id`,
		[]string{"1:uid-a:", "2::core-operator", "3::core-direct", "4::core-test", "5::"}},
	{"bin_uop_ledger: the uid out of actor, a Core actor out of station", `SELECT id || ':' || op || ':' || station_uid || ':' || actor FROM bin_uop_ledger ORDER BY id`,
		[]string{"1:bin_uop_delta:uid-a:", "2:stale_epoch_dropped:uid-a:", "3:payload_bound_first_delta:uid-a:",
			"4:payload_rebound_with_inventory:uid-a:", "5:payload_mismatch_dropped:uid-a:", "6:after_lift:uid-a:",
			"7:edge_rollback:uid-a:", "8:lifted::core-operator", "9:lifted:uid-a:system", "10:cycle_count::alice", "11:bin_uop_delta::"}},
	{"bin_uop_exception: the uid out of actor by (kind, op), hold_dropped's out of detail", `SELECT id || ':' || kind || ':' || station_uid || ':' || actor || ':' || COALESCE(detail::text, '') FROM bin_uop_exception ORDER BY id`,
		[]string{"1:negative_crossing:uid-a::", "2:stale_epoch:uid-a::", "3:payload_mismatch:uid-a::", "4:edge_rollback:uid-a::",
			"5:boundary:uid-a::", "6:report_divergence:uid-a::", "7:boundary::system:", "8:negative_crossing::alice:",
			`9:hold_dropped:uid-a:edge:loader:{"count": 3}`}},
	{"audit_log: floor_create's key follows the column, nothing else moves, a row that is not JSON stays as it was", `SELECT id || ':' || new_value FROM audit_log ORDER BY id`,
		[]string{`1:{"code": "P1", "station_uid": "uid-a"}`, `2:{"station":"uid-a"}`, `3:{"station": "uid-a", broken`}},
	{"edge_registry: folded, the collision and the both-blank row gone", `SELECT id || ':' || station_uid FROM edge_registry ORDER BY id`,
		[]string{"1:uid-r1", "2:uid-r2", "3:uid-r4"}},
	{"bin_uop_delta_daily: actor is station_uid", `SELECT station_uid FROM bin_uop_delta_daily`, []string{"uid-a"}},
	{"cell_part_events: the partition's row", `SELECT station_uid FROM cell_part_events`, []string{"uid-a"}},
	{"cell_part_events: the partition's index names, as Postgres names them for the renamed column",
		`SELECT indexname FROM pg_indexes WHERE tablename = 'cell_part_events_2026_06' ORDER BY indexname`,
		[]string{"cell_part_events_2026_06_station_uid_edge_snapshot_id_recor_idx", "cell_part_events_2026_06_station_uid_recorded_at_idx"}},
}

// TestV185_EveryRowLandsPerTheMoveRules builds a v184 database with
// v0.17.1's own boot, seeds rows of every class the move rules name (each
// actor, an Edge uid, blank; every listed op and kind and ops outside them;
// hold_dropped with detail; floor_create audit rows; a registry with a blank
// uid, a fold collision and a both-blank row; a partition), boots this build,
// and reads every row back: each lands per the move rules, no station_uid
// holds an actor, and the CHECKs refuse a bad insert. Then migrate down 185:
// every row as it was but the documented losses, the v184 shape exactly, and
// v0.17.1 boots it twice with every one of its verifies holding.
func TestV185_EveryRowLandsPerTheMoveRules(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	inst := start(ctx, t)

	name := v184Database(ctx, t, inst, root)
	if err := inst.ApplyRaw(ctx, name, v185Seed); err != nil {
		t.Fatalf("seed the v184 rows: %v", err)
	}
	seeded := openTest(t, inst, name)
	original := v184Rows(t, seeded)
	seeded.Close()
	t0 := time.Now()
	if err := inst.Reboot(ctx, name); err != nil {
		t.Fatalf("this build's boot (v185): %v", err)
	}
	t.Logf("v185 up on the seeded database: %v", time.Since(t0).Round(time.Millisecond))

	db := openTest(t, inst, name)
	assertV185Rows(t, db)

	// A re-run moves nothing twice: the runner re-runs v185 when its row is
	// gone (or its verify fails), and every op and move skips what is done.
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version = 185`); err != nil {
		t.Fatal(err)
	}
	if err := inst.Reboot(ctx, name); err != nil {
		t.Fatalf("v185 re-run on its own result: %v", err)
	}
	assertV185Rows(t, db)

	for _, bad := range []struct{ sql, constraint string }{
		{`INSERT INTO orders (edge_uuid, station_uid) VALUES ('bad-1', 'core-test')`, "orders_station_uid_not_actor"},
		{`INSERT INTO orders (edge_uuid, requested_by) VALUES ('bad-2', 'core')`, "orders_requested_by_known"},
		{`INSERT INTO mission_telemetry (order_id, station_uid) VALUES (99, 'core-direct')`, "mission_telemetry_station_uid_not_actor"},
		{`INSERT INTO mission_telemetry (order_id, requested_by) VALUES (98, 'core')`, "mission_telemetry_requested_by_known"},
		{`INSERT INTO orders (edge_uuid, station_uid, requested_by) VALUES ('bad-3', 'uid-a', 'core-test')`, "orders_one_requester"},
		{`INSERT INTO mission_telemetry (order_id, station_uid, requested_by) VALUES (97, 'uid-a', 'core-direct')`, "mission_telemetry_one_requester"},
		{`INSERT INTO edge_registry (station_uid) VALUES ('')`, "edge_registry_station_uid_nonblank"},
		{`INSERT INTO edge_registry (station_uid) VALUES ('uid-r1')`, "edge_registry_station_uid_key"},
	} {
		_, err := db.Exec(bad.sql)
		if err == nil || !strings.Contains(err.Error(), bad.constraint) {
			t.Errorf("%s: err %v, want the refusal of %s", bad.sql, err, bad.constraint)
		}
	}
	var head int
	if err := db.QueryRow(`SELECT max(version) FROM schema_migrations`).Scan(&head); err != nil || head != 185 {
		t.Fatalf("schema_migrations ends at v%d (err %v), want v185", head, err)
	}

	// Down: every row as it was at v184 but the documented losses, the v184
	// shape, and v0.17.1 boots it twice with every one of its verifies holding.
	t0 = time.Now()
	if err := store.MigrateDown(inst.DBConfigForTest(name), 185); err != nil {
		t.Fatalf("migrate down 185: %v", err)
	}
	t.Logf("v185 down on the seeded database: %v", time.Since(t0).Round(time.Millisecond))
	downed := v184Rows(t, db)
	for table, want := range original {
		if got := downed[table]; got != want {
			t.Errorf("%s after down:\n got  %s\n want %s", table, got, want)
		}
	}
	shape, err := inst.Dump(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	ref := v184Database(ctx, t, inst, root)
	refShape, err := inst.Dump(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if a, b := schemadump.Canonical(schemadump.ExcludePartitions(shape)), schemadump.Canonical(schemadump.ExcludePartitions(refShape)); a != b {
		t.Errorf("the database down 185 left is not the v184 shape:\n%s", shapeDiff(b, a))
	}
	rel, err := schemadump.BuildRelease(ctx, root, store.RetiredChainRelease, releaseWork)
	if err != nil {
		t.Fatal(err)
	}
	for boot := 1; boot <= 2; boot++ {
		out, err := inst.BootRelease(ctx, rel, name)
		if err != nil {
			t.Fatalf("%s boot %d after down: %v", store.RetiredChainRelease, boot, err)
		}
		if strings.Contains(out, "post-condition fails") {
			t.Errorf("%s boot %d after down re-ran a migration whose verify failed:\n%s", store.RetiredChainRelease, boot, out)
		}
	}
}

// v184Rows reads the seeded tables on their v184 names, one string per table,
// with the documented losses of a round trip through v185 masked: the rows the
// registry fold deleted (ids 4 and 5) and the uid it gave row 2, row 3's
// station_id where it was not its uid (down fills the column from station_uid),
// and audit_log.new_value's spelling (compared as jsonb).
func v184Rows(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	out := map[string]string{}
	for table, q := range map[string]string{
		"orders":              `SELECT id || ':' || station_id || ':' || projects_to FROM orders ORDER BY id`,
		"mission_telemetry":   `SELECT order_id || ':' || station_id FROM mission_telemetry ORDER BY order_id`,
		"bin_uop_ledger":      `SELECT id || ':' || op || ':' || station || ':' || actor FROM bin_uop_ledger ORDER BY id`,
		"bin_uop_exception":   `SELECT id || ':' || kind || ':' || op || ':' || actor || ':' || COALESCE(detail::text, '') FROM bin_uop_exception ORDER BY id`,
		"audit_log":           `SELECT id || ':' || CASE WHEN id = 3 THEN new_value ELSE (new_value::jsonb)::text END FROM audit_log ORDER BY id`,
		"edge_registry":       `SELECT id || ':' || CASE id WHEN 2 THEN '(folded)' WHEN 3 THEN '(selector restored as the uid)' ELSE station_id || ':' || station_uid END FROM edge_registry WHERE id NOT IN (4, 5) ORDER BY id`,
		"bin_uop_delta_daily": `SELECT actor FROM bin_uop_delta_daily`,
		"cell_part_events":    `SELECT cell_id FROM cell_part_events UNION ALL (SELECT indexname FROM pg_indexes WHERE tablename = 'cell_part_events_2026_06' ORDER BY indexname)`,
	} {
		rows, err := db.Query(q)
		if err != nil {
			t.Fatalf("read %s: %v", table, err)
		}
		var got []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			got = append(got, s)
		}
		rows.Close()
		out[table] = strings.Join(got, " | ")
	}
	return out
}

func assertV185Rows(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, w := range v185Want {
		rows, err := db.Query(w.query)
		if err != nil {
			t.Errorf("%s: %v", w.name, err)
			continue
		}
		var got []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			got = append(got, s)
		}
		rows.Close()
		if strings.Join(got, "\n") != strings.Join(w.want, "\n") {
			t.Errorf("%s:\n got  %q\n want %q", w.name, got, w.want)
		}
	}
}

// v184Database is an empty database booted by store.RetiredChainRelease:
// schema v184, the shape a plant's database has when this build first meets
// it.
func v184Database(ctx context.Context, t *testing.T, inst *schemadump.Instance, root string) string {
	t.Helper()
	name, err := inst.CreateDatabase("v184")
	if err != nil {
		t.Fatal(err)
	}
	bootRelease(ctx, t, inst, root, store.RetiredChainRelease, name, "the v184 database v185 starts from")
	return name
}

func openTest(t *testing.T, inst *schemadump.Instance, name string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", inst.DSNForTest(name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
