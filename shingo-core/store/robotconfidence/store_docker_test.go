//go:build docker

package robotconfidence_test

import (
	"database/sql"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/robotconfidence"
)

// store_docker_test.go — the Postgres-side guards on migration v77.
//
// WHAT THESE COVER THAT THE PURE TESTS CANNOT. The write rule and the residual
// math hold entirely without a database, which is why they are tag-free. What
// they cannot reach is whether the partition a row needs exists when the row
// arrives, whether a drop takes only the day it was aimed at, whether a NULL
// measure scans back as NULL rather than as 0.0, and — the one that motivated
// this file's ordering — whether the roll-up can emit a row for a segment that
// produced no valid sample at all.

var testDay = time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)

func rollUpCfg() robotconfidence.RollUpConfig {
	return robotconfidence.RollUpConfig{
		SnapTolerance: 2.0,
		BaselineDays:  14,
		Coverage:      robotconfidence.DefaultCoverage,
		// Required, not optional: version_id is NOT NULL, so a roll-up with
		// no resolver quarantines everything and writes nothing.
		Versions: store.LaneVersionResolver{},
	}
}

// openWithWindow opens a test database and pre-creates partitions across the
// whole baseline window so historical inserts never fall through a gap.
func openWithWindow(t *testing.T) *store.DB {
	t.Helper()
	db := testdb.Open(t)
	if err := robotconfidence.EnsurePartitionsRange(db.DB,
		testDay.AddDate(0, 0, -20), testDay.AddDate(0, 0, 1)); err != nil {
		t.Fatalf("ensure partitions: %v", err)
	}
	return db
}

// addSegment inserts a properly named scene edge, deriving both endpoint
// names from the instance so a fixture can still be addressed by one label.
// Use laneOf to build the key a query should look for.
func addSegment(t *testing.T, db *store.DB, area, instance string, fx, fy, tx, ty float64) {
	t.Helper()
	addNamedSegment(t, db, area, instance, instance+"A", instance+"B", fx, fy, tx, ty)
}

// addUnnamedSegment inserts an edge with NO endpoint names — what
// scene_edges' NOT NULL empty-string default permits, and what an older sync
// produced. Such an edge is UNKEYABLE and its samples are quarantined; see
// Segment.Keyable.
func addUnnamedSegment(t *testing.T, db *store.DB, area, instance string, fx, fy, tx, ty float64) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO scene_edges (area_name, instance_name, from_x, from_y, to_x, to_y)
		 VALUES ($1,$2,$3,$4,$5,$6)`, area, instance, fx, fy, tx, ty); err != nil {
		t.Fatalf("insert scene edge: %v", err)
	}
}

// laneOf is the lane key addSegment's edge aggregates onto — the sorted
// endpoint pair, which is what the roll-up writes and what a query must ask
// for. Going through the same rule the production code uses, rather than
// hardcoding the string, keeps the fixtures honest if that rule ever moves.
func laneOf(instance string) string {
	return robotconfidence.Segment{FromName: instance + "A", ToName: instance + "B"}.Lane()
}

// addNamedSegment inserts a scene edge WITH endpoint names — the shape a
// current sync writes, and the only shape in which the undirected lane key
// can do its job.
func addNamedSegment(t *testing.T, db *store.DB, area, instance, from, to string, fx, fy, tx, ty float64) {
	t.Helper()
	addNamedSegmentNoVersion(t, db, area, instance, from, to, fx, fy, tx, ty)
	// A synced scene writes a lane version alongside the edge, so a fixture
	// that skipped it would be testing a plant whose scene sync never ran —
	// a real state, but not the one most of these tests are about.
	ensureLaneVersion(t, db, area, laneKeyOf(from, to))
}

// addNamedSegmentNoVersion inserts the edge and NOTHING ELSE, for the tests
// that manage lane versions themselves or that mean to exercise the
// never-versioned case.
func addNamedSegmentNoVersion(t *testing.T, db *store.DB, area, instance, from, to string, fx, fy, tx, ty float64) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO scene_edges (area_name, instance_name, from_name, to_name, from_x, from_y, to_x, to_y)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, area, instance, from, to, fx, fy, tx, ty); err != nil {
		t.Fatalf("insert scene edge: %v", err)
	}
}

// laneKeyOf mirrors Segment.Lane: the sorted endpoint pair.
func laneKeyOf(from, to string) string {
	if to < from {
		from, to = to, from
	}
	return from + "-" + to
}

// ensureLaneVersion opens a lane's first version if it has none.
func ensureLaneVersion(t *testing.T, db *store.DB, area, lane string) {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM scene_lane_versions WHERE area_name=$1 AND lane=$2`,
		area, lane).Scan(&n); err != nil {
		t.Fatalf("count lane versions: %v", err)
	}
	if n > 0 {
		return
	}
	var diffID int64
	if err := db.QueryRow(
		`INSERT INTO scene_diffs (source, gate_hash, observed_at)
		 VALUES ('rds_scene','fixture',$1) RETURNING id`, testDay).Scan(&diffID); err != nil {
		t.Fatalf("insert diff: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO scene_lane_versions
		   (area_name, lane, shape_hash, def_hash, shape, directed_rows, diff_id, valid_from)
		 VALUES ($1,$2,'fx','fx','[]',2,$3,'0001-01-01 00:00:00+00')`,
		area, lane, diffID); err != nil {
		t.Fatalf("insert lane version: %v", err)
	}
}

func sample(robot string, at time.Time, conf, x, y float64, reloc int) robotconfidence.Sample {
	return robotconfidence.Sample{
		VehicleID: robot, SampledAt: at, Confidence: conf,
		X: x, Y: y, RelocStatus: reloc,
	}
}

func insert(t *testing.T, db *store.DB, samples ...robotconfidence.Sample) {
	t.Helper()
	if err := robotconfidence.InsertBatch(db.DB, samples, 0.50); err != nil {
		t.Fatalf("insert batch: %v", err)
	}
}

// ── The case the natural query silently drops ──────────────────────────────

// A segment whose EVERY sample that day was a localization failure is the
// worst place on the floor, and it has no valid reading to average. An
// aggregate grouped over valid samples cannot produce a row for a group that
// has none, so this case is invisible to a roll-up written the obvious way —
// while every other case in this file still passes.
//
// It must land as a row with NULL measures and non-zero failure counts.
// Skipping it would render the worst segment in the plant as ABSENT, which
// every reader parses as fine.
func TestRollUp_FailureOnlySegmentStillGetsARow(t *testing.T) {
	db := openWithWindow(t)
	addSegment(t, db, "area-a", "GOOD", 0, 0, 10, 0)
	addSegment(t, db, "area-a", "CURSED", 0, 50, 10, 50)

	at := testDay.Add(9 * time.Hour)
	// The cursed segment sees only FAILED relocations, from two robots, and
	// with a plausible-looking high confidence — the stale-high case.
	insert(t, db,
		sample("AMR-01", at, 0.66, 5, 50, 0),
		sample("AMR-01", at.Add(time.Minute), 0.66, 6, 50, 0),
		sample("AMR-02", at.Add(2*time.Minute), 0.71, 5, 50, 0),
		// A healthy segment alongside, so the test cannot pass by writing
		// every segment unconditionally.
		sample("AMR-01", at, 0.95, 5, 0, 1),
	)

	if _, err := db.RollUpRobotConfidence(testDay, rollUpCfg()); err != nil {
		t.Fatalf("roll-up: %v", err)
	}

	var mean, p05, minConf sql.NullFloat64
	var samples, robots, failedSamples, failedRobots int
	err := db.QueryRow(
		`SELECT mean_good, p05, min_conf, samples_good, robots, reloc_failed_samples, reloc_failed_robots
		   FROM lane_confidence_daily
		  WHERE day=$1 AND area_name='area-a' AND lane=$2`,
		testDay, laneOf("CURSED")).Scan(&mean, &p05, &minConf, &samples, &robots, &failedSamples, &failedRobots)
	if err == sql.ErrNoRows {
		t.Fatal("no row for a segment whose every sample was a localization failure — " +
			"the roll-up is aggregating over valid samples instead of unioning with failures")
	}
	if err != nil {
		t.Fatalf("read segment row: %v", err)
	}

	// NULL, not 0.0. There is no valid reading to average, and a zero here
	// would read as a catastrophic measurement rather than an absent one.
	if mean.Valid || p05.Valid || minConf.Valid {
		t.Errorf("measures must be NULL with no valid samples: mean=%v p05=%v min=%v",
			mean, p05, minConf)
	}
	if samples != 0 || robots != 0 {
		t.Errorf("valid-sample counts should be 0, got samples=%d robots=%d", samples, robots)
	}
	// Both counts: three failures by two robots is a PLACE problem, and a
	// bare count could not distinguish that from one robot failing thrice.
	if failedSamples != 3 {
		t.Errorf("reloc_failed_samples = %d, want 3", failedSamples)
	}
	if failedRobots != 2 {
		t.Errorf("reloc_failed_robots = %d, want 2", failedRobots)
	}
}

// The healthy neighbour in the same run must come out with real measures, so
// the test above cannot be satisfied by nulling everything.
func TestRollUp_ValidSegmentKeepsItsMeasures(t *testing.T) {
	db := openWithWindow(t)
	addSegment(t, db, "area-a", "GOOD", 0, 0, 10, 0)

	at := testDay.Add(9 * time.Hour)
	insert(t, db,
		sample("AMR-01", at, 0.90, 5, 0, 1),
		sample("AMR-02", at.Add(time.Minute), 0.80, 6, 0, 1),
	)
	if _, err := db.RollUpRobotConfidence(testDay, rollUpCfg()); err != nil {
		t.Fatalf("roll-up: %v", err)
	}

	var meanGood, minConf, p50 sql.NullFloat64
	var samples, samplesGood, robots, failed, sentinel int
	if err := db.QueryRow(
		`SELECT mean_good, min_conf, p50, samples, samples_good, robots,
		        reloc_failed_samples, sentinel_samples
		   FROM lane_confidence_daily
		  WHERE day=$1 AND lane=$2`, testDay, laneOf("GOOD")).
		Scan(&meanGood, &minConf, &p50, &samples, &samplesGood, &robots, &failed, &sentinel); err != nil {
		t.Fatalf("read lane row: %v", err)
	}
	if !meanGood.Valid || meanGood.Float64 < 0.849 || meanGood.Float64 > 0.851 {
		t.Errorf("mean_good = %v, want ~0.85", meanGood)
	}
	if !minConf.Valid || minConf.Float64 != 0.80 {
		t.Errorf("min_conf = %v, want 0.80", minConf)
	}
	// With no misses the two populations coincide, so p50 is a reading some
	// robot actually reported. NEAREST RANK, not interpolation: over
	// {0.80, 0.90} that is 0.80, the lower of the two, and NOT the 0.85 the
	// Median helper would return. The two are different functions on purpose
	// — a floor-quality figure is worth more as an observed value than as a
	// smoothed one — and anything reading p50 as "the median" needs to know
	// that on an even count they disagree.
	if !p50.Valid || p50.Float64 != 0.80 {
		t.Errorf("p50 = %v, want 0.80 (nearest rank over two values, not their mean)", p50)
	}
	if samples != 2 || samplesGood != 2 || robots != 2 || failed != 0 || sentinel != 0 {
		t.Errorf("samples=%d good=%d robots=%d failed=%d sentinel=%d, want 2/2/2/0/0",
			samples, samplesGood, robots, failed, sentinel)
	}
}

// The two populations must come apart, and this is the case where getting it
// wrong inverts the answer.
//
// A lane that returns a strong reading half the time and NOTHING the rest is
// the shape of every reflector-less zone at Springfield. Measured there,
// lanes running through such a zone average 0.897 against 0.740 elsewhere —
// they read BETTER than the plant, because the failures leave as no-estimates
// and what survives is truncated rather than degraded. Banding that
// conditioned mean scored AUC 0.081, i.e. almost perfectly backwards.
//
// So: mean_good must stay high (it is the truth about the readings that
// happened) and p50 over the full population must collapse (it is the truth
// about the lane). Both, in the same row, is the only way the map can be
// honest and the panel can still explain it.
func TestRollUp_NoEstimateCountsAsZeroInTheBandedStatistic(t *testing.T) {
	db := openWithWindow(t)
	addSegment(t, db, "area-a", "HALFBLIND", 0, 0, 10, 0)

	at := testDay.Add(9 * time.Hour)
	// The vendor publishes literal NEGATIVE zero for "no estimate here", and
	// a Go source literal cannot express that — `-0.0` is constant-folded to
	// plain 0.0, which staticcheck rightly flags. Copysign produces the real
	// wire value, so the fixture is what SEER actually sends rather than
	// something that merely satisfies the same comparison.
	noEstimate := math.Copysign(0, -1)
	var rows []robotconfidence.Sample
	// Four strong readings and four no-estimates, on one lane, two robots.
	for i := 0; i < 4; i++ {
		rows = append(rows,
			sample("AMR-01", at.Add(time.Duration(i)*time.Minute), 0.98, float64(i), 0, 1),
			sample("AMR-02", at.Add(time.Duration(i)*time.Minute+30*time.Second), noEstimate, float64(i)+0.5, 0, 1))
	}
	insert(t, db, rows...)
	if _, err := db.RollUpRobotConfidence(testDay, rollUpCfg()); err != nil {
		t.Fatalf("roll-up: %v", err)
	}

	var meanGood, p50, p95 sql.NullFloat64
	var samples, samplesGood, sentinel, sentinelRobots int
	if err := db.QueryRow(
		`SELECT mean_good, p50, p95, samples, samples_good, sentinel_samples, sentinel_robots
		   FROM lane_confidence_daily WHERE day=$1 AND lane=$2`, testDay, laneOf("HALFBLIND")).
		Scan(&meanGood, &p50, &p95, &samples, &samplesGood, &sentinel, &sentinelRobots); err != nil {
		t.Fatalf("read lane row: %v", err)
	}

	if samples != 8 || samplesGood != 4 || sentinel != 4 || sentinelRobots != 1 {
		t.Errorf("samples=%d good=%d sentinel=%d sentinelRobots=%d, want 8/4/4/1",
			samples, samplesGood, sentinel, sentinelRobots)
	}
	// The conditioned figure is excellent, and that is CORRECT — it is a true
	// statement about the readings that were produced.
	if !meanGood.Valid || meanGood.Float64 < 0.979 || meanGood.Float64 > 0.981 {
		t.Errorf("mean_good = %v, want ~0.98 — the surviving readings really are that good", meanGood)
	}
	// The banded figure collapses, and that is also correct: half the ticks
	// on this lane produced nothing.
	if !p50.Valid || p50.Float64 != 0 {
		t.Errorf("p50 = %v, want 0 — half the ticks produced no estimate, and a "+
			"median over the full population has to show that", p50)
	}
	// The upper tail still sees the good readings, so the row is not simply
	// zeroed out; the distribution genuinely spans both.
	if !p95.Valid || p95.Float64 < 0.979 {
		t.Errorf("p95 = %v, want ~0.98", p95)
	}
	// The trap, stated: if the map banded mean_good it would paint this lane
	// green. That is the AUC 0.081 result, in one row.
	if meanGood.Float64 <= p50.Float64 {
		t.Fatal("fixture is wrong: the conditioned mean must exceed the banded median here")
	}
}

// A robot on a different map than the fleet is quarantined, counted, and
// never averaged in.
//
// Hopkinsville 2026-08-06: eleven robots on Hop_20 and one connected robot on
// Hop_21, which RDS was simultaneously reporting as current_map_invalid. Its
// readings were real readings of real places — snapped against a scene built
// from a different map.
func TestRollUp_SamplesFromAnotherMapAreQuarantined(t *testing.T) {
	db := openWithWindow(t)
	addSegment(t, db, "area-a", "SHARED", 0, 0, 10, 0)

	at := testDay.Add(9 * time.Hour)
	withMap := func(s robotconfidence.Sample, md5 string) robotconfidence.Sample {
		s.MapMD5 = md5
		return s
	}
	var rows []robotconfidence.Sample
	// Majority: three robots, six readings, on the fleet map.
	for i := 0; i < 6; i++ {
		rows = append(rows, withMap(
			sample(fmt.Sprintf("AMR-0%d", i%3+1), at.Add(time.Duration(i)*time.Minute),
				0.90, float64(i), 0, 1), "fleet-map"))
	}
	// The odd robot out: two readings, wildly different value, other map.
	rows = append(rows,
		withMap(sample("AMR-99", at.Add(10*time.Minute), 0.10, 4, 0, 1), "other-map"),
		withMap(sample("AMR-99", at.Add(11*time.Minute), 0.10, 5, 0, 1), "other-map"))
	insert(t, db, rows...)

	res, err := db.RollUpRobotConfidence(testDay, rollUpCfg())
	if err != nil {
		t.Fatalf("roll-up: %v", err)
	}
	if res.MapMismatch != 2 {
		t.Errorf("result.MapMismatch = %d, want 2 — the count has to reach the log line", res.MapMismatch)
	}

	var meanGood sql.NullFloat64
	var samples, mismatch, robots int
	if err := db.QueryRow(
		`SELECT mean_good, samples, map_mismatch_samples, robots
		   FROM lane_confidence_daily WHERE day=$1 AND lane=$2`, testDay, laneOf("SHARED")).
		Scan(&meanGood, &samples, &mismatch, &robots); err != nil {
		t.Fatalf("read lane row: %v", err)
	}
	if samples != 6 || mismatch != 2 {
		t.Errorf("samples=%d mismatch=%d, want 6/2", samples, mismatch)
	}
	if robots != 3 {
		t.Errorf("robots = %d, want 3 — the quarantined robot must not be counted as having driven the lane", robots)
	}
	// 0.90 throughout, not dragged toward 0.10.
	if !meanGood.Valid || meanGood.Float64 < 0.899 || meanGood.Float64 > 0.901 {
		t.Errorf("mean_good = %v, want 0.90 — the other-map readings must not enter the average", meanGood)
	}

	// And the robot row names who was out of step, because that is a
	// maintenance signal rather than a data-quality footnote.
	var robotMismatch int
	if err := db.QueryRow(
		`SELECT map_mismatch_samples FROM robot_confidence_daily
		  WHERE day=$1 AND vehicle_id='AMR-99'`, testDay).Scan(&robotMismatch); err != nil {
		t.Fatalf("read robot row: %v", err)
	}
	if robotMismatch != 2 {
		t.Errorf("AMR-99 map_mismatch_samples = %d, want 2", robotMismatch)
	}
}

// Rows with no map hash predate v79 and must never be quarantined.
//
// "Not collected" is not "wrong map". Treating it as one would quarantine
// every historical row the first time the job ran after the migration —
// silently, and in the direction that looks like the plant went quiet.
func TestRollUp_MissingMapHashIsNotAMismatch(t *testing.T) {
	db := openWithWindow(t)
	addSegment(t, db, "area-a", "LEGACY", 0, 0, 10, 0)

	at := testDay.Add(9 * time.Hour)
	insert(t, db,
		sample("AMR-01", at, 0.90, 1, 0, 1),                  // no MapMD5 set
		sample("AMR-02", at.Add(time.Minute), 0.92, 2, 0, 1), // no MapMD5 set
	)
	res, err := db.RollUpRobotConfidence(testDay, rollUpCfg())
	if err != nil {
		t.Fatalf("roll-up: %v", err)
	}
	if res.MapMismatch != 0 {
		t.Errorf("MapMismatch = %d, want 0 — an empty hash is unknown, not wrong", res.MapMismatch)
	}
	var samples int
	if err := db.QueryRow(
		`SELECT samples FROM lane_confidence_daily WHERE day=$1 AND lane=$2`,
		testDay, laneOf("LEGACY")).Scan(&samples); err != nil {
		t.Fatalf("read lane row: %v", err)
	}
	if samples != 2 {
		t.Errorf("samples = %d, want 2", samples)
	}
}

// Reciprocal twins aggregate as ONE lane.
//
// This is the correctness bug the lane key exists for: scene_edges stores
// every two-way lane twice with identical geometry, so a sample sits exactly
// as close to one row as to the other and the winner was float noise.
// LM73-LM14 showed 48 readings and LM14-LM73 showed 116 — one piece of floor,
// reported as two, each with half the evidence.
func TestRollUp_ReciprocalTwinsAggregateAsOneLane(t *testing.T) {
	db := openWithWindow(t)
	addNamedSegment(t, db, "area-a", "LM73-LM14", "LM73", "LM14", 0, 0, 10, 0)
	addNamedSegment(t, db, "area-a", "LM14-LM73", "LM14", "LM73", 10, 0, 0, 0)

	at := testDay.Add(9 * time.Hour)
	var rows []robotconfidence.Sample
	for i := 0; i < 6; i++ {
		rows = append(rows, sample("AMR-01", at.Add(time.Duration(i)*time.Minute),
			0.90, float64(i), 0, 1))
	}
	insert(t, db, rows...)
	if _, err := db.RollUpRobotConfidence(testDay, rollUpCfg()); err != nil {
		t.Fatalf("roll-up: %v", err)
	}

	var n, samples int
	if err := db.QueryRow(
		`SELECT count(*), coalesce(sum(samples),0) FROM lane_confidence_daily WHERE day=$1`,
		testDay).Scan(&n, &samples); err != nil {
		t.Fatalf("count lanes: %v", err)
	}
	if n != 1 {
		t.Errorf("two directed rows for one lane produced %d aggregate rows, want 1 — "+
			"the samples are being split between twins", n)
	}
	if samples != 6 {
		t.Errorf("total samples = %d, want 6 — no reading may be lost or double-counted", samples)
	}
	var lane string
	db.QueryRow(`SELECT lane FROM lane_confidence_daily WHERE day=$1`, testDay).Scan(&lane)
	if lane != "LM14-LM73" {
		t.Errorf("lane = %q, want the sorted pair LM14-LM73", lane)
	}
}

// A segment nobody drove must not get a row, or the job writes one row per
// scene segment per day forever.
func TestRollUp_UntouchedSegmentGetsNoRow(t *testing.T) {
	db := openWithWindow(t)
	addSegment(t, db, "area-a", "DRIVEN", 0, 0, 10, 0)
	addSegment(t, db, "area-a", "UNTOUCHED", 0, 900, 10, 900)

	insert(t, db, sample("AMR-01", testDay.Add(9*time.Hour), 0.95, 5, 0, 1))
	if _, err := db.RollUpRobotConfidence(testDay, rollUpCfg()); err != nil {
		t.Fatalf("roll-up: %v", err)
	}

	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM lane_confidence_daily WHERE lane=$1`, laneOf("UNTOUCHED")).
		Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("a segment with no samples got %d row(s)", n)
	}
}

// ── Statistics take reloc_status = 1 only ──────────────────────────────────

// A robot sitting in FAILED on a segment must not drag that segment's mean
// down for every healthy robot that passes through — that is the confound the
// whole design exists to remove, reintroduced at the aggregate.
func TestRollUp_FailedSamplesDoNotEnterTheMean(t *testing.T) {
	db := openWithWindow(t)
	addSegment(t, db, "area-a", "MIXED", 0, 0, 10, 0)

	at := testDay.Add(9 * time.Hour)
	insert(t, db,
		sample("AMR-01", at, 0.90, 5, 0, 1),
		sample("AMR-02", at.Add(time.Minute), 0.90, 5, 0, 1),
		// FAILED, with a terrible value. Counted, never averaged.
		sample("AMR-03", at.Add(2*time.Minute), 0.05, 5, 0, 0),
		// COMPLETED — settled but unconfirmed, also held out of statistics.
		sample("AMR-04", at.Add(3*time.Minute), 0.10, 5, 0, 3),
	)
	if _, err := db.RollUpRobotConfidence(testDay, rollUpCfg()); err != nil {
		t.Fatalf("roll-up: %v", err)
	}

	var mean sql.NullFloat64
	var samples, failed int
	if err := db.QueryRow(
		`SELECT mean_good, samples_good, reloc_failed_samples FROM lane_confidence_daily
		  WHERE day=$1 AND lane=$2`, testDay, laneOf("MIXED")).
		Scan(&mean, &samples, &failed); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !mean.Valid || mean.Float64 != 0.90 {
		t.Errorf("mean = %v, want exactly 0.90 (only SUCCESS samples)", mean)
	}
	if samples != 2 {
		t.Errorf("samples = %d, want 2", samples)
	}
	if failed != 1 {
		t.Errorf("reloc_failed_samples = %d, want 1", failed)
	}
}

// ── Idempotency ────────────────────────────────────────────────────────────

// The job must be safe to re-run for a past day. The first time the snap
// tolerance or the coverage thresholds change, every affected day wants
// recomputing, and a job that could only append would make that a manual
// repair.
func TestRollUp_IsIdempotent(t *testing.T) {
	db := openWithWindow(t)
	addSegment(t, db, "area-a", "SEG", 0, 0, 10, 0)

	at := testDay.Add(9 * time.Hour)
	insert(t, db,
		sample("AMR-01", at, 0.90, 5, 0, 1),
		sample("AMR-02", at.Add(time.Minute), 0.80, 6, 0, 1),
	)
	for i := 0; i < 2; i++ {
		if _, err := db.RollUpRobotConfidence(testDay, rollUpCfg()); err != nil {
			t.Fatalf("roll-up %d: %v", i, err)
		}
	}

	var segRows, robotRows int
	if err := db.QueryRow(
		`SELECT count(*) FROM lane_confidence_daily WHERE day=$1`, testDay).Scan(&segRows); err != nil {
		t.Fatalf("count segments: %v", err)
	}
	if err := db.QueryRow(
		`SELECT count(*) FROM robot_confidence_daily WHERE day=$1`, testDay).Scan(&robotRows); err != nil {
		t.Fatalf("count robots: %v", err)
	}
	if segRows != 1 {
		t.Errorf("segment rows after two runs = %d, want 1", segRows)
	}
	if robotRows != 2 {
		t.Errorf("robot rows after two runs = %d, want 2", robotRows)
	}
}

// A robot with too little overlap must store NULL, not 0.0. The two are
// opposite findings and the difference has to survive the round trip.
func TestRollUp_ThinCoverageStoresNullResidual(t *testing.T) {
	db := openWithWindow(t)
	addSegment(t, db, "area-a", "SEG", 0, 0, 10, 0)

	insert(t, db, sample("AMR-01", testDay.Add(9*time.Hour), 0.90, 5, 0, 1))
	if _, err := db.RollUpRobotConfidence(testDay, rollUpCfg()); err != nil {
		t.Fatalf("roll-up: %v", err)
	}

	var residual sql.NullFloat64
	var cells, samples int
	if err := db.QueryRow(
		`SELECT residual, cells, samples FROM robot_confidence_daily
		  WHERE day=$1 AND vehicle_id='AMR-01'`, testDay).
		Scan(&residual, &cells, &samples); err != nil {
		t.Fatalf("read robot row: %v", err)
	}
	if residual.Valid {
		t.Errorf("residual = %v, want NULL — one cell is far below MinCells", residual)
	}
	if samples != 1 {
		t.Errorf("samples = %d, want 1 (the raw figure is still recorded)", samples)
	}
}

// ── Partition lifecycle ────────────────────────────────────────────────────

func partitionExists(t *testing.T, db *store.DB, name string) bool {
	t.Helper()
	var exists bool
	if err := db.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM pg_class WHERE relname = $1)`, name).Scan(&exists); err != nil {
		t.Fatalf("check partition %s: %v", name, err)
	}
	return exists
}

func TestPartitions_CreateInsertAndDrop(t *testing.T) {
	db := testdb.Open(t)

	// EnsurePartitions creates today and tomorrow on BOTH sample tables.
	if err := db.EnsureRobotConfidencePartitions(testDay); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	for _, name := range []string{
		"robot_confidence_samples_2026_08_05",
		"robot_confidence_samples_2026_08_06",
		"robot_confidence_low_2026_08_05",
		"robot_confidence_low_2026_08_06",
	} {
		if !partitionExists(t, db, name) {
			t.Errorf("expected partition %s", name)
		}
	}

	// A low-confidence sample double-writes: raw AND the forensic trail.
	insert(t, db, sample("AMR-01", testDay.Add(time.Hour), 0.30, 1, 1, 1))
	var raw, low int
	db.QueryRow(`SELECT count(*) FROM robot_confidence_samples`).Scan(&raw)
	db.QueryRow(`SELECT count(*) FROM robot_confidence_low`).Scan(&low)
	if raw != 1 || low != 1 {
		t.Errorf("double-write: raw=%d low=%d, want 1/1", raw, low)
	}

	// A healthy sample goes to raw only.
	insert(t, db, sample("AMR-01", testDay.Add(2*time.Hour), 0.95, 1, 1, 1))
	db.QueryRow(`SELECT count(*) FROM robot_confidence_samples`).Scan(&raw)
	db.QueryRow(`SELECT count(*) FROM robot_confidence_low`).Scan(&low)
	if raw != 2 || low != 1 {
		t.Errorf("healthy sample: raw=%d low=%d, want 2/1", raw, low)
	}
}

// A drop must take only the days it was aimed at. Retention is the one dial
// in this design that can be turned later, and a drop that overshot by a day
// would quietly destroy data the operator meant to keep.
func TestPartitions_DropTakesOnlyAgedDays(t *testing.T) {
	db := testdb.Open(t)
	if err := robotconfidence.EnsurePartitionsRange(db.DB,
		testDay.AddDate(0, 0, -5), testDay); err != nil {
		t.Fatalf("ensure range: %v", err)
	}

	// Keep 3 days as of testDay: the cutoff falls at 08-02, so 07-31 and
	// 08-01 go and 08-02 onward stay.
	n, err := robotconfidence.DropOldPartitions(db.DB,
		robotconfidence.TableSamples, 3, testDay)
	if err != nil {
		t.Fatalf("drop: %v", err)
	}
	if n != 2 {
		t.Errorf("dropped %d partitions, want 2", n)
	}
	if partitionExists(t, db, "robot_confidence_samples_2026_07_31") {
		t.Error("07-31 should have been dropped")
	}
	if !partitionExists(t, db, "robot_confidence_samples_2026_08_02") {
		t.Error("08-02 is inside retention and must survive")
	}
	if !partitionExists(t, db, "robot_confidence_samples_2026_08_05") {
		t.Error("the current day must never be dropped")
	}
	// The low trail has its own, much longer retention and must be untouched
	// by a raw-table drop.
	if !partitionExists(t, db, "robot_confidence_low_2026_07_31") {
		t.Error("dropping raw partitions must not touch the low-confidence trail")
	}
}

// reloc_status carries no DEFAULT on purpose: there is no quiet value for the
// enum, and a row written without it must fail rather than silently claim a
// healthy pose.
func TestSamples_RelocStatusHasNoDefault(t *testing.T) {
	db := testdb.Open(t)
	if err := robotconfidence.EnsurePartitions(db.DB, testDay); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	_, err := db.Exec(
		`INSERT INTO robot_confidence_samples (vehicle_id, sampled_at, confidence, x, y, angle)
		 VALUES ('AMR-01', $1, 0.95, 0, 0, 0)`, testDay.Add(time.Hour))
	if err == nil {
		t.Fatal("an insert omitting reloc_status must fail, not default to SUCCESS")
	}
}

// ── v78: the area membership that explains a -0.0 reading ──────────────────

// area_ids round-trips as a real TEXT[], including the multi-element case.
//
// THIS TEST IS THE VERIFICATION, NOT A FORMALITY. shingo-core talks to
// Postgres through pgx's database/sql shim and has no lib/pq, so there is no
// pq.Array to lean on — two other call sites in this repo worked around its
// absence with explicit placeholders rather than binding a slice. Whether a
// bare []string binds to TEXT[] through that path is a property of the
// driver, not something the type checker can answer, so it is asserted here
// against a real server. If this fails, the column type is the thing to
// change, not the assertion.
func TestSamples_AreaIDsRoundTrip(t *testing.T) {
	db := testdb.Open(t)
	if err := robotconfidence.EnsurePartitions(db.DB, testDay); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	withAreas := func(s robotconfidence.Sample, areas []string) robotconfidence.Sample {
		s.AreaIDs = areas
		return s
	}
	// Confidence 0.30 on the area-8 row so it also lands in the low trail —
	// the sentinel is exactly the case the forensic table exists to hold.
	insert(t, db,
		withAreas(sample("AMR-01", testDay.Add(time.Hour), 0.95, 1, 1, 1), []string{}),
		withAreas(sample("AMR-02", testDay.Add(2*time.Hour), 0.30, 2, 2, 1), []string{"8"}),
		withAreas(sample("AMR-03", testDay.Add(3*time.Hour), 0.90, 3, 3, 1), []string{"8", "12"}),
	)

	for _, tc := range []struct {
		robot string
		want  string
	}{
		{"AMR-01", "{}"},
		{"AMR-02", "{8}"},
		{"AMR-03", "{8,12}"},
	} {
		var got string
		if err := db.QueryRow(
			`SELECT area_ids::text FROM robot_confidence_samples WHERE vehicle_id = $1`,
			tc.robot).Scan(&got); err != nil {
			t.Fatalf("%s: scan: %v", tc.robot, err)
		}
		if got != tc.want {
			t.Errorf("%s: area_ids = %s, want %s", tc.robot, got, tc.want)
		}
	}

	// The double-write must carry it too, or the forensic trail loses the one
	// field that says whether a low reading was a zone or a lost robot.
	var lowAreas string
	if err := db.QueryRow(
		`SELECT area_ids::text FROM robot_confidence_low WHERE vehicle_id = 'AMR-02'`).
		Scan(&lowAreas); err != nil {
		t.Fatalf("low trail scan: %v", err)
	}
	if lowAreas != "{8}" {
		t.Errorf("low trail area_ids = %s, want {8}", lowAreas)
	}
}

// A nil slice must store as '{}' ("in no area"), never as NULL.
//
// After v78 there is exactly one writer and it always knows the answer, so
// NULL in this column can only ever mean "this row predates the migration".
// Letting a nil Go slice through as NULL would blur those two together and
// destroy the only marker of the pre-v78 window.
func TestSamples_NilAreaIDsStoresEmptyNotNull(t *testing.T) {
	db := testdb.Open(t)
	if err := robotconfidence.EnsurePartitions(db.DB, testDay); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	insert(t, db, sample("AMR-01", testDay.Add(time.Hour), 0.95, 1, 1, 1)) // AreaIDs left nil

	var isNull bool
	var areas string
	if err := db.QueryRow(
		`SELECT area_ids IS NULL, coalesce(area_ids::text, '<null>')
		   FROM robot_confidence_samples WHERE vehicle_id = 'AMR-01'`).
		Scan(&isNull, &areas); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if isNull {
		t.Error("a nil AreaIDs must store as '{}', not NULL — NULL is reserved for pre-v78 rows")
	}
	if areas != "{}" {
		t.Errorf("area_ids = %s, want {}", areas)
	}
}

// ── v79: the map the reading was taken on, and the alarms standing at the time ──

// map_md5 and alarm_codes round-trip, and alarm_codes binds as a real
// INTEGER[].
//
// SAME REASONING AS THE AREA_IDS TEST, DIFFERENT GO TYPE. Whether a Go slice
// binds to a Postgres array through pgx's database/sql shim is a driver
// property; []string was verified for TEXT[] at v78 and that result says
// nothing about []int32 → INTEGER[]. `int` is deliberately not used: it is
// platform-width and has no fixed Postgres partner.
//
// The alarm codes here are the real ones. 54018 is "reflectors in map not
// enough" and has been standing on every Springfield robot since the week of
// 2026-06-08; 54020 is "reflectors match failed". A row that carries a
// no-estimate reading AND the alarm explaining it is the join that did not
// exist, and it is the whole reason for the column.
func TestSamples_MapMD5AndAlarmCodesRoundTrip(t *testing.T) {
	db := testdb.Open(t)
	if err := robotconfidence.EnsurePartitions(db.DB, testDay); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	with := func(s robotconfidence.Sample, md5 string, codes []int32) robotconfidence.Sample {
		s.MapMD5 = md5
		s.AlarmCodes = codes
		return s
	}
	// AMR-02 is the interesting row: a sentinel reading, on the majority
	// map, with the alarm that explains it standing at the same instant.
	insert(t, db,
		with(sample("AMR-01", testDay.Add(time.Hour), 0.95, 1, 1, 1), "a54877f0", []int32{}),
		with(sample("AMR-02", testDay.Add(2*time.Hour), 0.30, 2, 2, 1), "a54877f0", []int32{54018}),
		with(sample("AMR-03", testDay.Add(3*time.Hour), 0.90, 3, 3, 1), "e8bd9f08", []int32{54018, 54020}),
	)

	for _, tc := range []struct {
		robot, md5, codes string
	}{
		{"AMR-01", "a54877f0", "{}"},
		{"AMR-02", "a54877f0", "{54018}"},
		{"AMR-03", "e8bd9f08", "{54018,54020}"},
	} {
		var md5, codes string
		if err := db.QueryRow(
			`SELECT map_md5, alarm_codes::text FROM robot_confidence_samples WHERE vehicle_id = $1`,
			tc.robot).Scan(&md5, &codes); err != nil {
			t.Fatalf("%s: scan: %v", tc.robot, err)
		}
		if md5 != tc.md5 {
			t.Errorf("%s: map_md5 = %q, want %q", tc.robot, md5, tc.md5)
		}
		if codes != tc.codes {
			t.Errorf("%s: alarm_codes = %s, want %s", tc.robot, codes, tc.codes)
		}
	}

	// The low trail carries both too. A forensic row that has lost the map
	// it was taken on cannot be quarantined later, and the trail outlives
	// the raw samples by 76 days.
	var lowMD5, lowCodes string
	if err := db.QueryRow(
		`SELECT map_md5, alarm_codes::text FROM robot_confidence_low WHERE vehicle_id = 'AMR-02'`).
		Scan(&lowMD5, &lowCodes); err != nil {
		t.Fatalf("low trail scan: %v", err)
	}
	if lowMD5 != "a54877f0" || lowCodes != "{54018}" {
		t.Errorf("low trail = (%q, %s), want (\"a54877f0\", {54018})", lowMD5, lowCodes)
	}
}

// A nil AlarmCodes stores as '{}' ("looked, none active"), never NULL.
//
// Same rule as area_ids, and it matters more here because "no alarms" is the
// normal case: if nil leaked through as NULL, the overwhelming majority of
// rows would claim the alarms were never collected and the pre-v79 marker
// would be worthless.
func TestSamples_NilAlarmCodesStoresEmptyNotNull(t *testing.T) {
	db := testdb.Open(t)
	if err := robotconfidence.EnsurePartitions(db.DB, testDay); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	insert(t, db, sample("AMR-01", testDay.Add(time.Hour), 0.95, 1, 1, 1)) // AlarmCodes left nil

	var isNull bool
	var codes string
	if err := db.QueryRow(
		`SELECT alarm_codes IS NULL, coalesce(alarm_codes::text, '<null>')
		   FROM robot_confidence_samples WHERE vehicle_id = 'AMR-01'`).
		Scan(&isNull, &codes); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if isNull {
		t.Error("a nil AlarmCodes must store as '{}', not NULL — NULL is reserved for pre-v79 rows")
	}
	if codes != "{}" {
		t.Errorf("alarm_codes = %s, want {}", codes)
	}
}

// A reading on an edge with no endpoint names is counted, not guessed at.
//
// The edge stays in the index deliberately: dropping it would send its
// samples onto whichever neighbour is within tolerance, and at Springfield
// 23.6% of samples have a rival lane within 5 cm, so that is a near-certain
// silent misattribution. Instead the reading lands here, is excluded from
// every statistic, and both counts reach the job's log line — the same
// treatment a foreign-map sample gets, for the same reason.
func TestRollUp_UnkeyableEdgeIsQuarantinedNotGuessed(t *testing.T) {
	db := openWithWindow(t)
	// One properly named lane, and one the old sync left unnamed, far apart
	// so neither can steal the other's samples.
	addNamedSegment(t, db, "area-a", "LM1-LM2", "LM1", "LM2", 0, 0, 10, 0)
	addUnnamedSegment(t, db, "area-a", "NAMELESS", 0, 60, 10, 60)

	at := testDay.Add(9 * time.Hour)
	insert(t, db,
		sample("AMR-01", at, 0.90, 5, 0, 1),
		sample("AMR-01", at.Add(time.Minute), 0.20, 5, 60, 1),
		sample("AMR-02", at.Add(2*time.Minute), 0.20, 6, 60, 1),
	)

	res, err := db.RollUpRobotConfidence(testDay, rollUpCfg())
	if err != nil {
		t.Fatalf("roll-up: %v", err)
	}
	if res.UnkeyableEdges != 1 {
		t.Errorf("UnkeyableEdges = %d, want 1", res.UnkeyableEdges)
	}
	if res.UnkeyableSamples != 2 {
		t.Errorf("UnkeyableSamples = %d, want 2 — the readings have to be counted, "+
			"not absorbed", res.UnkeyableSamples)
	}
	// Both numbers must reach the log line, or the defect is invisible to
	// anyone not reading the struct.
	if !strings.Contains(res.String(), "unkeyable_edges=1") ||
		!strings.Contains(res.String(), "unkeyable_samples=2") {
		t.Errorf("log line omits the counts: %s", res.String())
	}

	// Exactly one lane row, and it is the named one at its own value. The
	// 0.20 readings must not have leaked into it.
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM lane_confidence_daily WHERE day=$1`, testDay).Scan(&n); err != nil {
		t.Fatalf("count lanes: %v", err)
	}
	if n != 1 {
		t.Errorf("got %d lane rows, want 1 — an unkeyable edge must not produce one", n)
	}
	var lane string
	var meanGood sql.NullFloat64
	if err := db.QueryRow(
		`SELECT lane, mean_good FROM lane_confidence_daily WHERE day=$1`, testDay).
		Scan(&lane, &meanGood); err != nil {
		t.Fatalf("read lane row: %v", err)
	}
	if lane != "LM1-LM2" {
		t.Errorf("lane = %q, want LM1-LM2", lane)
	}
	if !meanGood.Valid || meanGood.Float64 < 0.899 || meanGood.Float64 > 0.901 {
		t.Errorf("mean_good = %v, want 0.90 — the unkeyable readings must not enter it", meanGood)
	}
}
