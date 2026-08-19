package schema

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// migrationOnlyTables are the tables a numbered migration creates, which the
// baseline DDL therefore does not declare.
//
// THIS IS NOT A LIST OF BUGS, and the difference matters because it is easy to
// read it as one. A missing CREATE TABLE does not make a fresh install diverge
// from a migrated one: migrate() applies the baseline AND then runs every
// versioned migration, on every open, so both kinds of database end up with
// these tables either way. TestSchemaConvergesAcrossVintages proves that
// property directly, by upgrading an old database and diffing it against a new
// one.
//
// The baseline was frozen at a point in time; everything since arrived as a
// migration. That is the normal shape of this schema, not an accident that
// happened seventeen times.
//
// What the list is for is the NEXT table. Adding one to a migration is fine;
// adding one without noticing which half of the schema it landed in is how a
// reader ends up consulting the constants file and getting an incomplete
// answer. Failing here makes that a decision instead of a side effect.
var migrationOnlyTables = map[string]string{
	"schema_migrations":           "the migration runner's own bookkeeping — it has to exist before a migration can record itself",
	"bin_loader_home_bin_types":   "added by v75 — what each loader window can physically take",
	"bin_loader_homes":            "added by a numbered migration after the baseline was frozen",
	"bin_loader_quotas":           "added by v75 — a loader's declared carrier mix",
	"bin_loader_payloads":         "added by a numbered migration after the baseline was frozen",
	"bin_loaders":                 "added by a numbered migration after the baseline was frozen",
	"bin_uop_audit":               "added by a numbered migration after the baseline was frozen",
	"cell_config":                 "added by a numbered migration after the baseline was frozen",
	"demand_origins":              "added by a numbered migration after the baseline was frozen",
	"node_maintain_levels":        "added by v90 — how many empty carriers of each type a maintained node group is to hold. Separate from bin_loader_quotas on purpose: a quota is a preference bounded by never-2N, a maintained level is the number Core keeps",
	"node_maintain_supports":      "added by v90 — which process nodes a maintained group serves; the resolved node set, because a claim is Edge-local and Core cannot read one",
	"edge_cells":                  "added by a numbered migration after the baseline was frozen",
	"edge_lineside_reports":       "added by a numbered migration after the baseline was frozen",
	"order_bins":                  "added by a numbered migration after the baseline was frozen",
	"process_styles":              "added by a numbered migration after the baseline was frozen",
	"reservations":                "added by a numbered migration after the baseline was frozen",
	"robot_confidence_daily":      "added by v77 — the permanent per-robot residual roll-up",
	"robot_confidence_low":        "added by v77 — the long-retention low-confidence forensic trail",
	"robot_confidence_samples":    "added by v77 — raw localization-confidence samples, daily partitions",
	"lane_confidence_daily":       "added by v80 — the permanent per-LANE roll-up, replacing segment_confidence_daily (which v80 drops: its rows were at the directed-segment granularity, which is half a lane)",
	"lane_robot_confidence_daily": "added by v83 — the permanent per-lane-per-robot grain the map filters by AMR; lane_confidence_daily merges its robots irreversibly, so this carries the intersection",
	"scene_diffs":                 "added by v81 — one row per OBSERVED map edit; the join that relates an RDS /scene edit to a robot .smap edit made in the same sitting",
	"scene_lane_versions":         "added by v81 — per-lane geometry versions, what lane_confidence_daily.version_id points at",
	"scene_map_versions":          "added by v81 — the archived .smap blob, gzipped, scan cloud in its own column",
	"scene_areas":                 "added by v81 — declared map areas, temporally versioned, parsed from the robot .smap",
	"scene_reflectors":            "added by v81 — reflector positions, temporally versioned; identity IS the position",
	"area_confidence_daily":       "added by v82 — the per-ZONE roll-up. One reading can be in several zones (SEER areas overlap), so its samples column does not sum to the plant total",
	"plant_confidence_daily":      "added by v82 — the plant-day record for counts that have no lane to hang on: orphans, unkeyable, unversioned, unattributed",
	"sourceability_events":        "added by a numbered migration after the baseline was frozen",
	"style_claims":                "added by a numbered migration after the baseline was frozen",
	"supply_refusals":             "added by a numbered migration after the baseline was frozen",
}

// TestBaselineDDL_DeclaresEveryTable pins which half of the schema each table
// lives in: declared in the baseline DDL, or named above as migration-created.
//
// The snapshot is the authority for what actually ships — it is generated by
// running the real migrate path against an empty database. A table in the
// snapshot that is in neither the baseline nor the list above is one nobody
// decided about.
func TestBaselineDDL_DeclaresEveryTable(t *testing.T) {
	t.Parallel()
	declared := ddlTables(t)
	shipped := snapshotTables(t)

	if len(shipped) == 0 {
		t.Fatal("parsed zero tables out of the snapshot; the parser is broken, not the schema")
	}

	var missing []string
	for _, tbl := range shipped {
		if declared[tbl] {
			continue
		}
		if _, ok := migrationOnlyTables[tbl]; ok {
			continue
		}
		missing = append(missing, tbl)
	}
	if len(missing) > 0 {
		t.Errorf("tables in the shipped schema that are in neither the baseline DDL nor migrationOnlyTables: %s\n"+
			"Say which half it belongs to: add the CREATE TABLE to postgres_ddl.go, or add the table to migrationOnlyTables naming what creates it.\n"+
			"Either is fine. Not choosing is what this test is here to catch.",
			strings.Join(missing, ", "))
	}

	// The allowlist decays too: an entry naming a table that the baseline has
	// since declared is a claim about the past.
	for tbl := range migrationOnlyTables {
		if declared[tbl] {
			t.Errorf("migrationOnlyTables names %q, but the baseline DDL now declares it — drop the entry", tbl)
		}
	}

	// AND IT DECAYS THE OTHER WAY, which is the direction that actually bit.
	//
	// An entry naming a table that no longer SHIPS is the same stale claim
	// wearing the opposite sign, and the check above cannot see it: it only
	// asks whether the baseline has taken the table over, never whether the
	// table still exists. segment_confidence_daily sat in this list describing
	// "the permanent per-segment roll-up" for the whole of the localization
	// branch, while v80 dropped it — the list read as a decision about a table
	// nobody could load.
	//
	// This is the third guard on this schema found reading the wrong property,
	// and it is here because the fix for the other two would not have caught
	// this one either.
	shipping := make(map[string]bool, len(shipped))
	for _, tbl := range shipped {
		shipping[tbl] = true
	}
	for tbl := range migrationOnlyTables {
		if tbl == "schema_migrations" {
			continue // the runner's own bookkeeping; created outside the dump path
		}
		if !shipping[tbl] {
			t.Errorf("migrationOnlyTables names %q, but no such table is in the shipped schema — "+
				"a migration dropped it and the entry outlived it. Drop the entry.", tbl)
		}
	}

	// THE FOURTH GUARD, added with v92: a table the BASELINE declares that no
	// longer ships. This is the direction that made v92's baseline edit
	// load-bearing — production_log was declared in postgres_ddl.go while a
	// migration dropped it, and neither check above can see that pair, because
	// neither ever asks the baseline about a table the snapshot has lost.
	//
	// The mechanics are the v24/v85 trap: schema.Apply runs the baseline on
	// every startup AHEAD of the versioned migrations, so a baseline CREATE for
	// a dropped table re-creates it every boot, the drop migration's
	// post-condition fails every boot, and the self-heal re-runs the drop every
	// boot. A DROP migration therefore must remove the baseline CREATE in the
	// same change, and this check is what makes forgetting it a test failure
	// instead of a journal line nobody reads.
	for tbl := range declared {
		if !shipping[tbl] {
			t.Errorf("baseline DDL declares %q, but no such table is in the shipped schema — "+
				"a migration dropped it and left the baseline CREATE behind. That CREATE "+
				"re-creates the table on every boot (schema.Apply runs the baseline first), "+
				"so the drop's post-condition fails forever. Remove the CREATE from "+
				"postgres_ddl.go in the same change as the drop.", tbl)
		}
	}
}

var (
	ddlTableRe      = regexp.MustCompile(`(?m)^CREATE TABLE IF NOT EXISTS\s+(\w+)`)
	snapshotTableRe = regexp.MustCompile(`(?m)^CREATE TABLE public\.(\w+)`)
)

// ddlTables returns the table names declared in the baseline DDL constant.
func ddlTables(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, m := range ddlTableRe.FindAllStringSubmatch(postgresDDL, -1) {
		out[m[1]] = true
	}
	if len(out) == 0 {
		t.Fatal("parsed zero tables out of postgresDDL; the parser is broken, not the constant")
	}
	return out
}

// snapshotTables returns the table names in the committed schema snapshot,
// sorted, excluding partitions of declaratively partitioned tables (those are
// created per-range by the partition machinery, not declared).
func snapshotTables(t *testing.T) []string {
	t.Helper()
	const path = "schema.snapshot.sql"
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	seen := map[string]bool{}
	for _, m := range snapshotTableRe.FindAllStringSubmatch(string(b), -1) {
		seen[m[1]] = true
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
