package store

import (
	"database/sql"
	"fmt"
	"log"
	"slices"
	"strings"

	"shingo/protocol/migrate"
)

// Migration 60: the plants' Edge files converge on the baseline, and the
// columns and tables nothing reads are dropped. One transaction (the runner's).
//
// What it drops is what RetiredChainRelease runs without: that release boots
// and reads orders, processes and claims on what v60 leaves
// (TestRetiredChainReleaseRunsOnThisSchema). Its own chain adds the nine claim
// columns back when it starts; Verify sees them and v60 runs again at this
// build's next start. There is no down step.
//
// It is frozen once shipped: the DDL below is what v60 left, and the baseline
// moves on without it.

// v60Columns is every column v60 drops, where it stands.
var v60Columns = []struct {
	table   string
	columns []string
}{
	{"orders", []string{"load_type", "bin_uop_remaining", "payload_id"}},
	{"style_node_claims", []string{
		"uop_capacity", "staging_node", "release_node",
		"inbound_source_node", "inbound_source_node_group",
		"outbound_source_node", "outbound_source_node_group", "outbound_source",
		"mode",
	}},
	{"core_loaders", []string{"config_gen"}},
	{"core_loader_positions", []string{"min_stock"}},
	{"core_loader_payloads", []string{"min_stock"}},
	{ClaimQuarantineTable, []string{"keep_staged"}},
}

// v60Tables is every table v60 drops, after the columns that name one.
var v60Tables = []string{"payloads", "processes_legacy"}

// v60RenamedAway are the scratch names an old rename-rebuild left in other
// tables' REFERENCES clauses.
var v60RenamedAway = []string{"processes_legacy", "styles_legacy"}

func v60Up(tx *sql.Tx) error {
	var enforced int
	if err := tx.QueryRow(`PRAGMA foreign_keys`).Scan(&enforced); err != nil {
		return fmt.Errorf("read foreign_keys: %w", err)
	}
	if enforced != 0 {
		return fmt.Errorf("foreign_keys is on: a rebuild's DROP would cascade into the tables that name the rebuilt one")
	}
	found, err := txStrings(tx, `SELECT m.name || '.' || c.name FROM sqlite_master m, pragma_table_info(m.name) c
		WHERE m.type = 'table'`)
	if err != nil {
		return err
	}
	hasColumn := func(table, column string) bool { return slices.Contains(found, table+"."+column) }
	hasTable := func(table string) bool {
		return slices.ContainsFunc(found, func(s string) bool { return strings.HasPrefix(s, table+".") })
	}

	if hasColumn(ClaimQuarantineTable, "keep_staged") {
		held, err := txStrings(tx, `SELECT 'claim ' || COALESCE(id, '?') || ' held ' || COALESCE(keep_staged, 'NULL')
			FROM `+ClaimQuarantineTable+` ORDER BY rowid`)
		if err != nil {
			return err
		}
		if len(held) > 0 {
			log.Printf("store: v60 drops %s.keep_staged: %s", ClaimQuarantineTable, strings.Join(held, ", "))
		}
	}
	for _, d := range v60Columns {
		for _, column := range d.columns {
			if !hasColumn(d.table, column) {
				continue
			}
			if _, err := tx.Exec(`ALTER TABLE ` + d.table + ` DROP COLUMN ` + column); err != nil {
				return fmt.Errorf("drop %s.%s: %w", d.table, column, err)
			}
		}
	}
	for _, table := range v60Tables {
		if !hasTable(table) {
			continue
		}
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			return fmt.Errorf("count %s: %w", table, err)
		}
		log.Printf("store: v60 drops table %s (%d rows)", table, n)
		if _, err := tx.Exec(`DROP TABLE ` + table); err != nil {
			return fmt.Errorf("drop %s: %w", table, err)
		}
	}
	for _, r := range v60Rebuilds {
		// A table that is not there is verifySchema's to name, not v60's to make.
		if !hasTable(r.table) {
			continue
		}
		if err := r.converge(tx); err != nil {
			return fmt.Errorf("%s: %w", r.table, err)
		}
	}
	return nil
}

// v60VerifySQL counts what v60 removes that is there: one statement.
var v60VerifySQL = func() string {
	quoted := func(names []string) string { return "('" + strings.Join(names, "', '") + "')" }
	var terms []string
	for _, d := range v60Columns {
		terms = append(terms, `(SELECT COUNT(*) FROM pragma_table_info('`+d.table+`') WHERE name IN `+quoted(d.columns)+`)`)
	}
	terms = append(terms, `(SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name IN `+quoted(v60Tables)+`)`)
	for _, r := range v60Rebuilds {
		terms = append(terms, `(SELECT COUNT(*) FROM pragma_foreign_key_list('`+r.table+`') WHERE "table" IN `+quoted(v60RenamedAway)+`)`)
	}
	return "SELECT " + strings.Join(terms, "\n\t+ ")
}()

// v60Verify holds while nothing v60 drops is there and no rebuilt table names
// a renamed-away one.
func v60Verify(q migrate.Querier) bool {
	var n int
	return q.QueryRow(v60VerifySQL).Scan(&n) == nil && n == 0
}

// txStrings reads a one-column query.
func txStrings(tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
