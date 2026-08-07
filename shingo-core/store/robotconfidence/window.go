package robotconfidence

import (
	"database/sql"
	"fmt"
	"strconv"
	"time"
)

// The windowed read — what the board asks for, answered entirely from the
// permanent record.
//
// NO RAW, NO SNAP, NO RE-ATTRIBUTION. The daily roll-up already did the
// expensive work: it snapped every sample to a lane and resolved the geometry
// version it was taken on. This sums what it wrote. A window is therefore a
// grouped query over an aggregate table rather than a batch job triggered by a
// page load, and 30 d and since-last-change become answerable at all rather
// than merely specified — raw is retained fourteen days.

// LaneWindow is one lane's distribution over a window.
type LaneWindow struct {
	Area string
	Lane string
	// Hist is the element-wise sum of the daily histograms. Percentiles come
	// off this; see Hist.PercentileEstimate for why they are estimates and the
	// daily ones are not.
	Hist Hist
	// Samples and SamplesGood are exact — counts DO re-aggregate, unlike the
	// percentiles that summarise them.
	Samples         int
	SamplesGood     int
	SentinelSamples int
	// Days is how many daily rows this window actually found. It is the
	// difference between "the plant was fine all month" and "we have two days
	// of data and a thirty-day label", which is the failure this whole design
	// exists to remove -- so it travels with every window rather than being
	// recoverable from a second query nobody makes.
	Days int
	// Versions is how many distinct geometry versions the window spans. More
	// than one means the lane was EDITED inside it, so a single number over
	// the whole window is averaging across a change -- exactly the blend the
	// version key was added to prevent. The caller decides what to do about
	// it; it must not be invisible.
	Versions int
	// HistIncomplete is true when any day in the window carried no histogram
	// -- a row written before the column existed. The counts are still right;
	// the percentiles are computed over less than the window claims, and
	// saying so is cheaper than being subtly wrong.
	HistIncomplete bool
}

// PercentileEstimate is the window's p-th percentile over every tick, counting
// a no-estimate as the zero it is. Estimate, not measurement — see Hist.
func (w LaneWindow) PercentileEstimate(p float64) (float64, bool) {
	return w.Hist.PercentileEstimate(p)
}

// LaneWindows sums lane_confidence_daily over [from, to) — day-grained, so
// `to` is exclusive and a caller asking for "the last 7 days" passes tomorrow.
//
// AGGREGATED IN GO, NOT IN SQL, and the reason is the histogram: Postgres has
// no element-wise array sum without an extension or a hand-rolled aggregate,
// and a hand-rolled one would be a second implementation of the merge that
// could disagree with Hist.Merge. One merge, in one language, used by both the
// daily path and the windowed path.
//
// The row count is bounded by lanes x days — 212 x 30 at Springfield, ~1060 x
// 30 at the 5x map — so this is tens of thousands of rows, not the millions the
// raw table holds. That difference is the whole point.
func LaneWindows(db *sql.DB, from, to time.Time) (map[string]*LaneWindow, error) {
	rows, err := db.Query(
		`SELECT day, area_name, lane, version_id, samples, samples_good,
		        sentinel_samples, coalesce(conf_hist, '{}')
		   FROM lane_confidence_daily
		  WHERE day >= $1 AND day < $2`, from, to)
	if err != nil {
		return nil, fmt.Errorf("lane windows: %w", err)
	}
	defer rows.Close()

	out := map[string]*LaneWindow{}
	versions := map[string]map[int64]bool{}
	days := map[string]map[string]bool{}
	for rows.Next() {
		var day time.Time
		var area, lane, hist string
		var versionID sql.NullInt64
		var samples, good, sentinel int
		if err := rows.Scan(&day, &area, &lane, &versionID, &samples, &good,
			&sentinel, &hist); err != nil {
			return nil, err
		}
		key := area + "\x00" + lane
		w := out[key]
		if w == nil {
			w = &LaneWindow{Area: area, Lane: lane}
			out[key] = w
			versions[key] = map[int64]bool{}
			days[key] = map[string]bool{}
		}
		w.Samples += samples
		w.SamplesGood += good
		w.SentinelSamples += sentinel
		days[key][day.Format("2006-01-02")] = true
		if versionID.Valid {
			versions[key][versionID.Int64] = true
		}
		if h, ok := HistFromSlice(parsePGInt32Array(hist)); ok {
			w.Hist.Merge(h)
		} else {
			// A row with no histogram: written before the column existed, or
			// stored at a different length. Counted rather than padded — a
			// padded histogram answers confidently from a distribution nobody
			// stored.
			w.HistIncomplete = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Days is counted per (lane, day) rather than as a row count, because the
	// version key means one lane can have SEVERAL rows on a day it was edited.
	// Counting rows would report a lane edited twice on Tuesday as three days
	// of data.
	for key, w := range out {
		w.Versions = len(versions[key])
		w.Days = len(days[key])
	}
	return out, nil
}

// parsePGInt32Array decodes Postgres's INTEGER[] output form.
//
// Same driver limitation as parsePGTextArray and the same reason it exists:
// pgx's database/sql shim binds a slice IN and cannot scan one back OUT, so
// the value arrives as the literal "{0,3,11}". Integer arrays need no quoting,
// so this reuses the text parser and converts — one array-literal parser, not
// two that can disagree about the edge cases.
//
// An unparseable element yields a nil result rather than a partial array: a
// histogram missing one bin still sums, still renders, and is wrong in a way
// nothing can see. Absent is the honest answer.
func parsePGInt32Array(s string) []int32 {
	parts := parsePGTextArray(s)
	if parts == nil {
		return nil
	}
	out := make([]int32, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.ParseInt(p, 10, 32)
		if err != nil {
			return nil
		}
		out = append(out, int32(n))
	}
	return out
}

// AreaWindow is one declared zone's distribution over a window.
//
// SEPARATE FROM THE POLYGONS ON PURPOSE. A zone's STATISTICS come from the
// roll-up, which attributes readings by the id the robot reports; its SHAPE
// comes from the .smap. Those arrive on different transports with different
// gates, so a zone can have numbers and no outline, or an outline and no
// numbers, and both are real states a reader needs to tell apart.
//
// The first is not hypothetical: it is every plant between the confidence
// collection starting and the first successful map fetch. Keying the zone
// panel to geometry would leave those numbers written and unread — which is
// the defect this whole line of work exists to remove.
type AreaWindow struct {
	Name            string
	Hist            Hist
	Samples         int
	SamplesGood     int
	SentinelSamples int
	Robots          int
	Days            int
	// Class is the last non-empty class seen across the window's days. Empty
	// when the map sync has never run, which is a real state and different
	// from a zone with no class.
	Class          string
	HistIncomplete bool
}

// PercentileEstimate is the zone's p-th percentile over every tick, counting a
// no-estimate as the zero it is. Estimate, not measurement -- see Hist.
func (w AreaWindow) PercentileEstimate(p float64) (float64, bool) {
	return w.Hist.PercentileEstimate(p)
}

// AreaWindows sums area_confidence_daily over [from, to).
//
// Same shape as LaneWindows and for the same reason: percentiles do not
// re-aggregate, histograms do, so a window is a read of the permanent record
// rather than a re-run of the roll-up.
func AreaWindows(db *sql.DB, from, to time.Time) (map[string]*AreaWindow, error) {
	rows, err := db.Query(
		`SELECT day, area_name, coalesce(class_name,''), samples, samples_good,
		        sentinel_samples, robots, coalesce(conf_hist, '{}')
		   FROM area_confidence_daily
		  WHERE day >= $1 AND day < $2`, from, to)
	if err != nil {
		return nil, fmt.Errorf("area windows: %w", err)
	}
	defer rows.Close()

	out := map[string]*AreaWindow{}
	days := map[string]map[string]bool{}
	robots := map[string]int{}
	for rows.Next() {
		var day time.Time
		var name, class, hist string
		var samples, good, sentinel, nrobots int
		if err := rows.Scan(&day, &name, &class, &samples, &good, &sentinel,
			&nrobots, &hist); err != nil {
			return nil, err
		}
		w := out[name]
		if w == nil {
			w = &AreaWindow{Name: name}
			out[name] = w
			days[name] = map[string]bool{}
		}
		w.Samples += samples
		w.SamplesGood += good
		w.SentinelSamples += sentinel
		if class != "" {
			w.Class = class
		}
		// The robot count is the WIDEST day rather than a sum: the same robot
		// driving a zone on Monday and Tuesday is one robot, and adding the
		// daily counts would report two.
		if nrobots > robots[name] {
			robots[name] = nrobots
		}
		days[name][day.Format("2006-01-02")] = true
		if h, ok := HistFromSlice(parsePGInt32Array(hist)); ok {
			w.Hist.Merge(h)
		} else {
			w.HistIncomplete = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for name, w := range out {
		w.Days = len(days[name])
		w.Robots = robots[name]
	}
	return out, nil
}

// LaneWindowBetween sums one lane's daily rows over an arbitrary day range.
//
// The change annotation needs the days BEFORE an edit and the days AFTER it,
// which is a different question from "the last 7 days" — the boundary is the
// edit, not the calendar. Same summing, narrower key.
func LaneWindowBetween(db *sql.DB, area, lane string, from, to time.Time) (*LaneWindow, error) {
	rows, err := db.Query(
		`SELECT day, samples, samples_good, sentinel_samples, robots,
		        coalesce(conf_hist, '{}')
		   FROM lane_confidence_daily
		  WHERE area_name = $1 AND lane = $2 AND day >= $3 AND day < $4`,
		area, lane, from, to)
	if err != nil {
		return nil, fmt.Errorf("lane window between: %w", err)
	}
	defer rows.Close()

	w := &LaneWindow{Area: area, Lane: lane}
	days := map[string]bool{}
	for rows.Next() {
		var day time.Time
		var hist string
		var samples, good, sentinel, nrobots int
		if err := rows.Scan(&day, &samples, &good, &sentinel, &nrobots, &hist); err != nil {
			return nil, err
		}
		_ = nrobots // robots_seen has no proportions, so the "5 of 6 in common"
		// clause the plan asks for cannot be built from it -- see the grain
		// decision in §9. Read and discarded rather than silently unselected.
		w.Samples += samples
		w.SamplesGood += good
		w.SentinelSamples += sentinel
		days[day.Format("2006-01-02")] = true
		if h, ok := HistFromSlice(parsePGInt32Array(hist)); ok {
			w.Hist.Merge(h)
		} else {
			w.HistIncomplete = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	w.Days = len(days)
	return w, nil
}

// PlantWindowBetween is the same sum over EVERY lane, which is the baseline the
// annotation is required to carry.
//
// WITHOUT IT A LANE TAKES CREDIT FOR THE WEATHER. A lane that rose 0.09 while
// the whole plant rose 0.08 has not improved by 0.09; it has improved by 0.01.
// Eleven characters on the face of the annotation defeat an entire class of
// false attribution, which is why the plan makes this mandatory rather than a
// toggle.
func PlantWindowBetween(db *sql.DB, from, to time.Time) (*LaneWindow, error) {
	rows, err := db.Query(
		`SELECT samples, samples_good, sentinel_samples, coalesce(conf_hist, '{}')
		   FROM lane_confidence_daily WHERE day >= $1 AND day < $2`, from, to)
	if err != nil {
		return nil, fmt.Errorf("plant window between: %w", err)
	}
	defer rows.Close()
	w := &LaneWindow{Area: "", Lane: "*"}
	for rows.Next() {
		var hist string
		var samples, good, sentinel int
		if err := rows.Scan(&samples, &good, &sentinel, &hist); err != nil {
			return nil, err
		}
		w.Samples += samples
		w.SamplesGood += good
		w.SentinelSamples += sentinel
		if h, ok := HistFromSlice(parsePGInt32Array(hist)); ok {
			w.Hist.Merge(h)
		} else {
			w.HistIncomplete = true
		}
	}
	return w, rows.Err()
}
