package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"shingo/protocol/testutil"
)

// The declaration channel used to run one way. Core could tell a station that a
// carrier's count had been decided — by a clear, a load, or a cycle count on
// the bins page. A station could only tell Core if the number rode an order
// release. An operator standing next to a carrier, seeing that the count was
// wrong, outside a release, had nowhere to say so, and that is the number most
// likely to be right.

func TestRecordBinCount_DeclaresToCoreAndTakesTheAnswerBack(t *testing.T) {
	t.Parallel()

	const binID, coreEpoch = int64(4242), int64(9)
	var declared struct {
		NodeName  string `json:"node_name"`
		ActualUOP int    `json:"actual_uop"`
		Actor     string `json:"actor"`
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/telemetry/bin-count" {
			_ = json.NewDecoder(r.Body).Decode(&declared)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "ok", "bin_id": binID, "expected": 25,
				"uop_remaining": declared.ActualUOP, "discrepancy": true,
				"delta_epoch": coreEpoch,
			})
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{"occupied": true, "payload_code": "PART-RC"}})
	}))
	defer srv.Close()

	db := testEngineDB(t)
	nodeID, _ := seedManualSwapClaim(t, db, "RC-DECLARE", "consume", "PART-RC", "EMPTY-TOTES")

	eng := testEngine(t, db)
	eng.SetInventoryDeltaSink(&fakeDeltaSink{db: db})
	eng.coreClient = NewCoreClient(srv.URL)

	rt, err := db.EnsureProcessNodeRuntime(nodeID)
	if err != nil || rt == nil {
		t.Fatalf("ensure runtime: %v", err)
	}
	bin := binID
	if err := db.SetProcessNodeRuntimeWithBinAndEpoch(nodeID, rt.ActiveClaimID, &bin, 4, 25); err != nil {
		t.Fatalf("bind the carrier: %v", err)
	}

	testutil.MustNoErr(t, eng.RecordBinCount(nodeID, 200, "operator-under-test"), "RecordBinCount")

	if declared.NodeName != "RC-DECLARE-MSWAP-NODE" || declared.ActualUOP != 200 {
		t.Errorf("Core was told node=%q count=%d, want the node and 200 — a correction that "+
			"does not reach Core leaves the ledger wrong with nobody aware",
			declared.NodeName, declared.ActualUOP)
	}
	if declared.Actor != "operator-under-test" {
		t.Errorf("declared actor = %q, want the operator — a count is a declaration and a "+
			"declaration has somebody behind it", declared.Actor)
	}

	rt, err = db.GetProcessNodeRuntime(nodeID)
	if err != nil || rt == nil {
		t.Fatalf("read runtime: %v", err)
	}
	if rt.RemainingUOPCached != 200 {
		t.Errorf("local count = %d, want 200 — the two sides have to hold the same number "+
			"after a correction", rt.RemainingUOPCached)
	}
	if rt.ActiveBinEpoch != coreEpoch {
		t.Errorf("epoch = %d, want %d — Core's stamp rides the reply, and adopting it costs "+
			"nothing on a write that is happening anyway", rt.ActiveBinEpoch, coreEpoch)
	}
	if rt.ActiveBinID == nil || *rt.ActiveBinID != binID {
		t.Errorf("active bin = %v, want %d — a count correction does not move a carrier", rt.ActiveBinID, binID)
	}
}

// TestRecordBinCount_WritesNothingLocallyWhenCoreRefuses: Core owns what a
// carrier IS, and it refuses a count for a node holding nothing, a negative
// number, or a part with no capacity configured. Writing locally anyway would
// leave the two sides disagreeing with the operator believing they had fixed
// it — which is worse than the wrong number they started with.
func TestRecordBinCount_WritesNothingLocallyWhenCoreRefuses(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/telemetry/bin-count" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "error", "detail": "no bin at node RC-REFUSE-MSWAP-NODE",
			})
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{"occupied": false}})
	}))
	defer srv.Close()

	db := testEngineDB(t)
	nodeID, _ := seedManualSwapClaim(t, db, "RC-REFUSE", "consume", "PART-RC2", "EMPTY-TOTES")

	eng := testEngine(t, db)
	eng.SetInventoryDeltaSink(&fakeDeltaSink{db: db})
	eng.coreClient = NewCoreClient(srv.URL)

	rt, err := db.EnsureProcessNodeRuntime(nodeID)
	if err != nil || rt == nil {
		t.Fatalf("ensure runtime: %v", err)
	}
	bin := int64(5555)
	if err := db.SetProcessNodeRuntimeWithBinAndEpoch(nodeID, rt.ActiveClaimID, &bin, 4, 25); err != nil {
		t.Fatalf("bind: %v", err)
	}

	if err := eng.RecordBinCount(nodeID, 200, "operator"); err == nil {
		t.Fatal("RecordBinCount returned nil when Core refused — the operator would think " +
			"the correction landed")
	}

	rt, err = db.GetProcessNodeRuntime(nodeID)
	if err != nil || rt == nil {
		t.Fatalf("read runtime: %v", err)
	}
	if rt.RemainingUOPCached != 25 {
		t.Errorf("local count = %d, want 25 unchanged — a refused declaration must not be "+
			"written on one side only", rt.RemainingUOPCached)
	}
}

// A negative count is refused before it leaves the station: nothing physical
// can be less than nothing there, and Core refuses it too.
func TestRecordBinCount_RefusesANegativeCount(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	nodeID, _ := seedManualSwapClaim(t, db, "RC-NEG", "consume", "PART-RC3", "EMPTY-TOTES")
	eng := testEngine(t, db)

	if err := eng.RecordBinCount(nodeID, -1, "operator"); err == nil {
		t.Error("a negative count was accepted")
	}
}
