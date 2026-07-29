package www

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"shingocore/config"
	"shingocore/domain"
)

// material_flags_view_test.go — 5.11's rules, held as executable claims.
//
// The two that carry the row are asserted first and hardest:
//
//  1. THE LEDGER SELECTS NOTHING. Both directions, using the two exemplars
//     measured in the Springfield dump — a three-week binding with a shallow
//     ledger, and a ten-binload ledger on a binding hours old. If a future edit
//     folds the sign back into the selector, exactly one of these fails.
//  2. NO DATA, ZERO AND NOT APPLICABLE STAY APART. Four absences can appear on
//     one carrier row and they mean four different things.

func mfDisp() config.DisplayConfig { return config.DisplayDefaults() }

func mfNow() time.Time { return time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC) }

// carrier builds a bound carrier whose binding started `ago` before mfNow.
func carrier(id int64, payload string, ledger int, capacity int, ago time.Duration) domain.CarrierBinding {
	bound := mfNow().Add(-ago)
	c := domain.CarrierBinding{
		BinID:        id,
		Label:        "CARRIER-" + strings.Repeat("0", 1),
		PayloadCode:  payload,
		NodeName:     "SMN_001",
		UOPRemaining: ledger,
		BoundAt:      &bound,
	}
	if capacity > 0 {
		c.UOPCapacity = &capacity
	}
	return c
}

// ── 1. The ledger selects nothing ────────────────────────────────────────────

// TestBindingSelectionIgnoresTheLedgerSign is the load-bearing test on this
// surface, and both halves come from measured Springfield rows.
//
// If a later edit adds "or uop_remaining < 0" to the selector, the SHALLOW case
// still passes and the SHORT case fails. If it REPLACES the age rule with the
// sign, the short case passes and the shallow one fails. Either mistake breaks
// exactly one assertion, which is why both are here rather than one.
func TestBindingSelectionIgnoresTheLedgerSign(t *testing.T) {
	c := mfDisp()

	// Springfield bin 27: the LONGEST binding in the dump, 22.99 days, ledger
	// −774 against a 4,500-unit capacity = 0.17 binloads. A depth filter never
	// sees it. It must be a candidate, and its reading must be the mild one.
	shallowAndOld := carrier(27, "63125-6TA0A.06", -774, 4500, 22*24*time.Hour+23*time.Hour)

	// Springfield bin 39: the DEEPEST ledger in the dump, −10,214 against a
	// 1,000-unit capacity = 10.2 binloads, on a binding 1.6 HOURS old. A depth
	// filter flags it. It must NOT be a candidate — nothing about ShinGo's
	// knowledge of that carrier is stale.
	deepAndNew := carrier(39, "74343-6SA0A.06", -10214, 1000, 96*time.Minute)

	rows, summary := BuildBindingRows(
		[]domain.CarrierBinding{shallowAndOld, deepAndNew}, mfNow(), c)

	if len(rows) != 1 {
		t.Fatalf("got %d candidates, want 1 — the selector must be the binding age "+
			"and nothing else. Rows: %+v", len(rows), rows)
	}
	if rows[0].BinID != 27 {
		t.Errorf("candidate is bin %d, want 27. The three-week binding with a shallow "+
			"ledger is the one that is stale; the ten-binload ledger on a 1.6-hour "+
			"binding is not.", rows[0].BinID)
	}
	if rows[0].Reading != ReadingOverpack {
		t.Errorf("bin 27's reading is %q, want %q — 774 below zero against a nominal "+
			"4,500 is 0.17 of one binload, which one overpacked carrier explains",
			rows[0].Reading, ReadingOverpack)
	}
	if summary.Bound != 2 || summary.Candidates != 1 {
		t.Errorf("summary bound=%d candidates=%d, want 2 and 1 — the denominator has to "+
			"count what was examined, not what was flagged", summary.Bound, summary.Candidates)
	}
}

// TestClassifyLedgerSizesAgainstTheCarriersOwnCapacity holds the reason the
// overpack line is a multiple rather than a unit count.
//
// −300 is more than a full carrier of a 250-unit payload and a twelfth of a
// 3,600-unit one. A units threshold would call the first mild and the second
// severe, which is backwards.
func TestClassifyLedgerSizesAgainstTheCarriersOwnCapacity(t *testing.T) {
	c := mfDisp()

	small := carrier(1, "SMALL", -300, 250, time.Hour)
	large := carrier(2, "LARGE", -300, 3600, time.Hour)

	rSmall, binloadsSmall, okSmall := ClassifyLedger(small, c)
	rLarge, binloadsLarge, okLarge := ClassifyLedger(large, c)

	if !okSmall || !okLarge {
		t.Fatalf("both are sizeable: small ok=%v large ok=%v", okSmall, okLarge)
	}
	if rSmall != ReadingBeyondBinload {
		t.Errorf("−300 on a 250-unit carrier read %q, want %q (%.2f binloads) — one "+
			"overpack cannot put 300 units into a 250-unit carrier", rSmall,
			ReadingBeyondBinload, binloadsSmall)
	}
	if rLarge != ReadingOverpack {
		t.Errorf("−300 on a 3,600-unit carrier read %q, want %q (%.2f binloads)",
			rLarge, ReadingOverpack, binloadsLarge)
	}
}

// TestClassifyLedgerNeverGuessesAnUnsizedNegative covers the case the ratio
// cannot be computed for at all.
//
// payloads.uop_capacity can be zero — BinService.RecordCount already refuses to
// count against such a payload — and dividing by it or by one would render a
// figure nobody measured. The reading must be its own fourth state.
func TestClassifyLedgerNeverGuessesAnUnsizedNegative(t *testing.T) {
	c := mfDisp()

	for _, tc := range []struct {
		name string
		bind domain.CarrierBinding
	}{
		{"capacity absent", carrier(1, "NOCAP", -500, 0, time.Hour)},
		{"capacity zero", func() domain.CarrierBinding {
			b := carrier(2, "ZEROCAP", -500, 1, time.Hour)
			zero := 0
			b.UOPCapacity = &zero
			return b
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, binloads, ok := ClassifyLedger(tc.bind, c)
			if r != ReadingUnsized {
				t.Errorf("reading = %q, want %q", r, ReadingUnsized)
			}
			if ok {
				t.Errorf("ok = true with binloads %.2f — an unsized negative has no ratio, "+
					"and a caller allowed to ignore that renders a number nobody measured",
					binloads)
			}
		})
	}
}

// TestSettledLedgerIsNotHealthy pins the naming decision.
//
// A carrier can sit on a three-week binding with a perfectly positive ledger —
// that is precisely the case the binding half exists to surface — so the
// zero-or-positive reading must not be called "OK" or "healthy" anywhere.
func TestSettledLedgerIsNotHealthy(t *testing.T) {
	c := mfDisp()
	rows, _ := BuildBindingRows([]domain.CarrierBinding{
		carrier(1, "PANEL", 420, 500, 5*24*time.Hour),
	}, mfNow(), c)

	if len(rows) != 1 {
		t.Fatalf("a five-day binding with a positive ledger must still be a candidate; got %d rows", len(rows))
	}
	if rows[0].Reading != ReadingSettled {
		t.Fatalf("reading = %q, want %q", rows[0].Reading, ReadingSettled)
	}
	label := strings.ToLower(rows[0].Reading.Label())
	for _, banned := range []string{"ok", "healthy", "good", "fine"} {
		if strings.Contains(label, banned) {
			t.Errorf("the zero-or-positive reading is labelled %q, which contains %q. A "+
				"settled ledger on a three-week binding is the case this section exists "+
				"for; calling it healthy would tell the reader to skip the row.",
				rows[0].Reading.Label(), banned)
		}
	}
}

// TestLedgerReadingLabelRendersAnUnknownAsItself is 5.5's rule applied to this
// vocabulary. The default must never fall through to the reassuring reading.
func TestLedgerReadingLabelRendersAnUnknownAsItself(t *testing.T) {
	got := LedgerReading("mixed_contents").Label()
	if got != "Mixed contents" {
		t.Errorf("Label() = %q, want %q — an unrecognised reading renders as itself, "+
			"underscores to spaces, nothing dropped", got, "Mixed contents")
	}
	if got == ReadingSettled.Label() {
		t.Error("an unknown reading rendered as Settled, which is the reassuring one")
	}
}

// ── 2. The three absence states stay apart ───────────────────────────────────

// TestBindingRowKeepsFourAbsencesApart is the number doctrine's rule 4 on the
// row that has the most ways to break it.
//
// FOUR THINGS CAN BE UNKNOWN AND THEY MEAN FOUR DIFFERENT THINGS: an unbound
// carrier has no binding to age; a bound carrier with no boundary row has an age
// nobody knows; a negative on a capacity-less payload cannot be sized; and a
// carrier nobody has counted has no count to age. None of them is zero.
func TestBindingRowKeepsFourAbsencesApart(t *testing.T) {
	c := mfDisp()

	unbound := domain.CarrierBinding{BinID: 90, Label: "EMPTY", UOPRemaining: 0}

	noBoundary := domain.CarrierBinding{
		BinID: 91, Label: "PREHISTORIC", PayloadCode: "PANEL", NodeName: "SMN_002",
		UOPRemaining: -50, // negative AND unsized: two absences on one row
	}

	neverCounted := carrier(92, "PANEL", 0, 500, 5*24*time.Hour)
	neverCounted.LastCountedAt = nil

	counted := carrier(93, "PANEL", -900, 500, 6*24*time.Hour)
	when := mfNow().Add(-48 * time.Hour)
	counted.LastCountedAt = &when

	rows, s := BuildBindingRows(
		[]domain.CarrierBinding{unbound, noBoundary, neverCounted, counted}, mfNow(), c)

	// The unbound carrier is NOT a candidate and is NOT silently dropped: it is
	// counted in its own category, so the page can state it.
	if s.Unbound != 1 {
		t.Errorf("summary.Unbound = %d, want 1", s.Unbound)
	}
	for _, r := range rows {
		if r.BinID == 90 {
			t.Error("an unbound carrier was listed as a stale-binding candidate. An empty " +
				"carrier's count cannot have drifted from anything, however long it has sat")
		}
	}
	if s.UnknownAge != 1 {
		t.Errorf("summary.UnknownAge = %d, want 1", s.UnknownAge)
	}
	if s.Candidates != 3 {
		t.Fatalf("candidates = %d, want 3 (the unknown-age one plus the two old bindings)", s.Candidates)
	}

	byID := map[int64]BindingRow{}
	for _, r := range rows {
		byID[r.BinID] = r
	}

	// UNKNOWN AGE IS NO-DATA, not a fresh binding, and it is FLOATED — an
	// unrankable row sorted beneath the ranked ones asserts it is younger than
	// all of them.
	if got := byID[91].AgeCell.Kind; got != CellNoData {
		t.Errorf("bin 91's age cell is %q, want %q — no boundary row means the age is "+
			"unknown, and rendering it as a value would say the binding is new", got, CellNoData)
	}
	if rows[0].BinID != 91 {
		t.Errorf("first row is bin %d, want 91 — the row whose age cannot be ranked must "+
			"float above the ranked ones, not sort as the smallest value", rows[0].BinID)
	}
	if byID[91].AgeCell.Title == "" {
		t.Error("a no-data cell with no title. The style guide requires the title to say " +
			"WHICH absence this is, and the constructor makes the reason mandatory")
	}

	// A NEGATIVE ON A CAPACITY-LESS PAYLOAD IS NO-DATA IN THE FIGURE COLUMN, not
	// zero binloads.
	if got := byID[91].Binloads.Kind; got != CellNoData {
		t.Errorf("bin 91's binloads cell is %q, want %q", got, CellNoData)
	}
	// AND THE READING COLUMN STILL SAYS SOMETHING. "Cannot size" is a finding
	// about that row — the ledger IS negative and there is no capacity to measure
	// it against — so it must not render as the same em dash a row nobody
	// examined would carry. One absence per fact, in the column where the fact is
	// actually missing.
	if got := byID[91].ReadingCell.Kind; got != CellValue {
		t.Errorf("bin 91's ledger-reading cell is %q, want %q. An em dash here reads as "+
			"\"nothing was examined\", which is milder and untrue — it would hide an "+
			"unclassifiable negative behind the mark a blank row carries.", got, CellValue)
	}
	if byID[91].ReadingCell.Text != ReadingUnsized.Label() {
		t.Errorf("bin 91's reading text is %q, want %q", byID[91].ReadingCell.Text,
			ReadingUnsized.Label())
	}
	if byID[91].ReadingCell.Title == "" {
		t.Error("\"Cannot size\" with no title. The reader has to be told WHY it cannot be " +
			"sized, and that the missing thing is the payload's capacity rather than the count")
	}

	// A SETTLED LEDGER'S BINLOAD FIGURE IS NOT-APPLICABLE, which is a different
	// claim from no-data: there is no negative to size, rather than a negative we
	// could not size.
	if got := byID[92].Binloads.Kind; got != CellNA {
		t.Errorf("bin 92's binloads cell is %q, want %q — a zero-or-positive ledger has "+
			"nothing to size, and that is not a missing measurement", got, CellNA)
	}
	if byID[92].Binloads.Kind == byID[91].Binloads.Kind {
		t.Error("not-applicable and no-data collapsed into one kind on the binloads column")
	}

	// NEVER COUNTED IS NO-DATA; a real count is a value. Two different facts.
	if got := byID[92].LastCounted.Kind; got != CellNoData {
		t.Errorf("bin 92 has never been counted; LastCounted kind = %q, want %q", got, CellNoData)
	}
	if got := byID[93].LastCounted.Kind; got != CellValue {
		t.Errorf("bin 93 was counted; LastCounted kind = %q, want %q", got, CellValue)
	}

	// THE LEDGER ITSELF IS ALWAYS A MEASUREMENT. bins.uop_remaining is NOT NULL,
	// so zero is a real zero and prints plainly — the doctrine's "do not
	// over-rotate" rider, which dashing out true zeros would violate in the
	// opposite direction.
	if byID[92].LedgerCell.Kind != CellValue || byID[92].LedgerCell.Text != "0" {
		t.Errorf("a measured zero ledger rendered as %+v, want a plain value \"0\"",
			byID[92].LedgerCell)
	}

	// AND THE APPLIER COLUMN'S CLEAR CASE IS A MEASURED "None", not an absence.
	// We looked and the answer is none; an em dash would say we had not looked.
	if byID[93].Anomaly.Kind != CellValue {
		t.Errorf("a carrier with no anomaly rendered as %q. We measured, and the answer "+
			"is none — that is a value, not an absence", byID[93].Anomaly.Kind)
	}
}

// TestBindingSummaryOldestIsNoDataWhenNothingIsKnowable covers the tile that is
// easiest to get wrong in the reassuring direction.
func TestBindingSummaryOldestIsNoDataWhenNothingIsKnowable(t *testing.T) {
	c := mfDisp()

	_, s := BuildBindingRows([]domain.CarrierBinding{
		{BinID: 1, PayloadCode: "PANEL", UOPRemaining: 0}, // bound, no boundary row
	}, mfNow(), c)

	if s.Oldest.Kind != CellNoData {
		t.Errorf("Oldest = %+v, want no-data. No carrier has a knowable binding age, and "+
			"printing \"0 s\" would say the whole fleet was just re-bound", s.Oldest)
	}

	// And it IS a value once one age is knowable.
	_, s2 := BuildBindingRows([]domain.CarrierBinding{
		carrier(2, "PANEL", 0, 500, 4*24*time.Hour),
	}, mfNow(), c)
	if s2.Oldest.Kind != CellValue || s2.Oldest.Text != "4d 00h" {
		t.Errorf("Oldest = %+v, want value \"4d 00h\"", s2.Oldest)
	}
}

// TestBindingAgeIsNotKnowableWithoutABoundary is the type-level half of the same
// rule, at the domain boundary where the information is either kept or lost.
func TestBindingAgeIsNotKnowableWithoutABoundary(t *testing.T) {
	bound := mfNow().Add(-3 * time.Hour)

	for _, tc := range []struct {
		name      string
		bind      domain.CarrierBinding
		wantKnown bool
	}{
		{"bound with a boundary", domain.CarrierBinding{PayloadCode: "P", BoundAt: &bound}, true},
		{"bound, no boundary", domain.CarrierBinding{PayloadCode: "P"}, false},
		{"unbound", domain.CarrierBinding{BoundAt: &bound}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, known := tc.bind.BindingAge(mfNow())
			if known != tc.wantKnown {
				t.Errorf("known = %v, want %v", known, tc.wantKnown)
			}
		})
	}

	// Clock skew clamps for arithmetic but stays KNOWN — the reading exists, it
	// is just at the boundary. Silently turning it into "unknown" would lose a
	// real binding.
	future := mfNow().Add(time.Hour)
	d, known := domain.CarrierBinding{PayloadCode: "P", BoundAt: &future}.BindingAge(mfNow())
	if !known || d != 0 {
		t.Errorf("a future boundary gave (%s, %v), want (0s, true)", d, known)
	}
}

// ── The episode half ─────────────────────────────────────────────────────────

// TestSelectMaterialFlagsTakesOnlyOpenEpisodesPastTheLine covers the selector
// and, more importantly, the denominator.
func TestSelectMaterialFlagsTakesOnlyOpenEpisodesPastTheLine(t *testing.T) {
	now := mfNow()
	c := mfDisp()
	expected := 4

	mk := func(id string, openedAgo time.Duration, closed bool) EpisodeRow {
		o := domain.DemandOrigin{
			OriginID: id, EpisodeKey: "k" + id, Kind: "cell", StationID: "S",
			OpenedAt: now.Add(-openedAgo), ExpectedOrders: &expected,
		}
		if closed {
			at := now.Add(-openedAgo).Add(time.Minute)
			o.ClosedAt = &at
			o.CloseReason = "recovered"
			o.ClosedBy = "sweep"
		}
		return BuildEpisodeRow(domain.DemandEpisode{DemandOrigin: o, Children: 2}, now, c)
	}

	rows := []EpisodeRow{
		mk("calm", 10*time.Minute, false),    // open, inside the calm band
		mk("worry", 50*time.Minute, false),   // open, past worry
		mk("concern", 3*time.Hour, false),    // open, past concern
		mk("closed-long", 5*time.Hour, true), // closed: history, not a flag
	}

	flags, s := SelectMaterialFlags(rows)

	if len(flags) != 2 {
		t.Fatalf("got %d flags, want 2 (worry + concern). Closed episodes are history "+
			"and belong on /demand-episodes; a calm open one is not a flag.", len(flags))
	}
	// LONGEST FIRST — the row someone walks to is the one that has waited longest.
	if flags[0].OriginID != "concern" {
		t.Errorf("first flag is %q, want \"concern\" — this section sorts by duration, "+
			"not by the browser's cost ratio", flags[0].OriginID)
	}
	if s.OpenTotal != 3 {
		t.Errorf("OpenTotal = %d, want 3. The denominator counts what is OPEN, including "+
			"the calm ones this list does not show — otherwise the flagged count reads as "+
			"the whole floor", s.OpenTotal)
	}
	if s.PastWorry != 2 || s.PastConcern != 1 {
		t.Errorf("PastWorry=%d PastConcern=%d, want 2 and 1. Concern is a subset of worry: "+
			"an episode past the second line is also past the first, and counting them "+
			"disjointly would make the two tiles fail to add up to the list", s.PastWorry, s.PastConcern)
	}
	if s.Longest.Kind != CellValue || s.Longest.Text != "3h 00m" {
		t.Errorf("Longest = %+v, want value \"3h 00m\"", s.Longest)
	}
}

// TestMaterialFlagSummaryHasNoLongestWhenNothingIsOpen is constraint 3 on the
// one tile that would otherwise print the most reassuring possible number.
func TestMaterialFlagSummaryHasNoLongestWhenNothingIsOpen(t *testing.T) {
	flags, s := SelectMaterialFlags(nil)
	if len(flags) != 0 {
		t.Fatalf("got %d flags from no rows", len(flags))
	}
	if s.Longest.Kind != CellNoData {
		t.Errorf("Longest = %+v, want no-data. The longest of no episodes is not \"0 s\", "+
			"and 0 s is exactly what a broken read would print", s.Longest)
	}
	if s.OpenTotal != 0 {
		t.Errorf("OpenTotal = %d, want a real measured 0", s.OpenTotal)
	}
}

// ── Constraint 2, as a property of the type ──────────────────────────────────

// TestNoDurationTotalOnTheSummary makes "no metric grading anyone" checkable
// rather than aspirational.
//
// A count of what is open right now cannot be accumulated into a scorecard. A
// SUM OF DURATIONS can, trivially, and it is one field away at all times — so
// the absence of that field is asserted here, by reflection, with the reason
// attached. Anyone adding it has to delete this test in the same diff.
func TestNoDurationTotalOnTheSummary(t *testing.T) {
	// Named fields, so a rename does not silently satisfy the check.
	banned := map[string]bool{
		"Total": true, "TotalDuration": true, "TotalWaiting": true,
		"Minutes": true, "TotalMinutes": true, "DowntimeMinutes": true,
		"Sum": true, "SumDuration": true, "Cumulative": true,
	}
	for name := range banned {
		if hasField(MaterialFlagSummary{}, name) {
			t.Errorf("MaterialFlagSummary has a %q field. Summing durations is the one step "+
				"that turns this flag into a downtime-minutes metric, and the data does not "+
				"record whether anyone was waiting. Constraint 2: no metric grading anyone.", name)
		}
	}
	// And nothing on it may be a time.Duration at all — a duration on a summary is
	// an accumulation waiting to happen, and the one real duration (the longest
	// open episode) is carried as a rendered Cell precisely so it cannot be added
	// to anything.
	if f, ok := firstDurationField(MaterialFlagSummary{}); ok {
		t.Errorf("MaterialFlagSummary.%s is a time.Duration. The summary carries rendered "+
			"Cells, not arithmetic — a Duration here is an accumulation one line away.", f)
	}
}

func hasField(v any, name string) bool {
	_, ok := reflect.TypeOf(v).FieldByName(name)
	return ok
}

func firstDurationField(v any) (string, bool) {
	t := reflect.TypeOf(v)
	dur := reflect.TypeOf(time.Duration(0))
	for i := 0; i < t.NumField(); i++ {
		if t.Field(i).Type == dur {
			return t.Field(i).Name, true
		}
	}
	return "", false
}
