package engine

import (
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"shingocore/fleet"
	"shingocore/store/scene"
)

// TestRobotMotion_AMovePastTheJitterWithinTheLinger pins the moving rule every
// reader takes from the poll (fleet.RobotStatus.Moving): a move between two
// polls of more than 2% of the median row, never less than 5 cm, makes a robot
// moving until robotMoveLinger after it. A first sight is no move, and a robot
// missing from a poll is seen for the first time when it returns.
func TestRobotMotion_AMovePastTheJitterWithinTheLinger(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	at := func(x, y float64) []fleet.RobotStatus { return []fleet.RobotStatus{{VehicleID: "AMR-01", X: x, Y: y}} }
	moved := func(scale, x, y float64) bool {
		var m robotMotion
		m.mark(at(0, 0), t0, scale)
		r := at(x, y)
		m.mark(r, t0.Add(2*time.Second), scale)
		return r[0].Moving
	}
	for _, c := range []struct {
		name        string
		scale, x, y float64
		want        bool
	}{
		{"10 m rows: 19 cm is jitter", 10, 0.19, 0, false},
		{"10 m rows: 21 cm is a move", 10, 0.21, 0, true},
		{"the move is a distance, both axes counted", 10, 0.15, 0.15, true},
		{"1 m rows: the 5 cm floor holds (4 cm is jitter)", 1, 0.04, 0, false},
		{"1 m rows: 6 cm is a move", 1, 0, 0.06, true},
		{"no scene to scale by: 4 cm is jitter", 0, 0.04, 0, false},
		{"no scene to scale by: 6 cm is a move", 0, 0.06, 0, true},
		{"standing still is not a move", 10, 0, 0, false},
	} {
		if got := moved(c.scale, c.x, c.y); got != c.want {
			t.Errorf("%s: moving = %v, want %v", c.name, got, c.want)
		}
	}

	var m robotMotion
	poll := func(at time.Duration, x float64, robots ...string) map[string]bool {
		var rs []fleet.RobotStatus
		for _, id := range robots {
			rs = append(rs, fleet.RobotStatus{VehicleID: id, X: x})
		}
		m.mark(rs, t0.Add(at), 0)
		out := map[string]bool{}
		for _, r := range rs {
			out[r.VehicleID] = r.Moving
		}
		return out
	}
	if got := poll(0, 5, "AMR-01"); got["AMR-01"] {
		t.Error("first sight: moving, want not (no earlier place to have moved from)")
	}
	if got := poll(2*time.Second, 6, "AMR-01"); !got["AMR-01"] {
		t.Error("moved 1 m on the next poll: not moving, want moving at once")
	}
	if got := poll(6*time.Second, 6, "AMR-01"); !got["AMR-01"] {
		t.Error("still for 4 s after its move: not moving, want moving (inside the linger)")
	}
	if got := poll(7*time.Second, 6, "AMR-01"); got["AMR-01"] {
		t.Error("still for 5 s after its move: moving, want not (the linger ran out)")
	}
	poll(9*time.Second, 6)
	if got := poll(11*time.Second, 20, "AMR-01"); got["AMR-01"] {
		t.Error("back after a poll that did not list it, 14 m on: moving, want not (a first sight again)")
	}
}

// TestMedianRowLength_IsTheMapsScale holds Core's medianRowLength, which the
// jitter is judged against, to the scale Core's robot map works out from the
// same /api/map/edges rows (testdata/row-scale.js runs the page's own
// sceneOfRows and shared/scene-net.js). Each made-up list is built so that one
// rule decides its median: count it wrong and the two disagree. Skipped
// without node.
func TestMedianRowLength_IsTheMapsScale(t *testing.T) {
	t.Parallel()
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH")
	}
	f := func(v float64) *float64 { return &v }
	edge := func(from string, fx, fy float64, to string, tx, ty float64, h ...float64) *scene.Edge {
		e := &scene.Edge{FromName: from, FromX: fx, FromY: fy, ToName: to, ToX: tx, ToY: ty}
		if len(h) == 4 {
			e.Ctrl1X, e.Ctrl1Y, e.Ctrl2X, e.Ctrl2Y = f(h[0]), f(h[1]), f(h[2]), f(h[3])
		}
		return e
	}
	cases := []struct {
		name  string
		edges []*scene.Edge
	}{
		{"both directions of a lane are two rows (1, 1, 10: the median is 1)", []*scene.Edge{
			edge("A", 0, 0, "B", 1, 0), edge("B", 1, 0, "A", 0, 0), edge("C", 0, 5, "D", 10, 5)}},
		{"a name is case-blind and placed by the first row naming it (3 and 100.08, not 3 and 5)", []*scene.Edge{
			edge("A", 0, 0, "B", 3, 0), edge("b", 100, 0, "C", 103, 4)}},
		{"a nameless end is a point by its place", []*scene.Edge{edge("", 0, 0, "", 4, 3)}},
		{"a row from a point to itself, and one of no length, are not counted", []*scene.Edge{
			edge("A", 0, 0, "a", 7, 0), edge("B", 1, 1, "C", 1, 1), edge("", 2, 2, "", 2, 2), edge("D", 0, 0, "E", 2, 0)}},
		{"a curved row is its cubic's length", []*scene.Edge{edge("A", 0, 0, "B", 10, 0, 0, 10, 10, 10)}},
		{"a partial pair of handles is a straight row", []*scene.Edge{
			{FromName: "A", ToName: "B", ToX: 10, Ctrl1X: f(0), Ctrl1Y: f(10), Ctrl2X: f(10)}}},
		{"no rows: 0", nil},
	}
	var fixtures [][]*scene.Edge
	for _, c := range cases {
		fixtures = append(fixtures, c.edges)
	}
	rows, err := json.Marshal(fixtures)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "fixtures.json")
	if err := os.WriteFile(file, rows, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(nodePath, filepath.Join("testdata", "row-scale.js"), file).CombinedOutput()
	if err != nil {
		t.Fatalf("row-scale.js: %v\n%s", err, out)
	}
	var js []float64
	if err := json.Unmarshal(out, &js); err != nil || len(js) != len(cases) {
		t.Fatalf("row-scale.js printed %q (%v), want %d scales", out, err, len(cases))
	}
	for i, c := range cases {
		if got := medianRowLength(c.edges); math.Abs(got-js[i]) > 1e-9 {
			t.Errorf("%s: medianRowLength = %v, the map's scale = %v", c.name, got, js[i])
		}
	}
}
