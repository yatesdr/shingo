// Package scene holds scene-point persistence for shingo-core.
//
// Phase 5 of the architecture plan moved scene_points CRUD out of the
// flat store/ package and into this sub-package. The outer store/ keeps
// a type alias (`store.ScenePoint = scene.Point`) and one-line delegate
// methods on *store.DB so external callers see no API change.
package scene

import (
	"database/sql"
	"fmt"
	"time"
)

// Point is the scene-point entity. The type is re-aliased at the outer
// store/ level as store.ScenePoint so scenesync, service/node_service.go,
// and the www handlers compile unchanged.
type Point struct {
	ID             int64     `json:"id"`
	AreaName       string    `json:"area_name"`
	InstanceName   string    `json:"instance_name"`
	ClassName      string    `json:"class_name"`
	PointName      string    `json:"point_name"`
	GroupName      string    `json:"group_name"`
	Label          string    `json:"label"`
	PosX           float64   `json:"pos_x"`
	PosY           float64   `json:"pos_y"`
	PosZ           float64   `json:"pos_z"`
	Dir            float64   `json:"dir"`
	PropertiesJSON string    `json:"properties_json"`
	SyncedAt       time.Time `json:"synced_at"`
}

const selectCols = `id, area_name, instance_name, class_name, point_name, group_name, label, pos_x, pos_y, pos_z, dir, properties_json, synced_at`

func scanPoint(row interface{ Scan(...any) error }) (*Point, error) {
	var sp Point
	err := row.Scan(&sp.ID, &sp.AreaName, &sp.InstanceName, &sp.ClassName,
		&sp.PointName, &sp.GroupName, &sp.Label,
		&sp.PosX, &sp.PosY, &sp.PosZ, &sp.Dir,
		&sp.PropertiesJSON, &sp.SyncedAt)
	if err != nil {
		return nil, err
	}
	return &sp, nil
}

func scanPoints(rows *sql.Rows) ([]*Point, error) {
	var points []*Point
	for rows.Next() {
		sp, err := scanPoint(rows)
		if err != nil {
			return nil, err
		}
		points = append(points, sp)
	}
	return points, rows.Err()
}

// Upsert inserts or updates a scene point keyed by (area_name, instance_name).
func Upsert(db *sql.DB, sp *Point) error {
	_, err := db.Exec(`INSERT INTO scene_points (area_name, instance_name, class_name, point_name, group_name, label, pos_x, pos_y, pos_z, dir, properties_json, synced_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, NOW())
		ON CONFLICT (area_name, instance_name) DO UPDATE SET
			class_name=EXCLUDED.class_name, point_name=EXCLUDED.point_name,
			group_name=EXCLUDED.group_name, label=EXCLUDED.label,
			pos_x=EXCLUDED.pos_x, pos_y=EXCLUDED.pos_y, pos_z=EXCLUDED.pos_z,
			dir=EXCLUDED.dir, properties_json=EXCLUDED.properties_json,
			synced_at=EXCLUDED.synced_at`,
		sp.AreaName, sp.InstanceName, sp.ClassName, sp.PointName, sp.GroupName, sp.Label,
		sp.PosX, sp.PosY, sp.PosZ, sp.Dir, sp.PropertiesJSON)
	return err
}

// List returns every scene point, ordered by area + class + instance.
func List(db *sql.DB) ([]*Point, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT %s FROM scene_points ORDER BY area_name, class_name, instance_name`, selectCols))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPoints(rows)
}

// ListByClass returns all scene points of a given class.
func ListByClass(db *sql.DB, className string) ([]*Point, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT %s FROM scene_points WHERE class_name=$1 ORDER BY area_name, instance_name`, selectCols), className)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPoints(rows)
}

// ListByArea returns all scene points in a given area.
func ListByArea(db *sql.DB, areaName string) ([]*Point, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT %s FROM scene_points WHERE area_name=$1 ORDER BY class_name, instance_name`, selectCols), areaName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPoints(rows)
}

// DeleteByArea removes every scene point in a given area.
func DeleteByArea(db *sql.DB, areaName string) error {
	_, err := db.Exec(`DELETE FROM scene_points WHERE area_name=$1`, areaName)
	return err
}
