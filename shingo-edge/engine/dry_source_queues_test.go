package engine

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
)

// dry_source_queues_test.go — a consume REQUEST into a source Core reports dry
// queues (owner: "queue is correct, it's trying to source a part"). The pair is
// created and parks at Core on waiting_for_material until stock lands; a second
// request while it is parked creates nothing (the swap is in flight), so the
// Springfield 2026-07-21 loop (two legs created and cancelled together, again
// and again) has nothing to re-arm. The refusal that stood here
// (guardSourceKnownDry) is retired with its pins.

// occupiedStub answers node-bins "occupied" for every node asked, so the
// REQUEST really builds a pair (no node-empty downgrade). A failed write is
// reported with Errorf: the handler runs on the server's goroutine.
func occupiedStub(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/telemetry/node-bins" {
			http.NotFound(w, r)
			return
		}
		var rows []string
		for _, n := range strings.Split(r.URL.Query().Get("nodes"), ",") {
			if n != "" {
				rows = append(rows, `{"node_name":"`+n+`","occupied":true}`)
			}
		}
		if _, err := w.Write([]byte("[" + strings.Join(rows, ",") + "]")); err != nil {
			t.Errorf("occupied stub: write the node-bins answer: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDrySource_APairQueuesAndDoesNotReArm(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	nodeID := seedLineSwapClaim(t, db, "DRYQ", protocol.SwapModeTwoRobot)
	eng := testEngine(t, db)
	testutil.MustNoErr(t, db.ReplaceSourcingState([]protocol.SourcingState{{ProcessID: "DRYQ-PROC", StyleID: "DRYQ-STYLE",
		Status: "red", Missing: []string{"PART-C"}, Absent: []string{"PART-C"}}}), "the feed: no bin of the part anywhere")
	bridgeCore(t, eng, occupiedStub(t).URL)

	res, err := eng.RequestNodeMaterial(nodeID, 1)
	if err != nil {
		t.Fatalf("the REQUEST into a dry source was refused (%v); it queues: the pair parks until stock lands", err)
	}
	if res.OrderA == nil || res.OrderB == nil || len(complexRequestsOnTheWire(t, db)) != 2 {
		t.Fatalf("the REQUEST did not arm the pair (legs on the wire: %d)", len(complexRequestsOnTheWire(t, db)))
	}

	if _, err := eng.RequestNodeMaterial(nodeID, 1); err == nil {
		t.Fatal("a second REQUEST while the pair is parked was accepted; it must meet the swap in flight")
	}
	if n := len(complexRequestsOnTheWire(t, db)); n != 2 {
		t.Errorf("legs on the wire after the second REQUEST = %d, want 2: nothing re-arms a parked pair", n)
	}
}
