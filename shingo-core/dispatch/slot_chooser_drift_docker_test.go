//go:build docker

package dispatch

import (
	"fmt"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch/binresolver"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/payloads"
	"shingocore/store/reservations"
)

// slot_chooser_drift_docker_test.go — no chooser offers a slot its own claim
// door refuses.
//
// Every function that picks a destination slot before claiming it has to agree
// with the claim about which slots are taken. When one does not, the refused
// slot is offered again on the next ask, and a deterministic chooser offers it
// for as long as the holder holds it — the shape of F6 (scripted sim
// 2026-10-03), where the dig's shuffle walk offered a reserve-holding swap's
// slot to nine dwellers for up to twelve minutes.
//
// The claim door is claimStoreSlot: no bin on the slot, and an exclusive slot
// reservation (ReserveSlot) the claimant can take. A chooser whose callers
// claim through ReserveSlot alone (the complex reserve) answers to the same
// test, since claimStoreSlot is ReserveSlot plus the bin check.
//
// The fixture puts ONE candidate under ONE kind of hold, and asks each chooser.
// Whatever a chooser returns must be claimable by the order that would claim
// it. The "free" row is the control: with nothing held, every chooser must
// return the candidate, or the rest of its rows prove nothing.
//
// Choosers in the engine package (the carried-bin return's chooser and the
// stage-2 pull's windows) cannot be called from here and carry the same table
// in engine/slot_chooser_drift_docker_test.go. Loader placement (loader_place.go)
// is here, as two rows: the home a leg is pointed at, and the buffer it falls
// back to.

type driftHold struct {
	name string
	// laneOnly marks a hold that exists only for a slot inside a lane.
	laneOnly bool
	apply    func(t *testing.T, db *store.DB, fx *driftFixture)
}

type driftFixture struct {
	prefix    string
	grp, dug  *nodes.Node
	lane      *nodes.Node // the candidate's lane; nil when the candidate is flat
	candidate *nodes.Node
	claimant  *orders.Order
	bp        *payloads.Payload
}

type driftChooser struct {
	name string
	flat bool
	// group builds a non-NGRP synthetic parent instead of a group. Only the
	// DefaultResolver arm reads it.
	plainParent bool
	setup       func(t *testing.T, db *store.DB, fx *driftFixture)
	choose      func(t *testing.T, db *store.DB, d *Dispatcher, fx *driftFixture) *nodes.Node
}

// setupDriftFixture builds:
//
//	<p>-GRP (NGRP, or a plain synthetic parent)
//	├── <p>-DUG  depth 2: S1 (blocker) · S2 (target)   <- a lane being dug, full
//	└── <p>-X    the candidate: a flat child, or the one slot of <p>-L
func setupDriftFixture(t *testing.T, db *store.DB, prefix string, ch driftChooser) *driftFixture {
	t.Helper()
	grpType, err := db.GetNodeTypeByCode("NGRP")
	testutil.MustNoErr(t, err, "get NGRP type")
	lanType, err := db.GetNodeTypeByCode("LANE")
	testutil.MustNoErr(t, err, "get LANE type")

	fx := &driftFixture{prefix: prefix, bp: &payloads.Payload{Code: prefix + "-P"}}
	testutil.MustNoErr(t, db.CreatePayload(fx.bp), "create payload")

	fx.grp = &nodes.Node{Name: prefix + "-GRP", Enabled: true, IsSynthetic: true}
	if !ch.plainParent {
		fx.grp.NodeTypeID = &grpType.ID
	}
	testutil.MustNoErr(t, db.CreateNode(fx.grp), "create parent")

	if !ch.plainParent {
		fx.dug = &nodes.Node{Name: prefix + "-DUG", NodeTypeID: &lanType.ID, ParentID: &fx.grp.ID, Enabled: true, IsSynthetic: true}
		testutil.MustNoErr(t, db.CreateNode(fx.dug), "create dug lane")
		for d := 1; d <= 2; d++ {
			at := d
			s := &nodes.Node{Name: fmt.Sprintf("%s-DUG-S%d", prefix, d), ParentID: &fx.dug.ID, Enabled: true, Depth: &at}
			testutil.MustNoErr(t, db.CreateNode(s), "create dug slot")
			createTestBinAtNode(t, db, fx.bp.Code, s.ID, fmt.Sprintf("%s-DUG-BIN%d", prefix, d))
		}
	}

	if ch.flat {
		fx.candidate = &nodes.Node{Name: prefix + "-X", ParentID: &fx.grp.ID, Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(fx.candidate), "create flat candidate")
	} else {
		fx.lane = &nodes.Node{Name: prefix + "-L", NodeTypeID: &lanType.ID, ParentID: &fx.grp.ID, Enabled: true, IsSynthetic: true}
		testutil.MustNoErr(t, db.CreateNode(fx.lane), "create candidate lane")
		one := 1
		fx.candidate = &nodes.Node{Name: prefix + "-X", ParentID: &fx.lane.ID, Enabled: true, Depth: &one}
		testutil.MustNoErr(t, db.CreateNode(fx.candidate), "create lane candidate")
	}

	fx.claimant = testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = prefix + "-CLAIMANT"
		o.OrderType = OrderTypeStore
		o.Status = protocol.StatusSourcing
		o.PayloadCode = fx.bp.Code
	})
	if ch.setup != nil {
		ch.setup(t, db, fx)
	}
	return fx
}

// driftHolder is the stranger holding the candidate. It names no delivery node
// unless a hold says so, so each row isolates one kind of hold.
func driftHolder(t *testing.T, db *store.DB, fx *driftFixture, mutate ...func(*orders.Order)) *orders.Order {
	t.Helper()
	return testdb.CreateOrder(t, db, append([]func(*orders.Order){func(o *orders.Order) {
		o.EdgeUUID = fx.prefix + "-HOLDER"
		o.OrderType = OrderTypeComplex
		o.Status = protocol.StatusSourcing
	}}, mutate...)...)
}

var driftHolds = []driftHold{
	{name: "free"},
	{name: "bin-present", apply: func(t *testing.T, db *store.DB, fx *driftFixture) {
		createTestBinAtNode(t, db, fx.bp.Code, fx.candidate.ID, fx.prefix+"-OCCUPANT")
	}},
	{name: "stranger-hard-claim", apply: func(t *testing.T, db *store.DB, fx *driftFixture) {
		h := driftHolder(t, db, fx)
		testutil.MustNoErr(t, db.ReserveSlot(fx.candidate.ID, h.ID), "reserve")
		testutil.MustNoErr(t, db.ConfirmSlotClaim(fx.candidate.ID, h.ID, nil), "confirm the hard claim")
	}},
	{name: "stranger-pending-reservation", apply: func(t *testing.T, db *store.DB, fx *driftFixture) {
		h := driftHolder(t, db, fx)
		testutil.MustNoErr(t, db.ReserveSlot(fx.candidate.ID, h.ID), "reserve")
	}},
	{name: "stranger-confirmed-reservation", apply: func(t *testing.T, db *store.DB, fx *driftFixture) {
		h := driftHolder(t, db, fx)
		testutil.MustNoErr(t, db.ReserveSlot(fx.candidate.ID, h.ID), "reserve")
		testutil.MustNoErr(t, db.ConfirmSlotReservation(fx.candidate.ID, h.ID), "confirm the reservation")
	}},
	{name: "live-delivery-node", apply: func(t *testing.T, db *store.DB, fx *driftFixture) {
		driftHolder(t, db, fx, func(o *orders.Order) {
			o.OrderType = OrderTypeStore
			o.Status = protocol.StatusQueued
			o.DeliveryNode = fx.candidate.Name
		})
	}},
	{name: "occupancy-row", laneOnly: true, apply: func(t *testing.T, db *store.DB, fx *driftFixture) {
		h := driftHolder(t, db, fx)
		ok, err := reservations.AcquireOccupancy(db.DB, h.ID, fx.lane.ID)
		testutil.MustNoErr(t, err, "take occupancy")
		if !ok {
			t.Fatal("the holder could not take occupancy of the candidate's lane")
		}
	}},
}

func storeAsker(fx *driftFixture) reservations.DigAsker { return digAskerFor(fx.claimant) }

func resolvedNode(res *ResolveResult, err error) *nodes.Node {
	if err != nil || res == nil {
		return nil
	}
	return res.Node
}

var driftChoosers = []driftChooser{
	{name: "shuffle-walk-plan-time-lane", choose: shuffleChoice(false)},
	{name: "shuffle-walk-plan-time-flat", flat: true, choose: shuffleChoice(false)},
	{name: "shuffle-walk-dwell-lane", choose: shuffleChoice(true)},
	{name: "shuffle-walk-dwell-flat", flat: true, choose: shuffleChoice(true)},
	{
		// The store selector, and through it the gate rebind
		// (lane_gate_release.go), which asks exactly this.
		name: "find-store-slot-in-lane",
		choose: func(t *testing.T, db *store.DB, d *Dispatcher, fx *driftFixture) *nodes.Node {
			n, err := db.FindStoreSlotInLaneExcluding(fx.lane.ID, fx.claimant.ID)
			if err != nil {
				return nil
			}
			return n
		},
	},
	{name: "group-resolver-lknd-lane", choose: groupStoreChoice},
	{name: "group-resolver-lknd-direct", flat: true, choose: groupStoreChoice},
	{name: "group-resolver-dpth-lane", setup: dpth, choose: groupStoreChoice},
	{name: "group-resolver-dpth-direct", flat: true, setup: dpth, choose: groupStoreChoice},
	{
		name: "group-resolver-vacated",
		flat: true,
		choose: func(t *testing.T, db *store.DB, d *Dispatcher, fx *driftFixture) *nodes.Node {
			gr := &GroupResolver{DB: db}
			return resolvedNode(gr.ResolveStoreVacated(fx.grp, fx.candidate, nil, "",
				binresolver.UnknownBinType("drift table"), storeAsker(fx)))
		},
	},
	{
		name:        "default-resolver-plain-parent",
		flat:        true,
		plainParent: true,
		choose: func(t *testing.T, db *store.DB, d *Dispatcher, fx *driftFixture) *nodes.Node {
			r := &DefaultResolver{DB: db}
			return resolvedNode(r.Resolve(fx.grp, binresolver.ResolveModeStore, "",
				binresolver.UnknownBinType("drift table"), storeAsker(fx), nil))
		},
	},
	{
		// A loader's carrier pull: the candidate is its one window. The claimant
		// is the order the pull creates.
		name: "loader-replenish-window",
		flat: true,
		choose: func(t *testing.T, db *store.DB, d *Dispatcher, fx *driftFixture) *nodes.Node {
			source := &nodes.Node{Name: fx.prefix + "-EMPTIES", Enabled: true}
			testutil.MustNoErr(t, db.CreateNode(source), "create the inbound source")
			res, err := d.ReplenishLoader(ReplenishRequest{
				StationID: "edge.line1", LoaderID: 99, PayloadCode: fx.bp.Code,
				Threshold: 10, CurrentUOP: 0, PerBinCapacity: 10,
			}, LoaderReplenishConfig{
				Layout:        loaders.LayoutSharedWindow,
				Homes:         []loaders.Home{{PositionNodeID: fx.candidate.ID}},
				NodeNames:     map[int64]string{fx.candidate.ID: fx.candidate.Name},
				Payloads:      []loaders.Payload{{PayloadCode: fx.bp.Code}},
				InboundSource: source.Name,
			})
			testutil.MustNoErr(t, err, "ReplenishLoader")
			if len(res.Created) == 0 {
				return nil
			}
			fx.claimant = res.Created[0]
			return fx.candidate
		},
	},
	// Loader placement. The live-delivery-node row passes because the door
	// accepts, on purpose: a loader does not take the clause about a live order
	// merely naming the slot. For a loader the first to claim goes
	// (CheckDropoffCapacity), because two returns waiting on one empty home each
	// name it and counting the name keeps both off it, and a return that gives
	// its home up to a refill merely naming it is link 2 of the 2026-08-26 chain
	// (loader_place_picks_docker_test.go pins both).
	{name: "loader-place-home", flat: true, setup: loaderDriftHome(false), choose: loaderDriftChoice},
	{name: "loader-place-buffer", flat: true, setup: loaderDriftHome(true), choose: loaderDriftChoice},
}

// loaderDriftHome makes the candidate a member of a dedicated loader: its home,
// or, with asBuffer, its only buffer behind a home a bin stands on. The
// candidate is a STOR-typed node standing alone, as a loader member does.
func loaderDriftHome(asBuffer bool) func(t *testing.T, db *store.DB, fx *driftFixture) {
	return func(t *testing.T, db *store.DB, fx *driftFixture) {
		storType := &nodes.NodeType{Code: protocol.NodeClassSTOR, Name: "Storage Slot"}
		if nt, err := db.GetNodeTypeByCode(protocol.NodeClassSTOR); err == nil && nt != nil {
			storType = nt
		} else {
			testutil.MustNoErr(t, db.CreateNodeType(storType), "create STOR type")
		}
		fx.candidate = &nodes.Node{Name: fx.prefix + "-M", Enabled: true, NodeTypeID: &storType.ID}
		testutil.MustNoErr(t, db.CreateNode(fx.candidate), "loader member")
		loaderID, err := db.CreateLoader(store.Loader{Name: fx.prefix + "-LD", Role: "consume",
			Layout: loaders.LayoutDedicatedPositions, Replenishment: "operator"})
		testutil.MustNoErr(t, err, "create loader")
		home := fx.candidate
		if asBuffer {
			home = &nodes.Node{Name: fx.prefix + "-H", Enabled: true, NodeTypeID: &storType.ID}
			testutil.MustNoErr(t, db.CreateNode(home), "loader home")
			createTestBinAtNode(t, db, fx.bp.Code, home.ID, fx.prefix+"-HOME-BIN")
			testutil.MustNoErr(t, db.UpsertLoaderHome(store.LoaderHome{LoaderID: loaderID,
				PositionNodeID: fx.candidate.ID, Kind: loaders.HomeKindBuffer}), "buffer")
		}
		testutil.MustNoErr(t, db.UpsertLoaderHome(store.LoaderHome{LoaderID: loaderID,
			PositionNodeID: home.ID, PayloadCode: fx.bp.Code, Kind: loaders.HomeKindHome}), "home")
	}
}

// loaderDriftChoice places a supply leg bound for the loader's home. The leg is
// the claimant; a wait offers nothing.
func loaderDriftChoice(t *testing.T, db *store.DB, d *Dispatcher, fx *driftFixture) *nodes.Node {
	home := fx.candidate
	if h, err := db.GetNodeByDotName(fx.prefix + "-H"); err == nil && h != nil {
		home = h
	}
	staging := &nodes.Node{Name: fx.prefix + "-STG", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(staging), "staging")
	leg := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID, o.StationID, o.OrderType, o.Status = fx.prefix+"-LEG", "test", OrderTypeComplex, protocol.StatusSourcing
		o.SourceNode, o.DeliveryNode, o.PayloadCode = staging.Name, home.Name, fx.bp.Code
	})
	steps := []resolvedStep{vsWait(staging.Name), vsPick(staging.Name), vsDrop(home.Name)}
	if wait := d.placeForDedicatedLoader(leg, steps, nil); wait != "" {
		return nil
	}
	fx.claimant = leg
	n, err := db.GetNodeByDotName(leg.DeliveryNode)
	testutil.MustNoErr(t, err, "read the pick")
	return n
}

func shuffleChoice(dwell bool) func(t *testing.T, db *store.DB, d *Dispatcher, fx *driftFixture) *nodes.Node {
	return func(t *testing.T, db *store.DB, d *Dispatcher, fx *driftFixture) *nodes.Node {
		asker, claimant := reservations.Anyone, noClaimantYet
		if dwell {
			asker, claimant = digAskerFor(fx.claimant), fx.claimant.ID
		}
		slots, err := findShuffleSlots(db, fx.dug.ID, fx.grp.ID, 1, asker, claimant, nil)
		if err != nil || len(slots) == 0 {
			return nil
		}
		return slots[0]
	}
}

func groupStoreChoice(t *testing.T, db *store.DB, d *Dispatcher, fx *driftFixture) *nodes.Node {
	gr := &GroupResolver{DB: db}
	return resolvedNode(gr.ResolveStore(fx.grp, "", binresolver.UnknownBinType("drift table"), storeAsker(fx)))
}

func dpth(t *testing.T, db *store.DB, fx *driftFixture) {
	testutil.MustNoErr(t, db.SetNodeProperty(fx.grp.ID, binresolver.PropStoreAlgorithm, StoreDPTH), "set DPTH")
}

// TestSlotChoosers_NeverOfferWhatTheClaimDoorRefuses is the drift guard.
//
// MUTATION: drop the reservation clause from any chooser (for the group
// resolver's direct arms, the SlotSpokenForByStranger check; for the shuffle
// walk, the takeable test). Its pending- and confirmed-reservation rows fail:
// the chooser returns the candidate and claimStoreSlot refuses it.
func TestSlotChoosers_NeverOfferWhatTheClaimDoorRefuses(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	n := 0
	for _, ch := range driftChoosers {
		for _, hold := range driftHolds {
			if hold.laneOnly && ch.flat {
				continue
			}
			n++
			prefix := fmt.Sprintf("DR%d", n)
			t.Run(ch.name+"/"+hold.name, func(t *testing.T) {
				fx := setupDriftFixture(t, db, prefix, ch)
				if hold.apply != nil {
					hold.apply(t, db, fx)
				}
				got := ch.choose(t, db, d, fx)
				if hold.name == "free" {
					if got == nil || got.ID != fx.candidate.ID {
						t.Fatalf("with nothing held, %s did not offer %s (got %v): the fixture does not reach "+
							"the candidate, so the hold rows would prove nothing", ch.name, fx.candidate.Name, got)
					}
				}
				if got == nil {
					return
				}
				if err := claimStoreSlot(db, fx.claimant, got); err != nil {
					t.Errorf("%s offered %s under a %s, and the claim door refused it: %v. A deterministic "+
						"chooser offers it again on every ask for as long as the hold stands",
						ch.name, got.Name, hold.name, err)
				}
			})
		}
	}
}
