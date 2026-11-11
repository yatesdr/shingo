package www

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"shingoedge/config"
	"shingoedge/engine"
	"shingoedge/store"
	"shingoedge/store/processes"
)

var updateCatidHealthGolden = flag.Bool("update-catid-health-golden", false,
	"rewrite testdata/catid-health/*.txt from the two health routes as they serve today")

// TestCatidHealthBodiesPins characterises the part-identity health strip's two
// read routes as they serve today, before S4 moves the strip off the
// Processes list: GET /api/catid-health (the summary, one row per process with
// any item) and GET /api/processes/{id}/catid-health (a process's items),
// through the real handlers (apiCatidHealthSummary, apiCatidHealth) over a
// REAL engine's CatidHealth and CatidHealthSummary on a synthetic store.
//
// The engine is built and not started, so it has no CATID monitor: every item
// that reads the PLC's live part (wrong part, changeover waiting, assumed,
// identity unreadable, the MES slots) or the station counters' read health is
// out of reach here, and a process with an identity tag reads "Part identity
// not monitored". Those kinds are the engine's own tests'
// (engine/catid_health_test.go). What this pins is every kind a stored row
// raises, the summary's rows over them, and the bodies the Processes page
// pins (testdata/catid-health-harness) are fed.
//
// The store grows in three stages, so the summary is pinned with 0, 1 and 3
// processes the list shows (a red or amber worst item):
//
//	n0  CELL-QUIET (no items: no row) and CELL-HELD (two info items: a row
//	    the list leaves out, worst "info")
//	n1  + CELL-SHARED (one amber item: a CATID two styles hold)
//	n3  + CELL-IDENT (red worst: an unmapped CATID with counts held on it, a
//	    failed post-cutover check, an unmapped CATID with a suggestion, a
//	    running and an incoming style with no CATIDs, identity not
//	    monitored) and CELL-PAIR (amber: a one-way A/B pair)
//
// item5-*.txt answers brief S4 item 5 (the shared-CATID item printing one
// style name twice, engine/catid_health.go sharedItems): two live styles of a
// process cannot carry byte-identical names (idx_styles_process_name_live,
// store/migrations_soft_delete.go), and the style routes store a name
// untrimmed (handlers_styles.go apiCreateStyle, apiUpdateStyle), so the item
// names two styles whose names differ only by a trailing space. It is pinned
// as served; nothing here fixes it.
//
// TIMES ARE NOT PINNED where the store stamps them: an observation's
// last_seen (datetime('now')) reads <now>, and a hold's age in its detail
// reads <age>. A hold's first stroke is a fixed instant, so its "since" is
// pinned as is.
//
// Regenerate with
//
//	go test ./www -run TestCatidHealthBodiesPins -update-catid-health-golden
//
// and read the diff. Then regenerate TestCatidHealthPagePins, which reads
// these bodies as the page's answers.
func TestCatidHealthBodiesPins(t *testing.T) {
	t.Parallel()
	got := map[string]string{}

	rig := newCatidHealthRig(t)
	seedQuiet(t, rig)
	seedHeld(t, rig)
	got["summary-n0.txt"] = rig.get(t, "/api/catid-health",
		"the summary with no process the list shows: CELL-HELD's two info items are a row, worst info")
	seedShared(t, rig)
	got["summary-n1.txt"] = rig.get(t, "/api/catid-health", "the summary with one process the list shows")
	seedIdent(t, rig)
	seedPair(t, rig)
	got["summary-n3.txt"] = rig.get(t, "/api/catid-health", "the summary with three processes the list shows")
	for _, p := range rig.order {
		got["items-"+strings.ToLower(p)+".txt"] = rig.get(t, fmt.Sprintf("/api/processes/%d/catid-health", rig.ids[p]),
			p+"'s items, worst first")
	}
	got["items-unknown-process.txt"] = rig.get(t, "/api/processes/999/catid-health", "a process id no row has")
	got["items-bad-id.txt"] = rig.get(t, "/api/processes/x/catid-health", "an id that is not a number")

	twin := newCatidHealthRig(t)
	pid := twin.process(t, "CELL-TWIN", "", "")
	a := twin.style(t, pid, "TWIN", "6001")
	twin.style(t, pid, "TWIN ", "6001")
	mustDo(t, twin.db.SetActiveStyle(pid, &a), "active TWIN")
	if _, err := twin.db.CreateStyle("TWIN", "", pid); err == nil {
		t.Fatal("a second live style named TWIN was stored: the item-5 premise (one live name per process) no longer holds")
	}
	got["item5-shared-twin.txt"] = twin.get(t, fmt.Sprintf("/api/processes/%d/catid-health", pid),
		"item 5: styles \"TWIN\" and \"TWIN \" (a trailing space) both pin 6001; a byte-identical second name is refused by the store")
	got["item5-summary.txt"] = twin.get(t, "/api/catid-health", "item 5: the summary row over it")

	checkGoldenDir(t, filepath.Join("testdata", "catid-health"), got, *updateCatidHealthGolden, "-update-catid-health-golden")
}

// catidHealthRig is the stub handlers with the two health verbs answered by a
// real engine over the stub's own database.
type catidHealthRig struct {
	db    *store.DB
	mux   http.Handler
	ids   map[string]int64
	order []string
}

// catidHealthEngine answers CatidHealth and CatidHealthSummary from the real
// engine and everything else from the stub.
type catidHealthEngine struct {
	*stubEngine
	real *engine.Engine
}

func (e catidHealthEngine) CatidHealth(processID int64) ([]engine.CatidHealthItem, error) {
	return e.real.CatidHealth(processID)
}

func (e catidHealthEngine) CatidHealthSummary() ([]engine.CatidHealthSummary, error) {
	return e.real.CatidHealthSummary()
}

func newCatidHealthRig(t *testing.T) *catidHealthRig {
	t.Helper()
	h, r := newTestHandlers(t)
	db := dbOf(h)
	cfg := config.Defaults()
	// A "no cart" row's times are written in the plant's zone; UTC so the
	// golden is the same on every box.
	cfg.Timezone = "UTC"
	real := engine.New(engine.Config{AppConfig: cfg, DB: db, LogFunc: t.Logf})
	h.orchestration = catidHealthEngine{stubEngine: h.orchestration.(*stubEngine), real: real}
	// The two routes as router.go registers them (the /api group, public).
	r.Get("/api/processes/{id}/catid-health", h.apiCatidHealth)
	r.Get("/api/catid-health", h.apiCatidHealthSummary)
	return &catidHealthRig{db: db, mux: r, ids: map[string]int64{}}
}

// catidHealthFirstStroke is every seeded hold's first stroke: fixed, so the
// item's since is pinned.
var catidHealthFirstStroke = time.Date(2026, 11, 2, 6, 0, 0, 0, time.UTC)

var (
	catidHealthSQLiteNow = regexp.MustCompile(`\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}`)
	catidHealthHoldAge   = regexp.MustCompile(`strokes over (?:\d+h)?(?:\d+m)?\d+s \(`)
)

// get serves path and returns it as a golden: what it is, the request, the
// status and content type, and the body indented (the body as served is the
// same JSON on one line; the routes' own test pins that byte shape).
func (r *catidHealthRig) get(t *testing.T, path, what string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, req)
	body := rec.Body.Bytes()
	var out bytes.Buffer
	if json.Valid(body) {
		if err := json.Indent(&out, body, "", "  "); err != nil {
			t.Fatalf("%s: indent: %v", path, err)
		}
	} else {
		out.Write(body)
	}
	text := catidHealthSQLiteNow.ReplaceAllString(out.String(), "<now>")
	text = catidHealthHoldAge.ReplaceAllString(text, "strokes over <age> (")
	return "# " + what + "\nGET " + path + "\n" + fmt.Sprint(rec.Code) + " " + rec.Header().Get("Content-Type") + "\n" +
		strings.TrimRight(text, "\n") + "\n"
}

func (r *catidHealthRig) process(t *testing.T, name, plc, tag string) int64 {
	t.Helper()
	id, err := r.db.CreateProcess(name, "", plc, tag, tag != "")
	mustDo(t, err, "create process "+name)
	r.ids[name] = id
	r.order = append(r.order, name)
	return id
}

func (r *catidHealthRig) style(t *testing.T, pid int64, name, pin string) int64 {
	t.Helper()
	id, err := r.db.CreateStyle(name, "", pid)
	mustDo(t, err, "create style "+name)
	if pin != "" {
		mustDo(t, r.db.SetStyleExpectedCATID(id, pin), "pin "+name)
	}
	return id
}

func (r *catidHealthRig) node(t *testing.T, pid int64, name string) int64 {
	t.Helper()
	id, err := r.db.CreateProcessNode(processes.NodeInput{ProcessID: pid, CoreNodeName: name, Code: name, Name: name,
		Sequence: 1, Enabled: true})
	mustDo(t, err, "create node "+name)
	return id
}

// CELL-QUIET: two styles, two CATIDs, no counter. No item.
func seedQuiet(t *testing.T, r *catidHealthRig) {
	pid := r.process(t, "CELL-QUIET", "", "")
	a := r.style(t, pid, "QUIET-ONE", "1001")
	r.style(t, pid, "QUIET-TWO", "1002")
	mustDo(t, r.db.SetActiveStyle(pid, &a), "active QUIET-ONE")
}

// CELL-HELD: a held share at a position and a "no cart" row there, both
// ledger rows (info), no counter.
func seedHeld(t *testing.T, r *catidHealthRig) {
	pid := r.process(t, "CELL-HELD", "", "")
	a := r.style(t, pid, "HELD-ONE", "4001")
	mustDo(t, r.db.SetActiveStyle(pid, &a), "active HELD-ONE")
	n := r.node(t, pid, "HELD-POS")
	_, err := r.db.HoldCatidDraw(pid, "4002", n, "PART-H", "held", 3, catidHealthFirstStroke)
	mustDo(t, err, "hold a share")
	_, err = r.db.HoldCatidDraw(pid, "", n, "PART-H", "no-cart", 2, catidHealthFirstStroke)
	mustDo(t, err, "log a no-cart row")
}

// CELL-SHARED: CATID 2001 in two styles' pins (the store's setter takes it;
// the style routes refuse it, CheckStyleCATIDPin).
func seedShared(t *testing.T, r *catidHealthRig) {
	pid := r.process(t, "CELL-SHARED", "", "")
	a := r.style(t, pid, "SHARED-ONE", "2001")
	r.style(t, pid, "SHARED-TWO", "2001,2002")
	mustDo(t, r.db.SetActiveStyle(pid, &a), "active SHARED-ONE")
}

// CELL-IDENT: a counter on an MES-style struct, so its identity tag is
// derived (MES_CELL.CATID_01) and the identity items run, with no monitor.
// IDENT-RUN runs with no CATIDs and IDENT-NEXT is the open target with none;
// IDENT-OLD pins 3001. The PLC sent 3003 (counts held on it) and 3004 (a
// completed changeover to IDENT-NEXT ended on it: the suggestion); a later
// changeover's post-cutover check failed on 3005.
func seedIdent(t *testing.T, r *catidHealthRig) {
	pid := r.process(t, "CELL-IDENT", "PLC-1", "MES_CELL.Prod_Counter_01")
	run := r.style(t, pid, "IDENT-RUN", "")
	next := r.style(t, pid, "IDENT-NEXT", "")
	old := r.style(t, pid, "IDENT-OLD", "3001")
	mustDo(t, r.db.SetActiveStyle(pid, &run), "active IDENT-RUN")
	mustDo(t, r.db.SetTargetStyle(pid, &next), "target IDENT-NEXT")
	mustDo(t, r.db.RecordPLCCATIDObservation(pid, "PLC-1", "3003"), "observe 3003")
	mustDo(t, r.db.RecordPLCCATIDObservation(pid, "PLC-1", "3004"), "observe 3004")
	_, err := r.db.HoldCatidDraw(pid, "3003", 0, "", "held", 5, catidHealthFirstStroke)
	mustDo(t, err, "hold the whole draw on 3003")
	co, err := r.db.InsertCompletedChangeover(pid, &old, next, "test", "", "")
	mustDo(t, err, "a completed changeover to IDENT-NEXT")
	mustDo(t, r.db.SetChangeoverStartCATID(co, "3001"), "its start CATID")
	mustDo(t, r.db.SetChangeoverEndCATID(co, "3004"), "its end CATID")
	flagged, err := r.db.InsertCompletedChangeover(pid, &next, run, "test", "", "")
	mustDo(t, err, "a completed changeover to IDENT-RUN")
	mustDo(t, r.db.SetChangeoverVerifyMismatch(flagged, "3005"), "its failed post-cutover check")
}

// CELL-PAIR: PAIR-A names PAIR-B as its A/B partner and PAIR-B names no one.
func seedPair(t *testing.T, r *catidHealthRig) {
	pid := r.process(t, "CELL-PAIR", "", "")
	a := r.style(t, pid, "PAIR-ONE", "5001")
	mustDo(t, r.db.SetActiveStyle(pid, &a), "active PAIR-ONE")
	for _, c := range [][2]string{{"PAIR-A", "PAIR-B"}, {"PAIR-B", ""}} {
		_, err := r.db.Exec(`INSERT INTO style_node_claims (style_id, core_node_name, role, swap_mode, payload_code, paired_core_node)
			VALUES (?, ?, 'produce', 'sequential', 'PART-P', ?)`, a, c[0], c[1])
		mustDo(t, err, "claim "+c[0])
	}
}

func mustDo(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// checkGoldenDir compares got against dir's files, or rewrites them under the
// flag. It owns dir: a file no entry of got names is reported (or removed under
// the flag), so the directory must hold this pin's goldens and nothing else.
func checkGoldenDir(t *testing.T, dir string, got map[string]string, update bool, flagName string) {
	t.Helper()
	names := make([]string, 0, len(got))
	for n := range got {
		names = append(names, n)
	}
	sort.Strings(names)
	if update {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	for _, n := range names {
		path := filepath.Join(dir, n)
		if update {
			if err := os.WriteFile(path, []byte(got[n]), 0o644); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: no golden (%v); regenerate with %s and read it", n, err, flagName)
			continue
		}
		if line, w, g, ok := firstDiffLine(string(want), got[n]); !ok {
			t.Errorf("%s: moved at line %d; regenerate only for a named change\n  golden: %s\n  now:    %s", path, line, w, g)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		if _, ok := got[e.Name()]; ok || e.IsDir() {
			continue
		}
		if update {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				t.Fatalf("remove stale %s: %v", e.Name(), err)
			}
			continue
		}
		t.Errorf("%s: a golden no pin writes; delete it, or restore the pin that wrote it", filepath.Join(dir, e.Name()))
	}
}
