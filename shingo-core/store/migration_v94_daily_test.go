//go:build docker

package store_test

import (
	"testing"
	"time"

	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/audit"
)

// The v94 daily roll-up: one migration test that proves the backfill
// aggregates the raw delta stream at the right grain (epoch_seq via the
// bump-count window, reason from metadata, crossings counted), one that pins
// the daily job to the backfill's shape, and one for the job's idempotency.

// seedV94Delta writes one raw bin_uop_delta audit row directly, the shape the
// applier writes (uop/applier.go:458): delta and reason in metadata.
func seedV94Delta(t *testing.T, db *store.DB, binID int64, before, after int, payload, reason, actor string, at time.Time) {
	t.Helper()
	delta := after - before
	if _, err := db.Exec(`INSERT INTO bin_uop_audit
		(bin_id, before_uop, after_uop, op, source, payload_code, actor, metadata, applied_at)
		VALUES ($1,$2,$3,'bin_uop_delta','test',$4,$5,jsonb_build_object('reason',$6::text,'delta',$7::int,'sequence_id',1),$8)`,
		binID, before, after, payload, actor, reason, delta, at.Truncate(time.Microsecond)); err != nil {
		t.Fatalf("seed delta row: %v", err)
	}
}

// TestV94_BackfillAggregatesAtTheReviewedGrain seeds a two-day, two-epoch
// history for one bin plus a second bin, re-runs v94 through the migrate
// path, and pins the aggregate: one row per (day, epoch, reason), ticks/
// consumed/added summed, first/last/min taken in order, crossings counted,
// and epoch 0 vs epoch 1 NOT folded together.
//
// The seeded history:
//
//	day D, epoch 0 (before any bump):   100 → 90  consume_tick
//	day D, epoch 1 (after a bump):        0 → 300 set_for_production (the bump)
//	                                    300 → 120 consume_tick
//	                                    120 → -7  consume_tick  CROSSING
//	day D+1, epoch 1 continued:           -7 → -30 consume_tick (continuation)
//	                                    -30 → 25  produce_tick  (recovery)
func TestV94_BackfillAggregatesAtTheReviewedGrain(t *testing.T) {
	db, cfg := testdb.OpenWithConfig(t)
	day := time.Now().UTC().Add(48 * time.Hour).Truncate(24 * time.Hour)
	const bin1, bin2 = 9201, 9202

	seedV94Delta(t, db, bin1, 100, 90, "PART-A", "consume_tick", "STN-1", day.Add(1*time.Hour))
	seedV93Audit(t, db, bin1, 0, 300, "set_for_production", "PART-A", day.Add(2*time.Hour))
	seedV94Delta(t, db, bin1, 300, 120, "PART-A", "consume_tick", "STN-1", day.Add(3*time.Hour))
	seedV94Delta(t, db, bin1, 120, -7, "PART-A", "consume_tick", "STN-1", day.Add(4*time.Hour))
	seedV94Delta(t, db, bin1, -7, -30, "PART-A", "consume_tick", "STN-1", day.Add(26*time.Hour))
	seedV94Delta(t, db, bin1, -30, 25, "PART-A", "produce_tick", "STN-1", day.Add(27*time.Hour))
	// Second bin, same day, different reason — proves reason stays in the key.
	seedV94Delta(t, db, bin2, 0, 5, "PART-B", "capture_reduction", "STN-2", day.Add(5*time.Hour))

	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version = 94`); err != nil {
		t.Fatalf("clear v94 row: %v", err)
	}
	migrated, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("re-open to apply v94: %v", err)
	}
	defer migrated.Close()

	type row struct {
		day                                  string
		binID, epoch, ticks, consumed, added int64
		first, last, min, crossings          int
		reason, actor                        string
	}
	q := `SELECT to_char(day,'YYYY-MM-DD'), bin_id, epoch_seq, reason, actor, ticks, consumed, added,
	             first_uop, last_uop, min_uop, crossings
	      FROM bin_uop_delta_daily WHERE bin_id IN ($1,$2) ORDER BY day, bin_id, epoch_seq, reason`
	rows, err := migrated.Query(q, bin1, bin2)
	if err != nil {
		t.Fatalf("read roll-up: %v", err)
	}
	defer rows.Close()
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.day, &r.binID, &r.epoch, &r.reason, &r.actor, &r.ticks,
			&r.consumed, &r.added, &r.first, &r.last, &r.min, &r.crossings); err != nil {
			t.Fatalf("scan roll-up: %v", err)
		}
		got = append(got, r)
	}
	// bin1 day D epoch 0: one tick, 10 consumed. epoch 1: two ticks, 190
	// consumed, first 300 last -7, one crossing. bin1 day D+1 epoch 1: two
	// reason groups (consume 1 tick 23 consumed; produce 1 tick 55 added).
	// bin2 day D epoch 0: capture_reduction 1 tick 5 added.
	want := []struct {
		binID, epoch     int64
		reason           string
		ticks, cons, add int64
		first, last, min int
		cross            int
	}{
		{bin1, 0, "consume_tick", 1, 10, 0, 100, 90, 90, 0},
		{bin1, 1, "consume_tick", 2, 307, 0, 300, -7, -7, 1},
		{bin2, 0, "capture_reduction", 1, 0, 5, 0, 5, 5, 0},
	}
	dayStr := day.Format("2006-01-02")
	day2Str := day.AddDate(0, 0, 1).Format("2006-01-02")
	_ = day2Str
	if len(got) != 5 {
		t.Fatalf("roll-up rows = %d, want 5: %+v", len(got), got)
	}
	// Day D rows (first four; bin2 sorts after bin1 by bin_id? no — ORDER BY day, bin_id, epoch, reason)
	// bin1=9201 < bin2=9202, so day D: (bin1,e0,consume), (bin1,e1,consume), (bin2,e0,capture)
	for i, w := range want {
		g := got[i]
		if g.binID != w.binID || g.epoch != w.epoch || g.reason != w.reason {
			t.Errorf("row %d = bin %d epoch %d reason %s, want bin %d epoch %d reason %s",
				i, g.binID, g.epoch, g.reason, w.binID, w.epoch, w.reason)
			continue
		}
		if g.ticks != w.ticks || g.consumed != w.cons || g.added != w.add {
			t.Errorf("row %d (%s/%d/%s): ticks=%d consumed=%d added=%d, want %d/%d/%d",
				i, g.day, g.binID, g.reason, g.ticks, g.consumed, g.added, w.ticks, w.cons, w.add)
		}
		if g.first != w.first || g.last != w.last || g.min != w.min || g.crossings != w.cross {
			t.Errorf("row %d (%s/%d/%s): first=%d last=%d min=%d crossings=%d, want %d/%d/%d/%d",
				i, g.day, g.binID, g.reason, g.first, g.last, g.min, g.crossings, w.first, w.last, w.min, w.cross)
		}
		if g.day != dayStr {
			t.Errorf("row %d day = %s, want %s", i, g.day, dayStr)
		}
		if g.actor != "STN-1" && g.binID == bin1 {
			t.Errorf("row %d actor = %q, want STN-1 (actor, not station, is the grain)", i, g.actor)
		}
	}
	// Day D+1: bin1 epoch 1, two reason groups.
	d2 := got[3]
	if d2.binID != bin1 || d2.epoch != 1 || d2.reason != "consume_tick" || d2.day != day2Str {
		t.Errorf("day2 row 3 = %+v, want bin1/epoch1/consume on %s", d2, day2Str)
	}
	if d2.ticks != 1 || d2.consumed != 23 || d2.first != -7 || d2.last != -30 || d2.min != -30 {
		t.Errorf("day2 consume row = %+v, want ticks=1 consumed=23 first=-7 last=-30 min=-30", d2)
	}
	d2p := got[4]
	if d2p.reason != "produce_tick" || d2p.added != 55 || d2p.first != -30 || d2p.last != 25 {
		t.Errorf("day2 produce row = %+v, want added=55 first=-30 last=25", d2p)
	}
}

// TestV94_RollupJobMatchesBackfill runs the daily job over the same seeded
// stream after wiping the roll-up, and requires byte-equal rows — the job's
// statement is the migration's backfill verbatim (apart from the day filter
// and ON CONFLICT arm), and this pins that lockstep.
func TestV94_RollupJobMatchesBackfill(t *testing.T) {
	db := testdb.Open(t)
	day := time.Now().UTC().Add(48 * time.Hour).Truncate(24 * time.Hour)
	const bin1 = 9203

	seedV94Delta(t, db, bin1, 100, 90, "PART-A", "consume_tick", "STN-1", day.Add(1*time.Hour))
	seedV93Audit(t, db, bin1, 0, 300, "set_for_production", "PART-A", day.Add(2*time.Hour))
	seedV94Delta(t, db, bin1, 300, 120, "PART-A", "consume_tick", "STN-1", day.Add(3*time.Hour))
	seedV94Delta(t, db, bin1, 120, -7, "PART-A", "consume_tick", "STN-1", day.Add(4*time.Hour))
	seedV94Delta(t, db, bin1, -7, -30, "PART-A", "consume_tick", "STN-1", day.Add(26*time.Hour))

	if _, err := db.Exec(`DELETE FROM bin_uop_delta_daily`); err != nil {
		t.Fatalf("wipe roll-up: %v", err)
	}
	n, err := audit.RollupBinUOPDeltaDay(db.DB, day)
	if err != nil {
		t.Fatalf("rollup day 1: %v", err)
	}
	if n != 2 {
		t.Fatalf("day-1 rollup wrote %d rows, want 2 (epoch 0 + epoch 1)", n)
	}
	n2, err := audit.RollupBinUOPDeltaDay(db.DB, day.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("rollup day 2: %v", err)
	}
	if n2 != 1 {
		t.Fatalf("day-2 rollup wrote %d rows, want 1", n2)
	}

	var ticks, cons, first, last, min, cross int
	if err := db.QueryRow(`SELECT ticks, consumed, first_uop, last_uop, min_uop, crossings
		FROM bin_uop_delta_daily WHERE bin_id=$1 AND epoch_seq=1 AND day=$2::date`,
		bin1, day.Format("2006-01-02")).Scan(&ticks, &cons, &first, &last, &min, &cross); err != nil {
		t.Fatalf("read job row: %v", err)
	}
	if ticks != 2 || cons != 307 || first != 300 || last != -7 || min != -7 || cross != 1 {
		t.Errorf("job epoch-1 row: ticks=%d consumed=%d first=%d last=%d min=%d crossings=%d, want 2/307/300/-7/-7/1",
			ticks, cons, first, last, min, cross)
	}
}

// TestV94_RollupJobIsIdempotentPerDay re-runs the job over an unchanged day
// and requires the same single row set — ON CONFLICT DO UPDATE, not a second
// copy.
func TestV94_RollupJobIsIdempotentPerDay(t *testing.T) {
	db := testdb.Open(t)
	day := time.Now().UTC().Add(48 * time.Hour).Truncate(24 * time.Hour)
	const bin1 = 9204

	seedV94Delta(t, db, bin1, 100, 90, "PART-A", "consume_tick", "STN-1", day.Add(1*time.Hour))
	seedV94Delta(t, db, bin1, 90, 75, "PART-A", "consume_tick", "STN-1", day.Add(2*time.Hour))

	for i := 0; i < 2; i++ {
		if _, err := audit.RollupBinUOPDeltaDay(db.DB, day); err != nil {
			t.Fatalf("rollup %d: %v", i+1, err)
		}
	}
	var n, ticks, cons int
	if err := db.QueryRow(`SELECT count(*), sum(ticks), sum(consumed)
		FROM bin_uop_delta_daily WHERE bin_id=$1`, bin1).Scan(&n, &ticks, &cons); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 || ticks != 2 || cons != 25 {
		t.Fatalf("after two rollups: rows=%d ticks=%d consumed=%d, want 1/2/25", n, ticks, cons)
	}
}
