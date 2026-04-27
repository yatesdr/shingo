package domain

import "time"

// ScenePoint is a single labelled point in the deployment scene —
// staging area marker, parking station, drop-off location, etc.
// Synced from the fleet's scene/area definition; consumed by the
// nodes-overview UI to render the physical layout.
//
// Stage 2A.2 lifted this struct into domain/ so the page-data
// builder can include scene points in its response without
// importing shingo-core/store/scene. The store package re-exports
// the type via `type Point = domain.ScenePoint`.
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
