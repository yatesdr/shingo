package store

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Every index a fresh boot ships is one a new database is born with, and the
// other way round.
//
// WHAT THIS USED TO GUARD, AND WHY THAT HAZARD IS GONE. The replaying schema
// applied its baseline DDL on every boot, before the migrations, and CREATE
// TABLE IF NOT EXISTS added no column to a table that already existed. So a
// baseline CREATE INDEX over a column a migration added ran, on every existing
// database, before that migration, and failed the whole schema apply: Core did
// not start. That happened deploying migration 55 to the houseserver sim on
// 2026-07-25, with every test green, because every test built a fresh
// database. This file read the upgrade order (the baseline DDL against
// migrations.go and the pre-baseline column list) to catch it.
//
// Since the squash at v184 the baseline meets only a database with no tables
// (store.migrate), so no baseline statement ever runs ahead of a migration on
// an old database. What can still go wrong is the baseline going stale against
// the snapshot: a migration above the retired chain that adds an index, with
// the snapshot regenerated and the baseline not, gives new databases a
// different index set from upgraded ones. The docker tests regenerate each
// file; this reads the two committed files and needs no container.

var (
	// CREATE [UNIQUE] INDEX <name> ON [ONLY] public.<table> ... ; as pg_dump
	// writes it, one line per statement.
	dumpIndexRe = regexp.MustCompile(`(?m)^CREATE (?:UNIQUE )?INDEX (\w+) ON (?:ONLY )?public\.\w+ .*;$`)
)

func mustReadSource(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// dumpIndexes maps each index's name to its whole statement.
func dumpIndexes(sql string) map[string]string {
	out := map[string]string{}
	for _, m := range dumpIndexRe.FindAllStringSubmatch(sql, -1) {
		out[m[1]] = m[0]
	}
	return out
}

func TestBaseline_CreatesEveryShippedIndex(t *testing.T) {
	t.Parallel()
	born := dumpIndexes(mustReadSource(t, "schema/baseline.sql"))
	shipped := dumpIndexes(mustReadSource(t, "schema/schema.snapshot.sql"))
	if len(shipped) == 0 || len(born) == 0 {
		t.Fatalf("parsed %d indexes from the snapshot and %d from the baseline; a parser that matches nothing "+
			"is worse than no test", len(shipped), len(born))
	}
	const regen = "Regenerate the baseline: `make schema-baseline` (go run ./cmd/schemabaseline), then `make schema-snapshot`."
	var problems []string
	for name, stmt := range shipped {
		switch b, ok := born[name]; {
		case !ok:
			problems = append(problems, "the snapshot ships "+name+" and the baseline does not create it")
		case b != stmt:
			problems = append(problems, "the baseline creates "+name+" differently:\n    baseline: "+b+"\n    snapshot: "+stmt)
		}
	}
	for name := range born {
		if _, ok := shipped[name]; !ok {
			problems = append(problems, "the baseline creates "+name+" and the snapshot does not ship it")
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Fatalf("a database born on the baseline would not carry the indexes a fresh boot ships:\n  %s\n\n%s",
			strings.Join(problems, "\n  "), regen)
	}
}

// The parser is the load-bearing part: a regex that quietly stops matching
// turns the check above into a no-op that always passes.
func TestSchemaDriftParsers_MatchTheShapesInUse(t *testing.T) {
	t.Parallel()
	if n := len(dumpIndexes(mustReadSource(t, "schema/schema.snapshot.sql"))); n < 50 {
		t.Errorf("the index regex matched %d indexes in the snapshot; it has far more, so it has drifted", n)
	}
	for _, sample := range []string{
		"CREATE INDEX idx_order_history_code ON public.order_history USING btree (code, created_at) WHERE (code IS NOT NULL);",
		"CREATE UNIQUE INDEX edge_registry_station_uid_key ON public.edge_registry USING btree (station_uid) WHERE (station_uid <> ''::text);",
		"CREATE INDEX idx_cell_part_events_cell_time ON ONLY public.cell_part_events USING btree (cell_id, recorded_at);",
	} {
		if len(dumpIndexes(sample)) != 1 {
			t.Errorf("the index regex did not parse %q", sample)
		}
	}
}
