package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Edge v48 (drop_style_node_claims_reorder_point_source). The reorder-point
// provenance stamp ("legacy", "manual", "unset", "calculated", or whatever the
// replenishment page was sent) leaves both claims tables. Nothing decided on
// it, so on a database carrying every value it ever held, each row's reorder
// point, Auto reorder, level and every other column read the same before the
// upgrade and after; the column does not come back at the second boot (the
// frozen chain's ALTER went with it, as v42's did).

const reorderSourceColumn = "reorder_point_source"

func reorderSourceProbe(table string) string {
	return `SELECT COUNT(*) FROM pragma_table_info('` + table + `') WHERE name = '` + reorderSourceColumn + `'`
}

// plantReorderSource gives a just-opened DB the shape an Edge before the drop
// left: the column on both claims tables, and the drop not yet applied.
func plantReorderSource(t *testing.T, db *DB, path string) {
	t.Helper()
	for table, decl := range map[string]string{
		"style_node_claims":  `TEXT NOT NULL DEFAULT 'legacy'`,
		ClaimQuarantineTable: `TEXT`, // the mirror's columns carry no constraint
	} {
		if probeRaw(t, path, reorderSourceProbe(table)) == 0 {
			if _, err := db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + reorderSourceColumn + ` ` + decl); err != nil {
				t.Fatalf("plant %s.%s: %v", table, reorderSourceColumn, err)
			}
		}
	}
	for _, m := range edgeMigrations() {
		if strings.Contains(m.Name, "reorder_point_source") {
			if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version = ?`, m.Version); err != nil {
				t.Fatalf("unapply v%d: %v", m.Version, err)
			}
		}
	}
}

// claimRowsLessSource is every row of table, every column but the stamp,
// one line per row in id order.
func claimRowsLessSource(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT * FROM ` + table + ` ORDER BY id`)
	if err != nil {
		t.Fatalf("read %s: %v", table, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("%s columns: %v", table, err)
	}
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan %s: %v", table, err)
		}
		var fields []string
		for i, c := range cols {
			if c == reorderSourceColumn {
				continue
			}
			v := vals[i]
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			fields = append(fields, fmt.Sprintf("%s=%v", c, v))
		}
		sort.Strings(fields)
		out = append(out, strings.Join(fields, " "))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s rows: %v", table, err)
	}
	return out
}

// claimReads is what the engine reads off each claim: the reorder point,
// Auto reorder, the level it orders against and the level's falling edge.
func claimReads(t *testing.T, db *DB, ids []int64) []string {
	t.Helper()
	var out []string
	for _, id := range ids {
		c, err := db.GetStyleNodeClaim(id)
		if err != nil {
			t.Fatalf("claim %d: %v", id, err)
		}
		out = append(out, fmt.Sprintf("%d %s point=%d auto=%v level=%d below=%v",
			id, c.Role, c.ReorderPoint, c.AutoReorder, c.DemandLevel(), c.BelowReorderSince))
	}
	return out
}

func TestReorderSourceDrop_EveryRowReadsTheSame(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "aged.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	plantReorderSource(t, db, path)
	exec := func(what, stmt string, args ...any) int64 {
		t.Helper()
		res, err := db.Exec(stmt, args...)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	exec("catalog", `INSERT INTO payload_catalog (id, name, code, uop_capacity) VALUES (1, 'RPS', 'RPS-PART', 120)`)
	style := exec("style", `INSERT INTO styles (name) VALUES ('RPS-STYLE')`)

	var ids []int64
	n := 0
	for _, source := range []string{"legacy", "manual", "unset", "", "calculated", "anything typed"} {
		for _, role := range []string{"consume", "produce"} {
			for _, point := range []int{0, 40} {
				for _, auto := range []int{0, 1} {
					n++
					below := any(nil)
					if n%3 == 0 {
						below = "2026-11-10 06:00:00"
					}
					ids = append(ids, exec("claim", `INSERT INTO style_node_claims
						(style_id, core_node_name, role, swap_mode, payload_code, reorder_point, auto_reorder,
						 reorder_point_source, below_reorder_since)
						VALUES (?, ?, ?, 'two_robot', 'RPS-PART', ?, ?, ?, ?)`,
						style, fmt.Sprintf("RPS-%02d", n), role, point, auto, source, below))
				}
			}
		}
	}
	ids = append(ids, exec("manual_swap claim", `INSERT INTO style_node_claims
		(style_id, core_node_name, role, swap_mode, payload_code, allowed_payload_codes, reorder_point, auto_reorder, reorder_point_source)
		VALUES (?, 'RPS-MANUAL', 'consume', 'manual_swap', 'RPS-PART', '["RPS-PART"]', 25, 1, 'manual')`, style))
	ids = append(ids, exec("retired claim", `INSERT INTO style_node_claims
		(style_id, core_node_name, role, swap_mode, payload_code, reorder_point, auto_reorder, reorder_point_source, retired_at)
		VALUES (?, 'RPS-RETIRED', 'consume', 'two_robot', 'RPS-PART', 30, 1, 'calculated', '2026-11-01 00:00:00')`, style))
	exec("mirror rows", `INSERT INTO `+ClaimQuarantineTable+`
		(id, style_id, core_node_name, reorder_point, auto_reorder, reorder_point_source)
		VALUES (901, ?, 'RPS-Q-MANUAL', 15, 1, 'manual'), (902, ?, 'RPS-Q-NULL', 0, 0, NULL)`, style, style)

	beforeLive := claimRowsLessSource(t, db.DB, "style_node_claims")
	beforeMirror := claimRowsLessSource(t, db.DB, ClaimQuarantineTable)
	beforeReads := claimReads(t, db, ids)
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	first := bootAndDump(t, path)
	for _, table := range []string{"style_node_claims", ClaimQuarantineTable} {
		if probeRaw(t, path, reorderSourceProbe(table)) != 0 {
			t.Errorf("%s keeps %s after the upgrade", table, reorderSourceColumn)
		}
	}
	if second := bootAndDump(t, path); first != second {
		t.Fatalf("the schema after boot 2 differs from boot 1 — something re-creates the dropped column.\n%s",
			firstDumpDiff(first, second))
	}

	db, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()
	for _, c := range []struct {
		what          string
		before, after []string
	}{
		{"style_node_claims rows", beforeLive, claimRowsLessSource(t, db.DB, "style_node_claims")},
		{"quarantine rows", beforeMirror, claimRowsLessSource(t, db.DB, ClaimQuarantineTable)},
		{"claim reads", beforeReads, claimReads(t, db, ids)},
	} {
		if strings.Join(c.before, "\n") != strings.Join(c.after, "\n") {
			t.Errorf("%s moved across the upgrade:\nbefore:\n%s\nafter:\n%s",
				c.what, strings.Join(c.before, "\n"), strings.Join(c.after, "\n"))
		}
	}
	if len(beforeLive) != len(ids) || len(beforeMirror) != 2 {
		t.Errorf("seeded %d live and %d mirror rows, want %d and 2", len(beforeLive), len(beforeMirror), len(ids))
	}
}

// A fresh database never carries the column, on either claims table, and a
// second boot changes nothing.
func TestReorderSourceDrop_FreshNeverHasIt(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "fresh.db")
	first := bootAndDump(t, path)
	if second := bootAndDump(t, path); first != second {
		t.Fatalf("fresh DB: boot 2 schema differs from boot 1.\n%s", firstDumpDiff(first, second))
	}
	for _, table := range []string{"style_node_claims", ClaimQuarantineTable} {
		if probeRaw(t, path, reorderSourceProbe(table)) != 0 {
			t.Errorf("a fresh %s has %s", table, reorderSourceColumn)
		}
	}
}
