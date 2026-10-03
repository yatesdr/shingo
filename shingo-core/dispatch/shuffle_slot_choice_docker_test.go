//go:build docker

package dispatch

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/payloads"
	"shingocore/store/reservations"
)

// shuffle_slot_choice_docker_test.go — what the shuffle walk picks, written down
// as a sequence.
//
// THE WALK IS DETERMINISTIC, SO ITS WHOLE ANSWER IS A LIST. findShuffleSlots is
// asked for one slot at a time with every slot it has already offered excluded,
// until it refuses. The list it produces is the order a dweller would be offered
// candidates in, and it is what changes, or must not change, when the
// availability question underneath the walk is re-spelled. Each case below holds
// one slot in one way and pins the list that results.
//
// The fixture:
//
//	CH-GRP
//	├── CH-DUG  depth 3: S1 (EMPTY) · S2 (blocker) · S3 (target)   <- being dug
//	├── CH-LA   depth 2: S1 · S2                                   <- empty
//	├── CH-LB   depth 1: S1                                        <- empty
//	└── CH-P1   (a direct child of the group)
//
// CH-DUG-S1 is empty and reachable, and it is never offered: no bin returns to
// the lane being dug.

type choiceFixture struct {
	grp, dug, la, lb, p1 *nodes.Node
	dugSlots, laSlots    []*nodes.Node
	lbSlot               *nodes.Node
	bp                   *payloads.Payload
}

func setupChoiceFixture(t *testing.T, db *store.DB, prefix string) *choiceFixture {
	t.Helper()
	grpType, err := db.GetNodeTypeByCode("NGRP")
	testutil.MustNoErr(t, err, "get NGRP type")
	lanType, err := db.GetNodeTypeByCode("LANE")
	testutil.MustNoErr(t, err, "get LANE type")

	f := &choiceFixture{bp: &payloads.Payload{Code: prefix + "-P"}}
	testutil.MustNoErr(t, db.CreatePayload(f.bp), "create payload")

	f.grp = &nodes.Node{Name: prefix + "-GRP", NodeTypeID: &grpType.ID, Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(f.grp), "create group")

	mkLane := func(name string, depth int) (*nodes.Node, []*nodes.Node) {
		l := &nodes.Node{Name: name, NodeTypeID: &lanType.ID, ParentID: &f.grp.ID, Enabled: true, IsSynthetic: true}
		testutil.MustNoErr(t, db.CreateNode(l), "create "+name)
		var out []*nodes.Node
		for d := 1; d <= depth; d++ {
			at := d
			s := &nodes.Node{Name: fmt.Sprintf("%s-S%d", name, d), ParentID: &l.ID, Enabled: true, Depth: &at}
			testutil.MustNoErr(t, db.CreateNode(s), "create slot")
			out = append(out, s)
		}
		reloaded, err := db.GetNode(l.ID)
		return testutil.Must(t, reloaded, err, "reload "+name), out
	}
	f.dug, f.dugSlots = mkLane(prefix+"-DUG", 3)
	f.la, f.laSlots = mkLane(prefix+"-LA", 2)
	var lb []*nodes.Node
	f.lb, lb = mkLane(prefix+"-LB", 1)
	f.lbSlot = lb[0]
	f.p1 = &nodes.Node{Name: prefix + "-P1", ParentID: &f.grp.ID, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(f.p1), "create direct child")

	createTestBinAtNode(t, db, f.bp.Code, f.dugSlots[1].ID, prefix+"-BLK")
	createTestBinAtNode(t, db, f.bp.Code, f.dugSlots[2].ID, prefix+"-TGT")
	return f
}

// walkShuffleSlots asks findShuffleSlots for one slot at a time, excluding each
// answer from the next ask, and returns the names in the order offered plus the
// refusal that ended the walk.
func walkShuffleSlots(t *testing.T, db *store.DB, f *choiceFixture, asker reservations.DigAsker) ([]string, error) {
	t.Helper()
	exclude := map[int64]bool{}
	var got []string
	for i := 0; i < 32; i++ {
		slots, err := findShuffleSlots(db, f.dug.ID, f.grp.ID, 1, asker, exclude)
		if err != nil {
			return got, err
		}
		if len(slots) != 1 {
			t.Fatalf("findShuffleSlots returned %d slots and no error for a count of 1", len(slots))
		}
		got = append(got, slots[0].Name)
		exclude[slots[0].ID] = true
	}
	t.Fatalf("the walk never ended: %v", got)
	return nil, nil
}

// strangerOrder is a live order that is nobody's dig: the holder in every case.
func strangerOrder(t *testing.T, db *store.DB, uuid string, mutate ...func(*orders.Order)) *orders.Order {
	t.Helper()
	return testdb.CreateOrder(t, db, append([]func(*orders.Order){func(o *orders.Order) {
		o.EdgeUUID = uuid
		o.OrderType = OrderTypeRetrieve
		o.Status = protocol.StatusSourcing
	}}, mutate...)...)
}

// claimCarriedBin gives an order a claimed bin standing on a node of its own,
// so the order counts as BRINGING a bin to its delivery_node (the in-flight
// shape orders.InFlightForDropoffSQL reads) without occupying any fixture slot.
func claimCarriedBin(t *testing.T, db *store.DB, o *orders.Order, bp string) {
	t.Helper()
	src := &nodes.Node{Name: o.EdgeUUID + "-SRC", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(src), "create the carried bin's node")
	b := createTestBinAtNode(t, db, bp, src.ID, o.EdgeUUID+"-BIN")
	testdb.ClaimBinForTest(t, db, b.ID, o.ID)
}

// TestShuffleWalk_PicksForTheHoldsItAlreadyRespects is the behaviour the
// re-spelling of the availability question must keep. None of these is the F6
// defect; every one is a hold the walk already answered correctly, and the
// sequence each produces is pinned exactly so a change in any pick is seen.
func TestShuffleWalk_PicksForTheHoldsItAlreadyRespects(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)

	cases := []struct {
		name string
		// hold mutates the fixture; it may return the asker the walk runs as.
		hold    func(t *testing.T, db *store.DB, f *choiceFixture, prefix string) reservations.DigAsker
		want    func(f *choiceFixture) []string
		wantErr func(t *testing.T, err error)
	}{
		{
			// Nothing held: direct children first, then each lane deepest-first.
			name: "free",
			want: func(f *choiceFixture) []string {
				return []string{f.p1.Name, f.laSlots[1].Name, f.laSlots[0].Name, f.lbSlot.Name}
			},
		},
		{
			// Every candidate has a bin in it: the walk refuses at once, and the
			// refusal is congestion (a wait), never a fault.
			name: "empty-group",
			hold: func(t *testing.T, db *store.DB, f *choiceFixture, p string) reservations.DigAsker {
				for i, n := range []*nodes.Node{f.p1, f.laSlots[0], f.laSlots[1], f.lbSlot} {
					createTestBinAtNode(t, db, f.bp.Code, n.ID, fmt.Sprintf("%s-FULL-%d", p, i))
				}
				return reservations.Anyone
			},
			want: func(f *choiceFixture) []string { return nil },
		},
		{
			// A bin on the direct child: it is skipped and nothing else moves.
			name: "occupied-direct",
			hold: func(t *testing.T, db *store.DB, f *choiceFixture, p string) reservations.DigAsker {
				createTestBinAtNode(t, db, f.bp.Code, f.p1.ID, p+"-OCC")
				return reservations.Anyone
			},
			want: func(f *choiceFixture) []string {
				return []string{f.laSlots[1].Name, f.laSlots[0].Name, f.lbSlot.Name}
			},
		},
		{
			// A bin at a lane's mouth: the mouth is taken and the empty slot behind
			// it is unreachable, so the whole lane drops out.
			name: "occupied-mouth",
			hold: func(t *testing.T, db *store.DB, f *choiceFixture, p string) reservations.DigAsker {
				createTestBinAtNode(t, db, f.bp.Code, f.laSlots[0].ID, p+"-OCC")
				return reservations.Anyone
			},
			want: func(f *choiceFixture) []string { return []string{f.p1.Name, f.lbSlot.Name} },
		},
		{
			// A stranger's HARD claim on the direct child, taken the production way
			// (slot reservation, then ConfirmSlotClaim): skipped.
			name: "hard-claim",
			hold: func(t *testing.T, db *store.DB, f *choiceFixture, p string) reservations.DigAsker {
				o := strangerOrder(t, db, p+"-HOLDER")
				testutil.MustNoErr(t, db.ReserveSlot(f.p1.ID, o.ID), "reserve the slot")
				testutil.MustNoErr(t, db.ConfirmSlotClaim(f.p1.ID, o.ID, nil), "confirm the slot claim")
				return reservations.Anyone
			},
			want: func(f *choiceFixture) []string {
				return []string{f.laSlots[1].Name, f.laSlots[0].Name, f.lbSlot.Name}
			},
		},
		{
			// A bare claimed_by with no reservation row behind it: skipped too.
			name: "bare-claimed-by",
			hold: func(t *testing.T, db *store.DB, f *choiceFixture, p string) reservations.DigAsker {
				o := strangerOrder(t, db, p+"-HOLDER")
				testdb.ClaimSlotForTest(t, db, f.lbSlot.ID, o.ID)
				return reservations.Anyone
			},
			want: func(f *choiceFixture) []string {
				return []string{f.p1.Name, f.laSlots[1].Name, f.laSlots[0].Name}
			},
		},
		{
			// An order on its way to LA-S2 carrying a claimed bin: LA-S2 is
			// skipped as inbound traffic, and LA-S1 is skipped because parking there
			// would seal it (the entombing guard).
			name: "inbound-with-bin",
			hold: func(t *testing.T, db *store.DB, f *choiceFixture, p string) reservations.DigAsker {
				o := strangerOrder(t, db, p+"-INBOUND", func(o *orders.Order) {
					o.DeliveryNode = f.laSlots[1].Name
					o.PayloadCode = f.bp.Code
				})
				claimCarriedBin(t, db, o, f.bp.Code)
				return reservations.Anyone
			},
			want: func(f *choiceFixture) []string { return []string{f.p1.Name, f.lbSlot.Name} },
		},
		{
			// A live order names LB-S1 as its delivery_node but holds no bin and no
			// reservation yet (it has planned, not dispatched). The walk counts only
			// orders BRINGING a bin, so LB-S1 is still offered.
			name: "named-not-bringing",
			hold: func(t *testing.T, db *store.DB, f *choiceFixture, p string) reservations.DigAsker {
				strangerOrder(t, db, p+"-PLANNED", func(o *orders.Order) {
					o.DeliveryNode = f.lbSlot.Name
					o.Status = protocol.StatusQueued
				})
				return reservations.Anyone
			},
			want: func(f *choiceFixture) []string {
				return []string{f.p1.Name, f.laSlots[1].Name, f.laSlots[0].Name, f.lbSlot.Name}
			},
		},
		{
			// A foreign dig holds lane LB: right of way removes the lane from the
			// pool at the source, and the shortfall names the dig rather than a
			// full group.
			name: "right-of-way",
			hold: func(t *testing.T, db *store.DB, f *choiceFixture, p string) reservations.DigAsker {
				d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
				foreign := strangerOrder(t, db, p+"-FOREIGN-DIG")
				if !d.laneLock.TryLock(f.lb.ID, foreign.ID) {
					t.Fatal("the foreign dig could not take lane LB")
				}
				return reservations.Anyone
			},
			want: func(f *choiceFixture) []string {
				return []string{f.p1.Name, f.laSlots[1].Name, f.laSlots[0].Name}
			},
			wantErr: func(t *testing.T, err error) {
				var held *DigParkingHeldError
				if !errors.As(err, &held) {
					t.Errorf("the walk ended with %v, want a DigParkingHeldError naming the foreign dig", err)
				}
			},
		},
		{
			// A bin at LA-S2 that a live order is coming to collect: LA-S1 stands in
			// front of it, and parking there would bury it (the burial guard).
			name: "burial",
			hold: func(t *testing.T, db *store.DB, f *choiceFixture, p string) reservations.DigAsker {
				b := createTestBinAtNode(t, db, f.bp.Code, f.laSlots[1].ID, p+"-WANTED")
				o := strangerOrder(t, db, p+"-COLLECTOR", func(o *orders.Order) { o.SourceNode = f.laSlots[1].Name })
				testdb.ClaimBinForTest(t, db, b.ID, o.ID)
				return reservations.Anyone
			},
			want: func(f *choiceFixture) []string { return []string{f.p1.Name, f.lbSlot.Name} },
		},
		{
			// LA-S2 empty and hard-claimed by a stranger: LA-S2 is skipped as
			// claimed, and LA-S1 because parking there would entomb it.
			name: "entombing",
			hold: func(t *testing.T, db *store.DB, f *choiceFixture, p string) reservations.DigAsker {
				o := strangerOrder(t, db, p+"-FILLER")
				testdb.ClaimSlotForTest(t, db, f.laSlots[1].ID, o.ID)
				return reservations.Anyone
			},
			want: func(f *choiceFixture) []string { return []string{f.p1.Name, f.lbSlot.Name} },
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prefix := fmt.Sprintf("CH%d", i)
			f := setupChoiceFixture(t, db, prefix)
			asker := reservations.Anyone
			if tc.hold != nil {
				asker = tc.hold(t, db, f, prefix)
			}
			got, err := walkShuffleSlots(t, db, f, asker)
			for _, name := range got {
				for _, s := range f.dugSlots {
					if name == s.Name {
						t.Fatalf("the walk offered %s, a slot of the lane being dug", name)
					}
				}
			}
			if want := tc.want(f); !reflect.DeepEqual(got, want) {
				t.Errorf("the walk offered %v, want %v", got, want)
			}
			if tc.wantErr != nil {
				tc.wantErr(t, err)
			} else if !errors.Is(err, ErrNoShuffleSlot) {
				t.Errorf("the walk ended with %v, want ErrNoShuffleSlot (a wait, not a fault)", err)
			}
		})
	}
}
