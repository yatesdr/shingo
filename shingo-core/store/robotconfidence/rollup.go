package robotconfidence

import (
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// The nightly roll-up. Raw samples expire; these aggregates do not, and they
// cannot be rebuilt once the rows behind them are gone — which is why the job
// ships with the collection rather than after it.

// RollUpConfig carries the tunables the job reads. Coverage is separate
// because it defines the statistic rather than tuning the deployment; see
// robotconfidence.go.
type RollUpConfig struct {
	// SnapTolerance is how far (metres) a sample may sit from a lane and
	// still be considered on it.
	//
	// 1.0 m, and it is now a MEASURED choice rather than an allowance for a
	// bad snap. The comment that used to sit here said scene_edges "keeps
	// only segment endpoints" and used that to justify a generous tolerance;
	// the handles were in the table all along and the snap now runs against
	// the real curve. Against it, the worst observed distance across 11,543
	// Springfield samples is 0.877 m and p99 is 0.322 m, with NOTHING at all
	// between 1 m and 5 m — so 1.0 m admits every real reading with headroom
	// and widening it buys nothing while starting to admit genuinely
	// off-network positions. Tightening to 0.5 m would drop 0.41% of
	// samples, and those are junction- and curve-adjacent, i.e.
	// systematically the interesting ones.
	SnapTolerance float64
	// BaselineDays is the trailing window the fleet median is computed over.
	BaselineDays int
	Coverage     Coverage
	// Versions resolves which geometry a lane had when a sample was taken.
	// Nil means no scene versioning is available, and every row is written
	// with a NULL version — the honest answer, and what every day of raw
	// collected before the versioning landed genuinely is.
	Versions VersionResolver
}

// RollUpResult reports what the job did, for the caller's log line.
type RollUpResult struct {
	Day         time.Time
	RobotRows   int
	SegmentRows int
	SamplesRead int
	Orphans     int
	// MapMismatch counts samples quarantined for having been taken on a
	// different map than the fleet's majority. Logged on every run because
	// a non-zero value here means a robot is out of step with the scene the
	// numbers are computed against, and nothing else in the system can see
	// that.
	MapMismatch int
	// UnkeyableEdges counts scene edges that carry no endpoint names and so
	// cannot be aggregated onto a physical lane; UnkeyableSamples counts the
	// readings that landed on one. Both, because one alone is ambiguous:
	// four defective edges nobody drives is a housekeeping note, four that
	// carry a shift's traffic is a hole in the data. Non-zero means scene
	// sync wrote a row it could not name — see Segment.Keyable.
	UnkeyableEdges   int
	UnkeyableSamples int
	// UnversionedSamples counts readings on a lane that has no geometry
	// version at all. Non-zero means the scene sync has never run since
	// versioning landed — a defect to surface, not a hole to carry in a key.
	// A lane that HAS been versioned covers every reading, because its first
	// version opens at -infinity.
	UnversionedSamples int
	ResidualsNull      int
	SegmentsFailOnly   int
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
	// Unkeyable edges stay in the index on purpose — see Segment.Keyable.
	// Dropping them would send their samples onto whichever neighbour is
	// within tolerance, which at this plant is a near-certainty rather than
	// a risk. Counted here so the defect is a number in the log rather than
	// an absence somebody has to notice.
	for _, s := range segments {
		if !s.Keyable() {
			res.UnkeyableEdges++
		}
	}
	index := NewSegmentIndex(segments)

	from := dayKey(day)
	to := from.AddDate(0, 0, 1)

	// ── Pass 1: the day being rolled up ────────────────────────────────────
	// dayRC and the lane statistics take reloc_status = 1 ONLY. States 0 and
	// 3 are stored and queryable but feed no statistic: a robot sitting in
	// FAILED at a charge point would otherwise drag that location's baseline
	// down for every healthy robot passing through, corrupting the residual
	// for everyone else — the exact confound this design exists to remove.
	//
	// The fleet's majority map for this day. Anything else is quarantined.
	//
	// THIS IS NOT HYPOTHETICAL. Measured at Hopkinsville 2026-08-06: eleven
	// robots on Hop_20 and AMR-11, connected, on Hop_21 — with RDS itself
	// reporting current_map_invalid and holding it undispatchable. Its
	// samples were being stored and snapped against scene_edges built from
	// the majority map: a real reading of a real place, attributed to the
	// wrong floor.
	//
	// Majority-of-the-day is the v1 binding because it needs nothing but the
	// column. Its known limit is a whole-fleet migration mid-day, where the
	// minority half is quarantined even though every robot is legitimately
	// on its own map; that is the conservative direction, and the map sync
	// replaces this with the scene's own recorded hash.
	fleetMap, err := FleetMapMode(db, from, to)
	if err != nil {
		return res, err
	}

	// The version index resolves, per sample, WHICH geometry the lane had
	// when the reading was taken. Loaded once for the window rather than
	// queried per row.
	// A MISSING RESOLVER IS AN ERROR, NOT A DEGRADED MODE. version_id is NOT
	// NULL, so a roll-up that cannot resolve versions would quarantine every
	// sample it read and write nothing at all — a total, silent loss of a
	// day, reported as success. Fail where the misconfiguration is.
	if cfg.Versions == nil {
		return res, fmt.Errorf(
			"robot confidence: roll-up has no version resolver; every sample " +
				"would be quarantined and the day would silently produce no rows")
	}
	versions, err := cfg.Versions.Load(db, from, to)
	if err != nil {
		return res, err
	}

	dayRC := map[string]map[string]*accum{} // robot -> lane -> accum (good ticks)
	laneAll := map[string][]float64{}       // lane -> every tick, misses as 0
	laneGood := map[string][]float64{}      // lane -> ticks that produced a number
	laneRobots := map[string]map[string]bool{}
	laneSentinel := map[string]int{}
	laneSentinelRobots := map[string]map[string]bool{}
	laneMismatch := map[string]int{}
	segFailed := map[string]int{} // lane -> reloc_status = 0 count
	segFailedRobots := map[string]map[string]bool{}
	robotVals := map[string][]float64{}
	robotSeen := map[string]bool{}
	robotMismatch := map[string]int{}

	err = ScanSamples(db, from, to, func(s RawSample) {
		res.SamplesRead++
		robotSeen[s.VehicleID] = true

		seg, onPath := index.Nearest(s.X, s.Y, cfg.SnapTolerance)
		if !onPath {
			res.Orphans++
		}
		// A reading that landed on an edge with no endpoint names has no
		// lane to be attributed to. Counted and dropped, never guessed at:
		// keying it on the directed instance name would put it in the same
		// table at a different granularity, which is the failure this whole
		// commit is about.
		if onPath && !seg.Keyable() {
			res.UnkeyableSamples++
			return
		}
		// The key carries the geometry version, so an edit at 14:00 splits
		// the day into one row per geometry instead of averaging across it.
		// A blend of two lanes presented as one measurement is undetectable
		// by any reader, and at a daily edit cadence it is most days.
		versionID := versions.At(seg.Area, seg.Lane(), s.SampledAt)
		// A lane with no version at all cannot be attributed to a geometry,
		// and inventing one would be the same defect as keying an unnameable
		// edge on its directed name. Counted and held out — the fix is to
		// make the scene sync run, not to widen the key.
		if onPath && versionID == nil {
			res.UnversionedSamples++
			return
		}
		key := versionedKey(seg.Key(), versionID)

		// QUARANTINE, NOT EXCLUDE. The row stays, the count is recorded, and
		// nothing about the lane's statistics is computed from it. Dropping
		// it quietly would trade a silent corruption for a silent omission,
		// and a lane that suddenly reads on a third of its usual n with no
		// explanation is the harder of the two to notice.
		//
		// An EMPTY hash is never a mismatch: those rows predate v79 and do
		// not know their map. Treating "not collected" as "wrong map" would
		// quarantine every historical row on the first run.
		if fleetMap != "" && s.MapMD5 != "" && s.MapMD5 != fleetMap {
			res.MapMismatch++
			robotMismatch[s.VehicleID]++
			if onPath {
				laneMismatch[key]++
			}
			return
		}

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

		// The robot's own mean is over readings it actually produced, so a
		// no-estimate is excluded here rather than counted as zero: this is
		// a figure ABOUT THE ROBOT, and a robot is not degraded by driving
		// through a zone the plant cannot localize in. The lane statistic
		// below makes the opposite choice for the opposite reason.
		//
		// Orphans stay in: it is descriptive of everything that robot
		// reported, and dropping readings because they failed to snap would
		// quietly reshape it.
		if !s.NoEstimate() {
			robotVals[s.VehicleID] = append(robotVals[s.VehicleID], s.Confidence)
		}
		if !onPath {
			return
		}

		if laneRobots[key] == nil {
			laneRobots[key] = map[string]bool{}
		}
		laneRobots[key][s.VehicleID] = true

		// THE FULL POPULATION, WITH A MISS COUNTED AS THE ZERO IT IS. This
		// is the statistic the map bands, and it is only bandable because
		// it is unconditioned: a lane that returns 0.98 half the time and
		// nothing the rest reads as 0.49 here and as 0.98 in mean_good, and
		// the difference between those two columns is the whole finding.
		if s.NoEstimate() {
			laneAll[key] = append(laneAll[key], 0)
			laneSentinel[key]++
			if laneSentinelRobots[key] == nil {
				laneSentinelRobots[key] = map[string]bool{}
			}
			laneSentinelRobots[key][s.VehicleID] = true
			return
		}
		laneAll[key] = append(laneAll[key], s.Confidence)
		laneGood[key] = append(laneGood[key], s.Confidence)

		// The residual compares robots to each other at the same place, so
		// it reads good ticks only — a miss is a property of the floor and
		// would otherwise be charged to whichever robot happened to drive
		// there.
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
		if s.RelocStatus != 1 || s.NoEstimate() {
			return
		}
		if fleetMap != "" && s.MapMD5 != "" && s.MapMD5 != fleetMap {
			return
		}
		seg, onPath := index.Nearest(s.X, s.Y, cfg.SnapTolerance)
		if !onPath || !seg.Keyable() {
			return
		}
		// The baseline deliberately does NOT split by version: it is the
		// fleet's typical reading at a place over fourteen days, and a lane
		// that was edited mid-window still describes one piece of floor for
		// the purpose of comparing robots to each other on it.
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
		row := RobotDaily{Day: from, VehicleID: robot, MapMismatchSamples: robotMismatch[robot]}

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
	keys := make(map[string]bool, len(laneAll)+len(segFailed))
	for _, m := range []map[string]int{segFailed, laneMismatch} {
		for key := range m {
			keys[key] = true
		}
	}
	for key := range laneAll {
		keys[key] = true
	}
	for key := range keys {
		all := laneAll[key]
		good := laneGood[key]
		failed := segFailed[key]
		// Never write a row for a lane nobody drove. Without this guard the
		// job emits one row per scene lane per day forever — at the 5x map
		// that is thousands of empty rows a day, and "no row" is the right
		// way to say "not driven" anyway.
		if len(all) == 0 && failed == 0 && laneMismatch[key] == 0 {
			continue
		}
		row := LaneDaily{
			Day:                from,
			Samples:            len(all),
			SamplesGood:        len(good),
			Robots:             len(laneRobots[key]),
			RobotsSeen:         sortedKeys(laneRobots[key]),
			SentinelSamples:    laneSentinel[key],
			SentinelRobots:     len(laneSentinelRobots[key]),
			RelocFailedSamples: failed,
			RelocFailedRobots:  len(segFailedRobots[key]),
			MapMismatchSamples: laneMismatch[key],
		}
		laneKey, versionID := splitVersionedKey(key)
		row.Area, row.Lane = SplitKey(laneKey)
		row.VersionID = versionID
		if len(all) > 0 {
			p05, p25, p50 := Percentile(all, 0.05), Percentile(all, 0.25), Percentile(all, 0.50)
			p75, p95 := Percentile(all, 0.75), Percentile(all, 0.95)
			row.P05, row.P25, row.P50, row.P75, row.P95 = &p05, &p25, &p50, &p75, &p95
		}
		if len(good) > 0 {
			mean, minConf := Mean(good), Min(good)
			row.MeanGood, row.MinConf = &mean, &minConf
		}
		if len(good) == 0 {
			// Nothing to average. Every measure over the good population
			// stays NULL — there is no valid reading, and 0.0 here would
			// read as a catastrophic measurement rather than an absent one.
			// The row is still written: this lane is the worst place on the
			// floor, not the least interesting, and an absent row reads as
			// fine to every human who sees it.
			res.SegmentsFailOnly++
		}
		if err := UpsertLaneDaily(db, row); err != nil {
			return res, err
		}
		res.SegmentRows++
	}
	return res, nil
}

// sortedKeys returns a set's members in a stable order.
//
// Sorted rather than map-iteration order so the stored array is
// deterministic: an upsert that rewrites the same day must produce the same
// row, or a diff of two roll-up runs is full of noise that is only ordering.
func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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
		"day=%s robots=%d lanes=%d samples=%d orphans=%d map_mismatch=%d "+
			"unkeyable_edges=%d unkeyable_samples=%d unversioned_samples=%d "+
			"residuals_null=%d fail_only_lanes=%d",
		r.Day.Format("2006-01-02"), r.RobotRows, r.SegmentRows,
		r.SamplesRead, r.Orphans, r.MapMismatch,
		r.UnkeyableEdges, r.UnkeyableSamples, r.UnversionedSamples,
		r.ResidualsNull, r.SegmentsFailOnly)
}
