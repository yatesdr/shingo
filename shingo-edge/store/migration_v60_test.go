package store

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"shingo/protocol/testutil"
)

// TestV60_RebuildsWithoutLosing: after v60 on the plant shape that needs every
// rebuild, legacy_alter_table is back off on the store's one connection, no
// scratch or renamed-away name is left in the stored schema, and the check
// that re-runs v60 holds.
func TestV60_RebuildsWithoutLosing(t *testing.T) {
	t.Parallel()
	db, err := Open(plantShapeDB(t, "plant_b_v59.sql"))
	testutil.MustNoErr(t, err, "start")
	defer db.Close()
	var legacy int
	testutil.MustNoErr(t, db.QueryRow(`PRAGMA legacy_alter_table`).Scan(&legacy), "read the pragma")
	if legacy != 0 {
		t.Error("legacy_alter_table was left on")
	}
	for _, name := range []string{"_v60", "styles_legacy", "processes_legacy", `REFERENCES payloads`} {
		if left := queryLines(t, db.DB, `SELECT name FROM sqlite_master WHERE instr(sql, ?) > 0`, name); len(left) > 0 {
			t.Errorf("%q is left in the stored schema of %v", name, left)
		}
	}
	if !v60Verify(db.DB) {
		t.Error("v60's Verify does not hold on what v60 left")
	}
}

// TestV60_AFailureChangesNothing: v60 is one transaction, and it will not
// rebuild over a column, index or trigger it does not know: it fails naming
// it, and every row, object and version is as it was.
func TestV60_AFailureChangesNothing(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ plant, want string }{
		{`ALTER TABLE process_changeovers ADD COLUMN by_hand TEXT`, "column by_hand"},
		{`CREATE INDEX idx_by_hand ON process_changeovers(state)`, "index idx_by_hand"},
		{`CREATE TRIGGER by_hand AFTER DELETE ON process_changeovers BEGIN SELECT 1; END`, "trigger by_hand"},
	} {
		t.Run(c.want, func(t *testing.T) {
			t.Parallel()
			path := plantShapeDB(t, "plant_b_v59.sql")
			raw := rawDB(t, path)
			execAll(t, raw, c.plant)
			testutil.MustNoErr(t, raw.Close(), "close")
			before := fileState(t, path)
			err := startEdge(t, path)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("start = %v, want v60's failure naming %s", err, c.want)
			}
			after := fileState(t, path)
			for _, key := range slices.Sorted(maps.Keys(before)) {
				if before[key] != after[key] {
					t.Errorf("a failed v60 changed %s", key)
				}
			}
		})
	}
}
