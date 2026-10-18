package www

import (
	"net/http"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/store/processes"
)

// handlers_routing_nodes_test.go — the routing-set endpoints under
// /api/processes/{id}/routing-nodes and the flow-composer gate PATCH.
//
// The store pins (store/routing_set_test.go) cover the derivation; these
// cover what the HTTP layer adds: server-stamped attribution, the Core-name
// guard reused from the process-node write, and the status codes an engineer's browser branches on.

// The GET's shape: the rows. The summary line and the gate that rode beside
// them came from the boot-time backfill's report and went with it (pick-a-part
// unit A); the panel read neither.
type routingSetResponse struct {
	Rows []domain.RoutingNode `json:"rows"`
}

func seedRoutingClaim(t *testing.T, pid int64, styleName string) int64 {
	t.Helper()
	sid := seedStyle(t, styleName, pid)
	if _, err := testDB.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
		StyleID: sid, CoreNodeName: "RT-PRESS", Role: "consume", SwapMode: "two_robot",
		PayloadCode: "RAW", InboundStaging: "RT_STG_01", InboundSource: "SMN_BUF_100",
		OutboundDestination: "Supermarket Area",
	}); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	return sid
}

// seedBackfilledRows writes the rows the one-time backfill (migration v25)
// would have written for seedRoutingClaim's claim: origin backfill, enabled.
func seedBackfilledRows(t *testing.T, pid int64) {
	t.Helper()
	for _, r := range []struct{ name, role string }{
		{"SMN_BUF_100", domain.RoutingRoleSource},
		{"RT_STG_01", domain.RoutingRoleStaging},
		{"Supermarket Area", domain.RoutingRoleDestination},
	} {
		if _, err := testDB.UpsertRoutingNode(domain.CoreNodeKinds{}, domain.RoutingNodeInput{
			ProcessID: pid, CoreNodeName: r.name, Role: r.role, Enabled: true, Origin: domain.RoutingOriginBackfill,
		}); err != nil {
			t.Fatalf("seed backfilled row %s: %v", r.name, err)
		}
	}
}

// TestRoutingNodes_GetIsTheRowsAndTheDeriveRouteIsGone: the GET answers the
// rows in the set, and POST …/derive — the re-derive no screen ever called —
// is no longer routed (pick-a-part unit A).
func TestRoutingNodes_GetIsTheRowsAndTheDeriveRouteIsGone(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)
	pid := seedProcess(t, "RT-Derive")
	seedRoutingClaim(t, pid, "RT-Derive-Style")
	seedBackfilledRows(t, pid)

	resp := doRequest(t, router, "GET", "/api/processes/"+itoa(pid)+"/routing-nodes", nil, cookie)
	assertStatus(t, resp, http.StatusOK)
	var got routingSetResponse
	decodeJSON(t, resp, &got)
	keys := make([]string, 0, len(got.Rows))
	for _, r := range got.Rows {
		keys = append(keys, r.CoreNodeName+":"+r.Role)
		if r.StyleCount != 1 {
			t.Errorf("%s: style count %d, want 1 — the evidence the panel shows under a name", r.CoreNodeName, r.StyleCount)
		}
	}
	if strings.Join(keys, ",") != "SMN_BUF_100:source,RT_STG_01:staging,Supermarket Area:destination" {
		t.Fatalf("rows = %v", keys)
	}

	resp = doRequest(t, router, "POST", "/api/processes/"+itoa(pid)+"/routing-nodes/derive", nil, cookie)
	if resp.StatusCode == http.StatusOK {
		t.Fatal("POST …/routing-nodes/derive still answers 200; the backfill and its door were deleted")
	}
}

// TestRoutingNodes_PostStampsEngineerAndSessionUser: an engineer's row is
// origin=engineer, called_by=<session user>, and the client cannot say
// otherwise. The Core-name guard applies exactly as it does to a
// process_node write, and a position is refused.
func TestRoutingNodes_PostStampsEngineerAndSessionUser(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)
	pid := seedProcess(t, "RT-Post")
	seedProcessNode(t, pid, 0, "RT-POS-01")
	h.engine.(*stubEngine).core = map[string]protocol.NodeInfo{"Supermarket Area": {}, "RT-POS-01": {}}

	resp := doRequest(t, router, "POST", "/api/processes/"+itoa(pid)+"/routing-nodes", map[string]any{
		"core_node_name": " Supermarket Area ", "role": "source", "label": "FG", "enabled": true,
		"origin": "backfill", "called_by": "mallory",
	}, cookie)
	assertStatus(t, resp, http.StatusOK)
	rows, err := testDB.ListRoutingNodes(pid)
	testutil.MustNoErr(t, err, "testDB.ListRoutingNodes")
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want one", rows)
	}
	if rows[0].CoreNodeName != "Supermarket Area" || rows[0].Origin != domain.RoutingOriginEngineer || rows[0].CalledBy != "testadmin" {
		t.Fatalf("row = %+v, want trimmed name, origin engineer, called_by testadmin (the client's origin/called_by are ignored)", rows[0])
	}

	resp = doRequest(t, router, "POST", "/api/processes/"+itoa(pid)+"/routing-nodes", map[string]any{
		"core_node_name": "NOT-ON-CORE", "role": "source",
	}, cookie)
	assertStatus(t, resp, http.StatusBadRequest)

	resp = doRequest(t, router, "POST", "/api/processes/"+itoa(pid)+"/routing-nodes", map[string]any{
		"core_node_name": "Supermarket Area", "role": "waypoint",
	}, cookie)
	assertStatus(t, resp, http.StatusBadRequest)

	resp = doRequest(t, router, "POST", "/api/processes/"+itoa(pid)+"/routing-nodes", map[string]any{
		"core_node_name": "RT-POS-01", "role": "staging",
	}, cookie)
	assertStatus(t, resp, http.StatusConflict)
	body := decodeBody(t, resp)
	if msg, _ := body["error"].(string); !strings.Contains(msg, "position") {
		t.Errorf("position refusal does not say why: %v", body)
	}

	// Unauthenticated: the admin gate, like every other spec write.
	resp = doRequest(t, router, "GET", "/api/processes/"+itoa(pid)+"/routing-nodes", nil, nil)
	if resp.StatusCode == http.StatusOK {
		t.Fatal("routing set readable without a session")
	}
}

// TestRoutingNodes_AdoptAndDelete: PATCH enables (adopts) a backfilled row
// and stamps the user; DELETE is refused with 409 and the style's name while
// a live claim routes through the node, and succeeds once nothing does.
func TestRoutingNodes_AdoptAndDelete(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)
	pid := seedProcess(t, "RT-Adopt")
	sid := seedRoutingClaim(t, pid, "RT-Adopt-Style")
	seedBackfilledRows(t, pid)
	rows, err := testDB.ListRoutingNodes(pid)
	testutil.MustNoErr(t, err, "testDB.ListRoutingNodes")
	var source, staging domain.RoutingNode
	for _, r := range rows {
		switch r.Role {
		case domain.RoutingRoleSource:
			source = r
		case domain.RoutingRoleStaging:
			staging = r
		}
	}

	// THE SWITCH IS THE APPROVAL (owner ruling 2026-09-10). Enabling a
	// backfilled name is an engineer taking it on, so the row's origin becomes
	// engineer and called_by records the session user. This asserted the origin
	// STAYED backfill, which is what left the panel unable to tell an approved
	// name from one still waiting, and the summary line counting it forever.
	resp := doRequest(t, router, "PATCH", "/api/processes/"+itoa(pid)+"/routing-nodes/"+itoa(source.ID), map[string]any{"enabled": true}, cookie)
	assertStatus(t, resp, http.StatusOK)
	rows, err = testDB.ListRoutingNodes(pid)
	testutil.MustNoErr(t, err, "testDB.ListRoutingNodes")
	for _, r := range rows {
		if r.ID == source.ID && (!r.Enabled || r.CalledBy != "testadmin" || r.Origin != domain.RoutingOriginEngineer) {
			t.Fatalf("adopted row = %+v, want enabled, called_by testadmin, origin engineer", r)
		}
	}
	// And the evidence the panel shows came back with it: the live styles whose
	// claim names this node in THIS role. Off the PANEL's read, which is the
	// only one that counts — the hot reads take the plain list.
	rows, err = testDB.ListRoutingNodesWithCounts(pid)
	testutil.MustNoErr(t, err, "testDB.ListRoutingNodesWithCounts")
	for _, r := range rows {
		if r.ID == source.ID && r.StyleCount != 1 {
			t.Errorf("adopted row style_count = %d, want 1 — the panel has no evidence to show under the name", r.StyleCount)
		}
	}
	// A body that says nothing is a 400, not a silent no-op.
	resp = doRequest(t, router, "PATCH", "/api/processes/"+itoa(pid)+"/routing-nodes/"+itoa(source.ID), map[string]any{}, cookie)
	assertStatus(t, resp, http.StatusBadRequest)

	resp = doRequest(t, router, "DELETE", "/api/processes/"+itoa(pid)+"/routing-nodes/"+itoa(staging.ID), nil, cookie)
	assertStatus(t, resp, http.StatusConflict)
	body := decodeBody(t, resp)
	if msg, _ := body["error"].(string); !strings.Contains(msg, "RT-Adopt-Style") {
		t.Errorf("refusal does not name the style: %v", body)
	}

	if err := testDB.DeleteStyle(sid); err != nil {
		t.Fatalf("retire style: %v", err)
	}
	resp = doRequest(t, router, "DELETE", "/api/processes/"+itoa(pid)+"/routing-nodes/"+itoa(staging.ID), nil, cookie)
	assertStatus(t, resp, http.StatusOK)
	// And a row of another process is not reachable through this one.
	other := seedProcess(t, "RT-Adopt-Other")
	resp = doRequest(t, router, "DELETE", "/api/processes/"+itoa(other)+"/routing-nodes/"+itoa(source.ID), nil, cookie)
	assertStatus(t, resp, http.StatusNotFound)
}

// TestProcess_PatchFlowComposerEnabled: the gate is its own PATCH on the
// process, and it persists both ways.
func TestProcess_PatchFlowComposerEnabled(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)
	pid := seedProcess(t, "RT-Gate")
	seedRoutingClaim(t, pid, "RT-Gate-Style")

	resp := doRequest(t, router, "PATCH", "/api/processes/"+itoa(pid), map[string]any{}, cookie)
	assertStatus(t, resp, http.StatusBadRequest)

	resp = doRequest(t, router, "PATCH", "/api/processes/"+itoa(pid), map[string]any{"flow_composer_enabled": true}, cookie)
	assertStatus(t, resp, http.StatusOK)
	p, err := testDB.GetProcess(pid)
	testutil.MustNoErr(t, err, "testDB.GetProcess")
	if !p.FlowComposerEnabled {
		t.Fatal("PATCH flow_composer_enabled=true did not persist")
	}

	resp = doRequest(t, router, "PATCH", "/api/processes/"+itoa(pid), map[string]any{"flow_composer_enabled": false}, cookie)
	assertStatus(t, resp, http.StatusOK)
	p, err = testDB.GetProcess(pid)
	testutil.MustNoErr(t, err, "testDB.GetProcess")
	if p.FlowComposerEnabled {
		t.Fatal("PATCH flow_composer_enabled=false did not persist")
	}
}
