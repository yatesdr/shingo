//go:build docker

package engine

import (
	"fmt"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/dispatch/binresolver"
	"shingocore/domain"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// kanban_door_docker_test.go — the one chooser at its doors and in the walk
// (plan.Planner.ChoosePush, kanban.AssignAcross), on the cost rig's shape: K whole,
// sources set per test: home first, then sources by rank; the family; a person's drop; a robot;
// a source with no level.

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
	slots := 0
	for _, row := range picture {
		levels[row.KanbanNodeID] = append(levels[row.KanbanNodeID], row.KanbanLevel())
		if row.KanbanNodeID == r.k.ID {
			sources = row.Sources
			slots = row.Slots
		}
	}
	ask := store.GroupLineAsk{Group: r.k, Levels: levels[r.k.ID], Windows: rows, Slots: slots}
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

// fullK stands a big on every free child of K: no slot left for any cart.
func (r *kanbanCostRig) fullK(t *testing.T) {
	t.Helper()
	kids, err := r.db.ListChildNodes(r.k.ID)
	testutil.MustNoErr(t, err, "K's children")
	for i, c := range kids {
		if n, err := r.db.CountBinsByNode(c.ID); err != nil {
			t.Fatal(err)
		} else if n == 0 {
			landAt(t, r.db, r.big, c.ID, fmt.Sprintf("KD-FILL-%d", i))
		}
	}
}

// TestKanbanDoor_ALoopCartLandsAtHomeFirst: K whole of small, its sources short of it. The
// level governs the keeper's asks and nothing else, so home takes the cart, and the walk does
// not say a source is who it is for.
func TestKanbanDoor_ALoopCartLandsAtHomeFirst(t *testing.T) {
	t.Parallel()
	r := newKanbanCostRig(t, 1, 2, 2)
	o := r.intake(t, "kd0", r.windows[1].Name, r.push)
	if !r.inGroup(t, o.DeliveryNode, r.k) {
		t.Errorf("the push went to %s, want a slot of K", o.DeliveryNode)
	}
	if got := walkSaid(r.walk(t, cart("W2", "KC-SMALL", false))); got != "W2=none-short" {
		t.Errorf("walk = %q, want none-short: it lands at home", got)
	}
}

// TestKanbanDoor_TheFirstSourceShortInRankOrder: two sources, K with no slot left. A cart
// arriving goes to the first source in rank order short of its type, not the shortest, at the
// door, on a parked pass and in the walk.
func TestKanbanDoor_TheFirstSourceShortInRankOrder(t *testing.T) {
	t.Parallel()
	setup := func(t *testing.T) (*kanbanCostRig, *nodes.Node) {
		r := newKanbanCostRig(t, 2, 2, 2)
		first := r.doorZone(t, "KD1-FIRST", 4, 2, 1)
		second := r.doorZone(t, "KD1-SECOND", 4, 2, 0)
		testdb.KanbanSources(t, r.db, r.k.ID, r.z45.ID, first.ID, second.ID)
		r.fullK(t)
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

// TestKanbanDoor_TheOutboundFull: K with no slot left, so the next source short; none short,
// so it parks named at K, visible, as a full group does, and never at a level.
func TestKanbanDoor_TheOutboundFull(t *testing.T) {
	t.Parallel()
	t.Run("the next source short", func(t *testing.T) {
		t.Parallel()
		r := newKanbanCostRig(t, 1, 2, 2)
		r.fullK(t)
		o := r.intake(t, "kd3a", r.windows[1].Name, r.push)
		if !r.inGroup(t, o.DeliveryNode, r.z45) {
			t.Errorf("the push went to %s, want the next source short, %s", o.DeliveryNode, r.z45.Name)
		}
	})
	t.Run("parked, named at the kanban", func(t *testing.T) {
		t.Parallel()
		r := newKanbanCostRig(t, 2, 2, 2)
		r.fullK(t)
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

// TestKanbanDoor_AnotherFamilyIsNoSourceOfTheCart: a 45x48 cart is never sent to, or lit for, a
// 32x30 source short of its own type, and a 32x30 cart passes over a 45x48 source to its own.
func TestKanbanDoor_AnotherFamilyIsNoSourceOfTheCart(t *testing.T) {
	t.Parallel()
	setup := func(t *testing.T, residentsZ45 int, sources ...func(*kanbanCostRig) int64) *kanbanCostRig {
		r := newKanbanCostRig(t, residentsZ45, 2, 2)
		pinFamilyOf(t, r.db, "KC-45", r.small, r.big, r.totes)
		var ids []int64
		for _, s := range sources {
			ids = append(ids, s(r))
		}
		testdb.KanbanSources(t, r.db, r.k.ID, ids...)
		_, err := r.db.Exec(`DELETE FROM bins WHERE label = 'KC-Z32-1'`)
		testutil.MustNoErr(t, err, "Z32 short of its own")
		r.fullK(t)
		return r
	}
	z45 := func(r *kanbanCostRig) int64 { return r.z45.ID }
	z32 := func(r *kanbanCostRig) int64 { return r.z32.ID }
	t.Run("a 45x48 cart", func(t *testing.T) {
		t.Parallel()
		r := setup(t, 2, z32, z45)
		if got := walkSaid(r.walk(t, cart("W2", "KC-SMALL", false))); got != "W2=none-short" {
			t.Errorf("walk = %q, want none-short: Z32 is short, of nothing this cart can be", got)
		}
		o := r.intake(t, "kd5a", r.windows[1].Name, r.push)
		if o.DeliveryNode != r.k.Name {
			t.Errorf("the push went to %s, want it left naming K", o.DeliveryNode)
		}
	})
	t.Run("a 32x30 cart", func(t *testing.T) {
		t.Parallel()
		r := setup(t, 1, z45, z32)
		if got := walkSaid(r.walk(t, cart("W4", "KC-32X30", false))); got != "W4=KC-32X30@KC-Z32" {
			t.Errorf("walk = %q, want the 32x30 source lit: Z45 is short, of no type of this family", got)
		}
		bin := landAt(t, r.db, r.t32, r.windows[3].ID, "KD5-W4")
		o := r.intake(t, "kd5b", r.windows[3].Name, bin)
		if !r.inGroup(t, o.DeliveryNode, r.z32) {
			t.Errorf("the push went to %s, want %s", o.DeliveryNode, r.z32.Name)
		}
	})
}

// TestKanbanDoor_APersonsNamedDropStaysNamed: a person's move to K off the same window goes
// where they named: the cap's answer, K. Core does not re-choose what somebody chose. With K at
// its level the door refuses them to their face, and sends them nowhere else. A slot named
// directly is no group, and no door asks.
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
// in K though a source is short. Characterization: the doors ask only the acquiring set, and
// this holds the chooser to that.
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

// TestKanbanDoor_APlainSourceIsNeverLit: a source with no level is short of nothing: never lit,
// and passed over at the door with room in it. The push parks visible, and the card says none
// short.
func TestKanbanDoor_APlainSourceIsNeverLit(t *testing.T) {
	t.Parallel()
	r := newKanbanCostRig(t, 2, 2, 2)
	spare := r.doorZone(t, "KD8-SPARE", 4, 0, 0)
	testdb.KanbanSources(t, r.db, r.k.ID, spare.ID, r.z45.ID)
	r.fullK(t)
	if got := walkSaid(r.walk(t, cart("W2", "KC-SMALL", false))); got != "W2=none-short" {
		t.Errorf("walk = %q, want none-short: a plain source is never lit", got)
	}
	o := r.intake(t, "kd8", r.windows[1].Name, r.push)
	if o.DeliveryNode != r.k.Name {
		t.Fatalf("the push went to %s, want it left naming %s", o.DeliveryNode, r.k.Name)
	}
}

// TestKanbanDoor_TwoParkedPushesDoNotWaitOnEachOther: K with no slot left and a source short by
// one, with two carts parked against it. Each counts as coming to the source once it is
// aimed, so each reading the other would leave it short for good. The elder takes the gap and the
// younger waits behind it.
func TestKanbanDoor_TwoParkedPushesDoNotWaitOnEachOther(t *testing.T) {
	t.Parallel()
	r := newKanbanCostRig(t, 1, 2, 2)
	r.fullK(t)
	elderBin := r.push
	youngerBin := landAt(t, r.db, r.small, r.windows[3].ID, "KD9-W4")
	elder := r.parked(t, "kd9-elder", r.windows[1].Name, elderBin)
	younger := r.parked(t, "kd9-younger", r.windows[3].Name, youngerBin)
	r.eng.fulfillment.RunOnce()
	if got := r.reload(t, elder); !r.inGroup(t, got.DeliveryNode, r.z45) {
		t.Errorf("the elder push is %s to %s (%q), want it into %s", got.Status, got.DeliveryNode, got.QueueCause, r.z45.Name)
	}
	if got := r.reload(t, younger); got.DeliveryNode != r.k.Name {
		t.Errorf("the younger push is %s to %s (%q), want it waiting named at K", got.Status, got.DeliveryNode, got.QueueCause)
	}
}

// TestKanbanDoor_AgreesWithTheKeepersCounts: the door's one statement says what the keeper's read
// says of a source, asks and comings included. Unsupported, so it has no line to hold.
func TestKanbanDoor_AgreesWithTheKeepersCounts(t *testing.T) {
	t.Parallel()
	r := newKanbanCostRig(t, 1, 2, 2)
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
	if door == nil || !door.Loop {
		t.Fatalf("the door = %+v for a window of the unloader whose outbound is K", door)
	}
	counts, err := r.db.ReadKanbanCounts([]int64{r.z45.ID})
	testutil.MustNoErr(t, err, "the counts")
	picture, err := r.db.ListKanbanPicture([]int64{r.z45.ID})
	testutil.MustNoErr(t, err, "the picture")
	for _, line := range door.Sources {
		for _, row := range picture {
			if row.KanbanNodeID != line.Group.ID || row.BinTypeID != r.small {
				continue
			}
			lv := row.KanbanLevel()
			want := counts.Level(lv, 0)
			if got := line.Short(protocol.CarrierEmpty); got != want.Short() || line.Resident != want.Resident ||
				line.Coming != want.Coming || line.Level.Want != lv.Want {
				t.Errorf("%s: door short %d (resident %d coming %d want %d), keeper short %d (resident %d coming %d want %d)",
					line.Group.Name, got, line.Resident, line.Coming, line.Level.Want,
					want.Short(), want.Resident, want.Coming, lv.Want)
			}
		}
	}
	if door.Sources[0].Coming != 1 {
		t.Errorf("fixture drift: Z45 coming %d, want 1", door.Sources[0].Coming)
	}
	if off, err := r.db.ReadKanbanDoor(r.k.ID, r.small, "KC-NOT-THE-LOOP", 0); err != nil || off != nil {
		t.Errorf("a source that is no part of the loop: door %+v, err %v; want nothing", off, err)
	}
}

// TestKanbanDoor_ACartGoingToASourceIsWalkedByNoGroup: a cart whose push already aims at a source
// chose at its CLEAR. It stands on its window until a robot lifts it, and the walk asks nothing of
// it, K's pass included, though K is short of its type.
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

// TestKanbanDoor_ACancelledPushReturnsToTheShortSource: the cancel-return's store takes the chooser
// for a cart a cancelled window's push left on a deck: home with a slot, else the source short.
func TestKanbanDoor_ACancelledPushReturnsToTheShortSource(t *testing.T) {
	t.Parallel()
	r := newKanbanCostRig(t, 1, 2, 2)
	bin, err := r.db.GetBin(r.push)
	testutil.MustNoErr(t, err, "the cart")
	carrier := &orders.Order{SourceIntent: dispatch.SourceIntentLocal, SourceNode: r.windows[1].Name, NamedBinID: &r.push}
	if slot, why := r.eng.storeInto(r.k, bin, carrier); slot == nil || !r.inGroup(t, slot.Name, r.k) {
		t.Errorf("the return took %+v (%s), want a slot of K", slot, why)
	}
	r.fullK(t)
	slot, why := r.eng.storeInto(r.k, bin, carrier)
	if slot == nil || !r.inGroup(t, slot.Name, r.z45) {
		t.Errorf("the return took %+v (%s), want a position of %s", slot, why, r.z45.Name)
	}
	if !strings.Contains(why, r.z45.Name) {
		t.Errorf("the audit note %q does not name the group", why)
	}
}

// intakeFrom is the wire's move at K from a node that is no window.
func (r *kanbanCostRig) intakeFrom(t *testing.T, uuid, source string, bin int64) *orders.Order {
	t.Helper()
	return r.intake(t, uuid, source, bin)
}

// doorBudget is a door's statements, at every state it can reach.
const doorBudget = 8

// groupFreeSlotsAdmitOnly narrows a group's free slots to one type, so none takes the cart.
func (r *kanbanCostRig) groupFreeSlotsAdmitOnly(t *testing.T, g *nodes.Node, typeID int64) {
	t.Helper()
	kids, err := r.db.ListChildNodes(g.ID)
	testutil.MustNoErr(t, err, "the slots")
	for _, kid := range kids {
		n, err := r.db.CountBinsByNode(kid.ID)
		testutil.MustNoErr(t, err, "the slot's carts")
		if n == 0 {
			testutil.MustNoErr(t, r.db.SetNodeBinTypes(kid.ID, []int64{typeID}), "the slot's types")
		}
	}
}

// TestKanbanDoor_EveryStateIsInBudget: the door is one read and one fit, wherever the cart goes.
// The read says which line has a slot for it, so a line with none costs nothing to pass.
func TestKanbanDoor_EveryStateIsInBudget(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(*testing.T, *kanbanCostRig) *nodes.Node
	}{
		{"home", func(*testing.T, *kanbanCostRig) *nodes.Node { return nil }},
		{"K admits none, a source short", func(t *testing.T, r *kanbanCostRig) *nodes.Node {
			first := r.doorZone(t, "KD9-FIRST", 4, 2, 1)
			testdb.KanbanSources(t, r.db, r.k.ID, first.ID)
			r.groupFreeSlotsAdmitOnly(t, r.k, r.t32)
			return first
		}},
		{"K and the first source admit none, the second short", func(t *testing.T, r *kanbanCostRig) *nodes.Node {
			first := r.doorZone(t, "KD9-FIRST", 4, 2, 1)
			second := r.doorZone(t, "KD9-SECOND", 4, 2, 0)
			testdb.KanbanSources(t, r.db, r.k.ID, first.ID, second.ID)
			r.groupFreeSlotsAdmitOnly(t, r.k, r.t32)
			r.groupFreeSlotsAdmitOnly(t, first, r.t32)
			return second
		}},
		{"K admits none, no source short", func(t *testing.T, r *kanbanCostRig) *nodes.Node {
			whole := r.doorZone(t, "KD9-WHOLE", 4, 1, 1)
			testdb.KanbanSources(t, r.db, r.k.ID, whole.ID)
			r.groupFreeSlotsAdmitOnly(t, r.k, r.t32)
			return nil
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newKanbanCostRig(t, 2, 2, 2)
			into := c.setup(t, r)
			r.counter.Reset()
			o := r.intake(t, "kd9", r.windows[1].Name, r.push)
			stmts := r.counter.Statements()
			door := stmts[:testutil.FirstBegin(stmts)]
			t.Logf("BUDGET door %d: into %s\n%s", len(door), o.DeliveryNode, testutil.StatementBreakdown(door))
			if into != nil && !r.inGroup(t, o.DeliveryNode, into) {
				t.Errorf("fixture drift: the push went to %s, want %s", o.DeliveryNode, into.Name)
			}
			if len(door) > doorBudget {
				t.Errorf("the door = %d statements, budget %d\n%s", len(door), doorBudget, testutil.StatementBreakdown(door))
			}
		})
	}
}

// TestKanbanDoor_ItsFreeSlotsAreTheResolversSlots: the door's count of a line's slots and the
// resolver's flat arm answer one question. A low count parks a cart with a slot standing free; a
// high one pays for a fit that fails.
func TestKanbanDoor_ItsFreeSlotsAreTheResolversSlots(t *testing.T) {
	t.Parallel()
	anOrder := func(t *testing.T, r *kanbanCostRig, uuid, delivery string) *orders.Order {
		return testdb.CreateOrder(t, r.db, func(o *orders.Order) {
			o.EdgeUUID = uuid
			o.StationID = "kc-edge"
			o.OrderType = dispatch.OrderTypeMove
			o.DeliveryNode = delivery
		})
	}
	types := func(t *testing.T, r *kanbanCostRig, node, typeID int64) {
		testutil.MustNoErr(t, r.db.SetNodeBinTypes(node, []int64{typeID}), "types")
	}
	set := func(t *testing.T, r *kanbanCostRig, s *nodes.Node, column string) {
		_, err := r.db.Exec(`UPDATE nodes SET `+column+` WHERE id = $1`, s.ID)
		testutil.MustNoErr(t, err, column)
	}
	cases := []struct {
		name  string
		admit bool
		setup func(*testing.T, *kanbanCostRig, *nodes.Node)
	}{
		{"nothing declared", true, func(*testing.T, *kanbanCostRig, *nodes.Node) {}},
		{"declared by name", true, func(t *testing.T, r *kanbanCostRig, s *nodes.Node) { types(t, r, s.ID, r.small) }},
		{"declared another", false, func(t *testing.T, r *kanbanCostRig, s *nodes.Node) { types(t, r, s.ID, r.t32) }},
		{"the group declares it", true, func(t *testing.T, r *kanbanCostRig, _ *nodes.Node) { types(t, r, r.k.ID, r.small) }},
		{"the group declares another", false, func(t *testing.T, r *kanbanCostRig, _ *nodes.Node) { types(t, r, r.k.ID, r.t32) }},
		{"mode all under a group declaring another", true, func(t *testing.T, r *kanbanCostRig, s *nodes.Node) {
			types(t, r, r.k.ID, r.t32)
			testutil.MustNoErr(t, r.db.SetNodeProperty(s.ID, "bin_type_mode", "all"), "mode")
		}},
		{"disabled", false, func(t *testing.T, r *kanbanCostRig, s *nodes.Node) { set(t, r, s, "enabled = false") }},
		{"synthetic", false, func(t *testing.T, r *kanbanCostRig, s *nodes.Node) { set(t, r, s, "is_synthetic = true") }},
		{"holds a cart", false, func(t *testing.T, r *kanbanCostRig, s *nodes.Node) { landAt(t, r.db, r.small, s.ID, "KD10-CART") }},
		{"a live order names it", false, func(t *testing.T, r *kanbanCostRig, s *nodes.Node) { anOrder(t, r, "kd10-namer", s.Name) }},
		{"claimed", false, func(t *testing.T, r *kanbanCostRig, s *nodes.Node) {
			o := anOrder(t, r, "kd10-claimer", "")
			testutil.MustNoErr(t, r.db.UpdateOrderStatus(o.ID, string(protocol.StatusDispatched), "a robot holds it"), "dispatched")
			set(t, r, s, fmt.Sprintf("claimed_by = %d", o.ID))
		}},
		{"reserved by another order", false, func(t *testing.T, r *kanbanCostRig, s *nodes.Node) {
			o := anOrder(t, r, "kd10-holder", "")
			testutil.MustNoErr(t, reservations.AcquireSlot(r.db.DB, o.ID, s.ID, "test"), "reserve")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newKanbanCostRig(t, 2, 2, 2)
			kids, err := r.db.ListChildNodes(r.k.ID)
			testutil.MustNoErr(t, err, "the slots")
			var last *nodes.Node
			for _, kid := range kids {
				n, err := r.db.CountBinsByNode(kid.ID)
				testutil.MustNoErr(t, err, "the slot's carts")
				switch {
				case n > 0:
				case last == nil:
					last = kid
				default:
					set(t, r, kid, "enabled = false")
				}
			}
			c.setup(t, r, last)

			door, err := r.db.ReadKanbanDoor(r.k.ID, r.small, r.windows[1].Name, 0)
			testutil.MustNoErr(t, err, "the door")
			gr := &binresolver.GroupResolver{DB: r.db.DoorReads(r.k.ID, &door.Kanban), Loop: true}
			res, err := gr.ResolveStore(r.k, "", binresolver.KnownBinType(r.small), domain.Anyone)
			resolved := err == nil && res.Node != nil
			if resolved != c.admit {
				t.Errorf("fixture drift: the resolver found a slot: %v (%v), want %v", resolved, err, c.admit)
			}
			if free := door.Kanban.FreeSlots > 0; free != resolved {
				t.Errorf("the door counts %d free slots; the resolver found one: %v", door.Kanban.FreeSlots, resolved)
			}
		})
	}
}

// TestKanbanDoor_ACancelledSwapsCartLands: a swap cancelled with its supply on a deck names a
// position K supports as its source, and the cart is K's loop's. It lands with K whole, as a
// cancelled push does: held there, a robot stands loaded until a person presses Recover.
func TestKanbanDoor_ACancelledSwapsCartLands(t *testing.T) {
	t.Parallel()
	r := wholeK(t)
	bin, err := r.db.GetBin(r.push)
	testutil.MustNoErr(t, err, "the cart")
	carrier := &orders.Order{OrderType: dispatch.OrderTypeComplex, Coordinated: true, SourceIntent: dispatch.SourceIntentLocal, SourceNode: "KC-P6-LF"}
	if slot, why := r.eng.storeInto(r.k, bin, carrier); slot == nil || !r.inGroup(t, slot.Name, r.k) {
		t.Errorf("the return took %+v (%s), want a slot of K", slot, why)
	}
}

// TestKanbanDoor_ACancelledCartIsAskedFromItsOwnLift: a multi-cart order's source_node names one
// of its lifts at most. This one names a node outside K's loop, and the cart's own row lifted it at
// a window of K's unloader. The door asks from the cart's lift, the loop's, so the cart lands with
// K whole; asked from the order's source it would be a stranger at K's level, and hold.
func TestKanbanDoor_ACancelledCartIsAskedFromItsOwnLift(t *testing.T) {
	t.Parallel()
	r := wholeK(t)
	bin, err := r.db.GetBin(r.push)
	testutil.MustNoErr(t, err, "the cart")
	pick, drop := 0, 1
	carrier := &orders.Order{OrderType: dispatch.OrderTypeComplex, Coordinated: true, SourceIntent: dispatch.SourceIntentLocal,
		SourceNode: "KC-ELSEWHERE",
		StepsJSON:  `[{"action":"pickup","node":"` + r.windows[1].Name + `"},{"action":"dropoff","node":"KC-P6-LF"}]`,
		Carts:      []domain.Cart{{BinID: bin.ID, PickStep: &pick, DropStep: &drop}}}
	if slot, why := r.eng.storeInto(r.k, bin, carrier); slot == nil || !r.inGroup(t, slot.Name, r.k) {
		t.Errorf("the return took %+v (%s), want a slot of K: asked from the cart's own lift", slot, why)
	}
}
