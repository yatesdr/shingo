package www

import (
	"net/http"
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/store/processes"
)

// The Quality Hold settings endpoints: the list stamps the derived state
// into its rows, and the containment-setting write stamps/clears the claims.
// A refusal (enable with no destination) is a 400, and the composer's draft
// round-trips through the derived read.
func TestApiProcessContainmentSetting(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	pid, err := testDB.CreateProcess("HoldProc", "", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	styleID, err := testDB.CreateStyle("HOLD-STYLE", "", pid)
	testutil.MustNoErr(t, err, "create style")
	_, err = processes.UpsertClaim(testDB.DB, processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: "PLN-1", Role: "produce",
		SwapMode: "two_robot", PayloadCode: "PART-H",
		InboundStaging: "STG-1", OutboundDestination: "FG-9",
	})
	testutil.MustNoErr(t, err, "seed produce claim")

	// The list starts with the hold off (derived from claims — none yet).
	resp := doRequest(t, router, "GET", "/api/processes", nil, cookie)
	assertStatus(t, resp, http.StatusOK)
	var rows []struct {
		ID                     int64  `json:"id"`
		QualityHoldEnabled     bool   `json:"quality_hold_enabled"`
		QualityHoldDestination string `json:"quality_hold_destination"`
	}
	decodeJSON(t, resp, &rows)
	for _, row := range rows {
		if row.ID == pid && (row.QualityHoldEnabled || row.QualityHoldDestination != "") {
			t.Errorf("row = %+v, want the hold off before any stamp", row)
		}
	}

	// Enable without a destination: refused, nothing stamped.
	resp = doRequest(t, router, "POST", "/api/processes/"+itoa(pid)+"/containment-setting",
		map[string]any{"enabled": true, "destination": ""}, cookie)
	assertStatus(t, resp, http.StatusBadRequest)

	// Enable with one: the produce claim carries it.
	resp = doRequest(t, router, "POST", "/api/processes/"+itoa(pid)+"/containment-setting",
		map[string]any{"enabled": true, "destination": "HOLD-2"}, cookie)
	assertStatus(t, resp, http.StatusOK)
	claims, err := testDB.ListStyleNodeClaims(styleID)
	testutil.MustNoErr(t, err, "list claims")
	if len(claims) != 1 || claims[0].ContainmentDestination != "HOLD-2" {
		t.Errorf("claims = %+v, want the containment route stamped", claims)
	}

	// The list now derives the toggle on.
	resp = doRequest(t, router, "GET", "/api/processes", nil, cookie)
	assertStatus(t, resp, http.StatusOK)
	decodeJSON(t, resp, &rows)
	for _, row := range rows {
		if row.ID == pid && (!row.QualityHoldEnabled || row.QualityHoldDestination != "HOLD-2") {
			t.Errorf("row = %+v, want the hold on with HOLD-2", row)
		}
	}

	// Disable: cleared.
	resp = doRequest(t, router, "POST", "/api/processes/"+itoa(pid)+"/containment-setting",
		map[string]any{"enabled": false, "destination": ""}, cookie)
	assertStatus(t, resp, http.StatusOK)
	claims, err = testDB.ListStyleNodeClaims(styleID)
	testutil.MustNoErr(t, err, "list claims after clear")
	if len(claims) != 1 || claims[0].ContainmentDestination != "" {
		t.Errorf("claims = %+v, want the route cleared", claims)
	}
}
