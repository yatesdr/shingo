package www

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"shingo/shared/clockglobals"
)

// TestEveryClockPageCarriesGlobals is Core's half of the shared rule: a full
// page whose scripts reach a clock function in shared/utils.js must carry
// window.PLANT_TZ and window.SHINGO_CLOCK.
//
// The audit lives in shingo/shared/clockglobals because the contract does —
// utils.js is shipped from that module and read by both trees, and one rule
// implemented twice is how the halves drift apart. See that package for what
// this catches and why nothing failed while it was broken.
//
// Core had four pages in that state: dashboard-display, dashboard-map,
// dashboard-node-report and heartbeat are standalone full pages that do not
// render through layout.html, so they never picked up the inlines that lived
// in it — the big display board included.
func TestEveryClockPageCarriesGlobals(t *testing.T) {
	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		t.Fatalf("sub static: %v", err)
	}

	// Core's full pages are layout.html and the standalone dashboards; all of
	// them now pull the partial, so the partial (or a literal inline) is the
	// only marker that counts here.
	carriers := []string{
		`{{template "clock-globals"`,
		"window.PLANT_TZ",
	}

	findings, subject, err := clockglobals.Audit(templateFS, static, carriers)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	for _, f := range findings {
		t.Error(clockglobals.Explain(f))
	}

	// A guard that silently matches nothing is worse than no guard: if the
	// script resolver or the import walk breaks, every page trivially passes.
	if subject == 0 {
		t.Fatal("no Core page was found to load a clock function — the script/import walk is " +
			"broken, not the templates (layout.html and the dashboards both qualify)")
	}
}

// pageJS lists Core's own browser modules — every .js under static except the
// vendored libraries and the node test files — as path → source.
func pageJS(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := fs.WalkDir(staticFS, "static", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "vendor" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".js") || strings.HasSuffix(path, ".test.js") {
			return nil
		}
		b, err := fs.ReadFile(staticFS, path)
		if err != nil {
			return err
		}
		out[path] = string(b)
		return nil
	})
	if err != nil {
		t.Fatalf("walk static: %v", err)
	}
	if len(out) < 20 {
		t.Fatalf("found only %d page modules under static — the walk is broken, not the pages", len(out))
	}
	return out
}

// stripLineComments drops // comments so a guard reads code, not the prose
// that explains why the code no longer does the thing.
var lineComment = regexp.MustCompile(`(?m)(^|[^:'"\\])//.*$`)

func stripLineComments(src string) string { return lineComment.ReplaceAllString(src, "$1") }

// TestNoPageModuleReadsTheBrowserCalendar: a page module may not take a date
// or time of day from the browser's own zone. Overview and its drill charts did
// (getHours for axis labels, getDate for "today"), so a viewer in another zone
// saw the plant's day start at 23:00 and captions carrying yesterday's date.
// Plant-local dates, clocks, windows and bucket labels come from
// components/plantclock.js; full stamps from shared/utils.js formatTime.
// The UTC getters stay legal — they are zone-free arithmetic.
func TestNoPageModuleReadsTheBrowserCalendar(t *testing.T) {
	// Number(n).toLocaleString() (a thousands separator) is not a clock read;
	// a Date's toLocaleString is.
	banned := regexp.MustCompile(`\.(get|set)(FullYear|Month|Date|Day|Hours|Minutes|Seconds)\(|\.toLocale(Date|Time)String\(|new Date\([^)]*\)\.toLocaleString\(`)
	var bad []string
	for path, src := range pageJS(t) {
		for i, line := range strings.Split(stripLineComments(src), "\n") {
			if banned.MatchString(line) {
				bad = append(bad, path+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Errorf("browser-zone calendar read (use components/plantclock.js): %s", b)
	}
}

// TestNoPageModuleDefinesADurationFormatter: there is one duration ladder,
// shared/utils.js formatDuration (the twin of protocol.FormatDuration, guide
// "Durations are compound"). Core had four more, which between them printed
// "21m 60s", "500ms", "76h 0m" and "0m" for no data.
//
// sourcing.js fmtHeld is out of this cleanup's scope (the Sourcing page). Its
// stated reason — that the shared ladder renders a measured zero as the dash —
// no longer holds (it prints "0 s"); it is listed here so it is not copied.
func TestNoPageModuleDefinesADurationFormatter(t *testing.T) {
	def := regexp.MustCompile(`function\s+((?:format|fmt)\w*(?:Dur|Duration|Seconds|Secs|Mins?|Held|Elapsed)\w*)\s*\(`)
	allowed := map[string]bool{"static/pages/sourcing.js fmtHeld": true}
	var bad []string
	for path, src := range pageJS(t) {
		for _, m := range def.FindAllStringSubmatch(stripLineComments(src), -1) {
			// A clock formatter ("formatClockSeconds") prints a time of day, not a span.
			if !allowed[path+" "+m[1]] && !strings.Contains(m[1], "Clock") {
				bad = append(bad, path+": "+m[1])
			}
		}
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Errorf("page-local duration formatter (import formatDuration from /static/shared/utils.js): %s", b)
	}
}

// TestEscapeHtmlDefinedOnce: the HTML escape is defined in one file across
// shared/ and Core's static tree — shared/utils.js, the copy that escapes
// quotes (pin: shared/utils.escape.test.js). Core had app.js's copy and five
// page-local ones (dashboard, dashboard-node-report, dashboard-map, missions'
// escapeText/escapeAttr), all but one leaving `"` and `'` alone, so the h template could
// be broken out of from inside an attribute. A page-local escaper under
// another name is caught by its shape: `&` replaced by hand, or a text node
// read back through innerHTML. Edge's own copies (shingoedge.js,
// operator-util.js) and shared/esc.js are outside this tree's say and listed
// in the round-2 report, not here.
func TestEscapeHtmlDefinedOnce(t *testing.T) {
	def := regexp.MustCompile(`function\s+escapeHtml\s*\(|\bescapeHtml\s*=\s*(?:function|\()`)
	shape := regexp.MustCompile(`replace\(/&/g|textContent\s*=[^;]*;\s*return\s+\w+\.innerHTML|createTextNode\([^)]*\)\);\s*return\s+\w+\.innerHTML`)
	files := pageJS(t)
	sharedDir := filepath.Join("..", "..", "shared")
	entries, err := os.ReadDir(sharedDir)
	if err != nil {
		t.Fatalf("read shared/: %v", err)
	}
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, ".js") || strings.HasSuffix(n, ".test.js") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(sharedDir, n))
		if err != nil {
			t.Fatal(err)
		}
		files["shared/"+n] = string(b)
	}
	var defs, shapes []string
	for path, src := range files {
		code := stripLineComments(src)
		if def.MatchString(code) {
			defs = append(defs, path)
		}
		if strings.HasPrefix(path, "static/") && shape.MatchString(code) {
			shapes = append(shapes, path)
		}
	}
	sort.Strings(defs)
	if strings.Join(defs, ",") != "shared/utils.js" {
		t.Errorf("escapeHtml defined in %v, want only shared/utils.js (import it from there or from /static/app.js)", defs)
	}
	sort.Strings(shapes)
	for _, p := range shapes {
		t.Errorf("page-local HTML escaper in %s: use escapeHtml or h from /static/app.js", p)
	}
}

// TestPlantTZReadInTwoFiles: the plant zone is read in exactly two browser
// files — shared/utils.js (formatTime, formatClock) and Core's
// components/plantclock.js (plant date, seconds, windows, bucket labels). A
// third reader is a third clock.
func TestPlantTZReadInTwoFiles(t *testing.T) {
	readers := map[string]bool{}
	for path, src := range pageJS(t) {
		if strings.Contains(stripLineComments(src), "PLANT_TZ") {
			readers[path] = true
		}
	}
	sharedDir := filepath.Join("..", "..", "shared")
	entries, err := os.ReadDir(sharedDir)
	if err != nil {
		t.Fatalf("read shared/: %v", err)
	}
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, ".js") || strings.HasSuffix(n, ".test.js") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(sharedDir, n))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(stripLineComments(string(b)), "PLANT_TZ") {
			readers["shared/"+n] = true
		}
	}
	want := []string{"shared/utils.js", "static/components/plantclock.js"}
	var got []string
	for r := range readers {
		got = append(got, r)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("PLANT_TZ readers = %v, want exactly %v", got, want)
	}
}

// TestPlantClockJS runs plantclock.test.js: plantclock.js agrees with the
// shared formatClock / formatTime on a vector table (UTC, America/Chicago,
// Asia/Tokyo, both DST edges, midnights), and resolves windows from the
// server's now on the plant calendar. Skipped without node, like the other JS
// wrappers; scripts/gate.sh refuses to run without node.
func TestPlantClockJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	script := filepath.Join("static", "components", "plantclock.test.js")
	out, err := exec.Command(nodePath, script).CombinedOutput()
	if err != nil {
		t.Fatalf("plantclock JS tests failed:\n%s\nerror: %v", out, err)
	}
}

// TestNoChartSetsItsOwnSmoothing: lines are straight (guide "Time series").
// components/charts.js sets tension 0 as the default; a dataset or chart that
// sets its own tension is drawing values nobody measured. charts.js itself is
// the one place the word may appear.
func TestNoChartSetsItsOwnSmoothing(t *testing.T) {
	tension := regexp.MustCompile(`\btension\s*:`)
	var bad []string
	for path, src := range pageJS(t) {
		if path == "static/components/charts.js" {
			continue
		}
		for i, line := range strings.Split(stripLineComments(src), "\n") {
			if tension.MatchString(line) {
				bad = append(bad, path+":"+strconv.Itoa(i+1))
			}
		}
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Errorf("chart sets its own line tension (the default in charts.js is 0): %s", b)
	}
}
