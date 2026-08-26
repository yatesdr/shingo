package engine

import (
	"encoding/json"
	"errors"
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
	processID, err := db.CreateProcess("PI-PROC", "press index", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	nodeID, err = db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "PRESS-HEAD", Code: "PH1",
		Name: "Press Head", Sequence: 1, Enabled: true,
	})
	testutil.MustNoErr(t, err, "create process node")
	styleID, err := db.CreateStyle("PI-STYLE", "press index style", processID)
	testutil.MustNoErr(t, err, "create style")
	testutil.MustNoErr(t, db.SetActiveStyle(processID, &styleID), "set active style")

	claimID, err := upsertClaimLegacySimple(db, processes.NodeClaimInput{
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

// The bug, stated as the fix: a bare on-deck position gets an empty, not a swap
// that can never source one.
func TestRequestEmptyBin_PrimesBarePosition(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	nodeID := seedPressIndexProduce(t, db)
	// Press holds a bin; the deck is bare — the exact Springfield shape.
	eng.coreClient = NewCoreClient(pressIndexBinsStub(t, "PRESS-HEAD").URL)

	got, err := eng.RequestEmptyBin(nodeID, "WIDGET-A")
	if err != nil {
		t.Fatalf("RequestEmptyBin: %v", err)
	}
	if got == nil {
		t.Fatal("the prime order must come back to the caller — the HMI renders it as the notice " +
			"telling the operator to press again when the empty lands")
	}
	if got.DeliveryNode != "PRESS-DECK" {
		t.Errorf("the empty must go to the BARE position, not the press; delivery_node = %q", got.DeliveryNode)
	}
	if !got.RetrieveEmpty {
		t.Error("a prime is a retrieve_empty: a move would hunt a FULL bin in what is an empties pool")
	}

	// THE POINT. Not "a prime was also made" — no swap at all this round. A swap
	// minted here is the un-sourceable index leg, and it takes its evac sibling
	// down with it when the operator gives up and cancels.
	if n := countOrdersByType(t, db, string(protocol.OrderTypeComplex)); n != 0 {
		t.Errorf("a bare on-deck position must suppress the swap entirely; complex orders = %d", n)
	}
}

// Double-tap. The second press must not fire a second empty at the same
// position, and must STILL not mint the swap — the position is physically bare
// whether or not the empty filling it is already on its way, so a swap released
// now gets exactly the un-sourceable leg the first press avoided.
func TestRequestEmptyBin_SecondPressHoldsRatherThanSwaps(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	nodeID := seedPressIndexProduce(t, db)
	eng.coreClient = NewCoreClient(pressIndexBinsStub(t, "PRESS-HEAD").URL)

	if _, err := eng.RequestEmptyBin(nodeID, "WIDGET-A"); err != nil {
		t.Fatalf("first press: %v", err)
	}
	before := countOrdersByType(t, db, string(protocol.OrderTypeRetrieveEmpty))

	_, err := eng.RequestEmptyBin(nodeID, "WIDGET-A")
	if err == nil {
		t.Fatal("the second press has nothing to do and must say so")
	}
	var inFlight *PrimeInFlightError
	if !errors.As(err, &inFlight) {
		t.Fatalf("the refusal must be the ADVISORY type — rendered red it reads as a fault the "+
			"operator has to fix, and the only correct response is to wait; got %T: %v", err, err)
	}
	if !inFlight.Advisory() {
		t.Error("PrimeInFlightError must report itself advisory")
	}
	if n := countOrdersByType(t, db, string(protocol.OrderTypeRetrieveEmpty)); n != before {
		t.Errorf("the second press must not fire a duplicate empty; retrieve_empty %d → %d", before, n)
	}
	if n := countOrdersByType(t, db, string(protocol.OrderTypeComplex)); n != 0 {
		t.Errorf("a still-bare position must keep the swap suppressed on the second press too; "+
			"complex orders = %d", n)
	}
}

// The regression guard, and the more important half of this file: a WHOLE cell
// must still swap. A guard that suppresses the swap whenever it cannot prove
// the cell is full would take the working path down with the broken one.
func TestRequestEmptyBin_WholeCellStillSwaps(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	nodeID := seedPressIndexProduce(t, db)
	eng.coreClient = NewCoreClient(pressIndexBinsStub(t, "PRESS-HEAD", "PRESS-DECK").URL)

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
	eng.coreClient = NewCoreClient(srv.URL)

	_, _ = eng.RequestEmptyBin(nodeID, "WIDGET-A")

	if n := countOrdersByType(t, db, string(protocol.OrderTypeRetrieveEmpty)); n != 0 {
		t.Errorf("an unanswerable occupancy read must not be treated as a bare position; "+
			"retrieve_empty orders = %d", n)
	}
}
