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
