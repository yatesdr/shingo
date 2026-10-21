package engine

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// dry_source_guard_roundtrip_test.go — what guardSourceKnownDry costs the Pi.
//
// D1 pinned 1 synchronous POST /api/inventory/preflight per consume REQUEST that
// arms a swap pair, and 0 otherwise. D2.5 puts the answer the guard needs
// (absent: no bin of the payload anywhere) on Core's sourcing feed, which the
// Edge already holds, so the guard is a local read: 0 round trips on every
// REQUEST, the pair-arming consume's 1 → 0 the change's named delta. RED at
// 5f3d673f (1 for the pair).

// countingPreflightStub answers node-bins "occupied" (so a two-robot consume
// really builds a pair) and preflight with stock present but none free, and
// counts the preflight calls.
func countingPreflightStub(t *testing.T, calls *atomic.Int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/telemetry/node-bins":
			var rows []string
			for _, n := range strings.Split(r.URL.Query().Get("nodes"), ",") {
				if n != "" {
					rows = append(rows, `{"node_name":"`+n+`","occupied":true}`)
				}
			}
			if _, err := w.Write([]byte("[" + strings.Join(rows, ",") + "]")); err != nil {
				t.Errorf("stub: write node-bins: %v", err)
			}
		case "/api/inventory/preflight":
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			if _, err := w.Write([]byte(`{"missing":["PART-C"],"absent":[],"available":[]}`)); err != nil {
				t.Errorf("stub: write preflight: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDrySourceGuard_NoRoundTripOnAnyRequest(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		seed      func(t *testing.T, db *store.DB) int64
		wantCalls int64
		wantPair  bool
		produce   bool // the produce door, RequestProduceSwap
	}{
		{"consume two_robot arms a pair", func(t *testing.T, db *store.DB) int64 {
			return seedLineSwapClaim(t, db, "RTP2", protocol.SwapModeTwoRobot)
		}, 0, true, false},
		{"consume single_robot is one order", func(t *testing.T, db *store.DB) int64 {
			return seedSingleRobotConsume(t, db, "RTP1")
		}, 0, false, false},
		{"produce two_robot arms a pair and never asks", func(t *testing.T, db *store.DB) int64 {
			_, nodeID, _, _ := seedProduceNode(t, db, protocol.SwapModeTwoRobot)
			return nodeID
		}, 0, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testEngineDB(t)
			nodeID := tc.seed(t, db)
			var calls atomic.Int64
			eng := testEngine(t, db)
			bridgeCore(t, eng, countingPreflightStub(t, &calls).URL)

			var err error
			if tc.produce {
				_, err = eng.RequestProduceSwap(nodeID, 0)
			} else {
				_, err = eng.RequestNodeMaterial(nodeID, 1)
			}
			testutil.MustNoErr(t, err, "the REQUEST")
			if tc.wantPair {
				if n := len(complexRequestsOnTheWire(t, db)); n != 2 {
					t.Fatalf("the REQUEST put %d legs on the wire, want the pair", n)
				}
			}
			if got := calls.Load(); got != tc.wantCalls {
				t.Errorf("preflight round trips = %d, want %d (err=%v)", got, tc.wantCalls, err)
			}
		})
	}
}

// seedSingleRobotConsume is seedLineSwapClaim's consume line in single_robot
// mode, which takes no paired node.
func seedSingleRobotConsume(t *testing.T, db *store.DB, prefix string) int64 {
	t.Helper()
	processID, err := db.CreateProcess(prefix+"-PROC", "", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: prefix + "-LINE", Code: prefix, Name: prefix + " line", Enabled: true,
	})
	testutil.MustNoErr(t, err, "create node")
	styleID, err := db.CreateStyle(prefix+"-STYLE", "", processID)
	testutil.MustNoErr(t, err, "create style")
	testutil.MustNoErr(t, db.SetActiveStyle(processID, &styleID), "set active style")
	_, err = db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: prefix + "-LINE", Role: protocol.ClaimRoleConsume,
		SwapMode: protocol.SwapModeSingleRobot, PayloadCode: "PART-C", UOPCapacity: 100,
		InboundSource: prefix + "-MARKET", InboundStaging: prefix + "-IN-STAGE",
		OutboundStaging: prefix + "-OUT-STAGE", OutboundDestination: prefix + "-MARKET",
	})
	testutil.MustNoErr(t, err, "upsert claim")
	_, err = db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "ensure runtime")
	return nodeID
}
