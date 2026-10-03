package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/orders"
)

// A REFILL THAT LANDS ON A SPOT NOBODY KEEPS IT FOR GOES STRAIGHT BACK.
//
// A refill already with the fleet cannot be cancelled. A changeover cancelled
// while the incoming style's refill flies leaves that bin landing on a spot the
// staying style keeps, after the cancel has decided the spot (sim F3). On a
// produce spot nothing else can see it. The completion kick judges the landing
// against the claim the spot is kept for at that moment.

// withCascade gives the fixture the production order emitter, so a status
// change runs the completion cascade (and its kick) as it does in the plant.
func withCascade(fx *coFixture) {
	fx.eng.orderMgr = orders.NewManager(fx.db, &orderEmitter{bus: fx.eng.Events}, "test.station")
}

// land confirms an order as Core's confirm does.
func land(t *testing.T, fx *coFixture, id int64) {
	t.Helper()
	o, err := fx.db.GetOrder(id)
	testutil.MustNoErr(t, err, "order")
	testutil.MustNoErr(t, fx.eng.orderMgr.ApplyCoreStatus(o, protocol.StatusConfirmed, "landed"), "confirm")
}

// liveSpot is the live (non-terminal) refills to and returns from a spot.
func liveSpot(t *testing.T, fx *coFixture, spot string) (refills, returns []domain.Order) {
	t.Helper()
	rows, err := fx.db.ListActiveOrders()
	testutil.MustNoErr(t, err, "orders")
	for _, o := range rows {
		if protocol.IsTerminal(o.Status) {
			continue
		}
		switch {
		case isRetrieve(o.OrderType) && o.DeliveryNode == spot:
			refills = append(refills, o)
		case o.OrderType == protocol.OrderTypeMove && o.SourceNode == spot && o.DeliveryNode != "L1":
			returns = append(returns, o)
		}
	}
	return refills, returns
}

func seedLanding(t *testing.T, role protocol.ClaimRole, rows map[string]NodeBinInfo) *coFixture {
	t.Helper()
	fx := seedKeepStagedChangeover(t,
		[]coClaim{{"L1", "SPOT", "SRC-OLD", "PART-OLD", role, protocol.SwapModeTwoRobot, true, false}},
		[]coClaim{{"L1", "SPOT", "SRC-NEW", "PART-NEW", role, protocol.SwapModeTwoRobot, true, false}},
		rows)
	withCascade(fx)
	return fx
}

func lineRows(role protocol.ClaimRole) map[string]NodeBinInfo {
	line := NodeBinInfo{Occupied: true, PayloadCode: "PART-OLD"}
	if role == protocol.ClaimRoleProduce {
		line = NodeBinInfo{Occupied: true}
	}
	return map[string]NodeBinInfo{"L1": line, "SPOT": {}}
}

var bothRoles = []protocol.ClaimRole{protocol.ClaimRoleConsume, protocol.ClaimRoleProduce}

// (a) A changeover is armed and the incoming style's refill lands before
// cutover: it is what the spot is being kept for, and it stays.
func TestKeepStagedLanding_IncomingRefillBeforeCutoverStays(t *testing.T) {
	t.Parallel()
	for _, role := range bothRoles {
		t.Run(string(role), func(t *testing.T) {
			t.Parallel()
			fx := seedLanding(t, role, lineRows(role))
			startKSChangeover(t, fx)
			refills, _ := liveSpot(t, fx, "SPOT")
			if len(refills) == 0 {
				t.Fatal("fixture: the start ordered no refill for the incoming style")
			}
			land(t, fx, refills[0].ID)
			if _, returns := liveSpot(t, fx, "SPOT"); len(returns) != 0 {
				t.Fatalf("the incoming style's refill landing before cutover was sent back: %+v", returns)
			}
		})
	}
}

// (b) The changeover is cancelled with the incoming style's refill already
// flying; it lands after the cancel decided the spot, and goes back to its own
// source, carrying its part, an empty untagged.
func TestKeepStagedLanding_FlownIncomingRefillAfterCancelGoesBack(t *testing.T) {
	t.Parallel()
	for _, role := range bothRoles {
		t.Run(string(role), func(t *testing.T) {
			t.Parallel()
			fx := seedLanding(t, role, lineRows(role))
			startKSChangeover(t, fx)
			markSpotOrdersFlown(t, fx, "SPOT", 1)
			testutil.MustNoErr(t, fx.eng.CancelProcessChangeover(fx.processID), "cancel")
			var flown *domain.Order
			refills, _ := liveSpot(t, fx, "SPOT")
			for i := range refills {
				if refills[i].PayloadCode == "PART-NEW" {
					flown = &refills[i]
				}
			}
			if flown == nil {
				t.Fatal("fixture: no incoming-style refill survived the cancel in flight")
			}

			land(t, fx, flown.ID)

			_, returns := liveSpot(t, fx, "SPOT")
			if len(returns) != 1 || returns[0].DeliveryNode != "SRC-NEW" {
				t.Fatalf("returns after the landing = %+v, want one to the refill's source SRC-NEW", returns)
			}
			want := "PART-NEW"
			if role == protocol.ClaimRoleProduce {
				want = ""
			}
			if returns[0].PayloadCode != want {
				t.Errorf("the return carries %q, want %q", returns[0].PayloadCode, want)
			}
		})
	}
}

// One return per bin: the landing sends it back, and a REQUEST in the next
// moment, which Core answers with the same wrong bin still on the spot, counts
// that return as the spare leaving instead of sending it back again. And the
// return's own completion orders nothing (the kick fires only on a refill).
func TestKeepStagedLanding_OneReturnPerBin_AndAReturnOrdersNothing(t *testing.T) {
	t.Parallel()
	rows := lineRows(protocol.ClaimRoleConsume)
	fx := seedLanding(t, protocol.ClaimRoleConsume, rows)
	startKSChangeover(t, fx)
	markSpotOrdersFlown(t, fx, "SPOT", 1)
	testutil.MustNoErr(t, fx.eng.CancelProcessChangeover(fx.processID), "cancel")
	refills, _ := liveSpot(t, fx, "SPOT")
	for _, r := range refills {
		if r.PayloadCode == "PART-NEW" {
			land(t, fx, r.ID)
		}
	}
	rows["SPOT"] = NodeBinInfo{Occupied: true, PayloadCode: "PART-NEW"}
	fx.eng.coreClient = NewCoreClient(ksNodeBinsStub(t, rows).URL)

	_, err := fx.eng.RequestNodeMaterial(fx.nodeIDs["L1"], 1)
	testutil.MustNoErr(t, err, "REQUEST")
	_, returns := liveSpot(t, fx, "SPOT")
	if len(returns) != 1 {
		t.Fatalf("returns = %d after the landing and a REQUEST, want 1", len(returns))
	}

	before, err := fx.db.ListActiveOrdersByProcessNode(fx.nodeIDs["L1"])
	testutil.MustNoErr(t, err, "rows")
	land(t, fx, returns[0].ID)
	after, err := fx.db.ListActiveOrdersByProcessNode(fx.nodeIDs["L1"])
	testutil.MustNoErr(t, err, "rows")
	if len(after) != len(before)-1 {
		t.Fatalf("live orders %d -> %d when the return completed, want one fewer and nothing new", len(before), len(after))
	}
}

// F5: a cancel with the staying style's spare already leaving on a flown return
// decides the spot as bare: no second return, and a refill of the staying part
// to stand behind it.
func TestKeepStagedChangeover_CancelCountsAReturnAlreadyFlying(t *testing.T) {
	t.Parallel()
	rows := map[string]NodeBinInfo{"L1": {Occupied: true, PayloadCode: "PART-OLD"},
		"SPOT": {Occupied: true, PayloadCode: "PART-OLD"}}
	fx := seedLanding(t, protocol.ClaimRoleConsume, rows)
	startKSChangeover(t, fx)
	_, returns := liveSpot(t, fx, "SPOT")
	if len(returns) != 1 {
		t.Fatalf("fixture: the start made %d returns, want 1", len(returns))
	}
	testutil.MustNoErr(t, fx.db.UpdateOrderStatus(returns[0].ID, string(protocol.StatusInTransit)), "the return flies")

	testutil.MustNoErr(t, fx.eng.CancelProcessChangeover(fx.processID), "cancel")

	refills, returns := liveSpot(t, fx, "SPOT")
	if len(returns) != 1 {
		t.Errorf("returns after the cancel = %d, want only the one already flying", len(returns))
	}
	old := 0
	for _, r := range refills {
		if r.PayloadCode == "PART-OLD" {
			old++
		}
	}
	if old != 1 {
		t.Fatalf("PART-OLD refills after the cancel = %d, want 1 to stand behind the spare that is leaving", old)
	}
}

// A produce changeover cancelled before the outgoing empty left the spot: no
// incoming refill has landed, so the empty standing there is the staying
// style's own. The cancel keeps it; judged by the claims' config, it was sent to
// the incoming style's source.
func TestKeepStagedChangeover_CancelKeepsTheStayingStylesEmpty(t *testing.T) {
	t.Parallel()
	fx := seedLanding(t, protocol.ClaimRoleProduce,
		map[string]NodeBinInfo{"L1": {Occupied: true}, "SPOT": {Occupied: true}})
	startKSChangeover(t, fx)
	if _, returns := liveSpot(t, fx, "SPOT"); len(returns) != 1 {
		t.Fatalf("fixture: the start made %d returns of the outgoing empty, want 1", len(returns))
	}

	testutil.MustNoErr(t, fx.eng.CancelProcessChangeover(fx.processID), "cancel")

	refills, returns := liveSpot(t, fx, "SPOT")
	if len(returns) != 0 {
		t.Fatalf("the cancel sent the staying style's own empty back: %+v", returns)
	}
	if len(refills) != 0 {
		t.Fatalf("the cancel ordered %d refill(s) with the staying style's empty still standing", len(refills))
	}
}
