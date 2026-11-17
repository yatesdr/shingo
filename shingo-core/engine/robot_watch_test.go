package engine

import (
	"slices"
	"testing"
	"time"

	"shingo/protocol"
	"shingocore/fleet"
	"shingocore/store/orders"
)

// TestRobotWatches_LeaseRunsFifteenSecondsFromTheLastWatch pins Core's lease:
// a watch is live until protocol.RobotWatchLease after the last one from its
// station, a later watch replaces the uuids and runs the lease again, and a
// lapsed lease is forgotten. Times are explicit; nothing sleeps.
func TestRobotWatches_LeaseRunsFifteenSecondsFromTheLastWatch(t *testing.T) {
	t.Parallel()
	var w robotWatches
	t0 := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	if got := w.live(t0); got != nil {
		t.Fatalf("no watch yet: live = %v, want nil", got)
	}
	w.renew("edge.a", []string{"u-1"}, t0)
	w.renew("edge.b", []string{"u-9"}, t0.Add(5*time.Second))

	stations := func(at time.Duration) []string {
		var out []string
		for _, sw := range w.live(t0.Add(at)) {
			out = append(out, sw.station)
		}
		return out
	}
	if got := stations(14 * time.Second); !slices.Equal(got, []string{"edge.a", "edge.b"}) {
		t.Errorf("14 s after edge.a's watch: live %v, want both", got)
	}
	if got := stations(15 * time.Second); !slices.Equal(got, []string{"edge.b"}) {
		t.Errorf("15 s after edge.a's watch: live %v, want edge.b alone (edge.a lapsed)", got)
	}
	// The lapsed lease is gone: a later read does not bring it back.
	w.renew("edge.b", []string{"u-9", "u-10"}, t0.Add(16*time.Second))
	got := w.live(t0.Add(25 * time.Second))
	if len(got) != 1 || got[0].station != "edge.b" || !slices.Equal(got[0].uuids, []string{"u-9", "u-10"}) {
		t.Errorf("after edge.b renewed with two orders: live %+v, want edge.b alone with [u-9 u-10]", got)
	}
	if got := stations(31 * time.Second); got != nil {
		t.Errorf("15 s after the last watch of all: live %v, want none", got)
	}
}

// TestHandleRobotWatch_KeysOnTheEnvelopesStation pins the watch door: the
// lease is the envelope's source station, its uuids sorted and each once; a
// body naming another station is dropped.
func TestHandleRobotWatch_KeysOnTheEnvelopesStation(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	from := func(station string) *protocol.Envelope {
		return &protocol.Envelope{Src: protocol.Address{Role: protocol.RoleEdge, Station: station}}
	}
	e.HandleRobotWatch(from("edge.a"), &protocol.RobotWatch{StationID: "edge.a", OrderUUIDs: []string{"u-2", "u-1", "u-2"}})
	e.HandleRobotWatch(from("edge.a"), &protocol.RobotWatch{StationID: "edge.b", OrderUUIDs: []string{"u-9"}})
	e.HandleRobotWatch(from(""), &protocol.RobotWatch{OrderUUIDs: []string{"u-8"}})

	got := e.robotWatches.live(time.Now())
	if len(got) != 1 || got[0].station != "edge.a" || !slices.Equal(got[0].uuids, []string{"u-1", "u-2"}) {
		t.Fatalf("live = %+v, want edge.a alone watching [u-1 u-2]", got)
	}
}

// TestBuildRobotPositions_OwnedOrdersOnAPolledRobot pins what one poll sends
// each watching station: the watched orders that are its own and whose robot
// the poll listed connected, with the robot's position, heading, whether it is
// moving and its named route; nothing for another station's order, an order
// with no robot row, or a robot absent from the poll or disconnected; and an
// empty orders list, not no message, for a station with none.
func TestBuildRobotPositions_OwnedOrdersOnAPolledRobot(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 14, 9, 0, 0, 987_654_321, time.UTC)
	watches := []stationWatch{
		{station: "edge.a", uuids: []string{"u-absent", "u-down", "u-mine", "u-norow", "u-theirs"}},
		{station: "edge.b", uuids: []string{"u-mine"}},
	}
	rows := []orders.OrderRobot{
		{UUID: "u-mine", StationID: "edge.a", RobotID: "AMR-01"},
		{UUID: "u-theirs", StationID: "edge.c", RobotID: "AMR-02"},
		{UUID: "u-absent", StationID: "edge.a", RobotID: "AMR-09"},
		{UUID: "u-down", StationID: "edge.a", RobotID: "AMR-03"},
	}
	robots := []fleet.RobotStatus{
		{VehicleID: "AMR-01", Connected: true, X: 12.5, Y: -3.25, Angle: 1.5708, Moving: true, Path: []string{"LM7", "LM31", "AP32"}},
		{VehicleID: "AMR-02", Connected: true, X: 1, Y: 1},
		{VehicleID: "AMR-03", Connected: false, X: 4, Y: 4},
	}

	got := buildRobotPositions(watches, rows, robots, at)
	if len(got) != 2 || got[0].station != "edge.a" || got[1].station != "edge.b" {
		t.Fatalf("built for %+v, want one message each for edge.a and edge.b", got)
	}
	a := got[0].body
	if !a.T.Equal(at.Truncate(time.Millisecond)) || a.IntervalMS != protocol.RobotPollInterval.Milliseconds() {
		t.Errorf("edge.a's t/interval = %s/%d, want the poll's time to the ms and %d", a.T, a.IntervalMS, protocol.RobotPollInterval.Milliseconds())
	}
	want := protocol.RobotPosition{OrderUUID: "u-mine", RobotID: "AMR-01", X: 12.5, Y: -3.25, Heading: 1.5708,
		Moving: true, Path: []string{"LM7", "LM31", "AP32"}}
	if len(a.Orders) != 1 || a.Orders[0].OrderUUID != want.OrderUUID || a.Orders[0].RobotID != want.RobotID ||
		a.Orders[0].X != want.X || a.Orders[0].Y != want.Y || a.Orders[0].Heading != want.Heading ||
		a.Orders[0].Moving != want.Moving || !slices.Equal(a.Orders[0].Path, want.Path) {
		t.Errorf("edge.a's orders = %+v, want only %+v", a.Orders, want)
	}
	if b := got[1].body; b.Orders == nil || len(b.Orders) != 0 {
		t.Errorf("edge.b watches an order that is edge.a's: orders = %#v, want [] (a message, with nothing in it)", b.Orders)
	}
}
