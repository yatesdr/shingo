package engine

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/orders"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// ── W1: THE DOUBLE-SUPPLY RACE ────────────────────────────────────────────
//
// One run of plants/demo.yaml on 2026-08-31 lost four cells to the same three
// seconds. The trace at ALN_004, wall clock:
//
//	05:51:45  bin_picked_up: bin=24 at=ALN_004         the swap lifts the old carrier
//	05:51:46  autoreorder eval: gate=no_bin_bound      ticks held, evaluator asleep
//	05:51:50  uop_adjustment: bound bin 24 to ALN_004  Core's intermediate dropoff re-arms it
//	05:51:52  autoreorder eval: remaining=13 canAccept=true reason=
//	05:51:52  [request-material] ALN_004 is empty (no bin), downgrading single_robot
//	05:51:52  [orders] create: type=move id=77 -> ALN_004
//	05:51:54  the in-flight swap sets ITS OWN bin down at ALN_004
//	05:51:56  order 137 HOLDING at ALN_004 — cannot place onto an occupied position
//
// and order 137 never moved again. It was written into ALN_004's ActiveOrderID,
// so CanAcceptOrders answered "active order in progress" forever and the removal
// that would have freed the position could never be raised. A machine, a robot
// and a carrier, locked together until a person intervenes.
//
// THE POPULATION THESE CASES HAVE TO REPRODUCE is the state of that cell at
// 05:51:52, and the part that is hard to believe is that BOTH runtime order
// pointers are empty. The swap's own pickup nulled ActiveOrderID at :45 —
// correctly, for reasons handler_bin_picked_up.go carries a Springfield incident
// about — and a single_robot swap has no staged leg. So the runtime says nothing
// at all, while orders.process_node_id still names the cell. That row is the
// witness, and asking it is the whole fix.

// spokenForFixture seeds a consume cell mid-swap: single_robot, below its
// reorder point, with Core answering the head position's occupancy.
//
// occupied selects which half of the swap Core is asked about: false is the
// pickup→delivery gap the race lives in, true is the same cell once the carrier
// has landed.
func spokenForFixture(t *testing.T, occupied bool) (*Engine, *store.DB, int64) {
	t.Helper()
	db := testEngineDB(t)
	eng := testEngine(t, db)

	procID, err := db.CreateProcess("W1-PROC", "double-supply race", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: procID, CoreNodeName: "ALN_004", Code: "A04", Name: "ALN_004", Sequence: 1, Enabled: true,
	})
	testutil.MustNoErr(t, err, "create node")
	styleID, err := db.CreateStyle("W1-STYLE", "", procID)
	testutil.MustNoErr(t, err, "create style")
	testutil.MustNoErr(t, db.SetActiveStyle(procID, &styleID), "set active style")

	claimID, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: "ALN_004", Role: protocol.ClaimRoleConsume,
		SwapMode: protocol.SwapModeSingleRobot, PayloadCode: "BRKT",
		UOPCapacity: 40, ReorderPoint: 25, AutoReorder: domain.Ptr(true),
		InboundSource: "SYN_MARKET", OutboundDestination: "SYN_MARKET",
		InboundStaging: "SLN_005", OutboundStaging: "SLN_006",
	})
	testutil.MustNoErr(t, err, "upsert claim")

	_, err = db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "ensure runtime")
	// remaining=13, under the reorder point of 25 — the cell wants material.
	// active_bin_id is bin 24, exactly as Core's 05:51:50 bind left it; the
	// runtime ORDER pointers stay nil, which is the state under test.
	bin := int64(24)
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, &bin, 13), "seed runtime")

	eng.coreClient = NewCoreClient(headOccupancyStub(t, occupied).URL)
	return eng, db, nodeID
}

// headOccupancyStub answers Core's node-bins telemetry with one fixed verdict
// for every node asked about. `false` is Core being honest during the
// pickup→delivery gap: the carrier really is in the robot's hands.
func headOccupancyStub(t *testing.T, occupied bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rows := []map[string]any{}
		if r.URL.Path == "/api/telemetry/node-bins" {
			for _, n := range strings.Split(r.URL.Query().Get("nodes"), ",") {
				if n == "" {
					continue
				}
				rows = append(rows, map[string]any{"node_name": n, "occupied": occupied})
			}
		}
		_ = json.NewEncoder(w).Encode(rows)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// seedInFlightSwap creates the swap order the way a single_robot cycle leaves it
// in the Edge DB at the moment of the race.
//
// delivery_node is the SUPERMARKET, not the cell, and that is not a shortcut in
// the fixture: a single_robot swap is ONE complex order carrying the new carrier
// in and the old one out, so the order's delivery node is where the OLD carrier
// ends up and the new one lands at the cell as an intermediate dropoff. It is
// why a delivery-node lookup cannot see this order coming, and why the guard
// asks process_node_id instead.
func seedInFlightSwap(t *testing.T, db *store.DB, nodeID int64) int64 {
	t.Helper()
	orderID, err := db.CreateOrder("w1-swap-in-flight", orders.TypeComplex,
		&nodeID, false, 1, "SYN_MARKET", "", "", "", true, "BRKT")
	testutil.MustNoErr(t, err, "create in-flight swap order")
	testutil.MustNoErr(t, db.UpdateOrderStatus(orderID, string(orders.StatusInTransit)), "set in_transit")
	return orderID
}

func countOrders(t *testing.T, db *store.DB) int {
	t.Helper()
	var n int
	testutil.MustNoErr(t, db.DB.QueryRow("SELECT COUNT(*) FROM orders").Scan(&n), "count orders")
	return n
}

// TestConsumeDowngrade_RefusesWhileTheSwapIsStillInFlight is the race case.
//
// RED at 771e9b75: the request is ACCEPTED and mints a second plain move
// SYN_MARKET → ALN_004 — order 137 in the run above, the one that never moved
// again.
func TestConsumeDowngrade_RefusesWhileTheSwapIsStillInFlight(t *testing.T) {
	t.Parallel()
	eng, db, nodeID := spokenForFixture(t, false)
	swapID := seedInFlightSwap(t, db, nodeID)

	// The pickup has already fired, so the cell's runtime names nothing. Assert
	// that rather than assume it: the whole defect is that this state LOOKS idle
	// and is not, and a fixture that quietly left a pointer set would pass for
	// the wrong reason.
	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "read runtime")
	if rt.ActiveOrderID != nil || rt.StagedOrderID != nil {
		t.Fatalf("fixture drifted: both runtime pointers must be nil at the moment of the race, got active=%v staged=%v",
			rt.ActiveOrderID, rt.StagedOrderID)
	}
	if ok, reason := eng.CanAcceptOrders(nodeID); !ok {
		t.Fatalf("fixture drifted: CanAcceptOrders said %q, but at 05:51:52 it said true — that is why the tick fired", reason)
	}

	before := countOrders(t, db)
	res, err := eng.requestNodeMaterialFor(nodeID, 1, protocol.EpisodeTriggerAutoreorder)
	if err == nil {
		minted := "nothing"
		if res != nil && res.Order != nil {
			minted = fmt.Sprintf("order %d (%s %s→%s)", res.Order.ID, res.Order.OrderType,
				res.Order.SourceNode, res.Order.DeliveryNode)
		}
		t.Fatalf("request ACCEPTED while swap %d is in flight; it minted %s — this is the double-supply race",
			swapID, minted)
	}
	if !strings.Contains(err.Error(), "already on its way") {
		t.Errorf("refusal = %q, want the sentence that says a bin is already coming", err)
	}
	if !strings.Contains(err.Error(), "still working this position") {
		t.Errorf("refusal = %q, want it to name the in-flight order", err)
	}
	if after := countOrders(t, db); after != before {
		t.Errorf("order count %d → %d: a second delivery was minted into a position a robot is already filling",
			before, after)
	}
}

// TestConsumeDowngrade_MintsOnceTheSwapCompletes is the escape case, and it is
// the half that proves replenishment was not simply switched off. Same cell,
// same threshold; the only difference is that the swap has landed.
func TestConsumeDowngrade_MintsOnceTheSwapCompletes(t *testing.T) {
	t.Parallel()
	eng, db, nodeID := spokenForFixture(t, true)
	swapID := seedInFlightSwap(t, db, nodeID)
	testutil.MustNoErr(t, db.UpdateOrderStatus(swapID, string(orders.StatusConfirmed)), "complete the swap")

	before := countOrders(t, db)
	if _, err := eng.requestNodeMaterialFor(nodeID, 1, protocol.EpisodeTriggerAutoreorder); err != nil {
		t.Fatalf("request refused after the swap completed: %v — the cell can no longer be resupplied", err)
	}
	if after := countOrders(t, db); after <= before {
		t.Errorf("order count %d → %d: nothing was minted for a cell below its reorder point", before, after)
	}
}

// TestConsumeDowngrade_StillDowngradesWhenTheNodeIsTrulyBare keeps the downgrade
// honest. A person pulled the carrier off by hand and nothing is on its way:
// Core's "empty" and "this position is bare" agree, which is the case the
// downgrade was written for. It must still fire, and the log line that announces
// it must still be true when it does.
func TestConsumeDowngrade_StillDowngradesWhenTheNodeIsTrulyBare(t *testing.T) {
	t.Parallel()
	eng, _, nodeID := spokenForFixture(t, false)

	res, err := eng.requestNodeMaterialFor(nodeID, 1, protocol.EpisodeTriggerOperator)
	if err != nil {
		t.Fatalf("bare position refused: %v — the guard is blocking the case the downgrade exists for", err)
	}
	if res == nil || res.Order == nil {
		t.Fatalf("no simple delivery minted for a bare position: %+v", res)
	}
	if res.Order.OrderType != orders.TypeMove {
		t.Errorf("order type = %q, want a plain move (the downgrade)", res.Order.OrderType)
	}
	if res.Order.SourceNode != "SYN_MARKET" || res.Order.DeliveryNode != "ALN_004" {
		t.Errorf("downgrade endpoints = %q→%q, want SYN_MARKET→ALN_004",
			res.Order.SourceNode, res.Order.DeliveryNode)
	}
}

// TestConsumeDowngrade_LoaderWindowIsExempt pins the exemption. A manual_swap
// window runs a multi-order queue on purpose — CanAcceptOrders returns true for
// one while its own orders are in flight — so holding it to one bin at a time
// would stall the empties it exists to supply. The same exemption
// guardStyleTransition and guardCatidMismatch carry.
func TestConsumeDowngrade_LoaderWindowIsExempt(t *testing.T) {
	t.Parallel()
	eng, db, nodeID := spokenForFixture(t, false)
	seedInFlightSwap(t, db, nodeID)

	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "get node")
	runtime, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "get runtime")
	claim := findActiveClaim(db, node)
	if claim == nil {
		t.Fatal("no active claim")
	}
	claim.SwapMode = protocol.SwapModeManualSwap

	if err := eng.guardPositionSpokenFor(node, runtime, claim); err != nil {
		t.Errorf("loader window refused: %v — manual_swap must stay exempt", err)
	}
}
