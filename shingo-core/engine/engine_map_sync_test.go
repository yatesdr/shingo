package engine

import (
	"testing"
	"time"

	"shingocore/fleet"
	"shingocore/store/sceneversion"
)

// The map-sync gate. Both halves are pure, so both are tested without a
// database or a robot — which is the point of extracting them.

func TestMajorityMap_PicksTheMapMostOfTheFleetIsOn(t *testing.T) {
	t.Parallel()
	// The Hopkinsville shape, measured 2026-08-06: eleven robots on Hop_20 and
	// AMR-11 on Hop_21, connected, held undispatchable by RDS.
	robots := []fleet.RobotStatus{
		{VehicleID: "AMR-01", CurrentMap: "Hop_20", MapMD5: "aaa", Connected: true},
		{VehicleID: "AMR-02", CurrentMap: "Hop_20", MapMD5: "aaa", Connected: true},
		{VehicleID: "AMR-11", CurrentMap: "Hop_21", MapMD5: "bbb", Connected: true},
	}
	fm, ok := majorityMap(robots)
	if !ok {
		t.Fatal("expected a majority map")
	}
	if fm.name != "Hop_20" || fm.md5 != "aaa" {
		t.Errorf("majority = %s/%s, want Hop_20/aaa", fm.name, fm.md5)
	}
	if fm.minority != 1 {
		t.Errorf("minority = %d, want 1 — the robot on the other map is COUNTED, "+
			"not silently ignored", fm.minority)
	}
	if fm.vehicle != "AMR-01" && fm.vehicle != "AMR-02" {
		t.Errorf("vehicle = %q, want one of the robots actually running Hop_20", fm.vehicle)
	}
}

// An empty hash is "we did not collect it", not a vote.
//
// Same rule the roll-up applies to map_mismatch — a row that predates the
// column does not know its map, and treating that as agreement would let a
// fleet with no hashes at all elect a map.
func TestMajorityMap_RobotsWithNoHashDoNotVote(t *testing.T) {
	t.Parallel()
	robots := []fleet.RobotStatus{
		{VehicleID: "AMR-01", CurrentMap: "SPRAMRMAP", MapMD5: "", Connected: true},
		{VehicleID: "AMR-02", CurrentMap: "", MapMD5: "aaa", Connected: true},
		{VehicleID: "", CurrentMap: "SPRAMRMAP", MapMD5: "aaa", Connected: true},
	}
	if fm, ok := majorityMap(robots); ok {
		t.Errorf("elected %+v from robots that reported no usable map hash", fm)
	}
}

// A fleet split evenly must pick the SAME map on every pass.
//
// Without a deterministic tie-break the gate alternates, and each alternation
// rewrites the whole area set and records it as an edit — a diff log full of
// changes nobody made, which is the exact failure the idempotent-on-content
// rule exists to prevent, arriving through the caller instead.
func TestMajorityMap_TieIsBrokenDeterministically(t *testing.T) {
	t.Parallel()
	robots := []fleet.RobotStatus{
		{VehicleID: "AMR-01", CurrentMap: "Zeta", MapMD5: "z", Connected: true},
		{VehicleID: "AMR-02", CurrentMap: "Alpha", MapMD5: "a", Connected: true},
	}
	first, ok := majorityMap(robots)
	if !ok {
		t.Fatal("expected a majority map")
	}
	// Re-run over a reordered slice: Go map iteration is randomised, so an
	// order-sensitive tie-break shows up here rather than once in production.
	for i := 0; i < 50; i++ {
		again, ok := majorityMap([]fleet.RobotStatus{robots[1], robots[0]})
		if !ok {
			t.Fatal("expected a majority map")
		}
		if again.name != first.name {
			t.Fatalf("tie broke differently across runs: %s then %s", first.name, again.name)
		}
	}
	if first.name != "Alpha" {
		t.Errorf("tie picked %q, want Alpha (lowest name)", first.name)
	}
}

func TestMapFetchReason(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name       string
		prev       sceneversion.MapVersionState
		found      bool
		wireMD5    string
		wantFetch  bool
		wantReason string
	}{
		{
			// The state this branch is in on every plant right now.
			name: "never archived", found: false, wireMD5: "aaa",
			wantFetch: true, wantReason: "no version archived yet",
		},
		{
			name:  "hash moved",
			prev:  sceneversion.MapVersionState{MapMD5: "aaa", SyncedAt: now.Add(-time.Hour)},
			found: true, wireMD5: "bbb",
			wantFetch: true, wantReason: "map hash moved",
		},
		{
			name:  "unchanged and recent",
			prev:  sceneversion.MapVersionState{MapMD5: "aaa", SyncedAt: now.Add(-time.Hour)},
			found: true, wireMD5: "aaa",
			wantFetch: false,
		},
		{
			// The backstop. Without it a hash that STOPPED ARRIVING and a map
			// that stopped changing are the same observation.
			name:  "unchanged but past the floor",
			prev:  sceneversion.MapVersionState{MapMD5: "aaa", SyncedAt: now.Add(-25 * time.Hour)},
			found: true, wireMD5: "aaa",
			wantFetch: true, wantReason: "daily floor",
		},
		{
			// Exactly at the floor counts: >= not >.
			name:  "unchanged exactly at the floor",
			prev:  sceneversion.MapVersionState{MapMD5: "aaa", SyncedAt: now.Add(-mapSyncFloor)},
			found: true, wireMD5: "aaa",
			wantFetch: true, wantReason: "daily floor",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason, fetch := mapFetchReason(tc.prev, tc.found, tc.wireMD5, now)
			if fetch != tc.wantFetch {
				t.Fatalf("fetch = %v, want %v (reason %q)", fetch, tc.wantFetch, reason)
			}
			if fetch && reason != tc.wantReason {
				t.Errorf("reason = %q, want %q — the three reasons are different "+
					"facts and the log has to say which fired", reason, tc.wantReason)
			}
		})
	}
}

// A DISCONNECTED ROBOT NEITHER VOTES NOR SERVES.
//
// This is the one that cost an afternoon. RDS keeps publishing a dropped
// robot's cached basic_info -- name, map, hash, all present -- so it reads as a
// perfectly good source. Ask it for its map and RDS times out and answers
// {"code":0,"msg":"ok"}: a success envelope for a robot that is not there.
//
// Measured at Springfield 2026-08-07, AMR-01 at connection_status 0 returned
// exactly that for every Robokit call, while AMR-03 returned the whole 7.3 MB
// map in 1.7 s.
func TestMajorityMap_SkipsDisconnectedRobots(t *testing.T) {
	t.Parallel()
	robots := []fleet.RobotStatus{
		// Offline, and still advertising a map it cannot serve. It is also the
		// MAJORITY here, so a filter applied only at serve time would still
		// elect the wrong map.
		{VehicleID: "AMR-01", CurrentMap: "OLD_MAP", MapMD5: "stale", Connected: false},
		{VehicleID: "AMR-11", CurrentMap: "OLD_MAP", MapMD5: "stale", Connected: false},
		{VehicleID: "AMR-03", CurrentMap: "SPRAMRMAP", MapMD5: "live", Connected: true},
	}
	fm, ok := majorityMap(robots)
	if !ok {
		t.Fatal("expected a majority map from the one connected robot")
	}
	if fm.name != "SPRAMRMAP" || fm.md5 != "live" {
		t.Errorf("elected %s/%s, want SPRAMRMAP/live — a dropped robot's hash is a "+
			"memory of whenever it dropped, and during a fleet-wide map push a "+
			"stale vote is a vote for the old map", fm.name, fm.md5)
	}
	if fm.vehicle != "AMR-03" {
		t.Errorf("would fetch from %q, want AMR-03 — asking an offline robot gets a "+
			"success envelope and no map", fm.vehicle)
	}
	if fm.minority != 0 {
		t.Errorf("minority = %d, want 0 — offline robots are not a minority MAP, "+
			"they are absent, and counting them would report a split that is "+
			"really an outage", fm.minority)
	}
}

// Every robot offline is "we cannot say", not a map.
func TestMajorityMap_AllDisconnectedElectsNothing(t *testing.T) {
	t.Parallel()
	if fm, ok := majorityMap([]fleet.RobotStatus{
		{VehicleID: "AMR-01", CurrentMap: "SPRAMRMAP", MapMD5: "aaa", Connected: false},
	}); ok {
		t.Errorf("elected %+v from a fleet with nothing connected", fm)
	}
}
