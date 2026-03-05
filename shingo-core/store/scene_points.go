package store

import (
	"database/sql"
	"fmt"
	"time"
)

type ScenePoint struct {
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

const scenePointSelectCols = `id, area_name, instance_name, class_name, point_name, group_name, label, pos_x, pos_y, pos_z, dir, properties_json, synced_at`

func scanScenePoint(row interface{ Scan(...any) error }) (*ScenePoint, error) {
	var sp ScenePoint
	var syncedAt any
	err := row.Scan(&sp.ID, &sp.AreaName, &sp.InstanceName, &sp.ClassName,
		&sp.PointName, &sp.GroupName, &sp.Label,
		&sp.PosX, &sp.PosY, &sp.PosZ, &sp.Dir,
		&sp.PropertiesJSON, &syncedAt)
	if err != nil {
		return nil, err
	}
	sp.SyncedAt = parseTime(syncedAt)
	return &sp, nil
}

func scanScenePoints(rows *sql.Rows) ([]*ScenePoint, error) {
	var points []*ScenePoint
	for rows.Next() {
		sp, err := scanScenePoint(rows)
		if err != nil {
			return nil, err
		}
		points = append(points, sp)
	}
	return points, rows.Err()
}

func (db *DB) UpsertScenePoint(sp *ScenePoint) error {
	if db.driver == "postgres" {
		_, err := db.Exec(db.Q(`INSERT INTO scene_points (area_name, instance_name, class_name, point_name, group_name, label, pos_x, pos_y, pos_z, dir, properties_json, synced_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NOW())
			ON CONFLICT (area_name, instance_name) DO UPDATE SET
				class_name=EXCLUDED.class_name, point_name=EXCLUDED.point_name,
				group_name=EXCLUDED.group_name, label=EXCLUDED.label,
				pos_x=EXCLUDED.pos_x, pos_y=EXCLUDED.pos_y, pos_z=EXCLUDED.pos_z,
				dir=EXCLUDED.dir, properties_json=EXCLUDED.properties_json,
				synced_at=EXCLUDED.synced_at`),
			sp.AreaName, sp.InstanceName, sp.ClassName, sp.PointName, sp.GroupName, sp.Label,
			sp.PosX, sp.PosY, sp.PosZ, sp.Dir, sp.PropertiesJSON)
		return err
	}
	_, err := db.Exec(`INSERT OR REPLACE INTO scene_points (area_name, instance_name, class_name, point_name, group_name, label, pos_x, pos_y, pos_z, dir, properties_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sp.AreaName, sp.InstanceName, sp.ClassName, sp.PointName, sp.GroupName, sp.Label,
		sp.PosX, sp.PosY, sp.PosZ, sp.Dir, sp.PropertiesJSON)
	return err
}

func (db *DB) ListScenePoints() ([]*ScenePoint, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`SELECT %s FROM scene_points ORDER BY area_name, class_name, instance_name`, scenePointSelectCols)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanScenePoints(rows)
}

func (db *DB) ListScenePointsByClass(className string) ([]*ScenePoint, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`SELECT %s FROM scene_points WHERE class_name=? ORDER BY area_name, instance_name`, scenePointSelectCols)), className)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanScenePoints(rows)
}

func (db *DB) ListScenePointsByArea(areaName string) ([]*ScenePoint, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`SELECT %s FROM scene_points WHERE area_name=? ORDER BY class_name, instance_name`, scenePointSelectCols)), areaName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanScenePoints(rows)
}

func (db *DB) ListBinLocations() ([]*ScenePoint, error) {
	return db.ListScenePointsByClass("GeneralLocation")
}

func (db *DB) ListSceneAreaNames() ([]string, error) {
	rows, err := db.Query(db.Q(`SELECT DISTINCT area_name FROM scene_points ORDER BY area_name`))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

func (db *DB) DeleteScenePointsByArea(areaName string) error {
	_, err := db.Exec(db.Q(`DELETE FROM scene_points WHERE area_name=?`), areaName)
	return err
}
