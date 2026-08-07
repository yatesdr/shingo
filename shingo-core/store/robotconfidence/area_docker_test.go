//go:build docker

package robotconfidence_test

import (
	"math"
	"testing"
	"time"

	"shingocore/store"
	"shingocore/store/robotconfidence"
)

// The zone roll-up and the plant-day record.
//
// Both exist because a per-lane table cannot hold them: a zone is one-to-many
// with readings, and the plant counts are facts about readings that have NO
// LANE at all.

// withAreaIDs is the vendor's own membership, as the wire sends it — UNPADDED.
// The map stores "08" for the same zone, so anything comparing the literal
// strings joins to nothing.
func withAreaIDs(s robotconfidence.Sample, ids ...string) robotconfidence.Sample {
	s.AreaIDs = ids
	return s
}

// ONE READING, MANY ZONES — and the totals deliberately do not add up.
//
// SEER areas overlap by design, so a reading inside two zones is counted in
// both. This is the property that makes summing this table's `samples` column
// wrong, and it is asserted rather than assumed because the natural reading of
// any daily aggregate is that its rows partition the day.
func TestRollUp_AZoneRollUpIsOneToMany(t *testing.T) {
	db := openWithWindow(t)
	addSegment(t, db, "area-a", "LM1-LM2", 0, 0, 10, 0)

	// Three readings. One is in zone 8 only, one in zone 12 only, one in both.
	insert(t, db,
		withAreaIDs(sample("AMR-01", testDay.Add(9*time.Hour), 0.90, 1, 0, 1), "8"),
		withAreaIDs(sample("AMR-01", testDay.Add(10*time.Hour), 0.80, 2, 0, 1), "12"),
		withAreaIDs(sample("AMR-02", testDay.Add(11*time.Hour), 0.70, 3, 0, 1), "8", "12"),
	)

	res, err := db.RollUpRobotConfidence(testDay, rollUpCfg())
	if err != nil {
		t.Fatalf("roll-up: %v", err)
	}
	if res.AreaRows != 2 {
		t.Errorf("AreaRows = %d, want 2", res.AreaRows)
	}

	// Zone ids are stored NORMALISED, so the wire's "8" and the map's "08" are
	// one row. A test that looked for "8" here would pass on a roll-up that
	// never normalised and then join to nothing in production.
	var n8, n12 int
	if err := db.QueryRow(
		`SELECT samples FROM area_confidence_daily WHERE day=$1 AND area_name='08'`,
		testDay).Scan(&n8); err != nil {
		t.Fatalf("zone 08: %v", err)
	}
	if err := db.QueryRow(
		`SELECT samples FROM area_confidence_daily WHERE day=$1 AND area_name='12'`,
		testDay).Scan(&n12); err != nil {
		t.Fatalf("zone 12: %v", err)
	}
	if n8 != 2 || n12 != 2 {
		t.Errorf("zone samples = 08:%d 12:%d, want 2 and 2 — the reading in both "+
			"zones belongs to both", n8, n12)
	}
	// Three readings, four zone-samples. If this ever equals SamplesRead
	// somebody has made the attribution exclusive and the overlap is gone.
	if n8+n12 == res.SamplesRead {
		t.Errorf("zone samples sum to SamplesRead (%d) — the attribution has "+
			"become one-to-one and overlapping zones are being dropped", res.SamplesRead)
	}
}

// A ZONE IS NOT A LANE. A reading that snapped to nothing still happened
// somewhere the robot could name, and gating zones on the lane would lose
// exactly the readings a dead zone produces most of.
func TestRollUp_ZonesCountReadingsThatSnappedToNoLane(t *testing.T) {
	db := openWithWindow(t)
	addSegment(t, db, "area-a", "LM1-LM2", 0, 0, 10, 0)

	insert(t, db,
		// On the lane, and in zone 8.
		withAreaIDs(sample("AMR-01", testDay.Add(9*time.Hour), 0.90, 1, 0, 1), "8"),
		// Far from any lane — an orphan — but still inside zone 8.
		withAreaIDs(sample("AMR-01", testDay.Add(10*time.Hour), 0.10, 500, 500, 1), "8"),
	)

	res, err := db.RollUpRobotConfidence(testDay, rollUpCfg())
	if err != nil {
		t.Fatalf("roll-up: %v", err)
	}
	if res.Orphans != 1 {
		t.Fatalf("Orphans = %d, want 1", res.Orphans)
	}
	var samples int
	if err := db.QueryRow(
		`SELECT samples FROM area_confidence_daily WHERE day=$1 AND area_name='08'`,
		testDay).Scan(&samples); err != nil {
		t.Fatalf("zone 08: %v", err)
	}
	if samples != 2 {
		t.Errorf("zone 08 samples = %d, want 2 — an orphan reading still has a "+
			"zone, and dropping it hides the readings a dead zone produces most of",
			samples)
	}
}

// The two populations split on the zone row exactly as they do on the lane row.
//
// This is the table where banding the CONDITIONED mean is most tempting,
// because the zone IS the reflector-area membership that scored AUC 0.081 —
// almost perfectly backwards. A zone that returns a good reading half the time
// and nothing the rest must read as 0.45 in p50 and 0.90 in mean_good.
func TestRollUp_ZoneKeepsBothPopulations(t *testing.T) {
	db := openWithWindow(t)
	addSegment(t, db, "area-a", "LM1-LM2", 0, 0, 10, 0)

	noEstimate := math.Copysign(0, -1) // the wire sentinel; -0.0 is not a Go literal
	insert(t, db,
		withAreaIDs(sample("AMR-01", testDay.Add(9*time.Hour), 0.90, 1, 0, 1), "8"),
		withAreaIDs(sample("AMR-01", testDay.Add(10*time.Hour), noEstimate, 2, 0, 1), "8"),
		withAreaIDs(sample("AMR-01", testDay.Add(11*time.Hour), 0.90, 3, 0, 1), "8"),
		withAreaIDs(sample("AMR-01", testDay.Add(12*time.Hour), noEstimate, 4, 0, 1), "8"),
	)
	if _, err := db.RollUpRobotConfidence(testDay, rollUpCfg()); err != nil {
		t.Fatalf("roll-up: %v", err)
	}

	var samples, good, sentinel, sentinelRobots int
	var meanGood, p50 *float64
	if err := db.QueryRow(
		`SELECT samples, samples_good, sentinel_samples, sentinel_robots, mean_good, p50
		   FROM area_confidence_daily WHERE day=$1 AND area_name='08'`, testDay).
		Scan(&samples, &good, &sentinel, &sentinelRobots, &meanGood, &p50); err != nil {
		t.Fatalf("zone 08: %v", err)
	}
	if samples != 4 || good != 2 || sentinel != 2 || sentinelRobots != 1 {
		t.Errorf("samples=%d good=%d sentinel=%d sentinelRobots=%d, want 4/2/2/1",
			samples, good, sentinel, sentinelRobots)
	}
	if meanGood == nil || math.Abs(*meanGood-0.90) > 1e-9 {
		t.Errorf("mean_good = %v, want 0.90 — the conditioned view", meanGood)
	}
	// p50 by nearest rank over [0, 0, 0.9, 0.9] is the lower middle: 0.
	if p50 == nil || *p50 != 0 {
		t.Errorf("p50 = %v, want 0 — the unconditioned view counts a miss as the "+
			"zero it is, which is the whole reason it can be banded", p50)
	}
}

// The plant row carries what has no lane to hang on, and it must SURVIVE.
//
// These counts reached a log line and nothing else, while the raw samples
// behind them expire at 14 days — so a fortnight after any interesting day,
// "was the plant like this" stopped being answerable.
func TestRollUp_PlantDailyRecordsWhatHasNoLane(t *testing.T) {
	db := openWithWindow(t)
	addSegment(t, db, "area-a", "LM1-LM2", 0, 0, 10, 0)
	addUnnamedSegment(t, db, "area-a", "NAMELESS", 0, 60, 10, 60)

	insert(t, db,
		// good, on the lane
		sample("AMR-01", testDay.Add(9*time.Hour), 0.90, 1, 0, 1),
		// on the unnameable edge -> unkeyable
		sample("AMR-01", testDay.Add(10*time.Hour), 0.90, 1, 60, 1),
		// nowhere near a lane -> orphan
		sample("AMR-01", testDay.Add(11*time.Hour), 0.90, 500, 500, 1),
		// reloc_status 3 -> unattributed, which used to be counted NOWHERE
		sample("AMR-02", testDay.Add(12*time.Hour), 0.90, 2, 0, 3),
	)

	res, err := db.RollUpRobotConfidence(testDay, rollUpCfg())
	if err != nil {
		t.Fatalf("roll-up: %v", err)
	}
	if res.UnattributedSamples != 1 {
		t.Errorf("UnattributedSamples = %d, want 1 — a reloc_status of 3 is read, "+
			"snapped and then held out, and it used to be counted nowhere at all",
			res.UnattributedSamples)
	}

	var read, orphans, unkEdges, unkSamples, unattributed int
	if err := db.QueryRow(
		`SELECT samples_read, orphan_samples, unkeyable_edges, unkeyable_samples,
		        unattributed_samples
		   FROM plant_confidence_daily WHERE day=$1`, testDay).
		Scan(&read, &orphans, &unkEdges, &unkSamples, &unattributed); err != nil {
		t.Fatalf("plant row: %v", err)
	}
	if read != 4 || orphans != 1 || unkEdges != 1 || unkSamples != 1 || unattributed != 1 {
		t.Errorf("plant row = read:%d orphans:%d unkEdges:%d unkSamples:%d unattributed:%d, "+
			"want 4/1/1/1/1", read, orphans, unkEdges, unkSamples, unattributed)
	}
	// unkeyable_edges counts SCENE ROWS and every other column counts SAMPLES.
	// Different units in adjacent columns is a trap, so the row says so in its
	// own doc; this line is here so a change of grain breaks a test.
	if unkEdges != 1 {
		t.Errorf("unkeyable_edges = %d, want 1 (edges, not samples)", unkEdges)
	}
}

// A reading whose LANE cannot be named is still a reading BY THAT ROBOT.
//
// The lane quarantines used to sit above the robot accumulation, so an
// unkeyable or unversioned sample vanished from the robot's own mean too —
// answering "which lane was this on" by discarding "how is this robot doing".
func TestRollUp_LaneQuarantineDoesNotErasyTheRobotsOwnReading(t *testing.T) {
	db := openWithWindow(t)
	addUnnamedSegment(t, db, "area-a", "NAMELESS", 0, 0, 10, 0)

	insert(t, db,
		sample("AMR-01", testDay.Add(9*time.Hour), 0.90, 1, 0, 1),
		sample("AMR-01", testDay.Add(10*time.Hour), 0.70, 2, 0, 1),
	)
	res, err := db.RollUpRobotConfidence(testDay, rollUpCfg())
	if err != nil {
		t.Fatalf("roll-up: %v", err)
	}
	if res.UnkeyableSamples != 2 {
		t.Fatalf("UnkeyableSamples = %d, want 2", res.UnkeyableSamples)
	}

	var samples int
	var mean *float64
	if err := db.QueryRow(
		`SELECT samples, mean FROM robot_confidence_daily WHERE day=$1 AND vehicle_id='AMR-01'`,
		testDay).Scan(&samples, &mean); err != nil {
		t.Fatalf("robot row: %v", err)
	}
	if samples != 2 {
		t.Errorf("robot samples = %d, want 2 — the lane could not be named, the "+
			"ROBOT's reading was never in doubt", samples)
	}
	if mean == nil || math.Abs(*mean-0.80) > 1e-9 {
		t.Errorf("robot mean = %v, want 0.80", mean)
	}
}

// The zone label is resolved at the DAY, not at now.
func TestRollUp_ZoneClassComesFromTheMapAsItWasThatDay(t *testing.T) {
	db := openWithWindow(t)
	addSegment(t, db, "area-a", "LM1-LM2", 0, 0, 10, 0)

	diffID := newSceneDiff(t, db, testDay)
	if _, err := db.Exec(
		`INSERT INTO scene_areas
		   (area_name, class_name, polygon, reflector_count, shape_hash, def_hash,
		    diff_id, valid_from)
		 VALUES ('08','ReflectorArea','[]',0,'sh','df',$1,'0001-01-01 00:00:00+00')`,
		diffID); err != nil {
		t.Fatalf("insert area: %v", err)
	}

	insert(t, db,
		withAreaIDs(sample("AMR-01", testDay.Add(9*time.Hour), 0.90, 1, 0, 1), "8"))

	cfg := rollUpCfg()
	cfg.AreaClasses = store.AreaClassLookup{}
	if _, err := db.RollUpRobotConfidence(testDay, cfg); err != nil {
		t.Fatalf("roll-up: %v", err)
	}

	var class string
	if err := db.QueryRow(
		`SELECT class_name FROM area_confidence_daily WHERE day=$1 AND area_name='08'`,
		testDay).Scan(&class); err != nil {
		t.Fatalf("zone 08: %v", err)
	}
	if class != "ReflectorArea" {
		t.Errorf("class_name = %q, want ReflectorArea — the class is the field "+
			"measured to predict anything, and the join has to survive the "+
			"robot saying \"8\" while the map says \"08\"", class)
	}
}

// A missing class resolver costs the LABEL, never the MEASUREMENT.
func TestRollUp_ZonesAreStillWrittenWithNoClassResolver(t *testing.T) {
	db := openWithWindow(t)
	addSegment(t, db, "area-a", "LM1-LM2", 0, 0, 10, 0)
	insert(t, db,
		withAreaIDs(sample("AMR-01", testDay.Add(9*time.Hour), 0.90, 1, 0, 1), "8"))

	cfg := rollUpCfg() // AreaClasses deliberately nil
	res, err := db.RollUpRobotConfidence(testDay, cfg)
	if err != nil {
		t.Fatalf("roll-up: %v", err)
	}
	if res.AreaRows != 1 {
		t.Fatalf("AreaRows = %d, want 1 — refusing to write a measurement to "+
			"protect a label is the wrong trade", res.AreaRows)
	}
	var class string
	var samples int
	if err := db.QueryRow(
		`SELECT class_name, samples FROM area_confidence_daily WHERE day=$1 AND area_name='08'`,
		testDay).Scan(&class, &samples); err != nil {
		t.Fatalf("zone 08: %v", err)
	}
	if class != "" || samples != 1 {
		t.Errorf("class=%q samples=%d, want empty class and 1 sample", class, samples)
	}
}
