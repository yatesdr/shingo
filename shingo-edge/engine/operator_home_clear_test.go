package engine

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
)

// homeClearCore is a Core that answers the home clear with status and body,
// and counts the requests and every other call (there must be none).
func homeClearCore(t *testing.T, status int, body protocol.ClearHomeResponse) (*httptest.Server, *atomic.Int64, *atomic.Int64, *protocol.ClearHomeRequest) {
	t.Helper()
	var clears, others atomic.Int64
	got := &protocol.ClearHomeRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/telemetry/clear-home" {
			others.Add(1)
			http.NotFound(w, r)
			return
		}
		clears.Add(1)
		if err := json.NewDecoder(r.Body).Decode(got); err != nil {
			t.Errorf("decode the clear request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Errorf("encode the answer: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &clears, &others, got
}

// TestClearLoaderHome_OneRequestRecordsTheZeroAndCreatesNothing: the Edge
// sends Core one request naming the home and its station, records the zero
// Core answers with (count 0 under the carrier's new stamp, known-empty), and
// creates no order and writes no runtime order pointer: Core authored the
// orders. Every arm records the zero and says what Core is doing in one
// sentence; the arm "none" is the tote-stacks tap's sentence (HomeClearStays).
// The one-live-clear fence is HomeClearRefused and a node Core cannot clear
// is HomeClearInvalid, each carrying Core's sentence, with nothing recorded.
func TestClearLoaderHome_OneRequestRecordsTheZeroAndCreatesNothing(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		status  int
		body    protocol.ClearHomeResponse
		said    string
		refused string // a HomeClearRefused sentence
		invalid string // a HomeClearInvalid sentence
		zeroed  bool
	}{
		{"spot", http.StatusOK, protocol.ClearHomeResponse{Status: "ok", Arm: protocol.ClearArmSpot,
			ParentUUID: "core-clear-x", BinID: 41, DeltaEpoch: 7}, homeClearSpot, "", "", true},
		{"demand", http.StatusOK, protocol.ClearHomeResponse{Status: "ok", Arm: protocol.ClearArmDemand,
			ParentUUID: "core-clear-y", BinID: 41, DeltaEpoch: 7}, homeClearDemand, "", "", true},
		{"none", http.StatusOK, protocol.ClearHomeResponse{Status: "ok", Arm: protocol.ClearArmNone,
			Detail: "the partial stays on its spot for now", BinID: 41, DeltaEpoch: 7}, HomeClearStays, "", "", true},
		{"fence", http.StatusConflict, protocol.ClearHomeResponse{Status: "error",
			Error: "CLH-HOME is already being cleared"}, "", "CLH-HOME is already being cleared", "", false},
		{"not a home", http.StatusBadRequest, protocol.ClearHomeResponse{Status: "error",
			Error: "CLH-HOME is a buffer position, not a home"}, "", "", "CLH-HOME is a buffer position, not a home", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			db := openIntentDB(t, copyEngineDBTemplate(t))
			defer db.Close()
			nodeID := seedIntentNode(t, db, "CLH-PROC", "CLH-HOME")
			binID := int64(41)
			_, err := db.EnsureProcessNodeRuntime(nodeID)
			testutil.MustNoErr(t, err, "runtime")
			testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBinAndEpoch(nodeID, nil, &binID, 3, 12), "the home's full carrier")
			srv, clears, others, req := homeClearCore(t, c.status, c.body)
			eng := newIntentEngine(t, db)
			eng.coreClient = stubCoreClient(srv.URL)

			said, err := eng.ClearLoaderHome(nodeID)
			var refused *HomeClearRefused
			var invalid *HomeClearInvalid
			switch {
			case c.refused != "":
				if !errors.As(err, &refused) || refused.Sentence != c.refused {
					t.Fatalf("ClearLoaderHome = %v, want HomeClearRefused %q", err, c.refused)
				}
			case c.invalid != "":
				if !errors.As(err, &invalid) || invalid.Sentence != c.invalid {
					t.Fatalf("ClearLoaderHome = %v, want HomeClearInvalid %q", err, c.invalid)
				}
			case err != nil:
				t.Fatalf("ClearLoaderHome: %v", err)
			case said != c.said:
				t.Errorf("the board is told %q, want %q", said, c.said)
			}
			if clears.Load() != 1 || others.Load() != 0 || req.HomeNode != "CLH-HOME" || req.Station != eng.cfg.StationID() {
				t.Errorf("Core saw %d clears, %d other calls, request %+v; want one clear naming CLH-HOME and the station",
					clears.Load(), others.Load(), *req)
			}
			rt, err := db.GetProcessNodeRuntime(nodeID)
			testutil.MustNoErr(t, err, "read runtime")
			if c.zeroed && (rt.RemainingUOPCached != 0 || rt.ActiveBinEpoch != 7 || !rt.LinesidePayloadKnown || rt.LinesidePayloadCode != "") {
				t.Errorf("the zero was not recorded: uop=%d epoch=%d known=%v code=%q", rt.RemainingUOPCached, rt.ActiveBinEpoch,
					rt.LinesidePayloadKnown, rt.LinesidePayloadCode)
			}
			if !c.zeroed && (rt.RemainingUOPCached != 12 || rt.ActiveBinEpoch != 3) {
				t.Errorf("a refusal before the zero changed the home: uop=%d epoch=%d", rt.RemainingUOPCached, rt.ActiveBinEpoch)
			}
			if rt.ActiveOrderID != nil || rt.StagedOrderID != nil {
				t.Errorf("runtime order pointers written: active=%v staged=%v", rt.ActiveOrderID, rt.StagedOrderID)
			}
			orders, err := db.ListActiveOrdersByProcessNode(nodeID)
			testutil.MustNoErr(t, err, "orders")
			if len(orders) != 0 {
				t.Errorf("the Edge created %d order(s); Core authors the clear's", len(orders))
			}
		})
	}
}
