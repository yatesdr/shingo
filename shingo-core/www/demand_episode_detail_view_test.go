package www

import (
	"bytes"
	"errors"
	"html/template"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingocore/domain"
)

// demand_episode_detail_view_test.go — the guards on origin-indexed forensics
// (5.12).
//
// No build tag, same reason as demand_episodes_view_test.go: these are pure
// functions and the rules they carry must be checked on every push without
// Docker. `disp()` and `at()` come from that file.

func openedAt() time.Time { return time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC) }

// originFor builds a minimal open episode.
func originFor() domain.DemandOrigin {
	return domain.DemandOrigin{
		OriginID:    "0f9a1c22-0000-0000-0000-000000000001",
		EpisodeKey:  "cell|devplant.line1|SNF2|PANEL-A|supply",
		Kind:        "cell",
		Direction:   "supply",
		Trigger:     "autoreorder",
		TriggerRef:  "claim-77",
		StationID:   "devplant.line1",
		ProcessID:   "SNF2",
		PayloadCode: "PANEL-A",
		OpenedAt:    openedAt(),
	}
}

func childOrder(id int64, status protocol.Status) *domain.Order {
	return &domain.Order{
		ID:           id,
		EdgeUUID:     "uuid-" + string(rune('a'+id)),
		OrderType:    "move",
		Status:       status,
		SourceNode:   "SMN_001",
		DeliveryNode: "PLN_01",
		PayloadCode:  "PANEL-A",
		OriginClass:  protocol.OriginClassAttached,
		CreatedAt:    openedAt().Add(2 * time.Minute),
	}
}

// ── THE RULE THIS PAGE EXISTS FOR ────────────────────────────────────────────

// TestChildReadFailureIsNotZeroOrders is the 5.12 guard, and it is the reason
// the builder takes the read error as a parameter.
//
// An episode with zero orders against a real demand is the worst thing the
// demand grain can show: a place asked for material and nothing was ever
// created. An episode whose child query FAILED renders identically the moment
// the error is dropped — an empty table under a real demand. One sends someone
// to the floor; the other sends them to the wrong floor, and leaves the broken
// read undiscovered because the page looks like it worked.
//
// The assertion is not "the error is stored somewhere". It is that the COUNT
// itself refuses to be a number, because the count is what a reader looks at.
func TestChildReadFailureIsNotZeroOrders(t *testing.T) {
	o := originFor()
	now := openedAt().Add(10 * time.Minute)

	failed := BuildEpisodeDetail(o, nil, false, errors.New("dial tcp: connection refused"), now, disp())
	measured := BuildEpisodeDetail(o, nil, false, nil, now, disp())

	// The measured zero is a real number and must print as one. Dashing out a
	// true zero hides the finding from the other direction.
	if measured.Orders.Kind != CellValue || measured.Orders.Text != "0" {
		t.Errorf("a measured zero must render as the number 0: kind=%q text=%q",
			measured.Orders.Kind, measured.Orders.Text)
	}
	if measured.ChildrenError != "" {
		t.Errorf("a successful read must carry no error, got %q", measured.ChildrenError)
	}

	// The unread one must not.
	if failed.Orders.Kind != CellNoData {
		t.Errorf("a failed child read must render the order count as no-data, got kind=%q text=%q",
			failed.Orders.Kind, failed.Orders.Text)
	}
	if failed.Orders.Text == measured.Orders.Text {
		t.Errorf("an unread order count and a measured zero rendered identically as %q — "+
			"that is the whole failure this page exists to prevent", failed.Orders.Text)
	}
	if !strings.Contains(failed.ChildrenError, "connection refused") {
		t.Errorf("the read failure must survive verbatim, got %q", failed.ChildrenError)
	}
	if failed.Children != nil {
		t.Errorf("a failed read must produce no child rows, got %d", len(failed.Children))
	}

	// And the ratio, whose numerator is the same unread count.
	if failed.Ratio.Kind != CellNoData {
		t.Errorf("with an unread numerator the ratio must be no-data, got kind=%q text=%q",
			failed.Ratio.Kind, failed.Ratio.Text)
	}
}

// TestExpectedSurvivesAFailedChildRead pins the other half: the header is still
// worth rendering when the children could not be read.
//
// The expectation is what tells a reader whether the missing children matter —
// an episode expecting six orders whose child list is unknown is a different
// situation from one expecting one. A handler that bailed on the child error
// would throw that away, so the builder must keep it.
func TestExpectedSurvivesAFailedChildRead(t *testing.T) {
	o := originFor()
	six := 6
	o.ExpectedOrders = &six

	d := BuildEpisodeDetail(o, nil, false, errors.New("read failed"), openedAt().Add(time.Minute), disp())

	if d.Expected.Kind != CellValue || d.Expected.Text != "6" {
		t.Errorf("expected orders must survive a failed CHILD read: kind=%q text=%q",
			d.Expected.Kind, d.Expected.Text)
	}
	if d.EpisodeKey != o.EpisodeKey || d.Station != o.StationID {
		t.Errorf("the header must render intact: key=%q station=%q", d.EpisodeKey, d.Station)
	}
}

// ── The three absence states, on a child row ─────────────────────────────────

// TestThreeAbsenceStatesOnAChildRowStayApart is the number doctrine applied one
// level down.
//
// Both cells here have all three states reachable and the discriminators are
// FACTS on the row, not guesses from the status vocabulary:
//
//	Robot  value   — a robot is recorded
//	       n/a     — the order never reached the fleet (no vendor order id), so
//	                 there was never a robot to name
//	       no data — it DID reach the fleet and no robot was recorded
//
//	Took   value   — it completed, and this is how long it took
//	       n/a     — it is still running: a question not yet asked
//	       no data — it is terminal with no completion time: a missing answer
//
// The test asserts the three are pairwise different in BOTH kind and rendered
// text, because a distinction that survives only in a struct field the template
// does not read is not a distinction a reader gets.
func TestThreeAbsenceStatesOnAChildRowStayApart(t *testing.T) {
	completed := openedAt().Add(9 * time.Minute)

	withRobot := childOrder(1, protocol.StatusInTransit)
	withRobot.RobotID = "AMR-04"
	withRobot.VendorOrderID = "v-1"

	neverDispatched := childOrder(2, protocol.StatusCancelled)

	lostRobot := childOrder(3, protocol.StatusConfirmed)
	lostRobot.VendorOrderID = "v-3"
	lostRobot.CompletedAt = &completed

	running := childOrder(4, protocol.StatusInTransit)
	running.RobotID = "AMR-09"
	running.VendorOrderID = "v-4"

	d := BuildEpisodeDetail(originFor(),
		[]*domain.Order{withRobot, neverDispatched, lostRobot, running},
		false, nil, openedAt().Add(20*time.Minute), disp())

	if len(d.Children) != 4 {
		t.Fatalf("want 4 child rows, got %d", len(d.Children))
	}

	robots := map[string]Cell{
		"recorded":         d.Children[0].Robot,
		"never dispatched": d.Children[1].Robot,
		"lost":             d.Children[2].Robot,
	}
	assertThreeStatesDiffer(t, "robot", robots["recorded"], robots["never dispatched"], robots["lost"],
		CellValue, CellNA, CellNoData)

	assertThreeStatesDiffer(t, "took",
		d.Children[2].Took, // confirmed with a completion time
		d.Children[3].Took, // still running
		d.Children[1].Took, // cancelled with no completion time
		CellValue, CellNA, CellNoData)
}

// assertThreeStatesDiffer holds the pairwise check in one place so a new cell
// can be added to the page without re-deriving what "different" means.
func assertThreeStatesDiffer(t *testing.T, label string, value, na, nodata Cell, wantValue, wantNA, wantNoData CellKind) {
	t.Helper()
	for _, c := range []struct {
		name string
		got  Cell
		want CellKind
	}{
		{"value", value, wantValue},
		{"n/a", na, wantNA},
		{"no data", nodata, wantNoData},
	} {
		if c.got.Kind != c.want {
			t.Errorf("%s %s: kind is %q, want %q (text=%q)", label, c.name, c.got.Kind, c.want, c.got.Text)
		}
	}
	if na.Text == nodata.Text {
		t.Errorf("%s: n/a and no-data both render as %q — two of the three states have collapsed",
			label, na.Text)
	}
	if value.Text == na.Text || value.Text == nodata.Text {
		t.Errorf("%s: a measured value renders the same as an absence (%q)", label, value.Text)
	}
	// Every absence must say WHICH absence it is. A dash with no title is a
	// dash the reader cannot resolve.
	if strings.TrimSpace(na.Title) == "" {
		t.Errorf("%s: the n/a cell carries no title saying why the question does not apply", label)
	}
	if strings.TrimSpace(nodata.Title) == "" {
		t.Errorf("%s: the no-data cell carries no title saying which absence it is", label)
	}
}

// ── The timeline ─────────────────────────────────────────────────────────────

// TestOffsetFromOpenIsSignedNotClamped guards the column that makes this page a
// timeline instead of a list.
//
// An order created BEFORE its own episode opened is a real possibility — Edge
// and Core clocks are independent, and an order stamped forward from a parent
// can carry an origin minted after it. FormatDuration clamps negatives to zero
// for display, which is right for a duration and wrong here: it would render the
// anomaly as "+0 s", i.e. as the most ordinary reading on the page, erasing
// exactly what someone opened this surface to find.
func TestOffsetFromOpenIsSignedNotClamped(t *testing.T) {
	early := childOrder(1, protocol.StatusConfirmed)
	early.CreatedAt = openedAt().Add(-90 * time.Second)

	late := childOrder(2, protocol.StatusConfirmed)
	late.CreatedAt = openedAt().Add(90 * time.Second)

	d := BuildEpisodeDetail(originFor(), []*domain.Order{early, late}, false, nil,
		openedAt().Add(time.Hour), disp())

	got := d.Children[0].SinceOpen
	if !strings.HasPrefix(got.Text, "−") {
		t.Errorf("an order created before its episode opened must render a signed negative offset, got %q", got.Text)
	}
	if !strings.Contains(strings.ToLower(got.Title), "before") {
		t.Errorf("the negative offset must say what it means, got title %q", got.Title)
	}
	if d.Children[0].SinceOpen.Text == d.Children[1].SinceOpen.Text {
		t.Errorf("90 seconds early and 90 seconds late both rendered as %q", got.Text)
	}
	if !strings.HasPrefix(d.Children[1].SinceOpen.Text, "+") {
		t.Errorf("a normal offset must be signed too, got %q", d.Children[1].SinceOpen.Text)
	}
}

// TestOpenEpisodeDetailUsesTheInjectedClock pins that the header's duration
// comes from the passed clock, not from time.Now inside the builder.
func TestOpenEpisodeDetailUsesTheInjectedClock(t *testing.T) {
	d := BuildEpisodeDetail(originFor(), nil, false, nil, openedAt().Add(at(50)), disp())

	if d.DurationText != "50m" {
		t.Errorf("duration: got %q, want %q — the builder is reading a clock it was not given",
			d.DurationText, "50m")
	}
	if d.Band != BandWorry {
		t.Errorf("50 minutes against a 45m worry line must band as worry, got %q", d.Band)
	}
}

// ── Attribution ──────────────────────────────────────────────────────────────

// TestOriginClassDefaultRendersUnknownAsItself applies rule 3 to a fourth
// vocabulary.
//
// origin_class has three values today and is the subject of an open plan item
// (5.7, the orphan lane), so it is a vocabulary actively expected to move. Same
// argument as close_reason, which grew twice: a default of "Unknown" turns the
// next addition into silent data loss on the one page built to notice it.
func TestOriginClassDefaultRendersUnknownAsItself(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{protocol.OriginClassAttached, "Attached"},
		{protocol.OriginClassOrphan, "Orphan"},
		{protocol.OriginClassNoDemand, "No demand"},
		{"", ""},
		// Deliberately not in today's vocabulary — a test listing only today's
		// values goes vacuous the moment the vocabulary grows, which is the
		// event it exists to survive.
		{"aged_out", "Aged out"},
		{"reattached_by_sweep", "Reattached by sweep"},
	} {
		if got := OriginClassLabel(tc.in); got != tc.want {
			t.Errorf("OriginClassLabel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestAttributionDisagreementIsFlagged pins the finding this column exists for.
//
// Every row on this page was found BY its origin_id. If origin_class says
// anything other than `attached`, the two columns disagree about the same order
// — the row has an origin and is simultaneously classed as having none. That is
// a data-integrity finding, not a display detail, and normalising it away is how
// it would stay unnoticed.
func TestAttributionDisagreementIsFlagged(t *testing.T) {
	attached := childOrder(1, protocol.StatusConfirmed)

	orphaned := childOrder(2, protocol.StatusConfirmed)
	orphaned.OriginClass = protocol.OriginClassOrphan

	unclassed := childOrder(3, protocol.StatusConfirmed)
	unclassed.OriginClass = ""

	d := BuildEpisodeDetail(originFor(), []*domain.Order{attached, orphaned, unclassed},
		false, nil, openedAt().Add(time.Hour), disp())

	if d.Children[0].OriginClassDisagrees {
		t.Error("an attached order must not be flagged as disagreeing")
	}
	if !d.Children[1].OriginClassDisagrees {
		t.Error("an order reached by origin_id but classed `orphan` must be flagged — " +
			"the two columns disagree about the same row")
	}
	if !d.Children[2].OriginClassDisagrees {
		t.Error("an order reached by origin_id with NO class must be flagged too — " +
			"an empty class is the absence of a class, not agreement with `attached`")
	}
}

// ── Kind-scoped fields ───────────────────────────────────────────────────────

// TestThresholdAndTriggerRefAreNotApplicableOnTheWrongKind guards two fields
// that would otherwise print a real-looking zero.
//
// A Core-minted threshold episode has no trigger reference because NOTHING
// ASKED — a monitor noticed a level crossed. A cell episode has no threshold at
// all, and `threshold` is an int, so the default rendering is `0`: "the
// threshold is zero", which is a statement about the floor that nobody made.
// Both are the question-does-not-apply state, not the missing-data one.
func TestThresholdAndTriggerRefAreNotApplicableOnTheWrongKind(t *testing.T) {
	now := openedAt().Add(time.Minute)

	cell := originFor() // kind=cell, has a trigger ref, no threshold
	cd := BuildEpisodeDetail(cell, nil, false, nil, now, disp())
	if cd.Threshold.Kind != CellNA {
		t.Errorf("a cell episode has no threshold: kind=%q text=%q", cd.Threshold.Kind, cd.Threshold.Text)
	}
	if cd.TriggerRef.Kind != CellValue || cd.TriggerRef.Text != "claim-77" {
		t.Errorf("a cell episode's trigger ref must render: kind=%q text=%q",
			cd.TriggerRef.Kind, cd.TriggerRef.Text)
	}

	thr := originFor()
	thr.Kind = "threshold"
	thr.TriggerRef = ""
	thr.Threshold = 12
	td := BuildEpisodeDetail(thr, nil, false, nil, now, disp())
	if td.Threshold.Kind != CellValue || td.Threshold.Text != "12" {
		t.Errorf("a threshold episode's threshold must render: kind=%q text=%q",
			td.Threshold.Kind, td.Threshold.Text)
	}
	if td.TriggerRef.Kind != CellNA {
		t.Errorf("a Core-minted threshold episode has no trigger reference — nothing asked: kind=%q",
			td.TriggerRef.Kind)
	}
	if td.Threshold.Text == cd.Threshold.Text {
		t.Errorf("a real threshold and an inapplicable one both render as %q", td.Threshold.Text)
	}
}

// ── The links ────────────────────────────────────────────────────────────────

// TestChildRowsLinkToTheirExistingMissionDetail pins the whole point of the
// page: the children are a way IN, not a list of numbers.
//
// The route shape has one home in Go rather than being assembled in a template,
// so a rename fails here instead of producing a dead link nobody clicks until
// they need it. The handler-level test asserts the rendered page carries this
// exact string.
func TestChildRowsLinkToTheirExistingMissionDetail(t *testing.T) {
	if got := MissionHref(4211); got != "/missions/4211" {
		t.Errorf("MissionHref: got %q, want %q", got, "/missions/4211")
	}
	if got := EpisodeDetailHref("abc-123"); got != "/demand-episodes/abc-123" {
		t.Errorf("EpisodeDetailHref: got %q, want %q", got, "/demand-episodes/abc-123")
	}

	d := BuildEpisodeDetail(originFor(), []*domain.Order{childOrder(77, protocol.StatusConfirmed)},
		false, nil, openedAt().Add(time.Hour), disp())
	if d.Children[0].MissionHref != MissionHref(77) {
		t.Errorf("child row href: got %q, want %q", d.Children[0].MissionHref, MissionHref(77))
	}
}

// TestTruncationIsCarriedNotSwallowed pins that a capped child list says so.
//
// Children-per-episode is unmeasured at a plant. A truncated list rendered as
// though it were complete is a page that lies about what a demand cost — the
// same argument ListDemandEpisodes makes for its own cap.
func TestTruncationIsCarriedNotSwallowed(t *testing.T) {
	d := BuildEpisodeDetail(originFor(), []*domain.Order{childOrder(1, protocol.StatusConfirmed)},
		true, nil, openedAt().Add(time.Hour), disp())
	if !d.ChildrenTruncated {
		t.Error("the cap bit and the page was not told")
	}
	// And it must never be set on the branch where nothing was read at all —
	// "there are more than 500" is a claim a failed query cannot support.
	failed := BuildEpisodeDetail(originFor(), nil, true, errors.New("boom"),
		openedAt().Add(time.Hour), disp())
	if failed.ChildrenTruncated {
		t.Error("a failed read must not claim the list was truncated")
	}
}

// ── The template's own branches ──────────────────────────────────────────────
//
// The two tests below execute the real template, because the branch that keeps
// a read failure from rendering as an empty floor lives in markup and a rule in
// a template is a rule with no test. They parse the same way NewRouter does —
// base (layout + partials) cloned per page — which is also what proves the
// de-cell partial is reachable from a page that does not define it.

func renderEpisodeDetail(t *testing.T, d EpisodeDetail) string {
	t.Helper()
	base := template.Must(template.New("").Funcs(templateFuncs()).
		ParseFS(templateFS, "templates/layout.html", "templates/partials/*.html"))
	page := template.Must(template.Must(base.Clone()).
		ParseFS(templateFS, "templates/demand-episode.html"))

	var buf bytes.Buffer
	if err := page.ExecuteTemplate(&buf, "content", map[string]any{
		"Detail":       d,
		"OriginID":     d.OriginID,
		"MinExpected":  2,
		"WorryAfter":   "45m",
		"ConcernAfter": "60m",
	}); err != nil {
		t.Fatalf("execute demand-episode.html: %v", err)
	}
	return buf.String()
}

// TestTemplateNeverRendersAnUnreadChildListAsAnEmptyFloor is the markup half of
// the rule this page exists for.
//
// The builder refuses to call an unread count zero; this asserts the template
// does not undo that by falling into its own empty-state branch. An empty state
// reading "no orders were created for this demand" under a query that never ran
// is the most alarming sentence on this surface, printed about nothing.
func TestTemplateNeverRendersAnUnreadChildListAsAnEmptyFloor(t *testing.T) {
	unread := BuildEpisodeDetail(originFor(), nil, false,
		errors.New("dial tcp: connection refused"), openedAt().Add(time.Minute), disp())

	body := renderEpisodeDetail(t, unread)

	if !strings.Contains(body, "Could not read this episode&#39;s orders") &&
		!strings.Contains(body, "Could not read this episode's orders") {
		t.Error("a failed child read must announce itself on the page")
	}
	if strings.Contains(body, "No orders were created for this demand") {
		t.Error("an UNREAD child list rendered the measured-zero empty state — the page is " +
			"telling someone a demand went unserved when nothing was counted at all")
	}
	if !strings.Contains(body, "connection refused") {
		t.Error("the underlying error must survive to the page")
	}
}

// TestTemplateRendersAMeasuredZeroAsAMeasurement is the other side. A true zero
// is the worst thing the demand grain can show and must read loudly AS A
// MEASUREMENT, or it is indistinguishable from the case above.
func TestTemplateRendersAMeasuredZeroAsAMeasurement(t *testing.T) {
	measured := BuildEpisodeDetail(originFor(), nil, false, nil,
		openedAt().Add(time.Minute), disp())

	body := renderEpisodeDetail(t, measured)

	if !strings.Contains(body, "No orders were created for this demand") {
		t.Error("a measured zero must get its own empty state")
	}
	if !strings.Contains(body, "measured zero") {
		t.Error("the empty state must say the query RAN")
	}
	if strings.Contains(body, "Could not read this episode") {
		t.Error("a measured zero rendered as a read failure")
	}
}

// TestSharedCellPartialKeepsTheThreeStatesApartInMarkup guards the MOVE.
//
// de-cell and de-cell-value were defined inside demand-episodes.html until this
// page needed them. Page templates are cloned from a base holding layout +
// partials, so a define in a page file is invisible to every other page — the
// obvious fix is to copy them, and two copies of "render nothing" is exactly how
// two of the three states end up collapsed on one surface and not the other.
// This asserts the one shared copy still renders three distinguishable things.
func TestSharedCellPartialKeepsTheThreeStatesApartInMarkup(t *testing.T) {
	base := template.Must(template.New("").Funcs(templateFuncs()).
		ParseFS(templateFS, "templates/layout.html", "templates/partials/*.html"))

	render := func(name string, c Cell) string {
		var buf bytes.Buffer
		if err := base.ExecuteTemplate(&buf, name, c); err != nil {
			t.Fatalf("execute %s: %v", name, err)
		}
		return strings.Join(strings.Fields(buf.String()), " ")
	}

	for _, name := range []string{"de-cell", "de-cell-value"} {
		value := render(name, Value("0"))
		nodata := render(name, NoData("the feed is down"))
		na := render(name, NA("does not apply"))

		if value == nodata || value == na || nodata == na {
			t.Errorf("%s: two of the three absence states render identically\n"+
				"  value : %s\n  nodata: %s\n  na    : %s", name, value, nodata, na)
		}
		// AND EACH KEEPS ITS OWN CLASS. Found by verify-red: deleting the nodata
		// arm so it fell through to the na arm left the comparison above GREEN,
		// because the two still differed by GLYPH ("—" versus "n/a"). They would
		// have been styled identically — .de-nodata is --text-muted, .de-na is
		// --sub-3 at a smaller size — so the weight distinction the style guide
		// asks for was gone while the test said it was fine. A distinction
		// carried only by the character is one stylesheet edit from being
		// carried by nothing.
		if !strings.Contains(nodata, "de-nodata") {
			t.Errorf("%s: the no-data state lost its own class: %s", name, nodata)
		}
		if !strings.Contains(na, `de-na"`) {
			t.Errorf("%s: the n/a state lost its own class: %s", name, na)
		}
		if strings.Contains(value, "de-nodata") || strings.Contains(value, `de-na"`) {
			t.Errorf("%s: a measured value picked up an absence class: %s", name, value)
		}
		// A measured zero must survive as the digit. This is the COALESCE(x,0)
		// failure with the sign flipped — an absence printed as zero is the bug,
		// and a zero printed as an absence is the same bug pointed the other way.
		if !strings.Contains(value, "0") {
			t.Errorf("%s: a measured zero did not render its digit: %s", name, value)
		}
		if !strings.Contains(nodata, "the feed is down") {
			t.Errorf("%s: no-data lost the title saying WHICH absence it is: %s", name, nodata)
		}
	}
}
