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
// routing-set save and composer saying the same, from the node list Core
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
