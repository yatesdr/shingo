package www

import (
	"bytes"
	"html/template"
	"io/fs"
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

	base := template.New("").Funcs(templateFuncs())
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
