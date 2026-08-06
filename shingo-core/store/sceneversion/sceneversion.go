// Package sceneversion records what changed in the plant's scene and when.
//
// It turns a sync from an overwrite into an event: one scene_diffs row per
// observed edit, one closed-and-reopened version row per object that actually
// moved, and a magnitude on both so a reader can tell a 2 mm re-registration
// from a re-route without opening the map.
//
// THE ORDERING IS THE WHOLE DESIGN. scenesync mirrors RDS by deleting each
// area's edges and re-inserting them, so by the time the new rows land the
// old ones are gone and nothing can measure the distance between them. The
// diff therefore has to run BEFORE the replace — fetch, diff, write versions,
// then replace — and that is a property of the call site, not of this
// package. ApplyLaneDiff is written to be called with both states in hand.
package sceneversion

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"shingocore/scenemap"
)

// Source names the transport an edit was observed on. The two have different
// gates and different clocks; the diff row is what relates an edit seen on
// one to an edit seen on the other.
const (
	SourceRDSScene  = "rds_scene"
	SourceRobotSmap = "robot_smap"
)

// Lane is one physical lane's state at a moment.
type Lane struct {
	Area    string
	Lane    string
	Version scenemap.LaneVersion
	// Shape is the canonical-direction vertex list, so a later version can
	// measure how far this one moved.
	Shape []scenemap.Point
}

// DiffResult is what a sync did, for the log line and for the caller's own
// bookkeeping.
type DiffResult struct {
	DiffID  int64
	Added   int
	Changed int
	Removed int
	// MedianDelta and MaxDelta are over the CHANGED lanes only. Added and
	// removed lanes have no predecessor to measure from, and folding a
	// sentinel in for them would drag the median toward a number that
	// describes nothing.
	MedianDelta float64
	MaxDelta    float64
	// Disagreements counts lanes whose two directed rows stopped mirroring.
	// Never non-zero at Springfield so far; when it is, lane-grain versioning
	// is describing only part of the truth for those lanes.
	Disagreements int
}

// Changed reports whether anything actually moved.
func (r DiffResult) Changed_() bool { return r.Added+r.Changed+r.Removed > 0 }

func (r DiffResult) String() string {
	return fmt.Sprintf("diff=%d added=%d changed=%d removed=%d median_delta=%.4fm max_delta=%.4fm disagreements=%d",
		r.DiffID, r.Added, r.Changed, r.Removed, r.MedianDelta, r.MaxDelta, r.Disagreements)
}

// openLane is a currently-valid version row.
type openLane struct {
	id      int64
	defHash string
	shape   []scenemap.Point
}

// ApplyLaneDiff compares the incoming lane set against what is currently
// open, writes the version rows, and records one diff row describing the edit.
//
// NOTHING IS WRITTEN WHEN NOTHING MOVED. A sync that observes an unchanged
// scene must not leave a diff row behind: the diff log is the answer to "what
// did we change", and padding it with one row per restart makes the real
// edits impossible to find. The whole thing runs in one transaction so an
// abandoned diff leaves no trace.
//
// `areas` bounds what is considered removed. A lane that is open in the
// database but absent from `lanes` is only closed if its area was part of
// this sync — otherwise a sync of one area would retire every lane in every
// other area, which is the same class of bug as the destructive replace this
// package exists to make legible.
func ApplyLaneDiff(db *sql.DB, source, gateHash string, observedAt time.Time,
	previousSync *time.Time, areas []string, lanes []Lane) (DiffResult, error) {

	var res DiffResult
	if len(areas) == 0 {
		return res, nil
	}
	tx, err := db.Begin()
	if err != nil {
		return res, fmt.Errorf("sceneversion: begin: %w", err)
	}
	defer tx.Rollback() // no-op after Commit

	open, err := loadOpenLanes(tx, areas)
	if err != nil {
		return res, err
	}

	if err := tx.QueryRow(
		`INSERT INTO scene_diffs (source, gate_hash, observed_at, previous_sync)
		 VALUES ($1,$2,$3,$4) RETURNING id`,
		source, gateHash, observedAt, previousSync).Scan(&res.DiffID); err != nil {
		return res, fmt.Errorf("sceneversion: open diff: %w", err)
	}

	var deltas []float64
	seen := map[string]bool{}

	for _, ln := range lanes {
		key := ln.Area + "\x00" + ln.Lane
		seen[key] = true
		if !ln.Version.TwinsAgree {
			res.Disagreements++
		}
		prev, exists := open[key]
		if exists && prev.defHash == ln.Version.DefHash {
			continue // unchanged; the open row stays open
		}

		var supersedes *int64
		var delta *float64
		if exists {
			supersedes = &prev.id
			d := scenemap.MaxVertexDelta(prev.shape, ln.Shape)
			// +Inf is a redraw rather than a move — a lane that gained or
			// lost a vertex. Stored as NULL: there is no distance, and a
			// number here would be invented.
			if !isInf(d) {
				delta = &d
				deltas = append(deltas, d)
			}
			if _, err := tx.Exec(
				`UPDATE scene_lane_versions SET valid_to = $1 WHERE id = $2`,
				observedAt, prev.id); err != nil {
				return res, fmt.Errorf("sceneversion: close lane %s: %w", ln.Lane, err)
			}
			res.Changed++
		} else {
			res.Added++
		}

		shapeJSON, err := json.Marshal(ln.Shape)
		if err != nil {
			return res, fmt.Errorf("sceneversion: encode shape for %s: %w", ln.Lane, err)
		}
		// THE FIRST VERSION OF A LANE IS VALID FROM -INFINITY, and this is the
		// distinction that makes version_id NOT NULL possible.
		//
		// valid_from is when the geometry BEGAN. observed_at — carried on the
		// diff row this version points at — is when we first SAW it. Those are
		// different facts and conflating them was the defect: stamping a first
		// version with the sync time claims the lane came into existence the
		// moment Core happened to look, which then leaves every reading taken
		// before that instant with no version at all.
		//
		// There is no such thing as a reading on a lane with no geometry. The
		// honest statement is "this is the earliest geometry we know of, and we
		// cannot say when it began" — an OPEN lower bound. It claims no
		// observation we did not make: provenance stays on the diff row.
		//
		// beginningOfTime rather than Postgres's '-infinity', which says this
		// exactly and which pgx's database/sql shim cannot scan back into a
		// time.Time (it arrives as the string "-infinity"). A concrete year-1
		// timestamp compares identically against every reading a plant will
		// ever take, round-trips through the driver, and needs no translation
		// in the several queries that read this column. It is a BOUND, not a
		// sentinel: nothing special-cases it and `valid_from <= sampled_at`
		// simply works.
		//
		// A later version keeps the observation time, because for those we do
		// know when the change happened: between the previous sync and this one.
		var validFrom *time.Time
		if exists {
			validFrom = &observedAt
		}
		if _, err := tx.Exec(
			`INSERT INTO scene_lane_versions
			   (area_name, lane, shape_hash, def_hash, shape, directed_rows,
			    twins_agree, disagreement, max_vertex_delta_m, supersedes_id,
			    diff_id, valid_from)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,
			         COALESCE($12::timestamptz, $13::timestamptz))`,
			ln.Area, ln.Lane, ln.Version.ShapeHash, ln.Version.DefHash,
			shapeJSON, ln.Version.Directed, ln.Version.TwinsAgree,
			ln.Version.Disagreement, delta, supersedes, res.DiffID, validFrom,
			beginningOfTime); err != nil {
			return res, fmt.Errorf("sceneversion: insert lane %s: %w", ln.Lane, err)
		}
	}

	// A lane that has disappeared is CLOSED, never deleted. Its history is
	// the answer to "how was this running before we took it out", which is
	// exactly the question somebody asks about a lane that no longer exists.
	for key, prev := range open {
		if seen[key] {
			continue
		}
		if _, err := tx.Exec(
			`UPDATE scene_lane_versions SET valid_to = $1 WHERE id = $2`,
			observedAt, prev.id); err != nil {
			return res, fmt.Errorf("sceneversion: close removed lane: %w", err)
		}
		res.Removed++
	}

	if !res.Changed_() {
		// Nothing moved. Roll the whole thing back so the diff log stays a
		// record of edits rather than of restarts.
		return DiffResult{}, nil
	}

	res.MedianDelta, res.MaxDelta = medianAndMax(deltas)
	if _, err := tx.Exec(
		`UPDATE scene_diffs
		    SET objects_added=$1, objects_changed=$2, objects_removed=$3,
		        median_delta_m=$4, max_delta_m=$5
		  WHERE id=$6`,
		res.Added, res.Changed, res.Removed,
		nullable(res.MedianDelta, len(deltas)), nullable(res.MaxDelta, len(deltas)),
		res.DiffID); err != nil {
		return res, fmt.Errorf("sceneversion: complete diff: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("sceneversion: commit: %w", err)
	}
	return res, nil
}

// loadOpenLanes reads the currently-valid version per lane in the given areas.
func loadOpenLanes(tx *sql.Tx, areas []string) (map[string]openLane, error) {
	out := map[string]openLane{}
	for _, area := range areas {
		rows, err := tx.Query(
			`SELECT id, lane, def_hash, shape FROM scene_lane_versions
			  WHERE area_name = $1 AND valid_to IS NULL`, area)
		if err != nil {
			return nil, fmt.Errorf("sceneversion: load open lanes: %w", err)
		}
		for rows.Next() {
			var id int64
			var lane, defHash string
			var shapeJSON []byte
			if err := rows.Scan(&id, &lane, &defHash, &shapeJSON); err != nil {
				rows.Close()
				return nil, err
			}
			var shape []scenemap.Point
			if err := json.Unmarshal(shapeJSON, &shape); err != nil {
				rows.Close()
				return nil, fmt.Errorf("sceneversion: decode shape for %s: %w", lane, err)
			}
			out[area+"\x00"+lane] = openLane{id: id, defHash: defHash, shape: shape}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// LaneVersionAt returns the version id in force on a lane at an instant, or
// nil when the lane had no version then.
//
// NIL IS A REAL ANSWER AND MUST NOT BECOME ZERO. A sample taken before the
// first sync, or on a lane that did not exist yet, has no version — writing 0
// would point at nothing while looking like a foreign key.
func LaneVersionAt(db *sql.DB, area, lane string, at time.Time) (*int64, error) {
	var id int64
	err := db.QueryRow(
		`SELECT id FROM scene_lane_versions
		  WHERE area_name=$1 AND lane=$2
		    AND valid_from <= $3 AND (valid_to IS NULL OR valid_to > $3)
		  ORDER BY valid_from DESC LIMIT 1`, area, lane, at).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sceneversion: lane version at: %w", err)
	}
	return &id, nil
}

// LaneVersionIndex is the whole plant's version history, for a roll-up that
// resolves a version per sample and must not issue one query per row.
type LaneVersionIndex struct {
	byLane map[string][]versionSpan
}

type versionSpan struct {
	id   int64
	from time.Time
	to   *time.Time
}

// LoadLaneVersionIndex reads every version overlapping [from, to).
func LoadLaneVersionIndex(db *sql.DB, from, to time.Time) (*LaneVersionIndex, error) {
	rows, err := db.Query(
		`SELECT area_name, lane, id, valid_from, valid_to FROM scene_lane_versions
		  WHERE valid_from < $2 AND (valid_to IS NULL OR valid_to > $1)
		  ORDER BY valid_from`, from, to)
	if err != nil {
		return nil, fmt.Errorf("sceneversion: load index: %w", err)
	}
	defer rows.Close()
	ix := &LaneVersionIndex{byLane: map[string][]versionSpan{}}
	for rows.Next() {
		var area, lane string
		var s versionSpan
		if err := rows.Scan(&area, &lane, &s.id, &s.from, &s.to); err != nil {
			return nil, err
		}
		k := area + "\x00" + lane
		ix.byLane[k] = append(ix.byLane[k], s)
	}
	return ix, rows.Err()
}

// At returns the version in force on a lane at an instant, or nil.
func (ix *LaneVersionIndex) At(area, lane string, at time.Time) *int64 {
	if ix == nil {
		return nil
	}
	spans := ix.byLane[area+"\x00"+lane]
	for i := len(spans) - 1; i >= 0; i-- {
		s := spans[i]
		if !at.Before(s.from) && (s.to == nil || at.Before(*s.to)) {
			id := s.id
			return &id
		}
	}
	return nil
}

func medianAndMax(d []float64) (float64, float64) {
	if len(d) == 0 {
		return 0, 0
	}
	s := append([]float64(nil), d...)
	sort.Float64s(s)
	max := s[len(s)-1]
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return s[mid], max
	}
	return (s[mid-1] + s[mid]) / 2, max
}

// nullable keeps a computed-over-nothing figure out of the record. A median
// over zero changed lanes is not 0.0 metres, it is absent.
func nullable(v float64, n int) any {
	if n == 0 {
		return nil
	}
	return v
}

func isInf(f float64) bool { return f > 1e300 }

// beginningOfTime is the lower bound a lane's FIRST version opens at.
//
// Postgres '-infinity' is the exact statement and is unusable here: pgx's
// database/sql shim returns it as the string "-infinity" and cannot scan it
// into a time.Time. Year 1 is earlier than any reading a plant will ever take,
// round-trips cleanly, and keeps every query that reads valid_from free of a
// translation branch.
var beginningOfTime = time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)
