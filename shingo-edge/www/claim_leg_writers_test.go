package www

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// claim_leg_writers_test.go — every writer of a claim leg or a routing row,
// handed a lane, and what it does with it.
//
// A claim has six legs: inbound_source, outbound_destination,
// changeover_evac_destination, containment_destination, inbound_staging and
// outbound_staging. A routing row is what the composer offers on one of them.
// Lines name node groups, never lanes, so a lane on any leg is a claim that
// searches or stores into that one lane only.
//
// ONE ROW PER (WRITER, LEG), and the writers are listed from the code rather
// than from the validator: a rule every writer "must call" is a rule a writer
// can skip, and the copies, the clone, the containment stamp, the routing
// switch and the derive all used to. The flow composer and the replenishment
// page's reorder edit are engine doors and have their rows in
// engine/claim_leg_writers_test.go.
//
// Outcomes: "saved" means the lane is stored; "refused" means the write was
// turned down and nothing landed; "skipped" means a derive wrote no row for
// the lane and still succeeded.

const (
	legSaved   = "saved"
	legRefused = "refused"
	legSkipped = "skipped"
)

// writersCore is Core's node list for the census: a group, one of its lanes,
// and plain nodes for every leg.
func writersCore() map[string]protocol.NodeInfo {
	core := laneLegsCore()
	for _, n := range []string{"LL-STG2", "LL-HOLD"} {
		core[n] = protocol.NodeInfo{Name: n}
	}
	return core
}

// cleanLegClaim is a single_robot produce claim that names a plain node or a
// group on all six legs and saves through every door.
//
// THE PAYLOAD IS THE STYLE'S OWN. Every claim here declares a containment
// destination, and the station view reads every such claim in the database
// keyed by (destination, payload); rows that shared one payload and differed
// on the outbound destination would read as a conflict in other tests of this
// package, which share the database.
func cleanLegClaim(sid int64) processes.NodeClaimInput {
	evac := "LL-SUP"
	return processes.NodeClaimInput{
		StyleID: sid, CoreNodeName: "LL-PRESS", Role: protocol.ClaimRoleProduce,
		SwapMode: protocol.SwapModeSingleRobot, PayloadCode: fmt.Sprintf("PART-LL-%d", sid),
		InboundSource: "LL-SUP", OutboundDestination: "LL-SUP",
		ChangeoverEvacDestination: &evac, ContainmentDestination: "LL-HOLD",
		InboundStaging: "LL-STG", OutboundStaging: "LL-STG2",
	}
}

// withLaneOn is the clean claim with the lane on one leg.
func withLaneOn(in processes.NodeClaimInput, leg string) processes.NodeClaimInput {
	const lane = "LL-LANE"
	switch leg {
	case "inbound_source":
		in.InboundSource = lane
	case "outbound_destination":
		in.OutboundDestination = lane
	case "changeover_evac_destination":
		v := lane
		in.ChangeoverEvacDestination = &v
	case "containment_destination":
		in.ContainmentDestination = lane
	case "inbound_staging":
		in.InboundStaging = lane
	case "outbound_staging":
		in.OutboundStaging = lane
	default:
		panic("unknown leg " + leg)
	}
	return in
}

// legValue reads one leg off a stored claim.
func legValue(c processes.NodeClaim, leg string) string {
	switch leg {
	case "inbound_source":
		return c.InboundSource
	case "outbound_destination":
		return c.OutboundDestination
	case "changeover_evac_destination":
		return c.ChangeoverEvacDestination
	case "containment_destination":
		return c.ContainmentDestination
	case "inbound_staging":
		return c.InboundStaging
	case "outbound_staging":
		return c.OutboundStaging
	}
	panic("unknown leg " + leg)
}

// seedLegacyLaneClaim stores a claim naming the lane on leg the way a row
// written before the refusal sits in a plant's database: through the store,
// with no node list to check it against.
func seedLegacyLaneClaim(t *testing.T, sid int64, leg string) {
	t.Helper()
	_, err := testDB.UpsertStyleNodeClaim(withLaneOn(cleanLegClaim(sid), leg))
	testutil.MustNoErr(t, err, "seed a stored claim naming a lane on "+leg)
}

// storedLane reports whether any live claim of the style names the lane on leg.
func storedLane(t *testing.T, sid int64, leg string) bool {
	t.Helper()
	claims, err := testDB.ListStyleNodeClaims(sid)
	testutil.MustNoErr(t, err, "list claims")
	for _, c := range claims {
		if legValue(c, leg) == "LL-LANE" {
			return true
		}
	}
	return false
}

// routingHasLane reports whether the process's routing set holds the lane in
// any role.
func routingHasLane(t *testing.T, db *store.DB, pid int64) bool {
	t.Helper()
	rows, err := db.ListRoutingNodes(pid)
	testutil.MustNoErr(t, err, "list routing nodes")
	for _, r := range rows {
		if r.CoreNodeName == "LL-LANE" {
			return true
		}
	}
	return false
}

type legCensusRow struct {
	writer, leg, want string
}

// claimWriterCensus is every claim-leg writer reachable from the admin API and
// the store, per leg, as it stands.
var claimWriterCensus = []legCensusRow{
	{"editor", "inbound_source", legRefused},
	{"editor", "outbound_destination", legRefused},
	{"editor", "changeover_evac_destination", legSaved},
	{"editor", "containment_destination", legSaved},
	{"editor", "inbound_staging", legSaved},
	{"editor", "outbound_staging", legSaved},

	{"containment stamp", "containment_destination", legSaved},
	// The stamp echoes every other leg of the claim it writes. A claim that
	// already names a lane on staging, stamped with a plain destination.
	{"containment stamp, echo", "inbound_staging", legSaved},
	// Two styles, the first clean and the second naming a lane: the stamp
	// lands on both, or on neither. Never on the first alone.
	{"containment stamp, two styles", "inbound_staging", legSaved},

	{"clone", "inbound_source", legSaved},
	{"clone", "outbound_destination", legSaved},
	{"clone", "changeover_evac_destination", legSaved},
	{"clone", "containment_destination", legSaved},
	{"clone", "inbound_staging", legSaved},
	{"clone", "outbound_staging", legSaved},

	{"generate", "inbound_source", legSaved},
	{"generate", "outbound_destination", legSaved},
	{"generate", "changeover_evac_destination", legSaved},
	{"generate", "containment_destination", legSaved},
	{"generate", "inbound_staging", legSaved},
	{"generate", "outbound_staging", legSaved},

	{"copy", "inbound_source", legSaved},
	{"copy", "outbound_destination", legSaved},
	{"copy", "changeover_evac_destination", legSaved},
	{"copy", "containment_destination", legSaved},
	{"copy", "inbound_staging", legSaved},
	{"copy", "outbound_staging", legSaved},

	// The overrides carry four of the six legs.
	{"copy with overrides", "inbound_source", legSaved},
	{"copy with overrides", "outbound_destination", legSaved},
	{"copy with overrides", "inbound_staging", legSaved},
	{"copy with overrides", "outbound_staging", legSaved},
}

// TestClaimLegWriters_ALaneOnEachLeg is the census of claim writers.
func TestClaimLegWriters_ALaneOnEachLeg(t *testing.T) {
	h, router := newAdminRouter(t)
	h.engine.(*stubEngine).core = writersCore()
	cookie := authCookie(t, h)

	for i, row := range claimWriterCensus {
		t.Run(row.writer+"/"+row.leg, func(t *testing.T) {
			tag := fmt.Sprintf("CW%02d", i)
			pid := seedProcess(t, tag+"-Line")
			sid := seedStyle(t, tag+"-Src", pid)
			var got string
			switch row.writer {
			case "editor":
				got = editorOutcome(t, router, cookie, sid, row.leg)
			case "containment stamp":
				got = containmentOutcome(t, router, cookie, pid, sid)
			case "containment stamp, echo":
				got = containmentEchoOutcome(t, router, cookie, pid, sid, row.leg)
			case "containment stamp, two styles":
				got = containmentTwoStylesOutcome(t, router, cookie, pid, sid, row.leg)
			case "clone":
				got = cloneOutcome(t, router, cookie, sid, row.leg)
			case "generate":
				got = generateOutcome(t, router, cookie, sid, row.leg)
			case "copy":
				got = copyOutcome(t, router, cookie, pid, sid, row.leg, false)
			case "copy with overrides":
				got = copyOutcome(t, router, cookie, pid, sid, row.leg, true)
			default:
				t.Fatalf("no driver for writer %q", row.writer)
			}
			if got != row.want {
				t.Errorf("%s with a lane on %s: %s, want %s", row.writer, row.leg, got, row.want)
			}
		})
	}
}

func editorOutcome(t *testing.T, router *chi.Mux, cookie *http.Cookie, sid int64, leg string) string {
	t.Helper()
	resp := doRequest(t, router, "POST", "/api/style-node-claims", withLaneOn(cleanLegClaim(sid), leg), cookie)
	switch {
	case resp.StatusCode == http.StatusOK && storedLane(t, sid, leg):
		return legSaved
	case resp.StatusCode == http.StatusBadRequest && !storedLane(t, sid, leg):
		return legRefused
	}
	t.Fatalf("editor: status %d, body %+v", resp.StatusCode, decodeBody(t, resp))
	return ""
}

func containmentOutcome(t *testing.T, router *chi.Mux, cookie *http.Cookie, pid, sid int64) string {
	t.Helper()
	_, err := testDB.UpsertStyleNodeClaim(cleanLegClaim(sid))
	testutil.MustNoErr(t, err, "seed a clean claim")
	resp := doRequest(t, router, "POST", "/api/processes/"+itoa(pid)+"/containment-setting",
		map[string]any{"enabled": true, "destination": "LL-LANE"}, cookie)
	switch {
	case resp.StatusCode == http.StatusOK && storedLane(t, sid, "containment_destination"):
		return legSaved
	case resp.StatusCode == http.StatusBadRequest && !storedLane(t, sid, "containment_destination"):
		return legRefused
	}
	t.Fatalf("containment stamp: status %d, body %+v", resp.StatusCode, decodeBody(t, resp))
	return ""
}

// stampedHold reports whether the style's claim carries the plain containment
// destination the echo rows stamp.
func stampedHold(t *testing.T, sid int64) bool {
	t.Helper()
	claims, err := testDB.ListStyleNodeClaims(sid)
	testutil.MustNoErr(t, err, "list claims")
	return len(claims) == 1 && claims[0].ContainmentDestination == "LL-SUP"
}

func stampHold(t *testing.T, router *chi.Mux, cookie *http.Cookie, pid int64) int {
	t.Helper()
	return doRequest(t, router, "POST", "/api/processes/"+itoa(pid)+"/containment-setting",
		map[string]any{"enabled": true, "destination": "LL-SUP"}, cookie).StatusCode
}

func containmentEchoOutcome(t *testing.T, router *chi.Mux, cookie *http.Cookie, pid, sid int64, leg string) string {
	t.Helper()
	seedLegacyLaneClaim(t, sid, leg)
	status := stampHold(t, router, cookie, pid)
	switch {
	case status == http.StatusOK && stampedHold(t, sid):
		return legSaved
	case status == http.StatusBadRequest && !stampedHold(t, sid):
		return legRefused
	}
	t.Fatalf("containment echo: status %d, stamped %v", status, stampedHold(t, sid))
	return ""
}

// containmentTwoStylesOutcome: sid is the source style ("…-Src"); a second
// style sorted after it names the lane.
func containmentTwoStylesOutcome(t *testing.T, router *chi.Mux, cookie *http.Cookie, pid, sid int64, leg string) string {
	t.Helper()
	_, err := testDB.UpsertStyleNodeClaim(cleanLegClaim(sid))
	testutil.MustNoErr(t, err, "seed the clean style")
	second := seedStyle(t, fmt.Sprintf("CW-zz-%d", sid), pid)
	seedLegacyLaneClaim(t, second, leg)
	status := stampHold(t, router, cookie, pid)
	switch {
	case status == http.StatusOK && stampedHold(t, sid) && stampedHold(t, second):
		return legSaved
	case status == http.StatusBadRequest && !stampedHold(t, sid) && !stampedHold(t, second):
		return legRefused
	}
	t.Fatalf("containment over two styles: status %d, first stamped %v, second stamped %v",
		status, stampedHold(t, sid), stampedHold(t, second))
	return ""
}

func cloneOutcome(t *testing.T, router *chi.Mux, cookie *http.Cookie, sid int64, leg string) string {
	t.Helper()
	seedLegacyLaneClaim(t, sid, leg)
	resp := doRequest(t, router, "POST", "/api/styles/"+itoa(sid)+"/clone",
		map[string]any{"name": fmt.Sprintf("CW-clone-%d", sid)}, cookie)
	body := decodeBody(t, resp)
	if resp.StatusCode == http.StatusOK {
		if storedLane(t, int64(body["id"].(float64)), leg) {
			return legSaved
		}
	} else if resp.StatusCode == http.StatusBadRequest {
		return legRefused
	}
	t.Fatalf("clone: status %d, body %+v", resp.StatusCode, body)
	return ""
}

func generateOutcome(t *testing.T, router *chi.Mux, cookie *http.Cookie, sid int64, leg string) string {
	t.Helper()
	seedLegacyLaneClaim(t, sid, leg)
	resp := doRequest(t, router, "POST", "/api/styles/"+itoa(sid)+"/generate",
		map[string]any{"variants": []map[string]any{{"name": fmt.Sprintf("CW-gen-%d", sid)}}}, cookie)
	body := decodeBody(t, resp)
	if resp.StatusCode == http.StatusOK {
		ids, _ := body["ids"].([]any)
		if len(ids) == 1 && storedLane(t, int64(ids[0].(float64)), leg) {
			return legSaved
		}
	} else if resp.StatusCode == http.StatusBadRequest {
		return legRefused
	}
	t.Fatalf("generate: status %d, body %+v", resp.StatusCode, body)
	return ""
}

// copyOutcome copies the source onto a sibling. Plain, the source carries the
// lane; with overrides, the source is clean and the override names the lane.
func copyOutcome(t *testing.T, router *chi.Mux, cookie *http.Cookie, pid, sid int64, leg string, override bool) string {
	t.Helper()
	tgt := seedStyle(t, fmt.Sprintf("CW-tgt-%d", sid), pid)
	body := map[string]any{"target_style_ids": []int64{tgt}, "include_payloads": true}
	if override {
		_, err := testDB.UpsertStyleNodeClaim(cleanLegClaim(sid))
		testutil.MustNoErr(t, err, "seed a clean claim")
		body["overrides"] = []map[string]string{{"node": "LL-PRESS", leg: "LL-LANE",
			"payload_code": fmt.Sprintf("PART-LL-%d", tgt)}}
	} else {
		seedLegacyLaneClaim(t, sid, leg)
	}
	resp := doRequest(t, router, "POST", "/api/styles/"+itoa(sid)+"/claims/copy-to", body, cookie)
	assertStatus(t, resp, http.StatusOK)
	var out struct {
		Results []struct {
			Status string `json:"status"`
			Reason string `json:"reason"`
		} `json:"results"`
	}
	testutil.MustNoErr(t, json.NewDecoder(resp.Body).Decode(&out), "decode copy results")
	if len(out.Results) != 1 {
		t.Fatalf("copy: %d results, want 1", len(out.Results))
	}
	switch {
	case out.Results[0].Status == "copied" && storedLane(t, tgt, leg):
		return legSaved
	case out.Results[0].Status == "failed" && !storedLane(t, tgt, leg):
		return legRefused
	}
	t.Fatalf("copy: result %+v", out.Results[0])
	return ""
}

// routingWriterCensus is every routing-row writer, per role, as it stands.
var routingWriterCensus = []legCensusRow{
	{"routing POST", "source", legRefused},
	{"routing POST", "destination", legRefused},
	{"routing POST", "staging", legSaved},
	{"routing PUT", "source", legRefused},
	{"routing PUT", "destination", legRefused},
	{"routing PUT", "staging", legSaved},
	{"routing PATCH enable", "source", legSaved},
	{"routing PATCH enable", "destination", legSaved},
	{"routing PATCH enable", "staging", legSaved},
}

// TestRoutingWriters_ALaneInEachRole is the census of routing-row writers
// driven by a person.
func TestRoutingWriters_ALaneInEachRole(t *testing.T) {
	h, router := newAdminRouter(t)
	h.engine.(*stubEngine).core = writersCore()
	cookie := authCookie(t, h)

	for i, row := range routingWriterCensus {
		t.Run(row.writer+"/"+row.leg, func(t *testing.T) {
			pid := seedProcess(t, fmt.Sprintf("RW%02d-Line", i))
			url := "/api/processes/" + itoa(pid) + "/routing-nodes"
			var status int
			switch row.writer {
			case "routing POST":
				status = doRequest(t, router, "POST", url,
					map[string]any{"core_node_name": "LL-LANE", "role": row.leg, "enabled": true}, cookie).StatusCode
			case "routing PUT":
				status = doRequest(t, router, "PUT", url, map[string]any{"nodes": []map[string]string{
					{"core_node_name": "LL-LANE", "role": row.leg}}}, cookie).StatusCode
			case "routing PATCH enable":
				id, err := testDB.UpsertRoutingNode(processes.RoutingNodeInput{
					ProcessID: pid, CoreNodeName: "LL-LANE", Role: row.leg,
					Origin: domain.RoutingOriginBackfill,
				})
				testutil.MustNoErr(t, err, "seed a switched-off lane row")
				status = doRequest(t, router, "PATCH", url+"/"+itoa(id), map[string]any{"enabled": true}, cookie).StatusCode
			default:
				t.Fatalf("no driver for writer %q", row.writer)
			}
			rows, err := testDB.ListRoutingNodes(pid)
			testutil.MustNoErr(t, err, "list routing rows")
			enabled := false
			for _, r := range rows {
				if r.CoreNodeName == "LL-LANE" && r.Enabled {
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
				t.Fatalf("%s %s: status %d, lane row enabled %v", row.writer, row.leg, status, enabled)
			}
			if got != row.want {
				t.Errorf("%s with a lane as %s: %s, want %s", row.writer, row.leg, got, row.want)
			}
		})
	}
}

// deriveWriterCensus is the routing backfill, per claim leg it reads. "boot"
// is the call cmd/shingoedge makes before Core has been heard from; "HTTP" is
// the Processes page's re-derive, with Core's node list in hand.
var deriveWriterCensus = []legCensusRow{
	{"derive at boot", "inbound_source", legSaved},
	{"derive at boot", "outbound_destination", legSaved},
	{"derive at boot", "changeover_evac_destination", legSaved},
	{"derive at boot", "inbound_staging", legSaved},
	{"derive at boot", "outbound_staging", legSaved},
	{"derive over HTTP", "inbound_source", legSaved},
	{"derive over HTTP", "outbound_destination", legSaved},
	{"derive over HTTP", "changeover_evac_destination", legSaved},
	{"derive over HTTP", "inbound_staging", legSaved},
	{"derive over HTTP", "outbound_staging", legSaved},
}

// TestRoutingDerive_ALaneOnEachLeg is the census of the two derives. The boot
// derive runs over every process in the database, so it gets one of its own.
func TestRoutingDerive_ALaneOnEachLeg(t *testing.T) {
	h, router := newAdminRouter(t)
	h.engine.(*stubEngine).core = writersCore()
	cookie := authCookie(t, h)

	for i, row := range deriveWriterCensus {
		t.Run(row.writer+"/"+row.leg, func(t *testing.T) {
			var db *store.DB
			var pid int64
			switch row.writer {
			case "derive at boot":
				var err error
				db, err = store.Open(filepath.Join(t.TempDir(), "boot.db"))
				testutil.MustNoErr(t, err, "open a boot database")
				t.Cleanup(func() { db.Close() })
				pid, err = db.CreateProcess("DW-Line", "", "", "", "", false)
				testutil.MustNoErr(t, err, "create process")
				sid, err := db.CreateStyle("DW-Style", "", pid)
				testutil.MustNoErr(t, err, "create style")
				_, err = db.UpsertStyleNodeClaim(withLaneOn(cleanLegClaim(sid), row.leg))
				testutil.MustNoErr(t, err, "seed a stored lane claim")
				_, err = db.DeriveRoutingNodes(nil)
				testutil.MustNoErr(t, err, "boot derive")
			case "derive over HTTP":
				db = testDB
				pid = seedProcess(t, fmt.Sprintf("DW%02d-Line", i))
				seedLegacyLaneClaim(t, seedStyle(t, fmt.Sprintf("DW%02d-Style", i), pid), row.leg)
				resp := doRequest(t, router, "POST", "/api/processes/"+itoa(pid)+"/routing-nodes/derive", nil, cookie)
				assertStatus(t, resp, http.StatusOK)
			default:
				t.Fatalf("no driver for writer %q", row.writer)
			}
			got := legSkipped
			if routingHasLane(t, db, pid) {
				got = legSaved
			}
			if got != row.want {
				t.Errorf("%s with a lane on %s: %s, want %s", row.writer, row.leg, got, row.want)
			}
		})
	}
}
