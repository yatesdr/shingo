package domain

import "time"

// SceneEdge is a drivable path segment between two scene points, synced
// from the fleet's scene definition (SEER "advanced curves"). From/To
// carry both the endpoint instance names and raw coordinates, so a
// consumer can render the segment even when an endpoint was never synced
// as a scene point. The robot-map dashboard renders these as the travel
// network and routes robots along them instead of deriving connectivity
// from point proximity (which invented links through walls).
//
// Ctrl1X/Ctrl1Y/Ctrl2X/Ctrl2Y are the segment's cubic-Bezier control handles
// in the From→To direction. All four are nil together on a segment the fleet
// drives straight — the map then draws a line, which for those segments IS the
// driven path. They are pointers rather than zero-valued floats because the
// origin is a real coordinate on a plant map: a straight aisle whose handles
// silently defaulted to (0,0) would be drawn sweeping tens of metres across
// the floor, which is exactly the failure that made this data worth carrying.
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
	Ctrl1X       *float64  `json:"ctrl1_x,omitempty"`
	Ctrl1Y       *float64  `json:"ctrl1_y,omitempty"`
	Ctrl2X       *float64  `json:"ctrl2_x,omitempty"`
	Ctrl2Y       *float64  `json:"ctrl2_y,omitempty"`
	SyncedAt     time.Time `json:"synced_at"`
}

// Curved reports whether the segment carries a complete pair of control
// handles. A partial pair is not a curve: three of four coordinates describe
// no cubic, and drawing one would invent the missing number.
func (e SceneEdge) Curved() bool {
	return e.Ctrl1X != nil && e.Ctrl1Y != nil && e.Ctrl2X != nil && e.Ctrl2Y != nil
}
