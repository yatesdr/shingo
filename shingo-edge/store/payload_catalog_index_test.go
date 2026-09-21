package store

import (
	"path/filepath"
	"strings"
	"testing"

	"shingoedge/store/internal/capacity"
)

// payload_catalog_index_test.go — the capacity subquery has an index, and this
// is the query that named it.
//
// A claim read does not look a payload up in Go; capacity.SQL puts a
// correlated scalar subquery in the SELECT list, so the lookup happens once per
// claim ROW inside whatever query is reading claims. That makes it invisible to
// anyone reading the Go, and it makes an unindexed payload_catalog.code one
// full catalog scan per claim — on a store pinned to one SQLite connection on a
// Pi, in the set-returning read the three per-node walkers now use.

// TestPayloadCatalogCodeIndex_ServesTheCapacitySubquery asserts the PLAN, not
// just the index's existence. An index nothing chooses is not a fix, and the
// claim being made here is specifically that this query stopped scanning.
func TestPayloadCatalogCodeIndex_ServesTheCapacitySubquery(t *testing.T) {
	t.Parallel()
	db, err := Open(filepath.Join(t.TempDir(), "idx.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	// The shape processes.ClaimsByNodeForStyles issues: the claim columns,
	// capacity resolved in the SELECT list, filtered by a style set. Spelled
	// with capacity.SQL rather than copied, so a change to the subquery is
	// measured rather than missed.
	query := `SELECT id, style_id, core_node_name, payload_code, ` +
		capacity.SQL("style_node_claims") +
		` FROM style_node_claims WHERE style_id IN (?,?) AND retired_at IS NULL`

	rows, err := db.DB.Query(`EXPLAIN QUERY PLAN `+query, 1, 2)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}

	joined := strings.Join(plan, "\n")
	if !strings.Contains(joined, "idx_payload_catalog_code") {
		t.Errorf("the capacity subquery does not use idx_payload_catalog_code. Plan:\n%s\n\n"+
			"Before the index this read `SCAN pc` — a full pass over payload_catalog for every "+
			"claim row the outer query returns.", joined)
	}
	for _, line := range plan {
		if strings.HasPrefix(line, "SCAN") && strings.Contains(line, "pc") {
			t.Errorf("the capacity subquery still scans the catalog: %q", line)
		}
	}
}
