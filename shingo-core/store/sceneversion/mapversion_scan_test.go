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

// EVERY VERSION KEEPS ITS SCAN. PIN: A is superseded by B and still holds its
// cloud; B, the current version, holds its own; a retired map name's single
// open row holds its own.
func TestPin_C_SupersededVersionKeepsItsScan(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	archive(t, db, "PIN_SCAN", "A", at)
	archive(t, db, "PIN_SCAN", "B", at.Add(time.Hour))
	archive(t, db, "PIN_SCAN_RETIRED", "A", at)

	if got := hasCloud(t, db, "PIN_SCAN"); !got["A"] || !got["B"] {
		t.Errorf("clouds held %v, want both at the base", got)
	}
	if got := hasCloud(t, db, "PIN_SCAN_RETIRED"); !got["A"] {
		t.Errorf("the retired name's open row lost its cloud: %v", got)
	}
}
