package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/store"
)

// operator_bin_ops_clear_order_test.go — the empty-out is created AFTER the
// manifest clear commits, and only once.
//
// THE RACE. The U2 used to be created BEFORE the bin-clear round trip to Core,
// on the reasoning that Core's bin record was "still coherent". Everything the
// U2 needs from that record is captured into locals first, so the ordering
// bought nothing — and it cost a window in which the carrier the U2 names still
// carries its payload on Core's side. A mover that reads the bin in that window
// carries a LABELLED carrier to the empty-totes destination, where the payload
// then exists only as a row nobody reconciles.
//
// WHAT MUST SURVIVE THE REORDER, and is pinned by the tests already in
// operator_bin_ops_test.go rather than repeated here: the U2 still fires for a
// PRESS-FED drain with no inbound U1 at all (that is why it hangs off the CLEAR
// and not off a U1 completion), it still fires exactly once, and the cleared
// payload still threads onto the move for tile routing.

// clearOrderServer records, at the moment Core is asked to clear the bin,
// whether an empty-out move already exists at the node. That is the ordering
// assertion: a move present at clear time means the U2 was created first.
func clearOrderServer(t *testing.T, db *store.DB, nodeID int64, payload string) (*httptest.Server, func() int) {
	t.Helper()
	var mu sync.Mutex
	movesAtClearTime := -1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/telemetry/node-bins":
			json.NewEncoder(w).Encode([]map[string]any{{"occupied": true, "payload_code": payload}})
		case "/api/telemetry/bin-clear":
			mu.Lock()
			all, err := db.ListActiveOrdersByProcessNode(nodeID)
			if err != nil {
				t.Errorf("ListActiveOrdersByProcessNode during clear: %v", err)
			}
			n := 0
			for _, o := range all {
				if o.OrderType == orders.TypeMove {
					n++
				}
			}
			movesAtClearTime = n
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		default:
			json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() int {
		mu.Lock()
		defer mu.Unlock()
		return movesAtClearTime
	}
}

// TestClearBin_EmptyOutIsCreatedAfterTheClearCommits is the ordering pin.
func TestClearBin_EmptyOutIsCreatedAfterTheClearCommits(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	nodeID, _ := seedManualSwapClaim(t, db, "U2-ORDER", "consume", "ORD-PART", "EMPTY-TOTES")

	srv, movesAtClear := clearOrderServer(t, db, nodeID, "ORD-PART")
	eng := testEngine(t, db)
	eng.coreClient = NewCoreClient(srv.URL)

	testutil.MustNoErr(t, eng.ClearBin(nodeID, ""), "ClearBin")

	if got := movesAtClear(); got != 0 {
		t.Errorf("%d empty-out move(s) already existed when Core was asked to clear the bin; "+
			"want 0. A U2 minted before the clear commits names a carrier that still "+
			"carries its payload on Core's side, and a mover reading it in that window "+
			"delivers a LABELLED carrier into the empties destination", got)
	}
	// And it did still fire — the reorder must not turn the U2 off.
	if n, payload := countMovesTo(t, db, nodeID, "EMPTY-TOTES"); n != 1 {
		t.Errorf("empty-out moves after the clear = %d, want exactly 1", n)
	} else if payload != "ORD-PART" {
		t.Errorf("empty-out payload = %q, want %q — the cleared payload is captured "+
			"BEFORE the clear precisely so the reorder can keep threading it", payload, "ORD-PART")
	}
}

// TestClearBin_DoubleTapCreatesOneEmptyOut pins the guard the reorder made
// necessary. PushEmptyOut has carried this guard all along ("the order layer has
// no dedup for move orders"); ClearBin had none, and moving the create after a
// round trip to Core widened the window a second tap can land in.
func TestClearBin_DoubleTapCreatesOneEmptyOut(t *testing.T) {
	t.Parallel()
	srv := fakeCoreBinServer(t, true, "TAP-PART")

	db := testEngineDB(t)
	nodeID, _ := seedManualSwapClaim(t, db, "U2-TAP", "consume", "TAP-PART", "EMPTY-TOTES")

	eng := testEngine(t, db)
	eng.coreClient = NewCoreClient(srv.URL)

	testutil.MustNoErr(t, eng.ClearBin(nodeID, ""), "ClearBin first tap")
	// The second tap must not mint a second move for one physical carrier — and
	// must NOT report an error either. The clear itself succeeded; telling the
	// operator their CLEAR failed would be a lie, and a retry would then find no
	// bin and create nothing at all. That is why this guard SKIPS where
	// PushEmptyOut's refuses.
	testutil.MustNoErr(t, eng.ClearBin(nodeID, ""), "ClearBin second tap")

	if n, _ := countMovesTo(t, db, nodeID, "EMPTY-TOTES"); n != 1 {
		t.Errorf("empty-out moves after two CLEAR taps = %d, want exactly 1. Two robots "+
			"would be sent for one carrier and the second would find nothing there", n)
	}
}
