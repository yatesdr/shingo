//go:build docker

package www

import (
	"net/http"
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// handlers_release_points_test.go — POST /api/release/points
// (apiReleasePoints over dispatch.ReleasePoints), the read the Edge asks before
// a release, pinned at the door: its two refusals, the empty answer, an order
// Core does not hold for the station, and one it does. What a pending segment
// enters and which lifts it waits on is dispatch's TestReleasePoints. A read
// writes nothing.
func TestReleasePointsDoor_Arms(t *testing.T) {
	t.Parallel()
	notFound := func(uuid string) protocol.ReleasePoint {
		return protocol.ReleasePoint{OrderUUID: uuid, Enters: []string{}, AwaitsLift: []protocol.LiftDependency{}}
	}
	points := func(pts ...protocol.ReleasePoint) func(*testing.T, *doorFix) (int, string) {
		return func(t *testing.T, _ *doorFix) (int, string) {
			if pts == nil {
				pts = []protocol.ReleasePoint{}
			}
			return http.StatusOK, doorJSON(t, protocol.ReleasePointsResponse{Points: pts})
		}
	}
	// staged is an order of station edge.rp staged at its first wait, its next
	// segment a drop at LINE1-IN.
	staged := func(t *testing.T, f *doorFix, uuid string, station string) {
		testdb.CreateOrder(t, f.db, func(o *orders.Order) {
			o.EdgeUUID, o.StationID, o.Status, o.WaitIndex = uuid, station, protocol.StatusStaged, 1
			o.StepsJSON = `[{"action":"pickup","node":"` + f.sd.StorageNode.Name + `"},{"action":"wait"},` +
				`{"action":"dropoff","node":"` + f.sd.LineNode.Name + `"}]`
		})
	}
	runDoorArms(t, func(h *Handlers) http.HandlerFunc { return h.apiReleasePoints }, "/api/release/points", []doorArm{
		{name: "an undecodable body is refused",
			arrange: func(*testing.T, *doorFix) (any, []int64) { return "{", nil },
			answer:  refusedWith(http.StatusBadRequest, "release points: unexpected EOF")},
		{name: "no station_id is refused",
			arrange: func(*testing.T, *doorFix) (any, []int64) {
				return map[string]any{"order_uuids": []string{"rp-1"}}, nil
			},
			answer: refusedWith(http.StatusBadRequest, "release points: station_id is required")},
		{name: "no orders asked answers an empty list",
			arrange: func(*testing.T, *doorFix) (any, []int64) { return map[string]any{"station_id": "edge.rp"}, nil },
			answer:  points()},
		{name: "an order Core does not hold is not found",
			arrange: func(*testing.T, *doorFix) (any, []int64) {
				return map[string]any{"station_id": "edge.rp", "order_uuids": []string{"rp-nobody"}}, nil
			},
			answer: points(notFound("rp-nobody"))},
		{name: "another station's order is not found",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				staged(t, f, "rp-theirs", "edge.other")
				return map[string]any{"station_id": "edge.rp", "order_uuids": []string{"rp-theirs"}}, nil
			},
			answer: points(notFound("rp-theirs"))},
		{name: "the station's staged order answers what its release enters",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				staged(t, f, "rp-mine", "edge.rp")
				return map[string]any{"station_id": "edge.rp", "order_uuids": []string{"rp-mine", "rp-nobody"}}, nil
			},
			answer: func(t *testing.T, f *doorFix) (int, string) {
				mine := notFound("rp-mine")
				mine.Found, mine.Enters = true, []string{f.sd.LineNode.Name}
				return points(mine, notFound("rp-nobody"))(t, f)
			}},
	})
}
