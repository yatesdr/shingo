package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Edge v55 (drop_style_node_claims_auto_request_payload). Auto request part
// leaves both claims tables. Nothing has read it since decffaa0 (2026-04-10),
// so on a database that holds values (SPR's shape: five claims at one
// position, one on the running style) and on one that holds none, every row's
// every other column, the claims the engine reads and the process's running
// style read the same before the upgrade and after; the column does not come
// back at the second boot.

const autoRequestColumn = "auto_request_payload"

func autoRequestProbe(table string) string {
	return `SELECT COUNT(*) FROM pragma_table_info('` + table + `') WHERE name = '` + autoRequestColumn + `'`
}

// plantAutoRequest gives a just-opened DB the shape an Edge before the drop
// left: the column on both claims tables, and the drop not yet applied.
func plantAutoRequest(t *testing.T, db *DB, path string) {
	t.Helper()
	for table, decl := range map[string]string{
		"style_node_claims":  `TEXT NOT NULL DEFAULT ''`,
		ClaimQuarantineTable: `TEXT`, // the mirror's columns carry no constraint
	} {
		if probeRaw(t, path, autoRequestProbe(table)) == 0 {
			if _, err := db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + autoRequestColumn + ` ` + decl); err != nil {
				t.Fatalf("plant %s.%s: %v", table, autoRequestColumn, err)
			}
		}
	}
	for _, m := range edgeMigrations() {
		if strings.Contains(m.Name, autoRequestColumn) {
			if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version = ?`, m.Version); err != nil {
				t.Fatalf("unapply v%d: %v", m.Version, err)
			}
		}
	}
}

// rowsLessAutoRequest is every row of table, every column but the dropped
// one, one line per row in id order.
func rowsLessAutoRequest(t *testing.T, db *sql.DB, table string) []string {
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
			if c == autoRequestColumn {
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

// engineReads is each claim as the engine reads it (its JSON, less the
// dropped key while the struct still has it) and the process's running style.
func engineReads(t *testing.T, db *DB, processID int64, ids []int64) []string {
	t.Helper()
	var out []string
	for _, id := range ids {
		c, err := db.GetStyleNodeClaim(id)
		if err != nil {
			t.Fatalf("claim %d: %v", id, err)
		}
		raw, err := json.Marshal(c)
		if err != nil {
			t.Fatalf("marshal claim %d: %v", id, err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("claim %d: %v", id, err)
		}
		delete(m, autoRequestColumn)
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var fields []string
		for _, k := range keys {
			fields = append(fields, fmt.Sprintf("%s=%v", k, m[k]))
		}
		out = append(out, strings.Join(fields, " "))
	}
	p, err := db.GetProcess(processID)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	out = append(out, fmt.Sprintf("running style %v", *p.ActiveStyleID))
	return out
}

// seedAutoRequestPlant is SPR's shape: one process (SNF3) running one of five
// styles, each with a produce claim at ALN_011 naming its own part as Auto
// request part when held, and a consume claim elsewhere with none; a retired
// claim and two quarantined rows besides.
func seedAutoRequestPlant(t *testing.T, db *DB, held bool) (processID int64, ids []int64) {
	t.Helper()
	exec := func(what, stmt string, args ...any) int64 {
		t.Helper()
		res, err := db.Exec(stmt, args...)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			t.Fatalf("%s: last insert id: %v", what, err)
		}
		return id
	}
	processID = exec("process", `INSERT INTO processes (name) VALUES ('SNF3')`)
	var styles []int64
	for i := 1; i <= 5; i++ {
		part := fmt.Sprintf("ARP-PART-%d", i)
		exec("catalog", `INSERT INTO payload_catalog (id, name, code, uop_capacity) VALUES (?, ?, ?, 60)`, i, part, part)
		style := exec("style", `INSERT INTO styles (name, process_id) VALUES (?, ?)`, part, processID)
		styles = append(styles, style)
		value := ""
		if held {
			value = part
		}
		ids = append(ids, exec("produce claim", `INSERT INTO style_node_claims
			(style_id, core_node_name, role, swap_mode, payload_code, inbound_staging, inbound_source, outbound_destination,
			 allowed_payload_codes, auto_request_payload, reorder_point, auto_reorder)
			VALUES (?, 'ALN_011', 'produce', 'two_robot', ?, 'ARP-STG', 'ARP-SRC', 'ARP-DST', ?, ?, 0, 0)`,
			style, part, `["`+part+`"]`, value))
		ids = append(ids, exec("consume claim", `INSERT INTO style_node_claims
			(style_id, core_node_name, role, swap_mode, payload_code, inbound_staging, inbound_source, outbound_destination,
			 reorder_point, auto_reorder)
			VALUES (?, 'ALN_012', 'consume', 'two_robot', ?, 'ARP-STG', 'ARP-SRC', 'ARP-DST', 20, 1)`, style, part))
	}
	if _, err := db.Exec(`UPDATE processes SET active_style_id = ? WHERE id = ?`, styles[1], processID); err != nil {
		t.Fatalf("running style: %v", err)
	}
	retired := ""
	if held {
		retired = "ARP-PART-1"
	}
	ids = append(ids, exec("retired claim", `INSERT INTO style_node_claims
		(style_id, core_node_name, role, swap_mode, payload_code, auto_request_payload, retired_at)
		VALUES (?, 'ALN_013', 'produce', 'two_robot', 'ARP-PART-1', ?, '2026-11-01 00:00:00')`, styles[0], retired))
	mirror := any(nil)
	if held {
		mirror = "ARP-PART-2"
	}
	exec("mirror rows", `INSERT INTO `+ClaimQuarantineTable+`
		(id, style_id, core_node_name, swap_mode, auto_request_payload)
		VALUES (901, ?, 'ARP-Q-1', 'manual_swap', ?), (902, ?, 'ARP-Q-2', 'manual_swap', NULL)`, styles[2], mirror, styles[2])
	return processID, ids
}

func TestAutoRequestDrop_EveryRowReadsTheSame(t *testing.T) {
	t.Parallel()
	for _, held := range []bool{true, false} {
		name := map[bool]string{true: "holds values (SPR's shape)", false: "holds none"}[held]
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "aged.db")
			db, err := Open(path)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			plantAutoRequest(t, db, path)
			processID, ids := seedAutoRequestPlant(t, db, held)
			var n int
			if err := db.QueryRow(`SELECT COUNT(*) FROM style_node_claims WHERE auto_request_payload != ''`).Scan(&n); err != nil {
				t.Fatalf("count held: %v", err)
			}
			if want := map[bool]int{true: 6, false: 0}[held]; n != want {
				t.Fatalf("seeded %d claims holding a value, want %d", n, want)
			}
			beforeLive := rowsLessAutoRequest(t, db.DB, "style_node_claims")
			beforeMirror := rowsLessAutoRequest(t, db.DB, ClaimQuarantineTable)
			beforeReads := engineReads(t, db, processID, ids)
			if err := db.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}

			first := bootAndDump(t, path)
			for _, table := range []string{"style_node_claims", ClaimQuarantineTable} {
				if probeRaw(t, path, autoRequestProbe(table)) != 0 {
					t.Errorf("%s keeps %s after the upgrade", table, autoRequestColumn)
				}
			}
			if second := bootAndDump(t, path); first != second {
				t.Fatalf("the schema after boot 2 differs from boot 1: something re-creates the dropped column.\n%s",
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
				{"style_node_claims rows", beforeLive, rowsLessAutoRequest(t, db.DB, "style_node_claims")},
				{"quarantine rows", beforeMirror, rowsLessAutoRequest(t, db.DB, ClaimQuarantineTable)},
				{"engine reads", beforeReads, engineReads(t, db, processID, ids)},
			} {
				if strings.Join(c.before, "\n") != strings.Join(c.after, "\n") {
					t.Errorf("%s moved across the upgrade:\nbefore:\n%s\nafter:\n%s",
						c.what, strings.Join(c.before, "\n"), strings.Join(c.after, "\n"))
				}
			}
			if len(beforeLive) != len(ids) || len(beforeMirror) != 2 {
				t.Errorf("seeded %d live and %d mirror rows, want %d and 2", len(beforeLive), len(beforeMirror), len(ids))
			}
		})
	}
}

// A fresh database never carries the column, on either claims table, and a
// second boot changes nothing.
func TestAutoRequestDrop_FreshNeverHasIt(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "fresh.db")
	first := bootAndDump(t, path)
	if second := bootAndDump(t, path); first != second {
		t.Fatalf("fresh DB: boot 2 schema differs from boot 1.\n%s", firstDumpDiff(first, second))
	}
	for _, table := range []string{"style_node_claims", ClaimQuarantineTable} {
		if probeRaw(t, path, autoRequestProbe(table)) != 0 {
			t.Errorf("a fresh %s has %s", table, autoRequestColumn)
		}
	}
}
