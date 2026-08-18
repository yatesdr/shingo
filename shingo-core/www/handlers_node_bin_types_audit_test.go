//go:build docker

package www

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/store/nodes"
)

// The group settings modal's carrier-type writes leave an audit trail.
//
// THIS IS NOW LOAD-BEARING, not tidiness. node_bin_types is what press typing
// reads: what a position will accept becomes the thing that types an empty pull,
// so "who narrowed this, and when" is a question somebody asks about an incident.
// Before this it could not be answered at all — every property write on the same
// screen left a row and these left none.

func latestAuditByAction(t *testing.T, h *Handlers, nodeID int64) map[string]string {
	t.Helper()
	rows, err := h.engine.AuditService().ListForEntity("node", nodeID)
	testutil.MustNoErr(t, err, "ListForEntity")
	out := map[string]string{}
	for _, r := range rows {
		// Newest first, so the first row for an action is the latest one.
		if _, seen := out[r.Action]; !seen {
			out[r.Action] = r.OldValue + " -> " + r.NewValue
		}
	}
	return out
}

// TestApiSetNodeBinTypes_Audits pins the JSON endpoint.
func TestApiSetNodeBinTypes_Audits(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)

	n := &nodes.Node{Name: "AUDIT-BT-NODE", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(n), "CreateNode")
	var bigID, smallID int64
	testutil.MustNoErr(t, db.QueryRow(
		`INSERT INTO bin_types (code) VALUES ('AUD-BIG') RETURNING id`).Scan(&bigID), "big")
	testutil.MustNoErr(t, db.QueryRow(
		`INSERT INTO bin_types (code) VALUES ('AUD-SMALL') RETURNING id`).Scan(&smallID), "small")

	post := func(ids string) {
		t.Helper()
		body := `{"node_id":` + strconv.FormatInt(n.ID, 10) + `,"bin_type_ids":[` + ids + `]}`
		req := httptest.NewRequest(http.MethodPost, "/api/nodes/bin-types", bytes.NewBufferString(body))
		rec := httptest.NewRecorder()
		h.apiSetNodeBinTypes(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
	}

	post(strconv.FormatInt(bigID, 10) + "," + strconv.FormatInt(smallID, 10))
	got := latestAuditByAction(t, h, n.ID)["bin_types"]
	// CODES, not ids: the row is read by a person, and a list of integers needs a
	// second lookup against a table that may have moved on since.
	if got != " -> AUD-BIG, AUD-SMALL" {
		t.Fatalf("audit bin_types = %q, want the empty-to-both transition in codes", got)
	}

	// Narrowing is recorded as the narrowing it is.
	post(strconv.FormatInt(smallID, 10))
	got = latestAuditByAction(t, h, n.ID)["bin_types"]
	if got != "AUD-BIG, AUD-SMALL -> AUD-SMALL" {
		t.Fatalf("audit bin_types after narrowing = %q", got)
	}

	// A save that changes nothing writes no row — the same rule the property
	// endpoint follows, so the trail stays a list of changes rather than a list
	// of times somebody pressed Save.
	beforeRows, err := h.engine.AuditService().ListForEntity("node", n.ID)
	testutil.MustNoErr(t, err, "ListForEntity before")
	post(strconv.FormatInt(smallID, 10))
	afterRows, err := h.engine.AuditService().ListForEntity("node", n.ID)
	testutil.MustNoErr(t, err, "ListForEntity after")
	if len(afterRows) != len(beforeRows) {
		t.Errorf("a no-op save wrote %d extra audit row(s)", len(afterRows)-len(beforeRows))
	}
}

// TestHandleNodeUpdate_AuditsAssignments pins the path the MODAL actually uses.
//
// The Allowed Bins and Allowed Stations controls do not go through
// apiSetNodeBinTypes — they ride the node form's ordinary POST into
// ApplyAssignments, which wrote four things and audited none of them. That is a
// bigger hole than the one the phase set out to close, and it is the one an
// operator's edits actually fall into.
func TestHandleNodeUpdate_AuditsAssignments(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)

	n := &nodes.Node{Name: "AUDIT-ASSIGN-NODE", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(n), "CreateNode")
	var btID int64
	testutil.MustNoErr(t, db.QueryRow(
		`INSERT INTO bin_types (code) VALUES ('AUD-ASSIGN') RETURNING id`).Scan(&btID), "bin type")

	form := url.Values{}
	form.Set("id", strconv.FormatInt(n.ID, 10))
	form.Set("name", n.Name)
	form.Set("enabled", "on")
	form.Set("bin_type_mode", "specific")
	form.Add("bin_type_ids", strconv.FormatInt(btID, 10))
	form.Set("station_mode", "specific")
	form.Add("stations", "EDGE-AUD")

	req := httptest.NewRequest(http.MethodPost, "/nodes/update", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.handleNodeUpdate(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body=%s", rec.Code, rec.Body.String())
	}

	got := latestAuditByAction(t, h, n.ID)
	// PER FIELD, not one combined row: somebody asking when this stopped
	// accepting a carrier is asking about bin_types, and a bundled row makes them
	// read three values they did not ask about.
	for _, action := range []string{"bin_type_mode", "bin_types", "station_mode", "stations"} {
		if _, ok := got[action]; !ok {
			t.Errorf("no audit row for %q; rows = %v", action, got)
		}
	}
	if got["bin_types"] != " -> AUD-ASSIGN" {
		t.Errorf("audit bin_types = %q, want the empty-to-AUD-ASSIGN transition", got["bin_types"])
	}
	if got["stations"] != " -> EDGE-AUD" {
		t.Errorf("audit stations = %q, want the empty-to-EDGE-AUD transition", got["stations"])
	}
}
