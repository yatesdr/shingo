package store_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// demand_tense_drift_test.go — one spelling for a demand record's tense.
//
// A demand_origins row is a forecast (opened_at NULL, closed_at NULL), open
// (opened_at set, closed_at NULL) or settled (closed_at set). Before a forecast
// row was legal, "open" and "not closed" were the same predicate, and eleven
// statements plus seven Go sites (and the open-key index) read it that way.
// Each of them now goes through the helpers: SQL through
// helpers.DemandOpenSQL / DemandForecastSQL / DemandUnsettledSQL /
// DemandSettledSQL (store/internal/helpers/demand_tense.go), Go through
// domain.DemandTenseOf / DemandOrigin.Tense (domain/demand_tense.go).
//
// Two checks, no database:
//   - the census: the 19 deciding sites DESIGN-D1 §2(c) counted, each named,
//     each shown to compose a helper and to spell no predicate itself (the
//     Go sites that decide by membership in an open reader are checked through
//     that reader);
//   - the walk: no other non-test file under shingo-core spells a tense
//     predicate in a statement touching demand_origins, or tests ClosedAt
//     against nil, outside the helper files and the migration history.
//
// Precedents: store/bins/one_sourcing_predicate_test.go,
// shingo-edge/engine/origin_totality_test.go.

var (
	tenseHelperCall = regexp.MustCompile(`\bDemand(Open|Forecast|Unsettled|Settled)SQL\(`)
	tenseHandSQL    = regexp.MustCompile(`(?i)\b(closed_at|opened_at)\s+IS\s+(NOT\s+)?NULL\b`)
	tenseHandGo     = regexp.MustCompile(`ClosedAt\s*[!=]=\s*nil`)
)

// tenseBody returns the source of one top-level func (from its signature to the
// next top-level func), or fails the test naming the site.
func tenseBody(t *testing.T, file, fn string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.FromSlash(file))
	if err != nil {
		t.Fatalf("read %s: %v (if the site moved, repoint the census rather than deleting it)", file, err)
	}
	_, rest, found := strings.Cut(string(b), fn)
	if !found {
		t.Fatalf("could not find %q in %s: the census is stale, and a silent miss would leave it passing on nothing", fn, file)
	}
	// A top-level func ends at the first "}" in column one; cutting there keeps
	// the next func's doc comment out of this one's body.
	body, _, _ := strings.Cut(rest, "\n}\n")
	return body
}

// sqlSite is a statement that decides a row's tense: it must compose at least
// min helper calls and spell no tense predicate itself.
type sqlSite struct {
	name string
	file string
	fn   string
	min  int
}

var tenseSQLSites = []sqlSite{
	{"1 supersede/promote at the Edge's open", "demand_origins.go", "func (db *DB) SupersedeOpenEpisode(", 2},
	{"2 the open episode for a key", "demand_origins.go", "func (db *DB) OpenOriginForKey(", 1},
	{"3 Core's close by id", "demand_origins.go", "func (db *DB) CloseDemandOriginByID(", 1},
	{"4 the inferred close", "demand_origins.go", "func (db *DB) CloseDemandOriginInferred(", 1},
	{"5 the childless sweep's open set", "demand_origins.go", "func (db *DB) ListOpenEpisodeStates(", 1},
	{"6 the open set of one kind", "demand_origins.go", "func (db *DB) ListOpenEpisodesOfKind(", 1},
	{"7 an ask's open maintain episode", "demand_origins.go", "func (db *DB) MaintainedEpisodeForOrigin(", 1},
	{"8 the demand browser's scope", "demand_origins.go", "func (db *DB) ListDemandEpisodes(", 2},
	{"9 the child-outcome scope", "demand_child_outcomes.go", "func (db *DB) CountChildrenByStatus(", 2},
	// 10 and 11: the statement moved from CountTypedInboundToGroup, now a count
	// over this one read, so the keeper and an unloader's group line share it.
	{"10 typed inbound to a group, arm 1", "orders/orders.go", "func ListTypedInboundToGroup(", 1},
	{"11 typed inbound to a group, arm 2", "orders/orders.go", "func ListTypedInboundToGroup(", 2},
}

func checkSQLSite(t *testing.T, s sqlSite) {
	t.Helper()
	body := tenseBody(t, s.file, s.fn)
	if n := len(tenseHelperCall.FindAllString(body, -1)); n < s.min {
		t.Errorf("%s (%s %s): composes %d tense helper(s), want at least %d", s.name, s.file, s.fn, n, s.min)
	}
	if m := tenseHandSQL.FindString(body); m != "" {
		t.Errorf("%s (%s %s): spells %q itself; compose helpers.Demand*SQL instead", s.name, s.file, s.fn, m)
	}
}

// goSite is a Go site that decides a row's tense. Either it calls a named
// helper itself (mustCall) and tests no ClosedAt against nil, or it decides by
// membership in an open reader (reader), which must itself pass its SQL check.
type goSite struct {
	name     string
	file     string
	fn       string
	mustCall string
	reader   string // method name the site must call
	readerOf int    // index into tenseSQLSites that reader resolves to
}

var tenseGoSites = []goSite{
	{name: "G1 the threshold monitor's open origin", file: "../engine/threshold_monitor.go",
		fn: "func (m *ThresholdMonitor) openOrigin(", reader: "OpenOriginForKey", readerOf: 1},
	{name: "G2 the startup sweep's open set", file: "../engine/threshold_monitor.go",
		fn: "func (m *ThresholdMonitor) startupSweep(", reader: "ListOpenThresholdEpisodes", readerOf: 5},
	{name: "G3 the binding sweep's candidates", file: "../engine/threshold_episodes.go",
		fn: "func (m *ThresholdMonitor) reconcileThresholdBindings(", reader: "ListOpenThresholdEpisodes", readerOf: 5},
	{name: "G4 the keeper's memory", file: "../engine/maintainer.go",
		fn: "func (m *Maintainer) Tick(", reader: "ListOpenEpisodesOfKind", readerOf: 5},
	{name: "G5 the childless sweep's age test", file: "../engine/demand_reconciler.go",
		fn: "func (e *Engine) reconcileChildlessEpisodes(", reader: "ListOpenEpisodeStates", readerOf: 4},
	{name: "G6 the Edge's open message", file: "../messaging/demand_origin_handler.go",
		fn: "func (s *CoreDataService) HandleDemandOrigin(", mustCall: "DemandTenseOf("},
	{name: "G7 the episode row's open flag", file: "../www/demand_episodes_view.go",
		fn: "func BuildEpisodeRow(", mustCall: ".Tense()"},
}

func checkGoSite(t *testing.T, g goSite) {
	t.Helper()
	body := tenseBody(t, g.file, g.fn)
	if m := tenseHandGo.FindString(body); m != "" {
		t.Errorf("%s (%s): tests %q itself; decide through domain.DemandTenseOf or DemandOrigin.Tense", g.name, g.file, m)
	}
	if g.mustCall != "" {
		if !strings.Contains(body, g.mustCall) {
			t.Errorf("%s (%s): does not decide the tense through %s", g.name, g.file, g.mustCall)
		}
		return
	}
	if !strings.Contains(body, "."+g.reader+"(") {
		t.Errorf("%s (%s): no longer reads %s; repoint the census to the reader it uses", g.name, g.file, g.reader)
		return
	}
	r := tenseSQLSites[g.readerOf]
	rb := tenseBody(t, r.file, r.fn)
	if !strings.Contains(rb, "DemandOpenSQL(") || tenseHandSQL.MatchString(rb) {
		t.Errorf("%s (%s): decides by membership in %s, whose statement (%s) does not compose "+
			"helpers.DemandOpenSQL and nothing else: a forecast would read as open here", g.name, g.file, g.reader, r.fn)
	}
}

// checkIndexSite: the open-key index predicate (v59, migration history, not
// rewritten) is the unsettled slot. The door that names that slot in its ON
// CONFLICT must name it through DemandUnsettledSQL, and the helper's spelling
// must be the index's.
func checkIndexSite(t *testing.T) {
	t.Helper()
	mig, err := os.ReadFile("migrations.go")
	if err != nil {
		t.Fatalf("read migrations.go: %v", err)
	}
	if !strings.Contains(string(mig), "ON demand_origins(episode_key) WHERE closed_at IS NULL") {
		t.Error("v59's open-key index predicate is not `closed_at IS NULL`: repoint DemandUnsettledSQL and this census")
	}
	h, err := os.ReadFile(filepath.FromSlash("internal/helpers/demand_tense.go"))
	if err != nil {
		t.Errorf("read internal/helpers/demand_tense.go: %v: the one spelling of the tense does not exist", err)
		return
	}
	unsettled := tenseBodyOf(string(h), "func DemandUnsettledSQL(")
	if !strings.Contains(unsettled, `demandCol(alias, "closed_at") + " IS NULL)"`) {
		t.Errorf("DemandUnsettledSQL no longer spells the index predicate `closed_at IS NULL`:\n%s", unsettled)
	}
	body := tenseBody(t, "demand_origins.go", "func (db *DB) OpenCoreEpisode(")
	if !strings.Contains(body, "DemandUnsettledSQL(") || !strings.Contains(body, "DemandForecastSQL(") {
		t.Error("OpenCoreEpisode does not name the index's slot through DemandUnsettledSQL and promote " +
			"through DemandForecastSQL")
	}
	if m := tenseHandSQL.FindString(body); m != "" {
		t.Errorf("OpenCoreEpisode spells %q itself", m)
	}
}

func tenseBodyOf(src, fn string) string {
	_, rest, found := strings.Cut(src, fn)
	if !found {
		return ""
	}
	body, _, _ := strings.Cut(rest, "\n}\n")
	return body
}

// TestDemandTense_NoQueryDecidesTenseOutsideTheHelpers is the census and the
// walk described at the top of the file.
func TestDemandTense_NoQueryDecidesTenseOutsideTheHelpers(t *testing.T) {
	t.Parallel()
	for _, s := range tenseSQLSites {
		t.Run("SQL "+s.name, func(t *testing.T) { checkSQLSite(t, s) })
	}
	t.Run("SQL 12 the open-key index's slot", checkIndexSite)
	for _, g := range tenseGoSites {
		t.Run(g.name, func(t *testing.T) { checkGoSite(t, g) })
	}
	t.Run("walk", walkTenseSpellings)
}

// tenseExempt are the files allowed to spell the predicates: the two helper
// files that ARE the spelling, and the migration history, which is never
// rewritten (v59's index DDL among it).
var tenseExempt = map[string]bool{
	"store/internal/helpers/demand_tense.go": true,
	"domain/demand_tense.go":                 true,
	"store/migrations.go":                    true,
}

func walkTenseSpellings(t *testing.T) {
	root := ".." // shingo-core
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" || d.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if tenseExempt[rel] {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if loc := tenseHandGo.FindIndex(src); loc != nil {
			t.Errorf("%s tests %q: decide a demand record's tense through domain.DemandTenseOf or DemandOrigin.Tense",
				rel, src[loc[0]:loc[1]])
		}
		f, perr := parser.ParseFile(fset, path, src, 0)
		if perr != nil {
			return perr
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if strings.Contains(lit.Value, "demand_origins") && tenseHandSQL.MatchString(lit.Value) {
				t.Errorf("%s:%d: a statement on demand_origins spells %q itself; compose helpers.Demand*SQL",
					rel, fset.Position(lit.Pos()).Line, tenseHandSQL.FindString(lit.Value))
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}
