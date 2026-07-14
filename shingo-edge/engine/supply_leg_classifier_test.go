package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/store"
	storeorders "shingoedge/store/orders"
	"shingoedge/store/processes"
)

// seedSwapClaim builds a node + claim for a two-robot swap mode, with the paired
// index node set (press-index requires it).
func seedSwapClaim(t *testing.T, db *store.DB, swapMode protocol.SwapMode) (nodeID int64, node *processes.Node, claim *processes.NodeClaim) {
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
		StyleID:             styleID,
		CoreNodeName:        "PRESS",
		Role:                "produce",
		SwapMode:            swapMode,
		PayloadCode:         "WIDGET-A",
		UOPCapacity:         100,
		InboundSource:       "MARKET-EMPTIES",
		InboundStaging:      "IN-STAGING",
		OutboundStaging:     "OUT-STAGING",
		OutboundDestination: "MARKET",
		PairedCoreNode:      "INDEX",
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

// mkSwapLeg creates one leg of a swap pair with its steps, and links the siblings
// (isSupplyOrderInTwoRobotSwap requires a sibling pointer).
func mkSwapLeg(t *testing.T, db *store.DB, nodeID int64, uuid, stepsJSON, deliveryNode string) *storeorders.Order {
	t.Helper()
	id, err := db.CreateOrder(uuid, orders.TypeComplex, &nodeID, false, 1, deliveryNode, "", "", "", false, "WIDGET-A")
	testutil.MustNoErr(t, err, "create order "+uuid)
	testutil.MustNoErr(t, db.UpdateOrderStepsJSON(id, stepsJSON), "steps "+uuid)
	o, err := db.GetOrder(id)
	testutil.MustNoErr(t, err, "get order "+uuid)
	return o
}

// TestIsSupplyOrderInTwoRobotSwap_ClassifiesByFinalDropoff pins the supply/evac
// classifier across BOTH two-robot modes. It was previously decided by
// order.DeliveryNode, which is wrong in both directions for press-index:
//
//   - R1 is the EVAC (it lifts the spent bin off the press) but stored the press
//     as its delivery node, so it was misread as SUPPLY;
//   - R2 is the real SUPPLY (it sets a bin on the press) but is auto-confirmed,
//     and dispatchComplexLeg blanks delivery_node for auto-confirm legs, so it
//     could never be recognised.
//
// The leg's steps say where it ends. Ask them.
func TestIsSupplyOrderInTwoRobotSwap_ClassifiesByFinalDropoff(t *testing.T) {
	cases := []struct {
		name string
		mode protocol.SwapMode
		// steps for each leg, and the (wrong or blank) delivery_node each really stores
		legSteps        string
		legDeliveryNode string
		wantSupply      bool
		why             string
	}{
		{
			name:            "two_robot supply leg (A) ends AT the press",
			mode:            protocol.SwapModeTwoRobot,
			legSteps:        `[{"action":"pickup","node":"MARKET-EMPTIES"},{"action":"dropoff","node":"IN-STAGING"},{"action":"wait","node":"IN-STAGING"},{"action":"pickup","node":"IN-STAGING"},{"action":"dropoff","node":"PRESS"}]`,
			legDeliveryNode: "PRESS",
			wantSupply:      true,
			why:             "orderA delivers the fresh bin to the press — it IS the supply leg",
		},
		{
			name:            "two_robot evac leg (B) ends at the market",
			mode:            protocol.SwapModeTwoRobot,
			legSteps:        `[{"action":"wait","node":"PRESS"},{"action":"pickup","node":"PRESS"},{"action":"dropoff","node":"MARKET"}]`,
			legDeliveryNode: "",
			wantSupply:      false,
			why:             "orderB carries the spent bin away — evac",
		},
		{
			name:            "press-index R1 ends at the INDEX node, not the press",
			mode:            protocol.SwapModeTwoRobotPressIndex,
			legSteps:        `[{"action":"wait","node":"PRESS"},{"action":"pickup","node":"PRESS"},{"action":"dropoff","node":"MARKET"},{"action":"pickup","node":"MARKET-EMPTIES"},{"action":"dropoff","node":"INDEX"}]`,
			legDeliveryNode: "PRESS", // the lie: DeliveryNodeA used to be the process node
			wantSupply:      false,
			why:             "R1 lifts the spent bin OFF the press and stages an empty at the index — it is the EVAC, despite delivery_node naming the press",
		},
		{
			name:            "press-index R2 ends AT the press",
			mode:            protocol.SwapModeTwoRobotPressIndex,
			legSteps:        `[{"action":"wait","node":"INDEX"},{"action":"pickup","node":"INDEX"},{"action":"dropoff","node":"PRESS"}]`,
			legDeliveryNode: "", // auto-confirm blanks it, so DeliveryNode could never identify this leg
			wantSupply:      true,
			why:             "R2 sets a bin ON the press — it IS the supply leg, and delivery_node can never say so because auto-confirm blanks it",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := testEngineDB(t)
			nodeID, node, claim := seedSwapClaim(t, db, tc.mode)
			eng := testEngine(t, db)

			leg := mkSwapLeg(t, db, nodeID, "uuid-leg", tc.legSteps, tc.legDeliveryNode)
			sibling := mkSwapLeg(t, db, nodeID, "uuid-sibling", `[{"action":"dropoff","node":"NOWHERE"}]`, "")
			testutil.MustNoErr(t, db.LinkOrderSiblings(leg.ID, sibling.ID), "link siblings")
			leg, _ = db.GetOrder(leg.ID)

			got := eng.isSupplyOrderInTwoRobotSwap(leg, node, claim)
			if got != tc.wantSupply {
				t.Errorf("isSupply = %v, want %v — %s", got, tc.wantSupply, tc.why)
			}
		})
	}
}

// A leg with no sibling is not part of a swap pair and must never classify as supply.
func TestIsSupplyOrderInTwoRobotSwap_RequiresSibling(t *testing.T) {
	db := testEngineDB(t)
	nodeID, node, claim := seedSwapClaim(t, db, protocol.SwapModeTwoRobotPressIndex)
	eng := testEngine(t, db)

	lone := mkSwapLeg(t, db, nodeID, "uuid-lone",
		`[{"action":"pickup","node":"INDEX"},{"action":"dropoff","node":"PRESS"}]`, "PRESS")
	if eng.isSupplyOrderInTwoRobotSwap(lone, node, claim) {
		t.Error("a leg with no sibling classified as supply — the swap pair is the precondition")
	}
}
