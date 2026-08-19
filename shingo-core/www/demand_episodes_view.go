package www

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"shingo/protocol"
	"shingocore/config"
	"shingocore/domain"
)

// demand_episodes_view.go — the demand browser's view model (Stage 5: 5.1, 5.4,
// 5.5, 5.6, and 5.14's rendering).
//
// EVERYTHING HERE IS A PURE FUNCTION OF (row, constants). No database, no
// clock reaching for time.Now, no template. That is deliberate: the rules this
// file encodes are the ones most likely to be got wrong and least likely to be
// noticed, so they have to be testable without Postgres and without a browser.
//
// The rules, and where each comes from:
//
//  1. NO DATA, ZERO AND NOT APPLICABLE RENDER DIFFERENTLY FROM EACH OTHER.
//     docs/ui-style-guide.md § The numbers themselves, rule 4. This is the one
//     the surface exists to get right: ZERO ORDERS AGAINST A REAL DEMAND is the
//     plan's worst case, and a page that cannot tell it from a dead feed sends
//     someone to inspect a healthy cell and says nothing on the day the feed
//     breaks. The distinction is carried by a TYPE (Cell), not by CSS, because
//     if it arrives at the renderer as a plain int it was destroyed upstream and
//     no styling recovers it.
//
//  2. A SMOOTH SCALE MUST NEVER ENCODE A THRESHOLD. The teal ramp carries
//     magnitude only up to WorryAfter. Crossing WorryAfter and crossing
//     ConcernAfter each take their own channel — a band name printed as text
//     plus a ring class — never colour alone. Were ConcernAfter simply the top
//     of the ramp, a 61-minute episode and a three-hour one would render
//     identically, losing all discrimination exactly past the line where it
//     matters most.
//
//  3. AN UNRECOGNISED VALUE IS NOT AN ABSENT ONE (5.5). Every close_reason and
//     closed_by switch renders an unknown value AS ITSELF. That vocabulary has
//     already grown twice — claim_removed, superseded — and a default rendering
//     "unknown" throws away the only information the row had.
//
//  4. NO DISPLAY CONSTANT IS A LITERAL. Every number comes from
//     config.DisplayConfig and has a provenance record. See config/provenance.go.

// ── Cell: a value that knows whether it had a value ──────────────────────────

// CellKind distinguishes the three states rule 4 of the number doctrine keeps
// apart. It exists because `int` cannot: Go's zero value is indistinguishable
// from unset, which is the same bug as COALESCE(x, 0) on a display column and
// `x || 0` in JavaScript.
type CellKind string

const (
	// CellValue — we measured, and this is the answer. INCLUDING ZERO. A real
	// measured zero renders as a plain 0 in normal text; dashing out true zeros
	// hides the finding from the other direction.
	CellValue CellKind = "value"

	// CellNoData — we have not heard, the window holds no rows, or the source is
	// unreachable. Renders as an em dash in muted text, with a title saying
	// WHICH of those it is.
	CellNoData CellKind = "nodata"

	// CellNA — the question does not apply to this row. Renders as the quietest
	// possible mark. An open episode has no close reason: that is not missing
	// data, it is a question that has not been asked yet.
	CellNA CellKind = "na"
)

// Cell is one rendered figure plus what is known about it.
//
// Title is not decoration. For CellNoData it is the only place the page says
// WHICH absence this is, and the style guide requires it.
type Cell struct {
	Kind  CellKind
	Text  string
	Title string

	// Muted greys a real value whose denominator is too small to support it
	// (5.4). Distinct from CellNoData: the number exists and is shown, it is
	// just not load-bearing. Greying a value and dashing it out are different
	// claims and must not collapse into one.
	Muted bool
}

// Value builds a measured cell. Use for anything that was actually counted,
// including zero.
func Value(text string) Cell { return Cell{Kind: CellValue, Text: text} }

// NoData builds an absence, and REQUIRES a reason — the style guide's "with a
// title saying which of those it is" is not optional, and a signature that
// allowed the reason to be omitted would make omitting it the easy path.
func NoData(why string) Cell {
	return Cell{Kind: CellNoData, Text: "—", Title: why}
}

// NA builds a not-applicable cell.
func NA(why string) Cell {
	return Cell{Kind: CellNA, Text: "n/a", Title: why}
}

// ── Duration bands (5.14) ────────────────────────────────────────────────────

// DurationBand is which side of the two SME-judged lines an episode sits on.
type DurationBand string

const (
	BandCalm    DurationBand = "calm"
	BandWorry   DurationBand = "worry"
	BandConcern DurationBand = "concern"
)

// Label is the band's PRINTED name. Rule: crossing a line takes its own channel,
// and that channel is text plus a ring — never colour alone. A reader with any
// form of colour vision deficiency, or looking at a printed screenshot, gets the
// same information.
func (b DurationBand) Label() string {
	switch b {
	case BandWorry:
		return "Worry"
	case BandConcern:
		return "Concern"
	case BandCalm:
		return ""
	default:
		// Rule 3 applied to this vocabulary too: an unrecognised band renders as
		// itself rather than silently as calm.
		return string(b)
	}
}

// BandFor classifies a duration against the two configured lines.
//
// Boundaries are INCLUSIVE of the line: exactly 45 minutes is Worry, exactly 60
// is Concern. A line an episode can sit exactly on and be treated as below it is
// a line that reads differently to the person who set it.
func BandFor(d time.Duration, c config.DisplayConfig) DurationBand {
	switch {
	case d >= c.ConcernAfter:
		return BandConcern
	case d >= c.WorryAfter:
		return BandWorry
	default:
		return BandCalm
	}
}

// RampStep returns which step of the --viz-seq-N teal ramp a duration renders
// at, from 1 to c.RampSteps, or 0 for "no ramp at all".
//
// THE RAMP STOPS AT WorryAfter. Past that line magnitude is carried by the band
// name and the ring, not by the colour, which is why this saturates rather than
// continuing. Calm is graduated; alerts are loud and separate.
//
// A zero or negative duration returns 0 — no ramp — rather than step 1. An
// episode that has just opened has no magnitude to show, and painting it the
// palest teal would make "nothing has happened yet" look like a small amount of
// something.
func RampStep(d time.Duration, c config.DisplayConfig) int {
	if d <= 0 || c.RampSteps <= 0 || c.WorryAfter <= 0 {
		return 0
	}
	if d >= c.WorryAfter {
		return c.RampSteps
	}
	// CEIL, NOT TRUNCATE. The steps bin the half-open range (0, WorryAfter) into
	// RampSteps equal buckets — with the default 45m/5 that is (0,9] (9,18]
	// (18,27] (27,36] (36,45). Truncating instead would give the top step to
	// exactly one instant, the worry line itself, which would make step 5 a
	// synonym for the band rather than the last step of the calm range and leave
	// the ramp visually short of its own top for every real value.
	step := int(math.Ceil(float64(d) / float64(c.WorryAfter) * float64(c.RampSteps)))
	if step < 1 {
		step = 1
	}
	if step > c.RampSteps {
		step = c.RampSteps
	}
	return step
}

// ── close_reason and closed_by (5.5, 5.6) ────────────────────────────────────

// CloseReasonLabel renders a close reason for display.
//
// THE DEFAULT RENDERS THE VALUE AS ITSELF. This is 5.5, and the reason is
// concrete: the vocabulary has grown twice already — claim_removed and
// superseded were both added after the first switches over it were written — and
// a default rendering "unknown" or "" would have turned each of those additions
// into a silent data-loss bug in the UI. The row's only information is the
// string it carries; a renderer that discards it in favour of a constant has
// destroyed the one thing it was given.
//
// Underscores become spaces so an unrecognised value reads as prose rather than
// as a leaked identifier, but the WORD IS NEVER CHANGED — no truncation, no
// substitution, no fallback.
func CloseReasonLabel(reason string) string {
	switch reason {
	case "":
		// Empty is a different case from unrecognised: there is no value here at
		// all. The caller decides whether that is n/a (episode still open) or no
		// data (closed with nothing recorded); this function only says it is
		// empty.
		return ""
	case "recovered":
		return "Recovered"
	case "changeover_complete":
		return "Changeover complete"
	case "cancelled":
		return "Cancelled"
	case "threshold_changed":
		return "Threshold changed"
	case "threshold_removed":
		return "Threshold removed"
	case "claim_removed":
		return "Claim removed"
	case "unattributed":
		return "Unattributed"
	case domain.CloseReasonSuperseded:
		return "Superseded"
	default:
		return humanizeUnknown(reason)
	}
}

// ClosedByLabel renders which mechanism closed an episode (5.6).
//
// Same default rule. The vocabulary here is two values today and the third
// state is NULL, which this function never sees — the caller handles it, because
// "the sender did not say" is an absence and belongs in a Cell, not a label.
func ClosedByLabel(by string) string {
	switch by {
	case "":
		return ""
	case "notification":
		return "Notification"
	case "sweep":
		return "Sweep"
	default:
		return humanizeUnknown(by)
	}
}

// humanizeUnknown makes an unrecognised enum value readable WITHOUT changing it.
//
// Every transformation here is reversible by eye: underscores to spaces, and the
// first letter capitalised. Nothing is dropped, nothing is replaced, and the
// value remains recognisable to whoever added it to the vocabulary — which is
// the point, since they are the person who will be reading this page trying to
// work out whether their new close reason is firing.
func humanizeUnknown(v string) string {
	s := strings.ReplaceAll(v, "_", " ")
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// ── Number formatting (the number doctrine) ──────────────────────────────────

// FormatDuration renders a duration compound, never as decimal hours, at the
// precision the measurement supports.
//
// Precision ladder, from docs/ui-style-guide.md: whole seconds under ten
// minutes, whole minutes above. An episode duration is a difference between two
// service clocks that are not synchronised to the millisecond, so sub-second
// digits would assert an accuracy the source cannot supply.
//
// A DAYS TIER WAS ADDED FOR 5.11 and it is a fix rather than an extension. The
// stale-binding candidates on /material-flags run to weeks — the longest binding
// in the Springfield dump is 22.99 days — and the hours tier rendered that as
// "551h 26m", which is compound, is not decimal hours, and is still a number
// nobody converts in their head. The ladder's own rule ("whole seconds under ten
// minutes, whole minutes above") generalises: at a day the minutes stop carrying
// anything a reader acts on, so the tier is days plus whole hours. Below 24h
// nothing changes.
func FormatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	const day = 24 * time.Hour
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < 10*time.Minute:
		// Compound, zero-padded seconds so the column stays aligned under
		// tabular-nums — "4m 07s", not "4m 7s".
		return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < day:
		return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd %02dh", int(d/day), int(d.Hours())%24)
	}
}

// FormatCount renders a count in full with a thousands separator.
//
// TABLES NEVER ABBREVIATE. A table is where someone reads the exact figure and
// copies it out, and "12.4k" destroys both uses. The k/M rule in the style guide
// is for space-constrained chrome only — axis ticks, chips, tile heroes — and
// this function is for table cells, so it does not implement it.
func FormatCount(n int) string {
	s := fmt.Sprintf("%d", n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// FormatRatio renders a cost ratio at one decimal place — the precision the
// style guide assigns to a ratio, and no more.
func FormatRatio(r float64) string { return fmt.Sprintf("%.1f×", r) }

// ── The row ──────────────────────────────────────────────────────────────────

// EpisodeRow is one line of the demand browser, fully rendered.
//
// The template does no arithmetic and makes no decisions — it prints these
// fields. Rules that live in a template are rules with no test.
type EpisodeRow struct {
	OriginID   string
	EpisodeKey string
	Kind       string
	KindLabel  string
	Direction  string
	Station    string
	Payload    string
	CoreNode   string

	OpenedAt time.Time
	Open     bool

	// Duration is the measured length: closed_at − opened_at, or now − opened_at
	// while open. Rendered via DurationText.
	Duration     time.Duration
	DurationText string

	// Band, BandLabel and RampStep are 5.14. BandLabel is the PRINTED name and is
	// empty for calm; RampStep is the teal magnitude, saturating at the worry
	// line. Colour never carries the band on its own.
	Band      DurationBand
	BandLabel string
	RampStep  int

	// Orders is the child count — always a real measured number, including 0,
	// which is the worst case on this page rather than the mildest.
	Orders     int
	OrdersText string

	// Expected is expected_orders, which is NULLABLE BY DESIGN: the threshold
	// formula divides by the catalog's UOPCapacity and the sizing entry point
	// (dispatch.BinsToReachThreshold) explicitly refuses capacity <= 0, so
	// "unknowable" happens. NOT 0 and NOT 1 — both are
	// lies that render as a real ratio.
	Expected Cell

	// Ratio is actual ÷ expected. No data when expected is unknown; muted when
	// the denominator is below the configured floor (5.4).
	Ratio     Cell
	RatioSort float64

	// CloseReason and ClosedBy are 5.5 and 5.6. Both are n/a while the episode
	// is open — a question not yet asked, not an absent answer.
	CloseReason Cell
	ClosedBy    Cell

	// Outcomes, Cause and CauseClass are 5.2, filled by AttachCauses from a
	// SECOND query rather than by BuildEpisodeRow. They are deliberately not
	// built here: the child mix is not on the episode row, and threading a
	// per-row lookup through this function would make a pure transform of one
	// stored row into something that needs the whole result set. See
	// demand_causes_view.go, which owns every rule about them.
	//
	// CAUSE IS THE AUTHORITATIVE ONE; Outcomes is the detail behind it. When the
	// mix could not be read Cause is no-data and Outcomes is the zero struct —
	// all-zero counts that MEAN NOTHING, because the query failed rather than
	// returning nothing. Render Outcomes only where Cause has already said the
	// read succeeded, or the page prints six confident zeroes for a number
	// nobody has.
	Outcomes   ChildOutcomes
	Cause      Cell
	CauseClass string

	// SortGroup orders the two unrankable classes above the ranked body. See
	// SortRows.
	SortGroup int
}

// Sort groups. Lower sorts first.
const (
	// sortGroupZeroOrders — a real expectation and NOT ONE ORDER against it.
	// The plan is explicit that this is worse than a high ratio, and its ratio is
	// 0.0, which a plain descending ratio sort would bury at the very bottom of
	// the page. Floating it is not a deviation from "sort by ratio"; it is the
	// only way a ratio sort can avoid hiding its own worst case.
	sortGroupZeroOrders = 0

	// sortGroupNoRatio — the denominator is unknowable, so the ratio column
	// cannot rank this row at all. Ranked rows below would otherwise be a claim
	// that this row is milder than all of them, which is not something the data
	// says.
	sortGroupNoRatio = 1

	// sortGroupRanked — everything the ratio can actually order.
	sortGroupRanked = 2
)

// BuildEpisodeRow renders one stored episode into its display form.
//
// `now` is a parameter, never time.Now() — an open episode's duration depends on
// it, and a function that reads the clock internally cannot be tested at a
// boundary. This repo already established clock injection in Phase 3 for exactly
// this reason.
func BuildEpisodeRow(e domain.DemandEpisode, now time.Time, c config.DisplayConfig) EpisodeRow {
	o := e.DemandOrigin
	children := e.Children
	open := o.ClosedAt == nil

	var d time.Duration
	if open {
		d = now.Sub(o.OpenedAt)
	} else {
		d = o.ClosedAt.Sub(o.OpenedAt)
	}
	if d < 0 {
		// Clock skew between two services. Clamp for display, but do not pretend
		// it did not happen — the title says so rather than the number lying.
		d = 0
	}

	band := BandFor(d, c)

	row := EpisodeRow{
		OriginID:     o.OriginID,
		EpisodeKey:   o.EpisodeKey,
		Kind:         o.Kind,
		KindLabel:    kindLabel(o.Kind),
		Direction:    string(o.Direction),
		Station:      o.StationID,
		Payload:      o.PayloadCode,
		CoreNode:     o.CoreNodeName,
		OpenedAt:     o.OpenedAt,
		Open:         open,
		Duration:     d,
		DurationText: FormatDuration(d),
		Band:         band,
		BandLabel:    band.Label(),
		RampStep:     RampStep(d, c),
		Orders:       children,
		OrdersText:   FormatCount(children),
	}

	// ── The maintain kind's two borrowed columns ─────────────────────────────
	//
	// A maintain episode has no payload and no formula, so two columns that are
	// load-bearing for every other kind arrive empty. Both are filled from what
	// the row ALREADY STORES rather than from a new column or a second query.
	//
	// PLACE: the carrier type is the identity here — one episode per (group,
	// type) — and it lives in the episode key, which is where the maintainer put
	// it. Parsing it back is not a workaround: the key is the canonical spelling
	// of that pair, and a duplicate copy in payload_code would be a second
	// spelling free to disagree with it.
	//
	// EXPECTED: want − resident at the moment the episode opened, which is
	// exactly Threshold − OpenedTotal, both already stored. That is the shortfall
	// the episode was opened over, so the ratio reads as "carriers this shortfall
	// eventually cost". It is deliberately NOT the keeper's first gap: the gap
	// also subtracts what was already coming, and a denominator that shrinks
	// because the keeper was smart would flatter the ratio for the wrong reason.
	expected := o.ExpectedOrders
	if o.Kind == protocol.EpisodeKindMaintain {
		if parsed, perr := protocol.ParseEpisodeKey(o.EpisodeKey); perr == nil && parsed.BinType != "" {
			row.Payload = parsed.BinType
		}
		if expected == nil {
			if shortfall := o.Threshold - o.OpenedTotal; shortfall > 0 {
				expected = &shortfall
			}
		}
	}

	// ── Expected, ratio, and the small-denominator rule (5.4) ────────────────
	switch {
	case expected == nil:
		why := "expected orders could not be computed for this episode"
		if o.ExpectedUnknownReason != "" {
			why = o.ExpectedUnknownReason
		}
		row.Expected = NoData(why)
		row.Ratio = NoData("no ratio without a denominator — " + why)
		row.SortGroup = sortGroupNoRatio

	case *expected <= 0:
		// Zero or negative expected is not a denominator. Guarded separately from
		// NULL because it arrives by a different route — a stored value that is
		// arithmetically unusable rather than an absent one — and dividing by it
		// would produce +Inf, which renders as a number.
		row.Expected = NoData(fmt.Sprintf(
			"expected orders recorded as %d, which cannot be a denominator", *expected))
		row.Ratio = NoData("no ratio: the recorded expectation is not a usable denominator")
		row.SortGroup = sortGroupNoRatio

	default:
		exp := *expected
		row.Expected = Value(FormatCount(exp))
		ratio := float64(children) / float64(exp)
		row.RatioSort = ratio
		row.Ratio = Value(FormatRatio(ratio))

		// Grey a ratio its denominator cannot support. The value is still shown —
		// muted is not absent — and the absolute order count beside it carries the
		// row, which is the whole point of 5.4: expected 1 / actual 3 reads as 3×
		// and is two extra orders.
		if exp < c.MinExpectedOrders {
			row.Ratio.Muted = true
			row.Ratio.Title = fmt.Sprintf(
				"expected %s — below the minimum denominator of %s, so this ratio is not "+
					"a reliable comparison. Read the order count instead.",
				FormatCount(exp), FormatCount(c.MinExpectedOrders))
		}

		switch {
		case children == 0:
			row.SortGroup = sortGroupZeroOrders
		default:
			row.SortGroup = sortGroupRanked
		}
	}

	// ── close_reason (5.5) and closed_by (5.6) ───────────────────────────────
	if open {
		// NOT no-data. The episode has not closed, so there is no close reason to
		// have heard about — the question does not apply yet.
		row.CloseReason = NA("still open — no close reason yet")
		row.ClosedBy = NA("still open — nothing has closed it yet")
	} else {
		if o.CloseReason == "" {
			row.CloseReason = NoData("closed with no reason recorded")
		} else {
			row.CloseReason = Value(CloseReasonLabel(o.CloseReason))
		}

		// closed_by is NULLABLE WITH NO DEFAULT, deliberately: NULL means the
		// sender did not say — an older Edge, or a row written before the column
		// existed. That is a different fact from "a notification path closed it",
		// and collapsing them would defeat the reason the column has no default.
		if o.ClosedBy == "" {
			row.ClosedBy = NoData("the sender did not say — an older Edge, or a row written " +
				"before closed_by existed")
		} else {
			row.ClosedBy = Value(ClosedByLabel(o.ClosedBy))
		}
	}

	return row
}

func kindLabel(kind string) string {
	switch kind {
	case "threshold":
		return "Threshold"
	case "cell":
		return "Cell"
	case "changeover":
		return "Changeover"
	case protocol.EpisodeKindMaintain:
		// "Maintained level", not "Maintain" — the noun says what the row is a
		// record of. Every other kind on this page names a thing that happened to
		// the plant; this one names a standing declaration that went unmet, and
		// the verb form reads like an instruction to the operator.
		return "Maintained level"
	default:
		// Rule 3 again. origin kinds can grow the same way close reasons did.
		return humanizeUnknown(kind)
	}
}

// SortRows orders the browser: by cost ratio, descending, with the two classes
// the ratio cannot rank floated above rather than buried below (5.1).
//
// A plain ORDER BY ratio DESC would put "zero orders against a real demand" —
// ratio 0.0, and the worst thing this page can show — at the very bottom, below
// every mild row on the floor. That is the number doctrine's failure one layer
// up: an absence sorted as though it were the smallest value.
//
// Ties break on duration, longest first, so two rows the ratio cannot separate
// are separated by the other axis. The design records that ratio and duration
// are independent, so this is a second reading rather than a tiebreak of
// convenience.
func SortRows(rows []EpisodeRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.SortGroup != b.SortGroup {
			return a.SortGroup < b.SortGroup
		}
		if a.SortGroup == sortGroupRanked && a.RatioSort != b.RatioSort {
			return a.RatioSort > b.RatioSort
		}
		return a.Duration > b.Duration
	})
}

// ── closed_by as a visible number (5.6) ──────────────────────────────────────

// ClosedBySummary is the 5.6 surface: how closes are split between the
// notification paths and the reconciling sweep.
//
// WHY THIS IS A NUMBER ON A PAGE. The sweep is the correctness floor under six
// notification close paths; it exists so a missed notification degrades to
// "closed late" instead of "stranded forever". That safety net has a cost: when
// the notification paths stop firing, everything still closes, the page still
// looks healthy, and NOTHING ELSE IN THE SYSTEM WOULD SAY SO. The sweep's share
// climbing toward 100% is the only signal that the fast paths have gone silent.
//
// The style guide assigns this a LINE of the sweep's share over time, because
// the alarm is a slope. This struct is the current reading that the line is
// plotted from; the line itself is a later round.
type ClosedBySummary struct {
	Notification int
	Sweep        int

	// Unrecorded counts closed episodes whose closed_by is NULL — the sender did
	// not say. Kept SEPARATE from both named paths rather than folded into
	// either: adding it to Sweep would invent an alarm, adding it to
	// Notification would suppress one.
	Unrecorded int

	// Other counts closed episodes carrying a closed_by value this build does
	// not recognise. Same reason the labels have a default: the vocabulary can
	// grow, and a summary that silently drops unknown values would under-report
	// the total and make the shares wrong.
	Other int

	Total int

	// SweepShare is the headline. NoData when nothing has closed — a share of
	// zero closes is not 0%, it is unmeasured, and 0% is exactly the reassuring
	// reading a broken feed would produce.
	SweepShare Cell
}

// SummarizeClosedBy counts closes by mechanism.
//
// Takes the raw closed_by strings, including empty for NULL, so the caller
// cannot lose the NULL case on the way in.
func SummarizeClosedBy(closedBy []string) ClosedBySummary {
	var s ClosedBySummary
	for _, v := range closedBy {
		s.Total++
		switch v {
		case "notification":
			s.Notification++
		case "sweep":
			s.Sweep++
		case "":
			s.Unrecorded++
		default:
			s.Other++
		}
	}

	if s.Total == 0 {
		// Not 0%. Nothing has closed, so the share has no value — and 0% is the
		// most reassuring number this tile can display, which makes it the worst
		// possible thing to print when the truth is "we have not measured".
		s.SweepShare = NoData("no episodes have closed in this window")
		return s
	}

	// Whole percent: the style guide's precision for a percentage, unless the
	// denominator runs to thousands. Rounded, not truncated, so 99.6% does not
	// print as 99% on the way to the alarm.
	pct := int(float64(s.Sweep)/float64(s.Total)*100 + 0.5)
	s.SweepShare = Value(fmt.Sprintf("%d%%", pct))
	return s
}
