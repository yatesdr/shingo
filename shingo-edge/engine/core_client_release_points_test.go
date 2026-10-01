package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
)

// TestCoreClient_ReleasePoints round-trips the release-points read: the
// request names the station and the orders, the reply decodes into the
// protocol type, and a non-200 is an error.
func TestCoreClient_ReleasePoints(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/release/points" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var req protocol.ReleasePointsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.StationID != "line-1" || len(req.OrderUUIDs) != 1 {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := json.NewEncoder(w).Encode(protocol.ReleasePointsResponse{Points: []protocol.ReleasePoint{{
			OrderUUID: req.OrderUUIDs[0], Found: true, Enters: []string{"SYN-PRESS"},
			AwaitsLift: []protocol.LiftDependency{{LifterUUID: "u-r1", Node: "SYN-PRESS", CoRelease: true}},
		}}}); err != nil {
			t.Errorf("encode: %v", err)
		}
	}))
	defer srv.Close()
	pts, err := NewCoreClient(srv.URL).ReleasePoints("line-1", []string{"u-r2"})
	testutil.MustNoErr(t, err, "release points")
	if len(pts) != 1 || !pts[0].Found || pts[0].AwaitsLift[0].LifterUUID != "u-r1" || !pts[0].AwaitsLift[0].CoRelease {
		t.Errorf("points = %+v", pts)
	}
	if _, err := NewCoreClient(srv.URL).ReleasePoints("line-9", []string{"u-r2"}); err == nil {
		t.Error("a 400 from Core decoded as an answer")
	}
}
