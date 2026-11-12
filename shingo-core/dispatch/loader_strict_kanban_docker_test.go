//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
	"shingocore/store/payloads"
)

// loader_strict_kanban_docker_test.go — a press kanban is for its presses. A
// loader's replenishment names no press, so on the plant-wide scan it is an
// outsider at a strict kanban, the answer it gets when it names the group; an
// open group and a kanban that is not strict still share with it, and the press
// the kanban supports is still served.

// lkRig is one part riding a carrier type of its own (the plant-wide scan sees
// only this rig's carriers), a loader window that asks for it with no source,
// and a press.
type lkRig struct {
	db            *store.DB
	d             *Dispatcher
	prefix, part  string
	bt            int64
	press, window *nodes.Node
}

func newLKRig(t *testing.T, prefix string) *lkRig {
	t.Helper()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	r := &lkRig{db: db, d: d, prefix: prefix, part: prefix + "-PART"}
	pl := &payloads.Payload{Code: r.part, UOPCapacity: 10}
	testutil.MustNoErr(t, db.CreatePayload(pl), "the part")
	bt := &bins.BinType{Code: prefix + "-BT"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "the carrier type")
	testutil.MustNoErr(t, db.SetPayloadBinTypes(pl.ID, []int64{bt.ID}), "the part rides the carrier type")
	r.bt = bt.ID
	r.press = &nodes.Node{Name: prefix + "-PRESS", Enabled: true}
	testutil.MustNoErr(t, nodes.Create(db.DB, r.press), "the press")
	r.window = &nodes.Node{Name: prefix + "-WINDOW", Enabled: true}
	testutil.MustNoErr(t, nodes.Create(db.DB, r.window), "the loader's window")
	return r
}

// group builds a group with one slot holding one empty of the rig's carrier
// type: a kanban (strict or not, supporting the press) or a plain group.
func (r *lkRig) group(t *testing.T, kanban, strict bool) int64 {
	t.Helper()
	grp, err := nodes.CreateGroup(r.db.DB, r.prefix+"-GRP")
	testutil.MustNoErr(t, err, "the group")
	if kanban {
		testdb.MarkGroup(t, r.db, grp)
		testdb.KanbanStrict(t, r.db, grp, strict)
		testutil.MustNoErr(t, nodes.SetKanbanSupports(r.db.DB, grp, []int64{r.press.ID}), "the kanban supports the press")
	}
	slot := &nodes.Node{Name: r.prefix + "-GRP-1", Enabled: true, ParentID: &grp}
	testutil.MustNoErr(t, nodes.Create(r.db.DB, slot), "the slot")
	var empty int64
	testutil.MustNoErr(t, r.db.QueryRow(`INSERT INTO bins (bin_type_id, label, node_id, status)
		VALUES ($1, $2, $3, 'available') RETURNING id`, r.bt, r.prefix+"-EMPTY", slot.ID).Scan(&empty), "the empty")
	return empty
}

// loaderAsk is the loader's replenishment that has Core search the plant
// (AnySource): one window, one empty, no source and no press. Its carrier is
// picked as the scanner picks a Core ask's, from the stored order
// (SourceFinder.FindSource, intake's call too).
func (r *lkRig) loaderAsk(t *testing.T) SourceResult {
	t.Helper()
	res, err := r.d.ReplenishLoader(ReplenishRequest{
		Origin: ReplenishByThreshold, OrderType: OrderTypeRetrieveEmpty, StationID: "edge.line1",
		LoaderID: 98, PayloadCode: r.part, Threshold: 10, CurrentUOP: 0, PerBinCapacity: 10, AnySource: true,
	}, LoaderReplenishConfig{
		Layout:    loaders.LayoutSharedWindow,
		Homes:     []loaders.Home{{PositionNodeID: r.window.ID}},
		NodeNames: map[int64]string{r.window.ID: r.window.Name},
		Payloads:  []loaders.Payload{{PayloadCode: r.part}},
	})
	testutil.MustNoErr(t, err, "ReplenishLoader")
	if len(res.Created) != 1 {
		t.Fatalf("created %d asks (held by %v, skipped %q), want 1", len(res.Created), res.HeldBy, res.Skipped)
	}
	o, err := r.db.GetOrder(res.Created[0].ID)
	testutil.MustNoErr(t, err, "read the ask back")
	if o.SourceNode != "" || o.ProcessNode != "" {
		t.Fatalf("the ask names source %q, press %q; want neither (the plant-wide scan)", o.SourceNode, o.ProcessNode)
	}
	return r.d.finder.FindSource(o, IntentEmpty)
}

func found(res SourceResult, bin int64) bool {
	return res.Outcome == OutcomeFound && res.Bin != nil && res.Bin.ID == bin
}

func TestLoaderAsk_AStrictKanbanIsThePressesNotTheLoaders(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name           string
		kanban, strict bool
		wantPicked     bool
	}{
		{"a strict kanban", true, true, false},
		{"a kanban that is not strict", true, false, true},
		{"an open group", false, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newLKRig(t, "LKA"+map[bool]string{true: "K", false: "G"}[c.kanban]+map[bool]string{true: "S", false: "O"}[c.strict])
			empty := r.group(t, c.kanban, c.strict)
			res := r.loaderAsk(t)
			if found(res, empty) != c.wantPicked {
				t.Errorf("the loader's plant-wide ask: outcome %v under %q; finds the only empty %d = %v, want %v",
					res.Outcome, res.QueueCause, empty, found(res, empty), c.wantPicked)
			}
			if !c.wantPicked && res.Outcome != OutcomeWait {
				t.Errorf("the loader's ask: outcome %v, want a wait: the strict kanban's empty is no loader's", res.Outcome)
			}
		})
	}
}

// TestLoaderAsk_TheStrictKanbansPressIsStillServed: the press the strict kanban
// supports asks for an empty with no source, naming itself, and the plant-wide
// scan serves it from the kanban the loader may not take from.
func TestLoaderAsk_TheStrictKanbansPressIsStillServed(t *testing.T) {
	t.Parallel()
	r := newLKRig(t, "LKP")
	empty := r.group(t, true, true)
	r.d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{OrderUUID: "lkp-press",
		OrderType: OrderTypeRetrieve, RetrieveEmpty: true, PayloadCode: r.part,
		DeliveryNode: r.press.Name, ProcessNode: r.press.Name, Quantity: 1})
	o := testdb.RequireOrder(t, r.db, "lkp-press")
	if o.Plan == nil || o.Plan.BinID != empty {
		t.Errorf("the supported press's plant-wide empty: %s under %q with pick %+v, want the kanban's empty %d",
			o.Status, o.QueueCause, o.Plan, empty)
	}
}
