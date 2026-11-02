//go:build docker

package engine

import (
	"fmt"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/dispatch/binresolver"
	"shingocore/dispatch/plan"
	"shingocore/domain"
	"shingocore/internal/testdb"
	"shingocore/kanban"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// kanban_pins_v5_docker_test.go — the loop at its doors, the walk's lit type in the keeper's
// count, the episode's key, and a system with no unloader at all. Taken before the shape
// changes. Predictions: PINS.md, "BRIEF v5".

// wholeK is the cost rig with K whole and no sources: nothing but K can take a cart.
func wholeK(t *testing.T) *kanbanCostRig {
	t.Helper()
	r := newKanbanCostRig(t, 2, 2, 2)
	testdb.KanbanSources(t, r.db, r.k.ID)
	return r
}

// pinFamilyOf groups the rig's types, where the table exists to say so.
func pinFamilyOf(t *testing.T, db *store.DB, family string, ids ...int64) {
	t.Helper()
	if _, err := db.DB.Exec(`INSERT INTO carrier_families (code) VALUES ($1) ON CONFLICT DO NOTHING`, family); err != nil {
		t.Logf("no family table at this tree: %v", err)
		return
	}
	for _, id := range ids {
		_, err := db.DB.Exec(`UPDATE bin_types SET family_id = (SELECT id FROM carrier_families WHERE code = $1) WHERE id = $2`, family, id)
		testutil.MustNoErr(t, err, "family")
	}
}

// TestKanbanPin_ALoopCartLandsWithASlotFree: K whole of small, slots free, no source, no
// A cart of the loop lands, at every door a push meets. The level governs the
// keeper's asks only: this is the wedge.
func TestKanbanPin_ALoopCartLandsWithASlotFree(t *testing.T) {
	t.Parallel()
	t.Run("intake", func(t *testing.T) {
		t.Parallel()
		r := wholeK(t)
		o := r.intake(t, "loop-intake", r.windows[1].Name, r.push)
		if !r.inGroup(t, o.DeliveryNode, r.k) {
			t.Errorf("the push is %s, named at %s (%s), want a slot of K", o.Status, o.DeliveryNode, o.QueueCause)
		}
	})
	t.Run("the scanner's arm", func(t *testing.T) {
		t.Parallel()
		r := wholeK(t)
		o := r.parked(t, "loop-pass", r.windows[1].Name, r.push)
		r.eng.fulfillment.RunOnce()
		got := r.reload(t, o)
		if !r.inGroup(t, got.DeliveryNode, r.k) || got.QueueCause == string(dispatch.CauseNGRPAtLevel) {
			t.Errorf("the push is %s to %s (%s), want dispatched into K", got.Status, got.DeliveryNode, got.QueueCause)
		}
	})
	t.Run("a cancelled push", func(t *testing.T) {
		t.Parallel()
		r := wholeK(t)
		o := r.parked(t, "loop-cancel", r.windows[1].Name, r.push)
		bin, err := r.db.GetBin(r.push)
		testutil.MustNoErr(t, err, "the cart")
		c := r.eng.dispatcher.ChooseCancelledPush(cartPickNode(o, r.push), r.k, binresolver.KnownCarrier(bin))
		if !c.Applies || c.Slot == nil {
			t.Errorf("the return: %+v, want a slot of K", c)
		}
	})
	t.Run("a cart off a slot of K", func(t *testing.T) {
		t.Parallel()
		r := wholeK(t)
		kids, err := r.db.ListChildNodes(r.k.ID)
		testutil.MustNoErr(t, err, "K's children")
		var from string
		var cart int64
		for _, c := range kids {
			if on, err := r.db.ListBinsByNode(c.ID); err == nil && len(on) > 0 && on[0].BinTypeID == r.small {
				from, cart = c.Name, on[0].ID
			}
		}
		o := r.parked(t, "loop-slot", r.windows[1].Name, r.push)
		o.SourceNode = from
		bin, err := r.db.GetBin(cart)
		testutil.MustNoErr(t, err, "the cart")
		c := r.eng.dispatcher.ChooseCancelledPush(cartPickNode(o, cart), r.k, binresolver.KnownCarrier(bin))
		if !c.Applies || c.Slot == nil {
			t.Errorf("a cancelled leg off K's own slot: %+v, want a slot of K", c)
		}
	})
}

// TestKanbanPin_AStrangerStillMeetsTheCap: the cap stays for a cart from outside the loop.
func TestKanbanPin_AStrangerStillMeetsTheCap(t *testing.T) {
	t.Parallel()
	r := wholeK(t)
	away := &nodes.Node{Name: "KC-AWAY", Enabled: true}
	testutil.MustNoErr(t, r.db.CreateNode(away), "the node")
	cart := landAt(t, r.db, r.small, away.ID, "KC-STRANGER")
	o := r.intakeFrom(t, "stranger", away.Name, cart)
	if r.inGroup(t, o.DeliveryNode, r.k) {
		t.Errorf("a stranger took %s with K whole of it", o.DeliveryNode)
	}
}

// stubDoor answers the door's read and nothing else: what it says decides.
type stubDoor struct {
	*store.DB
	door *nodes.KanbanDoor
}

func (s stubDoor) ReadKanbanDoor(int64, int64, string, int64) (*nodes.KanbanDoor, error) {
	return s.door, nil
}

// pinLoopLine is K's line with K whole of the type, in the verdict the door carries.
func pinLoopLine(r *kanbanCostRig) *nodes.KanbanDoor {
	lv := store.KanbanLevel{GroupNodeID: r.k.ID, BinTypeID: r.small, BinTypeCode: "KC-SMALL", Want: 1, Base: 1}
	return &nodes.KanbanDoor{Loop: true, Kanban: nodes.DoorLine{Group: r.k, Level: &lv, Have: kanban.Have{Resident: 1}, FreeSlots: 1}}
}

// TestKanbanPin_NoDoorReDerivesTheVerdict: the door's read says the cart is the loop's, from a
// source no SQL would call the loop's, with K whole. It lands: the chooser's slot pick does not
// ask again. Nil from the read means nothing else is asked, and the door keeps the cap's path.
func TestKanbanPin_NoDoorReDerivesTheVerdict(t *testing.T) {
	t.Parallel()
	r := wholeK(t)
	bin, err := r.db.GetBin(r.push)
	testutil.MustNoErr(t, err, "the cart")
	ask := plan.PushAsk{Group: r.k, Source: "KC-NOT-THE-LOOP", Stated: binresolver.KnownCarrier(bin), Asker: domain.Anyone}

	stub := stubDoor{DB: r.db, door: pinLoopLine(r)}
	resolver := &dispatch.DefaultResolver{DB: stub}
	p := plan.NewPlanner(stub, nil, resolver, func(string, ...any) {})
	if c := p.ChoosePush(ask); !c.Applies || c.Slot == nil {
		t.Errorf("the loop's verdict with K whole: %+v, want a slot of K", c)
	}
	stub.door = nil
	p = plan.NewPlanner(stub, nil, resolver, func(string, ...any) {})
	if c := p.ChoosePush(ask); c.Applies {
		t.Errorf("no door: %+v, want the cap's path", c)
	}
}

// TestKanbanPin_AWindowCartCountsAsWhatItIsLit: K short one totes, a small standing at a
// window that the walk lights totes. It comes back as totes, so nothing is asked outside.
func TestKanbanPin_AWindowCartCountsAsWhatItIsLit(t *testing.T) {
	t.Parallel()
	r := newKanbanCostRig(t, 2, 2, 2)
	pinFamilyOf(t, r.db, "KC-45", r.small, r.big, r.totes)
	testutil.MustNoErr(t, r.db.SetKanbanLevel(store.KanbanLevel{GroupNodeID: r.k.ID, BinTypeID: r.totes, Want: 2}), "totes short")
	m := r.eng.KanbanKeeper()
	m.Tick()
	st := mntState(t, m, "KC-K", "KC-TOTES")
	if st.Gap > 0 || st.Created != 0 {
		t.Errorf("K's totes = gap %d, created %d; want the window's small counted as what it is lit: %+v", st.Gap, st.Created, st)
	}
}

// TestKanbanPin_RenamingATypeMidEpisodeAsksNothingNew: bins.UpdateType rewrites a code in place.
// The episode's key does not name it, so the open episode goes on matching its asks.
func TestKanbanPin_RenamingATypeMidEpisodeAsksNothingNew(t *testing.T) {
	t.Parallel()
	r := newKanbanCostRig(t, 2, 2, 2)
	_, err := r.db.Exec(`DELETE FROM bins WHERE label IN ('KC-W1-FULL', 'KC-W2-EMPTY', 'KC-W3-EMPTY')`)
	testutil.MustNoErr(t, err, "clear the windows")
	testutil.MustNoErr(t, r.db.SetKanbanLevel(store.KanbanLevel{GroupNodeID: r.k.ID, BinTypeID: r.small, Want: 2}), "short one")
	m := r.eng.KanbanKeeper()
	m.Tick()
	if st := mntState(t, m, "KC-K", "KC-SMALL"); st.Created != 1 {
		t.Fatalf("fixture drift: the tick asked %d: %+v", st.Created, st)
	}
	bt, err := r.db.GetBinType(r.small)
	testutil.MustNoErr(t, err, "the type")
	bt.Code = "KC-SMALLER"
	testutil.MustNoErr(t, r.db.UpdateBinType(bt), "the rename")
	m.Tick()
	var live int
	testutil.MustNoErr(t, r.db.QueryRow(`SELECT count(*) FROM orders WHERE edge_uuid LIKE 'core-mnt-%' AND status NOT IN (`+
		protocol.TerminalStatusSQLList()+`)`).Scan(&live), "asks")
	if live != 1 {
		t.Errorf("%d live asks after the rename, want the one", live)
	}
}

// TestKanbanPin_TheSecondSystem: a kanban fed from a plain supermarket with no unloader at all.
// It ticks, asks, closes and shows its line with no code of its own: a plant makes one with
// rows alone. Green at the tree, and stays so.
func TestKanbanPin_TheSecondSystem(t *testing.T) {
	t.Parallel()
	r := newKanbanCostRig(t, 2, 2, 2)
	testutil.MustNoErr(t, r.db.SetKanbanLevel(store.KanbanLevel{GroupNodeID: r.k2.ID, BinTypeID: r.small, Want: 2}), "K2 short")
	m := r.eng.KanbanKeeper()
	m.Tick()
	st := mntState(t, m, "KC-K2", "KC-SMALL")
	if st.Created != 1 || st.OriginID == "" {
		t.Fatalf("the ask: %+v", st)
	}
	kids, err := r.db.ListChildNodes(r.k2.ID)
	testutil.MustNoErr(t, err, "the slots")
	id := kids[len(kids)-1].ID
	b := &bins.Bin{BinTypeID: r.small, Label: fmt.Sprintf("KC-K2-ARRIVED-%d", id), NodeID: &id, Status: "available"}
	testutil.MustNoErr(t, r.db.CreateBin(b), "it arrives")
	_, err = r.db.DB.Exec(`UPDATE orders SET status = 'confirmed' WHERE edge_uuid LIKE 'core-mnt-%'`)
	testutil.MustNoErr(t, err, "the ask is done")
	m.Tick()
	st = mntState(t, m, "KC-K2", "KC-SMALL")
	if st.OriginID != "" || st.Gap > 0 || st.Resident != 2 {
		t.Errorf("after it landed: %+v, want closed and whole", st)
	}
}
