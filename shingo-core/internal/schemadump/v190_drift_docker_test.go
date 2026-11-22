//go:build docker

package schemadump_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"shingocore/internal/schemadump"
	"shingocore/store"
)

// v190Fixtures are the two plants' leftovers (schemadump.DriftA, DriftB), each
// with the drift report's lines for it as the plant's boot logged them, and
// the objects migration 190 changes on it (one audit_log row each). Where a
// leftover table's shape is the plant's own and where it is a stand-in is
// schemadump's to say (drift.go).
var v190Fixtures = []struct {
	name    string
	drift   string
	lines   []string
	changed int
}{
	{"drift-a", schemadump.DriftA, []string{
		"schema drift: column demand_registry.payload_code differs: the database has text NOT NULL, this build ships text DEFAULT ''::text NOT NULL",
		"schema drift: column nodes.node_type_id differs: the database has integer, this build ships bigint",
		"schema drift: column nodes.parent_id differs: the database has integer, this build ships bigint",
		"schema drift: column payloads.bin_id is in the database and not in this build's schema",
		"schema drift: column payloads.id differs: the database has bigint DEFAULT nextval('public.payloads_id_seq1'::regclass) NOT NULL, this build ships bigint DEFAULT nextval('public.payloads_id_seq'::regclass) NOT NULL",
		"schema drift: column payloads.manifest_confirmed is in the database and not in this build's schema",
		"schema drift: constraint nodes.nodes_node_type_id_fkey is in the database and not in this build's schema",
		"schema drift: constraint payloads.payloads_bin_id_fkey1 is in the database and not in this build's schema",
		"schema drift: constraint payloads.payloads_code_key is missing (this build ships it)",
		"schema drift: constraint payloads.payloads_code_key1 is in the database and not in this build's schema",
		"schema drift: constraint payloads.payloads_pkey is missing (this build ships it)",
		"schema drift: constraint payloads.payloads_pkey1 is in the database and not in this build's schema",
		"schema drift: table db_test is in the database and not in this build's schema",
		"schema drift: table payload_instances is in the database and not in this build's schema",
	}, 12},
	{"drift-b", schemadump.DriftB, []string{
		"schema drift: column demand_registry.payload_code differs: the database has text NOT NULL, this build ships text DEFAULT ''::text NOT NULL",
		"schema drift: column orders.deferred_steps_json is in the database and not in this build's schema",
		"schema drift: table lane_access is in the database and not in this build's schema",
	}, 3},
}

// TestV190_TheLeftoversGoAndDownPutsThemBack proves migration 190 on each
// plant's leftovers and on a database born on the baseline:
//
//   - born on the baseline: down 190 recreates nothing, and up again changes
//     nothing (the schema dump is the same at each step, no audit row);
//   - with a plant's leftovers on that database at v189: the drift report
//     reads exactly the lines the plant's boot logged (so the fixture is the
//     plant's); up leaves no drift line and the shape of a database born on
//     the baseline, recording one audit_log row per object it changed; down
//     restores the shape the leftovers had (dump equal, the same drift lines)
//     and no row (db_test comes back empty), and deletes the record; up again
//     is clean once more.
//
// TestSchemaConvergesAcrossVintages walks the same leftovers up from v0.18.0,
// as a plant's database meets this build.
func TestV190_TheLeftoversGoAndDownPutsThemBack(t *testing.T) {
	t.Parallel()
	for _, fx := range v190Fixtures {
		t.Run(fx.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			inst := start(ctx, t)
			name, err := inst.BuildFresh(ctx)
			if err != nil {
				t.Fatal(err)
			}
			cfg := inst.DBConfigForTest(name)
			db := openTest(t, inst, name)
			shape := func(when string) string {
				t.Helper()
				d, err := inst.Dump(ctx, name)
				if err != nil {
					t.Fatalf("%s: dump: %v", when, err)
				}
				return schemadump.Canonical(schemadump.ExcludePartitions(d))
			}
			drift := func(when string) []string {
				t.Helper()
				sdb, err := store.OpenWithoutMigrate(cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer sdb.Close()
				lines, err := sdb.SchemaDrift()
				if err != nil {
					t.Fatalf("%s: drift report: %v", when, err)
				}
				if lines == nil {
					lines = []string{}
				}
				return lines
			}
			recorded := func(when string, want int) {
				t.Helper()
				var n int
				if err := db.QueryRow(`SELECT count(*) FROM audit_log WHERE entity_type = 'schema' AND entity_id = 190`).Scan(&n); err != nil {
					t.Fatal(err)
				}
				if n != want {
					t.Errorf("%s: %d audit_log rows of migration 190, want %d", when, n, want)
				}
			}
			down := func(when string) {
				t.Helper()
				if err := store.MigrateDown(cfg, 190); err != nil {
					t.Fatalf("%s: migrate down 190: %v", when, err)
				}
			}
			up := func(when string) {
				t.Helper()
				if err := inst.Reboot(ctx, name); err != nil {
					t.Fatalf("%s: this build's boot: %v", when, err)
				}
			}

			born := shape("born on the baseline")
			down("born on the baseline")
			if got := shape("down on a baseline-born database"); got != born {
				t.Errorf("down 190 on a database born on the baseline changed its shape:\n%s", shapeDiff(born, got))
			}
			up("baseline-born, up again")
			if got := shape("up again on a baseline-born database"); got != born {
				t.Errorf("up 190 on a database born on the baseline changed its shape:\n%s", shapeDiff(born, got))
			}
			recorded("baseline-born", 0)

			down("before the leftovers")
			if err := inst.ApplyRaw(ctx, name, fx.drift); err != nil {
				t.Fatalf("apply the leftovers: %v", err)
			}
			withDrift := shape("with the leftovers")
			if got := drift("with the leftovers"); !reflect.DeepEqual(got, fx.lines) {
				t.Fatalf("the fixture's drift report is not the plant's:\n got  %s\n want %s",
					strings.Join(got, "\n      "), strings.Join(fx.lines, "\n      "))
			}

			up("the leftovers")
			if got := drift("after up"); len(got) != 0 {
				t.Errorf("drift after up 190:\n  %s", strings.Join(got, "\n  "))
			}
			if got := shape("after up"); got != born {
				t.Errorf("after up 190 the database is not the shape of one born on the baseline:\n%s", shapeDiff(born, got))
			}
			recorded("after up", fx.changed)

			down("the leftovers")
			if got := drift("after down"); !reflect.DeepEqual(got, fx.lines) {
				t.Errorf("drift after down 190 is not the leftovers' again:\n got  %s\n want %s",
					strings.Join(got, "\n      "), strings.Join(fx.lines, "\n      "))
			}
			if got := shape("after down"); got != withDrift {
				t.Errorf("down 190 did not restore the leftovers' shape:\n%s", shapeDiff(withDrift, got))
			}
			recorded("after down", 0)
			if fx.name == "drift-a" {
				var rows int
				if err := db.QueryRow(`SELECT count(*) FROM db_test`).Scan(&rows); err != nil || rows != 0 {
					t.Errorf("db_test after down: %d rows (err %v), want the table back empty (shape, not rows)", rows, err)
				}
			}

			up("again")
			if got := drift("after up again"); len(got) != 0 {
				t.Errorf("drift after up 190 again:\n  %s", strings.Join(got, "\n  "))
			}
			if got := shape("after up again"); got != born {
				t.Errorf("after up 190 again the database is not the baseline's shape:\n%s", shapeDiff(born, got))
			}
		})
	}
}
