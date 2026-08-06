package robotconfidence

import (
	"database/sql"
	"fmt"
	"regexp"
	"time"
)

// SQL shell for the localization-confidence tables (migration v77). All
// analytical math is in robotconfidence.go's pure functions; this file only
// reads and writes. Same split as store/heartbeat.

// The two partitioned sample tables. Exported so the boot/nightly wiring in
// cmd/shingocore can name them without restating string literals.
const (
	TableSamples = "robot_confidence_samples"
	TableLow     = "robot_confidence_low"
)

// partitionedTables is the allow-list for every function that interpolates a
// table name into DDL. Table names cannot be bound as query parameters, so
// the only safe construction is one that can never carry a caller's string
// through to the server.
var partitionedTables = map[string]bool{TableSamples: true, TableLow: true}

// Sample is one stored localization reading. Every field maps to a column;
// see migration v77 for why each one is here.
//
// RelocStatus has no meaningful zero value — 0 is FAILED, a real and
// alarming state — so this struct is always constructed with it set
// explicitly. The column carries no DEFAULT precisely so that a writer which
// forgets it fails loudly instead of recording a healthy pose that was never
// observed.
type Sample struct {
	VehicleID   string
	SampledAt   time.Time
	Confidence  float64
	X           float64
	Y           float64
	Angle       float64
	Station     string
	LastStation string
	OrderID     int64
	OnTask      bool
	Blocked     bool
	RelocStatus int
	// AreaIDs is the robot's advanced-area membership at sample time. It is
	// what separates "this zone has no localization" from "this robot is
	// lost" — a Confidence of -0.0 inside a known area is the vendor saying
	// it has no estimate here, not a robot reporting zero certainty.
	//
	// Always set by the one writer, including to an empty slice for the
	// normal case of a robot in no area. A NULL in this column means the row
	// predates migration v78, not that the robot was outside every area.
	AreaIDs []string
	// MapMD5 is the hash of the map THIS robot was localizing against when
	// the reading was taken.
	//
	// It is on the sample row and not merely on a scene table because a
	// fleet is not guaranteed to be on one map. Measured at Hopkinsville
	// 2026-08-06: eleven robots on Hop_20 and one on Hop_21. A sample from
	// the odd robot out is snapped, at roll-up, against scene_edges built
	// from a different map — so this column is what lets that row be
	// quarantined instead of silently mixed into a lane average. Recovering
	// it afterwards is impossible: nothing else records which map a given
	// tick was taken under.
	MapMD5 string
	// AlarmCodes are the robot's active alarm codes at sample time.
	//
	// They were never dropped at unmarshal — rds.RbkReport.Alarms is decoded
	// and fleet.RobotStatus.Alarms is populated — they simply never reached
	// a durable row. The codes live on mission_telemetry attached to mission
	// ENDPOINTS, so a no-estimate reading here cannot be joined to the alarm
	// that accompanied it. 54018 ("reflectors in map not enough") names the
	// exact zones it is complaining about and has been standing on every
	// Springfield robot since June; one column links the symptom to the
	// diagnosis in a single row.
	//
	// []int32 and not []int: pgx carries an int4 array codec, and `int` is
	// platform-width with no fixed Postgres partner. The same caution v78
	// applied to []string → TEXT[] — whether a Go slice binds to a Postgres
	// array through the database/sql shim is a driver property, not
	// something the compiler can answer, so it is asserted by a round-trip
	// test rather than assumed.
	AlarmCodes []int32
}

const insertSampleSQL = `INSERT INTO %s
	(vehicle_id, sampled_at, confidence, x, y, angle, station, last_station, order_id, on_task, blocked, reloc_status, area_ids, map_md5, alarm_codes)
	VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`

// InsertBatch writes one poll tick's worth of samples in a single
// transaction, double-writing anything below lowThreshold into
// robot_confidence_low.
//
// DOUBLE-WRITE, NOT COPY-BEFORE-DROP. The low-confidence trail outlives raw
// retention by months and is the forensic record for "why did this robot
// strand here". Deriving it later with a nightly copy job would mean a single
// failed run silently loses the only rows anybody will still want in ninety
// days. Writing it at sample time costs one extra INSERT on roughly the small
// fraction of samples that are actually low, and cannot be missed.
//
// One transaction per tick rather than per row: the write rule evaluates all
// robots against the same poll, so the batch is the natural unit and it keeps
// round-trips down to one regardless of fleet size.
func InsertBatch(db *sql.DB, samples []Sample, lowThreshold float64) error {
	if len(samples) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("robot confidence: begin: %w", err)
	}
	defer tx.Rollback() // no-op after Commit

	rawStmt, err := tx.Prepare(fmt.Sprintf(insertSampleSQL, TableSamples))
	if err != nil {
		return fmt.Errorf("robot confidence: prepare raw: %w", err)
	}
	defer rawStmt.Close()

	var lowStmt *sql.Stmt
	for _, s := range samples {
		// areaIDs is normalised to a non-nil empty slice so the column is
		// written as '{}' ("in no area", an observation) rather than NULL
		// ("not collected", which after v78 would be a lie).
		areaIDs := s.AreaIDs
		if areaIDs == nil {
			areaIDs = []string{}
		}
		// Same rule for alarms: '{}' is "we looked and there were none",
		// NULL would be "we did not look". The writer always looked.
		alarms := s.AlarmCodes
		if alarms == nil {
			alarms = []int32{}
		}
		if _, err := rawStmt.Exec(s.VehicleID, s.SampledAt, s.Confidence, s.X, s.Y, s.Angle,
			s.Station, s.LastStation, s.OrderID, s.OnTask, s.Blocked, s.RelocStatus, areaIDs,
			s.MapMD5, alarms); err != nil {
			return fmt.Errorf("robot confidence: insert sample: %w", err)
		}
		if s.Confidence >= lowThreshold {
			continue
		}
		if lowStmt == nil {
			lowStmt, err = tx.Prepare(fmt.Sprintf(insertSampleSQL, TableLow))
			if err != nil {
				return fmt.Errorf("robot confidence: prepare low: %w", err)
			}
			defer lowStmt.Close()
		}
		if _, err := lowStmt.Exec(s.VehicleID, s.SampledAt, s.Confidence, s.X, s.Y, s.Angle,
			s.Station, s.LastStation, s.OrderID, s.OnTask, s.Blocked, s.RelocStatus, areaIDs,
			s.MapMD5, alarms); err != nil {
			return fmt.Errorf("robot confidence: insert low: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("robot confidence: commit: %w", err)
	}
	return nil
}

// ── Partition lifecycle ────────────────────────────────────────────────────

// EnsurePartitions creates today's and tomorrow's partitions on both sample
// tables if absent. Run at boot and daily so an INSERT never falls through a
// day boundary into a missing partition. Idempotent.
//
// DAILY, not monthly like store/heartbeat. The retention here is measured in
// days (14 raw), so a monthly partition could not be dropped until the whole
// month aged out — which would hold six weeks of rows to honour a two-week
// policy. The granularity of the partition has to match the granularity of
// the retention or the DROP stops being the cheap operation it was chosen for.
func EnsurePartitions(db *sql.DB, ref time.Time) error {
	for _, table := range []string{TableSamples, TableLow} {
		for _, d := range []time.Time{ref, ref.AddDate(0, 0, 1)} {
			if err := createDayPartition(db, table, d); err != nil {
				return err
			}
		}
	}
	return nil
}

// EnsurePartitionsRange creates partitions for every day in [start, end] on
// both tables. Used by sim startup to pre-create a fast-forward window so
// inserts never fail during catch-up. Idempotent.
func EnsurePartitionsRange(db *sql.DB, start, end time.Time) error {
	d := start.UTC().Truncate(24 * time.Hour)
	last := end.UTC().Truncate(24 * time.Hour)
	for !d.After(last) {
		for _, table := range []string{TableSamples, TableLow} {
			if err := createDayPartition(db, table, d); err != nil {
				return err
			}
		}
		d = d.AddDate(0, 0, 1)
	}
	return nil
}

func createDayPartition(db *sql.DB, table string, day time.Time) error {
	if !partitionedTables[table] {
		return fmt.Errorf("robot confidence: not a partitioned table: %q", table)
	}
	start := day.UTC().Truncate(24 * time.Hour)
	end := start.AddDate(0, 0, 1)
	name := partitionName(table, start)
	// name and bounds derive from validated date components and an
	// allow-listed table, not from user input.
	q := fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')`,
		name, table, start.Format("2006-01-02"), end.Format("2006-01-02"))
	if _, err := db.Exec(q); err != nil {
		return fmt.Errorf("create partition %s: %w", name, err)
	}
	return nil
}

// partitionName follows the house form established by
// store/heartbeat.partitionName (<table>_YYYY_MM), extended by a day
// component. store/downtime briefly diverged to a _y2026m08 form and
// migration v57 renamed it back; do not reintroduce a second convention.
func partitionName(table string, day time.Time) string {
	return fmt.Sprintf("%s_%04d_%02d_%02d", table, day.Year(), int(day.Month()), day.Day())
}

var partitionSuffixRe = regexp.MustCompile(`_(\d{4})_(\d{2})_(\d{2})$`)

// DropOldPartitions drops partitions of table whose day ends before
// now-keepDays, and returns how many went.
//
// DROP rather than DELETE, and this is the reason the tables are partitioned
// at all: dropping a partition is instant and leaves no dead tuples, while
// deleting a day of rows scans, writes tombstones, and hands the whole lot to
// autovacuum. At tens of thousands of rows a day that difference compounds.
//
// Retention is the ONE number in this design that is not a one-way door.
// Daily partitions make changing it a config edit plus the next run of this
// loop — no rewrite, no backfill, nothing recomputed. Everything else about
// the shape of these tables is permanent; this is a dial.
func DropOldPartitions(db *sql.DB, table string, keepDays int, now time.Time) (int, error) {
	if !partitionedTables[table] {
		return 0, fmt.Errorf("robot confidence: not a partitioned table: %q", table)
	}
	cutoff := now.UTC().AddDate(0, 0, -keepDays).Truncate(24 * time.Hour)
	rows, err := db.Query(`SELECT c.relname
		FROM pg_inherits i
		JOIN pg_class c ON c.oid = i.inhrelid
		JOIN pg_class p ON p.oid = i.inhparent
		WHERE p.relname = $1`, table)
	if err != nil {
		return 0, fmt.Errorf("list partitions of %s: %w", table, err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return 0, err
		}
		names = append(names, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	dropped := 0
	for _, name := range names {
		m := partitionSuffixRe.FindStringSubmatch(name)
		if m == nil {
			// Not a partition this code named. Leave it alone rather than
			// guess — an unrecognised child is somebody else's business.
			continue
		}
		var y, mo, d int
		if _, err := fmt.Sscanf(m[1]+" "+m[2]+" "+m[3], "%d %d %d", &y, &mo, &d); err != nil {
			continue
		}
		start := time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC)
		// The partition covers [start, start+1d); it is expendable once its
		// whole span is older than the cutoff.
		if !start.Before(cutoff) {
			continue
		}
		if _, err := db.Exec(fmt.Sprintf(`DROP TABLE IF EXISTS %s`, name)); err != nil {
			return dropped, fmt.Errorf("drop partition %s: %w", name, err)
		}
		dropped++
	}
	return dropped, nil
}

// ── Reads for the roll-up ──────────────────────────────────────────────────

// RawSample is one row as the roll-up reads it back: only the columns the
// aggregation needs.
type RawSample struct {
	VehicleID   string
	Confidence  float64
	X           float64
	Y           float64
	RelocStatus int
}

// ScanSamples streams rows in [from, to) to fn, without materialising the
// window. The baseline pass covers fourteen days — several hundred thousand
// rows — and there is no reason for any of it to be resident at once: the
// accumulators the caller builds are bounded by robots × segments, a few
// thousand entries regardless of how many samples flow through.
func ScanSamples(db *sql.DB, from, to time.Time, fn func(RawSample)) error {
	rows, err := db.Query(
		`SELECT vehicle_id, confidence, x, y, reloc_status
		 FROM `+TableSamples+`
		 WHERE sampled_at >= $1 AND sampled_at < $2`, from, to)
	if err != nil {
		return fmt.Errorf("scan samples: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var s RawSample
		if err := rows.Scan(&s.VehicleID, &s.Confidence, &s.X, &s.Y, &s.RelocStatus); err != nil {
			return err
		}
		fn(s)
	}
	return rows.Err()
}

// LoadSegments reads the synced scene's path segments for snapping.
func LoadSegments(db *sql.DB) ([]Segment, error) {
	rows, err := db.Query(
		`SELECT area_name, instance_name, from_x, from_y, to_x, to_y FROM scene_edges`)
	if err != nil {
		return nil, fmt.Errorf("load scene edges: %w", err)
	}
	defer rows.Close()
	var out []Segment
	for rows.Next() {
		var s Segment
		if err := rows.Scan(&s.Area, &s.Instance, &s.FromX, &s.FromY, &s.ToX, &s.ToY); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ── Roll-up writes ─────────────────────────────────────────────────────────

// RobotDaily is one row of robot_confidence_daily. Residual, Mean and P05 are
// pointers because all three are genuinely absent when coverage is too thin,
// and NULL has to survive to the renderer as something visibly different from
// zero — "not enough data to say" and "exactly average" are opposite
// findings.
type RobotDaily struct {
	Day       time.Time
	VehicleID string
	Residual  *float64
	Cells     int
	Samples   int
	Mean      *float64
	P05       *float64
}

// UpsertRobotDaily writes one robot's day, replacing any existing row.
//
// Idempotent on the primary key so the job is safe to re-run for a past day.
// That is not a theoretical nicety: the first time the snap tolerance or the
// coverage thresholds change, every affected day wants recomputing, and a job
// that could only ever append would make that a manual repair.
func UpsertRobotDaily(db *sql.DB, r RobotDaily) error {
	_, err := db.Exec(
		`INSERT INTO robot_confidence_daily (day, vehicle_id, residual, cells, samples, mean, p05)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)
		 ON CONFLICT (day, vehicle_id) DO UPDATE SET
		   residual = EXCLUDED.residual, cells = EXCLUDED.cells,
		   samples = EXCLUDED.samples, mean = EXCLUDED.mean, p05 = EXCLUDED.p05`,
		r.Day, r.VehicleID, r.Residual, r.Cells, r.Samples, r.Mean, r.P05)
	if err != nil {
		return fmt.Errorf("upsert robot_confidence_daily: %w", err)
	}
	return nil
}

// SegmentDaily is one row of segment_confidence_daily.
//
// Mean, P05 and MinConf are pointers because a segment can legitimately have
// no valid reading to average: if every sample there that day was taken
// during a failed relocation, the confidence numbers cannot be believed but
// the failures absolutely can. That is the worst segment on the floor, and it
// has to be describable — writing 0.0 would make it look catastrophic in a
// way the data does not support, and omitting the row would make it look
// fine, which is worse.
//
// The two failure counts are COUNTS OF EVENTS and need no trust in the
// reported confidence at all. Both are kept because one alone is ambiguous:
// fourteen failures by one robot is a robot problem, fourteen by six robots
// is a place problem. They are excluded from Mean/P05/MinConf, all of which
// are computed over reloc_status = 1 only.
type SegmentDaily struct {
	Day                time.Time
	Area               string
	Instance           string
	Mean               *float64
	P05                *float64
	MinConf            *float64
	Samples            int
	Robots             int
	RelocFailedSamples int
	RelocFailedRobots  int
}

// UpsertSegmentDaily writes one segment's day, replacing any existing row.
func UpsertSegmentDaily(db *sql.DB, s SegmentDaily) error {
	_, err := db.Exec(
		`INSERT INTO segment_confidence_daily
		   (day, area_name, edge_instance, mean, p05, min_conf, samples, robots,
		    reloc_failed_samples, reloc_failed_robots)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		 ON CONFLICT (day, area_name, edge_instance) DO UPDATE SET
		   mean = EXCLUDED.mean, p05 = EXCLUDED.p05, min_conf = EXCLUDED.min_conf,
		   samples = EXCLUDED.samples, robots = EXCLUDED.robots,
		   reloc_failed_samples = EXCLUDED.reloc_failed_samples,
		   reloc_failed_robots = EXCLUDED.reloc_failed_robots`,
		s.Day, s.Area, s.Instance, s.Mean, s.P05, s.MinConf, s.Samples, s.Robots,
		s.RelocFailedSamples, s.RelocFailedRobots)
	if err != nil {
		return fmt.Errorf("upsert segment_confidence_daily: %w", err)
	}
	return nil
}
