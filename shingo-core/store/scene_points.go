package store

// Phase 5 delegate file: scene-point CRUD lives in store/scene/. This
// file preserves the *store.DB method surface so external callers don't
// need to change.

import "shingocore/store/scene"

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
