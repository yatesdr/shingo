package engine

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
)

// A PRESS WHOSE LINE HOLDS A BIN AND WHOSE PAIRED POSITION DOES NOT, asked by
// every button of both roles. The answer is one decision (planBareLine): the
// bare position gets one plain delivery and no swap is built, a second press
// while that bin is on its way is held with a notice, and nothing is primed
// while a swap is still working the cell, because that swap's own legs are
// about to fill the position.

type pairedDoor struct {
	name string
	role protocol.ClaimRole
	door string
	uop  int
}

var pairedDoors = []pairedDoor{
	{"material request", protocol.ClaimRoleConsume, doorMaterial, 30},
	{"produce request", protocol.ClaimRoleProduce, doorProduce, 30},
	{"empty-bin request", protocol.ClaimRoleProduce, doorEmptyBin, 0},
}

func pressDoor(eng *Engine, nodeID int64, door string) error {
	var err error
	switch door {
	case doorMaterial:
		_, err = eng.RequestNodeMaterial(nodeID, 1)
	case doorProduce:
		_, err = eng.RequestProduceSwap(nodeID)
	case doorEmptyBin:
		_, err = eng.RequestEmptyBin(nodeID, ksPart)
	}
	return err
}

func TestPairedPrime_EveryButtonPrimesOnceThenHolds(t *testing.T) {
	t.Parallel()
	for _, d := range pairedDoors {
		t.Run(d.name, func(t *testing.T) {
			t.Parallel()
			eng, db, nodeID := seedCensusCell(t, d.role, protocol.SwapModeTwoRobotPressIndex, d.uop)
			var calls atomic.Int32
			eng.coreClient = stubCoreClient(censusStub(t, &calls, ksLine).URL)

			testutil.MustNoErr(t, pressDoor(eng, nodeID, d.door), "first press")
			rows, err := db.ListActiveOrdersByProcessNode(nodeID)
			testutil.MustNoErr(t, err, "rows")
			if len(rows) != 1 || rows[0].OrderType == orders.TypeComplex || rows[0].DeliveryNode != censusDeck {
				t.Fatalf("first press made %+v, want one plain delivery to %s and no swap", rows, censusDeck)
			}

			err = pressDoor(eng, nodeID, d.door)
			var held *PrimeInFlightError
			if !errors.As(err, &held) || !held.Advisory() {
				t.Fatalf("second press while the bin is on its way: %v, want the advisory hold", err)
			}
			rows, err = db.ListActiveOrdersByProcessNode(nodeID)
			testutil.MustNoErr(t, err, "rows")
			if len(rows) != 1 {
				t.Errorf("the line holds %d orders after the hold, want 1", len(rows))
			}
		})
	}
}

func TestPairedPrime_NothingWhileASwapWorksTheCell(t *testing.T) {
	t.Parallel()
	for _, d := range pairedDoors {
		t.Run(d.name, func(t *testing.T) {
			t.Parallel()
			eng, db, nodeID := seedCensusCell(t, d.role, protocol.SwapModeTwoRobotPressIndex, d.uop)
			var calls atomic.Int32
			eng.coreClient = stubCoreClient(censusStub(t, &calls, ksLine).URL)
			// A swap leg still working the cell, in the line's runtime slot: the
			// bin it lifted from the paired position is what made it bare.
			leg, err := eng.orderMgr.CreateMoveOrder(&nodeID, 1, ksMarket, ksDest, false, orders.Origin{})
			testutil.MustNoErr(t, err, "the working leg")
			testutil.MustNoErr(t, db.UpdateProcessNodeRuntimeOrders(nodeID, &leg.ID, nil), "runtime slot")

			err = pressDoor(eng, nodeID, d.door)
			if err == nil || !(strings.Contains(err.Error(), "a swap is already in progress") ||
				strings.Contains(err.Error(), "active order in progress")) {
				t.Errorf("press while a swap works the cell: %v, want it refused", err)
			}
			rows, err := db.ListActiveOrdersByProcessNode(nodeID)
			testutil.MustNoErr(t, err, "rows")
			for _, o := range rows {
				if o.ID != leg.ID {
					t.Errorf("made %d %s %s->%s while a swap works the cell", o.ID, o.OrderType, o.SourceNode, o.DeliveryNode)
				}
			}
		})
	}
}
