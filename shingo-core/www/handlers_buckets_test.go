//go:build docker

package www

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
)

// Issue 2 (lineside-buckets-investigation-2026-05-18.md): Core's
// lineside_buckets table is populated end-to-end by the existing
// Edge's pile levels, but no operator-facing UI surfaced it.
// These tests pin the read-only listing endpoint that the inventory
// page now consumes alongside the existing bins table.

// TestApiBuckets_EmptyDB pins the empty-DB response: 200 + a JSON
// array decode (possibly empty).
func TestApiBuckets_EmptyDB(t *testing.T) {
	t.Parallel()
	h, _ := testHandlers(t)

	rec := getPlain(t, h.apiBuckets, "/api/buckets")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var rows []map[string]any
	testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&rows), "decode")
}

// TestApiBuckets_WithSeededBuckets pins the happy path: rows seeded
// into lineside_buckets surface in the JSON response with the
// node-derived cell/lane fields populated from the existing inventory
// join pattern, plus the pile's station / part / state / qty.
func TestApiBuckets_WithSeededBuckets(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)

	seedBucket(t, db, "STATION-BKT", sd.StorageNode.ID, "active", "PAY-BKT-A", 11)
	seedBucket(t, db, "STATION-BKT", sd.StorageNode.ID, "active", "PAY-BKT-B", 23)

	rec := getPlain(t, h.apiBuckets, "/api/buckets")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var rows []map[string]any
	testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&rows), "decode")

	found := map[string]int{}
	for _, r := range rows {
		code, _ := r["payload_code"].(string)
		qty, _ := r["qty"].(float64) // JSON numbers
		found[code] = int(qty)
	}
	if found["PAY-BKT-A"] != 11 {
		t.Errorf("PAY-BKT-A qty = %d, want 11; rows=%+v", found["PAY-BKT-A"], rows)
	}
	if found["PAY-BKT-B"] != 23 {
		t.Errorf("PAY-BKT-B qty = %d, want 23; rows=%+v", found["PAY-BKT-B"], rows)
	}

	// Spot-check that each row carries station + node_name (the keys
	// the inventory page renders against). Cell / lane may be empty
	// for a storage node without a parent hierarchy, but the keys
	// MUST be present so the JS doesn't render "undefined".
	for _, r := range rows {
		if _, ok := r["station"]; !ok {
			t.Errorf("row missing station key: %+v", r)
		}
		if _, ok := r["node_name"]; !ok {
			t.Errorf("row missing node_name key: %+v", r)
		}
		if _, ok := r["state"]; !ok {
			t.Errorf("row missing state key: %+v", r)
		}
	}
}

// TestApiBuckets_OrderedByCellStationNode pins the documented sort
// order: rows come back ordered so an operator scrolling the table
// sees buckets grouped by cell first, then station, then node. This
// matches the existing inventory table's group-first layout.
func TestApiBuckets_OrderedByCellStationNode(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)

	// Two stations on the same node — sort must put STATION-A first.
	seedBucket(t, db, "STATION-Z", sd.StorageNode.ID, "active", "PART-Z", 5)
	seedBucket(t, db, "STATION-A", sd.StorageNode.ID, "active", "PART-A", 9)

	rec := getPlain(t, h.apiBuckets, "/api/buckets")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var rows []map[string]any
	testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&rows), "decode")
	if len(rows) < 2 {
		t.Fatalf("expected >=2 rows, got %d", len(rows))
	}
	firstStation, _ := rows[0]["station"].(string)
	if firstStation != "STATION-A" {
		t.Errorf("first row station = %q, want STATION-A (cell→station→node sort)", firstStation)
	}
}

// TestHandleInventory_ListsBucketsSection pins the page-render path: the
// inventory.html template includes a "Lineside Buckets" section that
// the operator-facing page renders alongside the bins table.
func TestHandleInventory_ListsBucketsSection(t *testing.T) {
	t.Parallel()
	h, _ := testHandlersForPages(t)

	rec := getPlain(t, h.handleInventory, "/inventory")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Lineside Buckets") {
		t.Errorf("expected 'Lineside Buckets' section in /inventory page, not found; body len=%d", len(body))
	}
}

// seedBucket inserts one lineside_buckets row: Core's mirror of one Edge pile
// in the given state ("active" or "stranded").
//
// The table keys on core_node_name; the node name is looked up from nodeID
// here so call sites can pass sd.StorageNode.ID / sd.LineNode.ID.
func seedBucket(t *testing.T, db *store.DB, station string, nodeID int64, state, payloadCode string, qty int) {
	t.Helper()
	var coreNodeName string
	if err := db.QueryRow(`SELECT name FROM nodes WHERE id=$1`, nodeID).Scan(&coreNodeName); err != nil {
		t.Fatalf("seedBucket lookup node name for id=%d: %v", nodeID, err)
	}
	if _, err := db.Exec(`INSERT INTO lineside_buckets (station, core_node_name, state, payload_code, qty)
		VALUES ($1, $2, $3, $4, $5)`, station, coreNodeName, state, payloadCode, qty); err != nil {
		t.Fatalf("seed bucket (%s/%s): %v", station, payloadCode, err)
	}
}
