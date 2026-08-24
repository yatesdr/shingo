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
