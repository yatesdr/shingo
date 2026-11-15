package engine

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"shingocore/fleet"
	"shingocore/scenesync"
	"shingocore/store/scene"
)

// routeScene is a synthetic stored scene: LM7 → LM31 → AP32 → CP40, plus LM12
// and AP12, two names that share the number 12. Ids and names invented.
func routeScene(extra ...string) *scenesync.PointIndex {
	edges := []*scene.Edge{
		{FromName: "LM7", ToName: "LM31"},
		{FromName: "LM31", ToName: "AP32"},
		{FromName: "AP32", ToName: "CP40"},
		{FromName: "LM12", ToName: "AP12"},
	}
	for _, name := range extra {
		edges = append(edges, &scene.Edge{FromName: "CP40", ToName: name})
	}
	return scenesync.NewPointIndex(nil, edges)
}

// TestResolveRobotPaths_NamesRoutesAndLogsOncePerChange pins the poll's
// half of the route: each robot's Path is its PathIDs as scene names in
// order, a route with an unnameable id carries no Path at all, and the line
// saying so is written when the failure changes, never on every 2 s poll.
func TestResolveRobotPaths_NamesRoutesAndLogsOncePerChange(t *testing.T) {
	t.Parallel()
	var logged []string
	e := &Engine{logFn: func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }}
	e.pointIndex.Store(routeScene())
	notDrawn := map[string]routeNotDrawn{}

	poll := func(routes map[string][]string) map[string][]string {
		robots := make([]fleet.RobotStatus, 0, len(routes))
		for _, id := range []string{"AMR-01", "AMR-02", "AMR-03", "AMR-04"} {
			if ids, ok := routes[id]; ok {
				robots = append(robots, fleet.RobotStatus{VehicleID: id, PathIDs: ids})
			}
		}
		e.resolveRobotPaths(robots, notDrawn)
		out := map[string][]string{}
		for _, r := range robots {
			out[r.VehicleID] = r.Path
		}
		return out
	}
	want := func(step string, got map[string][]string, id string, path ...string) {
		t.Helper()
		if !slices.Equal(got[id], path) {
			t.Errorf("%s: %s Path = %v, want %v", step, id, got[id], path)
		}
	}
	wantLogs := func(step string, n int, contains ...string) {
		t.Helper()
		if len(logged) != n {
			t.Fatalf("%s: %d log lines, want %d: %q", step, len(logged), n, logged)
		}
		for _, c := range contains {
			if !strings.Contains(strings.Join(logged, "\n"), c) {
				t.Errorf("%s: no log line says %q: %q", step, c, logged)
			}
		}
	}

	first := map[string][]string{
		"AMR-01": {"7", "31", "32", "40"}, // drawable
		"AMR-02": {"7", "31", "99"},       // 99 names no point
		"AMR-03": {"31", "12"},            // 12 names two
		"AMR-04": nil,                     // idle
	}
	got := poll(first)
	want("poll 1", got, "AMR-01", "LM7", "LM31", "AP32", "CP40")
	want("poll 1", got, "AMR-02")
	want("poll 1", got, "AMR-03")
	want("poll 1", got, "AMR-04")
	wantLogs("poll 1", 2, `AMR-02: no route drawn: no scene point is numbered "99"`,
		`AMR-03: no route drawn: point "12" could be any of AP12, LM12`)

	// The same routes on the next polls, and AMR-02 a point further on with
	// the same unknown destination: nothing new to say.
	poll(first)
	got = poll(map[string][]string{"AMR-01": {"31", "32", "40"}, "AMR-02": {"31", "99"}, "AMR-03": {"31", "12"}})
	want("poll 3", got, "AMR-01", "LM31", "AP32", "CP40")
	wantLogs("poll 3", 2)

	// AMR-02 is sent somewhere drawable, then back to the unknown point: the
	// failure is new again, so it is said again.
	got = poll(map[string][]string{"AMR-02": {"31", "32"}})
	want("poll 4", got, "AMR-02", "LM31", "AP32")
	poll(map[string][]string{"AMR-02": {"32", "99"}})
	wantLogs("poll 5", 3, `AMR-02: no route drawn: no scene point is numbered "99"`)

	// The scene resyncs with point 99 in it: the same ids now draw.
	e.pointIndex.Store(routeScene("LM99"))
	got = poll(map[string][]string{"AMR-02": {"32", "40", "99"}})
	want("after resync", got, "AMR-02", "AP32", "CP40", "LM99")
	wantLogs("after resync", 3)
}

// Before any scene is stored there is nothing to name from: no robot gets a
// route, and each says why once.
func TestResolveRobotPaths_NoSceneYet(t *testing.T) {
	t.Parallel()
	var logged []string
	e := &Engine{logFn: func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }}
	notDrawn := map[string]routeNotDrawn{}
	for range 3 {
		robots := []fleet.RobotStatus{{VehicleID: "AMR-01", PathIDs: []string{"7", "31"}}}
		e.resolveRobotPaths(robots, notDrawn)
		if robots[0].Path != nil {
			t.Fatalf("Path = %v with no scene stored, want none", robots[0].Path)
		}
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "no synced scene") {
		t.Errorf("log = %q, want one line saying there is no synced scene", logged)
	}
}
