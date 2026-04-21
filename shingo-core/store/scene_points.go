package store

// Phase 5 delegate file: scene-point CRUD lives in store/scene/. This
// file preserves the *store.DB method surface so external callers don't
// need to change.

import (
	"shingocore/store/scene"
)

// ScenePoint preserves the store.ScenePoint public API.
type ScenePoint = scene.Point

func (db *DB) UpsertScenePoint(sp *ScenePoint) error {
	return scene.Upsert(db.DB, sp)
}

func (db *DB) ListScenePoints() ([]*ScenePoint, error) {
	return scene.List(db.DB)
}

func (db *DB) ListScenePointsByClass(className string) ([]*ScenePoint, error) {
	return scene.ListByClass(db.DB, className)
}

func (db *DB) ListScenePointsByArea(areaName string) ([]*ScenePoint, error) {
	return scene.ListByArea(db.DB, areaName)
}

func (db *DB) DeleteScenePointsByArea(areaName string) error {
	return scene.DeleteByArea(db.DB, areaName)
}
