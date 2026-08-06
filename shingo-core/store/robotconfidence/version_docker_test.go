//go:build docker

package robotconfidence_test

import (
	"database/sql"
	"testing"
	"time"

	"shingocore/store"
)

// openLaneVersion writes a version row and returns its id. valid_to nil means
// still in force.
func openLaneVersion(t *testing.T, db *store.DB, area, lane string,
	from time.Time, to *time.Time) int64 {
	t.Helper()
	var diffID int64
	if err := db.QueryRow(
		`INSERT INTO scene_diffs (source, gate_hash, observed_at)
		 VALUES ('rds_scene','test',$1) RETURNING id`, from).Scan(&diffID); err != nil {
		t.Fatalf("insert diff: %v", err)
	}
	var id int64
	if err := db.QueryRow(
		`INSERT INTO scene_lane_versions
		   (area_name, lane, shape_hash, def_hash, shape, directed_rows,
		    diff_id, valid_from, valid_to)
		 VALUES ($1,$2,'sh','df','[]',2,$3,$4,$5) RETURNING id`,
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
	addNamedSegment(t, db, "area-a", "LM1-LM2", "LM1", "LM2", 0, 0, 10, 0)

	noon := testDay.Add(12 * time.Hour)
	oldV := openLaneVersion(t, db, "area-a", "LM1-LM2", testDay, &noon)
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

	got := map[int64]struct {
		samples int
		mean    float64
	}{}
	for rows.Next() {
		var v sql.NullInt64
		var n int
		var mean sql.NullFloat64
		if err := rows.Scan(&v, &n, &mean); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if !v.Valid {
			t.Errorf("a row came back with no version — every sample here falls "+
				"inside a version's validity window (samples=%d)", n)
			continue
		}
		got[v.Int64] = struct {
			samples int
			mean    float64
		}{n, mean.Float64}
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

// A sample taken before any version existed stores a NULL version, and the
// row still lands.
//
// Every day of raw already in a plant database when this deploys is exactly
// that case — collected before scene versioning existed. A NOT NULL key would
// have made those days unwritable, and a sentinel id meaning "unknown" would
// be absence coalesced into a foreign key.
func TestRollUp_SamplesBeforeAnyVersionKeepANullVersion(t *testing.T) {
	db := openWithWindow(t)
	addNamedSegment(t, db, "area-a", "LM1-LM2", "LM1", "LM2", 0, 0, 10, 0)

	// The lane's first version opens tomorrow; today's readings predate it.
	openLaneVersion(t, db, "area-a", "LM1-LM2", testDay.AddDate(0, 0, 1), nil)

	insert(t, db,
		sample("AMR-01", testDay.Add(9*time.Hour), 0.90, 1, 0, 1),
		sample("AMR-01", testDay.Add(10*time.Hour), 0.92, 2, 0, 1),
	)

	cfg := rollUpCfg()
	cfg.Versions = store.LaneVersionResolver{}
	if _, err := db.RollUpRobotConfidence(testDay, cfg); err != nil {
		t.Fatalf("roll-up: %v", err)
	}

	var version sql.NullInt64
	var samples int
	if err := db.QueryRow(
		`SELECT version_id, samples FROM lane_confidence_daily
		  WHERE day=$1 AND lane=$2`, testDay, laneOf2("LM1-LM2")).
		Scan(&version, &samples); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if version.Valid {
		t.Errorf("version_id = %d, want NULL — these readings predate every "+
			"version of this lane", version.Int64)
	}
	if samples != 2 {
		t.Errorf("samples = %d, want 2 — the row must still be written", samples)
	}
}

// The upsert is idempotent on BOTH partial unique indexes.
//
// version_id NULL and version_id set live under two different indexes, so a
// writer that named only one conflict target would insert duplicates for the
// other. Re-running a day is the first thing anyone does when the snap logic
// changes.
func TestRollUp_IsIdempotentForBothVersionedAndUnversionedRows(t *testing.T) {
	db := openWithWindow(t)
	addNamedSegment(t, db, "area-a", "VERSIONED", "VA", "VB", 0, 0, 10, 0)
	addNamedSegment(t, db, "area-a", "UNVERSIONED", "UA", "UB", 0, 60, 10, 60)

	openLaneVersion(t, db, "area-a", "VA-VB", testDay, nil)

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
	if n != 2 {
		t.Errorf("three roll-up passes produced %d rows, want 2 — one of the two "+
			"partial unique indexes is not being used as a conflict target", n)
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
