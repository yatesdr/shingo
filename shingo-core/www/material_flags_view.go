package www

import (
	"fmt"
	"sort"
	"time"

	"shingocore/config"
	"shingocore/domain"
)

// material_flags_view.go — Stage 5.11's view model.
//
// ── WHAT THIS ROW IS, AFTER TWO CORRECTIONS ──────────────────────────────────
//
// PLAN-master calls 5.11 "the material-downtime flag". The name outlived the
// reading behind it and the page does not use it. TWO CORRECTIONS, in order:
//
//  1. THE DOWNTIME READING OF A NEGATIVE LEDGER IS REFUTED, logically and
//     empirically. Logically: a negative can only deepen while Core is up,
//     because each step IS an applied row. Empirically, Springfield 2026-07-27:
//     across the 93.9-hour excursion that reached −5,401 (bin 18, 07-16 20:02Z
//     to 07-20 17:54Z) Core wrote in 62 of the 94 hours; and the −10,214
//     excursion (bin 39) lasted 23.4 SECONDS, not the 71 hours a depth ÷ takt
//     estimator predicts. Depth measures BINDING STALENESS, and not always
//     that. Nothing on this page converts a ledger into minutes.
//
//  2. A NEGATIVE UOP IS A CYCLE COUNT, NOT AN OUTAGE. It means an overpacked
//     carrier — more parts went in than were declared — or parts that arrived
//     off the books. The old wording sent every one of these to the wrong owner.
//     It also must not suppress replenishment: the reading is too LOW, so the
//     honest response is to keep ordering and get the carrier counted.
//
// ── THE ONE STRUCTURAL RULE OF THIS FILE ─────────────────────────────────────
//
// THE FLAG COMES FROM THE EPISODE. THE LEDGER IS A SEPARATE READING AND NEVER
// THE SELECTOR. If the flag hangs off "uop_remaining < 0" it inherits the
// ledger's noise, and the Springfield dump refutes that selector in BOTH
// directions at once. Bindings segmented at audit.EpochBumpOps, 531 of them over
// 82.2 days:
//
//   - THE SELECTOR MISSES. The longest binding in the dump — bin 27, 22.99 days
//     on 63125-6TA0A.06 — carries a ledger of −774 against a 4,500-unit
//     capacity: 0.17 of one binload, indistinguishable from an ordinary
//     overpack. A depth filter never sees the stalest binding on the plant.
//   - THE SELECTOR FIRES ANYWAY. The deepest ledger in the dump — bin 39,
//     −10,214, 10.2 binloads — sat on a binding 1.6 HOURS old. Bin 18's −7,000
//     (11.7 binloads) on one 4.9 hours old.
//   - AND IT IS MOSTLY SHORT. Of 191 negative bindings, 166 lasted under three
//     days. Depth against binding age correlates at Pearson 0.553 — related,
//     nowhere near the same axis.
//
// So the two halves of this page are selected on two different columns, sit
// under two different headings, and go to two different people. They are not
// merged into one indicator and there is no combined score, because a combined
// score is exactly the artefact that sent the flags to the wrong owner.
//
// ── THE FOUR BINDING CONSTRAINTS, AND WHERE EACH ONE LIVES ───────────────────
//
//  1. FLAG, NEVER ATTRIBUTION. Nothing here names a cause. The episode half says
//     a place has been asking for material and for how long; it does not say why
//     nothing came, because the data does not record that.
//  2. NO METRIC GRADING ANYONE. There is DELIBERATELY no duration total anywhere
//     on this surface — see MaterialFlagSummary. Summing durations is the single
//     step that turns a flag into a downtime-minutes scorecard, and this data
//     cannot support one.
//  3. STATE CONFIDENCE HONESTLY. Every Cell that stands for an absence carries a
//     required reason, and the copy says which readings are measured and which
//     are not recorded anywhere.
//  4. A DOOR, NOT A VERDICT. Every row links out — the episode to
//     /demand-episodes/{originID}, the carrier to /bins — so the page ends in
//     somewhere to look rather than in a conclusion.
//
// Everything below is a pure function of (rows, now, constants): no database, no
// time.Now, no template. Same discipline as demand_episodes_view.go, and for the
// same reason — these are the rules most likely to be got wrong and least likely
// to be noticed.

// ── The episode half: the flag ────────────────────────────────────────────────

// MaterialFlagSummary is the episode half's context strip.
//
// COUNTS AND ONE LONGEST, AND NOTHING THAT ACCUMULATES. There is no
// "total minutes waiting" here and there must never be: a sum over durations is
// the artefact a scorecard is built from, it is trivially comparable between
// shifts and cells, and the data underneath it does not record whether anybody
// was actually waiting. A point-in-time count cannot be accumulated into a grade
// from this page, which is the property being protected.
type MaterialFlagSummary struct {
	// OpenTotal is how many episodes are open right now, at any duration. A real
	// measured count including zero.
	OpenTotal int
	// PastWorry / PastConcern are the two configured bands, counted. Both are
	// subsets of OpenTotal, printed beside it so the reader can see the flagged
	// rows as a share of what is open rather than as a bare alarm count.
	PastWorry   int
	PastConcern int

	// Longest is the longest OPEN episode's duration. NoData when nothing is open
	// — the longest of no episodes is not "0 s", and 0 s is the most reassuring
	// thing this tile could print.
	Longest Cell
}

// SelectMaterialFlags returns the open episodes past the worry line, longest
// first, plus the summary over the whole open population.
//
// TAKES ALREADY-BUILT EpisodeRows rather than domain episodes, so the duration,
// band, ramp step and every absence state on the row are the SAME values
// /demand-episodes renders. A second construction path for the same row is how
// two surfaces start disagreeing about how long an episode has been open.
//
// CLOSED EPISODES ARE NOT FLAGS. A flag is a thing to walk to; an episode that
// closed is history, and it lives on the browser. Filtering here rather than in
// SQL keeps the summary's denominator honest — OpenTotal counts what was open,
// including the calm ones this list does not show.
func SelectMaterialFlags(rows []EpisodeRow) ([]EpisodeRow, MaterialFlagSummary) {
	var s MaterialFlagSummary
	out := make([]EpisodeRow, 0, len(rows))
	var longest time.Duration
	var anyOpen bool

	for _, r := range rows {
		if !r.Open {
			continue
		}
		s.OpenTotal++
		anyOpen = true
		if r.Duration > longest {
			longest = r.Duration
		}
		switch r.Band {
		case BandConcern:
			s.PastConcern++
			s.PastWorry++
			out = append(out, r)
		case BandWorry:
			s.PastWorry++
			out = append(out, r)
		}
	}

	if anyOpen {
		s.Longest = Value(FormatDuration(longest))
	} else {
		s.Longest = NoData("no episodes are open, so there is no longest one — " +
			"this is not a duration of zero")
	}

	// LONGEST FIRST, which is a different question from the browser's ratio sort
	// and deliberately not SortRows. The browser asks "which demand cost the most
	// orders"; this asks "which place has been asking longest", and the answer to
	// the second is the one someone walks to.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Duration > out[j].Duration })
	return out, s
}

// ── The ledger half: stale-binding candidates ────────────────────────────────

// LedgerReading is which of four things a carrier's ledger is saying.
//
// IT CLASSIFIES AND SELECTS NOTHING. Nothing in BuildBindingRows filters on it.
type LedgerReading string

const (
	// ReadingSettled — the ledger is zero or positive. There is no negative to
	// account for. NOT "healthy": a settled ledger on a three-week-old binding is
	// exactly the case the binding half exists to surface.
	ReadingSettled LedgerReading = "settled"

	// ReadingOverpack — negative, but within one carrier-load of zero. Explicable
	// as a single overpacked carrier: more parts went in than the payload declares.
	// A cycle count.
	ReadingOverpack LedgerReading = "overpack"

	// ReadingBeyondBinload — negative by more than the configured multiple of the
	// carrier's own capacity. One overpack cannot account for it; more than a
	// binload of parts moved without ShinGo seeing it. Still a cycle count.
	ReadingBeyondBinload LedgerReading = "beyond"

	// ReadingUnsized — negative, and the bound payload has no UOP capacity
	// recorded, so the depth cannot be expressed in binloads at all. An absence,
	// and it must not render as either of the two sized readings.
	ReadingUnsized LedgerReading = "unsized"
)

// Label is the PRINTED name. Crossing the overpack line takes its own channel —
// text plus a ring — never colour alone, the same rule the duration band follows.
func (r LedgerReading) Label() string {
	switch r {
	case ReadingSettled:
		return "Settled"
	case ReadingOverpack:
		return "Within one binload"
	case ReadingBeyondBinload:
		return "Beyond one binload"
	case ReadingUnsized:
		return "Cannot size"
	default:
		// The unrecognised-value rule (5.5) applied to this vocabulary too. A
		// reading this build does not know renders as itself, never as settled —
		// settled is the reassuring one.
		return humanizeUnknown(string(r))
	}
}

// ClassifyLedger says which reading a carrier's ledger is, and how many binloads
// below zero it sits.
//
// The bool reports whether the binload figure exists at all. A (0, false) that a
// caller could ignore would render "we cannot size this negative" as "zero
// binloads below zero", which is the settled reading with the evidence removed.
func ClassifyLedger(c domain.CarrierBinding, k config.DisplayConfig) (LedgerReading, float64, bool) {
	if c.UOPRemaining >= 0 {
		return ReadingSettled, 0, false
	}
	if c.UOPCapacity == nil || *c.UOPCapacity <= 0 {
		return ReadingUnsized, 0, false
	}
	binloads := float64(-c.UOPRemaining) / float64(*c.UOPCapacity)
	if binloads >= k.OverpackBinloads {
		return ReadingBeyondBinload, binloads, true
	}
	return ReadingOverpack, binloads, true
}

// BindingRow is one stale-binding candidate, fully rendered.
type BindingRow struct {
	BinID   int64
	Label   string
	Payload string
	Node    Cell

	// Age is the binding's measured age; AgeKnown says whether it is knowable.
	// AgeCell carries the rendering: a value when known, no-data when the audit
	// trail holds no boundary for this carrier.
	Age      time.Duration
	AgeKnown bool
	AgeCell  Cell

	// BoundAt is the boundary row's timestamp, printed beside the age so the
	// reading is checkable against bin_uop_audit rather than merely asserted.
	// No-data when there is no boundary row.
	BoundAt Cell

	// Ledger is the live count. ALWAYS A MEASURED VALUE, including zero and
	// including negatives — there is no absence case for this column, because
	// bins.uop_remaining is NOT NULL and every carrier has one.
	Ledger     int
	LedgerCell Cell

	// Reading and Binloads are the two channels of the same classification: the
	// printed name and the sized figure. Binloads is not-applicable on a settled
	// ledger and no-data when the payload has no capacity — two different
	// absences, and they render differently.
	Reading      LedgerReading
	ReadingClass string
	ReadingCell  Cell
	Binloads     Cell

	// LastCounted is how long ago someone physically counted this carrier.
	// No-data when never — a measured fact about the carrier, stated as an
	// absence of a count rather than as an age of zero.
	LastCounted Cell

	// Anomaly is the applier's own flag: deltas refused, or a payload rebound
	// over inventory. A MEASURED "None" when clear, not an absence — this is the
	// number doctrine's "do not over-rotate" rider, and dashing out a real
	// negative result would hide the finding from the other direction.
	Anomaly Cell

	// SortGroup floats the rows the age column cannot rank. See sortBindings.
	SortGroup int
}

// Binding sort groups. Lower sorts first.
const (
	// bindingGroupUnknownAge — bound, but no boundary row exists, so the binding
	// age is unknowable. It cannot be ranked against the rows below, and ranking
	// it beneath them would assert it is younger than all of them, which is not
	// something the data says. Same rule as sortGroupNoRatio on the browser.
	bindingGroupUnknownAge = 0
	// bindingGroupRanked — everything the age column can order.
	bindingGroupRanked = 1
)

// BindingSummary is the ledger half's context strip: the whole carrier
// population, partitioned by what can be said about each one.
//
// THE THREE ABSENCE CATEGORIES ARE COUNTED SEPARATELY AND PRINTED SEPARATELY.
// Unbound carriers have NO binding to age (not-applicable); bound carriers with
// no boundary row have one that cannot be aged (no-data); the rest are measured.
// Folding the first two into the third would make the candidate list look
// complete when it is not, and folding them into each other would say a carrier
// holding nothing is a carrier whose history is missing.
type BindingSummary struct {
	// Carriers is every carrier ShinGo knows about.
	Carriers int
	// Bound is how many hold a payload, and therefore have a binding at all.
	Bound int
	// Unbound is how many hold none. NOT candidates however long they have sat
	// there: an empty carrier's count cannot have drifted from anything.
	Unbound int
	// UnknownAge is bound carriers with no EpochBumpOps row in the audit trail.
	// These ARE listed as candidates — an unknowable age cannot be ruled out, and
	// dropping them would be an absence the page can never say it had.
	UnknownAge int
	// Candidates is how many rows the table below shows.
	Candidates int

	// Oldest is the longest KNOWN binding age across the whole fleet, flagged or
	// not. NoData when no carrier's age is knowable.
	Oldest Cell
}

// BuildBindingRows turns the carrier population into the candidate list and its
// summary.
//
// ── THE SELECTION RULE, IN ONE PLACE ─────────────────────────────────────────
//
// A carrier is a candidate when it is BOUND and either
//
//   - its binding is at least k.StaleBindingAfter old, or
//   - its binding age is not knowable.
//
// THE LEDGER APPEARS NOWHERE IN THAT RULE. It is rendered on every row, sized
// and named, as the thing a person reads once the row has already been selected
// — because it says what KIND of cycle count to expect, and it says nothing
// reliable about staleness (see the file header for the measurements).
//
// The second clause is the one that is easy to leave out. A carrier whose bind
// predates the audit trail has an age this page cannot compute, and excluding it
// would mean the surface silently under-reports on exactly the carriers whose
// history is thinnest. Included, floated to the top, and labelled as unknowable.
func BuildBindingRows(cs []domain.CarrierBinding, now time.Time, k config.DisplayConfig) ([]BindingRow, BindingSummary) {
	var s BindingSummary
	var oldest time.Duration
	var anyKnown bool
	out := make([]BindingRow, 0, len(cs))

	for _, c := range cs {
		s.Carriers++
		if c.PayloadCode == "" {
			s.Unbound++
			continue
		}
		s.Bound++

		age, known := c.BindingAge(now)
		if known {
			if age > oldest {
				oldest = age
			}
			anyKnown = true
		} else {
			s.UnknownAge++
		}

		if known && age < k.StaleBindingAfter {
			continue
		}
		out = append(out, buildBindingRow(c, age, known, k))
	}

	s.Candidates = len(out)
	if anyKnown {
		s.Oldest = Value(FormatDuration(oldest))
	} else {
		s.Oldest = NoData("no carrier has a load, clear or release row in the audit " +
			"trail, so no binding age is knowable — not a fleet of fresh bindings")
	}

	sortBindings(out)
	return out, s
}

func buildBindingRow(c domain.CarrierBinding, age time.Duration, known bool, k config.DisplayConfig) BindingRow {
	r := BindingRow{
		BinID:    c.BinID,
		Label:    c.Label,
		Payload:  c.PayloadCode,
		Age:      age,
		AgeKnown: known,
		Ledger:   c.UOPRemaining,
		// The ledger is NOT NULL on every carrier, so it is always a measurement.
		// Zero prints as zero and a negative prints with its sign — the doctrine's
		// "a real measured zero stays 0, plainly".
		LedgerCell: Value(FormatCount(c.UOPRemaining)),
	}

	if c.NodeName != "" {
		r.Node = Value(c.NodeName)
	} else {
		// A carrier not at a node is in transit or unplaced. The question "where"
		// has an answer that is simply not recorded at this instant, which is an
		// absence and not a blank.
		r.Node = NoData("no node recorded for this carrier — in transit, or never placed")
	}

	if known {
		r.SortGroup = bindingGroupRanked
		r.AgeCell = Value(FormatDuration(age))
		r.BoundAt = Value(c.BoundAt.UTC().Format("2006-01-02 15:04Z"))
	} else {
		r.SortGroup = bindingGroupUnknownAge
		r.AgeCell = NoData("no load, clear or release row exists for this carrier — the " +
			"binding predates the audit trail, so its age is unknown. NOT a new binding")
		r.BoundAt = NoData("no boundary row in bin_uop_audit for this carrier")
	}

	reading, binloads, sized := ClassifyLedger(c, k)
	r.Reading = reading
	r.ReadingClass = string(reading)
	switch reading {
	case ReadingSettled:
		r.ReadingCell = Value(reading.Label())
		r.Binloads = NA("the ledger is zero or positive, so there is no negative to size")
	case ReadingUnsized:
		// THE READING IS A VALUE AND THE FIGURE IS THE ABSENCE, not both.
		// "Cannot size" is something this row KNOWS: the ledger is negative and
		// the payload has no capacity to measure it against. An em dash here
		// would read identically to "nothing was examined about this row", which
		// is a different and much milder claim — and it would hide an
		// unclassifiable negative behind the same mark a blank row carries. The
		// absence belongs one column along, where the number genuinely does not
		// exist.
		r.ReadingCell = Value(reading.Label())
		r.ReadingCell.Title = fmt.Sprintf(
			"the ledger reads %s, but %s has no UOP capacity recorded, so the depth cannot "+
				"be expressed in binloads and this negative cannot be told apart from an "+
				"overpack. Set the payload's capacity and it can",
			FormatCount(c.UOPRemaining), c.PayloadCode)
		r.Binloads = NoData("no UOP capacity recorded for this payload, so there is no " +
			"figure to print — the ledger beside it is still a measurement")
	default:
		r.ReadingCell = Value(reading.Label())
		r.ReadingCell.Title = fmt.Sprintf(
			"%s against a nominal %s per carrier. A negative is a CYCLE COUNT, not an "+
				"outage: more parts went in than were declared, or parts arrived off the "+
				"books. It does not suppress replenishment.",
			FormatCount(c.UOPRemaining), FormatCount(*c.UOPCapacity))
	}
	if sized {
		r.Binloads = Value(FormatRatio(binloads))
		r.Binloads.Title = fmt.Sprintf("%s below zero against a nominal %s per carrier",
			FormatCount(-c.UOPRemaining), FormatCount(*c.UOPCapacity))
	}

	if c.LastCountedAt != nil {
		r.LastCounted = Value(c.LastCountedAt.UTC().Format("2006-01-02"))
	} else {
		r.LastCounted = NoData("this carrier has never been cycle counted — there is no " +
			"count to age, which is a fact about the carrier and not a read failure")
	}

	if c.AnomalyAt != nil {
		r.Anomaly = Value("Deltas refused")
		r.Anomaly.Title = "the applier has refused a delta for this carrier or rebound its " +
			"payload over existing inventory (uop/applier.go). Visibility only — it gates " +
			"no claim and no dispatch"
	} else {
		// A MEASURED NEGATIVE RESULT, printed plainly. Not an em dash: we looked,
		// and the answer is none. Dashing this out would hide the finding from the
		// other direction, which the number doctrine calls out as the same error
		// mirrored.
		r.Anomaly = Value("None")
	}

	return r
}

// sortBindings puts the unrankable rows first, then oldest binding first.
//
// The floated group is the same doctrine as SortRows on the browser: an absence
// must never sort as though it were the smallest value. A carrier whose binding
// age is unknown, ordered beneath a two-day binding, is a claim that its binding
// is younger than two days — which is precisely what nobody knows.
func sortBindings(rows []BindingRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.SortGroup != b.SortGroup {
			return a.SortGroup < b.SortGroup
		}
		if a.Age != b.Age {
			return a.Age > b.Age
		}
		// Deeper ledger breaks a tie between two equally-old bindings. A tiebreak
		// only — the ledger still selects nothing.
		return a.Ledger < b.Ledger
	})
}
