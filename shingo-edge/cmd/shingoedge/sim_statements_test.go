//go:build sim

package main

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"shingoedge/store"
	"shingoedge/store/orders"
	"shingoedge/store/processes"
)

func TestSimStatementKey_NamesVerbAndTable(t *testing.T) {
	for q, want := range map[string]string{
		"SELECT a FROM process_node_runtime_states WHERE x=?": "SELECT process_node_runtime_states",
		"  update process_node_runtime_states SET a=1":        "UPDATE process_node_runtime_states",
		"INSERT OR IGNORE INTO outbox (a) VALUES (?)":         "INSERT outbox",
		"DELETE FROM node_lineside_bucket WHERE id=?":         "DELETE node_lineside_bucket",
		"WITH x AS (SELECT 1) SELECT * FROM orders":           "SELECT orders",
		"PRAGMA foreign_keys=ON":                              "OTHER PRAGMA",
	} {
		if got := keyOf(q); got != want {
			t.Errorf("keyOf(%q) = %q, want %q", q, got, want)
		}
	}
}

// A statement is named by its principal table: the FROM of the outermost
// SELECT, not the first FROM in the text. The cases are the shapes the Edge
// store really sends: a scalar sub-select in the select list (every claim read
// resolves its capacity from payload_catalog that way), a sub-select inside a
// select-list sub-select, a CTE whose body has nested parentheses, a FROM
// sub-query, and a comment or string literal that spells FROM.
func TestSimStatementName_PrincipalTable(t *testing.T) {
	for q, want := range map[string]string{
		"SELECT id, COALESCE((SELECT pc.uop_capacity FROM payload_catalog pc WHERE pc.code = style_node_claims.payload_code), -1) FROM style_node_claims WHERE style_id = ?": "SELECT style_node_claims",
		"SELECT a, COALESCE((SELECT group_concat(x) FROM (SELECT DISTINCT n.x FROM process_nodes n)), '') FROM styles WHERE process_id = ?1":                                 "SELECT styles",
		"WITH open AS (SELECT id FROM orders WHERE status IN (SELECT s FROM terminal)) SELECT * FROM open":                                                                   "SELECT orders",
		"WITH RECURSIVE r(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM r) SELECT n FROM r":                                                                                      "SELECT r",
		"SELECT * FROM (SELECT m.id FROM orders m ORDER BY m.id DESC LIMIT 1)":                                                                                               "SELECT orders",
		"SELECT x /* FROM decoy */ , 'FROM decoy2' FROM \"Bins\" b JOIN payload_catalog p ON p.code = b.payload":                                                             "SELECT bins",
		"-- FROM decoy\nSELECT 1 FROM main.outbox":                                                                                                                           "SELECT outbox",
		"SELECT changes()":                                            "SELECT -",
		"REPLACE INTO feed_copy (k) VALUES (?)":                       "INSERT feed_copy",
		"UPDATE OR IGNORE orders SET a = (SELECT b FROM bins)":        "UPDATE orders",
		"DELETE FROM outbox WHERE id IN (SELECT id FROM outbox_dead)": "DELETE outbox",
	} {
		if got := keyOf(q); got != want {
			t.Errorf("keyOf(%q)\n  = %q, want %q", q, got, want)
		}
	}
}

// The same rule on the statements the Edge store actually issues, captured
// through the counting store so the test names what production sends and not a
// copy of it. These are the reads the old first-FROM rule mis-named: every
// claim read as payload_catalog, the plant-claims publisher's styles read as
// process_nodes.
func TestSimStatementName_RealEdgeStatements(t *testing.T) {
	db, counter, err := store.OpenCounting(filepath.Join(t.TempDir(), "edge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	cases := []struct {
		name string
		call func() error
		want string
	}{
		{"processes.ListClaims", func() error { _, err := processes.ListClaims(db.DB, 1); return err }, "SELECT style_node_claims"},
		{"processes.ListLiveClaimsByProcess", func() error { _, err := processes.ListLiveClaimsByProcess(db.DB, 1); return err }, "SELECT style_node_claims"},
		{"processes.GetClaimByNode", func() error {
			_, err := processes.GetClaimByNode(db.DB, 1, "N")
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}, "SELECT style_node_claims"},
		{"processes.ListStylesAndPositionsByProcess", func() error {
			_, _, err := processes.ListStylesAndPositionsByProcess(db.DB, 1)
			return err
		}, "SELECT styles"},
		{"DB.ReleaseCountWithCursor", func() error {
			_, _, _, err := db.ReleaseCountWithCursor(1, 0, "node", "N", 1)
			return err
		}, "UPDATE process_node_runtime_states"},
		{"orders.KeptSpotRowsQuery", func() error {
			q, args := orders.KeptSpotRowsQuery(1, "S", "L")
			rows, err := db.Query(q, args...)
			if err != nil {
				return err
			}
			return rows.Close()
		}, "SELECT orders"},
	}
	for _, c := range cases {
		counter.Reset()
		if err := c.call(); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		stmts := counter.Drain()
		if len(stmts) == 0 {
			t.Fatalf("%s: no statement counted", c.name)
		}
		for _, q := range stmts {
			if got := keyOf(q); got != c.want {
				t.Errorf("%s: keyOf = %q, want %q\n  statement: %.160s", c.name, got, c.want, q)
			}
		}
	}
}
