// end_of_line_test.go — the end-of-line sentence: the run-out measured from
// the record where a changeover's landing preceded the counter's reset, the
// typed depth where nothing is measured.
package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingoedge/store/counters"
	"shingoedge/store/stations"
)

// eolRig is a cell counted at its production counter: one consume position,
// styles A (CATID 1001) and B (1002), A running, the point on its counter tag.
type eolRig struct {
	*pairRig
	a, b, rp int64
	base     time.Time
}

func newEOLRig(t *testing.T) *eolRig {
	t.Helper()
	r := newPairRig(t)
	r.consume("EOL-1", "", "simple", true)
	const plcName, tag = "plc", "MES_Cell.Prod_Counter_01"
	_, err := r.db.Exec(`UPDATE processes SET counter_plc_name = ?, counter_tag_name = ? WHERE id = ?`, plcName, tag, r.pid)
	testutil.MustNoErr(t, err, "the process's counter")
	b, err := r.db.CreateStyle("EOL-B", "", r.pid)
	testutil.MustNoErr(t, err, "style B")
	testutil.MustNoErr(t, r.db.SetStyleExpectedCATID(r.style, "1001"), "pin A")
	testutil.MustNoErr(t, r.db.SetStyleExpectedCATID(b, "1002"), "pin B")
	rp, err := r.db.CreateReportingPoint(plcName, tag, r.style)
	testutil.MustNoErr(t, err, "the point")
	return &eolRig{pairRig: r, a: r.style, b: b, rp: rp, base: time.Now().Add(-6 * time.Hour).Truncate(time.Second)}
}

// at is minutes past the rig's base.
func (r *eolRig) at(min int) time.Time { return r.base.Add(time.Duration(min) * time.Minute) }

// ticks records n strokes of one each on catid, a minute apart from min.
func (r *eolRig) ticks(min, n int, catid string) {
	r.t.Helper()
	for i := 0; i < n; i++ {
		_, err := r.db.InsertCounterSnapshot(r.rp, int64(i+1), 1, "", counters.TickStamp{
			RecordedAt: r.at(min).Add(time.Duration(i) * time.Second), ProcessID: r.pid, StyleID: r.a, CATID: catid})
		testutil.MustNoErr(r.t, err, "a tick")
	}
}

// reset records the counter going back to 0 at min, the PLC on catid.
func (r *eolRig) reset(min int, catid string) {
	r.t.Helper()
	_, err := r.db.InsertCounterSnapshot(r.rp, 0, 0, "reset", counters.TickStamp{
		RecordedAt: r.at(min), ProcessID: r.pid, StyleID: r.a, CATID: catid})
	testutil.MustNoErr(r.t, err, "a reset")
}

// landed records a changeover from -> to completed at min.
func (r *eolRig) landed(from, to int64, min int) int64 {
	r.t.Helper()
	at := r.at(min).UTC().Format("2006-01-02 15:04:05")
	res, err := r.db.Exec(`INSERT INTO process_changeovers (process_id, from_style_id, to_style_id, state, called_by,
		notes, started_at, completed_at, triggered_by, updated_at) VALUES (?, ?, ?, 'completed', 'test', '', ?, ?, 'manual', ?)`,
		r.pid, from, to, at, at, at)
	testutil.MustNoErr(r.t, err, "a changeover")
	id, err := res.LastInsertId()
	testutil.MustNoErr(r.t, err, "its id")
	return id
}

func (r *eolRig) measured() map[int64]int64 {
	r.t.Helper()
	eol, err := r.eng.EndOfLine(r.pid)
	testutil.MustNoErr(r.t, err, "the end of the line")
	out := map[int64]int64{}
	for _, m := range eol.Measured {
		out[m.ChangeoverID] = m.Parts
	}
	return out
}

// TestRunOut_MeasuredOnlyWhenTheLandingPrecedesTheReset: the cart lands while
// A's last parts are still in the line; the 16 counted on A's CATID after the
// landing and before the counter's reset are the run-out. A landing whose
// counter has not reset yet (its run-out still running) is not measured.
func TestRunOut_MeasuredOnlyWhenTheLandingPrecedesTheReset(t *testing.T) {
	t.Parallel()
	r := newEOLRig(t)
	r.ticks(0, 10, "1001")
	first := r.landed(r.a, r.b, 20)
	r.ticks(21, 16, "1001") // the run-out, on A's part
	r.reset(40, "1002")     // the program change
	r.ticks(41, 5, "1002")
	second := r.landed(r.b, r.a, 60)
	r.ticks(61, 5, "1002") // B's run-out, the counter not reset yet
	got := r.measured()
	if len(got) != 1 || got[first] != 16 {
		t.Fatalf("measured run-outs = %v, want changeover %d: 16 and nothing for %d (no reset since its landing)",
			got, first, second)
	}
}

// TestRunOut_NoneWhenTheProgramChangedFirst: the program changed (the counter
// reset) before the landing, so the line had run out before the cart landed:
// nothing on A's part is counted after the landing, and nothing is measured.
func TestRunOut_NoneWhenTheProgramChangedFirst(t *testing.T) {
	t.Parallel()
	r := newEOLRig(t)
	r.ticks(0, 10, "1001")
	r.reset(10, "1002") // the program change
	r.ticks(11, 5, "1002")
	co := r.landed(r.a, r.b, 20)
	r.ticks(21, 8, "1002")
	r.reset(40, "1001") // the next program change
	r.ticks(41, 3, "1001")
	if got := r.measured(); len(got) != 0 {
		t.Fatalf("measured run-outs = %v, want none for changeover %d (the program changed first)", got, co)
	}
}

// TestSetup_SentenceCarriesTheMeasuredRangeOrTheTypedN: the setup page's
// sentence carries N measured — the run-outs' range, with each changeover and
// its date — where the record gives one, else the typed N with who typed it
// and when, else says neither exists. The typed N never hides a measured one.
func TestSetup_SentenceCarriesTheMeasuredRangeOrTheTypedN(t *testing.T) {
	t.Parallel()
	const head = "Counted at the end of the line: carts are charged when the part is made."
	sentence := func(r *eolRig) (string, bool) {
		eol, err := r.eng.EndOfLine(r.pid)
		testutil.MustNoErr(t, err, "the end of the line")
		if !eol.Applies {
			t.Fatal("a process counted at its production counter: the sentence does not apply")
		}
		return eol.Sentence, eol.HasN
	}
	day := func(r *eolRig, min int) string { return r.at(min).UTC().Format("2006-01-02") }

	t.Run("measured", func(t *testing.T) {
		t.Parallel()
		r := newEOLRig(t)
		one := r.landed(r.a, r.b, 0)
		r.ticks(1, 12, "1001")
		r.reset(20, "1002")
		two := r.landed(r.b, r.a, 30)
		r.ticks(31, 16, "1002")
		r.reset(50, "1001")
		n := int64(9)
		testutil.MustNoErr(t, r.eng.ProcessService().SetLineDepth(r.pid, &n, "engineer"), "type a depth")
		want := fmt.Sprintf("%s About 12–16 parts sit between the cart and the counter (measured at changeovers #%d on %s, #%d on %s).",
			head, one, day(r, 0), two, day(r, 30))
		if got, hasN := sentence(r); got != want || !hasN {
			t.Fatalf("sentence = %q, want %q", got, want)
		}
	})
	t.Run("typed", func(t *testing.T) {
		t.Parallel()
		r := newEOLRig(t)
		n := int64(14)
		testutil.MustNoErr(t, r.eng.ProcessService().SetLineDepth(r.pid, &n, "engineer"), "type a depth")
		typed, err := r.db.GetLineDepth(r.pid)
		testutil.MustNoErr(t, err, "the typed depth")
		want := fmt.Sprintf("%s About 14 parts sit between the cart and the counter (typed by engineer on %s).",
			head, typed.At.UTC().Format("2006-01-02"))
		if got, hasN := sentence(r); got != want || !hasN {
			t.Fatalf("sentence = %q, want %q", got, want)
		}
	})
	t.Run("neither", func(t *testing.T) {
		t.Parallel()
		r := newEOLRig(t)
		if got, hasN := sentence(r); hasN || !strings.HasPrefix(got, head) || !strings.Contains(got, "not measured yet") {
			t.Fatalf("sentence = %q (has N %t), want the first half and that nothing is measured or typed", got, hasN)
		}
	})
}

// TestStationTile_CarriesTheEndOfLineSentence: a station's tile at a consume
// position the production counter counts carries the sentence once it has an
// N (here typed); a position counted at a station counter carries none.
func TestStationTile_CarriesTheEndOfLineSentence(t *testing.T) {
	t.Parallel()
	r := newEOLRig(t)
	other := r.consume("EOL-2", "", "simple", true)
	stationCounter(t, r.db, r.pid, other)
	sid, err := r.db.CreateOperatorStation(stations.Input{ProcessID: r.pid, Name: "EOL-STATION"})
	testutil.MustNoErr(t, err, "a station")
	_, err = r.db.Exec(`UPDATE process_nodes SET operator_station_id = ? WHERE process_id = ?`, sid, r.pid)
	testutil.MustNoErr(t, err, "the positions on it")
	n := int64(14)
	testutil.MustNoErr(t, r.eng.ProcessService().SetLineDepth(r.pid, &n, "engineer"), "type a depth")
	r.eng.stationService.SetEndOfLineResolver(r.eng.endOfLineNote)

	view, err := r.eng.stationService.BuildView(context.Background(), sid)
	testutil.MustNoErr(t, err, "the view")
	got := map[string]string{}
	for _, tile := range view.Nodes {
		got[tile.Node.Name] = tile.EndOfLine
	}
	if !strings.Contains(got["EOL-1"], "About 14 parts sit between the cart and the counter") {
		t.Errorf("the cell-counted tile carries %q, want the sentence with the typed 14", got["EOL-1"])
	}
	if got["EOL-2"] != "" {
		t.Errorf("the station-counted tile carries %q, want none", got["EOL-2"])
	}
}
