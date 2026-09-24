//go:build docker

package engine

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/scenemap"
	"shingocore/store"
	"shingocore/store/sceneversion"
)

// map_sync_passes_test.go — how often the map sync fetches a map, pass by pass
// (memory close-out addendum B, 2026-09-24).
//
// passes runs mapSyncPass's decision and apply steps against a real database
// every 5 minutes of simulated time: read the latest version, ask
// mapFetchReason, and on a fetch archive what the robot sent. It stands in for
// the robot download, which is the part this counts: each fetch is one full
// .smap pulled over the robot link (16 MB at Hopkinsville).

// testSmap is a minimal .smap: a header and a scan cloud of three points. tag
// changes the bytes, which is what a map edit does to its content hash.
func testSmap(name, tag string) []byte {
	return []byte(fmt.Sprintf(`{"header":{"mapName":%q,"resolution":0.02,"version":%q},`+
		`"normalPosList":[{"x":1,"y":2},{"x":3,"y":4},{"x":5,"y":6}]}`, name, tag))
}

func wireMD5(raw []byte) string {
	sum := md5.Sum(raw)
	return hex.EncodeToString(sum[:])
}

// passes runs n passes starting at start, the robot serving raw each pass, and
// returns how many fetched.
func passes(t *testing.T, db *store.DB, raw []byte, start time.Time, n int) int {
	t.Helper()
	parsed, err := scenemap.Parse(raw)
	testutil.MustNoErr(t, err, "parse test map")
	fetches := 0
	for i := 0; i < n; i++ {
		now := start.Add(time.Duration(i) * mapSyncInterval)
		prev, found, err := db.LatestMapVersion(parsed.Name)
		testutil.MustNoErr(t, err, "latest map version")
		if _, fetch := mapFetchReason(prev, found, wireMD5(raw), now); !fetch {
			continue
		}
		fetches++
		var previous *time.Time
		if found {
			at := prev.SyncedAt
			previous = &at
		}
		_, err = db.ApplyMapSnapshot(sceneversion.MapSnapshot{
			MapName: parsed.Name, MapMD5: wireMD5(raw), SourceRobot: "AMR-T",
			Raw: raw, Parsed: parsed, ObservedAt: now,
		}, previous)
		testutil.MustNoErr(t, err, "apply map snapshot")
	}
	return fetches
}

const passesPerDay = int(24 * time.Hour / mapSyncInterval)

// A STABLE MAP, TWO DAYS OF PASSES: ONE FETCH PER FLOOR. The first pass
// archives it; the fetch at the 24 h floor finds it unchanged and records it
// confirmed, and the floor is measured from that. 2 of the 576 passes fetch.
// Inverts the pin that 289 did (1 + every pass after the floor).
func TestMapSync_StableMapIsFetchedOncePerFloor(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if got := passes(t, db, testSmap("PIN_STABLE", "A"), start, 2*passesPerDay); got != 2 {
		t.Errorf("%d fetches in two days, want 2 (one archive, one confirmation at the floor)", got)
	}
}

// A MAP EDITED AND EDITED BACK (A, then B, then A again): ONE FETCH. The fetch
// finds A's content in an older row, makes that row the current version again
// and supersedes B, so the next pass's latest version matches the robot. Inverts
// the pin that every pass of the hour fetched.
func TestMapSync_EditedBackMapIsFetchedOnce(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	a, b := testSmap("PIN_ABA", "A"), testSmap("PIN_ABA", "B")
	passes(t, db, a, start, 1)
	passes(t, db, b, start.Add(time.Hour), 1)

	const hour = int(time.Hour / mapSyncInterval)
	if got := passes(t, db, a, start.Add(2*time.Hour), hour); got != 1 {
		t.Errorf("%d fetches in the hour after the edit back, want 1", got)
	}
	latest, found, err := db.LatestMapVersion("PIN_ABA")
	testutil.MustNoErr(t, err, "latest map version")
	if !found || latest.MapMD5 != wireMD5(a) {
		t.Errorf("latest version is %q, want A's %q", latest.MapMD5, wireMD5(a))
	}
	var open int
	testutil.MustNoErr(t, db.QueryRow(`SELECT count(*) FROM scene_map_versions
		WHERE map_name = 'PIN_ABA' AND superseded_at IS NULL`).Scan(&open), "count open versions")
	if open != 1 {
		t.Errorf("%d open versions of PIN_ABA, want 1", open)
	}
}
