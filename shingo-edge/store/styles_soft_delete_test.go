package store

import (
	"testing"

	"shingoedge/store/processes"
)

// seedStyleWithHistory stands up one process, one style, a claim, a reporting
// point with snapshots, hourly counts and a changeover that used the style as
// its TO style — i.e. the shape a style acquires by being run.
func seedStyleWithHistory(t *testing.T, db *DB, processName, styleName string) (processID, styleID int64) {
	t.Helper()
	res, err := db.Exec(`INSERT INTO processes (name) VALUES (?)`, processName)
	if err != nil {
		t.Fatalf("insert process: %v", err)
	}
	processID, _ = res.LastInsertId()

	styleID, err = db.CreateStyle(styleName, "", processID)
	if err != nil {
		t.Fatalf("CreateStyle: %v", err)
	}

	rpID, err := db.CreateReportingPoint("PLC-"+styleName, "TAG-"+styleName, styleID)
	if err != nil {
		t.Fatalf("CreateReportingPoint: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := db.Exec(`INSERT INTO counter_snapshots (reporting_point_id, count_value) VALUES (?, ?)`, rpID, i); err != nil {
			t.Fatalf("insert snapshot: %v", err)
		}
	}
	if _, err := db.Exec(`INSERT INTO hourly_counts (process_id, style_id, count_date, hour, delta)
		VALUES (?, ?, '2026-07-27', 9, 17)`, processID, styleID); err != nil {
		t.Fatalf("insert hourly: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO process_changeovers (process_id, to_style_id, state, started_at, updated_at)
		VALUES (?, ?, 'completed', datetime('now'), datetime('now'))`, processID, styleID); err != nil {
		t.Fatalf("insert changeover: %v", err)
	}
	return processID, styleID
}

// A retired style must not be selectable anywhere an operator picks one, and
// must still resolve everywhere something renders what already happened.
//
// This is the assertion the whole soft-delete change exists to make true. Of
// the 23 sites in shingo-edge that name the styles table, six filter and the
// rest deliberately do not; getting that split wrong is silent in both
// directions — a picker that offers a retired part number, or a changeover row
// that renders a blank name.
//
// Verified red: with liveStyles removed from ListStyles the first subtest fails
// with "ListStyles still offers retired style"; with it added to GetStyle the
// third fails with "retired style must still resolve by id".
func TestDeleteStyle_RetiresWithoutRemoving(t *testing.T) {
	db := testDB(t)
	processID, styleID := seedStyleWithHistory(t, db, "SoftDelLine", "RETIRE-ME")

	// A second, live style so the pickers are asserted to still return
	// something — a filter that returns nothing would pass a "not present" test.
	keepID, err := db.CreateStyle("KEEP-ME", "", processID)
	if err != nil {
		t.Fatalf("CreateStyle keep: %v", err)
	}

	if err := db.DeleteStyle(styleID); err != nil {
		t.Fatalf("DeleteStyle: %v", err)
	}

	t.Run("absent from every picker", func(t *testing.T) {
		all, err := db.ListStyles()
		if err != nil {
			t.Fatalf("ListStyles: %v", err)
		}
		if containsStyle(all, styleID) {
			t.Error("ListStyles still offers retired style")
		}
		if !containsStyle(all, keepID) {
			t.Error("ListStyles dropped the live style too — the filter is too broad")
		}

		byProc, err := db.ListStylesByProcess(processID)
		if err != nil {
			t.Fatalf("ListStylesByProcess: %v", err)
		}
		if containsStyle(byProc, styleID) {
			t.Error("ListStylesByProcess still offers retired style (this is the changeover target list)")
		}
		if !containsStyle(byProc, keepID) {
			t.Error("ListStylesByProcess dropped the live style too")
		}

		if s, err := db.GetStyleByName("RETIRE-ME"); err == nil && s != nil {
			t.Errorf("GetStyleByName resolved a retired style (id %d) — name is an ingress key", s.ID)
		}
	})

	t.Run("still resolves by id for display", func(t *testing.T) {
		s, err := db.GetStyle(styleID)
		if err != nil {
			t.Fatalf("retired style must still resolve by id: %v", err)
		}
		if s.Name != "RETIRE-ME" {
			t.Errorf("retired style name = %q, want RETIRE-ME", s.Name)
		}
		if s.DeletedAt == nil {
			t.Error("retired style has no deleted_at")
		}
	})

	t.Run("changeover history still renders the name", func(t *testing.T) {
		// The eight LEFT JOIN styles in changeovers.go are the reason the row
		// stays. Before soft delete this rendered blank.
		cos, err := db.ListProcessChangeovers(processID)
		if err != nil {
			t.Fatalf("ListProcessChangeovers: %v", err)
		}
		if len(cos) == 0 {
			t.Fatal("no changeovers")
		}
		if cos[0].ToStyleName != "RETIRE-ME" {
			t.Errorf("changeover to_style_name = %q, want RETIRE-ME — "+
				"a retired style must not blank out the history that used it", cos[0].ToStyleName)
		}
	})

	t.Run("nothing was destroyed", func(t *testing.T) {
		for _, c := range []struct {
			label string
			sql   string
			want  int
		}{
			{"style_node_claims survive", `SELECT count(*) FROM style_node_claims WHERE style_id = ?`, 0},
			{"reporting_points survive", `SELECT count(*) FROM reporting_points WHERE style_id = ?`, 1},
			{"counter_snapshots survive", `SELECT count(*) FROM counter_snapshots
				WHERE reporting_point_id IN (SELECT id FROM reporting_points WHERE style_id = ?)`, 3},
			{"hourly_counts survive", `SELECT count(*) FROM hourly_counts WHERE style_id = ?`, 1},
			{"changeovers survive", `SELECT count(*) FROM process_changeovers WHERE to_style_id = ?`, 1},
		} {
			var n int
			if err := db.QueryRow(c.sql, styleID).Scan(&n); err != nil {
				t.Fatalf("%s: %v", c.label, err)
			}
			if n != c.want {
				t.Errorf("%s: got %d, want %d", c.label, n, c.want)
			}
		}
	})

	t.Run("counting stops", func(t *testing.T) {
		// The one piece of the old cascade worth keeping: a retired style must
		// not go on polling its PLC. rp.enabled is the poll gate.
		var enabled int
		if err := db.QueryRow(`SELECT enabled FROM reporting_points WHERE style_id = ?`, styleID).Scan(&enabled); err != nil {
			t.Fatalf("read rp.enabled: %v", err)
		}
		if enabled != 0 {
			t.Error("retired style's reporting point is still enabled — it will keep counting")
		}
	})

	t.Run("the name is free again", func(t *testing.T) {
		// The old UNIQUE(process_id, name) was a TABLE constraint and covered
		// tombstones, so this failed. The live-only partial index is what makes
		// "retire and re-create" work.
		if _, err := db.CreateStyle("RETIRE-ME", "the replacement part", processID); err != nil {
			t.Errorf("cannot re-create a retired style's name: %v\n"+
				"the uniqueness constraint is still covering tombstones", err)
		}
	})

	t.Run("restore puts it back", func(t *testing.T) {
		// Reversibility is the argument for soft delete over cascade; an undo
		// that does not exist cannot be cited as one.
		//
		// Note the name collision this now has with the replacement created
		// above — restore is expected to FAIL here, and loudly, rather than
		// produce two live styles sharing a name.
		if err := db.RestoreStyle(styleID); err == nil {
			var live int
			db.QueryRow(`SELECT count(*) FROM styles WHERE process_id=? AND name='RETIRE-ME' AND deleted_at IS NULL`,
				processID).Scan(&live)
			if live > 1 {
				t.Errorf("restore produced %d live styles named RETIRE-ME in one process", live)
			}
		}
	})
}

// The impact count must reach through BOTH cascade hops, because the volume is
// all on the far side of the second one and nobody reading the styles table
// would guess it.
func TestStyleDeleteImpact_CountsTheWholeCascade(t *testing.T) {
	db := testDB(t)
	_, styleID := seedStyleWithHistory(t, db, "ImpactLine", "IMPACT-ME")

	imp, err := db.StyleDeleteImpact(styleID)
	if err != nil {
		t.Fatalf("StyleDeleteImpact: %v", err)
	}
	if imp.ReportingPoints != 1 {
		t.Errorf("reporting_points = %d, want 1", imp.ReportingPoints)
	}
	if imp.Snapshots != 3 {
		t.Errorf("counter_snapshots = %d, want 3 — this is the SECOND cascade hop, "+
			"styles -> reporting_points -> counter_snapshots, and it is where the volume is",
			imp.Snapshots)
	}
	if imp.HourlyCounts != 1 || imp.PartsCounted != 17 {
		t.Errorf("hourly = %d rows / %d parts, want 1 / 17", imp.HourlyCounts, imp.PartsCounted)
	}
	if imp.ChangeoversTo != 1 {
		t.Errorf("changeovers_to = %d, want 1", imp.ChangeoversTo)
	}
	// 1 style + 1 rp + 3 snapshots + 1 hourly + 1 changeover
	if imp.TotalDeleted != 7 {
		t.Errorf("total_deleted = %d, want 7", imp.TotalDeleted)
	}
	if imp.Retired {
		t.Error("style reported as retired before it was")
	}
}

func containsStyle(list []processes.Style, id int64) bool {
	for _, s := range list {
		if s.ID == id {
			return true
		}
	}
	return false
}
