//go:build docker

package telemetry_test

import (
	"math"
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/telemetry"
)

// U3's route index, against a real Postgres.
//
// The whole figure lives in one SQL statement — percentile_cont twice, a CTE
// chain, and a join on a string-concatenation expression that has to be the SAME
// expression the by-route grouping uses. None of that can be checked by reading
// it, and the failure modes are quiet ones: a route median taken over the wrong
// partition still returns a plausible number, and a floor applied in the wrong
// CTE silently changes which missions count.
//
// The fixture is built so the right answer is arithmetic rather than
// approximately-right — and the FIRST version of it was wrong in a way worth
// recording, because it is the trap this whole figure sits on. It gave each route
// four missions from AMR-01 at the nominal duration and four from AMR-02 at twice
// it, then expected AMR-01 to index 1.0. It measured 0.667, correctly: with half
// the route's missions at 2x, the route's own median is 1.5x nominal, so the
// robot running at nominal is BELOW its route's median. A robot's index moves when
// another robot's behaviour changes, because they share the denominator. That is a
// real property of the figure and not a fixture artefact, and any reading of the
// column has to hold it in mind.
//
// So the fixture gives every route a majority of BASELINE missions that pin its
// median, and measures two robots against it:
//
//	route LONG  (SM -> PLN): 6x AMR-BASE @1000 + 2x AMR-01 @1000 + 2x AMR-02 @2000
//	                         n=10, median 1000, QUALIFIES at 8
//	route SHORT (LN -> LN2): 6x AMR-BASE @100  + 2x AMR-01 @100  + 2x AMR-02 @200
//	                         n=10, median 100,  QUALIFIES at 8
//	route RARE  (X -> Y)   : 2x AMR-03 @9000/9100 — n=2, does NOT qualify
//
// AMR-BASE -> 1.0 over 12 · AMR-01 -> 1.0 over 4 · AMR-02 -> 2.0 over 4 ·
// AMR-03 -> NO INDEX, and must be absent from the map rather than present at 0.
func seedMission(t *testing.T, db *store.DB, orderID int64, station, robot, src, dst string, durMS int64) {
	t.Helper()
	if err := telemetry.UpsertMission(db.DB, &telemetry.Mission{
		OrderID: orderID, RobotID: robot, StationID: station,
		SourceNode: src, DeliveryNode: dst,
		TerminalState: "FINISHED", DurationMS: durMS,
		BlocksJSON: "[]", ErrorsJSON: "[]", WarningsJSON: "[]", NoticesJSON: "[]",
	}); err != nil {
		t.Fatalf("seed mission %d: %v", orderID, err)
	}
}

func TestRouteIndex_Arithmetic(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	var id int64 = 71000
	next := func() int64 { id++; return id }

	// LONG: 6 baseline @1000 pin the median, then 2 at nominal and 2 at twice it.
	for i := 0; i < 6; i++ {
		seedMission(t, db, next(), "RI", "AMR-BASE", "RI-SM", "RI-PLN", 1000)
	}
	for i := 0; i < 2; i++ {
		seedMission(t, db, next(), "RI", "AMR-01", "RI-SM", "RI-PLN", 1000)
	}
	for i := 0; i < 2; i++ {
		seedMission(t, db, next(), "RI", "AMR-02", "RI-SM", "RI-PLN", 2000)
	}
	// SHORT: same shape, one order of magnitude down, so a single global median
	// cannot accidentally satisfy both routes.
	for i := 0; i < 6; i++ {
		seedMission(t, db, next(), "RI", "AMR-BASE", "RI-LN", "RI-LN2", 100)
	}
	for i := 0; i < 2; i++ {
		seedMission(t, db, next(), "RI", "AMR-01", "RI-LN", "RI-LN2", 100)
	}
	for i := 0; i < 2; i++ {
		seedMission(t, db, next(), "RI", "AMR-02", "RI-LN", "RI-LN2", 200)
	}
	// RARE: 2 missions, below the floor. AMR-03 only.
	seedMission(t, db, next(), "RI", "AMR-03", "RI-X", "RI-Y", 9000)
	seedMission(t, db, next(), "RI", "AMR-03", "RI-X", "RI-Y", 9100)

	idx, qualifying, err := telemetry.GetRobotRouteIndex(db.DB, telemetry.Filter{StationID: "RI"}, 8)
	if err != nil {
		t.Fatalf("GetRobotRouteIndex: %v", err)
	}

	if qualifying != 2 {
		t.Errorf("qualifying routes = %d, want 2 (LONG and SHORT clear 8; RARE has 2)", qualifying)
	}

	// LONG's median is 1000 and SHORT's is 100 — one order of magnitude apart. If
	// the query took ONE median over all missions instead of one per route, no
	// robot could index at 1.0 on both, which is what these two assertions catch.
	if got, ok := idx["AMR-01"]; !ok {
		t.Error("AMR-01 has no index, but it ran 4 missions on two qualifying routes")
	} else {
		if math.Abs(got.Index-1.0) > 1e-9 {
			t.Errorf("AMR-01 index = %v, want 1.0 — it runs both routes at exactly their median", got.Index)
		}
		if got.Samples != 4 {
			t.Errorf("AMR-01 index samples = %d, want 4", got.Samples)
		}
	}

	if got, ok := idx["AMR-02"]; !ok {
		t.Error("AMR-02 has no index, but it ran 4 missions on two qualifying routes")
	} else {
		if math.Abs(got.Index-2.0) > 1e-9 {
			t.Errorf("AMR-02 index = %v, want 2.0 — it takes twice each route's median", got.Index)
		}
		if got.Samples != 4 {
			t.Errorf("AMR-02 index samples = %d, want 4", got.Samples)
		}
	}

	// The baseline robot indexes at 1.0 over all twelve of its missions. It is
	// here to pin the medians, and its own figure is the sanity check that the
	// denominator is what the fixture says.
	if got, ok := idx["AMR-BASE"]; !ok {
		t.Error("AMR-BASE has no index")
	} else if math.Abs(got.Index-1.0) > 1e-9 || got.Samples != 12 {
		t.Errorf("AMR-BASE index = %v over %d samples, want 1.0 over 12", got.Index, got.Samples)
	}

	// The load-bearing absence. AMR-03's only route has 2 missions, so it has no
	// denominator — and it must be ABSENT from the map, not present at 0 or at 1.
	// A robot that ran only unqualified routes reading 1.0x would say "perfectly
	// average" about a robot nothing was measured for.
	if got, ok := idx["AMR-03"]; ok {
		t.Errorf("AMR-03 has index %v over %d samples, but its only route (2 missions) is below the floor — "+
			"it must be absent so the caller can render an absence rather than a value", got.Index, got.Samples)
	}
}

// TestRouteIndex_ImmuneToRouteMix is the test that justifies the whole change.
//
// Two robots run at IDENTICAL speed relative to their routes. One is given the
// long haul and the other the short hop, which is RDS's decision and not the
// robots'. The mean duration says one is 10x the other; the index says they are
// the same, which is the true statement about the robots.
//
// VERIFIED RED BY: changing the denominator from the mission's own route median
// to a single global median — AMR-SLOW then indexes at ~1.8 and AMR-FAST at
// ~0.18, reproducing exactly the false conclusion the bar list used to draw.
func TestRouteIndex_ImmuneToRouteMix(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	var id int64 = 72000
	next := func() int64 { id++; return id }

	// The long haul: 8 missions, all AMR-SLOW, all 5000 ms. Median 5000.
	for i := 0; i < 8; i++ {
		seedMission(t, db, next(), "MX", "AMR-SLOW", "MX-SM", "MX-PLN", 5000)
	}
	// The short hop: 8 missions, all AMR-FAST, all 500 ms. Median 500.
	for i := 0; i < 8; i++ {
		seedMission(t, db, next(), "MX", "AMR-FAST", "MX-LN", "MX-LN2", 500)
	}

	idx, qualifying, err := telemetry.GetRobotRouteIndex(db.DB, telemetry.Filter{StationID: "MX"}, 8)
	if err != nil {
		t.Fatalf("GetRobotRouteIndex: %v", err)
	}
	if qualifying != 2 {
		t.Fatalf("qualifying routes = %d, want 2", qualifying)
	}

	slow, okS := idx["AMR-SLOW"]
	fast, okF := idx["AMR-FAST"]
	if !okS || !okF {
		t.Fatalf("missing an index: slow=%v fast=%v", okS, okF)
	}
	// Both robots are exactly at their own route's median, so both index at 1.0
	// even though their mean durations differ by 10x.
	if math.Abs(slow.Index-1.0) > 1e-9 || math.Abs(fast.Index-1.0) > 1e-9 {
		t.Errorf("indices = slow %v / fast %v, want 1.0 / 1.0 — the index must not carry the route mix "+
			"(mean durations here are 5000 vs 500, a 10x difference that says nothing about the robots)",
			slow.Index, fast.Index)
	}
}

// TestRouteIndex_NoRouteQualifies covers the state that drops the COLUMN rather
// than dashing a cell, and it is the reason GetRobotRouteIndex returns a count
// separately from the map.
//
// With every route below the floor the map is empty — and an empty map on its own
// cannot distinguish "no route qualified" from "no robot ran on the ones that
// did". Reporting the first from the second would be this repo's reachability
// defect again: reading an absence as a positive finding.
func TestRouteIndex_NoRouteQualifies(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	var id int64 = 73000
	seedMission(t, db, id+1, "NQ", "AMR-01", "NQ-A", "NQ-B", 1000)
	seedMission(t, db, id+2, "NQ", "AMR-01", "NQ-C", "NQ-D", 2000)

	idx, qualifying, err := telemetry.GetRobotRouteIndex(db.DB, telemetry.Filter{StationID: "NQ"}, 8)
	if err != nil {
		t.Fatalf("GetRobotRouteIndex: %v", err)
	}
	if len(idx) != 0 {
		t.Errorf("index map has %d entries, want 0 — no route has 8 missions", len(idx))
	}
	if qualifying != 0 {
		t.Errorf("qualifying routes = %d, want 0 — this is the value that drops the column", qualifying)
	}
}

// TestRouteIndex_ExcludesNonPositiveDurations pins the guard every other duration
// query in this package carries.
//
// The sim writes a negative duration_ms on nearly every row (clock skew), and a
// negative numerator produces a NEGATIVE index — a robot reading as faster than
// instantaneous, sorted to the top of any ascending column. The same rows also
// must not count toward a route's sample floor, or a route made entirely of
// skewed rows would qualify with a median of garbage.
func TestRouteIndex_ExcludesNonPositiveDurations(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	var id int64 = 74000
	next := func() int64 { id++; return id }

	// 8 good missions on one route: qualifies, median 1000.
	for i := 0; i < 8; i++ {
		seedMission(t, db, next(), "NP", "AMR-01", "NP-SM", "NP-PLN", 1000)
	}
	// 8 skewed missions on another: must NOT qualify, because none of them count.
	for i := 0; i < 8; i++ {
		seedMission(t, db, next(), "NP", "AMR-02", "NP-X", "NP-Y", -3600000)
	}

	idx, qualifying, err := telemetry.GetRobotRouteIndex(db.DB, telemetry.Filter{StationID: "NP"}, 8)
	if err != nil {
		t.Fatalf("GetRobotRouteIndex: %v", err)
	}
	if qualifying != 1 {
		t.Errorf("qualifying routes = %d, want 1 — the all-negative route must not qualify", qualifying)
	}
	if got, ok := idx["AMR-02"]; ok {
		t.Errorf("AMR-02 has index %v, but every one of its missions has a non-positive duration", got.Index)
	}
	if got, ok := idx["AMR-01"]; !ok || math.Abs(got.Index-1.0) > 1e-9 {
		t.Errorf("AMR-01 index = %v (present=%v), want 1.0", got.Index, ok)
	}
}
