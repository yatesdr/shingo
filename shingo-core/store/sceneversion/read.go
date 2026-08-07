package sceneversion

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"shingocore/scenemap"
)

// Read side: what the map page and the diff rail ask for.
//
// EVERY QUERY HERE IS TEMPORAL, and none of them means "current" implicitly.
// A query written against "the current map" returns different attribution for
// the same historical sample before and after an edit, which is the exact
// failure the versioning exists to prevent. Callers that want now pass now.

// AreaView is one declared area as of some instant.
type AreaView struct {
	ID      int64            `json:"id"`
	Name    string           `json:"area_name"`
	Class   string           `json:"class_name"`
	Polygon []scenemap.Point `json:"polygon"`
	// ReflectorCount is PROVENANCE, not a predictor, and the distinction is
	// load-bearing enough to travel with the field. Measured, the count of
	// reflectors inside a zone has no predictive power over its no-estimate
	// rate and the sign runs backwards; what predicts is Class. Render the
	// class, not this. It is here because "this declared reflector zone
	// contains zero reflectors" is the most actionable sentence this project
	// has produced, and because it is the input to any future coverage work.
	ReflectorCount int       `json:"reflector_count"`
	ValidFrom      time.Time `json:"valid_from"`
	// DiffID links this version to the edit that produced it, so the page can
	// go from a zone to "what else changed at the same time".
	DiffID int64 `json:"diff_id"`
}

// ReflectorView is one reflector as of some instant.
type ReflectorView struct {
	ID   int64   `json:"id"`
	Kind string  `json:"kind"`
	X    float64 `json:"x"`
	Y    float64 `json:"y"`
	// Width is null on some cylinders — three of Springfield's seventy-one —
	// and stays null rather than becoming 0.0, which would claim a
	// zero-width reflector nobody measured.
	Width     *float64  `json:"width"`
	ValidFrom time.Time `json:"valid_from"`
}

// AreasAt returns the areas in force at an instant.
func AreasAt(db *sql.DB, at time.Time) ([]AreaView, error) {
	rows, err := db.Query(
		`SELECT id, area_name, class_name, polygon, reflector_count, valid_from, diff_id
		   FROM scene_areas
		  WHERE valid_from <= $1 AND (valid_to IS NULL OR valid_to > $1)
		  ORDER BY area_name`, at)
	if err != nil {
		return nil, fmt.Errorf("sceneversion: areas at: %w", err)
	}
	defer rows.Close()
	out := []AreaView{}
	for rows.Next() {
		var a AreaView
		var poly []byte
		if err := rows.Scan(&a.ID, &a.Name, &a.Class, &poly, &a.ReflectorCount,
			&a.ValidFrom, &a.DiffID); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(poly, &a.Polygon); err != nil {
			return nil, fmt.Errorf("sceneversion: decode polygon %s: %w", a.Name, err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ReflectorsAt returns the reflectors in force at an instant.
func ReflectorsAt(db *sql.DB, at time.Time) ([]ReflectorView, error) {
	rows, err := db.Query(
		`SELECT id, kind, x, y, width, valid_from FROM scene_reflectors
		  WHERE valid_from <= $1 AND (valid_to IS NULL OR valid_to > $1)
		  ORDER BY id`, at)
	if err != nil {
		return nil, fmt.Errorf("sceneversion: reflectors at: %w", err)
	}
	defer rows.Close()
	out := []ReflectorView{}
	for rows.Next() {
		var r ReflectorView
		if err := rows.Scan(&r.ID, &r.Kind, &r.X, &r.Y, &r.Width, &r.ValidFrom); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DiffView is one observed edit, for the change rail.
//
// IT SAYS ONLY WHAT A DIFF CAN KNOW. Nobody is typing anything, so there is no
// author and no reason — every field here is derivable from comparing two
// versions. It cannot say WHY a lane moved and must not pretend to; naming the
// affected lanes is the part that does the real work.
type DiffView struct {
	ID     int64  `json:"id"`
	Source string `json:"source"`
	// ObservedAt is when the change was SEEN. PreviousSync is when we last
	// looked, so the window the edit happened in is explicit rather than
	// implied — two edits between syncs are one row, and without the lower
	// bound "when" is unbounded on the early side.
	ObservedAt   time.Time  `json:"observed_at"`
	PreviousSync *time.Time `json:"previous_sync"`
	Added        int        `json:"objects_added"`
	Changed      int        `json:"objects_changed"`
	Removed      int        `json:"objects_removed"`
	// MedianDelta and MaxDelta are what turn "something happened" into "what
	// kind of thing happened": thousands of objects at a median of 0.004 m is
	// a rescan, seventeen at 2.3 m is a re-route. Null when nothing had a
	// predecessor to be measured from.
	MedianDelta *float64 `json:"median_delta_m"`
	MaxDelta    *float64 `json:"max_delta_m"`
}

// RecentDiffs returns the change log, newest first.
func RecentDiffs(db *sql.DB, limit int) ([]DiffView, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := db.Query(
		`SELECT id, source, observed_at, previous_sync,
		        objects_added, objects_changed, objects_removed,
		        median_delta_m, max_delta_m
		   FROM scene_diffs ORDER BY observed_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("sceneversion: recent diffs: %w", err)
	}
	defer rows.Close()
	out := []DiffView{}
	for rows.Next() {
		var d DiffView
		if err := rows.Scan(&d.ID, &d.Source, &d.ObservedAt, &d.PreviousSync,
			&d.Added, &d.Changed, &d.Removed, &d.MedianDelta, &d.MaxDelta); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// MapVersionState is what the map-sync gate needs to decide whether to fetch.
type MapVersionState struct {
	// MapMD5 is the robot-reported hash of the newest stored version of this
	// map. Empty when a version was archived without one.
	MapMD5 string
	// SyncedAt is when that version was observed. The daily floor is measured
	// from here, so a plant whose hash never moves still re-reads once a day.
	SyncedAt time.Time
}

// LatestMapVersion returns the newest archived version of one named map.
//
// FALSE MEANS "NEVER FETCHED", NOT "UNCHANGED", and the caller must not
// conflate them — a plant that has never pulled a map has no areas and no
// reflectors, which is a very different state from one whose map is stable.
//
// Keyed on map NAME because that is what a robot reports it is running and
// what #4011 is asked for. Two maps at one plant (Hopkinsville ran Hop_20 and
// Hop_21 simultaneously) are two independent version streams, and collapsing
// them would make each one's changes look like the other's.
func LatestMapVersion(db *sql.DB, mapName string) (MapVersionState, bool, error) {
	var st MapVersionState
	err := db.QueryRow(
		`SELECT map_md5, synced_at FROM scene_map_versions
		  WHERE map_name = $1
		  ORDER BY synced_at DESC, id DESC LIMIT 1`, mapName).Scan(&st.MapMD5, &st.SyncedAt)
	if err == sql.ErrNoRows {
		return MapVersionState{}, false, nil
	}
	if err != nil {
		return MapVersionState{}, false, fmt.Errorf("sceneversion: latest map version: %w", err)
	}
	return st, true, nil
}

// LanesChangedByDiff names the lanes one edit touched, which is the part of a
// diff row that does real work — an engineer reads "what did I touch", not a
// count.
func LanesChangedByDiff(db *sql.DB, diffID int64) ([]string, error) {
	rows, err := db.Query(
		`SELECT DISTINCT lane FROM scene_lane_versions WHERE diff_id=$1 ORDER BY lane`, diffID)
	if err != nil {
		return nil, fmt.Errorf("sceneversion: lanes by diff: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// LanesChangedIn names the lanes whose geometry changed inside [from, to).
//
// "CHANGED", NOT "HAS A VERSION". Every lane has a version — the first one
// opens at the beginning of time — so a query that merely found a version row
// would mark the entire plant as changed on a board whose only change mark is
// supposed to answer "what did I touch". A change is a version whose valid_from
// falls INSIDE the window, which by construction excludes every first version.
//
// Returned as the roll-up's own composite key (area \x00 lane) so the caller
// joins against LaneWindows without re-deriving the separator in a second
// place.
func LanesChangedIn(db *sql.DB, from, to time.Time) (map[string]bool, error) {
	rows, err := db.Query(
		`SELECT DISTINCT area_name, lane FROM scene_lane_versions
		  WHERE valid_from >= $1 AND valid_from < $2
		    AND supersedes_id IS NOT NULL`, from, to)
	if err != nil {
		return nil, fmt.Errorf("sceneversion: lanes changed in window: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var area, lane string
		if err := rows.Scan(&area, &lane); err != nil {
			return nil, err
		}
		out[area+"\x00"+lane] = true
	}
	return out, rows.Err()
}
