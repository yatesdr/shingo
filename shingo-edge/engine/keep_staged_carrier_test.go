package engine

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/orders"
)

// AN EMPTY ON A PRODUCE SPOT IS JUDGED BY ITS CARRIER.
//
// The node-bins read every decision point makes says what stands on the spot,
// carrier type included. An empty is right when the claim's part may ride that
// carrier, by the catalog Core sends with the node list: the part's carriers,
// or any carrier when the part has none listed — Core's own rule at the
// pickup. These drive each decision point end to end against a Core stub.

const (
	ksTypeA = "TYPE-A"
	ksTypeB = "TYPE-B"
)

// ksCatalog is a catalog in which each part rides one carrier.
func ksCatalog(rides map[string]string) []protocol.PayloadBinTypeInfo {
	var out []protocol.PayloadBinTypeInfo
	for part, carrier := range rides {
		out = append(out, protocol.PayloadBinTypeInfo{PayloadCode: part, BinTypeCode: carrier})
	}
	return out
}

// An empty of a carrier the part does not ride is wrong at every decision point.
// A changeover's start and cancel, which change what the spot is kept for, send
// it back and refill the spot. A running line leaves it standing: the keeper
// orders nothing, and a request builds its market swap, which Core holds while
// the wrong empty occupies the staging spot.
func TestKeepStagedCarrier_AWrongCarrierAtEveryDecision(t *testing.T) {
	t.Parallel()
	wrong := NodeBinInfo{Occupied: true, BinTypeCode: ksTypeB}

	t.Run("request", func(t *testing.T) {
		t.Parallel()
		eng, db, nodeID, _ := keepStagedCell(t, protocol.ClaimRoleProduce, protocol.SwapModeTwoRobot,
			map[string]NodeBinInfo{ksLine: {Occupied: true}, ksSpot: wrong})
		eng.SetPayloadBinTypes(ksCatalog(map[string]string{ksPart: ksTypeA}))
		_, err := eng.RequestProduceSwap(nodeID)
		testutil.MustNoErr(t, err, "request")
		if got := swapFetchesFrom(t, eng, db, nodeID); got != "market" {
			t.Errorf("the swap fetches from %q, want the market: the wrong empty is not a spare", got)
		}
		if got := readSpotOrders(t, db, nodeID); got.returns != 0 || got.refills != 0 {
			t.Fatalf("returns=%d refills=%d from a request, want none: the keeper decides the spot",
				got.returns, got.refills)
		}
	})

	t.Run("level sweep", func(t *testing.T) {
		t.Parallel()
		eng, db, nodeID, _ := keepStagedCell(t, protocol.ClaimRoleProduce, protocol.SwapModeTwoRobot,
			map[string]NodeBinInfo{ksLine: {Occupied: true}, ksSpot: wrong})
		eng.SetPayloadBinTypes(ksCatalog(map[string]string{ksPart: ksTypeA}))
		holdingSwap(t, db, nodeID)
		eng.sweepCellLevels()
		if got := readSpotOrders(t, db, nodeID); got.returns != 0 || got.refills != 0 {
			t.Fatalf("returns=%d refills=%d, want none: a running line leaves the wrong empty standing",
				got.returns, got.refills)
		}
	})

	t.Run("changeover start", func(t *testing.T) {
		t.Parallel()
		// The line changes over and its part does not: the empty standing there
		// was always wrong for it.
		fx := seedKeepStagedChangeover(t,
			[]coClaim{{"L1", "SPOT", "SRC", "PART-A", protocol.ClaimRoleProduce, protocol.SwapModeTwoRobot, true, true}},
			[]coClaim{{"L1", "SPOT", "SRC", "PART-A", protocol.ClaimRoleProduce, protocol.SwapModeTwoRobot, true, false}},
			map[string]NodeBinInfo{"L1": {Occupied: true}, "SPOT": wrong})
		fx.eng.SetPayloadBinTypes(ksCatalog(map[string]string{"PART-A": ksTypeA}))
		startKSChangeover(t, fx)
		got := readSpotTraffic(t, fx, "SPOT")
		if len(got.returns) != 1 || got.returns[0].DeliveryNode != "SRC" || got.refills["L1"] != 2 {
			t.Fatalf("returns=%+v refills=%v, want the wrong empty back to SRC and 2 refills", got.returns, got.refills)
		}
	})

	t.Run("changeover cancel", func(t *testing.T) {
		t.Parallel()
		rows := map[string]NodeBinInfo{"L1": {Occupied: true}, "SPOT": {Occupied: true, BinTypeCode: ksTypeA}}
		fx := seedKeepStagedChangeover(t,
			[]coClaim{{"L1", "SPOT", "SRC-OLD", "PART-OLD", protocol.ClaimRoleProduce, protocol.SwapModeTwoRobot, true, false}},
			[]coClaim{{"L1", "SPOT", "SRC-NEW", "PART-NEW", protocol.ClaimRoleProduce, protocol.SwapModeTwoRobot, true, false}},
			rows)
		fx.eng.SetPayloadBinTypes(ksCatalog(map[string]string{"PART-OLD": ksTypeA, "PART-NEW": ksTypeA}))
		startKSChangeover(t, fx)
		// An empty of a carrier neither part rides reached the spot by hand.
		rows["SPOT"] = wrong
		fx.eng.coreClient = stubCoreClient(ksNodeBinsStub(t, rows).URL)
		testutil.MustNoErr(t, fx.eng.CancelProcessChangeover(fx.processID), "cancel")
		// It goes back where the leaving style's spares come from (returnTo).
		got := readSpotTraffic(t, fx, "SPOT")
		if len(got.returns) != 1 || got.returns[0].DeliveryNode != "SRC-NEW" || got.refills["L1"] != 1 {
			t.Fatalf("returns=%+v refills=%v, want the wrong empty back to SRC-NEW and 1 refill of PART-OLD",
				got.returns, got.refills)
		}
	})
}

// A CANCEL DECIDES THE SPOT THE SAME WHATEVER THE CLOCKS SAY. The changeover's
// start is stamped by the engine clock and an order row by the database's;
// in the sped-up sim they disagree. Here the incoming style's empty, of a
// carrier the staying part does not ride, has landed on the spot, and the
// start is stamped behind it, then ahead of it.
func TestKeepStagedCarrier_ACancelDecidesTheSameAtAnyClock(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		start time.Duration // the start's stamp relative to the landed refill's row
	}{
		{"engine clock behind wall time", -time.Minute},
		{"engine clock ahead of wall time", time.Hour},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			rows := map[string]NodeBinInfo{"L1": {Occupied: true}, "SPOT": {Occupied: true, BinTypeCode: ksTypeA}}
			fx := seedKeepStagedChangeover(t,
				[]coClaim{{"L1", "SPOT", "SRC-OLD", "PART-OLD", protocol.ClaimRoleProduce, protocol.SwapModeTwoRobot, true, false}},
				[]coClaim{{"L1", "SPOT", "SRC-NEW", "PART-NEW", protocol.ClaimRoleProduce, protocol.SwapModeTwoRobot, true, false}},
				rows)
			fx.eng.SetPayloadBinTypes(ksCatalog(map[string]string{"PART-OLD": ksTypeA, "PART-NEW": ksTypeB}))
			coID := startKSChangeover(t, fx)
			if got := readSpotTraffic(t, fx, "SPOT"); len(got.returns) != 1 || got.refills["L1"] != 2 {
				t.Fatalf("fixture: start made returns=%d refills=%v, want 1 and 2", len(got.returns), got.refills)
			}
			// The return went, and one PART-NEW refill landed: an empty of TYPE-B.
			landed := int64(0)
			live, err := fx.db.ListActiveOrders()
			testutil.MustNoErr(t, err, "orders")
			for _, o := range live {
				if o.OrderType == orders.TypeRetrieve && o.DeliveryNode == "SPOT" && landed == 0 {
					landed = o.ID
				}
			}
			stamp := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
			_, err = fx.db.DB.Exec(`UPDATE orders SET status=?, created_at=? WHERE id=?`,
				string(protocol.StatusConfirmed), stamp.Format(time.RFC3339), landed)
			testutil.MustNoErr(t, err, "the refill landed")
			_, err = fx.db.DB.Exec(`UPDATE process_changeovers SET started_at=? WHERE id=?`,
				stamp.Add(c.start).Format(time.RFC3339), coID)
			testutil.MustNoErr(t, err, "the start's stamp")
			rows["SPOT"] = NodeBinInfo{Occupied: true, BinTypeCode: ksTypeB}
			fx.eng.coreClient = stubCoreClient(ksNodeBinsStub(t, rows).URL)

			testutil.MustNoErr(t, fx.eng.CancelProcessChangeover(fx.processID), "cancel")

			got := readSpotTraffic(t, fx, "SPOT")
			if len(got.returns) != 1 || got.returns[0].DeliveryNode != "SRC-NEW" {
				t.Fatalf("returns = %+v, want the incoming style's empty back to SRC-NEW", got.returns)
			}
			old := 0
			for _, o := range mustActive(t, fx) {
				if o.OrderType == orders.TypeRetrieve && o.DeliveryNode == "SPOT" && o.PayloadCode == "PART-OLD" {
					old++
				}
			}
			if old != 1 {
				t.Fatalf("PART-OLD refills after the cancel = %d, want 1", old)
			}
		})
	}
}

// A PART CHANGE BETWEEN PARTS THAT RIDE THE SAME CARRIER KEEPS THE EMPTY. The
// empty standing on the spot serves the incoming part; nothing goes back, and
// the spot is refilled only for what the changeover's own leg lifts from it.
func TestKeepStagedCarrier_ASameCarrierPartChangeKeepsTheEmpty(t *testing.T) {
	t.Parallel()
	for _, mode := range []protocol.SwapMode{protocol.SwapModeTwoRobot, protocol.SwapModeSingleRobot,
		protocol.SwapModeSequential, protocol.SwapModeTwoRobotPressIndex} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			fx := seedKeepStagedChangeover(t,
				[]coClaim{{"L1", "SPOT", "SRC-OLD", "PART-OLD", protocol.ClaimRoleProduce, mode, true, false}},
				[]coClaim{{"L1", "SPOT", "SRC-NEW", "PART-NEW", protocol.ClaimRoleProduce, mode, true, false}},
				map[string]NodeBinInfo{"L1": {Occupied: true}, "SPOT": {Occupied: true, BinTypeCode: ksTypeA}})
			fx.eng.SetPayloadBinTypes(ksCatalog(map[string]string{"PART-OLD": ksTypeA, "PART-NEW": ksTypeA}))
			startKSChangeover(t, fx)
			got := readSpotTraffic(t, fx, "SPOT")
			lifts := 0
			if planLiftsSpotFor(t, fx) {
				lifts = 1
			}
			if len(got.returns) != 0 || got.refills["L1"] != lifts {
				t.Fatalf("returns=%d refills=%v, want none back and %d refill(s) for what the leg lifts",
					len(got.returns), got.refills, lifts)
			}
		})
	}
}

// planLiftsSpotFor reports whether the started changeover's own legs pick up
// at the spot: a live complex order of the line with a pickup there.
func planLiftsSpotFor(t *testing.T, fx *coFixture) bool {
	t.Helper()
	for _, o := range mustActive(t, fx) {
		if o.OrderType != orders.TypeComplex {
			continue
		}
		raw, err := fx.db.GetOrderStepsJSON(o.ID)
		testutil.MustNoErr(t, err, "steps")
		steps, err := decodeSteps(raw)
		testutil.MustNoErr(t, err, "decode steps")
		for _, s := range steps {
			if s.Action == protocol.ActionPickup && s.Node == "SPOT" {
				return true
			}
		}
	}
	return false
}

func mustActive(t *testing.T, fx *coFixture) []domain.Order {
	t.Helper()
	live, err := fx.db.ListActiveOrders()
	testutil.MustNoErr(t, err, "orders")
	return live
}
