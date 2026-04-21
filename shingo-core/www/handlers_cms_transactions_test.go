//go:build docker

package www

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"shingocore/internal/testdb"
)

// Characterization tests for handlers_cms_transactions.go —
// GET /api/cms-transactions returns CMS transactions filtered (optionally)
// by node_id with limit/offset paging. Empty DB returns an empty array and
// a bogus node_id returns 400.

// --- apiListCMSTransactions -------------------------------------------------

// TestApiListCMSTransactions_EmptyDB pins the empty-DB shape: the endpoint
// returns 200 with an empty JSON list (decodes to a zero-length slice —
// either `[]` or `null` depending on Go JSON encoding, so we tolerate both).
func TestApiListCMSTransactions_EmptyDB(t *testing.T) {
	h, _ := testHandlers(t)

	rec := getPlain(t, h.apiListCMSTransactions, "/api/cms-transactions")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var list []map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("expected empty list from fresh DB, got %d rows: %+v", len(list), list)
	}
}

// TestApiListCMSTransactions_WithLimitOffset pins the fact that limit/offset
// query params don't error out on an empty DB (they're applied to the SQL
// regardless of whether there are rows). A 200 OK is enough here.
func TestApiListCMSTransactions_WithLimitOffset(t *testing.T) {
	h, _ := testHandlers(t)

	rec := getPlain(t, h.apiListCMSTransactions, "/api/cms-transactions?limit=5&offset=0")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var list []map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

// TestApiListCMSTransactions_BadNodeID pins the validation path: a node_id
// that can't be parsed as int64 returns 400 with {"error": "invalid node_id"}.
func TestApiListCMSTransactions_BadNodeID(t *testing.T) {
	h, _ := testHandlers(t)

	rec := getPlain(t, h.apiListCMSTransactions, "/api/cms-transactions?node_id=not-a-number")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertJSONError(t, rec.Body.Bytes(), "invalid node_id")
}

// TestApiListCMSTransactions_KnownNodeID pins the per-node filter path: a
// valid integer node_id (even if the node has no transactions) triggers the
// filtered query branch and returns 200 + empty list.
func TestApiListCMSTransactions_KnownNodeID(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)

	rec := getPlain(t, h.apiListCMSTransactions,
		fmt.Sprintf("/api/cms-transactions?node_id=%d&limit=10&offset=0", sd.StorageNode.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var list []map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("expected empty list for unseeded node, got %d: %+v", len(list), list)
	}
}

// TestApiListCMSTransactions_UnknownNodeID pins that a syntactically-valid
// but nonexistent node_id still returns 200 (SQL just returns no rows) rather
// than a NotFound.
func TestApiListCMSTransactions_UnknownNodeID(t *testing.T) {
	h, _ := testHandlers(t)

	rec := getPlain(t, h.apiListCMSTransactions, "/api/cms-transactions?node_id=99999999")
	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestApiListCMSTransactions_IgnoresBogusLimitOffset pins the parser's
// tolerance: negative/zero/garbage limit/offset params are silently ignored
// (fall back to the defaults of limit=100 offset=0) rather than erroring.
func TestApiListCMSTransactions_IgnoresBogusLimitOffset(t *testing.T) {
	h, _ := testHandlers(t)

	rec := getPlain(t, h.apiListCMSTransactions,
		"/api/cms-transactions?limit=abc&offset=-1")
	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}
