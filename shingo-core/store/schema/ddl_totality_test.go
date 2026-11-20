package schema_test

import (
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"shingocore/store"
)

// The baseline (baseline.sql) is what a new database is born on, and the
// snapshot (schema.snapshot.sql) is what a fresh boot of this build ships. Both
// are generated, by different commands (make schema-baseline, then make
// schema-snapshot), so either can be committed stale against the other. The
// docker tests regenerate each; this one reads the two committed files and
// needs no container, so a stale pair fails the plain gate.
//
// It is the DDL-totality test of the replaying schema, rewritten for one home.
// That test asked which half of the schema each table lived in, the baseline
// DDL or a numbered migration, because both ran on every boot and a table the
// baseline still declared after a migration dropped it came back every boot.
// The squash at v183 left one half: the baseline creates every table, and a
// later migration's table reaches the baseline when it is regenerated. What is
// left to decide is that the two files agree.

var (
	createTableRe = regexp.MustCompile(`(?m)^CREATE TABLE public\.(\w+)`)
	partitionRe   = regexp.MustCompile(`(?m)^.*(PARTITION OF |ATTACH PARTITION ).*$`)
	sessionRe     = regexp.MustCompile(`(?m)^(SET |SELECT pg_catalog\.set_config\(|\\(un)?restrict ).*$`)
	migrationRe   = regexp.MustCompile(`(?m)^INSERT INTO public\.schema_migrations VALUES \((\d+), now\(\)\);$`)
)

const regen = "run `make schema-baseline` (go run ./cmd/schemabaseline), then `make schema-snapshot`, and commit both"

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

func tables(t *testing.T, what, sql string) []string {
	t.Helper()
	seen := map[string]bool{}
	for _, m := range createTableRe.FindAllStringSubmatch(sql, -1) {
		seen[m[1]] = true
	}
	if len(seen) == 0 {
		t.Fatalf("parsed zero tables out of %s; the parser is broken, not the schema", what)
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// TestBaseline_CreatesEveryShippedTable: the baseline creates exactly the
// tables the snapshot ships, carries no partition (partitions are made at
// runtime, by store/heartbeat and store/robotconfidence), sets no session
// setting (one would outlive the baseline on the pool connection that ran it),
// and records every version of the retired chain.
func TestBaseline_CreatesEveryShippedTable(t *testing.T) {
	t.Parallel()
	baseline := readFile(t, "baseline.sql")
	snapshot := readFile(t, "schema.snapshot.sql")

	born := map[string]bool{}
	for _, tbl := range tables(t, "baseline.sql", baseline) {
		born[tbl] = true
	}
	shipped := map[string]bool{}
	for _, tbl := range tables(t, "schema.snapshot.sql", snapshot) {
		shipped[tbl] = true
		if !born[tbl] {
			t.Errorf("the snapshot ships table %q and the baseline does not create it: a database born on the baseline "+
				"lacks it until a migration adds it. The baseline is stale: %s.", tbl, regen)
		}
	}
	for tbl := range born {
		if !shipped[tbl] {
			t.Errorf("the baseline creates table %q and the snapshot does not ship it: a new database would carry "+
				"a table no upgraded one has. The baseline is stale: %s.", tbl, regen)
		}
	}

	for _, line := range partitionRe.FindAllString(baseline, -1) {
		t.Errorf("the baseline carries a partition, which runtime code creates and a new database must not be "+
			"born with: %s. Regenerate it (%s); the generator refuses a dump that has one.", line, regen)
	}
	for _, line := range sessionRe.FindAllString(baseline, -1) {
		t.Errorf("the baseline changes a session setting, which would outlive it on the pool connection that "+
			"ran it: %s. Regenerate it (%s); the generator strips these.", line, regen)
	}

	recorded := map[int]bool{}
	for _, m := range migrationRe.FindAllStringSubmatch(baseline, -1) {
		v, _ := strconv.Atoi(m[1])
		recorded[v] = true
	}
	for v := 1; v <= store.RetiredChain; v++ {
		if !recorded[v] {
			t.Errorf("the baseline does not record schema_migrations v%d, so a database born on it is refused at its "+
				"next boot as missing it. Regenerate it: %s.", v, regen)
		}
	}
}

// TestBaselineParsers_MatchTheShapesInUse pins the parsers against the real
// shapes: a regex that quietly stops matching turns the checks above into
// no-ops that always pass.
func TestBaselineParsers_MatchTheShapesInUse(t *testing.T) {
	t.Parallel()
	baseline := readFile(t, "baseline.sql")
	if n := len(createTableRe.FindAllString(baseline, -1)); n < 50 {
		t.Errorf("the table regex matched %d tables in the baseline; it has far more, so the regex has drifted", n)
	}
	if n := len(migrationRe.FindAllString(baseline, -1)); n < store.RetiredChain {
		t.Errorf("the schema_migrations regex matched %d rows; the baseline records %d, so the regex has drifted", n, store.RetiredChain)
	}
	for _, line := range []string{
		"CREATE TABLE public.cell_part_events_2026_06 PARTITION OF public.cell_part_events",
		"ALTER TABLE ONLY public.cell_part_events ATTACH PARTITION public.cell_part_events_2026_06 FOR VALUES FROM ('2026-06-01') TO ('2026-07-01');",
	} {
		if !partitionRe.MatchString(line) {
			t.Errorf("the partition regex does not match %q", line)
		}
	}
	for _, line := range []string{
		"SET statement_timeout = 0;",
		"SELECT pg_catalog.set_config('search_path', '', false);",
		`\restrict abc123`,
	} {
		if !sessionRe.MatchString(line) {
			t.Errorf("the session-setting regex does not match %q", line)
		}
	}
}
