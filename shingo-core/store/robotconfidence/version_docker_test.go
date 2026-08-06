//go:build docker

package robotconfidence_test

import (
	"testing"
	"time"

	"shingocore/store"
)

// newSceneDiff writes a diff row for a fixture version to hang off.
func newSceneDiff(t *testing.T, db *store.DB, observed time.Time) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(
		`INSERT INTO scene_diffs (source, gate_hash, observed_at)
		 VALUES ('rds_scene','test',$1) RETURNING id`, observed).Scan(&id); err != nil {
		t.Fatalf("insert diff: %v", err)
	}
	return id
}

// openFirstLaneVersion writes a lane's FIRST version, which opens at
// an early bound — the same thing ApplyLaneDiff does, so the fixture and production
// agree about what a first version means. validTo nil leaves it in force.
func openFirstLaneVersion(t *testing.T, db *store.DB, area, lane string,
	observed time.Time, validTo *time.Time) int64 {
	t.Helper()
	diffID := newSceneDiff(t, db, observed)
	var id int64
	if err := db.QueryRow(
		`INSERT INTO scene_lane_versions
		   (area_name, lane, shape_hash, def_hash, shape, directed_rows,
		    diff_id, valid_from, valid_to)
		 VALUES ($1,$2,'sh','df','[]',2,$3,'0001-01-01 00:00:00+00',$4) RETURNING id`,
		area, lane, diffID, validTo).Scan(&id); err != nil {
		t.Fatalf("insert first lane version: %v", err)
	}
	return id
}

// openLaneVersion writes a SUBSEQUENT version, which opens when it was seen —
// for those we do know when the change happened.
func openLaneVersion(t *testing.T, db *store.DB, area, lane string,
	from time.Time, to *time.Time) int64 {
	t.Helper()
	diffID := newSceneDiff(t, db, from)
	var id int64
	if err := db.QueryRow(
		`INSERT INTO scene_lane_versions
		   (area_name, lane, shape_hash, def_hash, shape, directed_rows,
		    diff_id, valid_from, valid_to)
		 VALUES ($1,$2,'sh2','df2','[]',2,$3,$4,$5) RETURNING id`,
		area, lane, diffID, from, to).Scan(&id); err != nil {
		t.Fatalf("insert lane version: %v", err)
	}
	return id
}

// A lane edited part-way through a day produces TWO rows, one per geometry.
//
// This is the whole reason the version is in the key. Maps here are edited
// close to daily, so an edit at 14:00 leaves that lane with hours of one
// geometry and hours of another; a single day row averages across the change
// and presents the blend as a measurement. No reader can detect that from the
// row, and it happens on the day the reader most wants to look.
func TestRollUp_MidDayEditSplitsTheDay(t *testing.T) {
	db := openWithWindow(t)
	addNamedSegmentNoVersion(t, db, "area-a", "LM1-LM2", "LM1", "LM2", 0, 0, 10, 0)

	noon := testDay.Add(12 * time.Hour)
	oldV := openFirstLaneVersion(t, db, "area-a", "LM1-LM2", testDay, &noon)
	newV := openLaneVersion(t, db, "area-a", "LM1-LM2", noon, nil)

	// Morning readings are poor; afternoon readings, after the edit, are good.
	// A single blended row would report something in between and look like a
	// mediocre lane rather than a lane that was fixed.
	insert(t, db,
		sample("AMR-01", testDay.Add(9*time.Hour), 0.40, 1, 0, 1),
		sample("AMR-01", testDay.Add(10*time.Hour), 0.42, 2, 0, 1),
		sample("AMR-02", testDay.Add(15*time.Hour), 0.95, 3, 0, 1),
		sample("AMR-02", testDay.Add(16*time.Hour), 0.97, 4, 0, 1),
	)

	cfg := rollUpCfg()
	cfg.Versions = store.LaneVersionResolver{}
	if _, err := db.RollUpRobotConfidence(testDay, cfg); err != nil {
		t.Fatalf("roll-up: %v", err)
	}

	rows, err := db.Query(
		`SELECT version_id, samples, mean_good FROM lane_confidence_daily
		  WHERE day=$1 AND lane=$2 ORDER BY version_id`, testDay, laneOf2("LM1-LM2"))
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	type row struct {
		samples int
		mean    float64
	}
	got := map[int64]row{}
	for rows.Next() {
		var v int64
		var r row
		if err := rows.Scan(&v, &r.samples, &r.mean); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[v] = r
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows for one lane on one day, want 2 — the edit at noon "+
			"is being averaged across instead of splitting the day", len(got))
	}
	before, ok := got[oldV]
	if !ok {
		t.Fatalf("no row for the pre-edit geometry (version %d)", oldV)
	}
	after, ok := got[newV]
	if !ok {
		t.Fatalf("no row for the post-edit geometry (version %d)", newV)
	}
	if before.samples != 2 || after.samples != 2 {
		t.Errorf("sample split = %d before / %d after, want 2/2",
			before.samples, after.samples)
	}
	// The point of the split, stated as a number: the two geometries read
	// very differently, and a blend would have hidden it.
	if before.mean > 0.5 {
		t.Errorf("pre-edit mean_good = %.3f, want ~0.41", before.mean)
	}
	if after.mean < 0.9 {
		t.Errorf("post-edit mean_good = %.3f, want ~0.96", after.mean)
	}
	if after.mean-before.mean < 0.4 {
		t.Error("the two halves of the day are supposed to differ sharply; if " +
			"they do not, this fixture no longer tests what it claims to")
	}
}

// A lane's FIRST version covers readings taken before it was ever seen.
//
// This is what makes version_id NOT NULL possible, and it rests on two facts
// that were being conflated. valid_from is when the geometry BEGAN; the diff
// row's observed_at is when we first SAW it. Stamping a first version with the
// sync time claims the lane came into existence the moment Core happened to
// look — which strands every reading taken before that instant with no
// version, and there is no such thing as a reading on a lane with no geometry.
//
// An open lower bound says the true thing: this is the earliest geometry we
// know of, and we cannot say when it began.
func TestRollUp_FirstVersionCoversReadingsTakenBeforeItWasSeen(t *testing.T) {
	db := openWithWindow(t)
	addNamedSegmentNoVersion(t, db, "area-a", "LM1-LM2", "LM1", "LM2", 0, 0, 10, 0)

	// The first version is written by a sync that ran TOMORROW — the
	// boot-window shape, where sampling starts before the scene sync finishes.
	first := openFirstLaneVersion(t, db, "area-a", "LM1-LM2", testDay.AddDate(0, 0, 1), nil)

	insert(t, db,
		sample("AMR-01", testDay.Add(9*time.Hour), 0.90, 1, 0, 1),
		sample("AMR-01", testDay.Add(10*time.Hour), 0.92, 2, 0, 1),
	)

	cfg := rollUpCfg()
	cfg.Versions = store.LaneVersionResolver{}
	res, err := db.RollUpRobotConfidence(testDay, cfg)
	if err != nil {
		t.Fatalf("roll-up: %v", err)
	}
	if res.UnversionedSamples != 0 {
		t.Errorf("UnversionedSamples = %d, want 0 — a first version opens at "+
			"an early bound and covers these", res.UnversionedSamples)
	}

	var version int64
	var samples int
	if err := db.QueryRow(
		`SELECT version_id, samples FROM lane_confidence_daily
		  WHERE day=$1 AND lane=$2`, testDay, laneOf2("LM1-LM2")).
		Scan(&version, &samples); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if version != first {
		t.Errorf("version_id = %d, want %d — readings before the first sync "+
			"belong to the earliest geometry we know of", version, first)
	}
	if samples != 2 {
		t.Errorf("samples = %d, want 2", samples)
	}
}

// A lane with NO version at all is quarantined and COUNTED.
//
// Different state from "before the first version": it means the scene sync has
// never run since versioning landed. A defect to surface, not a hole to carry
// in a key — and inventing a version would be the same mistake as keying an
// unnameable edge on its directed name.
func TestRollUp_LaneWithNoVersionIsQuarantined(t *testing.T) {
	db := openWithWindow(t)
	addNamedSegmentNoVersion(t, db, "area-a", "LM1-LM2", "LM1", "LM2", 0, 0, 10, 0)
	// No version row written at all.

	insert(t, db,
		sample("AMR-01", testDay.Add(9*time.Hour), 0.90, 1, 0, 1),
		sample("AMR-01", testDay.Add(10*time.Hour), 0.92, 2, 0, 1),
	)

	cfg := rollUpCfg()
	cfg.Versions = store.LaneVersionResolver{}
	res, err := db.RollUpRobotConfidence(testDay, cfg)
	if err != nil {
		t.Fatalf("roll-up: %v", err)
	}
	if res.UnversionedSamples != 2 {
		t.Errorf("UnversionedSamples = %d, want 2 — held out AND counted, "+
			"never silently dropped", res.UnversionedSamples)
	}
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM lane_confidence_daily WHERE day=$1`, testDay).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("got %d rows for an unversioned lane, want 0", n)
	}
}

// The upsert is idempotent. Re-running a day is the first thing anyone does
// when the snap logic changes.
func TestRollUp_IsIdempotentAcrossReruns(t *testing.T) {
	db := openWithWindow(t)
	addNamedSegmentNoVersion(t, db, "area-a", "VERSIONED", "VA", "VB", 0, 0, 10, 0)
	addNamedSegmentNoVersion(t, db, "area-a", "UNVERSIONED", "UA", "UB", 0, 60, 10, 60)

	openFirstLaneVersion(t, db, "area-a", "VA-VB", testDay, nil)

	insert(t, db,
		sample("AMR-01", testDay.Add(9*time.Hour), 0.90, 5, 0, 1),
		sample("AMR-01", testDay.Add(9*time.Hour+time.Minute), 0.91, 5, 60, 1),
	)

	cfg := rollUpCfg()
	cfg.Versions = store.LaneVersionResolver{}
	for i := 0; i < 3; i++ {
		if _, err := db.RollUpRobotConfidence(testDay, cfg); err != nil {
			t.Fatalf("roll-up pass %d: %v", i, err)
		}
	}

	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM lane_confidence_daily WHERE day=$1`, testDay).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	// One row: the versioned lane once. The unversioned lane is quarantined
	// on every pass and writes nothing on any of them.
	if n != 1 {
		t.Errorf("three roll-up passes produced %d rows, want 1", n)
	}
}

// laneOf2 mirrors laneOf for fixtures added with explicit endpoint names.
func laneOf2(instance string) string {
	switch instance {
	case "LM1-LM2":
		return "LM1-LM2"
	case "VERSIONED":
		return "VA-VB"
	case "UNVERSIONED":
		return "UA-UB"
	}
	return instance
}
