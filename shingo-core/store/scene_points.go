package store

// Phase 5 delegate file: scene-point CRUD lives in store/scene/. This
// file preserves the *store.DB method surface so external callers don't
// need to change.

import (
	"time"

	"shingocore/store/scene"
	"shingocore/store/sceneversion"
)

func (db *DB) UpsertScenePoint(sp *scene.Point) error {
	return scene.Upsert(db.DB, sp)
}

func (db *DB) ListScenePoints() ([]*scene.Point, error) {
	return scene.List(db.DB)
}

func (db *DB) ListScenePointsByClass(className string) ([]*scene.Point, error) {
	return scene.ListByClass(db.DB, className)
}

// StationsForPointName resolves a robot-reported point name to the station(s)
// bound to it — see scene.StationsForPointName for why it is a slice.
func (db *DB) StationsForPointName(pointName string) ([]string, error) {
	return scene.StationsForPointName(db.DB, pointName)
}

// ClassOfPoint reports what class the scene holds for an instance name.
// See scene.ClassOfPoint.
func (db *DB) ClassOfPoint(instanceName string) (string, error) {
	return scene.ClassOfPoint(db.DB, instanceName)
}

// CountStationPoints reports how many bin locations the scene holds — the
// refusal path's way of telling "not a station" from "never synced".
func (db *DB) CountStationPoints() (int, error) {
	return scene.CountStationPoints(db.DB)
}

func (db *DB) ListScenePointsByArea(areaName string) ([]*scene.Point, error) {
	return scene.ListByArea(db.DB, areaName)
}

func (db *DB) DeleteScenePointsByArea(areaName string) error {
	return scene.DeleteByArea(db.DB, areaName)
}

func (db *DB) UpsertSceneEdge(se *scene.Edge) error {
	return scene.UpsertEdge(db.DB, se)
}

func (db *DB) ListSceneEdges() ([]*scene.Edge, error) {
	return scene.ListEdges(db.DB)
}

func (db *DB) DeleteSceneEdgesByArea(areaName string) error {
	return scene.DeleteEdgesByArea(db.DB, areaName)
}

// ReplaceAreaScene swaps one area's points and edges atomically. See
// scene.ReplaceArea for why the transaction is load-bearing.
func (db *DB) ReplaceAreaScene(areaName string, points []*scene.Point, edges []*scene.Edge) error {
	return scene.ReplaceArea(db.DB, areaName, points, edges)
}

// LatestMapVersion reports the newest archived version of one named map, and
// whether any exists. See sceneversion.LatestMapVersion.
func (db *DB) LatestMapVersion(mapName string) (sceneversion.MapVersionState, bool, error) {
	return sceneversion.LatestMapVersion(db.DB, mapName)
}

// LanesChangedIn names the lanes whose geometry changed inside a window. See
// sceneversion.LanesChangedIn — a first version does not count as a change.
func (db *DB) LanesChangedIn(from, to time.Time) (map[string]bool, error) {
	return sceneversion.LanesChangedIn(db.DB, from, to)
}

// LaneLastChange reports when a lane's geometry last changed and by how far.
// A first version does not count. See sceneversion.LastChange.
func (db *DB) LaneLastChange(area, lane string) (time.Time, *float64, bool, error) {
	return sceneversion.LastChange(db.DB, area, lane)
}

// ApplyMapSnapshot archives one observed .smap and versions its areas and
// reflectors. See sceneversion.ApplyMapSnapshot.
func (db *DB) ApplyMapSnapshot(snap sceneversion.MapSnapshot, previousSync *time.Time) (sceneversion.MapSyncResult, error) {
	return sceneversion.ApplyMapSnapshot(db.DB, snap, previousSync)
}

func (db *DB) ListSceneAreas() ([]string, error) {
	return scene.ListAreas(db.DB)
}

// ApplyLaneVersions records one scene edit: a diff row plus a version row per
// lane that actually moved. Delegates to store/sceneversion, matching the
// shape of the other sub-package delegates so callers see one *store.DB API.
func (db *DB) ApplyLaneVersions(source, gateHash string, observedAt time.Time,
	previousSync *time.Time, areas []string, lanes []sceneversion.Lane) (sceneversion.DiffResult, error) {
	return sceneversion.ApplyLaneDiff(db.DB, source, gateHash, observedAt, previousSync, areas, lanes)
}

// ── Scene versioning read side ─────────────────────────────────────────────
//
// Every one of these takes an INSTANT. None of them means "current"
// implicitly: a query written against "the current map" returns different
// attribution for the same historical sample before and after an edit, which
// is the failure the versioning exists to prevent. A caller that wants now
// passes now, visibly.

func (db *DB) SceneAreasAt(at time.Time) ([]sceneversion.AreaView, error) {
	return sceneversion.AreasAt(db.DB, at)
}

func (db *DB) SceneReflectorsAt(at time.Time) ([]sceneversion.ReflectorView, error) {
	return sceneversion.ReflectorsAt(db.DB, at)
}

func (db *DB) RecentSceneDiffs(limit int) ([]sceneversion.DiffView, error) {
	return sceneversion.RecentDiffs(db.DB, limit)
}

// LanesChangedByDiffs names the lanes each of these edits touched, one query for
// the whole page.
func (db *DB) LanesChangedByDiffs(diffIDs []int64) (map[int64][]string, error) {
	return sceneversion.LanesChangedByDiffs(db.DB, diffIDs)
}

// ListScenePointNames returns every scene point as (instance_name, class_name).
//
// NARROW ON PURPOSE. ListScenePoints reads the whole row — coordinates,
// properties JSON, the lot — and this feeds the per-station node-list sync,
// which runs on a timer for every Edge. The two things a downstream consumer
// needs are the name to match against and the class to tell a waypoint from an
// action point; the geometry stays here.
//
// DISTINCT, because instance names are unique per AREA and a plant with two
// mapped areas can legitimately name the same point in both. The consumer is
// asking "is this a real map point", which is an area-independent question.
func (db *DB) ListScenePointNames() ([][2]string, error) {
	rows, err := db.DB.Query(`
		SELECT DISTINCT instance_name, class_name
		FROM scene_points
		WHERE instance_name <> ''
		ORDER BY instance_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var pair [2]string
		if err := rows.Scan(&pair[0], &pair[1]); err != nil {
			return nil, err
		}
		out = append(out, pair)
	}
	return out, rows.Err()
}

// ListSceneEdgeEndpoints returns every drivable segment as (from_name, to_name).
//
// Segments with a blank endpoint are dropped: an edge that names only one end
// joins nothing, and the adapter already skips the fully-degenerate ones. Same
// narrowness argument as ListScenePointNames — this is adjacency, not geometry.
func (db *DB) ListSceneEdgeEndpoints() ([][2]string, error) {
	rows, err := db.DB.Query(`
		SELECT DISTINCT from_name, to_name
		FROM scene_edges
		WHERE from_name <> '' AND to_name <> ''
		ORDER BY from_name, to_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var pair [2]string
		if err := rows.Scan(&pair[0], &pair[1]); err != nil {
			return nil, err
		}
		out = append(out, pair)
	}
	return out, rows.Err()
}
