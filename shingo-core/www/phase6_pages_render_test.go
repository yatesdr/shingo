package www

import (
	"bytes"
	"html/template"
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"shingocore/domain"
)

// phase6_pages_render_test.go — the Phase 6 pages actually render.
//
// ── WHY THIS EXISTS, AND IT IS NOT BELT-AND-BRACES ───────────────────────────
//
// router.go clones (layout + partials) SEPARATELY FOR EACH PAGE, so a
// {{define}} written inside a page template is reachable from that page and
// from nowhere else. Nothing catches the mistake earlier than render: the
// template parses, template.Must is happy, every unit test is green, and the
// page 500s the first time a human opens it.
//
// That is precisely what happened while building 5.7. /orphans consumes the
// de-cell renderer, de-cell was defined inside demand-episodes.html, and the
// orphan page — whose entire job is to be readable on the day something is
// wrong — failed with "no such template de-cell". The fix moved de-cell into
// partials/cells.html; this test is what would have found it, and what will
// find it again when the next Phase 6 surface reaches for a shared define.
//
// It also holds the two rendering constraints no Go unit test can see, because
// they are properties of the emitted markup rather than of any value.

// renderPage builds a page exactly the way router.go does — same base, same
// clone-per-page, same entry template — so a failure here is a failure the
// server would have had.
func renderPage(t *testing.T, page string, data map[string]any) string {
	t.Helper()

	pages, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		t.Fatalf("glob templates: %v", err)
	}
	var found bool
	for _, p := range pages {
		if p == "templates/"+page {
			found = true
		}
	}
	if !found {
		t.Fatalf("%s is not discovered by router.go's glob, so it is not routable", page)
	}

	base := template.New("").Funcs(templateFuncs(nil))
	base = template.Must(base.ParseFS(templateFS,
		"templates/layout.html", "templates/partials/*.html"))
	clone := template.Must(base.Clone())
	clone = template.Must(clone.ParseFS(templateFS, "templates/"+page))

	if data["Authenticated"] == nil {
		data["Authenticated"] = true
	}
	var buf bytes.Buffer
	if err := clone.ExecuteTemplate(&buf, "layout", data); err != nil {
		t.Fatalf("render %s: %v\n\nA template that PARSES can still fail here — a "+
			"{{template}} call to a define that lives in another page is invisible "+
			"until execution, because each page is cloned separately.", page, err)
	}
	return buf.String()
}

// TestOrphansPageRenders is 5.7's page.
func TestOrphansPageRenders(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	since := now.Add(-4 * time.Hour)
	oldest := now.Add(-2 * time.Hour)
	c := disp()

	out := renderPage(t, "orphans.html", map[string]any{
		"Page": "orphans", "WindowDays": 7,
		"BucketLabel": FormatDuration(c.OrphanBucket), "MinBucketOrders": c.MinBucketOrders,
		"Lane": BuildOrphanLane([]domain.OrphanSite{
			{StationID: "LIVE", Live: 2, Aged: 3, Total: 5, OldestLive: &oldest},
			{StationID: "CLEAN", Live: 0, Aged: 1, Total: 1, OldestLive: nil},
		}, now),
		"Trend": BuildOrphanTrend([]domain.OrphanBucket{
			{Start: since, Orphans: 3, Orders: 100},                  // full-strength rate
			{Start: since.Add(2 * time.Hour), Orphans: 1, Orders: 4}, // muted: thin
			// since+1h and since+3h are absent: unmeasured.
		}, since, now, c),
	})

	// ALL THREE ABSENCE STATES, plus the muted fourth, in one rendering. The
	// unit tests hold them apart as values; this holds them apart as MARKUP,
	// which is where they are finally either different or not.
	for _, want := range []string{
		"de-nodata", // an empty bucket's unmeasured rate
		"de-na",     // a station with no live findings
		"de-muted",  // a rate over a denominator too thin to support it
		"or-chart", "or-bucket-empty", "or-bar-",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered /orphans is missing %q", want)
		}
	}
}

// TestDemandEpisodesPageRendersTheCauseColumn is 5.2's addition to the browser.
func TestDemandEpisodesPageRendersTheCauseColumn(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	expected := 4
	rows := []EpisodeRow{
		BuildEpisodeRow(domain.DemandEpisode{
			DemandOrigin: domain.DemandOrigin{
				OriginID: "o1", EpisodeKey: "k1", Kind: "cell", Direction: "supply",
				StationID: "S1", PayloadCode: "PANEL-A",
				OpenedAt: now.Add(-70 * time.Minute), ExpectedOrders: &expected,
			}, Children: 2}, now, disp()),
		// A zero-order episode: the page's worst case.
		BuildEpisodeRow(domain.DemandEpisode{
			DemandOrigin: domain.DemandOrigin{
				OriginID: "o2", EpisodeKey: "k2", Kind: "threshold",
				StationID: "S2", OpenedAt: now.Add(-10 * time.Minute),
				ExpectedOrders: &expected,
			}, Children: 0}, now, disp()),
	}
	byOrigin := FoldChildCounts([]domain.ChildStatusCount{
		{OriginID: "o1", Status: "cancelled", ReachedVendor: false, Count: 2},
	})
	AttachCauses(rows, byOrigin, true)

	out := renderPage(t, "demand-episodes.html", map[string]any{
		"Page": "demand-episodes", "WindowHours": 24, "WorryAfter": "45m",
		"ConcernAfter": "60m", "MinExpected": 2, "Rows": rows, "Limit": 200,
		"CauseTotals": SummarizeCauses(byOrigin),
	})

	for _, want := range []string{
		"Cause", "de-causes",
		"Cancelled early", "de-cause-cancelled_early",
		"No orders", // the zero-order episode, as a measured finding
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered /demand-episodes is missing %q", want)
		}
	}

	// THE PROXY MUST NOT CLAIM MORE THAN IT MEASURES, all the way to the pixel.
	// The unit test holds the label; this holds the whole page, so a heading, a
	// caption or a tooltip cannot reintroduce the claim the label avoided.
	for _, forbidden := range []string{"Re-armed", "re-arm churn", "Rearm"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("rendered /demand-episodes contains %q. The bucket is 'cancelled "+
				"before the vendor acknowledged it', a SUPERSET of re-arm churn — the "+
				"page must not name it after the hypothesis anywhere.", forbidden)
		}
	}
}

// TestMaterialFlagsPageRenders is 5.11's page, and it holds the two things no
// Go unit test can see: that both sections are on the page at once, and that the
// copy makes no claim the data cannot support.
func TestMaterialFlagsPageRenders(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	c := disp()

	// One expectation of 1 so the ratio renders MUTED — a real value whose
	// denominator cannot support it, which is the fourth state and not one of the
	// three absences.
	thin := 1
	flagRows := []EpisodeRow{
		BuildEpisodeRow(domain.DemandEpisode{DemandOrigin: domain.DemandOrigin{
			OriginID: "o1", EpisodeKey: "k1", Kind: "cell", StationID: "S1",
			PayloadCode: "PANEL-A", OpenedAt: now.Add(-3 * time.Hour),
			ExpectedOrders: &thin,
		}, Children: 3}, now, c),
	}
	flags, flagSummary := SelectMaterialFlags(flagRows)

	bound := now.Add(-9 * 24 * time.Hour)
	capacity := 250
	counted := now.Add(-30 * 24 * time.Hour)
	bindings, bindingSummary := BuildBindingRows([]domain.CarrierBinding{
		// Beyond one binload: the one reading that carries a ring.
		{BinID: 1, Label: "CARRIER-0001", PayloadCode: "PANEL-A", NodeName: "SMN_001",
			UOPRemaining: -800, UOPCapacity: &capacity, BoundAt: &bound, LastCountedAt: &counted},
		// Settled ledger on an old binding: binloads is NOT APPLICABLE (de-na),
		// and the carrier has never been counted (de-nodata).
		{BinID: 2, Label: "CARRIER-0002", PayloadCode: "PANEL-B", NodeName: "SMN_002",
			UOPRemaining: 420, UOPCapacity: &capacity, BoundAt: &bound},
		// Within one binload — the Springfield bin-27 shape: a long binding whose
		// ledger is shallow enough for one overpack to explain. It is a candidate
		// on its AGE, and its reading gets no ring.
		{BinID: 5, Label: "CARRIER-0005", PayloadCode: "PANEL-D", NodeName: "SMN_005",
			UOPRemaining: -50, UOPCapacity: &capacity, BoundAt: &bound},
		// Negative with no capacity recorded: the READING says "Cannot size" as
		// words and the FIGURE column carries the em dash. One absence per fact.
		{BinID: 6, Label: "CARRIER-0006", PayloadCode: "PANEL-GHOST", NodeName: "SMN_006",
			UOPRemaining: -9, BoundAt: &bound},
		// No boundary row: binding age unknowable (de-nodata) and it is listed.
		{BinID: 3, Label: "CARRIER-0003", PayloadCode: "PANEL-C", UOPRemaining: 0},
		// Unbound: counted in the summary, never a candidate row.
		{BinID: 4, Label: "CARRIER-0004"},
	}, now, c)

	out := renderPage(t, "material-flags.html", map[string]any{
		"Page": "material-flags", "WorryAfter": FormatDuration(c.WorryAfter),
		"ConcernAfter":      FormatDuration(c.ConcernAfter),
		"StaleBindingAfter": FormatDuration(c.StaleBindingAfter),
		"OverpackBinloads":  FormatRatio(c.OverpackBinloads),
		"Flags":             flags, "FlagSummary": flagSummary, "FlagLimit": 200,
		"Bindings": bindings, "BindingSummary": bindingSummary,
	})

	// ALL FOUR STATES IN ONE RENDERING. The unit tests hold them apart as values;
	// this holds them apart as MARKUP, which is where they are finally either
	// different or not.
	for _, want := range []string{
		"de-nodata",                                           // an unknowable binding age, and a never-counted carrier
		"de-na",                                               // a settled ledger has no negative to size
		"de-muted",                                            // a ratio whose denominator is below the floor
		"mf-read-beyond",                                      // the one ledger reading that carries a ring
		"Beyond one binload", "Within one binload", "Settled", // the printed names
		"Cannot size",        // the unsizeable negative says so IN WORDS, not as a dash
		"mf-heading--second", // BOTH sections rendered, not one
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered /material-flags is missing %q", want)
		}
	}

	// THE TWO OWNERS ARE NAMED, which is the disambiguation the row exists for. A
	// reader who cannot tell which section is theirs is the reader the old
	// "material downtime" wording misled.
	for _, want := range []string{"Owner: whoever moves material", "Owner: whoever cycle counts"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered /material-flags does not name an owner: missing %q", want)
		}
	}

	// NO CLAIM THE DATA CANNOT SUPPORT, all the way to the pixel. Constraints 1
	// and 2: nothing attributes, and nothing accumulates into a downtime metric.
	// The page is allowed to say the word "downtime" while DENYING it — which is
	// why these are phrases and not the bare word.
	for _, forbidden := range []string{
		"Total waiting", "Total downtime", "Downtime minutes", "minutes of downtime",
		"Minutes lost", "caused by", "responsible for",
	} {
		if strings.Contains(out, forbidden) {
			t.Errorf("rendered /material-flags contains %q. Constraint 1 is flag-never-"+
				"attribution and constraint 2 is no-metric-grading-anyone; a total of "+
				"durations or a named cause breaks one of them.", forbidden)
		}
	}
	// And the denial itself has to be present — a page that merely omits the claim
	// leaves the reader to make it.
	if !strings.Contains(out, "It does not record whether a line was stopped") {
		t.Error("the page does not state what it cannot see. Constraint 3 is state " +
			"confidence honestly, and the reader has to be told that no line-stopped " +
			"signal exists anywhere in this data")
	}
}

// TestPhase6PagesUseNoChips holds the earned constraint at the markup level.
//
// The .chip-* family fails both contrast floors STRUCTURALLY — a chip's fill is
// a color-mix of its own label colour, so the two floors pull against each
// other and no percentage satisfies both. Fifteen of twenty-eight combinations
// are below AA on text, worst .chip-ok at 2.89:1. Phase 6 is the surface that
// would consume them at scale, which is exactly why it does not.
//
// Asserted against the TEMPLATE SOURCE rather than the rendered output,
// deliberately: rendered output carries layout.html's chrome, and a rule about
// what these pages introduce must not be able to fail because of something they
// inherit.
func TestPhase6PagesUseNoChips(t *testing.T) {
	for _, page := range []string{
		"templates/demand-episodes.html",
		"templates/orphans.html",
		"templates/material-flags.html",
		"templates/partials/cells.html",
	} {
		raw, err := templateFS.ReadFile(page)
		if err != nil {
			t.Fatalf("read %s: %v", page, err)
		}
		// TEMPLATE COMMENTS ARE STRIPPED FIRST. These files explain at length
		// why they use no chips, so a naive substring search matches the
		// explanation and fails on the very files that comply. A rule about
		// emitted markup has to be checked against emitted markup.
		src := stripTemplateComments(string(raw))
		if strings.Contains(src, "chip-") {
			t.Errorf("%s references a .chip-* class. The family fails both contrast "+
				"floors structurally (worst 2.89:1) and cannot be fixed by tuning; use a "+
				"ring plus a printed name, as the duration band and the cause column "+
				"already do.", page)
		}
		// Inline style is forbidden for new code — a data-driven geometry has to
		// be a class, which is why the trend bars quantise.
		if strings.Contains(src, `style="`) {
			t.Errorf("%s carries an inline style=. New code puts styling in classes; a "+
				"value-driven dimension quantises to a class set instead.", page)
		}
	}
}

// TestSharedCellDefinesLiveInPartials is the structural half of the render bug.
//
// de-cell is consumed by more than one page, and a {{define}} inside a page is
// reachable only from that page. Keeping this checkable means the next surface
// that reaches for it gets a clear failure instead of a 500.
//
// DELIBERATELY FILENAME-AGNOSTIC. The invariant is "these defines live in a
// partial and in exactly one place", not "they live in the file I happened to
// create". Two Phase 6 lanes hit this same 500 independently and each moved the
// defines to a partial of its own naming; a test that pinned one filename would
// fail on the other lane's merge while the invariant it exists for was
// perfectly satisfied. It also catches the outcome that actually matters when
// both partials survive a merge — partials/*.html parses ALL of them, so a
// duplicate define is resolved silently by parse order and one page's absence
// styling starts coming from a file nobody thinks is live.
func TestSharedCellDefinesLiveInPartials(t *testing.T) {
	partials, err := fs.Glob(templateFS, "templates/partials/*.html")
	if err != nil {
		t.Fatalf("glob partials: %v", err)
	}

	for _, def := range []string{`{{define "de-cell"}}`, `{{define "de-cell-value"}}`} {
		var homes []string
		for _, p := range partials {
			src, readErr := templateFS.ReadFile(p)
			if readErr != nil {
				continue
			}
			if strings.Contains(string(src), def) {
				homes = append(homes, p)
			}
		}
		switch len(homes) {
		case 0:
			t.Errorf("no partial defines %s — every page that calls it 500s at render", def)
		case 1: // the invariant
		default:
			t.Errorf("%s is defined in %d partials (%v). partials/*.html parses all of "+
				"them, so one silently wins by parse order and the absence styling on "+
				"some page comes from a file nobody thinks is live.",
				def, len(homes), homes)
		}
	}

	// And no PAGE may define them at all. A page-local copy is invisible to
	// every other page — the bug this test was written for — and drifts from
	// the shared one silently.
	pages, _ := fs.Glob(templateFS, "templates/*.html")
	for _, p := range pages {
		src, readErr := templateFS.ReadFile(p)
		if readErr != nil {
			continue
		}
		if strings.Contains(string(src), `{{define "de-cell"}}`) {
			t.Errorf("%s defines de-cell. It belongs in a partial, once — a page-local "+
				"copy is reachable from that page and from nowhere else.", p)
		}
	}
}

// TestNoSecondCellRendererUnderAnotherName is the NAME-AGNOSTIC half.
//
// ── WHY THE TEST ABOVE IS NOT ENOUGH, MEASURED ───────────────────────────────
//
// TestSharedCellDefinesLiveInPartials asserts on the strings `de-cell` and
// `de-cell-value`. It is filename-agnostic and that was the right call — two
// lanes moved the defines to partials of their own naming and it passed both.
// But it is not NAME-agnostic, and a third lane wrote the same renderer as
// `num-cell`. With partials/cells.html and partials/num-cell.html both present
// after the round-3 merge, that test PASSED: `de-cell` still lived in exactly
// one partial, and `num-cell` was invisible to every assertion in this file.
// Verified by running it in that state before consolidating.
//
// That is the whole failure mode of a name-based guard on a copy-paste problem.
// The next lane will not call it de-cell either — it will call it stat-cell, or
// value-cell, or figure — and the assertion above will pass again.
//
// So this one asserts on SHAPE. A Cell renderer is recognisable without its
// name: it dispatches on .Kind across at least two of the three states, and it
// emits a specific set of CSS classes. Two defines with the same (kinds,
// classes) signature are the same renderer whatever they are called.
//
// A genuinely NEW weight is still allowed, and that is deliberate rather than a
// gap: de-cell-value dispatches on the same kinds but emits kpi-value/tnum
// instead of the span classes, so it has its own signature and passes. The rule
// is "one implementation per rendered form", not "one define".
//
// PAGES ARE SCANNED TOO. A structural copy in a page file is the original 500 —
// invisible to every other page — and a page-local copy under a fresh name is
// exactly what the name-based check cannot see.
func TestNoSecondCellRendererUnderAnotherName(t *testing.T) {
	type home struct {
		file string
		def  string
	}
	bySignature := map[string][]home{}

	files, err := fs.Glob(templateFS, "templates/partials/*.html")
	if err != nil {
		t.Fatalf("glob partials: %v", err)
	}
	pages, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		t.Fatalf("glob pages: %v", err)
	}
	files = append(files, pages...)

	for _, f := range files {
		raw, readErr := templateFS.ReadFile(f)
		if readErr != nil {
			continue
		}
		for name, body := range templateDefines(stripTemplateComments(string(raw))) {
			sig, ok := cellRendererSignature(body)
			if !ok {
				continue
			}
			bySignature[sig] = append(bySignature[sig], home{file: f, def: name})
		}
	}

	if len(bySignature) == 0 {
		t.Fatal("no Cell renderer found in any template. Either the doctrine was deleted or " +
			"cellRendererSignature has stopped recognising it — and a guard that recognises " +
			"nothing passes forever.")
	}

	for sig, homes := range bySignature {
		if len(homes) < 2 {
			continue
		}
		var names []string
		for _, h := range homes {
			names = append(names, h.def+" in "+h.file)
		}
		sort.Strings(names)
		t.Errorf("%d defines are the SAME Cell renderer under different names: %s\n"+
			"  signature: %s\n"+
			"  partials/*.html parses all of them, so a caller silently gets whichever "+
			"copy it happens to name, and the two copies drift the first time one is "+
			"corrected. Three round-3 lanes wrote this renderer independently; keep one, "+
			"in a partial, and repoint the callers.", len(homes), strings.Join(names, "; "), sig)
	}
}

var (
	reTemplateAction = regexp.MustCompile(`\{\{-?\s*(\w+)`)
	reKindLiteral    = regexp.MustCompile(`eq\s+\.Kind\s+"([a-z_]+)"`)
	reClassAttr      = regexp.MustCompile(`class="([^"]*)"`)
)

// templateDefines returns each {{define "name"}}…{{end}} body in src.
//
// Depth-counted rather than matched on the first {{end}}: every one of these
// renderers is a three-arm {{if}}/{{else if}}/{{else}}, so a first-{{end}} scan
// would cut the body off inside the value arm and the signature would miss the
// two classes that distinguish it.
func templateDefines(src string) map[string]string {
	out := map[string]string{}
	const open = `{{define "`
	for i := 0; ; {
		j := strings.Index(src[i:], open)
		if j < 0 {
			return out
		}
		start := i + j + len(open)
		q := strings.Index(src[start:], `"`)
		if q < 0 {
			return out
		}
		name := src[start : start+q]
		rest := src[start+q:]
		if k := strings.Index(rest, "}}"); k >= 0 {
			rest = rest[k+2:]
		}
		depth := 1
		body := rest
		for _, m := range reTemplateAction.FindAllStringSubmatchIndex(rest, -1) {
			switch rest[m[2]:m[3]] {
			case "if", "range", "with", "block", "define":
				depth++
			case "end":
				depth--
				if depth == 0 {
					body = rest[:m[0]]
				}
			}
			if depth == 0 {
				break
			}
		}
		out[name] = body
		i = start + q
	}
}

// cellRendererSignature reduces a define body to what makes it a Cell renderer,
// with the name — the only thing three lanes disagreed about — thrown away.
//
// Two components, and both are needed. The .Kind literals alone would collide
// de-cell with de-cell-value, which are two legitimate weights of one doctrine.
// The class set alone would collide any two defines that happen to style the
// same thing without dispatching on absence at all.
//
// Reported as not-a-renderer unless it dispatches on at least TWO kinds: a
// single mention of .Kind is a caller or a special case, not the doctrine.
func cellRendererSignature(body string) (string, bool) {
	kinds := map[string]bool{}
	for _, m := range reKindLiteral.FindAllStringSubmatch(body, -1) {
		kinds[m[1]] = true
	}
	if len(kinds) < 2 {
		return "", false
	}
	classes := map[string]bool{}
	for _, m := range reClassAttr.FindAllStringSubmatch(body, -1) {
		// A class attribute built from a template action is not a literal class
		// set and cannot be compared as one.
		if strings.Contains(m[1], "{{") {
			continue
		}
		for _, tok := range strings.Fields(m[1]) {
			classes[tok] = true
		}
	}
	return "kinds=" + sortedKeys(kinds) + " classes=" + sortedKeys(classes), true
}

func sortedKeys(m map[string]bool) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// TestPlainValueCellEmitsNoEmptyTitle holds the rendering decision the merge
// made, at the markup level.
//
// The three round-3 copies disagreed here and only here. num-cell's plain-value
// arm emitted `<span title="{{.Title}}">`; de-cell's emitted bare text. Value()
// leaves Title empty, so num-cell put title="" on every plain value that had
// nothing to say — five per healthy /cycle-time row, measured. de-cell as-is
// dropped the one plain value that DOES have something to say: the Tail cut
// column's derivation.
//
// The merged partial emits the attribute if and only if there is a title, so
// both halves of that are assertions and this test holds both.
//
// VERIFIED RED BY: reverting the value arm to num-cell's unconditional
// `<span title="{{.Title}}">` — the empty-title assertion fired with 5. And by
// reverting it to de-cell's bare `{{.Text}}` — the Tail-cut assertion fired.
func TestPlainValueCellEmitsNoEmptyTitle(t *testing.T) {
	c := disp()
	rows := []EpisodeRow{{
		Expected:    Value("4"),                       // a plain value, no title
		Ratio:       Value("50%"),                     // ditto
		CloseReason: NoData("no reason was recorded"), // an absence, title required
		Cause:       NA("no children yet"),
		ClosedBy:    Value("sweep"),
		OriginID:    "o1", KindLabel: "cell", OpenedAt: time.Now().UTC(),
	}}
	out := renderPage(t, "demand-episodes.html", map[string]any{
		"Page": "demand-episodes", "WindowHours": 24,
		"WorryAfter": FormatDuration(c.WorryAfter), "ConcernAfter": FormatDuration(c.ConcernAfter),
		"MinExpected": c.MinExpectedOrders, "Rows": rows, "Limit": 200,
	})
	if n := strings.Count(out, `title=""`); n != 0 {
		t.Errorf("rendered /demand-episodes carries %d empty title attributes. An empty title "+
			"is not a neutral no-op: HTML defines it as asserting that no advisory text applies "+
			"here AND that an ancestor's does not either, so it suppresses a tooltip rather than "+
			"deferring to one. Emit the attribute only when there is something to say.", n)
	}
	// And the other half: a value WITH a title still gets one.
	titled := renderPage(t, "demand-episodes.html", map[string]any{
		"Page": "demand-episodes", "WindowHours": 24,
		"WorryAfter": FormatDuration(c.WorryAfter), "ConcernAfter": FormatDuration(c.ConcernAfter),
		"MinExpected": c.MinExpectedOrders, "Limit": 200,
		"Rows": []EpisodeRow{{
			OriginID: "o2", KindLabel: "cell", OpenedAt: time.Now().UTC(),
			Expected: Cell{Kind: CellValue, Text: "4", Title: "why four"},
		}},
	})
	if !strings.Contains(titled, `title="why four"`) {
		t.Error(`a plain value carrying a Title rendered without it. The Tail cut column on ` +
			`/cycle-time is exactly this case — its title spells out "median + k × (p90 − ` +
			`median)" for a number nobody can derive from the row — and dropping it is what ` +
			`taking de-cell's arm unchanged would have done.`)
	}
}

// stripTemplateComments removes {{/* ... */}} blocks.
//
// Needed because the Phase 6 templates document their own constraints in
// prose, and a source-level rule that cannot tell a rule from its explanation
// fires on the files that follow it — while a file that quietly used a chip
// and said nothing would pass. Checking the markup instead of the file makes
// the test measure the thing it names.
func stripTemplateComments(s string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, "{{/*")
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		j := strings.Index(s[i:], "*/}}")
		if j < 0 {
			return b.String()
		}
		s = s[i+j+len("*/}}"):]
	}
}
