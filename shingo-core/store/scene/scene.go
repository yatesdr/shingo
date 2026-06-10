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

	"shingocore/domain"
)

// Point is the scene-point entity. The struct lives in
// shingocore/domain (Stage 2A.2); this alias keeps the scene.Point
// name used by scan helpers and the outer store/ re-export, and
// lets the www handlers + node-page builder reference scene points
// via shingocore/domain instead of this persistence sub-package.
type Point = domain.ScenePoint

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

// ── scene edges (drivable path segments, synced from advanced curves) ──

// Edge is the scene-edge entity; the struct lives in shingocore/domain
// (mirroring the scene.Point = domain.ScenePoint alias above).
type Edge = domain.SceneEdge

const edgeCols = `id, area_name, instance_name, class_name, from_name, to_name, from_x, from_y, to_x, to_y, synced_at`

func scanEdge(row interface{ Scan(...any) error }) (*Edge, error) {
	var se Edge
	err := row.Scan(&se.ID, &se.AreaName, &se.InstanceName, &se.ClassName,
		&se.FromName, &se.ToName,
		&se.FromX, &se.FromY, &se.ToX, &se.ToY,
		&se.SyncedAt)
	if err != nil {
		return nil, err
	}
	return &se, nil
}

// UpsertEdge inserts or updates a scene edge keyed by (area_name, instance_name).
func UpsertEdge(db *sql.DB, se *Edge) error {
	_, err := db.Exec(`INSERT INTO scene_edges (area_name, instance_name, class_name, from_name, to_name, from_x, from_y, to_x, to_y, synced_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NOW())
		ON CONFLICT (area_name, instance_name) DO UPDATE SET
			class_name=EXCLUDED.class_name, from_name=EXCLUDED.from_name,
			to_name=EXCLUDED.to_name,
			from_x=EXCLUDED.from_x, from_y=EXCLUDED.from_y,
			to_x=EXCLUDED.to_x, to_y=EXCLUDED.to_y,
			synced_at=EXCLUDED.synced_at`,
		se.AreaName, se.InstanceName, se.ClassName, se.FromName, se.ToName,
		se.FromX, se.FromY, se.ToX, se.ToY)
	return err
}

// ListEdges returns every scene edge, ordered by area + instance.
func ListEdges(db *sql.DB) ([]*Edge, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT %s FROM scene_edges ORDER BY area_name, instance_name`, edgeCols))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var edges []*Edge
	for rows.Next() {
		se, err := scanEdge(rows)
		if err != nil {
			return nil, err
		}
		edges = append(edges, se)
	}
	return edges, rows.Err()
}

// DeleteEdgesByArea removes every scene edge in a given area.
func DeleteEdgesByArea(db *sql.DB, areaName string) error {
	_, err := db.Exec(`DELETE FROM scene_edges WHERE area_name=$1`, areaName)
	return err
}

// ListAreas returns the distinct area names stored across scene points and
// edges. Scene sync reconciles this set against the fleet's current areas:
// any stored area no longer in the fleet payload is stale and gets swept
// (areas deleted from RDS used to linger forever as ghost points on the map).
func ListAreas(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`SELECT area_name FROM scene_points
		UNION SELECT area_name FROM scene_edges ORDER BY area_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var areas []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		areas = append(areas, a)
	}
	return areas, rows.Err()
}
