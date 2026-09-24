//go:build docker

package sceneversion_test

import (
	"fmt"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/scenemap"
	"shingocore/store"
	"shingocore/store/sceneversion"
)

// mapversion_scan_test.go — which map versions keep their laser scan
// (memory close-out addendum C, 2026-09-24). scan_cloud_gz is 90-92% of
// scene_map_versions at both plants and nothing reads it.

func smap(name, tag string) []byte {
	return []byte(fmt.Sprintf(`{"header":{"mapName":%q,"resolution":0.02,"version":%q},`+
		`"normalPosList":[{"x":1,"y":2},{"x":3,"y":4},{"x":5,"y":6}]}`, name, tag))
}

func archive(t *testing.T, db *store.DB, name, tag string, at time.Time) {
	t.Helper()
	raw := smap(name, tag)
	parsed, err := scenemap.Parse(raw)
	testutil.MustNoErr(t, err, "parse")
	_, err = db.ApplyMapSnapshot(sceneversion.MapSnapshot{
		MapName: name, MapMD5: name + tag, SourceRobot: "AMR-T", Raw: raw, Parsed: parsed, ObservedAt: at,
	}, nil)
	testutil.MustNoErr(t, err, "apply "+name+" "+tag)
}

// hasCloud maps each archived version (by its tag) to whether it still holds
// its scan cloud.
func hasCloud(t *testing.T, db *store.DB, name string) map[string]bool {
	t.Helper()
	rows, err := db.Query(`SELECT map_md5, scan_cloud_gz IS NOT NULL FROM scene_map_versions WHERE map_name = $1`, name)
	testutil.MustNoErr(t, err, "read versions")
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var md5 string
		var has bool
		testutil.MustNoErr(t, rows.Scan(&md5, &has), "scan")
		out[md5[len(name):]] = has
	}
	testutil.MustNoErr(t, rows.Err(), "rows")
	return out
}

// A SUPERSEDED VERSION KEEPS ITS MAP BUT NOT ITS LASER SCAN. A is superseded
// by B and loses scan_cloud_gz, keeping its row and body_gz; B, the current
// version, keeps its scan; a retired map name's single open row keeps its own.
// Inverts the pin that every version kept its scan.
func TestMapScan_SupersededVersionLosesOnlyItsScan(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	archive(t, db, "PIN_SCAN", "A", at)
	archive(t, db, "PIN_SCAN", "B", at.Add(time.Hour))
	archive(t, db, "PIN_SCAN_RETIRED", "A", at)

	if got := hasCloud(t, db, "PIN_SCAN"); got["A"] || !got["B"] {
		t.Errorf("clouds held %v, want A dropped and B kept", got)
	}
	if got := hasCloud(t, db, "PIN_SCAN_RETIRED"); !got["A"] {
		t.Errorf("the retired name's open row lost its cloud: %v", got)
	}
	var bodies int
	testutil.MustNoErr(t, db.QueryRow(`SELECT count(*) FROM scene_map_versions
		WHERE map_name = 'PIN_SCAN' AND body_gz IS NOT NULL`).Scan(&bodies), "count bodies")
	if bodies != 2 {
		t.Errorf("%d versions keep their map body, want both", bodies)
	}
}

// AN EDIT BACK GETS ITS SCAN BACK. A, then B, then A again: A is current once
// more, and the current version holds its scan, taken again from the bytes the
// robot just sent. B is superseded and loses its own.
func TestMapScan_EditedBackVersionGetsItsScanBack(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	archive(t, db, "PIN_SCAN_ABA", "A", at)
	archive(t, db, "PIN_SCAN_ABA", "B", at.Add(time.Hour))
	archive(t, db, "PIN_SCAN_ABA", "A", at.Add(2*time.Hour))

	if got := hasCloud(t, db, "PIN_SCAN_ABA"); !got["A"] || got["B"] {
		t.Errorf("clouds held %v, want A (current again) kept and B dropped", got)
	}
}
