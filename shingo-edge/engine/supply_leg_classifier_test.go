package engine

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/store"
	storeorders "shingoedge/store/orders"
	"shingoedge/store/processes"
)

// seedSwapClaim builds a node + claim for a two-robot swap mode. secondPaired
// selects the press-index layout: "" is 2-position, a node name is 3-position.
func seedSwapClaim(t *testing.T, db *store.DB, swapMode protocol.SwapMode, secondPaired string) (nodeID int64, node *processes.Node, claim *processes.NodeClaim) {
	t.Helper()
	processID, err := db.CreateProcess("SUP-PROC", "", "active_production", "", "", false, false)
	testutil.MustNoErr(t, err, "create process")
	nodeID, err = db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "PRESS", Code: "sup-press", Name: "PRESS", Enabled: true,
	})
	testutil.MustNoErr(t, err, "create node")
	styleID, err := db.CreateStyle("SUP-STYLE", "", processID)
	testutil.MustNoErr(t, err, "create style")
	testutil.MustNoErr(t, db.SetActiveStyle(processID, &styleID), "set active style")
	_, err = db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID:              styleID,
		CoreNodeName:         "PRESS",
		Role:                 "produce",
		SwapMode:             swapMode,
		PayloadCode:          "WIDGET-A",
		UOPCapacity:          100,
		InboundSource:        "MARKET-EMPTIES",
		InboundStaging:       "IN-STAGING",
		OutboundStaging:      "OUT-STAGING",
		OutboundDestination:  "MARKET",
		PairedCoreNode:       "INDEX-B",
		SecondPairedCoreNode: secondPaired,
	})
	testutil.MustNoErr(t, err, "upsert claim")
	node, err = db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "get node")
	claim = findActiveClaim(db, node)
	if claim == nil {
		t.Fatal("claim not found — seed contract changed")
	}
	return nodeID, node, claim
}

// mkSwapLeg creates one leg of a swap pair with its steps.
func mkSwapLeg(t *testing.T, db *store.DB, nodeID int64, uuid string, steps []protocol.ComplexOrderStep, deliveryNode string) *storeorders.Order {
	t.Helper()
	stepsJSON, err := json.Marshal(steps)
	testutil.MustNoErr(t, err, "marshal steps "+uuid)
	id, err := db.CreateOrder(uuid, orders.TypeComplex, &nodeID, false, 1, deliveryNode, "", "", "", false, "WIDGET-A")
	testutil.MustNoErr(t, err, "create order "+uuid)
	testutil.MustNoErr(t, db.UpdateOrderStepsJSON(id, string(stepsJSON)), "steps "+uuid)
	o, err := db.GetOrder(id)
	testutil.MustNoErr(t, err, "get order "+uuid)
	return o
}

// TestIsSupplyOrderInTwoRobotSwap_ClassifiesByPlacedBin pins the supply/evac
// classifier across both two-robot modes AND both press-index layouts.
//
// The steps come from the REAL builders via BuildSwapDispatch — not from
// hand-written JSON. The previous version of this table hand-wrote its fixtures
// and only ever encoded the 2-position R2 shape, so it asserted "press-index R2
// is protected" and was green while the 3-position R2 shipped unprotected. A
// fixture that restates what the builder produces can only ever pin what its
// author already believed.
//
// The 3-position R2 row is the one that matters: it drops a bin on the press
// MID-sequence and then carries on to re-index the next position, so its final
// dropoff is the index node while the bin it supplied is still sitting on the
// press. "Where does the leg end?" gets it wrong; legPlacesBinAt gets it right.
//
// Each leg is stored with the delivery_node production would give it (leg A's
// derived value, blanked when auto-confirmed; leg B's always blank) — proving
// the classifier no longer depends on that column in either direction.
func TestIsSupplyOrderInTwoRobotSwap_ClassifiesByPlacedBin(t *testing.T) {
	cases := []struct {
		name         string
		mode         protocol.SwapMode
		secondPaired string // "" = 2-position press-index
		legB         bool   // false = leg A (two_robot supply / press-index R1)
		wantSupply   bool
		why          string
	}{
		{
			name:       "two_robot A delivers the fresh bin to the press",
			mode:       protocol.SwapModeTwoRobot,
			wantSupply: true,
			why:        "A ends dropoff(PRESS) and never picks it back up — it IS the supply leg",
		},
		{
			name:       "two_robot B carries the spent bin away",
			mode:       protocol.SwapModeTwoRobot,
			legB:       true,
			wantSupply: false,
			why:        "B picks up FROM the press and drops at the market — evac",
		},
		{
			name:       "press-index 2-pos R1 clears the press and stages an empty at the index",
			mode:       protocol.SwapModeTwoRobotPressIndex,
			wantSupply: false,
			why:        "R1 lifts the spent bin OFF the press — it is the EVAC, whatever delivery_node says",
		},
		{
			name:       "press-index 2-pos R2 sets a bin on the press",
			mode:       protocol.SwapModeTwoRobotPressIndex,
			legB:       true,
			wantSupply: true,
			why:        "R2 indexes the staged bin onto the press — it IS the supply leg",
		},
		{
			name:         "press-index 3-pos R1 clears the press and stages an empty at C",
			mode:         protocol.SwapModeTwoRobotPressIndex,
			secondPaired: "INDEX-C",
			wantSupply:   false,
			why:          "R1 still lifts the spent bin OFF the press — evac",
		},
		{
			name:         "press-index 3-pos R2 sets a bin on the press, then re-indexes C into B",
			mode:         protocol.SwapModeTwoRobotPressIndex,
			secondPaired: "INDEX-C",
			legB:         true,
			wantSupply:   true,
			why:          "R2's dropoff(PRESS) has no later pickup(PRESS) — the bin stays. Its FINAL dropoff is the index node, which is why a where-does-it-end test called this an evac and let Core wipe the manifest of the bin it just supplied",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := testEngineDB(t)
			nodeID, node, claim := seedSwapClaim(t, db, tc.mode, tc.secondPaired)
			eng := testEngine(t, db)

			// The steps a real dispatch would issue, for this exact claim.
			disp, err := BuildSwapDispatch(node, claim)
			testutil.MustNoErr(t, err, "build swap dispatch")

			steps, deliveryNode := disp.StepsA, disp.DeliveryNodeA
			if disp.AutoConfirmA {
				deliveryNode = "" // dispatchComplexLeg blanks it for auto-confirm legs
			}
			if tc.legB {
				// Leg B's delivery_node is blank at every call site AND blanked
				// again by AutoConfirmB — no value can ever identify it.
				steps, deliveryNode = disp.StepsB, ""
			}
			if len(steps) == 0 {
				t.Fatal("builder produced no steps — claim seed is missing a required field")
			}

			leg := mkSwapLeg(t, db, nodeID, "uuid-leg", steps, deliveryNode)
			sibling := mkSwapLeg(t, db, nodeID, "uuid-sibling",
				[]protocol.ComplexOrderStep{{Action: protocol.ActionDropoff, Node: "NOWHERE"}}, "")
			testutil.MustNoErr(t, db.LinkOrderSiblings(leg.ID, sibling.ID), "link siblings")
			leg, _ = db.GetOrder(leg.ID)

			got, err := eng.isSupplyOrderInTwoRobotSwap(leg, node, claim)
			testutil.MustNoErr(t, err, "classify leg")
			if got != tc.wantSupply {
				t.Errorf("isSupply = %v, want %v — %s\nsteps: %v", got, tc.wantSupply, tc.why, steps)
			}
		})
	}
}

// A leg with no sibling is not part of a swap pair and must never classify as supply.
func TestIsSupplyOrderInTwoRobotSwap_RequiresSibling(t *testing.T) {
	db := testEngineDB(t)
	nodeID, node, claim := seedSwapClaim(t, db, protocol.SwapModeTwoRobotPressIndex, "")
	eng := testEngine(t, db)

	lone := mkSwapLeg(t, db, nodeID, "uuid-lone", []protocol.ComplexOrderStep{
		{Action: protocol.ActionPickup, Node: "INDEX-B"},
		{Action: protocol.ActionDropoff, Node: "PRESS"},
	}, "PRESS")
	got, err := eng.isSupplyOrderInTwoRobotSwap(lone, node, claim)
	testutil.MustNoErr(t, err, "classify lone leg")
	if got {
		t.Error("a leg with no sibling classified as supply — the swap pair is the precondition")
	}
}

// An unreadable leg is REFUSED, not guessed. Returning false here would mean
// "evac", which is the branch that clears the bin's manifest — the ALN_002 wipe.
func TestIsSupplyOrderInTwoRobotSwap_UnreadableStepsError(t *testing.T) {
	db := testEngineDB(t)
	nodeID, node, claim := seedSwapClaim(t, db, protocol.SwapModeTwoRobotPressIndex, "")
	eng := testEngine(t, db)

	leg := mkSwapLeg(t, db, nodeID, "uuid-broken", []protocol.ComplexOrderStep{
		{Action: protocol.ActionDropoff, Node: "PRESS"},
	}, "")
	sibling := mkSwapLeg(t, db, nodeID, "uuid-broken-sib", []protocol.ComplexOrderStep{
		{Action: protocol.ActionDropoff, Node: "NOWHERE"},
	}, "")
	testutil.MustNoErr(t, db.LinkOrderSiblings(leg.ID, sibling.ID), "link siblings")
	testutil.MustNoErr(t, db.UpdateOrderStepsJSON(leg.ID, `{"not":"a step list"}`), "corrupt steps")
	leg, _ = db.GetOrder(leg.ID)

	if _, err := eng.isSupplyOrderInTwoRobotSwap(leg, node, claim); err == nil {
		t.Error("undecodable steps classified silently — an unclassifiable leg must refuse, not guess evac")
	}
}

// legPlacesBinAt is a pure step-shape predicate; pin its definition directly so
// the "no LATER pickup" half can't be quietly dropped.
func TestLegPlacesBinAt(t *testing.T) {
	cases := []struct {
		name  string
		steps []protocol.ComplexOrderStep
		want  bool
	}{
		{
			name: "dropoff at node, nothing after",
			steps: []protocol.ComplexOrderStep{
				{Action: protocol.ActionPickup, Node: "SRC"},
				{Action: protocol.ActionDropoff, Node: "PRESS"},
			},
			want: true,
		},
		{
			name: "dropoff at node, then picked back up",
			steps: []protocol.ComplexOrderStep{
				{Action: protocol.ActionDropoff, Node: "PRESS"},
				{Action: protocol.ActionPickup, Node: "PRESS"},
			},
			want: false,
		},
		{
			name: "picked up first, then a fresh bin dropped — the single-robot shape",
			steps: []protocol.ComplexOrderStep{
				{Action: protocol.ActionPickup, Node: "PRESS"},
				{Action: protocol.ActionDropoff, Node: "OUT-STAGING"},
				{Action: protocol.ActionDropoff, Node: "PRESS"},
			},
			want: true,
		},
		{
			name: "only ever picks up from the node",
			steps: []protocol.ComplexOrderStep{
				{Action: protocol.ActionWait, Node: "PRESS"},
				{Action: protocol.ActionPickup, Node: "PRESS"},
				{Action: protocol.ActionDropoff, Node: "MARKET"},
			},
			want: false,
		},
		{
			name: "never touches the node",
			steps: []protocol.ComplexOrderStep{
				{Action: protocol.ActionPickup, Node: "A"},
				{Action: protocol.ActionDropoff, Node: "B"},
			},
			want: false,
		},
		{
			name:  "no steps",
			steps: nil,
			want:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := legPlacesBinAt(tc.steps, "PRESS"); got != tc.want {
				t.Errorf("legPlacesBinAt = %v, want %v", got, tc.want)
			}
		})
	}
}
