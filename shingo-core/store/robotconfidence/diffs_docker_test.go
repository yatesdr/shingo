//go:build docker

package robotconfidence_test

import (
	"testing"
	"time"
)

// RecentSceneDiffs lists EDITS, not archives.
//
// The .smap sync path commits its diff row even when nothing moved — the row
// is the map version's provenance, and it must keep existing. The change rail
// is a different audience: a "0 changed · 0 added · 0 removed" entry is noise
// that buries the real edits, and at Springfield it was half the list. The
// read filters it; the write keeps it.
func TestRecentSceneDiffs_ListsEditsNotArchives(t *testing.T) {
	t.Parallel()
	db := openWithWindow(t)

	// The three rows: a real edit, an empty archive row, then a second real
	// edit — so ordering among the survivors is also pinned.
	mk := func(at time.Time, changed int) int64 {
		t.Helper()
		var id int64
		if err := db.QueryRow(
			`INSERT INTO scene_diffs (source, gate_hash, observed_at,
			                          objects_changed)
			 VALUES ('scene-sync', 'h', $1, $2) RETURNING id`,
			at, changed).Scan(&id); err != nil {
			t.Fatalf("insert diff: %v", err)
		}
		if _, err := db.Exec(
			`UPDATE scene_diffs SET objects_added = 0, objects_removed = 0
			  WHERE id = $1`, id); err != nil {
			t.Fatalf("complete diff: %v", err)
		}
		return id
	}
	first := mk(testDay.AddDate(0, 0, -2), 3)
	empty := mk(testDay.AddDate(0, 0, -1), 0)
	second := mk(testDay, 1)
	_ = empty

	diffs, err := db.RecentSceneDiffs(10)
	if err != nil {
		t.Fatalf("recent diffs: %v", err)
	}
	if len(diffs) != 2 {
		t.Fatalf("got %d diffs, want 2 (the empty archive row must not list): %v",
			len(diffs), diffs)
	}
	if diffs[0].ID != second || diffs[1].ID != first {
		t.Errorf("order = [%d, %d], want newest-first [%d, %d]",
			diffs[0].ID, diffs[1].ID, second, first)
	}
}

// LanesChangedByDiffs answers for a page of diffs in one query, and it has to
// answer for EVERY id it was handed — including ones that changed no lanes.
//
// The per-diff form it replaced returned `[]string{}` for a diff with no lane
// rows, and that is not interchangeable with nil here: the localization board
// reads this straight out of JSON, where nil marshals to `null` and empty to
// `[]`. A batched query that simply omits absent ids would flip that for every
// archive-only diff on the rail.
func TestLanesChangedByDiffs_GroupsByDiffAndKeepsEmptyNonNil(t *testing.T) {
	t.Parallel()
	db := openWithWindow(t)

	mkDiff := func(at time.Time) int64 {
		t.Helper()
		var id int64
		if err := db.QueryRow(
			`INSERT INTO scene_diffs (source, gate_hash, observed_at, objects_changed)
			 VALUES ('scene-sync', 'h', $1, 1) RETURNING id`, at).Scan(&id); err != nil {
			t.Fatalf("insert diff: %v", err)
		}
		return id
	}
	mkLane := func(diffID int64, area, lane string) {
		t.Helper()
		if _, err := db.Exec(
			`INSERT INTO scene_lane_versions
			   (area_name, lane, shape_hash, def_hash, shape, directed_rows, diff_id, valid_from)
			 VALUES ($1, $2, 'sh', 'df', '{}'::jsonb, 1, $3, $4)`,
			area, lane, diffID, testDay); err != nil {
			t.Fatalf("insert lane version: %v", err)
		}
	}

	twoLanes := mkDiff(testDay.AddDate(0, 0, -2))
	oneLane := mkDiff(testDay.AddDate(0, 0, -1))
	noLanes := mkDiff(testDay)

	// LN_01 twice on one diff, in two areas -- the same lane name genuinely
	// occurs in more than one area, and the partial unique index allows only one
	// OPEN row per (area, lane), so this is how the DISTINCT gets exercised at
	// all. The board wants lane names, not (area, lane) pairs.
	mkLane(twoLanes, "area-1", "LN_02")
	mkLane(twoLanes, "area-1", "LN_01")
	mkLane(twoLanes, "area-2", "LN_01")
	mkLane(oneLane, "area-1", "LN_09")

	got, err := db.LanesChangedByDiffs([]int64{twoLanes, oneLane, noLanes})
	if err != nil {
		t.Fatalf("lanes by diffs: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d entries, want one per requested id: %v", len(got), got)
	}
	if want := []string{"LN_01", "LN_02"}; !equalStrings(got[twoLanes], want) {
		t.Errorf("twoLanes = %v, want %v (deduped and lane-ordered)", got[twoLanes], want)
	}
	if want := []string{"LN_09"}; !equalStrings(got[oneLane], want) {
		t.Errorf("oneLane = %v, want %v", got[oneLane], want)
	}
	empty, ok := got[noLanes]
	if !ok {
		t.Fatal("a diff that changed no lanes has no entry; every requested id must get one")
	}
	if empty == nil {
		t.Error("a diff that changed no lanes returned nil, want an empty slice: nil marshals to null, not []")
	}
	if len(empty) != 0 {
		t.Errorf("noLanes = %v, want empty", empty)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
