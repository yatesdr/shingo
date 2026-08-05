package robotconfidence

import (
	"database/sql"
	"fmt"
	"time"
)

// The nightly roll-up. Raw samples expire; these aggregates do not, and they
// cannot be rebuilt once the rows behind them are gone — which is why the job
// ships with the collection rather than after it.

// RollUpConfig carries the tunables the job reads. Coverage is separate
// because it defines the statistic rather than tuning the deployment; see
// robotconfidence.go.
type RollUpConfig struct {
	// SnapTolerance is how far (metres) a sample may sit from a path segment
	// and still be considered on it. Generous by necessity: scene_edges keeps
	// only segment endpoints, so a curved path is snapped against its chord,
	// which at Springfield diverges from the driven lane by up to 1.30 m.
	SnapTolerance float64
	// BaselineDays is the trailing window the fleet median is computed over.
	BaselineDays int
	Coverage     Coverage
}

// RollUpResult reports what the job did, for the caller's log line.
type RollUpResult struct {
	Day              time.Time
	RobotRows        int
	SegmentRows      int
	SamplesRead      int
	Orphans          int
	ResidualsNull    int
	SegmentsFailOnly int
}

// dayKey truncates to the UTC day the partitions are cut on.
//
// THE DAY IS UTC, NOT PLANT-LOCAL, and that is a deliberate alignment with
// the partition boundaries rather than an oversight. It does mean a plant
// "day" is split: UTC midnight falls in the evening local time at both
// plants, so one production shift can land in two rows. For the trend
// questions these tables answer — is this segment worse than it was, is this
// robot below its peers — a consistent 24-hour bucket is what matters and the
// offset is constant. Incident forensics is unaffected either way: it reads
// the raw samples, which keep their exact timestamps.
func dayKey(t time.Time) time.Time { return t.UTC().Truncate(24 * time.Hour) }

// accum is a running count and sum, resolved to a CellStat at the end.
type accum struct {
	n   int
	sum float64
}

func (a *accum) add(v float64) { a.n++; a.sum += v }

func (a accum) stat() CellStat {
	if a.n == 0 {
		return CellStat{}
	}
	return CellStat{N: a.n, Mean: a.sum / float64(a.n)}
}

// RollUp computes and stores one day's aggregates.
//
// Run this BEFORE the retention drop. With a 14-day raw window and a 14-day
// baseline there is slack, but the ordering is made explicit at the call site
// rather than left to depend on it.
func RollUp(db *sql.DB, day time.Time, cfg RollUpConfig) (RollUpResult, error) {
	res := RollUpResult{Day: dayKey(day)}

	segments, err := LoadSegments(db)
	if err != nil {
		return res, err
	}
	index := NewSegmentIndex(segments)

	from := dayKey(day)
	to := from.AddDate(0, 0, 1)

	// ── Pass 1: the day being rolled up ────────────────────────────────────
	// dayRC and the segment statistics take reloc_status = 1 ONLY. States 0
	// and 3 are stored and queryable but feed no statistic: a robot sitting
	// in FAILED at a charge point would otherwise drag that location's
	// baseline down for every healthy robot passing through, corrupting the
	// residual for everyone else — the exact confound this design exists to
	// remove.
	dayRC := map[string]map[string]*accum{} // robot -> segment -> accum
	segVals := map[string][]float64{}       // segment -> confidences
	segRobots := map[string]map[string]bool{}
	segFailed := map[string]int{} // segment -> reloc_status = 0 count
	segFailedRobots := map[string]map[string]bool{}
	robotVals := map[string][]float64{}
	robotSeen := map[string]bool{}

	err = ScanSamples(db, from, to, func(s RawSample) {
		res.SamplesRead++
		robotSeen[s.VehicleID] = true

		seg, onPath := index.Nearest(s.X, s.Y, cfg.SnapTolerance)
		if !onPath {
			res.Orphans++
		}
		key := seg.Key()

		if s.RelocStatus == 0 {
			// The confidence VALUE here cannot be trusted; the FACT of the
			// failure can be, completely. Counted, never averaged.
			//
			// Distinct robots as well as raw count: fourteen failures by one
			// robot is a robot problem, fourteen by six robots is a place
			// problem, and the bare count cannot tell them apart.
			if onPath {
				segFailed[key]++
				if segFailedRobots[key] == nil {
					segFailedRobots[key] = map[string]bool{}
				}
				segFailedRobots[key][s.VehicleID] = true
			}
			return
		}
		if s.RelocStatus != 1 {
			return // COMPLETED: settled but unconfirmed, held out of statistics
		}

		// The robot's own mean includes orphans: it is a descriptive figure
		// over everything that robot reported, and dropping readings because
		// they failed to snap would quietly reshape it.
		robotVals[s.VehicleID] = append(robotVals[s.VehicleID], s.Confidence)
		if !onPath {
			return
		}
		segVals[key] = append(segVals[key], s.Confidence)
		if segRobots[key] == nil {
			segRobots[key] = map[string]bool{}
		}
		segRobots[key][s.VehicleID] = true
		if dayRC[s.VehicleID] == nil {
			dayRC[s.VehicleID] = map[string]*accum{}
		}
		if dayRC[s.VehicleID][key] == nil {
			dayRC[s.VehicleID][key] = &accum{}
		}
		dayRC[s.VehicleID][key].add(s.Confidence)
	})
	if err != nil {
		return res, err
	}

	// ── Pass 2: the trailing baseline ──────────────────────────────────────
	// The baseline is a TRAILING window, not the same day, and that is what
	// makes a fleet-wide event visible. Against a same-day median, if
	// reflectors get dusty plant-wide on a Thursday every robot's residual
	// stays flat and the event vanishes — the median moves with the damage.
	// Against fourteen days, the whole fleet dips together, which is the
	// finding.
	//
	// The window includes the day being rolled up, as one day in fourteen.
	// That is enough dilution to keep a plant-wide event visible while still
	// giving a newly-laid segment a usable baseline on its first day. Where a
	// plant holds fewer raw days than BaselineDays the query simply finds
	// less; the statistic degrades toward same-day rather than failing.
	baseFrom := to.AddDate(0, 0, -cfg.BaselineDays)
	baseRC := map[string]map[string]*accum{}
	err = ScanSamples(db, baseFrom, to, func(s RawSample) {
		if s.RelocStatus != 1 {
			return
		}
		seg, onPath := index.Nearest(s.X, s.Y, cfg.SnapTolerance)
		if !onPath {
			return
		}
		key := seg.Key()
		if baseRC[s.VehicleID] == nil {
			baseRC[s.VehicleID] = map[string]*accum{}
		}
		if baseRC[s.VehicleID][key] == nil {
			baseRC[s.VehicleID][key] = &accum{}
		}
		baseRC[s.VehicleID][key].add(s.Confidence)
	})
	if err != nil {
		return res, err
	}

	medians := FleetMedians(resolve(baseRC), cfg.Coverage)

	// ── Write the robot rows ───────────────────────────────────────────────
	dayStats := resolve(dayRC)
	for robot := range robotSeen {
		row := RobotDaily{Day: from, VehicleID: robot}

		vals := robotVals[robot]
		row.Samples = len(vals)
		if len(vals) > 0 {
			mean := Mean(vals)
			p05 := Percentile(vals, 0.05)
			row.Mean, row.P05 = &mean, &p05
		}

		residual, cells, ok := Residual(dayStats[robot], medians, cfg.Coverage)
		row.Cells = cells
		if ok {
			row.Residual = &residual
		} else {
			// Left NULL on purpose. A robot compared to its peers in too few
			// places has not scored 0.0 — it has not been scored.
			res.ResidualsNull++
		}
		if err := UpsertRobotDaily(db, row); err != nil {
			return res, err
		}
		res.RobotRows++
	}

	// ── Write the segment rows ─────────────────────────────────────────────
	//
	// THE UNION IS THE POINT, AND ITERATING segVals ALONE IS THE BUG. A
	// segment whose every sample that day was a localization failure appears
	// in segFailed and NOT in segVals, because it produced no valid reading
	// to aggregate. Walking only the valid-sample map drops precisely that
	// segment — the worst place on the floor — while every other case still
	// comes out right, so nothing else in the suite notices. The same trap
	// exists in SQL: a GROUP BY over valid samples cannot emit a row for a
	// group that has none, which is why this must be a union rather than an
	// aggregate with a filter.
	keys := make(map[string]bool, len(segVals)+len(segFailed))
	for key := range segVals {
		keys[key] = true
	}
	for key := range segFailed {
		keys[key] = true
	}
	for key := range keys {
		vals := segVals[key]
		failed := segFailed[key]
		// Never write a row for a segment nobody drove. Without this guard
		// the job emits one row per scene segment per day forever.
		if len(vals) == 0 && failed == 0 {
			continue
		}
		row := SegmentDaily{
			Day:                from,
			Samples:            len(vals),
			Robots:             len(segRobots[key]),
			RelocFailedSamples: failed,
			RelocFailedRobots:  len(segFailedRobots[key]),
		}
		row.Area, row.Instance = SplitKey(key)
		if len(vals) > 0 {
			mean, p05, minConf := Mean(vals), Percentile(vals, 0.05), Min(vals)
			row.Mean, row.P05, row.MinConf = &mean, &p05, &minConf
		} else {
			// Failures only. The three measures stay NULL — there is no valid
			// reading to average, and 0.0 here would read as a catastrophic
			// measurement rather than an absent one.
			res.SegmentsFailOnly++
		}
		if err := UpsertSegmentDaily(db, row); err != nil {
			return res, err
		}
		res.SegmentRows++
	}
	return res, nil
}

// resolve converts the running accumulators to finished CellStats.
func resolve(in map[string]map[string]*accum) map[string]map[string]CellStat {
	out := make(map[string]map[string]CellStat, len(in))
	for robot, segs := range in {
		m := make(map[string]CellStat, len(segs))
		for seg, a := range segs {
			m[seg] = a.stat()
		}
		out[robot] = m
	}
	return out
}

// String renders the result as the job's one-line summary.
func (r RollUpResult) String() string {
	return fmt.Sprintf(
		"day=%s robots=%d segments=%d samples=%d orphans=%d residuals_null=%d fail_only_segments=%d",
		r.Day.Format("2006-01-02"), r.RobotRows, r.SegmentRows,
		r.SamplesRead, r.Orphans, r.ResidualsNull, r.SegmentsFailOnly)
}
