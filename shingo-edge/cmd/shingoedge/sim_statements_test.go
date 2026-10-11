//go:build sim

package main

import "testing"

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
