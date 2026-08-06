//go:build docker

package robotconfidence_test

import (
	"database/sql"
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

func addSegment(t *testing.T, db *store.DB, area, instance string, fx, fy, tx, ty float64) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO scene_edges (area_name, instance_name, from_x, from_y, to_x, to_y)
		 VALUES ($1,$2,$3,$4,$5,$6)`, area, instance, fx, fy, tx, ty); err != nil {
		t.Fatalf("insert scene edge: %v", err)
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
		`SELECT mean, p05, min_conf, samples, robots, reloc_failed_samples, reloc_failed_robots
		   FROM segment_confidence_daily
		  WHERE day=$1 AND area_name='area-a' AND edge_instance='CURSED'`,
		testDay).Scan(&mean, &p05, &minConf, &samples, &robots, &failedSamples, &failedRobots)
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

	var mean, minConf sql.NullFloat64
	var samples, robots, failed int
	if err := db.QueryRow(
		`SELECT mean, min_conf, samples, robots, reloc_failed_samples
		   FROM segment_confidence_daily
		  WHERE day=$1 AND edge_instance='GOOD'`, testDay).
		Scan(&mean, &minConf, &samples, &robots, &failed); err != nil {
		t.Fatalf("read segment row: %v", err)
	}
	if !mean.Valid || mean.Float64 < 0.849 || mean.Float64 > 0.851 {
		t.Errorf("mean = %v, want ~0.85", mean)
	}
	if !minConf.Valid || minConf.Float64 != 0.80 {
		t.Errorf("min_conf = %v, want 0.80", minConf)
	}
	if samples != 2 || robots != 2 || failed != 0 {
		t.Errorf("samples=%d robots=%d failed=%d, want 2/2/0", samples, robots, failed)
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
		`SELECT count(*) FROM segment_confidence_daily WHERE edge_instance='UNTOUCHED'`).
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
		`SELECT mean, samples, reloc_failed_samples FROM segment_confidence_daily
		  WHERE day=$1 AND edge_instance='MIXED'`, testDay).
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
		`SELECT count(*) FROM segment_confidence_daily WHERE day=$1`, testDay).Scan(&segRows); err != nil {
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
