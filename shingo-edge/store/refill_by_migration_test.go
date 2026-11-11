package store

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"shingo/protocol"
	"shingoedge/domain"
)

// Edge v47 (refill_by_and_one_tool_change), over its three stores: the claims
// table, its quarantine mirror, and the recipes' advanced blocks. A database
// as v46 left it carries index_robot_supplies and evacuate_on_changeover on
// both claims tables and in its recipes. Boot 1 moves the first onto
// refill_by, drops both, and names each live claim whose switch was on with no
// position marked. Boot 2 changes nothing.

const (
	v47OldColumns = `SELECT COUNT(*) FROM pragma_table_info('%s')
		WHERE name IN ('index_robot_supplies', 'evacuate_on_changeover')`
	v47RefillBy = `SELECT COUNT(*) FROM pragma_table_info('%s') WHERE name = 'refill_by'`
)

func v47Probe(t *testing.T, path, query, table string) int {
	t.Helper()
	return probeRaw(t, path, strings.Replace(query, "%s", table, 1))
}

// Not parallel: it reads the standard logger, which a parallel test would
// write into. Go runs every parallel top-level test after this one returns.
func TestV47MovesTheRefillSettingAndDropsTheSwitch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v46.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
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

	// The v46 shape, on the live table and on its mirror.
	exec("old live columns", `ALTER TABLE style_node_claims ADD COLUMN evacuate_on_changeover INTEGER NOT NULL DEFAULT 0`)
	exec("old live columns", `ALTER TABLE style_node_claims ADD COLUMN index_robot_supplies INTEGER NOT NULL DEFAULT 0`)
	exec("old live columns", `ALTER TABLE style_node_claims DROP COLUMN refill_by`)
	exec("old mirror columns", `ALTER TABLE `+ClaimQuarantineTable+` ADD COLUMN evacuate_on_changeover INTEGER`)
	exec("old mirror columns", `ALTER TABLE `+ClaimQuarantineTable+` ADD COLUMN index_robot_supplies INTEGER`)
	exec("old mirror columns", `ALTER TABLE `+ClaimQuarantineTable+` DROP COLUMN refill_by`)
	exec("roll back", `DELETE FROM schema_migrations WHERE version = ?`, edgeMigrationVersion(t, "refill_by_and_one_tool_change"))

	style := exec("style", `INSERT INTO styles (name) VALUES ('V47-STYLE')`)
	claim := func(node string, robot2, evac int, marks, retired string) int64 {
		t.Helper()
		return exec("claim "+node, `INSERT INTO style_node_claims
			(style_id, core_node_name, swap_mode, index_robot_supplies, evacuate_on_changeover, changeover_evac_nodes, retired_at)
			VALUES (?, ?, 'two_robot_press_index', ?, ?, ?, NULLIF(?, ''))`, style, node, robot2, evac, marks, retired)
	}
	flipped := claim("V47-FLIPPED", 1, 0, "", "")
	unmarked := claim("V47-UNMARKED", 0, 1, "", "")
	marked := claim("V47-MARKED", 0, 1, `["V47-MARKED"]`, "")
	retired := claim("V47-RETIRED", 0, 1, "", "2026-11-01 00:00:00")
	exec("mirror rows", `INSERT INTO `+ClaimQuarantineTable+` (id, style_id, core_node_name, index_robot_supplies, evacuate_on_changeover)
		VALUES (901, ?, 'V47-Q-FLIPPED', 1, 1), (902, ?, 'V47-Q-OLD', NULL, NULL)`, style, style)

	process := exec("process", `INSERT INTO processes (name) VALUES ('V47-PROCESS')`)
	preset := func(name, flow, archived string) int64 {
		t.Helper()
		return exec("recipe "+name, `INSERT INTO flow_presets (process_id, name, version, flow_json, created_by, archived_at)
			VALUES (?, ?, 3, ?, 'seed', NULLIF(?, ''))`, process, name, flow, archived)
	}
	both := preset("Both", `{"cells":[{"core_node_name":"V47-FLIPPED","role":"produce","swap_mode":"two_robot_press_index",`+
		`"payload_code":"","advanced":{"auto_confirm":true,"evacuate_on_changeover":true,"index_robot_supplies":true}}],`+
		`"groups":[{"of":"V47-FLIPPED","name":"G","spots":[{"core_node_name":"V47-FLIPPED"}],"advanced":{"index_robot_supplies":false}}]}`, "")
	archived := preset("Archived", `{"cells":[{"core_node_name":"V47-FLIPPED","role":"produce","swap_mode":"two_robot_press_index",`+
		`"payload_code":"","advanced":{"index_robot_supplies":true}}]}`, "2026-11-02 00:00:00")
	const untouchedFlow = `{"cells":[{"core_node_name":"V47-MARKED","role":"produce","swap_mode":"two_robot","payload_code":"","advanced":{"auto_confirm":true}}]}`
	untouched := preset("Untouched", untouchedFlow, "")
	const brokenFlow = `not json`
	broken := preset("Broken", brokenFlow, "")
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	var logged bytes.Buffer
	log.SetOutput(&logged)
	first := bootAndDump(t, path)
	log.SetOutput(os.Stderr)

	for _, table := range []string{"style_node_claims", ClaimQuarantineTable} {
		if n := v47Probe(t, path, v47OldColumns, table); n != 0 {
			t.Errorf("%s keeps %d of the two old columns after the upgrade", table, n)
		}
		if n := v47Probe(t, path, v47RefillBy, table); n != 1 {
			t.Errorf("%s has no refill_by after the upgrade", table)
		}
	}

	db, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()
	refill := func(table string, id int64) string {
		t.Helper()
		var v string
		if err := db.QueryRow(`SELECT COALESCE(refill_by, '') FROM `+table+` WHERE id = ?`, id).Scan(&v); err != nil {
			t.Fatalf("%s row %d: %v", table, id, err)
		}
		return v
	}
	for _, c := range []struct {
		table string
		id    int64
		want  protocol.RefillBy
	}{
		{"style_node_claims", flipped, protocol.RefillByIndexRobot},
		{"style_node_claims", unmarked, protocol.RefillByClearingRobot},
		{"style_node_claims", marked, protocol.RefillByClearingRobot},
		{"style_node_claims", retired, protocol.RefillByClearingRobot},
		{ClaimQuarantineTable, 901, protocol.RefillByIndexRobot},
		{ClaimQuarantineTable, 902, protocol.RefillByClearingRobot},
	} {
		if got := refill(c.table, c.id); got != string(c.want) {
			t.Errorf("%s row %d: refill_by = %q, want %q", c.table, c.id, got, c.want)
		}
	}

	// One line, for the live claim whose switch was on with nothing marked.
	if out := logged.String(); !strings.Contains(out, "V47-UNMARKED") {
		t.Errorf("the upgrade did not name the claim that had the switch on and marks nothing:\n%s", out)
	} else {
		for _, quiet := range []string{"V47-MARKED", "V47-RETIRED", "V47-FLIPPED"} {
			if strings.Contains(out, quiet) {
				t.Errorf("the upgrade named %s, which marks positions, is retired, or never had the switch on:\n%s", quiet, out)
			}
		}
	}

	readFlow := func(id int64) (string, int) {
		t.Helper()
		var flow string
		var version int
		if err := db.QueryRow(`SELECT flow_json, version FROM flow_presets WHERE id = ?`, id).Scan(&flow, &version); err != nil {
			t.Fatalf("recipe %d: %v", id, err)
		}
		return flow, version
	}
	for _, id := range []int64{both, archived} {
		flow, version := readFlow(id)
		if strings.Contains(flow, "index_robot_supplies") || strings.Contains(flow, "evacuate_on_changeover") {
			t.Errorf("recipe %d keeps an old key: %s", id, flow)
		}
		if version != 3 {
			t.Errorf("recipe %d: version = %d, a migration takes no version", id, version)
		}
		cells, _, err := domain.ParseRecipe(flow)
		if err != nil || len(cells) != 1 || cells[0].Advanced == nil {
			t.Fatalf("recipe %d after the upgrade: %v %+v", id, err, cells)
		}
		if got := cells[0].Advanced.RefillBy; got != protocol.RefillByIndexRobot {
			t.Errorf("recipe %d: the cell's refill_by = %q, want index_robot", id, got)
		}
	}
	flow, _ := readFlow(both)
	cells, groups, err := domain.ParseRecipe(flow)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !cells[0].Advanced.AutoConfirm {
		t.Error("the cell's other advanced fields went with the old keys")
	}
	if len(groups) != 1 || groups[0].Name != "G" || (groups[0].Advanced != nil && groups[0].Advanced.RefillBy != "") {
		t.Errorf("the group = %+v, want it as it was, saying nothing of who refills (off is the default)", groups)
	}
	if got, _ := readFlow(untouched); got != untouchedFlow {
		t.Errorf("a recipe with neither key was rewritten: %s", got)
	}
	if got, _ := readFlow(broken); got != brokenFlow {
		t.Errorf("a recipe that does not parse was rewritten: %s", got)
	}
	before := map[int64]string{}
	for _, id := range []int64{both, archived, untouched, broken} {
		before[id], _ = readFlow(id)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second := bootAndDump(t, path)
	if first != second {
		t.Fatalf("the schema after boot 2 differs from boot 1: something re-creates a dropped column.\n%s",
			firstDumpDiff(first, second))
	}
	db, err = Open(path)
	if err != nil {
		t.Fatalf("third open: %v", err)
	}
	defer db.Close()
	for id, want := range before {
		if got, _ := readFlow(id); got != want {
			t.Errorf("recipe %d changed at the second boot:\n  was %s\n  now %s", id, want, got)
		}
	}
	if got := refill("style_node_claims", flipped); got != string(protocol.RefillByIndexRobot) {
		t.Errorf("the flipped claim reads %q after the second boot", got)
	}
}

// A fresh database is born with refill_by on both tables and neither old
// column, records v47, and two boots leave the same schema.
func TestV47FreshIsBornWithRefillBy(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "fresh.db")
	first := bootAndDump(t, path)
	second := bootAndDump(t, path)
	if first != second {
		t.Fatalf("fresh DB: boot 2 schema differs from boot 1.\n%s", firstDumpDiff(first, second))
	}
	for _, table := range []string{"style_node_claims", ClaimQuarantineTable} {
		if n := v47Probe(t, path, v47OldColumns, table); n != 0 {
			t.Errorf("a fresh %s has %d of the old columns", table, n)
		}
		if n := v47Probe(t, path, v47RefillBy, table); n != 1 {
			t.Errorf("a fresh %s has no refill_by", table)
		}
	}
	if n := probeRaw(t, path, `SELECT COUNT(*) FROM pragma_table_info('style_node_claims')
		WHERE name = 'refill_by' AND "notnull" = 1 AND dflt_value = '''clearing_robot'''`); n != 1 {
		t.Error("a fresh claims table's refill_by is not NOT NULL DEFAULT 'clearing_robot'")
	}
	if n := probeRaw(t, path, `SELECT COUNT(*) FROM schema_migrations WHERE version = `+
		strconv.Itoa(edgeMigrationVersion(t, "refill_by_and_one_tool_change"))); n != 1 {
		t.Error("v47 not recorded on a fresh DB")
	}
}
