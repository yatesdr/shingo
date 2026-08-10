package robotconfidence

import (
	"database/sql"
	"fmt"
	"sort"
	"time"

	"shingocore/scenemap"
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
	//
	// REQUIRED. version_id is NOT NULL, so a roll-up without a resolver would
	// quarantine every sample and silently write nothing at all — a total loss
	// of a day, reported as success — and RollUp rejects that rather than
	// degrading into it.
	//
	// This comment used to say the opposite: that nil meant "every row is
	// written with a NULL version, the honest answer". That described the
	// model the primary-key change replaced, and it sat eight lines above the
	// code that now makes nil an error. A field's own documentation is the
	// first thing a caller reads.
	Versions VersionResolver
	// AreaClasses labels each zone row with the kind of zone it describes.
	//
	// OPTIONAL, unlike Versions, and the asymmetry is deliberate. A missing
	// version resolver loses a whole day; a missing class resolver costs one
	// descriptive column while the zone statistics stay correct. Refusing to
	// write a day's measurements to protect a label would be the wrong trade.
	AreaClasses AreaClassResolver
}

// RollUpResult reports what the job did, for the caller's log line.
type RollUpResult struct {
	Day       time.Time
	RobotRows int
	// LaneRows and LanesFailOnly were SegmentRows and SegmentsFailOnly. The
	// rows they count have been per-LANE since the aggregate stopped keying on
	// the directed segment; the names were the last thing still saying
	// "segment", and a field name that describes the previous grain is how a
	// reader re-derives the wrong denominator.
	LaneRows    int
	AreaRows    int
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
	// version opens at the beginning of time.
	//
	// (Stored as a year-1 timestamp, NOT Postgres '-infinity', which says the
	// bound exactly and cannot be scanned back into a time.Time through pgx's
	// database/sql shim. Three comments in this package used to say
	// "-infinity" flatly, so a reader grepping the DDL for it found nothing.
	// See sceneversion.beginningOfTime.)
	UnversionedSamples int
	// UnattributedSamples counts readings held out for a reloc_status that is
	// neither SUCCESS nor FAILED — state 3 is stored at both plants.
	//
	// IT USED TO BE COUNTED NOWHERE. The roll-up returned early on it, so a
	// sample that was read, snapped cleanly, taken on the right map and on a
	// versioned lane could still vanish from every total. The comment above
	// the branch grouped states 0 and 3 as "feed no statistic", but 0 reaches
	// a durable column and 3 reached nothing at all — a silent exclusion
	// inside the code written to remove silent exclusions.
	UnattributedSamples int
	ResidualsNull       int
	LanesFailOnly       int
	// LaneRobotRows counts per-lane-per-robot rows. Same lifetime as
	// LaneRows (kept forever); same pass, just keyed by vehicle too. See
	// lane_robot_confidence_daily.
	LaneRobotRows int
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
	// Zone accumulators. Keyed on the NORMALISED area id, so the robot "8" and
	// the map "08" are one zone rather than two that never join.
	areaAll := map[string][]float64{}
	areaGood := map[string][]float64{}
	areaRobots := map[string]map[string]bool{}
	areaSentinel := map[string]int{}
	areaSentinelRobots := map[string]map[string]bool{}
	// The distributions. Built alongside the raw slices rather than derived
	// from them afterwards, so the histogram sees exactly the population the
	// percentiles are taken over -- deriving it later is how the two drift.
	laneHist := map[string]*Hist{}
	areaHist := map[string]*Hist{}
	// Per-lane-per-robot grain, for the map's ?robot= filter. Keyed by the
	// lane's versioned key + vehicle_id, so it is a finer cut of the SAME
	// conditioned population laneHist accumulates over (orphan/unkeyable/
	// unversioned samples have already returned above). The snap already ran
	// for the lane; this reuses its result and adds map entries, nothing more.
	laneRobotAll := map[string][]float64{}
	laneRobotGood := map[string][]float64{}
	laneRobotHist := map[string]*Hist{}
	laneRobotSent := map[string]int{}
	laneRobotKey := map[string]string{} // composite key -> lane versioned key (for splitKey later)
	laneRobotVeh := map[string]string{} // composite key -> vehicle_id

	err = ScanSamples(db, from, to, func(s RawSample) {
		res.SamplesRead++
		robotSeen[s.VehicleID] = true

		seg, onPath := index.Nearest(s.X, s.Y, cfg.SnapTolerance)
		if !onPath {
			res.Orphans++
		}

		// -- gates that disqualify the READING, not just its lane -----------
		//
		// THE ORDER CHANGED, AND THE ORDER IS THE ARGUMENT. The lane
		// quarantines used to sit above everything, so a sample whose LANE
		// could not be named was dropped from the robot figure and from its
		// zones too. But "which lane was this on" and "how well is this robot
		// localizing" are different questions, and only the first is
		// unanswerable here. A reading on an unkeyable edge is still a real
		// reading, by a real robot, inside a real zone.
		//
		// A foreign-map reading is the exception and stays above everything:
		// it is not a reading of this plant at all, so nothing below may count
		// it.
		//
		// QUARANTINE, NOT EXCLUDE. The count is recorded and no statistic is
		// computed from it. An EMPTY hash is never a mismatch: those rows
		// predate v79 and do not know their map.
		if fleetMap != "" && s.MapMD5 != "" && s.MapMD5 != fleetMap {
			res.MapMismatch++
			robotMismatch[s.VehicleID]++
			if onPath && seg.Keyable() {
				if v := versions.At(seg.Area, seg.Lane(), s.SampledAt); v != nil {
					laneMismatch[versionedKey(seg.Key(), v)]++
				}
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
			if onPath && seg.Keyable() {
				if v := versions.At(seg.Area, seg.Lane(), s.SampledAt); v != nil {
					key := versionedKey(seg.Key(), v)
					segFailed[key]++
					if segFailedRobots[key] == nil {
						segFailedRobots[key] = map[string]bool{}
					}
					segFailedRobots[key][s.VehicleID] = true
				}
			}
			return
		}
		if s.RelocStatus != 1 {
			// COMPLETED: settled but unconfirmed. Held out of every statistic
			// and now COUNTED, which it was not before -- see
			// UnattributedSamples.
			res.UnattributedSamples++
			return
		}

		// -- the robot own figure -------------------------------------------
		//
		// Over readings it actually produced, so a no-estimate is excluded
		// here rather than counted as zero: this is a figure ABOUT THE ROBOT,
		// and a robot is not degraded by driving through a zone the plant
		// cannot localize in. The lane and zone statistics below make the
		// opposite choice for the opposite reason.
		//
		// Orphans stay in, and so now do readings whose lane could not be
		// keyed or versioned: this is descriptive of everything that robot
		// reported, and dropping readings because Core could not name the
		// floor under them would quietly reshape it.
		if !s.NoEstimate() {
			robotVals[s.VehicleID] = append(robotVals[s.VehicleID], s.Confidence)
		}

		// -- zone attribution: one reading, many zones ----------------------
		//
		// INDEPENDENT OF THE LANE, deliberately. A zone is not a lane: a
		// reading that snapped to nothing, or to an edge Core cannot name,
		// still happened somewhere the robot could name. Gating zones on the
		// lane would lose exactly the readings a dead zone produces most of.
		//
		// Membership is the ROBOT's own, from rbk_report.area_ids, normalised
		// because the robot says "8" and the map says "08" -- a join on the
		// literal strings returns no rows and no error, a quiet zero exactly
		// where the finding should be.
		for _, raw := range s.AreaIDs {
			id := scenemap.NormalizeAreaID(raw)
			if id == "" {
				continue
			}
			if areaRobots[id] == nil {
				areaRobots[id] = map[string]bool{}
			}
			areaRobots[id][s.VehicleID] = true
			if areaHist[id] == nil {
				areaHist[id] = &Hist{}
			}
			areaHist[id].Add(s.Confidence)
			if s.NoEstimate() {
				areaAll[id] = append(areaAll[id], 0)
				areaSentinel[id]++
				if areaSentinelRobots[id] == nil {
					areaSentinelRobots[id] = map[string]bool{}
				}
				areaSentinelRobots[id][s.VehicleID] = true
				continue
			}
			areaAll[id] = append(areaAll[id], s.Confidence)
			areaGood[id] = append(areaGood[id], s.Confidence)
		}

		// -- lane attribution ------------------------------------------------
		if !onPath {
			return
		}
		// A reading that landed on an edge with no endpoint names has no lane
		// to be attributed to. Counted and held out of the LANE statistics,
		// never guessed at: keying it on the directed instance name would put
		// it in the same table at a different granularity.
		if !seg.Keyable() {
			res.UnkeyableSamples++
			return
		}
		// The key carries the geometry version, so an edit at 14:00 splits the
		// day into one row per geometry instead of averaging across it.
		versionID := versions.At(seg.Area, seg.Lane(), s.SampledAt)
		if versionID == nil {
			// A lane with no version at all cannot be attributed to a
			// geometry, and inventing one would be the same defect as keying
			// an unnameable edge on its directed name.
			res.UnversionedSamples++
			return
		}
		key := versionedKey(seg.Key(), versionID)

		if laneRobots[key] == nil {
			laneRobots[key] = map[string]bool{}
		}
		laneRobots[key][s.VehicleID] = true
		if laneHist[key] == nil {
			laneHist[key] = &Hist{}
		}
		laneHist[key].Add(s.Confidence)

		// The per-lane-per-robot grain: same accumulation as the lane, keyed by
		// the lane's versioned key + vehicle. The composite key is built once
		// here and used for the all/good/hist/sentinel maps below.
		rkey := key + "\x03" + s.VehicleID
		laneRobotKey[rkey] = key
		laneRobotVeh[rkey] = s.VehicleID
		if laneRobotHist[rkey] == nil {
			laneRobotHist[rkey] = &Hist{}
		}
		laneRobotHist[rkey].Add(s.Confidence)

		// THE FULL POPULATION, WITH A MISS COUNTED AS THE ZERO IT IS. This is
		// the statistic the map bands, and it is only bandable because it is
		// unconditioned: a lane that returns 0.98 half the time and nothing
		// the rest reads as 0.49 here and as 0.98 in mean_good, and the
		// difference between those two columns is the whole finding.
		if s.NoEstimate() {
			laneAll[key] = append(laneAll[key], 0)
			laneSentinel[key]++
			if laneSentinelRobots[key] == nil {
				laneSentinelRobots[key] = map[string]bool{}
			}
			laneSentinelRobots[key][s.VehicleID] = true
			laneRobotAll[rkey] = append(laneRobotAll[rkey], 0)
			laneRobotSent[rkey]++
			return
		}
		laneAll[key] = append(laneAll[key], s.Confidence)
		laneGood[key] = append(laneGood[key], s.Confidence)
		laneRobotAll[rkey] = append(laneRobotAll[rkey], s.Confidence)
		laneRobotGood[rkey] = append(laneRobotGood[rkey], s.Confidence)

		// The residual compares robots to each other at the same place, so it
		// reads good ticks only -- a miss is a property of the floor and would
		// otherwise be charged to whichever robot happened to drive there.
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
		if h := laneHist[key]; h != nil {
			row.ConfHist = *h
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
			res.LanesFailOnly++
		}
		if err := UpsertLaneDaily(db, row); err != nil {
			return res, err
		}
		res.LaneRows++
	}

	// -- Write the lane-per-robot rows ----------------------------------------
	//
	// SAME SAMPLES, DIFFERENT DENOMINATOR. The lane row above answers "how is
	// this lane"; this row answers "how is this lane AS THIS ROBOT SEES IT",
	// which is the view the map switches to when an operator picks one AMR.
	// The lane is the union of its robots and the robot is the union of its
	// lanes, and neither union recovers the intersection — so this grain is
	// its own row, written on the same single pass, not a GROUP BY in SQL.
	for rkey := range laneRobotKey {
		all := laneRobotAll[rkey]
		good := laneRobotGood[rkey]
		laneKey, versionID := splitVersionedKey(laneRobotKey[rkey])
		area, lane := SplitKey(laneKey)
		row := LaneRobotDaily{
			Day:             from,
			Area:            area,
			Lane:            lane,
			VehicleID:       laneRobotVeh[rkey],
			Samples:         len(all),
			SamplesGood:     len(good),
			SentinelSamples: laneRobotSent[rkey],
			VersionID:       versionID,
		}
		if h := laneRobotHist[rkey]; h != nil {
			row.ConfHist = *h
		}
		if len(all) > 0 {
			p05, p25, p50 := Percentile(all, 0.05), Percentile(all, 0.25), Percentile(all, 0.50)
			p75, p95 := Percentile(all, 0.75), Percentile(all, 0.95)
			row.P05, row.P25, row.P50, row.P75, row.P95 = &p05, &p25, &p50, &p75, &p95
		}
		if len(good) > 0 {
			mean, minConf := Mean(good), Min(good)
			row.MeanGood, row.MinConf = &mean, &minConf
		}
		if err := UpsertLaneRobotDaily(db, row); err != nil {
			return res, err
		}
		res.LaneRobotRows++
	}

	// -- Write the zone rows ------------------------------------------------
	//
	// ONE READING CAN BE IN SEVERAL ZONES, so these rows do not partition the
	// day and their `samples` does not sum to SamplesRead. SEER areas overlap
	// by design; the robot reports membership as a list and this preserves it.
	//
	// The class label is resolved AT THE DAY being rolled up, not at now. A
	// zone re-declared between then and now describes a different thing, and
	// stamping last Tuesday with today's class is the defect the lane
	// versioning exists to prevent, one table over.
	var classes map[string]string
	if cfg.AreaClasses != nil {
		var cerr error
		classes, cerr = cfg.AreaClasses.ClassesAt(db, from)
		if cerr != nil {
			// Logged by the caller through the returned error only if fatal;
			// here it is not. A missing label must not cost the measurement.
			classes = nil
		}
	}
	for id, all := range areaAll {
		good := areaGood[id]
		row := AreaDaily{
			Day:             from,
			AreaName:        id,
			ClassName:       classes[id],
			Samples:         len(all),
			SamplesGood:     len(good),
			Robots:          len(areaRobots[id]),
			RobotsSeen:      sortedKeys(areaRobots[id]),
			SentinelSamples: areaSentinel[id],
			SentinelRobots:  len(areaSentinelRobots[id]),
		}
		if h := areaHist[id]; h != nil {
			row.ConfHist = *h
		}
		p05, p25, p50 := Percentile(all, 0.05), Percentile(all, 0.25), Percentile(all, 0.50)
		p75, p95 := Percentile(all, 0.75), Percentile(all, 0.95)
		row.P05, row.P25, row.P50, row.P75, row.P95 = &p05, &p25, &p50, &p75, &p95
		if len(good) > 0 {
			mean, minConf := Mean(good), Min(good)
			row.MeanGood, row.MinConf = &mean, &minConf
		}
		// len(good) == 0 leaves MeanGood and MinConf NULL rather than 0.0: a
		// zone where every reading was a no-estimate has no valid reading to
		// average, and 0.0 would read as a catastrophic measurement rather
		// than an absent one. That zone is the most important one on the map.
		if err := UpsertAreaDaily(db, row); err != nil {
			return res, err
		}
		res.AreaRows++
	}

	// -- Write the plant row ------------------------------------------------
	//
	// LAST, because it records what the rest of this function did. Every count
	// here reached a log line and nothing else before this table existed, and
	// the raw samples behind them expire at 14 days -- so a fortnight after any
	// interesting day, "was the plant like this" stopped being answerable.
	if err := UpsertPlantDaily(db, PlantDaily{
		Day:                 from,
		SamplesRead:         res.SamplesRead,
		OrphanSamples:       res.Orphans,
		UnkeyableEdges:      res.UnkeyableEdges,
		UnkeyableSamples:    res.UnkeyableSamples,
		UnversionedSamples:  res.UnversionedSamples,
		MapMismatchSamples:  res.MapMismatch,
		UnattributedSamples: res.UnattributedSamples,
		RobotRows:           res.RobotRows,
		LaneRows:            res.LaneRows,
		AreaRows:            res.AreaRows,
		LaneRobotRows:       res.LaneRobotRows,
		ResidualsNull:       res.ResidualsNull,
		LanesFailOnly:       res.LanesFailOnly,
	}); err != nil {
		return res, err
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
		"day=%s robots=%d lanes=%d lane_robot=%d areas=%d samples=%d orphans=%d map_mismatch=%d "+
			"unkeyable_edges=%d unkeyable_samples=%d unversioned_samples=%d "+
			"unattributed_samples=%d residuals_null=%d fail_only_lanes=%d",
		r.Day.Format("2006-01-02"), r.RobotRows, r.LaneRows, r.LaneRobotRows, r.AreaRows,
		r.SamplesRead, r.Orphans, r.MapMismatch,
		r.UnkeyableEdges, r.UnkeyableSamples, r.UnversionedSamples,
		r.UnattributedSamples, r.ResidualsNull, r.LanesFailOnly)
}
