package www

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// claim_leg_members_test.go — every door that names a node where Core picks,
// handed a member of a node group, and a slot inside a lane (a name Core's
// list never holds). Where Core picks (a claim's source and destination legs,
// a routing row's source and destination, an API order's source and
// destination) a line names the group or a node in no group; a staging spot
// names the exact node, a member or not (owner, 2026-11-11). The lane census
// beside this one is claim_leg_writers_test.go.

const (
	memberSlot = "LL-SLOT"    // a member of LL-GRP
	laneSlot   = "LL-LANE-S1" // a slot inside a lane: never in Core's list
)

// withMembers is the writers' node list with a group and its member, and the
// group map the stub's station service is wired with. NO LANE, as a plant-wide
// list has none (Core never sends a synthetic node's children, and a lane is
// one): a check that only runs once a lane is known never runs there.
func withMembers(h *Handlers) {
	core := writersCore()
	delete(core, "LL-LANE")
	core["LL-GRP"] = protocol.NodeInfo{Name: "LL-GRP", NodeType: protocol.NodeClassNGRP}
	core[memberSlot] = protocol.NodeInfo{Name: memberSlot}
	h.engine.(*stubEngine).core = core
	h.engine.(*stubEngine).groups = map[string][]string{"LL-GRP": {memberSlot}}
}

// withNodeOn is the clean claim with name on one leg.
func withNodeOn(in processes.NodeClaimInput, leg, name string) processes.NodeClaimInput {
	switch leg {
	case "inbound_source":
		in.InboundSource = name
	case "outbound_destination":
		in.OutboundDestination = name
	case "containment_destination":
		in.ContainmentDestination = name
	case "partial_destination":
		in.PartialDestination = name
	case "inbound_staging":
		in.InboundStaging = name
	case "outbound_staging":
		in.OutboundStaging = name
	default:
		panic("unknown leg " + leg)
	}
	return in
}

// storedOn reports whether any live claim of the style names name on leg.
func storedOn(t *testing.T, db *store.DB, sid int64, leg, name string) bool {
	t.Helper()
	claims, err := db.ListStyleNodeClaims(sid)
	testutil.MustNoErr(t, err, "list claims")
	for _, c := range claims {
		v := map[string]string{"inbound_source": c.InboundSource, "outbound_destination": c.OutboundDestination,
			"containment_destination": c.ContainmentDestination, "partial_destination": c.PartialDestination,
			"inbound_staging": c.InboundStaging, "outbound_staging": c.OutboundStaging}[leg]
		if v == name {
			return true
		}
	}
	return false
}

// TestContainmentSetting_AMemberOrALaneSlotIsRefused: the order-1208 shape at
// its source. A containment destination that is a member of a group, or a slot
// inside a lane, is a drop the group resolver and the lane gate move off, and
// the arrival hold then misses the carrier. The group itself saves.
func TestContainmentSetting_AMemberOrALaneSlotIsRefused(t *testing.T) {
	t.Parallel()
	h, router := newAdminRouter(t)
	db := dbOf(h)
	withMembers(h)
	cookie := authCookie(t, h)
	for i, c := range []struct {
		dest   string
		status int
	}{{memberSlot, http.StatusBadRequest}, {laneSlot, http.StatusBadRequest}, {"LL-GRP", http.StatusOK}} {
		t.Run(c.dest, func(t *testing.T) {
			pid := seedProcess(t, db, fmt.Sprintf("CM%02d-Line", i))
			sid := seedStyle(t, db, fmt.Sprintf("CM%02d-Src", i), pid)
			_, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, cleanLegClaim(sid))
			testutil.MustNoErr(t, err, "seed a clean claim")
			resp := doRequest(t, router, "POST", "/api/processes/"+itoa(pid)+"/containment-setting",
				map[string]any{"enabled": true, "destination": c.dest}, cookie)
			if resp.StatusCode != c.status {
				t.Fatalf("containment %q: status %d, want %d (%+v)", c.dest, resp.StatusCode, c.status, decodeBody(t, resp))
			}
			if stored := storedOn(t, db, sid, "containment_destination", c.dest); stored != (c.status == http.StatusOK) {
				t.Errorf("containment %q stored %v after status %d", c.dest, stored, c.status)
			}
		})
	}
}

// TestCloneWriter_AMemberOnEachLeg: a clone echoes every leg of the claims it
// copies, so a claim naming a member where Core picks is refused, and one
// naming a member as a staging spot copies.
func TestCloneWriter_AMemberOnEachLeg(t *testing.T) {
	t.Parallel()
	h, router := newAdminRouter(t)
	db := dbOf(h)
	withMembers(h)
	cookie := authCookie(t, h)
	for i, row := range []legCensusRow{
		{"clone", "inbound_source", legRefused},
		{"clone", "outbound_destination", legRefused},
		{"clone", "containment_destination", legRefused},
		{"clone", "partial_destination", legRefused},
		{"clone", "inbound_staging", legSaved},
		{"clone", "outbound_staging", legSaved},
	} {
		t.Run(row.leg, func(t *testing.T) {
			pid := seedProcess(t, db, fmt.Sprintf("CL%02d-Line", i))
			sid := seedStyle(t, db, fmt.Sprintf("CL%02d-Src", i), pid)
			// Stored before the rule: no node list to check it against.
			_, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, withNodeOn(cleanLegClaim(sid), row.leg, memberSlot))
			testutil.MustNoErr(t, err, "seed a claim naming a member")
			resp := doRequest(t, router, "POST", "/api/styles/"+itoa(sid)+"/clone",
				map[string]any{"name": fmt.Sprintf("CL-clone-%d", sid)}, cookie)
			body := decodeBody(t, resp)
			got := ""
			switch {
			case resp.StatusCode == http.StatusOK && storedOn(t, db, int64(body["id"].(float64)), row.leg, memberSlot):
				got = legSaved
			case resp.StatusCode == http.StatusBadRequest:
				got = legRefused
			default:
				t.Fatalf("clone: status %d, body %+v", resp.StatusCode, body)
			}
			if got != row.want {
				t.Errorf("clone of a claim naming a member on %s: %s, want %s", row.leg, got, row.want)
			}
		})
	}
}

// TestRoutingWriters_AMemberInEachRole: a routing row is what the composer
// offers on a leg, so a member is refused as a source or a destination and
// saved as a staging spot, on every writer of a row.
func TestRoutingWriters_AMemberInEachRole(t *testing.T) {
	t.Parallel()
	h, router := newAdminRouter(t)
	db := dbOf(h)
	withMembers(h)
	cookie := authCookie(t, h)
	want := map[string]string{"source": legRefused, "destination": legRefused, "staging": legSaved}
	for i, writer := range []string{"routing POST", "routing PUT", "routing PATCH enable"} {
		for j, role := range []string{"source", "destination", "staging"} {
			t.Run(writer+"/"+role, func(t *testing.T) {
				pid := seedProcess(t, db, fmt.Sprintf("RM%d%d-Line", i, j))
				url := "/api/processes/" + itoa(pid) + "/routing-nodes"
				var status int
				switch writer {
				case "routing POST":
					status = doRequest(t, router, "POST", url,
						map[string]any{"core_node_name": memberSlot, "role": role, "enabled": true}, cookie).StatusCode
				case "routing PUT":
					status = doRequest(t, router, "PUT", url, map[string]any{"nodes": []map[string]string{
						{"core_node_name": memberSlot, "role": role}}}, cookie).StatusCode
				case "routing PATCH enable":
					id, err := db.UpsertRoutingNode(domain.CoreNodeKinds{}, processes.RoutingNodeInput{
						ProcessID: pid, CoreNodeName: memberSlot, Role: role, Origin: domain.RoutingOriginBackfill,
					})
					testutil.MustNoErr(t, err, "seed a switched-off member row")
					status = doRequest(t, router, "PATCH", url+"/"+itoa(id), map[string]any{"enabled": true}, cookie).StatusCode
				}
				rows, err := db.ListRoutingNodes(pid)
				testutil.MustNoErr(t, err, "list routing rows")
				enabled := false
				for _, r := range rows {
					if r.CoreNodeName == memberSlot && r.Enabled {
						enabled = true
					}
				}
				got := ""
				switch {
				case status == http.StatusOK && enabled:
					got = legSaved
				case status == http.StatusBadRequest && !enabled:
					got = legRefused
				default:
					t.Fatalf("%s %s: status %d, member row enabled %v", writer, role, status, enabled)
				}
				if got != want[role] {
					t.Errorf("%s with a member as %s: %s, want %s", writer, role, got, want[role])
				}
			})
		}
	}
}

// TestAPIOrders_AMemberWhereCorePicksIsRefused: an order the API takes names
// the source it takes from and the destination it sends to, which are Core's
// to pick in a group. A member or a lane slot is refused with the rule's
// sentence; the group is not.
func TestAPIOrders_AMemberWhereCorePicksIsRefused(t *testing.T) {
	t.Parallel()
	h, router := newApiOrdersRouter(t)
	withMembers(h)
	var cookie *http.Cookie // the creation routes' admin gate is pinned in handlers_api_orders_test.go
	refused := func(t *testing.T, path string, body map[string]any) bool {
		t.Helper()
		resp := doRequest(t, router, "POST", path, body, cookie)
		raw, err := io.ReadAll(resp.Body)
		testutil.MustNoErr(t, err, "read the answer")
		msg := string(raw)
		return resp.StatusCode == http.StatusBadRequest &&
			(strings.Contains(msg, protocol.MsgMemberIsCoresToPick) || strings.Contains(msg, protocol.MsgNotInCoresNodeList))
	}
	for _, name := range []string{memberSlot, laneSlot} {
		for _, c := range []struct {
			path string
			body map[string]any
		}{
			{"/api/orders/retrieve", map[string]any{"payload_code": "P", "source_node": name, "delivery_node": "LL-PRESS"}},
			{"/api/orders/retrieve", map[string]any{"payload_code": "P", "source_node": "LL-GRP", "delivery_node": name}},
			{"/api/orders/move", map[string]any{"source_node": name, "delivery_node": "LL-GRP"}},
			{"/api/orders/move", map[string]any{"source_node": "LL-PRESS", "delivery_node": name}},
		} {
			if !refused(t, c.path, c.body) {
				t.Errorf("%s %v: not refused for naming %q", c.path, c.body, name)
			}
		}
	}
	if refused(t, "/api/orders/retrieve", map[string]any{"payload_code": "P", "source_node": "LL-GRP", "delivery_node": "LL-PRESS"}) {
		t.Error("a retrieve from the group was refused by the member rule")
	}
}

// TestCoreNodeRows_AMemberCarriesItsGroup: the pickers' node list marks a
// member with its group, so a picker for a role where Core picks leaves it out.
func TestCoreNodeRows_AMemberCarriesItsGroup(t *testing.T) {
	t.Parallel()
	h, router := newAdminRouter(t)
	withMembers(h)
	resp := doRequest(t, router, "GET", "/api/core-nodes", nil, authCookie(t, h))
	var rows []map[string]any
	decodeJSON(t, resp, &rows)
	groupOf := map[string]any{}
	for _, r := range rows {
		groupOf[r["name"].(string)] = r["group"]
	}
	if groupOf[memberSlot] != "LL-GRP" {
		t.Errorf("the member's row carries group %v, want LL-GRP", groupOf[memberSlot])
	}
	if g, ok := groupOf["LL-GRP"]; !ok || g != nil {
		t.Errorf("the group's own row carries group %v, want none", g)
	}
}
