package www

import (
	"net/http"
	"testing"

	"shingoedge/domain"
	"shingoedge/store/processes"
)

// handlers_process_payloads_test.go — the two LIST-SHAPED writes Add Process
// makes, and the one rule that is not about their contents.
//
// WHY LIST-SHAPED AT ALL. Add Process wrote one POST per routing name and had
// no door onto the part set at all, so an engineer creating a cell with six
// routing names and six parts would have made ten round trips to a Pi with one
// SQLite connection, each of them a separate transaction that could half-land.
// One body per list is one refusal per list.
//
// AND NEITHER BROADCASTS material-refresh. Every board on the plant is
// subscribed to that event and answers it with a view build (operator.js,
// onMaterialRefresh → scheduleRefresh). A part set and a routing set are
// ENGINEERING facts — no board's tiles change when one is written — so a
// broadcast here would be N view builds on one connection for a screen change
// nobody made.

func payloadProcess(t *testing.T, name string) int64 {
	t.Helper()
	pid := seedProcess(t, name)
	if _, err := testDB.CreateProcessNode(processes.NodeInput{
		ProcessID: pid, CoreNodeName: "PY_01", Name: "PY_01", Enabled: true,
	}); err != nil {
		t.Fatalf("create position: %v", err)
	}
	return pid
}

func TestProcessPayloads_PutIsASetToInOneTrip(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)
	pid := payloadProcess(t, "PAYLOAD-PUT")

	resp := doRequest(t, router, "PUT", "/api/processes/"+itoa(pid)+"/payloads",
		map[string]any{"payloads": []string{"PY-A", "PY-B", "PY-C"}}, cookie)
	assertStatus(t, resp, http.StatusOK)

	got, err := testDB.ListProcessPayloads(pid)
	if err != nil {
		t.Fatalf("ListProcessPayloads: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("stored %v, want three parts from one request", got)
	}

	// A SET-TO: the second body is the whole list, not an addition to it.
	resp = doRequest(t, router, "PUT", "/api/processes/"+itoa(pid)+"/payloads",
		map[string]any{"payloads": []string{"PY-B"}}, cookie)
	assertStatus(t, resp, http.StatusOK)
	got, err = testDB.ListProcessPayloads(pid)
	if err != nil {
		t.Fatalf("ListProcessPayloads: %v", err)
	}
	if len(got) != 1 || got[0] != "PY-B" {
		t.Fatalf("stored %v after the second write, want [PY-B]", got)
	}

	// An EMPTY list is a real answer — "this cell has no part set of its own" —
	// and not a body to refuse.
	resp = doRequest(t, router, "PUT", "/api/processes/"+itoa(pid)+"/payloads",
		map[string]any{"payloads": []string{}}, cookie)
	assertStatus(t, resp, http.StatusOK)
	got, err = testDB.ListProcessPayloads(pid)
	if err != nil {
		t.Fatalf("ListProcessPayloads: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("stored %v after an empty write, want none", got)
	}
}

// THE ROUTING ARRAY ADDS AND ADOPTS; IT NEVER DELETES.
//
// A set-to here would be a delete path, and a routing delete is refused while a
// live claim routes through the name (ErrRoutingNodeInUse) — so a set-to would
// either refuse the whole write for a name the engineer never touched, or
// silently drop a row a flow depends on. Add Process only ever adds, and the
// Edit sheet keeps its diff, where a removal is a DELETE the engineer sees
// refused by name.
func TestProcessRoutingNodes_PutAddsAndAdoptsAndNeverDeletes(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)
	pid := payloadProcess(t, "ROUTING-PUT")

	// A row that already exists and is switched OFF: the array write adopts it
	// rather than minting a second one.
	offID, err := testDB.UpsertRoutingNode(domain.RoutingNodeInput{
		ProcessID: pid, CoreNodeName: "RS-OFF", Role: domain.RoutingRoleSource,
		Origin: domain.RoutingOriginBackfill,
	})
	if err != nil {
		t.Fatalf("seed the disabled row: %v", err)
	}
	if err := testDB.SetRoutingNodeEnabled(pid, offID, false, "seed"); err != nil {
		t.Fatalf("switch it off: %v", err)
	}
	// And a row the body does NOT name, which must survive.
	if _, err := testDB.UpsertRoutingNode(domain.RoutingNodeInput{
		ProcessID: pid, CoreNodeName: "RS-KEEP", Role: domain.RoutingRoleDestination, Enabled: true,
	}); err != nil {
		t.Fatalf("seed the untouched row: %v", err)
	}

	resp := doRequest(t, router, "PUT", "/api/processes/"+itoa(pid)+"/routing-nodes",
		map[string]any{"nodes": []map[string]any{
			{"core_node_name": "RS-OFF", "role": "source"},
			{"core_node_name": "RS-NEW", "role": "staging"},
			{"core_node_name": "RS-LATER", "role": "staging"},
		}}, cookie)
	assertStatus(t, resp, http.StatusOK)

	rows, err := testDB.ListRoutingNodes(pid)
	if err != nil {
		t.Fatalf("ListRoutingNodes: %v", err)
	}
	byName := map[string]domain.RoutingNode{}
	for _, r := range rows {
		byName[r.CoreNodeName] = r
	}
	if len(rows) != 4 {
		t.Fatalf("the routing set holds %d rows (%v), want four — the named three plus the one nobody touched",
			len(rows), byName)
	}
	if r := byName["RS-OFF"]; r.ID != offID || !r.Enabled {
		t.Errorf("RS-OFF came back as id %d enabled=%v; the array write must ADOPT the existing row (id %d), not mint a second",
			r.ID, r.Enabled, offID)
	}
	if r := byName["RS-NEW"]; !r.Enabled || r.Role != domain.RoutingRoleStaging {
		t.Errorf("RS-NEW came back role=%q enabled=%v, want staging and on", r.Role, r.Enabled)
	}
	if _, kept := byName["RS-KEEP"]; !kept {
		t.Error("RS-KEEP is gone; the array write deleted a row the body never named")
	}

	// THE ORDER IN THE BODY IS THE SEQUENCE, PER ROLE, because the lowest
	// sequence of a role is the default a new position opens on
	// (composer-model's defaultRouting). Per role and not over the whole body:
	// the sequence is only ever compared within a role, and counting across
	// them would make a staging row's default depend on how many sources came
	// before it in a list nobody thinks of as ordered that way.
	if r := byName["RS-NEW"]; r.Sequence != 0 {
		t.Errorf("RS-NEW has sequence %d, want 0 — it is the first staging name the engineer picked", r.Sequence)
	}
	if r := byName["RS-LATER"]; r.Sequence != 1 {
		t.Errorf("RS-LATER has sequence %d, want 1 — it is the second staging name", r.Sequence)
	}
	if r := byName["RS-OFF"]; r.Sequence != 0 {
		t.Errorf("RS-OFF has sequence %d, want 0 — it is the first SOURCE, and the roles are counted apart",
			r.Sequence)
	}

	// A BAD ROLE REFUSES THE WHOLE BODY rather than writing the good half: this
	// is one list, and a partial write would report success for a set the
	// engineer never asked for.
	resp = doRequest(t, router, "PUT", "/api/processes/"+itoa(pid)+"/routing-nodes",
		map[string]any{"nodes": []map[string]any{
			{"core_node_name": "RS-GOOD", "role": "source"},
			{"core_node_name": "RS-BAD", "role": "waypoint"},
		}}, cookie)
	assertStatus(t, resp, http.StatusBadRequest)
	rows, err = testDB.ListRoutingNodes(pid)
	if err != nil {
		t.Fatalf("ListRoutingNodes: %v", err)
	}
	for _, r := range rows {
		if r.CoreNodeName == "RS-GOOD" {
			t.Error("the refused body wrote RS-GOOD anyway; the list is validated before any of it lands")
		}
	}
}

// A POSITION OF THE PROCESS IS NOT A ROUTING ROW, and the refusal has to name
// it: the store refuses one (ErrRoutingNodeIsPosition) and a 409 with the
// node's name in it is what the sheet puts in front of the engineer.
func TestProcessRoutingNodes_PutRefusesAPositionByName(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)
	pid := payloadProcess(t, "ROUTING-PUT-POS")

	resp := doRequest(t, router, "PUT", "/api/processes/"+itoa(pid)+"/routing-nodes",
		map[string]any{"nodes": []map[string]any{
			{"core_node_name": "PY_01", "role": "staging"},
		}}, cookie)
	assertStatus(t, resp, http.StatusConflict)
	if msg, _ := decodeBody(t, resp)["error"].(string); msg == "" {
		t.Error("the refusal carries no sentence")
	}
}
