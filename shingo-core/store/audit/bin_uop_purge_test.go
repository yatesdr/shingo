//go:build docker

package audit_test

import (
	"testing"
	"time"

	"database/sql"

	"shingocore/internal/testdb"
	"shingocore/store/audit"
)

// The D6 retention purge: delta rows past the window go; everything else —
// fresh deltas, old NON-delta rows (bump ops feed the epoch derivation, the
// rare observations are forensic) — stays.

func TestPurgeOldBinUOPDelta_DeletesOnlyExpiredDeltaRows(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	now := time.Now().UTC()
	old := now.Add(-91 * 24 * time.Hour)
	fresh := now.Add(-10 * 24 * time.Hour)
	pi := func(n int) *int { return &n }

	// Expired delta: deleted.
	mustAppend(t, db.DB, 1, pi(10), 7, "bin_uop_delta", old)
	// Fresh delta: stays.
	mustAppend(t, db.DB, 1, pi(7), 4, "bin_uop_delta", fresh)
	// Expired NON-delta (bump op): stays forever — the epoch derivation counts it.
	mustAppend(t, db.DB, 1, pi(0), 300, audit.OpSetForProduction, old)
	// Expired NON-delta (rare observation): stays.
	mustAppendOverride(t, db.DB, 1, 50, 50, audit.OpStaleEpochDropped, old)

	n, err := audit.PurgeOldBinUOPDelta(db.DB, 90, now)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 1 {
		t.Fatalf("purged %d rows, want 1 (the expired delta only)", n)
	}

	var deltas, bumps, obs, freshDeltas int
	if err := db.QueryRow(`SELECT
		count(*) FILTER (WHERE op='bin_uop_delta' AND applied_at < $1),
		count(*) FILTER (WHERE op = $2),
		count(*) FILTER (WHERE op = $3),
		count(*) FILTER (WHERE op='bin_uop_delta' AND applied_at >= $1)
		FROM bin_uop_ledger WHERE bin_id=1`,
		now.Add(-90*24*time.Hour), audit.OpSetForProduction, audit.OpStaleEpochDropped).
		Scan(&deltas, &bumps, &obs, &freshDeltas); err != nil {
		t.Fatalf("read: %v", err)
	}
	if deltas != 0 {
		t.Errorf("expired deltas remaining = %d, want 0", deltas)
	}
	if freshDeltas != 1 {
		t.Errorf("fresh deltas remaining = %d, want 1", freshDeltas)
	}
	if bumps != 1 || obs != 1 {
		t.Errorf("non-delta rows: bump=%d obs=%d, want 1 and 1 (never purged)", bumps, obs)
	}

	// Idempotent: a second run finds nothing.
	n2, err := audit.PurgeOldBinUOPDelta(db.DB, 90, now)
	if err != nil {
		t.Fatalf("second purge: %v", err)
	}
	if n2 != 0 {
		t.Errorf("second purge deleted %d rows, want 0", n2)
	}
}

// The daily roll-up (RollupBinUOPDeltaDay), the permanent half of the D6 trade
// beside the purge above. These three replace store's v94 migration tests:
// the backfill they compared the job against is gone with the chain, so the
// job's grain is pinned on its own.

// dailyRow is one bin_uop_delta_daily row as the roll-up writes it.
type dailyRow struct {
	day                         string
	binID, epoch                int64
	reason, station             string
	ticks, consumed, added      int64
	first, last, min, crossings int
}

// readDaily returns bin_uop_delta_daily's rows for bins, in grain order.
func readDaily(t *testing.T, db *sql.DB, bins ...int64) []dailyRow {
	t.Helper()
	rows, err := db.Query(`SELECT to_char(day,'YYYY-MM-DD'), bin_id, epoch_seq, reason, station_uid, ticks, consumed, added,
		       first_uop, last_uop, min_uop, crossings
		FROM bin_uop_delta_daily WHERE bin_id = ANY($1) ORDER BY day, bin_id, epoch_seq, reason`, bins)
	if err != nil {
		t.Fatalf("read roll-up: %v", err)
	}
	defer rows.Close()
	var got []dailyRow
	for rows.Next() {
		var r dailyRow
		if err := rows.Scan(&r.day, &r.binID, &r.epoch, &r.reason, &r.station, &r.ticks,
			&r.consumed, &r.added, &r.first, &r.last, &r.min, &r.crossings); err != nil {
			t.Fatalf("scan roll-up: %v", err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("roll-up rows: %v", err)
	}
	return got
}

// seedRollupDelta writes one raw bin_uop_delta row in the applier's shape:
// the delta and its reason in metadata.
func seedRollupDelta(t *testing.T, db *sql.DB, binID int64, before, after int, payload, reason, station string, at time.Time) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO bin_uop_ledger
		(bin_id, before_uop, after_uop, op, source, payload_code, station_uid, metadata, applied_at)
		VALUES ($1,$2,$3,'bin_uop_delta','test',$4,$5,jsonb_build_object('reason',$6::text,'delta',$7::int,'sequence_id',1),$8)`,
		binID, before, after, payload, station, reason, after-before, at.Truncate(time.Microsecond)); err != nil {
		t.Fatalf("seed delta row: %v", err)
	}
}

// TestRollupBinUOPDeltaDay_AggregatesAtTheReviewedGrain: one row per (day,
// bin, epoch, payload, reason, station_uid); ticks, consumed and added summed;
// first, last and min taken in order; crossings counted; epoch 0 and epoch 1
// of one day not folded together; a second day's rows split by reason. It is
// the runtime half of store's deleted TestV94_BackfillAggregatesAtTheReviewedGrain,
// run through the job over the same two-day history:
//
//	day D, epoch 0 (before any bump):     100 → 90  consume_tick
//	day D, epoch 1 (after a bump):          0 → 300 set_for_production (the bump)
//	                                      300 → 120 consume_tick
//	                                      120 → -7  consume_tick  CROSSING
//	day D+1, epoch 1 continued:            -7 → -30 consume_tick (continuation)
//	                                      -30 → 25  produce_tick  (recovery)
//	day D, a second bin:                    0 → 5   capture_reduction
func TestRollupBinUOPDeltaDay_AggregatesAtTheReviewedGrain(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	day := time.Now().UTC().Add(48 * time.Hour).Truncate(24 * time.Hour)
	const bin1, bin2 = 9201, 9202
	pi := func(n int) *int { return &n }

	seedRollupDelta(t, db.DB, bin1, 100, 90, "PART-A", "consume_tick", "STN-1", day.Add(1*time.Hour))
	mustAppend(t, db.DB, bin1, pi(0), 300, audit.OpSetForProduction, day.Add(2*time.Hour))
	seedRollupDelta(t, db.DB, bin1, 300, 120, "PART-A", "consume_tick", "STN-1", day.Add(3*time.Hour))
	seedRollupDelta(t, db.DB, bin1, 120, -7, "PART-A", "consume_tick", "STN-1", day.Add(4*time.Hour))
	seedRollupDelta(t, db.DB, bin1, -7, -30, "PART-A", "consume_tick", "STN-1", day.Add(26*time.Hour))
	seedRollupDelta(t, db.DB, bin1, -30, 25, "PART-A", "produce_tick", "STN-1", day.Add(27*time.Hour))
	seedRollupDelta(t, db.DB, bin2, 0, 5, "PART-B", "capture_reduction", "STN-2", day.Add(5*time.Hour))

	for i, wantN := range []int64{3, 2} {
		n, err := audit.RollupBinUOPDeltaDay(db.DB, day.AddDate(0, 0, i))
		if err != nil {
			t.Fatalf("rollup day %d: %v", i+1, err)
		}
		if n != wantN {
			t.Errorf("day-%d rollup wrote %d rows, want %d", i+1, n, wantN)
		}
	}

	d1, d2 := day.Format("2006-01-02"), day.AddDate(0, 0, 1).Format("2006-01-02")
	want := []dailyRow{
		{d1, bin1, 0, "consume_tick", "STN-1", 1, 10, 0, 100, 90, 90, 0},
		{d1, bin1, 1, "consume_tick", "STN-1", 2, 307, 0, 300, -7, -7, 1},
		{d1, bin2, 0, "capture_reduction", "STN-2", 1, 0, 5, 0, 5, 5, 0},
		{d2, bin1, 1, "consume_tick", "STN-1", 1, 23, 0, -7, -30, -30, 0},
		{d2, bin1, 1, "produce_tick", "STN-1", 1, 0, 55, -30, 25, 25, 0},
	}
	got := readDaily(t, db.DB, bin1, bin2)
	if len(got) != len(want) {
		t.Fatalf("roll-up rows = %d, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestRollupBinUOPDeltaDay_TheEpochCarriesAcrossDays: the epoch window is the
// bin's whole stream, not the day, so day 2's only delta, which follows the
// day-1 bump, lands in epoch 1. A day-scoped window would number it 0,
// disagreeing with NextBinUOPEpochSeq. It replaces store's
// deleted TestV94_RollupJobMatchesBackfill, whose body it keeps; the backfill that
// test's name compared against is gone with the chain.
func TestRollupBinUOPDeltaDay_TheEpochCarriesAcrossDays(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	day := time.Now().UTC().Add(48 * time.Hour).Truncate(24 * time.Hour)
	const bin1 = 9203
	pi := func(n int) *int { return &n }

	seedRollupDelta(t, db.DB, bin1, 100, 90, "PART-A", "consume_tick", "STN-1", day.Add(1*time.Hour))
	mustAppend(t, db.DB, bin1, pi(0), 300, audit.OpSetForProduction, day.Add(2*time.Hour))
	seedRollupDelta(t, db.DB, bin1, 300, 120, "PART-A", "consume_tick", "STN-1", day.Add(3*time.Hour))
	seedRollupDelta(t, db.DB, bin1, 120, -7, "PART-A", "consume_tick", "STN-1", day.Add(4*time.Hour))
	seedRollupDelta(t, db.DB, bin1, -7, -30, "PART-A", "consume_tick", "STN-1", day.Add(26*time.Hour))

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
	var day2Epoch int64
	if err := db.QueryRow(`SELECT epoch_seq FROM bin_uop_delta_daily
		WHERE bin_id=$1 AND day=$2::date`, bin1, day.AddDate(0, 0, 1).Format("2006-01-02")).Scan(&day2Epoch); err != nil {
		t.Fatalf("read day-2 epoch: %v", err)
	}
	if day2Epoch != 1 {
		t.Fatalf("day-2 delta landed in epoch %d, want 1 (the day-1 bump must carry across days)", day2Epoch)
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

// TestRollupBinUOPDeltaDay_IsIdempotentPerDay re-runs the job over an
// unchanged day and requires the same single row set: ON CONFLICT DO UPDATE,
// not a second copy and not a sum of two runs. It replaces store's
// deleted TestV94_RollupJobIsIdempotentPerDay.
func TestRollupBinUOPDeltaDay_IsIdempotentPerDay(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	day := time.Now().UTC().Add(48 * time.Hour).Truncate(24 * time.Hour)
	const bin1 = 9204

	seedRollupDelta(t, db.DB, bin1, 100, 90, "PART-A", "consume_tick", "STN-1", day.Add(1*time.Hour))
	seedRollupDelta(t, db.DB, bin1, 90, 75, "PART-A", "consume_tick", "STN-1", day.Add(2*time.Hour))

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

// mustAppend writes one bin_uop_ledger row at a controlled time. bin_id is
// planted directly (no FK on bin_uop_ledger.bin_id? it has none — the audit
// stream deliberately keeps rows whose bin is gone).
func mustAppend(t *testing.T, db *sql.DB, binID int64, before *int, after int, op string, at time.Time) {
	t.Helper()
	var b any
	if before != nil {
		b = *before
	}
	if _, err := db.Exec(`INSERT INTO bin_uop_ledger
		(bin_id, before_uop, after_uop, op, source, payload_code, actor, applied_at)
		VALUES ($1,$2,$3,$4,'test','PART-A','test',$5)`,
		binID, b, after, op, at.Truncate(time.Microsecond)); err != nil {
		t.Fatalf("seed audit row (%s): %v", op, err)
	}
}

func mustAppendOverride(t *testing.T, db *sql.DB, binID int64, suggested, operator int, op string, at time.Time) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO bin_uop_ledger
		(bin_id, before_uop, after_uop, op, source, payload_code, actor, metadata, applied_at)
		VALUES ($1,$2,$3,$4,'test','PART-A','test','{"delta":-3}',$5)`,
		binID, suggested, operator, op, at.Truncate(time.Microsecond)); err != nil {
		t.Fatalf("seed override row (%s): %v", op, err)
	}
}
