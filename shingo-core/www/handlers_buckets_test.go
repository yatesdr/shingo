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
// LinesideBucketDelta pipeline, but no operator-facing UI surfaces it.
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
// join pattern, plus the bucket's station / style / part / qty.
func TestApiBuckets_WithSeededBuckets(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)

	seedBucket(t, db, "STATION-BKT", sd.StorageNode.ID, "", 1, "PART-BKT-A", 11)
	seedBucket(t, db, "STATION-BKT", sd.StorageNode.ID, "", 1, "PART-BKT-B", 23)

	rec := getPlain(t, h.apiBuckets, "/api/buckets")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var rows []map[string]any
	testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&rows), "decode")

	found := map[string]int{}
	for _, r := range rows {
		part, _ := r["part_number"].(string)
		qty, _ := r["qty"].(float64) // JSON numbers
		found[part] = int(qty)
	}
	if found["PART-BKT-A"] != 11 {
		t.Errorf("PART-BKT-A qty = %d, want 11; rows=%+v", found["PART-BKT-A"], rows)
	}
	if found["PART-BKT-B"] != 23 {
		t.Errorf("PART-BKT-B qty = %d, want 23; rows=%+v", found["PART-BKT-B"], rows)
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
	seedBucket(t, db, "STATION-Z", sd.StorageNode.ID, "", 1, "PART-Z", 5)
	seedBucket(t, db, "STATION-A", sd.StorageNode.ID, "", 1, "PART-A", 9)

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

// TestApiBucketDelete_RemovesRowAndDedup pins Round-3 Obs 10's admin
// recovery path. Seed a bucket + a matching inventory_delta_dedup row,
// hit POST /api/buckets/delete with the bucket's id, assert both rows
// are gone afterward. This is the cleanup hatch for Core-only orphan
// buckets the cross-namespace bugs in pre-Obs-8 builds left behind.
func TestApiBucketDelete_RemovesRowAndDedup(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)

	seedBucket(t, db, "STATION-DEL", sd.StorageNode.ID, "", 1, "PART-DEL", 17)

	var bucketID int64
	testutil.MustNoErr(t,
		db.QueryRow(`SELECT id FROM lineside_buckets WHERE station='STATION-DEL'`).Scan(&bucketID),
		"lookup seeded bucket id")

	// Seed a matching dedup row so we can verify it gets removed too.
	scopeKey := sd.StorageNode.Name + "||1|PART-DEL"
	if _, err := db.Exec(`INSERT INTO inventory_delta_dedup (station, scope_kind, scope_key, last_seq)
		VALUES ('STATION-DEL', 'bucket', $1, 42)`, scopeKey); err != nil {
		t.Fatalf("seed dedup: %v", err)
	}

	rec := postJSON(t, h.apiBucketDelete, "/api/buckets/delete",
		map[string]any{"id": bucketID})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var bucketCount int
	_ = db.QueryRow(`SELECT COUNT(*) FROM lineside_buckets WHERE id=$1`, bucketID).Scan(&bucketCount)
	if bucketCount != 0 {
		t.Errorf("bucket row count after delete = %d, want 0", bucketCount)
	}

	var dedupCount int
	_ = db.QueryRow(`SELECT COUNT(*) FROM inventory_delta_dedup
		WHERE station='STATION-DEL' AND scope_kind='bucket' AND scope_key=$1`, scopeKey).Scan(&dedupCount)
	if dedupCount != 0 {
		t.Errorf("dedup row count after delete = %d, want 0 (deletion must clear both rows atomically)", dedupCount)
	}
}

// TestApiBucketDelete_UnknownIDReturns404 pins the not-found response.
func TestApiBucketDelete_UnknownIDReturns404(t *testing.T) {
	t.Parallel()
	h, _ := testHandlers(t)

	rec := postJSON(t, h.apiBucketDelete, "/api/buckets/delete",
		map[string]any{"id": int64(999999999)})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestApiBucketDelete_MissingIDReturns400 pins the validation: id is
// required and must be positive.
func TestApiBucketDelete_MissingIDReturns400(t *testing.T) {
	t.Parallel()
	h, _ := testHandlers(t)

	rec := postJSON(t, h.apiBucketDelete, "/api/buckets/delete",
		map[string]any{})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// seedBucket inserts one lineside_buckets row. Station / node / pair_key
// / style_id / part_number must be set; qty is the count.
//
// nodeID is unused since the Round-3 Obs 8 migration; we derive the
// node name from the row's node_id at seed time so call sites don't
// have to refactor to the new shape. Pass either sd.StorageNode.ID /
// sd.LineNode.ID — the lookup happens here.
func seedBucket(t *testing.T, db *store.DB, station string, nodeID int64, pairKey string, styleID int64, part string, qty int) {
	t.Helper()
	var coreNodeName string
	if err := db.QueryRow(`SELECT name FROM nodes WHERE id=$1`, nodeID).Scan(&coreNodeName); err != nil {
		t.Fatalf("seedBucket lookup node name for id=%d: %v", nodeID, err)
	}
	if _, err := db.Exec(`INSERT INTO lineside_buckets (station, core_node_name, pair_key, style_id, part_number, qty)
		VALUES ($1, $2, $3, $4, $5, $6)`, station, coreNodeName, pairKey, styleID, part, qty); err != nil {
		t.Fatalf("seed bucket (%s/%s): %v", station, part, err)
	}
}
