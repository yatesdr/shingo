//go:build docker

package audit_test

import (
	"database/sql"
	"testing"
	"time"

	"shingocore/domain"
	"shingocore/internal/testdb"
	"shingocore/store/audit"
)

// cycle_time_docker_test.go — ListCycleEvents against real Postgres.
//
// The drift guards in cycle_time_test.go hold this package's claims about the
// APPLIER's source. This file holds the claims about the QUERY, and it needs a
// database because every one of them is a property of SQL rather than of Go:
// that metadata->>'reason' selects on jsonb the way the filter assumes, that the
// op filter excludes the observation rows, that the cap keeps the NEWEST rows
// rather than an arbitrary page, and that the result comes back oldest-first.
//
// The failure mode without it is quiet. A query that ERRORS renders the page's
// read-failure card, which is at least honest; a query that RUNS and selects the
// wrong rows renders a distribution that looks exactly like a real one.

func seedDelta(t *testing.T, db *sql.DB, op, payload, actor, reason string, at time.Time) {
	t.Helper()
	var meta any
	if reason != "" {
		meta = `{"reason":"` + reason + `","delta":1,"sequence_id":7}`
	}
	if _, err := db.Exec(`INSERT INTO bin_uop_ledger
		(bin_id, before_uop, after_uop, op, source, payload_code, actor, metadata, applied_at)
		VALUES (901, 0, 1, $1, 'test', $2, $3, $4, $5)`,
		op, payload, actor, meta, at); err != nil {
		t.Fatalf("seed bin_uop_ledger: %v", err)
	}
}

func onlyStation(events []domain.CycleEvent, station string) []domain.CycleEvent {
	out := make([]domain.CycleEvent, 0, len(events))
	for _, e := range events {
		if e.Station == station {
			out = append(out, e)
		}
	}
	return out
}

func TestListCycleEvents_SelectsOnlyAppliedCycleTicks(t *testing.T) {
	db := testdb.Open(t).DB

	base := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	const station = "CYCLE-TEST-STATION"

	// Two intervals of PART-A produce, one PART-B, one consume tick — all real.
	seedDelta(t, db, "bin_uop_delta", "PART-A", station, "produce_tick", base)
	seedDelta(t, db, "bin_uop_delta", "PART-A", station, "produce_tick", base.Add(25*time.Second))
	seedDelta(t, db, "bin_uop_delta", "PART-A", station, "produce_tick", base.Add(50*time.Second))
	seedDelta(t, db, "bin_uop_delta", "PART-B", station, "produce_tick", base.Add(10*time.Second))
	seedDelta(t, db, "bin_uop_delta", "PART-A", station, "consume_tick", base.Add(30*time.Second))

	// A blank payload: INCLUDED by the query, reported as unattributable by the
	// domain. The query must not filter it, or the page loses the ability to say
	// how many rows it could not use.
	seedDelta(t, db, "bin_uop_delta", "", station, "produce_tick", base.Add(15*time.Second))

	// Excluded, and each for its own reason. An operator pulling parts and an
	// admin correcting a count are human decisions, not a machine's cadence. A
	// stale-epoch row is an OBSERVATION of a delta that was dropped rather than
	// applied — it moved no count, so it is not a tick. A cycle count is neither.
	seedDelta(t, db, "bin_uop_delta", "PART-A", station, "capture_reduction", base.Add(20*time.Second))
	seedDelta(t, db, "bin_uop_delta", "PART-A", station, "operator_correction", base.Add(35*time.Second))
	seedDelta(t, db, "stale_epoch_dropped", "PART-A", station, "produce_tick", base.Add(40*time.Second))
	seedDelta(t, db, "cycle_count", "PART-A", station, "", base.Add(45*time.Second))

	events, truncated, err := audit.ListCycleEvents(db, base.Add(-time.Minute), 500)
	if err != nil {
		t.Fatalf("ListCycleEvents: %v", err)
	}
	if truncated {
		t.Error("the cap bit on a ten-row fixture")
	}

	mine := onlyStation(events, station)
	if len(mine) != 6 {
		t.Fatalf("got %d events for %s, want 6 (five attributable ticks plus the blank "+
			"payload). An excluded reason or op is leaking in: %+v", len(mine), station, mine)
	}

	for _, e := range mine {
		switch e.Direction {
		case domain.CycleDirectionProduce, domain.CycleDirectionConsume:
		default:
			t.Errorf("a %q row was selected — only produce and consume ticks are cycles", e.Direction)
		}
	}

	// OLDEST FIRST. The query orders DESC so the cap keeps the newest rows; the
	// differencing wants chronological order, and a reversal would produce a
	// series of negative gaps that the domain clamps to zero — a whole page of
	// zero-second cycles with nothing saying why.
	for i := 1; i < len(mine); i++ {
		if mine[i].At.Before(mine[i-1].At) {
			t.Fatalf("events are not oldest-first at index %d: %s then %s",
				i, mine[i-1].At, mine[i].At)
		}
	}

	// And the point of the whole seam: the partition produces the takt, not the
	// interleave. PART-A produce is a 25 s cycle; every other row seeded above
	// sits inside that span and would corrupt it if it reached this partition.
	series, unattributable := domain.BuildCycleSeries(mine)
	if unattributable != 1 {
		t.Errorf("unattributable = %d, want 1 (the blank payload)", unattributable)
	}
	var produceA *domain.CycleSeries
	for i := range series {
		if series[i].Key.Payload == "PART-A" && series[i].Key.Direction == domain.CycleDirectionProduce {
			produceA = &series[i]
		}
	}
	if produceA == nil {
		t.Fatalf("no PART-A produce series: %+v", series)
	}
	if len(produceA.Gaps) != 2 {
		t.Fatalf("PART-A produce has %d gaps, want 2", len(produceA.Gaps))
	}
	for _, g := range produceA.Gaps {
		if g != 25*time.Second {
			t.Errorf("PART-A produce gap %s, want 25s — anything else means a row from "+
				"another key or another reason landed in this partition", g)
		}
	}
}

func TestListCycleEvents_WindowAndCap(t *testing.T) {
	db := testdb.Open(t).DB

	base := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	const station = "CYCLE-WINDOW-STATION"
	for i := 0; i < 6; i++ {
		seedDelta(t, db, "bin_uop_delta", "PART-W", station, "produce_tick",
			base.Add(time.Duration(i)*time.Minute))
	}

	// The window excludes what predates it.
	events, _, err := audit.ListCycleEvents(db, base.Add(3*time.Minute), 500)
	if err != nil {
		t.Fatalf("ListCycleEvents: %v", err)
	}
	if got := len(onlyStation(events, station)); got != 3 {
		t.Errorf("a window from base+3m returned %d rows for this station, want 3", got)
	}

	// THE CAP KEEPS THE NEWEST ROWS, so every gap inside the returned set is a
	// real gap between two adjacent events. A cap that kept the oldest, or an
	// arbitrary page, would leave gaps that skip over unread rows and read as
	// long cycles that never happened.
	var newest time.Time
	if err := db.QueryRow(`SELECT MAX(applied_at) FROM bin_uop_ledger
		WHERE op = 'bin_uop_delta'
		  AND metadata->>'reason' IN ('produce_tick','consume_tick')`).Scan(&newest); err != nil {
		t.Fatalf("read newest qualifying row: %v", err)
	}

	events, truncated, err := audit.ListCycleEvents(db, base.Add(-time.Minute), 2)
	if err != nil {
		t.Fatalf("ListCycleEvents: %v", err)
	}
	if !truncated {
		t.Error("the cap bit but was not reported — a distribution over a silently " +
			"shortened window misreports its own n")
	}
	if len(events) != 2 {
		t.Fatalf("got %d rows under a cap of 2", len(events))
	}
	if events[0].At.After(events[1].At) {
		t.Error("the capped result is not oldest-first")
	}
	if !events[1].At.Equal(newest.In(events[1].At.Location())) {
		t.Errorf("the newest returned row is %s, but the newest qualifying row in the table "+
			"is %s — the cap must keep the most recent contiguous run, or the gaps inside "+
			"the result skip over rows that were never read", events[1].At, newest)
	}
}
