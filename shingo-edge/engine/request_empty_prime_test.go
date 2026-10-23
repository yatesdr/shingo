package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// request_empty_prime_test.go — the partial-empty prime, reached through
// REQUEST EMPTY BIN.
//
// The prime shipped in BuildProducePlan, which is REQUEST SWAP's planner.
// RequestEmptyBin is a second door onto the same choreography and it called
// BuildSwapDispatch directly, so a press-index cell with a bare on-deck
// position minted the full two-leg swap with no guard: the index leg opens with
// a pickup at a position holding nothing, Core cannot reserve a bin that is not
// there, and the leg sits in `sourcing` until somebody cancels the pair. Its
// sibling evac is cancelled with it, so the cycle never completes and the press
// stays down.
//
// Springfield PLN_004, 2026-08-26: eight cancelled pairs, zero completed
// cycles, while the identical cell one claim over ran fine because its operator
// happened to use the OTHER button.
//
// This is also the button an operator reaches for while LOOKING at an empty
// position — so of the two doors, this is the one that most needed the guard.

// pressIndexBinsStub serves /api/telemetry/node-bins, reporting Occupied=true
// for exactly the named nodes. Occupancy is the whole input to the guard, so
// the test drives it directly rather than through bin state.
func pressIndexBinsStub(t *testing.T, occupied ...string) *httptest.Server {
	t.Helper()
	occ := map[string]bool{}
	for _, n := range occupied {
		occ[n] = true
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/telemetry/node-bins" {
			_ = json.NewEncoder(w).Encode([]map[string]any{})
			return
		}
		rows := []map[string]any{}
		for n := range strings.SplitSeq(r.URL.Query().Get("nodes"), ",") {
			rows = append(rows, map[string]any{"node_name": n, "occupied": occ[n]})
		}
		_ = json.NewEncoder(w).Encode(rows)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// seedPressIndexProduce builds a two-position press-index produce cell:
// PRESS-HEAD is the press, PRESS-DECK the on-deck position the index leg
// picks from.
func seedPressIndexProduce(t *testing.T, db *store.DB) (nodeID int64) {
	t.Helper()
	processID, err := db.CreateProcess("PI-PROC", "press index", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	nodeID, err = db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "PRESS-HEAD", Code: "PH1",
		Name: "Press Head", Sequence: 1, Enabled: true,
	})
	testutil.MustNoErr(t, err, "create process node")
	styleID, err := db.CreateStyle("PI-STYLE", "press index style", processID)
	testutil.MustNoErr(t, err, "create style")
	testutil.MustNoErr(t, db.SetActiveStyle(processID, &styleID), "set active style")

	claimID, err := upsertClaimRetiredMode(db, processes.NodeClaimInput{
		StyleID:             styleID,
		CoreNodeName:        "PRESS-HEAD",
		Role:                "produce",
		SwapMode:            protocol.SwapModeTwoRobotPressIndex,
		PayloadCode:         "WIDGET-A",
		UOPCapacity:         100,
		AutoReorder:         domain.Ptr(false),
		InboundSource:       "EMPTY-STORAGE",
		OutboundDestination: "FILLED-STORAGE",
		PairedCoreNode:      "PRESS-DECK",
	})
	testutil.MustNoErr(t, err, "upsert claim")

	_, err = db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "ensure runtime")
	// ZERO COUNTED PARTS, deliberately. Springfield's counter tag is not wired,
	// so every cell there reads 0 forever — the state the guard has to work in.
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, &claimID, 0), "set runtime")
	return nodeID
}

func countOrdersByType(t *testing.T, db *store.DB, orderType string) int {
	t.Helper()
	var n int
	testutil.MustNoErr(t, db.DB.QueryRow(
		`SELECT count(*) FROM orders WHERE order_type = ?`, orderType).Scan(&n), "count orders")
	return n
}

// The bug, stated as the fix: a vacant on-deck position gets an empty, and the
// swap is queued behind it, never ahead: Core ingests in creation order and
// holds the pair for the empty coming to its on-deck (W5), so the index leg
// never meets a vacant position with nothing on its way.
func TestRequestEmptyBin_PrimesTheVacantPositionAheadOfTheSwap(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	nodeID := seedPressIndexProduce(t, db)
	// Press holds a bin; the deck is vacant — the exact Springfield shape.
	bridgeCore(t, eng, pressIndexBinsStub(t, "PRESS-HEAD").URL)
	holdPlaces(t, eng, occupiedAt("PRESS-HEAD"))

	if _, err := eng.RequestEmptyBin(nodeID, "WIDGET-A"); err != nil {
		t.Fatalf("RequestEmptyBin: %v", err)
	}
	all, err := db.ListOrders()
	testutil.MustNoErr(t, err, "list orders")
	var prime int64
	var legs []int64
	for _, o := range all {
		switch {
		case o.RetrieveEmpty && o.DeliveryNode == "PRESS-DECK":
			prime = o.ID
		case o.OrderType == protocol.OrderTypeComplex:
			legs = append(legs, o.ID)
		}
	}
	if prime == 0 || len(legs) != 2 {
		t.Fatalf("want a retrieve_empty to the vacant PRESS-DECK and the two-leg swap; prime %d, legs %v", prime, legs)
	}
	for _, l := range legs {
		if l < prime {
			t.Errorf("leg %d was created before the prime %d: the swap must queue behind it", l, prime)
		}
	}
}

// Double-tap. The second press meets the swap in flight and adds nothing: no
// second empty at the same position, no second swap.
func TestRequestEmptyBin_SecondPressMeetsTheSwap(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	nodeID := seedPressIndexProduce(t, db)
	bridgeCore(t, eng, pressIndexBinsStub(t, "PRESS-HEAD").URL)
	holdPlaces(t, eng, occupiedAt("PRESS-HEAD"))

	if _, err := eng.RequestEmptyBin(nodeID, "WIDGET-A"); err != nil {
		t.Fatalf("first press: %v", err)
	}
	empties := countOrdersByType(t, db, string(protocol.OrderTypeRetrieveEmpty))
	legs := countOrdersByType(t, db, string(protocol.OrderTypeComplex))

	if _, err := eng.RequestEmptyBin(nodeID, "WIDGET-A"); err == nil {
		t.Fatal("the second press has nothing to do and must say so")
	}
	if n := countOrdersByType(t, db, string(protocol.OrderTypeRetrieveEmpty)); n != empties {
		t.Errorf("the second press must not fire a duplicate empty; retrieve_empty %d → %d", empties, n)
	}
	if n := countOrdersByType(t, db, string(protocol.OrderTypeComplex)); n != legs {
		t.Errorf("the second press must not mint a second swap; complex orders %d → %d", legs, n)
	}
}

// The regression guard, and the more important half of this file: a WHOLE cell
// must still swap, and prime nothing. A chain built whenever the read cannot
// prove the cell full would send carriers at positions that hold one.
func TestRequestEmptyBin_WholeCellStillSwaps(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	nodeID := seedPressIndexProduce(t, db)
	bridgeCore(t, eng, pressIndexBinsStub(t, "PRESS-HEAD", "PRESS-DECK").URL)
	holdPlaces(t, eng, occupiedAt("PRESS-HEAD", "PRESS-DECK"))

	if _, err := eng.RequestEmptyBin(nodeID, "WIDGET-A"); err != nil {
		t.Fatalf("RequestEmptyBin on a full cell: %v", err)
	}
	if n := countOrdersByType(t, db, string(protocol.OrderTypeComplex)); n == 0 {
		t.Error("both positions occupied is the ordinary swap — the guard must stand aside")
	}
	if n := countOrdersByType(t, db, string(protocol.OrderTypeRetrieveEmpty)); n != 0 {
		t.Errorf("nothing is bare, so nothing should be primed; retrieve_empty orders = %d", n)
	}
}

// Core unreachable reads as OCCUPIED everywhere (isOccupied's missing-entry
// default), so a Core blip must not start firing empties at positions nobody
// can see. Fail toward today's behaviour, not toward phantom deliveries.
func TestRequestEmptyBin_UnreachableCoreDoesNotPrime(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	nodeID := seedPressIndexProduce(t, db)
	srv := pressIndexBinsStub(t, "PRESS-HEAD")
	srv.Close() // Core is down for this one
	bridgeCore(t, eng, srv.URL)
	holdPlaces(t, eng, occupiedAt("PRESS-HEAD"))

	_, _ = eng.RequestEmptyBin(nodeID, "WIDGET-A")

	if n := countOrdersByType(t, db, string(protocol.OrderTypeRetrieveEmpty)); n != 0 {
		t.Errorf("an unanswerable occupancy read must not be treated as a bare position; "+
			"retrieve_empty orders = %d", n)
	}
}
