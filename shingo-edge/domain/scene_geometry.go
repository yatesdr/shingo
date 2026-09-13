package domain

import (
	"fmt"

	"shingo/protocol"
)

// SceneGeometry is the Edge's copy of the vendor map's geometry: where every
// scene point is, and the shape of every drivable segment. It is what the
// station's cell picture draws from.
//
// Delivered by Core on the node-list sync when the Edge's revision is stale,
// held durably (scene_geometry_* tables) and hot (the engine), and replaced
// ONLY by a complete response — see NewSceneGeometry. Revision is the value
// the heartbeater quotes back so Core can leave the geometry off.
type SceneGeometry struct {
	Revision string                    `json:"revision"`
	Points   map[string]ScenePointGeom `json:"points"`
	Edges    []SceneEdgeGeom           `json:"edges"`
}

// ScenePointGeom is one placed scene point. ClassName is kept because the
// node→map join is on instance_name WHERE class_name = 'GeneralLocation' —
// the bin location — and never on label, which is blank at Springfield.
type ScenePointGeom struct {
	InstanceName string  `json:"instance_name"`
	ClassName    string  `json:"class_name"`
	X            float64 `json:"x"`
	Y            float64 `json:"y"`
	Dir          float64 `json:"dir"`
}

// SceneEdgeGeom is one drivable segment. Handles is nil on a straight segment
// and [c1x, c1y, c2x, c2y] on a curved one — all four or none, because three
// coordinates describe no cubic and a fourth would have to be invented.
type SceneEdgeGeom struct {
	From    string      `json:"from"`
	To      string      `json:"to"`
	FromX   float64     `json:"from_x"`
	FromY   float64     `json:"from_y"`
	ToX     float64     `json:"to_x"`
	ToY     float64     `json:"to_y"`
	Handles *[4]float64 `json:"handles,omitempty"`
}

// NewSceneGeometry builds the cache shape from one node-list response, or
// says why the response is not one the cache may take.
//
// COMPLETE OR NOTHING. The cache is replaced wholesale, so a response that
// would leave it holding less than the whole map is refused: no revision (Core
// could not read the scene in full), no points or no edges, any point without
// both coordinates, any edge without all four endpoint coordinates. A
// name-only response — the ordinary case, when the revision matched — fails
// the coordinate check and leaves the cache untouched, which is the point.
// This mirrors ReplaceCoreLoaders' all-or-nothing posture with the check
// moved in front of the write.
//
// nil IS ABSENCE, NEVER ZERO. The wire carries pointers precisely so that a
// point at (0,0) and a point whose geometry was left off can be told apart;
// this is the one place that distinction is consumed, and it must not be
// collapsed with a "missing means 0" default.
func NewSceneGeometry(revision string, points []protocol.ScenePointInfo, edges []protocol.SceneEdgeInfo) (*SceneGeometry, error) {
	if revision == "" {
		return nil, fmt.Errorf("scene geometry: response carries no revision")
	}
	if len(points) == 0 || len(edges) == 0 {
		return nil, fmt.Errorf("scene geometry: response carries %d points and %d edges; both are needed", len(points), len(edges))
	}
	g := &SceneGeometry{Revision: revision, Points: make(map[string]ScenePointGeom, len(points)), Edges: make([]SceneEdgeGeom, 0, len(edges))}
	for _, p := range points {
		if p.InstanceName == "" {
			continue
		}
		if p.PosX == nil || p.PosY == nil {
			return nil, fmt.Errorf("scene geometry: point %s carries no coordinates (a name-only response)", p.InstanceName)
		}
		geom := ScenePointGeom{InstanceName: p.InstanceName, ClassName: p.ClassName, X: *p.PosX, Y: *p.PosY}
		if p.Dir != nil {
			geom.Dir = *p.Dir
		}
		g.Points[p.InstanceName] = geom
	}
	for _, e := range edges {
		if e.From == "" || e.To == "" {
			continue
		}
		if e.FromX == nil || e.FromY == nil || e.ToX == nil || e.ToY == nil {
			return nil, fmt.Errorf("scene geometry: edge %s->%s carries no endpoint coordinates", e.From, e.To)
		}
		geom := SceneEdgeGeom{From: e.From, To: e.To, FromX: *e.FromX, FromY: *e.FromY, ToX: *e.ToX, ToY: *e.ToY}
		if e.Ctrl1X != nil && e.Ctrl1Y != nil && e.Ctrl2X != nil && e.Ctrl2Y != nil {
			geom.Handles = &[4]float64{*e.Ctrl1X, *e.Ctrl1Y, *e.Ctrl2X, *e.Ctrl2Y}
		}
		g.Edges = append(g.Edges, geom)
	}
	if len(g.Points) == 0 || len(g.Edges) == 0 {
		return nil, fmt.Errorf("scene geometry: every row was unnamed")
	}
	return g, nil
}
