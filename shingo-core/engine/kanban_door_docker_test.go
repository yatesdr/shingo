//go:build docker

package engine

import (
	"fmt"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// kanban_door_docker_test.go — the one chooser at its doors and in the walk
// (plan.Planner.ChoosePush, kanban.AssignAcross), on the cost rig's shape: K whole,
// sources and overflow set per test: two sources with K whole, a loan repaid before
// the overflow, the outbound full; Short against the cap; the floor; a person's drop;
// a robot; a source with no level.

// doorZone is a flat group declaring small at want with `held` standing, or plain.
func (r *kanbanCostRig) doorZone(t *testing.T, name string, slots, want, held int) *nodes.Node {
	t.Helper()
	g, kids := kanbanCostFlat(t, r.db, name, slots)
	if want > 0 {
		testutil.MustNoErr(t, r.db.SetKanbanLevel(store.KanbanLevel{GroupNodeID: g.ID, BinTypeID: r.small, Want: want}), "level")
	}
	for i := 0; i < held; i++ {
		landAt(t, r.db, r.small, kids[i].ID, fmt.Sprintf("%s-S%d", name, i))
	}
	return g
}

// intake is the wire's push at K, up to its birth: the door's choice.
func (r *kanbanCostRig) intake(t *testing.T, uuid, window string, bin int64) *orders.Order {
	t.Helper()
	o, _, lerr := r.eng.dispatcher.Lifecycle().CreateInboundOrder("kc-edge", &protocol.OrderRequest{
		OrderUUID: uuid, OrderType: dispatch.OrderTypeMove, Quantity: 1, OriginClass: protocol.OriginClassNoDemand,
		SourceNode: window, DeliveryNode: r.k.Name, BinID: bin}, nil)
	if lerr != nil {
		t.Fatalf("intake refused the push: %s", lerr.Detail)
	}
	return o
}

// parked is a window's push already born aimed at K, holding its cart, as a push
// admission could not place is: the scanner's arm asks next.
func (r *kanbanCostRig) parked(t *testing.T, uuid, window string, bin int64) *orders.Order {
	t.Helper()
	o := testdb.CreateOrder(t, r.db, func(o *orders.Order) {
		o.EdgeUUID = uuid
		o.StationID = "kc-edge"
		o.OrderType = dispatch.OrderTypeMove
		o.SourceIntent = dispatch.SourceIntentLocal
		o.OriginClass = protocol.OriginClassNoDemand
		o.SourceNode = window
		o.DeliveryNode = r.k.Name
		o.NamedBinID = &bin
	})
	testutil.MustNoErr(t, reservations.Acquire(r.db.DB, o.ID, o.ID, bin, "test"), "the push holds its cart")
	testdb.HoldCart(t, r.db, o.ID, bin)
	return o
}

// walk is K's line as a snapshot build asks it, the rig's windows given as rows.
func (r *kanbanCostRig) walk(t *testing.T, rows ...protocol.NodeBinsRow) []protocol.NodeBinsAssignment {
	t.Helper()
	picture, err := r.db.ListKanbanPictureWithSources([]int64{r.k.ID})
	testutil.MustNoErr(t, err, "the picture")
	levels := map[int64][]store.KanbanLevel{}
	var sources []store.KanbanRef
	for _, row := range picture {
		levels[row.KanbanNodeID] = append(levels[row.KanbanNodeID], row.KanbanLevel())
		if row.KanbanNodeID == r.k.ID {
			sources = row.Sources
		}
	}
	ask := store.GroupLineAsk{Group: r.k, Levels: levels[r.k.ID], Windows: rows}
	for _, s := range sources {
		ask.Sources = append(ask.Sources, store.GroupLineSource{ID: s.ID, Name: s.Name, Levels: levels[s.ID]})
	}
	lines, err := r.eng.UnloaderGroupLines([]store.GroupLineAsk{ask})
	testutil.MustNoErr(t, err, "the lines")
	if len(lines) != 1 {
		t.Fatalf("lines = %+v", lines)
	}
	return lines[0].Assignments
}

// walked says a line's assignments as words: node=type@group, or node=none-short.
func walkSaid(as []protocol.NodeBinsAssignment) string {
	var out []string
	for _, a := range as {
		s := a.Node + "=" + a.BinType
		if a.NoneShort {
			s = a.Node + "=none-short"
		}
		if a.Group != "" {
			s += "@" + a.Group
		}
		out = append(out, s)
	}
	return strings.Join(out, " ")
}

func (r *kanbanCostRig) reload(t *testing.T, o *orders.Order) *orders.Order {
	t.Helper()
	got, err := r.db.GetOrder(o.ID)
	testutil.MustNoErr(t, err, "reload the push")
	return got
}

// parksNamedAtK asserts the push waits, visible, named at K, with no robot.
func (r *kanbanCostRig) parksNamedAtK(t *testing.T, o *orders.Order) {
	t.Helper()
	got := r.reload(t, o)
	if got.DeliveryNode != r.k.Name || got.Status != protocol.StatusQueued || got.QueueCause != string(dispatch.CauseNGRPAtLevel) {
		t.Errorf("the push is %s to %s (%q); want queued, named at %s, under %s",
			got.Status, got.DeliveryNode, got.QueueCause, r.k.Name, dispatch.CauseNGRPAtLevel)
	}
}

// TestKanbanDoor_TheFirstSourceShortInRankOrder: two sources and K whole. A cart
// arriving goes to the first source in rank order short of its type, not the shortest,
// at the door, on a parked pass and in the walk.
func TestKanbanDoor_TheFirstSourceShortInRankOrder(t *testing.T) {
	t.Parallel()
	setup := func(t *testing.T) (*kanbanCostRig, *nodes.Node) {
		r := newKanbanCostRig(t, 2, 2, 2)
		first := r.doorZone(t, "KD1-FIRST", 4, 2, 1)
		second := r.doorZone(t, "KD1-SECOND", 4, 2, 0)
		testdb.KanbanSources(t, r.db, r.k.ID, r.z45.ID, first.ID, second.ID)
		return r, first
	}
	t.Run("at the door", func(t *testing.T) {
		t.Parallel()
		r, first := setup(t)
		o := r.intake(t, "kd1-door", r.windows[1].Name, r.push)
		if !r.inGroup(t, o.DeliveryNode, first) {
			t.Errorf("the push went to %s, want the first source short of it, %s", o.DeliveryNode, first.Name)
		}
	})
	t.Run("on a parked pass", func(t *testing.T) {
		t.Parallel()
		r, first := setup(t)
		o := r.parked(t, "kd1-pass", r.windows[1].Name, r.push)
		r.eng.fulfillment.RunOnce()
		if got := r.reload(t, o); !r.inGroup(t, got.DeliveryNode, first) || got.Status != protocol.StatusDispatched {
			t.Errorf("the push is %s to %s, want dispatched into %s", got.Status, got.DeliveryNode, first.Name)
		}
	})
	t.Run("in the walk", func(t *testing.T) {
		t.Parallel()
		r, _ := setup(t)
		got := walkSaid(r.walk(t, cart("W1", "KC-SMALL", false), cart("W2", "KC-SMALL", false), cart("W3", "KC-SMALL", false)))
		if want := "W1=KC-SMALL@KD1-FIRST W2=KC-SMALL@KD1-SECOND W3=KC-SMALL@KD1-SECOND"; got != want {
			t.Errorf("walk = %q, want %q: rank order over what is left, each source for what it is short of", got, want)
		}
	})
}

// TestKanbanDoor_ALoanIsRepaidBeforeTheOverflow: borrowed from source 1, overflow =
// source 2. Source 1 is short, so it is repaid and the overflow is not reached.
func TestKanbanDoor_ALoanIsRepaidBeforeTheOverflow(t *testing.T) {
	t.Parallel()
	r := newKanbanCostRig(t, 1, 2, 2)
	other := r.doorZone(t, "KD2-OTHER", 4, 0, 0)
	testdb.KanbanSources(t, r.db, r.k.ID, r.z45.ID, other.ID)
	testdb.KanbanOverflow(t, r.db, r.k.ID, other.Name)
	if got := walkSaid(r.walk(t, cart("W2", "KC-SMALL", false))); got != "W2=KC-SMALL@KC-Z45" {
		t.Errorf("walk = %q, want the source it borrowed from lit", got)
	}
	o := r.intake(t, "kd2", r.windows[1].Name, r.push)
	if !r.inGroup(t, o.DeliveryNode, r.z45) {
		t.Errorf("the push went to %s, want the short source %s, not the overflow %s", o.DeliveryNode, r.z45.Name, other.Name)
	}
}

// TestKanbanDoor_TheOutboundFull: K short with no child free, so the next source short;
// none short, so the type's overflow; that full too, so it parks named at K, visible,
// as a full group does.
func TestKanbanDoor_TheOutboundFull(t *testing.T) {
	t.Parallel()
	// K short of small with no child free: every child stands a big.
	fullK := func(t *testing.T, r *kanbanCostRig) {
		testutil.MustNoErr(t, r.db.SetKanbanLevel(store.KanbanLevel{GroupNodeID: r.k.ID, BinTypeID: r.small, Want: 4}), "K short of small")
		kids, err := r.db.ListChildNodes(r.k.ID)
		testutil.MustNoErr(t, err, "K's children")
		for i, c := range kids {
			if n, _ := r.db.CountBinsByNode(c.ID); n == 0 {
				landAt(t, r.db, r.big, c.ID, fmt.Sprintf("KD3-FILL-%d", i))
			}
		}
	}
	t.Run("the next source short", func(t *testing.T) {
		t.Parallel()
		r := newKanbanCostRig(t, 1, 2, 2)
		fullK(t, r)
		o := r.intake(t, "kd3a", r.windows[1].Name, r.push)
		if !r.inGroup(t, o.DeliveryNode, r.z45) {
			t.Errorf("the push went to %s, want the next source short, %s", o.DeliveryNode, r.z45.Name)
		}
	})
	t.Run("the type's overflow", func(t *testing.T) {
		t.Parallel()
		r := newKanbanCostRig(t, 2, 2, 2)
		fullK(t, r)
		spare := r.doorZone(t, "KD3-SPARE", 2, 0, 0)
		testdb.KanbanOverflow(t, r.db, r.k.ID, spare.Name)
		o := r.intake(t, "kd3b", r.windows[1].Name, r.push)
		if !r.inGroup(t, o.DeliveryNode, spare) {
			t.Errorf("the push went to %s, want the overflow %s", o.DeliveryNode, spare.Name)
		}
	})
	t.Run("parked, named at the kanban", func(t *testing.T) {
		t.Parallel()
		r := newKanbanCostRig(t, 2, 2, 2)
		fullK(t, r)
		spare := r.doorZone(t, "KD3-SPARE", 1, 0, 1)
		testdb.KanbanOverflow(t, r.db, r.k.ID, spare.Name)
		o := r.intake(t, "kd3c", r.windows[1].Name, r.push)
		if o.DeliveryNode != r.k.Name {
			t.Fatalf("the push went to %s, want it left naming %s", o.DeliveryNode, r.k.Name)
		}
		r.eng.fulfillment.RunOnce()
		got := r.reload(t, o)
		if got.DeliveryNode != r.k.Name || got.Status != protocol.StatusQueued || got.QueueCause != string(dispatch.CauseNGRPFull) {
			t.Errorf("the push is %s to %s (%q); want queued, named at %s, under %s", got.Status, got.DeliveryNode,
				got.QueueCause, r.k.Name, dispatch.CauseNGRPFull)
		}
	})
}

// TestKanbanDoor_AGapAnAskCoversIsNotAdmitted: Short, not the cap. K holds none of its
// want of 1 with a keeper's ask live, so the cap would admit the push and K would end
// with want + 1 when the ask lands. At intake, at the wire's decision after it (whose
// gate is the cap), and on a parked pass. K has no source or overflow here, so it waits.
//
// And the push is what repays the lender the ask took from, with one to repay.
func TestKanbanDoor_AGapAnAskCoversIsNotAdmitted(t *testing.T) {
	t.Parallel()
	asked := func(t *testing.T) *kanbanCostRig {
		r := newKanbanCostRig(t, 2, 2, 2)
		testdb.KanbanOverflow(t, r.db, r.k.ID, "")
		_, err := r.db.Exec(`DELETE FROM bins WHERE label = 'KC-K-S-1'`)
		testutil.MustNoErr(t, err, "K short of small")
		m := r.eng.KanbanKeeper()
		m.Tick()
		if st := mntState(t, m, r.k.Name, "KC-SMALL"); st.Created != 1 {
			t.Fatalf("fixture drift: the keeper asked %d: %+v", st.Created, st)
		}
		return r
	}
	for _, door := range []string{"intake", "wire", "parked"} {
		t.Run(door, func(t *testing.T) {
			t.Parallel()
			r := asked(t)
			testdb.KanbanSources(t, r.db, r.k.ID)
			var o *orders.Order
			switch door {
			case "intake":
				o = r.intake(t, "kd4", r.windows[1].Name, r.push)
				if o.DeliveryNode != r.k.Name {
					t.Fatalf("the push went to %s; K's gap is its ask's", o.DeliveryNode)
				}
			case "wire":
				r.eng.Dispatcher().HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
					OrderUUID: "kd4", OrderType: protocol.OrderTypeMove, Quantity: 1, OriginClass: protocol.OriginClassNoDemand,
					SourceNode: r.windows[1].Name, DeliveryNode: r.k.Name, BinID: r.push})
				o = testdb.RequireOrder(t, r.db, "kd4")
			default:
				o = r.parked(t, "kd4", r.windows[1].Name, r.push)
			}
			r.eng.fulfillment.RunOnce()
			r.parksNamedAtK(t, o)
		})
	}
	t.Run("the lender is repaid", func(t *testing.T) {
		t.Parallel()
		r := asked(t)
		o := r.intake(t, "kd4l", r.windows[1].Name, r.push)
		if !r.inGroup(t, o.DeliveryNode, r.z45) {
			t.Errorf("the push went to %s, want %s, which the ask borrowed from", o.DeliveryNode, r.z45.Name)
		}
	})
}

// TestKanbanDoor_ADifferentFloorIsNoSourceOfTheCart: a 45x48 cart is never sent to, or
// lit for, a 32x30 source short of its own type, and a 32x30 cart passes over a 45x48
// source to its own.
func TestKanbanDoor_ADifferentFloorIsNoSourceOfTheCart(t *testing.T) {
	t.Parallel()
	t.Run("a 45x48 cart", func(t *testing.T) {
		t.Parallel()
		r := newKanbanCostRig(t, 2, 2, 2)
		testdb.KanbanOverflow(t, r.db, r.k.ID, "")
		testdb.KanbanSources(t, r.db, r.k.ID, r.z32.ID, r.z45.ID)
		_, err := r.db.Exec(`DELETE FROM bins WHERE label = 'KC-Z32-1'`)
		testutil.MustNoErr(t, err, "Z32 short of its own")
		if got := walkSaid(r.walk(t, cart("W2", "KC-SMALL", false))); got != "W2=none-short" {
			t.Errorf("walk = %q, want none-short: Z32 is short, of nothing this cart can be", got)
		}
		o := r.intake(t, "kd5a", r.windows[1].Name, r.push)
		r.eng.fulfillment.RunOnce()
		r.parksNamedAtK(t, o)
	})
	t.Run("a 32x30 cart", func(t *testing.T) {
		t.Parallel()
		r := newKanbanCostRig(t, 1, 2, 2)
		testdb.KanbanOverflow(t, r.db, r.k.ID, "")
		testdb.KanbanSources(t, r.db, r.k.ID, r.z45.ID, r.z32.ID)
		_, err := r.db.Exec(`DELETE FROM bins WHERE label = 'KC-Z32-1'`)
		testutil.MustNoErr(t, err, "Z32 short of its own")
		if got := walkSaid(r.walk(t, cart("W4", "KC-32X30", false))); got != "W4=KC-32X30@KC-Z32" {
			t.Errorf("walk = %q, want the 32x30 source lit: Z45 is short, of no type on this floor", got)
		}
		bin := landAt(t, r.db, r.t32, r.windows[3].ID, "KD5-W4")
		o := r.intake(t, "kd5b", r.windows[3].Name, bin)
		if !r.inGroup(t, o.DeliveryNode, r.z32) {
			t.Errorf("the push went to %s, want %s", o.DeliveryNode, r.z32.Name)
		}
	})
}

// TestKanbanDoor_APersonsNamedDropStaysNamed: a person's move to K off the same window,
// at a moment the chooser would send a push to a source (K's gap is an ask's, Z45 short),
// goes where they named: the cap's answer, K. Core does not re-choose what somebody
// chose. With K at its level the door refuses them to their face, and sends them nowhere
// else. A slot named directly is no group, and no door asks.
func TestKanbanDoor_APersonsNamedDropStaysNamed(t *testing.T) {
	t.Parallel()
	move := func(r *kanbanCostRig, label, dest string) (*BinMoveResult, error) {
		return r.eng.CreateBinMove(BinMoveRequest{Selection: BinSelectionByLabel, BinLabel: label, DestNodeName: dest, StationID: "kc-page"})
	}
	t.Run("the group they named", func(t *testing.T) {
		t.Parallel()
		r := newKanbanCostRig(t, 2, 2, 2)
		_, err := r.db.Exec(`DELETE FROM bins WHERE label = 'KC-K-S-1'`)
		testutil.MustNoErr(t, err, "K short of small")
		r.eng.KanbanKeeper().Tick()
		res, err := move(r, "KC-W2-EMPTY", r.k.Name)
		testutil.MustNoErr(t, err, "a person's move")
		got, err := r.db.GetOrder(res.OrderID)
		testutil.MustNoErr(t, err, "the move")
		if !r.inGroup(t, got.DeliveryNode, r.k) {
			t.Errorf("a person's move to %s is %s to %s, want it into the group they named", r.k.Name, got.Status, got.DeliveryNode)
		}
	})
	t.Run("waiting on the group", func(t *testing.T) {
		t.Parallel()
		// The door's plan settles a person's group at birth, so a move waiting on one
		// comes from a slot lost since. Station-unaware is what says a person's page.
		r := newKanbanCostRig(t, 1, 2, 2)
		testdb.KanbanOverflow(t, r.db, r.k.ID, "")
		o := testdb.CreateOrder(t, r.db, func(o *orders.Order) {
			o.EdgeUUID = "kd6"
			o.StationID = "kc-page"
			o.OrderType = dispatch.OrderTypeMove
			o.SourceIntent = dispatch.SourceIntentLocal
			o.OriginClass = protocol.OriginClassNoDemand
			o.StationUnaware = true
			o.SourceNode = r.windows[1].Name
			o.DeliveryNode = r.k.Name
			o.NamedBinID = &r.push
		})
		testutil.MustNoErr(t, reservations.Acquire(r.db.DB, o.ID, o.ID, r.push, "test"), "the move holds its cart")
		testdb.HoldCart(t, r.db, o.ID, r.push)
		r.eng.fulfillment.RunOnce()
		r.parksNamedAtK(t, o)
	})
	t.Run("at its level", func(t *testing.T) {
		t.Parallel()
		r := newKanbanCostRig(t, 1, 2, 2)
		if res, err := move(r, "KC-W2-EMPTY", r.k.Name); err == nil {
			t.Errorf("a person's move to a whole %s was taken: %+v; want it refused, not sent to a short source", r.k.Name, res)
		}
	})
	t.Run("a slot", func(t *testing.T) {
		t.Parallel()
		r := newKanbanCostRig(t, 1, 2, 2)
		res, err := move(r, "KC-W2-EMPTY", r.k.Name+"-P8")
		testutil.MustNoErr(t, err, "a person's move to a slot")
		got, err := r.db.GetOrder(res.OrderID)
		testutil.MustNoErr(t, err, "the move")
		if got.DeliveryNode != r.k.Name+"-P8" {
			t.Errorf("a person's move to a slot went to %s", got.DeliveryNode)
		}
	})
}

// TestKanbanDoor_ARobotUnderTheCartIsNotRedirected: a push a robot has keeps its drop
// in K though K is whole and a source short. Characterization: the doors ask only the
// acquiring set, and this holds the chooser to that.
func TestKanbanDoor_ARobotUnderTheCartIsNotRedirected(t *testing.T) {
	t.Parallel()
	r := newKanbanCostRig(t, 1, 2, 2)
	bin := r.push
	moving := testdb.CreateOrder(t, r.db, func(o *orders.Order) {
		o.EdgeUUID = "kd7"
		o.StationID = "kc-edge"
		o.OrderType = dispatch.OrderTypeMove
		o.SourceIntent = dispatch.SourceIntentLocal
		o.OriginClass = protocol.OriginClassNoDemand
		o.Status = protocol.StatusDispatched
		o.SourceNode = r.windows[1].Name
		o.DeliveryNode = r.k.Name + "-P8"
		o.RobotID = "AMR-KD7"
		o.NamedBinID = &bin
		o.HoldFromBirth(bin)
	})
	testdb.ClaimBinForTest(t, r.db, bin, moving.ID)
	r.eng.fulfillment.RunOnce()
	r.eng.fulfillment.RunOnce()
	if got := r.reload(t, moving); got.DeliveryNode != r.k.Name+"-P8" || got.Status != protocol.StatusDispatched {
		t.Errorf("the push a robot has is %s to %s, want dispatched, untouched", got.Status, got.DeliveryNode)
	}
}

// TestKanbanDoor_APlainSourceIsReachedOnlyAsOverflow: a source with no level is
// short of nothing: never lit, passed over at the door with room in it, and reached
// when it is the type's overflow. The card says none short.
func TestKanbanDoor_APlainSourceIsReachedOnlyAsOverflow(t *testing.T) {
	t.Parallel()
	r := newKanbanCostRig(t, 2, 2, 2)
	testdb.KanbanOverflow(t, r.db, r.k.ID, "")
	spare := r.doorZone(t, "KD8-SPARE", 4, 0, 0)
	testdb.KanbanSources(t, r.db, r.k.ID, spare.ID, r.z45.ID)

	if got := walkSaid(r.walk(t, cart("W2", "KC-SMALL", false))); got != "W2=none-short" {
		t.Errorf("walk = %q, want none-short: a plain source is never lit", got)
	}
	o := r.intake(t, "kd8", r.windows[1].Name, r.push)
	if o.DeliveryNode != r.k.Name {
		t.Fatalf("the push went to %s with no overflow set, want it left naming %s", o.DeliveryNode, r.k.Name)
	}
	r.eng.fulfillment.RunOnce()
	r.parksNamedAtK(t, o)

	testdb.KanbanOverflow(t, r.db, r.k.ID, spare.Name)
	r.eng.fulfillment.RunOnce()
	if got := r.reload(t, o); !r.inGroup(t, got.DeliveryNode, spare) {
		t.Errorf("the push is %s to %s, want it taken by %s as the overflow", got.Status, got.DeliveryNode, spare.Name)
	}
	if got := walkSaid(r.walk(t, cart("W2", "KC-SMALL", false))); got != "W2=none-short" {
		t.Errorf("walk with the plain source as overflow = %q, want still none-short", got)
	}
}

// TestKanbanDoor_TwoParkedPushesDoNotWaitOnEachOther: K short by one with two carts
// parked against it. Each counts as coming, and the keeper rightly asks nothing, so
// each reading the other would leave K short for good. The elder takes the gap and the
// younger waits behind it.
//
// Red with KanbanComingOfTypeSQL counting every other order: both stay parked.
func TestKanbanDoor_TwoParkedPushesDoNotWaitOnEachOther(t *testing.T) {
	t.Parallel()
	r := newKanbanCostRig(t, 2, 2, 2)
	testdb.KanbanOverflow(t, r.db, r.k.ID, "")
	elderBin := r.push
	youngerBin := landAt(t, r.db, r.small, r.windows[3].ID, "KD9-W4")
	elder := r.parked(t, "kd9-elder", r.windows[1].Name, elderBin)
	younger := r.parked(t, "kd9-younger", r.windows[3].Name, youngerBin)
	r.eng.fulfillment.RunOnce()
	r.parksNamedAtK(t, elder)
	r.parksNamedAtK(t, younger)

	_, err := r.db.Exec(`DELETE FROM bins WHERE label = 'KC-K-S-1'`)
	testutil.MustNoErr(t, err, "K short by one")
	r.eng.fulfillment.RunOnce()
	if got := r.reload(t, elder); !r.inGroup(t, got.DeliveryNode, r.k) {
		t.Errorf("the elder push is %s to %s (%q), want it into %s", got.Status, got.DeliveryNode, got.QueueCause, r.k.Name)
	}
	r.parksNamedAtK(t, younger)
}

// TestKanbanDoor_AgreesWithTheKeepersCounts: the door's one statement says what the
// keeper's batched reads say of the same groups, asks and comings included.
func TestKanbanDoor_AgreesWithTheKeepersCounts(t *testing.T) {
	t.Parallel()
	r := newKanbanCostRig(t, 1, 2, 2)
	_, err := r.db.Exec(`DELETE FROM bins WHERE label = 'KC-K-S-1'`)
	testutil.MustNoErr(t, err, "K short of small")
	m := r.eng.KanbanKeeper()
	m.Tick()
	// One push aimed into Z45: a coming of the source's.
	kids, err := r.db.ListChildNodes(r.z45.ID)
	testutil.MustNoErr(t, err, "Z45's children")
	coming := r.push
	testdb.CreateOrder(t, r.db, func(o *orders.Order) {
		o.EdgeUUID = "kd10"
		o.StationID = "kc-edge"
		o.OrderType = dispatch.OrderTypeMove
		o.SourceNode = r.windows[1].Name
		o.DeliveryNode = kids[len(kids)-1].Name
		o.NamedBinID = &coming
	})

	door, err := r.db.ReadKanbanDoor(r.k.ID, r.small, r.windows[1].Name, 0)
	testutil.MustNoErr(t, err, "the door")
	if door == nil {
		t.Fatal("the door read nothing for a window of the unloader whose outbound is K")
	}
	episodes, err := r.db.ListOpenKeeperEpisodes()
	testutil.MustNoErr(t, err, "the episodes")
	asked := map[string]int{}
	for _, e := range episodes {
		asked[e.EpisodeKey] = e.Asked
	}
	counts, err := r.db.ReadKanbanCounts([]int64{r.k.ID, r.z45.ID})
	testutil.MustNoErr(t, err, "the counts")
	picture, err := r.db.ListKanbanPicture([]int64{r.k.ID, r.z45.ID})
	testutil.MustNoErr(t, err, "the picture")
	for _, line := range append([]store.DoorLine{door.Kanban}, door.Sources...) {
		for _, row := range picture {
			if row.KanbanNodeID != line.Group.ID || row.BinTypeID != r.small {
				continue
			}
			lv := row.KanbanLevel()
			want := counts.Level(lv, asked[protocol.KanbanEpisodeKey(line.Group.Name, lv.BinTypeCode)])
			if got := line.Short(protocol.CarrierEmpty); got != want.Short() || line.Resident != want.Resident ||
				line.Asked != want.Asked || line.Coming != want.Coming || line.Level.Want != lv.Want {
				t.Errorf("%s: door short %d (resident %d asked %d coming %d want %d), keeper short %d (resident %d asked %d coming %d want %d)",
					line.Group.Name, got, line.Resident, line.Asked, line.Coming, line.Level.Want,
					want.Short(), want.Resident, want.Asked, want.Coming, lv.Want)
			}
		}
	}
	if door.Kanban.Asked != 1 || door.Sources[0].Coming != 1 {
		t.Errorf("fixture drift: K asked %d, Z45 coming %d, want 1 and 1", door.Kanban.Asked, door.Sources[0].Coming)
	}
	if off, err := r.db.ReadKanbanDoor(r.k.ID, r.small, "KC-NOT-A-WINDOW", 0); err != nil || off != nil {
		t.Errorf("a source that is no window: door %+v, err %v; want nothing", off, err)
	}
}

// TestKanbanDoor_ACartGoingToASourceIsWalkedByNoGroup: a cart whose push already aims
// at a source chose at its CLEAR. It stands on its window until a robot lifts it, and
// the walk asks nothing of it, K's pass included, though K is short of its type.
func TestKanbanDoor_ACartGoingToASourceIsWalkedByNoGroup(t *testing.T) {
	t.Parallel()
	r := newKanbanCostRig(t, 1, 2, 2)
	kids, err := r.db.ListChildNodes(r.z45.ID)
	testutil.MustNoErr(t, err, "Z45's children")
	testdb.CreateOrder(t, r.db, func(o *orders.Order) {
		o.EdgeUUID = "kd11"
		o.StationID = "kc-edge"
		o.OrderType = dispatch.OrderTypeMove
		o.SourceNode = r.windows[1].Name
		o.DeliveryNode = kids[len(kids)-1].Name
		o.NamedBinID = &r.push
	})
	_, err = r.db.Exec(`DELETE FROM bins WHERE label = 'KC-K-S-1'`)
	testutil.MustNoErr(t, err, "K short of small")
	row := cart("W2", "KC-SMALL", false)
	row.BinID = r.push
	if got := walkSaid(r.walk(t, row)); got != "" {
		t.Errorf("walk = %q, want nothing asked of a cart on its way into a source", got)
	}
}

// TestKanbanDoor_ACancelledPushReturnsToTheShortSource: the cancel-return's store takes
// the chooser for a cart a cancelled window's push left on a deck.
func TestKanbanDoor_ACancelledPushReturnsToTheShortSource(t *testing.T) {
	t.Parallel()
	r := newKanbanCostRig(t, 1, 2, 2)
	testdb.KanbanOverflow(t, r.db, r.k.ID, "")
	bin, err := r.db.GetBin(r.push)
	testutil.MustNoErr(t, err, "the cart")
	carrier := &orders.Order{SourceIntent: dispatch.SourceIntentLocal, SourceNode: r.windows[1].Name, NamedBinID: &r.push}
	slot, why := r.eng.storeInto(r.k, bin, carrier)
	if slot == nil || !r.inGroup(t, slot.Name, r.z45) {
		t.Errorf("the return took %+v (%s), want a position of %s", slot, why, r.z45.Name)
	}
	if !strings.Contains(why, r.z45.Name) {
		t.Errorf("the audit note %q does not name the group", why)
	}
}

// TestOverflow_ATypesOwnOverflowOutranksTheKanbansDefault: for a pusher that is no
// window's, on the cap's path. One type's overflow is its row's, another's the default.
func TestOverflow_ATypesOwnOverflowOutranksTheKanbansDefault(t *testing.T) {
	t.Parallel()
	r := newKanbanCostRig(t, 1, 2, 2)
	_, err := r.db.Exec(`DELETE FROM bins WHERE label = 'KC-Z32-1'`)
	testutil.MustNoErr(t, err, "Z32 has room")
	z32 := r.z32.ID
	testutil.MustNoErr(t, r.db.SaveKanban(store.KanbanChange{NodeID: r.k.ID, EnsureRow: true,
		Levels: []store.KanbanLevelChange{{BinTypeID: r.t32, Overflow: &z32}}}, "test"), "32x30 overflows to Z32")
	line := &nodes.Node{Name: "KD12-LINE", Enabled: true}
	testutil.MustNoErr(t, r.db.CreateNode(line), "a line")
	own := landAt(t, r.db, r.t32, line.ID, "KD12-32")
	o := r.intakeFrom(t, "kd12a", line.Name, own)
	if !r.inGroup(t, o.DeliveryNode, r.z32) {
		t.Errorf("a 32x30 at K at its level went to %s, want its own overflow %s", o.DeliveryNode, r.z32.Name)
	}
	second := &nodes.Node{Name: "KD12-LINE2", Enabled: true}
	testutil.MustNoErr(t, r.db.CreateNode(second), "a line")
	small := landAt(t, r.db, r.small, second.ID, "KD12-S")
	o = r.intakeFrom(t, "kd12b", second.Name, small)
	if !r.inGroup(t, o.DeliveryNode, r.z45) {
		t.Errorf("a small at K at its level went to %s, want the default %s", o.DeliveryNode, r.z45.Name)
	}
}

// intakeFrom is the wire's move at K from a node that is no window.
func (r *kanbanCostRig) intakeFrom(t *testing.T, uuid, source string, bin int64) *orders.Order {
	t.Helper()
	return r.intake(t, uuid, source, bin)
}
