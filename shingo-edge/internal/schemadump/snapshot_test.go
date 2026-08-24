package schemadump_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"shingoedge/internal/schemadump"
)

// moduleRoot is shingo-edge/, two levels up from internal/schemadump.
func moduleRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

// TestSchemaSnapshotIsCurrent builds the schema from scratch and compares it
// to the committed snapshot.
//
// The snapshot is the only file in this module that IS the schema. It is worth
// nothing if it drifts, and nobody will remember to regenerate it — so this
// test remembers, and it fails with the exact command that fixes it.
//
// Unlike Core's, this needs no Docker: SQLite is a file, so it runs in the
// default test suite where it will actually be seen.
func TestSchemaSnapshotIsCurrent(t *testing.T) {
	root := moduleRoot(t)
	path := filepath.Join(root, filepath.FromSlash(schemadump.SnapshotPath))

	committed, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v\n\nrun `%s` and commit the result", schemadump.SnapshotPath, err, schemadump.RegenCommand)
	}

	built := buildFresh(t)
	want := strings.ReplaceAll(string(committed), "\r\n", "\n")
	if built == want {
		return
	}
	t.Fatalf("schema snapshot is stale — run `%s` and commit the result\n\nfirst difference:\n%s",
		schemadump.RegenCommand, firstDiff(want, built, "committed", "built"))
}

// TestSchemaConvergesAcrossVintages builds the database two ways and asserts
// they describe the same shape:
//
//	fresh — today's baseline + the whole migrate path
//	aged  — an OLD baseline (from git) + the whole migrate path
//
// schema.Apply runs CREATE TABLE IF NOT EXISTS, which does nothing to a table
// that already exists. So a column added to sqlite_ddl.go's CREATE TABLE
// reaches a fresh edge instantly and a plant edge NEVER — and every other test
// in this module builds a fresh edge. Edge's escape hatch is the idempotent
// ALTER ADD COLUMN pass in migrations.go, and until now nothing checked that a
// new baseline column had an entry there.
//
// A vintage that fails for a reason you cannot explain is a FINDING, not an
// obstacle: it means an edge at that vintage genuinely gets a different
// database from a new one.
func TestSchemaConvergesAcrossVintages(t *testing.T) {
	root := filepath.Dir(moduleRoot(t)) // repo root — git needs the work tree
	fresh := buildFresh(t)

	// Every known divergence must still be observed by the end of the run. An
	// allowlist that silently keeps covering a problem somebody already fixed
	// is how allowlists rot.
	unseen := map[string]string{}
	for _, kd := range schemadump.KnownDivergences {
		unseen[kd.Key] = kd.Why
	}

	for _, v := range schemadump.Vintages {
		t.Run(strings.NewReplacer("^", "-parent", "/", "-", ".", "_").Replace(v.Rev), func(t *testing.T) {
			baseline, err := schemadump.BaselineFromGit(root, v.Rev)
			if err != nil {
				t.Fatalf("read baseline at %s: %v\n(%s)", v.Rev, err, v.Why)
			}
			dir := t.TempDir()
			path, err := schemadump.BuildAged(dir, baseline)
			if err != nil {
				t.Fatalf("upgrade a %s edge database to today: %v\n(%s)", v.Rev, err, v.Why)
			}
			aged, err := schemadump.Dump(path)
			schemadump.RemoveDB(path)
			if err != nil {
				t.Fatalf("dump aged database: %v", err)
			}

			// Canonical, not literal — column ORDER and the stored CREATE
			// text legitimately differ. See schemadump.Canonical.
			var unexplained []string
			for _, d := range shapeDiff(schemadump.Canonical(fresh), schemadump.Canonical(aged)) {
				if _, known := unseen[d]; known {
					delete(unseen, d)
					continue
				}
				if isKnown(d) {
					continue // already accounted for by an earlier vintage
				}
				unexplained = append(unexplained, d)
			}
			if len(unexplained) == 0 {
				return
			}
			t.Fatalf("an edge database created at %s and upgraded to today does NOT match a fresh one,\n"+
				"and this difference is not one of the recorded ones.\n"+
				"vintage: %s\n\n%s\n\n"+
				"This is the shape that reaches a fresh install and never reaches a plant:\n"+
				"schema.Apply's CREATE TABLE IF NOT EXISTS does nothing to a table that already\n"+
				"exists. If the change is a new column, it needs an idempotent ALTER ADD COLUMN\n"+
				"in store/migrations.go too.",
				v.Rev, v.Why, "  "+strings.Join(unexplained, "\n  "))
		})
	}

	// ONLY IF EVERY VINTAGE ACTUALLY RAN. A vintage that could not be built
	// observed nothing, so "this recorded divergence no longer occurs" would be
	// a statement about a comparison that never happened — and it fired exactly
	// that way against a shallow CI clone, burying the real failure under four
	// confident and wrong ones.
	//
	// An allowlist check is only meaningful when the thing it allows had a
	// chance to appear.
	if t.Failed() {
		t.Log("skipping the stale-entry check: at least one vintage did not run, so nothing was observed")
		return
	}
	for key, why := range unseen {
		t.Errorf("recorded divergence no longer occurs — delete it from KnownDivergences:\n  %s\n  (%s)", key, why)
	}
}

func isKnown(diff string) bool {
	for _, kd := range schemadump.KnownDivergences {
		if kd.Key == diff {
			return true
		}
	}
	return false
}

func buildFresh(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path, err := schemadump.BuildFresh(dir)
	if err != nil {
		t.Fatalf("build fresh database: %v", err)
	}
	dump, err := schemadump.Dump(path)
	schemadump.RemoveDB(path)
	if err != nil {
		t.Fatalf("dump fresh database: %v", err)
	}
	return dump
}

// shapeDiff reports the divergence PER OBJECT, and for a table present on both
// sides, per column.
//
// A whole-statement diff of a 40-column CREATE TABLE prints two 2,000-character
// lines and leaves the reader to spot the one word that changed. What the
// convergence test has to answer is "which column, and how" — the difference
// between an unreadable failure and an actionable one.
func shapeDiff(fresh, aged string) []string {
	f, a := objects(fresh), objects(aged)
	names := map[string]bool{}
	for n := range f {
		names[n] = true
	}
	for n := range a {
		names[n] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)

	var out []string
	for _, n := range sorted {
		fv, inF := f[n]
		av, inA := a[n]
		switch {
		case inF && !inA:
			out = append(out, n+": absent from the upgraded database")
		case !inF && inA:
			out = append(out, n+": present in the upgraded database only")
		case fv == av:
		default:
			for _, line := range memberDiff(fv, av) {
				out = append(out, n+": "+line)
			}
		}
	}
	return out
}

var objectHead = regexp.MustCompile(`(?is)^(CREATE\s+(?:UNIQUE\s+)?(TABLE|INDEX|VIEW|TRIGGER)\s+(?:IF\s+NOT\s+EXISTS\s+)?["\x60\[]?(\w+)["\x60\]]?)`)

// objects keys each canonical statement by "<kind> <name>".
func objects(canonical string) map[string]string {
	out := map[string]string{}
	for stmt := range strings.SplitSeq(canonical, "\n") {
		s := strings.TrimSpace(stmt)
		if s == "" {
			continue
		}
		m := objectHead.FindStringSubmatch(s)
		if m == nil {
			out[s] = s
			continue
		}
		out[strings.ToLower(m[2])+" "+m[3]] = s
	}
	return out
}

// memberDiff compares the comma-separated members of two canonical CREATE
// statements — for a table, its columns and constraints.
func memberDiff(fresh, aged string) []string {
	fm, am := members(fresh), members(aged)
	seen := map[string]int{}
	for _, m := range fm {
		seen[m]++
	}
	var only []string
	for _, m := range am {
		if seen[m] > 0 {
			seen[m]--
			continue
		}
		only = append(only, "upgraded only: "+m)
	}
	var missing []string
	for m, n := range seen {
		for range n {
			missing = append(missing, "fresh only: "+m)
		}
	}
	sort.Strings(missing)
	sort.Strings(only)
	return append(missing, only...)
}

func members(stmt string) []string {
	open := strings.Index(stmt, "(")
	closeIdx := strings.LastIndex(stmt, ")")
	if open < 0 || closeIdx <= open {
		return []string{stmt}
	}
	raw := strings.Split(stmt[open+1:closeIdx], ", ")
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		if t := strings.TrimSpace(r); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// firstDiff renders the first divergence with a little context.
func firstDiff(a, b, aLabel, bLabel string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	n := min(len(al), len(bl))
	for i := range n {
		if al[i] == bl[i] {
			continue
		}
		var sb strings.Builder
		for j := max(0, i-3); j < i; j++ {
			sb.WriteString("  " + al[j] + "\n")
		}
		sb.WriteString("- " + aLabel + ": " + al[i] + "\n")
		sb.WriteString("+ " + bLabel + ": " + bl[i] + "\n")
		return sb.String()
	}
	return "(one is a prefix of the other)"
}

// ---------------------------------------------------------------------------
// The two-statement drift guard.
// ---------------------------------------------------------------------------

// alterCreateDivergence is one recorded difference between how migrate()'s
// ALTER pass declares a column and how the baseline CREATE declares it.
type alterCreateDivergence struct {
	// Key is "table.column".
	Key string
	// Why is what was found when it was examined. As with KnownDivergences, an
	// entry without this is indistinguishable from someone silencing a failure.
	Why string
}

// recordedAlterDrift is the state of the world on 2026-08-24, when the private
// styleNodeClaimsColumnGuards list was deleted and this assertion replaced it.
//
// NOTHING NEW GOES IN THIS LIST. Both entries change the value an existing
// plant row would get, so fixing either is a DATA decision rather than a
// mechanism one, and neither is urgent — see each Why.
var recordedAlterDrift = []alterCreateDivergence{
	{
		Key: "style_node_claims.swap_mode",
		Why: "ALTER declares DEFAULT 'simple', the baseline CREATE declares no default. " +
			"Harmless today only because this column's default is what triggers the v33 " +
			"rebuild, which then normalises it away — the one column whose drift repairs " +
			"itself. Every writer names the column, so the default is reachable only by a " +
			"hand-written INSERT that omits it.",
	},
	{
		Key: "core_loaders.loader_key",
		Why: "ALTER declares DEFAULT '', the baseline CREATE declares no default. The cache " +
			"is dropped and rebuilt full-state on the next node-list sync when the legacy " +
			"key shape is detected (hasLegacyLoaderCacheKey), so an upgraded row carrying " +
			"the ALTER's default is replaced rather than read.",
	},
}

// TestAlterPassMatchesBaselineCreate asserts that where migrate()'s idempotent
// ALTER pass and the baseline CREATE TABLE both declare the same column, they
// declare it IDENTICALLY — type, constraints and DEFAULT.
//
// Edge describes its schema twice: sqlite_ddl.go's CREATE TABLE, which is all a
// fresh install ever runs, and the ALTER ADD COLUMN pass in migrations.go,
// which is the only road to an existing plant database. Two statements of one
// shape drift, and the drift is INVISIBLE in the normal course of events —
// CREATE TABLE IF NOT EXISTS no-ops on a plant, duplicate ADD COLUMN no-ops on
// a fresh install, so each database only ever executes one of the two.
//
// It is also PERMANENT. An ALTER-added column's declared DEFAULT cannot be
// changed afterwards — SQLite has no ALTER COLUMN DROP DEFAULT, only a full
// table rebuild — so a divergence here splits fresh installs from upgraded
// plants for the life of the database.
//
// There was a third copy of this schema until 2026-08-24 (a private guard list
// inside the rebuild file, dead on any database that had already crossed the
// rebuild). It is deleted; this test is what stops the remaining two from
// repeating what the three of them did.
func TestAlterPassMatchesBaselineCreate(t *testing.T) {
	root := moduleRoot(t)

	migSrc, err := os.ReadFile(filepath.Join(root, "store", "migrations.go"))
	if err != nil {
		t.Fatalf("read store/migrations.go: %v", err)
	}
	ddlSrc, err := os.ReadFile(filepath.Join(root, "store", "schema", "sqlite_ddl.go"))
	if err != nil {
		t.Fatalf("read store/schema/sqlite_ddl.go: %v", err)
	}

	created := baselineColumnDefs(string(ddlSrc))
	if len(created) == 0 {
		t.Fatal("parsed no CREATE TABLE statements out of sqlite_ddl.go — the parser has drifted from the file")
	}

	altered := alterColumnDefs(string(migSrc))
	if len(altered) == 0 {
		t.Fatal("parsed no ALTER TABLE ... ADD COLUMN statements out of migrations.go — the parser has drifted from the file")
	}

	unseen := map[string]string{}
	for _, d := range recordedAlterDrift {
		unseen[d.Key] = d.Why
	}

	compared := 0
	for _, a := range altered {
		want, declared := created[a.table+"."+a.column]
		if !declared {
			// Legitimate and common: a column the baseline CREATE no longer
			// carries (retired, or renamed to something else), or one that only
			// ever existed as an ALTER. Both statements run on a fresh install,
			// so an ALTER-only column is not a divergence.
			continue
		}
		compared++
		if want == a.def {
			continue
		}
		key := a.table + "." + a.column
		if _, known := unseen[key]; known {
			delete(unseen, key)
			continue
		}
		t.Errorf("column %s is declared two different ways, and each database executes only one of them:\n"+
			"  migrations.go ALTER : %s\n"+
			"  sqlite_ddl.go CREATE: %s\n\n"+
			"A fresh install gets the CREATE's definition and an upgraded plant gets the ALTER's,\n"+
			"permanently — SQLite cannot drop a column default without rebuilding the table.\n"+
			"Make the two identical. If the difference is deliberate and the data decision has\n"+
			"been made, record it in recordedAlterDrift with what was found.",
			key, a.def, want)
	}

	if compared == 0 {
		t.Fatal("no column was declared by BOTH statements — the parser matched nothing and this test asserted nothing")
	}
	for key, why := range unseen {
		t.Errorf("recorded ALTER/CREATE drift no longer occurs — delete it from recordedAlterDrift:\n  %s\n  (%s)", key, why)
	}
}

type alteredColumn struct {
	table  string
	column string
	def    string
}

var (
	addColumnStmt   = regexp.MustCompile(`ALTER TABLE (\w+) ADD COLUMN (\w+)([^"` + "`" + `]*)`)
	createTableStmt = regexp.MustCompile(`(?s)CREATE TABLE IF NOT EXISTS (\w+)\s*\((.*?)\n\);`)
	sqlComment      = regexp.MustCompile(`--[^\n]*`)
	spaceRun        = regexp.MustCompile(`\s+`)
)

// alterColumnDefs pulls every ADD COLUMN out of the migration source. The
// statements are Go string literals, so the definition runs to the closing
// quote or backtick.
func alterColumnDefs(src string) []alteredColumn {
	var out []alteredColumn
	for _, m := range addColumnStmt.FindAllStringSubmatch(src, -1) {
		out = append(out, alteredColumn{
			table:  m[1],
			column: m[2],
			def:    normalizeColumnDef(m[3]),
		})
	}
	return out
}

// baselineColumnDefs maps "table.column" to its definition in the baseline
// CREATE TABLE statements. Table constraints (UNIQUE(...), PRIMARY KEY(...))
// are not columns and are skipped — a line whose first token is one of those
// keywords never collides with a column name.
func baselineColumnDefs(src string) map[string]string {
	out := map[string]string{}
	for _, m := range createTableStmt.FindAllStringSubmatch(src, -1) {
		table := m[1]
		body := sqlComment.ReplaceAllString(m[2], "")
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), ","))
			if line == "" {
				continue
			}
			name, rest, ok := strings.Cut(line, " ")
			if !ok {
				continue
			}
			switch strings.ToUpper(name) {
			case "UNIQUE", "PRIMARY", "FOREIGN", "CHECK", "CONSTRAINT":
				continue
			}
			out[table+"."+name] = normalizeColumnDef(rest)
		}
	}
	return out
}

// normalizeColumnDef collapses whitespace and strips a trailing comma, so the
// comparison is about the declaration and not about how it is laid out.
func normalizeColumnDef(def string) string {
	s := spaceRun.ReplaceAllString(def, " ")
	return strings.TrimSuffix(strings.TrimSpace(s), ",")
}
