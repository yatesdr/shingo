//go:build docker

package engine

import (
	"fmt"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/fleet"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// slot_chooser_drift_docker_test.go — the engine's half of the drift guard in
// dispatch/slot_chooser_drift_docker_test.go: no chooser offers a slot its own
// claim door refuses. The chooser here, chooseDeclared (the carried-bin
// return's: the cancelled order's automatic return and the bins page's Return
// button), picks a destination before an order exists, in a claim's source
// group, stored into through storeInto, and hands it to an order that claims
// through the dispatcher's ReserveStorageDropoff (claimStoreSlot for a
// storage-classed node). Stage 2's window list is dispatch's now, and its row
// is in dispatch's half.
//
// The candidate is a lane slot, so the claim door is real. The "free" row is
// the control.

type engDriftFixture struct {
	prefix    string
	candidate *nodes.Node
	lane      *nodes.Node // nil when the candidate is not a lane slot
	payload   string
}

type engDriftHold struct {
	name     string
	laneOnly bool
	apply    func(t *testing.T, db *store.DB, fx *engDriftFixture)
}

func engDriftHolder(t *testing.T, db *store.DB, fx *engDriftFixture, mutate ...func(*orders.Order)) *orders.Order {
	t.Helper()
	return testdb.CreateOrder(t, db, append([]func(*orders.Order){func(o *orders.Order) {
		o.EdgeUUID = fx.prefix + "-HOLDER"
		o.OrderType = dispatch.OrderTypeComplex
		o.Status = protocol.StatusPending
	}}, mutate...)...)
}

// engDriftCommitted builds a holder at `dispatched` (C1.5): a hard hold (a slot
// claim, a confirmed row, occupancy) belongs to a committed robot, and a
// pre-dispatch order holds nothing hard.
func engDriftCommitted(o *orders.Order) { o.Status = protocol.StatusDispatched }

var engDriftHolds = []engDriftHold{
	{name: "free"},
	{name: "bin-present", apply: func(t *testing.T, db *store.DB, fx *engDriftFixture) {
		createTestBinAtNode(t, db, fx.payload, fx.candidate.ID, fx.prefix+"-OCCUPANT")
	}},
	{name: "stranger-hard-claim", apply: func(t *testing.T, db *store.DB, fx *engDriftFixture) {
		h := engDriftHolder(t, db, fx, engDriftCommitted)
		testutil.MustNoErr(t, db.ReserveSlot(fx.candidate.ID, h.ID), "reserve")
		testutil.MustNoErr(t, db.ConfirmSlotClaim(fx.candidate.ID, h.ID, nil), "confirm the hard claim")
	}},
	{name: "stranger-pending-reservation", apply: func(t *testing.T, db *store.DB, fx *engDriftFixture) {
		h := engDriftHolder(t, db, fx)
		testutil.MustNoErr(t, db.ReserveSlot(fx.candidate.ID, h.ID), "reserve")
	}},
	{name: "stranger-confirmed-reservation", apply: func(t *testing.T, db *store.DB, fx *engDriftFixture) {
		h := engDriftHolder(t, db, fx, engDriftCommitted)
		testutil.MustNoErr(t, db.ReserveSlot(fx.candidate.ID, h.ID), "reserve")
		testutil.MustNoErr(t, db.ConfirmSlotReservation(fx.candidate.ID, h.ID), "confirm the reservation")
	}},
	{name: "live-delivery-node", apply: func(t *testing.T, db *store.DB, fx *engDriftFixture) {
		engDriftHolder(t, db, fx, func(o *orders.Order) {
			o.OrderType = protocol.OrderTypeStore
			o.Status = protocol.StatusQueued
			o.DeliveryNode = fx.candidate.Name
		})
	}},
	{name: "occupancy-row", laneOnly: true, apply: func(t *testing.T, db *store.DB, fx *engDriftFixture) {
		h := engDriftHolder(t, db, fx, engDriftCommitted)
		ok, err := reservations.AcquireOccupancy(db.DB, h.ID, fx.lane.ID)
		testutil.MustNoErr(t, err, "take occupancy")
		if !ok {
			t.Fatal("the holder could not take occupancy of the candidate's lane")
		}
	}},
}

type engDriftChooser struct {
	name   string
	inLane bool
	// build makes the candidate and returns a function that asks the chooser.
	build func(t *testing.T, db *store.DB, eng *Engine, fx *engDriftFixture) func() *nodes.Node
}

var engDriftChoosers = []engDriftChooser{
	{
		name:   "carried-bin-return",
		inLane: true,
		build: func(t *testing.T, db *store.DB, eng *Engine, fx *engDriftFixture) func() *nodes.Node {
			// A claim sources the bin's part from a group of one lane, one slot
			// deep: the only place the chooser can answer is the candidate.
			grp, lanes, slots := laneGroup(t, db, fx.prefix, 1, 1)
			fx.lane, fx.candidate = lanes[0], slots[0][0]
			seedClaim(t, db, "PROC-"+fx.prefix, "STY", "LINE-"+fx.prefix, fx.payload, grp.Name, true)
			bin, carrier := seedCancelledCarry(t, db, "AMR-"+fx.prefix, fx.payload, "LINE-"+fx.prefix)
			return func() *nodes.Node {
				dest, _, err := eng.chooseDeclared(bin, fleet.RobotStatus{}, carrier)
				if err != nil {
					return nil // the chooser held the bin: it offered nothing
				}
				return dest
			}
		},
	},
}

// TestEngineSlotChoosers_NeverOfferWhatTheClaimDoorRefuses is the engine half
// of the drift guard.
//
// MUTATION: drop the reservation clause from nodes.SlotTakeableSQL, which the
// carried-bin return reaches through the store selector. The chooser's
// pending- and confirmed-reservation rows fail.
func TestEngineSlotChoosers_NeverOfferWhatTheClaimDoorRefuses(t *testing.T) {
	db := testDB(t)
	_, _, p := setupTestData(t, db)
	// Started, because the claim door is the dispatcher's and Start builds it.
	eng := newTestEngine(t, db, simulator.New())

	n := 0
	for _, ch := range engDriftChoosers {
		for _, hold := range engDriftHolds {
			if hold.laneOnly && !ch.inLane {
				continue
			}
			n++
			prefix := fmt.Sprintf("EDR%d", n)
			t.Run(ch.name+"/"+hold.name, func(t *testing.T) {
				fx := &engDriftFixture{prefix: prefix, payload: p.Code}
				choose := ch.build(t, db, eng, fx)
				if hold.apply != nil {
					hold.apply(t, db, fx)
				}
				got := choose()
				if hold.name == "free" && (got == nil || got.ID != fx.candidate.ID) {
					t.Fatalf("with nothing held, %s did not offer %s (got %v): the fixture does not reach "+
						"the candidate, so the hold rows would prove nothing", ch.name, fx.candidate.Name, got)
				}
				if got == nil {
					return
				}
				claimant := testdb.CreateOrder(t, db, func(o *orders.Order) {
					o.EdgeUUID = prefix + "-CLAIMANT"
					o.OrderType = dispatch.OrderTypeMove
					o.Status = protocol.StatusPending
					o.DeliveryNode = got.Name
				})
				if dest := eng.dispatcher.ReserveStorageDropoff(claimant); dest.Refused() {
					t.Errorf("%s offered %s under a %s, and the claim door refused it (%s): %v",
						ch.name, got.Name, hold.name, dest.Cause, dest.Err)
				}
			})
		}
	}
}
