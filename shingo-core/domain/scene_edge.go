package domain

import "time"

// SceneEdge is a drivable path segment between two scene points, synced
// from the fleet's scene definition (SEER "advanced curves"). From/To
// carry both the endpoint instance names and raw coordinates, so a
// consumer can render the segment even when an endpoint was never synced
// as a scene point. The robot-map dashboard renders these as the travel
// network and routes robots along them instead of deriving connectivity
// from point proximity (which invented links through walls).
type SceneEdge struct {
	ID           int64     `json:"id"`
	AreaName     string    `json:"area_name"`
	InstanceName string    `json:"instance_name"`
	ClassName    string    `json:"class_name"`
	FromName     string    `json:"from_name"`
	ToName       string    `json:"to_name"`
	FromX        float64   `json:"from_x"`
	FromY        float64   `json:"from_y"`
	ToX          float64   `json:"to_x"`
	ToY          float64   `json:"to_y"`
	SyncedAt     time.Time `json:"synced_at"`
}
