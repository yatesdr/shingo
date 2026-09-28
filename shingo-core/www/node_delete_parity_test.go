//go:build docker

package www

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
)

// node_delete_parity_test.go — the nodes page's delete door answers like the
// node-group door (I4): an NGRP that active orders still source from is
// refused, and deleting an NGRP tells the Edge its node structure changed.
//
// The form door has no force option, so the group door's force cascade is not
// mirrored here: failing the blocked orders is that door's explicit choice.

func TestHandleNodeDelete_NGRPWithActiveOrdersIsRefused(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	// A bare group — no lane under it — so nothing but the order check stands
	// between the request and the delete.
	grpID, err := db.CreateNodeGroup("GRP-NDEL-BLOCK")
	if err != nil {
		t.Fatalf("create node group: %v", err)
	}
	blocking := createActiveOrderRefSource(t, db, "ndel-block-1", "line-1", "GRP-NDEL-BLOCK")

	rec := postForm(t, h.handleNodeDelete, "/nodes/delete", url.Values{"id": {strconv.FormatInt(grpID, 10)}})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "active order") {
		t.Errorf("refusal does not say why: %q", rec.Body.String())
	}
	if _, err := db.GetNode(grpID); err != nil {
		t.Errorf("group was deleted despite 409: %v", err)
	}
	if o := testdb.RequireOrder(t, db, blocking.EdgeUUID); o.Status != "pending" {
		t.Errorf("blocking order status = %q, want pending (untouched)", o.Status)
	}
}

func TestHandleNodeDelete_NGRPNotifiesEdge(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	grpID, err := db.CreateNodeGroup("GRP-NDEL-NOTIFY")
	if err != nil {
		t.Fatalf("create node group: %v", err)
	}

	rec := postForm(t, h.handleNodeDelete, "/nodes/delete", url.Values{"id": {strconv.FormatInt(grpID, 10)}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	requireOutboxSubject(t, db, protocol.SubjectNodeStructureChanged)
}
