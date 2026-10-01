package engine

import (
	"shingo/protocol"
	"shingoedge/release"
	"shingoedge/store"
)

// release_fake_points_test.go — the harness's Core answer to releasePoints.
//
// Core computes a release point from its own plan (dispatch/release_points.go,
// pinned in Core's release_points_docker_test.go). The harness has no Core, so
// it answers from the Edge's own copy of each leg's steps, the way Core's rule
// reads them (SHAPE §3.4):
//
//   - Enters: the pickups and drops of the leg's pending segment — the steps
//     after the station wait it is at or heading to, up to its next wait;
//   - AwaitsLift: for each node that segment sets a bin down on, holding a
//     bin now, that the leg's sibling lifts from (in its remaining steps)
//     before any drop of its own there; CoRelease when the sibling is staged
//     at a station wait whose next segment lifts there and this leg picks
//     something up before it places.
//
// "Holds a bin now" is every node until a lift there is reported to the
// harness (h.pickedUp), which is the one fact Core has that the Edge does not.
func (h *relHarness) fakePoints(req protocol.ReleasePointsRequest) protocol.ReleasePointsResponse {
	r0, w0 := h.counter.Reads(), h.counter.Writes()
	resp := fakeReleasePoints(h.db, h.lifted, req)
	h.coreReads += h.counter.Reads() - r0
	h.coreWrites += h.counter.Writes() - w0
	return resp
}

// dbPoints is the fake as an engine's point source, for the unit tests' test
// engine: no lift is ever reported to it, so every node holds its bin.
type dbPoints struct{ db *store.DB }

func (p dbPoints) ReleasePoints(station string, uuids []string) ([]protocol.ReleasePoint, error) {
	return fakeReleasePoints(p.db, nil, protocol.ReleasePointsRequest{StationID: station, OrderUUIDs: uuids}).Points, nil
}

func fakeReleasePoints(db *store.DB, lifted map[string]bool, req protocol.ReleasePointsRequest) protocol.ReleasePointsResponse {
	var out protocol.ReleasePointsResponse
	for _, uuid := range req.OrderUUIDs {
		pt := protocol.ReleasePoint{OrderUUID: uuid, Enters: []string{}, AwaitsLift: []protocol.LiftDependency{}}
		o, err := db.GetOrderByUUID(uuid)
		if err != nil || o == nil {
			out.Points = append(out.Points, pt)
			continue
		}
		pt.Found = true
		steps := stepsOfOrder(db, o.ID)
		in, _ := release.DecodeIntent(o.ReleaseIntent)
		point := release.PointOf(o.Status, o.StationWait, o.WaitKind, in, purposesOf(steps))
		seg := segmentAfterWait(steps, point.Ordinal)
		for _, s := range seg {
			if (s.Action == protocol.ActionPickup || s.Action == protocol.ActionDropoff) && s.Node != "" {
				pt.Enters = append(pt.Enters, s.Node)
			}
		}
		if o.SiblingOrderID != nil {
			if sib, err := db.GetOrder(*o.SiblingOrderID); err == nil && !protocol.IsTerminal(sib.Status) {
				sibSteps := stepsOfOrder(db, sib.ID)
				sin, _ := release.DecodeIntent(sib.ReleaseIntent)
				sp := release.PointOf(sib.Status, sib.StationWait, sib.WaitKind, sin, purposesOf(sibSteps))
				remaining := stationStepsFrom(sibSteps, sp.Ordinal)
				var sibSeg []protocol.ComplexOrderStep
				if sp.AtWait {
					sibSeg = segmentAfterWait(sibSteps, sp.Ordinal)
				}
				seen := map[string]bool{}
				for i, s := range seg {
					if s.Action != protocol.ActionDropoff || s.Node == "" || seen[s.Node] {
						continue
					}
					seen[s.Node] = true
					if lifted[s.Node] || !liftsFirst(remaining, s.Node) {
						continue
					}
					pt.AwaitsLift = append(pt.AwaitsLift, protocol.LiftDependency{
						LifterUUID: sib.UUID, Node: s.Node,
						CoRelease: sp.AtWait && liftsIn(sibSeg, s.Node) && picksBefore(seg, i),
					})
				}
			}
		}
		out.Points = append(out.Points, pt)
	}
	return out
}

func stepsOfOrder(db *store.DB, id int64) []protocol.ComplexOrderStep {
	raw, err := db.GetOrderStepsJSON(id)
	if err != nil || raw == "" {
		return nil
	}
	steps, err := decodeSteps(raw)
	if err != nil {
		return nil
	}
	return steps
}

func purposesOf(steps []protocol.ComplexOrderStep) []string {
	return release.FactsFromSteps(steps, "").Purposes
}

// segmentAfterWait is the steps after the station wait with this ordinal, up
// to the next wait of any kind.
func segmentAfterWait(steps []protocol.ComplexOrderStep, ordinal int) []protocol.ComplexOrderStep {
	n := 0
	for i, s := range steps {
		if s.Action == protocol.ActionWait && protocol.IsStationWaitKind(s.WaitKind) {
			if n == ordinal {
				var out []protocol.ComplexOrderStep
				for _, t := range steps[i+1:] {
					if t.Action == protocol.ActionWait {
						break
					}
					out = append(out, t)
				}
				return out
			}
			n++
		}
	}
	return nil
}

// stationStepsFrom is the steps from the station wait with this ordinal on
// (a leg's remaining work, its next lift included).
func stationStepsFrom(steps []protocol.ComplexOrderStep, ordinal int) []protocol.ComplexOrderStep {
	if ordinal == 0 {
		return steps // not yet past a station wait: all of it, as Core reads it
	}
	n := 0
	for i, s := range steps {
		if s.Action == protocol.ActionWait && protocol.IsStationWaitKind(s.WaitKind) {
			if n == ordinal {
				return steps[i:]
			}
			n++
		}
	}
	if ordinal == 0 {
		return steps
	}
	return nil
}

func liftsFirst(steps []protocol.ComplexOrderStep, node string) bool {
	for _, s := range steps {
		if s.Node != node {
			continue
		}
		switch s.Action {
		case protocol.ActionPickup:
			return true
		case protocol.ActionDropoff:
			return false
		}
	}
	return false
}

func liftsIn(seg []protocol.ComplexOrderStep, node string) bool {
	for _, s := range seg {
		if s.Action == protocol.ActionPickup && s.Node == node {
			return true
		}
	}
	return false
}

func picksBefore(seg []protocol.ComplexOrderStep, i int) bool {
	for _, s := range seg[:i] {
		if s.Action == protocol.ActionPickup {
			return true
		}
	}
	return false
}
