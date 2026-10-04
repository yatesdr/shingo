package www

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/store/processes"
)

// handlers_claim_lane_legs_test.go — lines name node groups, never lanes
// (owner, 2026-10-03), and an empties group is not where a line's fulls come
// from. Core refuses both for a loader at its own save; these pin the Edge's
// claim save and routing-set save saying the same, from the node list Core
// already sends.

// laneLegsCore is a group with one lane and a maintained (empties) group, as
// Core's node list carries them once the Edge has bare-named the children.
func laneLegsCore() map[string]protocol.NodeInfo {
	return map[string]protocol.NodeInfo{
		"LL-SUP":     {Name: "LL-SUP", NodeType: protocol.NodeClassNGRP},
		"LL-LANE":    {Name: "LL-LANE", NodeType: protocol.NodeClassLANE},
		"LL-EMPTIES": {Name: "LL-EMPTIES", NodeType: protocol.NodeClassNGRP, Maintained: true},
		"LL-PRESS":   {Name: "LL-PRESS"},
		"LL-STG":     {Name: "LL-STG"},
	}
}

func laneLegsClaim(sid int64, role protocol.ClaimRole, source, dest string) processes.NodeClaimInput {
	return processes.NodeClaimInput{
		StyleID: sid, CoreNodeName: "LL-PRESS", Role: role, SwapMode: protocol.SwapModeTwoRobot,
		PayloadCode: "PART-LL", InboundStaging: "LL-STG",
		InboundSource: source, OutboundDestination: dest,
	}
}

// assertRefusedOn posts the claim and wants a 400 carrying one error on field
// whose message starts with Core's sentence, and nothing stored.
func assertRefusedOn(t *testing.T, h *Handlers, router *chi.Mux, sid int64, body processes.NodeClaimInput, field, sentence string) {
	t.Helper()
	resp := doRequest(t, router, "POST", "/api/style-node-claims", body, authCookie(t, h))
	assertStatus(t, resp, http.StatusBadRequest)
	got := decodeBody(t, resp)
	raw, _ := got["field_errors"].([]any)
	found := false
	for _, r := range raw {
		m := r.(map[string]any)
		if m["field"] == field && strings.HasPrefix(m["message"].(string), sentence) {
			found = true
		}
	}
	if !found {
		t.Fatalf("want an error on %s reading %q; body = %+v", field, sentence, got)
	}
	claims, err := testDB.ListStyleNodeClaims(sid)
	testutil.MustNoErr(t, err, "list claims")
	if len(claims) != 0 {
		t.Fatalf("a refused claim was stored: %+v", claims)
	}
}

// TestUpsertClaim_RefusesALaneAsASource: the source leg — where fulls come
// from for a consume line and empties for a produce line — names a lane.
func TestUpsertClaim_RefusesALaneAsASource(t *testing.T) {
	h, router := newAdminRouter(t)
	h.engine.(*stubEngine).core = laneLegsCore()
	sid := seedStyle(t, "LL-SrcStyle", seedProcess(t, "LL-SrcLine"))
	for _, role := range []protocol.ClaimRole{protocol.ClaimRoleConsume, protocol.ClaimRoleProduce} {
		assertRefusedOn(t, h, router, sid, laneLegsClaim(sid, role, "LL-LANE", "LL-SUP"),
			"inbound_source", protocol.MsgLaneIsNotASource)
	}
}

// TestUpsertClaim_RefusesALaneAsADestination: the destination leg, the twin
// sentence.
func TestUpsertClaim_RefusesALaneAsADestination(t *testing.T) {
	h, router := newAdminRouter(t)
	h.engine.(*stubEngine).core = laneLegsCore()
	sid := seedStyle(t, "LL-DstStyle", seedProcess(t, "LL-DstLine"))
	for _, role := range []protocol.ClaimRole{protocol.ClaimRoleConsume, protocol.ClaimRoleProduce} {
		assertRefusedOn(t, h, router, sid, laneLegsClaim(sid, role, "LL-SUP", "LL-LANE"),
			"outbound_destination", protocol.MsgLaneIsNotADestination)
	}
}

// TestUpsertClaim_RefusesFullsFromAnEmptiesGroup: a consume line's source is a
// maintained group. A produce line taking its empties from the same group is
// the group's purpose and saves.
func TestUpsertClaim_RefusesFullsFromAnEmptiesGroup(t *testing.T) {
	h, router := newAdminRouter(t)
	h.engine.(*stubEngine).core = laneLegsCore()
	sid := seedStyle(t, "LL-MgStyle", seedProcess(t, "LL-MgLine"))
	assertRefusedOn(t, h, router, sid, laneLegsClaim(sid, protocol.ClaimRoleConsume, "LL-EMPTIES", "LL-SUP"),
		"inbound_source", protocol.MsgFullsFromAnEmptiesBank)

	resp := doRequest(t, router, "POST", "/api/style-node-claims",
		laneLegsClaim(sid, protocol.ClaimRoleProduce, "LL-EMPTIES", "LL-SUP"), authCookie(t, h))
	assertStatus(t, resp, http.StatusOK)
}

// TestUpsertClaim_OlderCoreRefusesNothing: a Core that sends no maintained
// flag, and a node list not yet heard, both save what they saved before.
func TestUpsertClaim_OlderCoreRefusesNothing(t *testing.T) {
	h, router := newAdminRouter(t)
	old := laneLegsCore()
	old["LL-EMPTIES"] = protocol.NodeInfo{Name: "LL-EMPTIES", NodeType: protocol.NodeClassNGRP}
	h.engine.(*stubEngine).core = old
	sid := seedStyle(t, "LL-OldStyle", seedProcess(t, "LL-OldLine"))
	resp := doRequest(t, router, "POST", "/api/style-node-claims",
		laneLegsClaim(sid, protocol.ClaimRoleConsume, "LL-EMPTIES", "LL-SUP"), authCookie(t, h))
	assertStatus(t, resp, http.StatusOK)

	h.engine.(*stubEngine).core = nil
	sid2 := seedStyle(t, "LL-NoCoreStyle", seedProcess(t, "LL-NoCoreLine"))
	resp = doRequest(t, router, "POST", "/api/style-node-claims",
		laneLegsClaim(sid2, protocol.ClaimRoleConsume, "LL-LANE", "LL-LANE"), authCookie(t, h))
	assertStatus(t, resp, http.StatusOK)
}

// TestRoutingNodes_RefusesALaneAsASourceOrDestination: the routing set is what
// the composer offers on those legs, so a lane is refused there too, on both
// the single-row POST and the whole-list PUT. A lane as a STAGING row is
// refused too (owner, 2026-10-04): a staging node is the spot a robot waits
// at, and a lane has no bin of its own to answer for it.
func TestRoutingNodes_RefusesALaneAsASourceOrDestination(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)
	h.engine.(*stubEngine).core = laneLegsCore()
	pid := seedProcess(t, "LL-Routing")
	url := "/api/processes/" + itoa(pid) + "/routing-nodes"

	for role, sentence := range map[string]string{
		"source": protocol.MsgLaneIsNotASource, "destination": protocol.MsgLaneIsNotADestination,
		"staging": protocol.MsgLaneIsNotAStagingNode,
	} {
		resp := doRequest(t, router, "POST", url, map[string]any{"core_node_name": "LL-LANE", "role": role}, cookie)
		assertStatus(t, resp, http.StatusBadRequest)
		if msg, _ := decodeBody(t, resp)["error"].(string); !strings.HasPrefix(msg, sentence) {
			t.Errorf("POST %s: error = %q, want it to start %q", role, msg, sentence)
		}
		resp = doRequest(t, router, "PUT", url, map[string]any{"nodes": []map[string]string{
			{"core_node_name": "LL-SUP", "role": role}, {"core_node_name": "LL-LANE", "role": role},
		}}, cookie)
		assertStatus(t, resp, http.StatusBadRequest)
		if msg, _ := decodeBody(t, resp)["error"].(string); !strings.HasPrefix(msg, sentence) {
			t.Errorf("PUT %s: error = %q, want it to start %q", role, msg, sentence)
		}
	}
	rows, err := testDB.ListRoutingNodes(pid)
	testutil.MustNoErr(t, err, "list routing nodes")
	if len(rows) != 0 {
		t.Fatalf("a refused body landed: %+v", rows)
	}
}

// TestComposer_OffersNoLaneChipOnASourceOrDestination: the composer's source
// and destination chips are the enabled routing rows of those roles, and a
// lane row already in the set (put there before the save refused one, or
// derived from a stored claim) is not offered. Nor is a staging lane, since
// a staging leg refuses a lane too; the group rows stay. Both composer reads,
// the desktop's and the station's, go through the same filter.
func TestComposer_OffersNoLaneChipOnASourceOrDestination(t *testing.T) {
	h, _ := newAdminRouter(t)
	pid := seedProcess(t, "LL-Composer")
	for _, row := range []struct{ name, role string }{
		{"LL-LANE", "source"}, {"LL-SUP", "source"},
		{"LL-LANE", "destination"}, {"LL-SUP", "destination"},
		{"LL-LANE", "staging"},
	} {
		_, err := testDB.UpsertRoutingNode(domain.CoreNodeKinds{}, processes.RoutingNodeInput{
			ProcessID: pid, CoreNodeName: row.name, Role: row.role, Enabled: true,
			Origin: domain.RoutingOriginEngineer,
		})
		testutil.MustNoErr(t, err, "seed routing row")
	}
	r := chi.NewRouter()
	r.Get("/api/processes/{id}/composer", h.apiProcessComposer)

	offered := func() map[string]bool {
		resp := doRequest(t, r, "GET", "/api/processes/"+itoa(pid)+"/composer", nil, nil)
		assertStatus(t, resp, http.StatusOK)
		var data domain.ComposerData
		testutil.MustNoErr(t, json.NewDecoder(resp.Body).Decode(&data), "decode composer")
		got := map[string]bool{}
		for _, rn := range data.Routing {
			got[rn.Role+"/"+rn.CoreNodeName] = true
		}
		return got
	}

	// No node list heard: nothing is known to be a lane, so every row is offered.
	if got := offered(); !got["source/LL-LANE"] || !got["destination/LL-LANE"] {
		t.Fatalf("with no node list the lane rows must still be offered: %v", got)
	}

	h.engine.(*stubEngine).core = laneLegsCore()
	got := offered()
	if got["source/LL-LANE"] || got["destination/LL-LANE"] {
		t.Errorf("a lane is offered on a source or destination leg: %v", got)
	}
	if got["staging/LL-LANE"] {
		t.Errorf("a lane is offered as a staging node: %v", got)
	}
	for _, k := range []string{"source/LL-SUP", "destination/LL-SUP"} {
		if !got[k] {
			t.Errorf("%s is missing from the offers: %v", k, got)
		}
	}
}
