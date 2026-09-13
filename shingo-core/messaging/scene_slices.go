package messaging

import (
	"log"

	"shingo/protocol"
	"shingocore/store/scene"
)

// sceneSlices reads the vendor scene and projects it for one Edge's node
// list: every name every time, the geometry only when the Edge's revision
// does not match, and the revision the rows were cut at.
//
// NAMES EVERY TIME. The key-route validator asks "is this a real map point"
// against the full name set on every sync, and the "absence is never a
// finding" guards depend on the set being whole — so the revision
// short-circuit strips coordinates, never rows.
//
// GEOMETRY ON MISMATCH, which covers three Edges that look the same on the
// wire: one with nothing cached, one whose cache is stale, and an older binary
// that does not send the field. All three quote a revision that is not this
// one (or none) and all three get the full scene.
//
// DEGRADE ON ERROR, KEEP THE NODE LIST. A scene read that fails must not abort
// the node list — that is the posture this replaces and it stands. What
// changes is that a partial read mints NO revision: the Edge replaces its
// durable cache only on a complete response with a revision, so a revision
// on half a scene would let it believe it holds the whole one. Names that
// could be read are still sent, and no geometry is (it would be dropped
// unrevisioned on the far side anyway).
//
// ONE ROW PER NAME. Instance names are unique per AREA in the vendor's model,
// so a plant with two mapped areas can legitimately name the same point in
// both. The consumer keys on the name, so the first row in (area, class,
// name) order wins — deterministic, and the same answer the DISTINCT in the
// old name-only queries gave for the name set. Edges likewise, by endpoints.
func (s *CoreDataService) sceneSlices(edgeRevision, station string) ([]protocol.ScenePointInfo, []protocol.SceneEdgeInfo, string) {
	points, perr := s.db.ListScenePoints()
	if perr != nil {
		log.Printf("core_handler: list scene points for %s: %v", station, perr)
	}
	edges, eerr := s.db.ListSceneEdges()
	if eerr != nil {
		log.Printf("core_handler: list scene edges for %s: %v", station, eerr)
	}
	var revision string
	if perr == nil && eerr == nil {
		revision = scene.Revision(points, edges)
	}
	withGeometry := revision != "" && edgeRevision != revision
	return projectScenePoints(points, withGeometry), projectSceneEdges(edges, withGeometry), revision
}

func projectScenePoints(points []*scene.Point, withGeometry bool) []protocol.ScenePointInfo {
	var out []protocol.ScenePointInfo
	seen := make(map[string]bool, len(points))
	for _, p := range points {
		if p.InstanceName == "" || seen[p.InstanceName] {
			continue
		}
		seen[p.InstanceName] = true
		info := protocol.ScenePointInfo{InstanceName: p.InstanceName, ClassName: p.ClassName}
		if withGeometry {
			x, y, d := p.PosX, p.PosY, p.Dir
			info.PosX, info.PosY, info.Dir = &x, &y, &d
		}
		out = append(out, info)
	}
	return out
}

// projectSceneEdges carries the handles ALL FOUR OR NONE. A NULL column is
// nil on the wire; a partial pair describes no cubic (domain.SceneEdge.Curved
// is the same rule) and goes across as the chord rather than with three
// numbers and an invented fourth.
func projectSceneEdges(edges []*scene.Edge, withGeometry bool) []protocol.SceneEdgeInfo {
	var out []protocol.SceneEdgeInfo
	seen := make(map[[2]string]bool, len(edges))
	for _, e := range edges {
		if e.FromName == "" || e.ToName == "" {
			continue
		}
		key := [2]string{e.FromName, e.ToName}
		if seen[key] {
			continue
		}
		seen[key] = true
		info := protocol.SceneEdgeInfo{From: e.FromName, To: e.ToName}
		if withGeometry {
			fx, fy, tx, ty := e.FromX, e.FromY, e.ToX, e.ToY
			info.FromX, info.FromY, info.ToX, info.ToY = &fx, &fy, &tx, &ty
			if e.Curved() {
				c1x, c1y, c2x, c2y := *e.Ctrl1X, *e.Ctrl1Y, *e.Ctrl2X, *e.Ctrl2Y
				info.Ctrl1X, info.Ctrl1Y, info.Ctrl2X, info.Ctrl2Y = &c1x, &c1y, &c2x, &c2y
			}
		}
		out = append(out, info)
	}
	return out
}
