//go:build docker

package engine

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/fleet"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/messaging"
	"shingocore/service"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/payloads"
	"shingocore/store/plantclaims"
	"shingocore/store/reservations"
)

// clear_home_docker_test.go — a dedicated loader home's clear, authored by
// Core (engine/clear_home.go, dispatch/clear_home.go), end to end against the
// fleet simulator: the arms, the refusals, the re-run, a cancel, a fault, and
// a crash inside the birth.

const clearStation = "edge.clear"

// clearRig is one dedicated loader: home H pinned to the part with a full
// carrier E on it, buffer S holding a partial P, buffer X open or occupied.
type clearRig struct {
	db       *store.DB
	eng      *Engine
	sim      *simulator.SimulatorBackend
	d        *dispatch.Dispatcher
	part     string
	home     *nodes.Node
	s, x     *nodes.Node
	e, p     *bins.Bin
	xBin     *bins.Bin
	counter  *store.QueryCounter // set by clearRigOpts.counting
	loaderID int64
}

func newClearRig(t *testing.T, prefix string, openSpot bool) *clearRig {
	t.Helper()
	return newClearRigWith(t, prefix, openSpot, clearRigOpts{})
}

// clearRigOpts are the rig's two variants: an engine on a statement-counting
// database (counter set), and a fleet that wraps the simulator.
type clearRigOpts struct {
	counting bool
	fleet    func(*simulator.SimulatorBackend) fleet.Backend
}

func newClearRigWith(t *testing.T, prefix string, openSpot bool, opts clearRigOpts) *clearRig {
	t.Helper()
	db, cfg := testdb.OpenWithConfig(t)
	testdb.SetupStandardData(t, db)
	r := &clearRig{db: db, part: prefix + "-PART"}
	testutil.MustNoErr(t, db.CreatePayload(&payloads.Payload{Code: r.part, Description: "clear part", UOPCapacity: 10}), "payload")
	mk := func(name string) *nodes.Node {
		n := &nodes.Node{Name: name, Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(n), "node "+name)
		return n
	}
	r.home, r.s, r.x = mk(prefix+"-H"), mk(prefix+"-S"), mk(prefix+"-X")
	id, err := db.CreateLoader(loaders.Loader{Name: prefix, Role: "produce",
		Layout: protocol.LoaderLayoutDedicatedPositions, Replenishment: "operator"})
	testutil.MustNoErr(t, err, "loader")
	r.loaderID = id
	testutil.MustNoErr(t, db.UpsertLoaderHome(loaders.Home{LoaderID: id, PositionNodeID: r.home.ID,
		PayloadCode: r.part, Kind: loaders.HomeKindHome}), "home")
	testutil.MustNoErr(t, db.UpsertLoaderHome(loaders.Home{LoaderID: id, PositionNodeID: r.s.ID,
		Kind: loaders.HomeKindBuffer, SortOrder: 1}), "buffer S")
	testutil.MustNoErr(t, db.UpsertLoaderHome(loaders.Home{LoaderID: id, PositionNodeID: r.x.ID,
		Kind: loaders.HomeKindBuffer, SortOrder: 2}), "buffer X")
	r.e = clearBin(t, db, r.part, r.home.ID, prefix+"-E", 10)
	r.p = clearBin(t, db, r.part, r.s.ID, prefix+"-P", 4)
	if !openSpot {
		r.xBin = clearBin(t, db, r.part, r.x.ID, prefix+"-XB", 3)
	}
	r.sim = simulator.New()
	var flt fleet.Backend = r.sim
	if opts.fleet != nil {
		flt = opts.fleet(r.sim)
	}
	if opts.counting {
		cdb, counter, err := store.OpenCounting(cfg)
		testutil.MustNoErr(t, err, "counting db")
		t.Cleanup(func() { cdb.Close() })
		r.db, r.counter = cdb, counter
	}
	r.eng = newTestEngine(t, r.db, flt)
	r.d = r.eng.Dispatcher()
	return r
}

// clearBin stands a confirmed carrier of the part with uop parts on a node.
func clearBin(t *testing.T, db *store.DB, part string, nodeID int64, label string, uop int) *bins.Bin {
	t.Helper()
	b := testdb.CreateBinAtNode(t, db, part, nodeID, label)
	testutil.MustNoErr(t, db.SetBinManifest(b.ID, `{"items":[]}`, part, uop), "manifest "+label)
	testutil.MustNoErr(t, db.ConfirmBinManifest(b.ID, ""), "confirm "+label)
	got, err := db.GetBin(b.ID)
	testutil.MustNoErr(t, err, "reload "+label)
	return got
}

func (r *clearRig) tap(t *testing.T) ClearHomeAnswer {
	t.Helper()
	return r.eng.ClearLoaderHome(clearStation, protocol.ClearHomeRequest{HomeNode: r.home.Name})
}

// parentOf reads the live clear parent for the home, or nil.
func (r *clearRig) parent(t *testing.T) *orders.Order {
	t.Helper()
	var id int64
	err := r.db.DB.QueryRow(`SELECT id FROM orders WHERE delivery_node=$1 AND edge_uuid LIKE 'core-clear-%' ORDER BY id DESC LIMIT 1`,
		r.home.Name).Scan(&id)
	if err != nil {
		return nil
	}
	o, err := r.db.GetOrder(id)
	testutil.MustNoErr(t, err, "parent")
	return o
}

// children are the parent's children by sequence; partial is the stamped one.
func (r *clearRig) children(t *testing.T, parentID int64) (partial *orders.Order, empties []*orders.Order) {
	t.Helper()
	kids, err := r.db.ListChildOrders(parentID)
	testutil.MustNoErr(t, err, "children")
	for _, k := range kids {
		if k.BinID != nil && *k.BinID == r.p.ID {
			partial = k
		} else {
			empties = append(empties, k)
		}
	}
	return partial, empties
}

func (r *clearRig) reload(t *testing.T, id int64) *orders.Order {
	t.Helper()
	o, err := r.db.GetOrder(id)
	testutil.MustNoErr(t, err, fmt.Sprintf("reload order %d", id))
	return o
}

// lift drives an order's pickup block, as the fleet reports it.
func (r *clearRig) lift(t *testing.T, id int64) {
	t.Helper()
	r.liftAt(t, id, "")
}

// liftAt is lift with the block reported at location ("" = where the fleet
// was sent).
func (r *clearRig) liftAt(t *testing.T, id int64, location string) {
	t.Helper()
	o := r.reload(t, id)
	if o.VendorOrderID == "" {
		t.Fatalf("order %d was never dispatched (status %s, cause %q)", id, o.Status, o.QueueCause)
	}
	r.sim.DriveState(o.VendorOrderID, "RUNNING")
	for _, b := range r.sim.BlocksForOrder(o.VendorOrderID) {
		if IsPickupBlock(b.BinTask) {
			at := b.Location
			if location != "" {
				at = location
			}
			r.sim.CompleteBlock(o.VendorOrderID, b.BlockID, at, b.BinTask, 0, 0)
			return
		}
	}
	t.Fatalf("order %d has no pickup block", id)
}

func (r *clearRig) finish(t *testing.T, id int64) {
	t.Helper()
	r.sim.DriveState(r.reload(t, id).VendorOrderID, "FINISHED")
}

// tradeLegs are a trade's two moves, reloaded: l1 the partial's move P S→H,
// l2 the empty's move E H→S.
func (r *clearRig) tradeLegs(t *testing.T, parentID int64) (l1, l2 *orders.Order) {
	t.Helper()
	partial, empties := r.children(t, parentID)
	if partial == nil || len(empties) != 1 {
		t.Fatalf("children: partial=%v empties=%d, want the partial's move and one empty's move", partial, len(empties))
	}
	return r.reload(t, partial.ID), r.reload(t, empties[0].ID)
}

// binOn asserts a bin is recorded on a node.
func (r *clearRig) binOn(t *testing.T, binID int64, n *nodes.Node) {
	t.Helper()
	b, err := r.db.GetBin(binID)
	testutil.MustNoErr(t, err, "reload bin")
	if b.NodeID == nil || *b.NodeID != n.ID {
		t.Errorf("bin %s is on node %v, want %s (%d)", b.Label, b.NodeID, n.Name, n.ID)
	}
}

// TestClearHome_SpotArm: an open buffer spot and no pull. One parent in
// `reshuffling`, the empty's move dispatched at confirm, the partial's move
// held on the row until the empty is lifted off the home, then sent; both
// land and the parent is confirmed. The carrier is zeroed at confirm, the
// answer names it and its new epoch, and the station is pushed the parent and
// both children.
func TestClearHome_SpotArm(t *testing.T) {
	t.Parallel()
	r := newClearRig(t, "CHS", true)
	before, err := r.db.GetBin(r.e.ID)
	testutil.MustNoErr(t, err, "E before")

	ans := r.tap(t)
	if ans.Status != http.StatusOK || ans.Body.Arm != protocol.ClearArmSpot {
		t.Fatalf("tap: %d %+v, want 200 spot", ans.Status, ans.Body)
	}
	e, err := r.db.GetBin(r.e.ID)
	testutil.MustNoErr(t, err, "E after")
	if e.PayloadCode != "" || e.UOPRemaining != 0 || ans.Body.BinID != r.e.ID || ans.Body.DeltaEpoch <= before.DeltaEpoch ||
		ans.Body.DeltaEpoch != e.DeltaEpoch {
		t.Errorf("the zero: E payload=%q uop=%d epoch=%d; answer bin=%d epoch=%d (before %d)",
			e.PayloadCode, e.UOPRemaining, e.DeltaEpoch, ans.Body.BinID, ans.Body.DeltaEpoch, before.DeltaEpoch)
	}
	parent := r.parent(t)
	if parent == nil || parent.Status != protocol.StatusReshuffling || parent.EdgeUUID != ans.Body.ParentUUID {
		t.Fatalf("parent %+v, want one clear parent in reshuffling named by the answer", parent)
	}
	partial, empties := r.children(t, parent.ID)
	if partial == nil || len(empties) != 1 {
		t.Fatalf("children: partial=%v empties=%d, want the partial's move and one empty's move", partial, len(empties))
	}
	em := r.reload(t, empties[0].ID)
	if em.VendorOrderID == "" || em.DeliveryNode != r.x.Name || em.Sequence != 1 {
		t.Errorf("empty's move: vendor=%q to=%s seq=%d, want dispatched to %s at seq 1", em.VendorOrderID, em.DeliveryNode, em.Sequence, r.x.Name)
	}
	if partial.VendorOrderID != "" || dispatch.QueueCause(partial.QueueCause) != dispatch.CauseVacateWaitingLift {
		t.Errorf("partial's move before the lift: vendor=%q cause=%q, want held with vacate-waiting-lift", partial.VendorOrderID, partial.QueueCause)
	}
	if !strings.Contains(partial.QueueReason, r.e.Label) {
		t.Errorf("partial's move says %q, want it to name %s", partial.QueueReason, r.e.Label)
	}
	var pushed int
	testutil.MustNoErr(t, r.db.DB.QueryRow(`SELECT COUNT(*) FROM outbox WHERE station_id=$1 AND msg_type=$2`,
		clearStation, "data."+protocol.SubjectOrderProjected).Scan(&pushed), "outbox")
	if pushed != 3 {
		t.Errorf("%d projections pushed to the station, want 3 (parent, partial's move, empty's move)", pushed)
	}

	// A spurious advance before the lift sends nothing: the gate, not the
	// sequence, holds the partial's move.
	testutil.MustNoErr(t, r.d.AdvanceCompoundOrder(parent.ID), "advance before the lift")
	if got := r.reload(t, partial.ID); got.VendorOrderID != "" {
		t.Fatalf("the partial's move went before the empty was lifted off the home")
	}
	// Nor does the event path: a re-drive naming the empty asks whether it
	// left (bins.LeftSince), not whether an event fired.
	r.d.RedriveVacateWaits(r.e.ID)
	if got := r.reload(t, partial.ID); got.VendorOrderID != "" {
		t.Fatalf("a re-drive naming the empty sent the partial's move before its lift")
	}
	// A pickup block reported at a node the empty is not recorded on is not
	// its lift: the empty stays on the home and the partial's move stays held.
	r.liftAt(t, em.ID, r.s.Name)
	if e, err := r.db.GetBin(r.e.ID); err != nil || e.NodeID == nil || *e.NodeID != r.home.ID {
		t.Fatalf("a pickup at %s moved the empty recorded on %s (%v)", r.s.Name, r.home.Name, err)
	}
	if got := r.reload(t, partial.ID); got.VendorOrderID != "" {
		t.Fatalf("a pickup at %s released the partial's move: a lift that never happened", r.s.Name)
	}
	r.lift(t, em.ID)
	if got := r.reload(t, partial.ID); got.VendorOrderID == "" {
		t.Fatalf("the empty's lift did not send the partial's move (status %s cause %q)", got.Status, got.QueueCause)
	}
	r.finish(t, em.ID)
	r.lift(t, partial.ID)
	r.finish(t, partial.ID)
	if got := r.reload(t, parent.ID); got.Status != protocol.StatusConfirmed {
		t.Errorf("parent after both landed: %s, want confirmed", got.Status)
	}
}

// TestClearHome_DemandArm_DryPull: a pull for another node queued dry (no
// empty anywhere it can see) takes the home's carrier through its own finder
// once the clear makes it sourceable; no empty's move is born. The pull's lift
// sends the partial's move: two lifts, two drops.
func TestClearHome_DemandArm_DryPull(t *testing.T) {
	t.Parallel()
	r := newClearRig(t, "CHD", false)
	far := &nodes.Node{Name: "CHD-FAR", Enabled: true}
	testutil.MustNoErr(t, r.db.CreateNode(far), "far node")
	r.d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{OrderUUID: "chd-pull",
		OrderType: dispatch.OrderTypeRetrieveEmpty, DeliveryNode: far.Name, Quantity: 1})
	pull := testdb.RequireOrder(t, r.db, "chd-pull")
	if pull.Status != protocol.StatusQueued {
		t.Fatalf("fixture: the pull must be queued dry, is %s", pull.Status)
	}

	ans := r.tap(t)
	if ans.Status != http.StatusOK || ans.Body.Arm != protocol.ClearArmDemand {
		t.Fatalf("tap: %d %+v, want 200 demand", ans.Status, ans.Body)
	}
	pull = r.reload(t, pull.ID)
	if pull.BinID == nil || *pull.BinID != r.e.ID || pull.VendorOrderID == "" {
		t.Fatalf("pull: bin=%v vendor=%q, want it dispatched with %s", pull.BinID, pull.VendorOrderID, r.e.Label)
	}
	parent := r.parent(t)
	partial, empties := r.children(t, parent.ID)
	if len(empties) != 0 || partial == nil || partial.VendorOrderID != "" {
		t.Fatalf("demand arm: empties=%d partial=%+v, want no empty's move and the partial's move held", len(empties), partial)
	}
	r.lift(t, pull.ID)
	if got := r.reload(t, partial.ID); got.VendorOrderID == "" {
		t.Fatalf("the pull's lift did not send the partial's move (cause %q)", got.QueueCause)
	}
	var vendors int
	testutil.MustNoErr(t, r.db.DB.QueryRow(`SELECT COUNT(*) FROM orders WHERE vendor_order_id <> '' AND id IN ($1, $2)`,
		pull.ID, partial.ID).Scan(&vendors), "count robots")
	if vendors != 2 {
		t.Errorf("%d robot jobs, want 2 (the pull, the partial's move)", vendors)
	}
}

// TestClearHome_DemandArm_TheLoadersOwnPull: the plant's own pull — the
// replenishment loop's core-l1- ask for another home of the same loader,
// sourced from the loader's inbound group — queued dry is a candidate, and
// its own finder takes the home's carrier. The group does not hold the homes
// (the shape where the homes stand outside the market they draw from), so
// the candidate is admitted by its delivery, a position of the home's loader,
// and the finder sees the carrier through the live clear's exemption.
func TestClearHome_DemandArm_TheLoadersOwnPull(t *testing.T) {
	t.Parallel()
	r := newClearRig(t, "CHL", true)
	part2 := "CHL-PART2"
	testutil.MustNoErr(t, r.db.CreatePayload(&payloads.Payload{Code: part2, Description: "second home", UOPCapacity: 10}), "payload 2")
	h2 := &nodes.Node{Name: "CHL-H2", Enabled: true}
	testutil.MustNoErr(t, r.db.CreateNode(h2), "home 2")
	testutil.MustNoErr(t, r.db.UpsertLoaderHome(loaders.Home{LoaderID: r.loaderID, PositionNodeID: h2.ID,
		PayloadCode: part2, Kind: loaders.HomeKindHome, SortOrder: 3}), "home 2 of the loader")
	ngrp, err := r.db.GetNodeTypeByCode(protocol.NodeClassNGRP)
	testutil.MustNoErr(t, err, "NGRP type")
	market := &nodes.Node{Name: "CHL-SM", NodeTypeID: &ngrp.ID, Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, r.db.CreateNode(market), "market")
	testutil.MustNoErr(t, r.db.CreateNode(&nodes.Node{Name: "CHL-SM-1", ParentID: &market.ID, Enabled: true}), "market slot")

	cfg, found, err := r.d.LoadReplenishConfig(r.loaderID)
	testutil.MustNoErr(t, err, "replenish config")
	if !found {
		t.Fatal("fixture: the loader has no replenishment config")
	}
	cfg.Replenishment, cfg.InboundSource = loaders.ReplenishmentThreshold, market.Name
	res, err := r.d.ReplenishLoader(dispatch.ReplenishRequest{Origin: dispatch.ReplenishByThreshold,
		OrderType: dispatch.OrderTypeRetrieveEmpty, StationID: clearStation, LoaderID: r.loaderID,
		PayloadCode: part2, Threshold: 10, PerBinCapacity: 10}, cfg)
	testutil.MustNoErr(t, err, "replenish")
	if len(res.Created) != 1 {
		t.Fatalf("fixture: the loop created %d asks (skipped %q, held %v), want one for %s", len(res.Created), res.Skipped, res.HeldBy, h2.Name)
	}
	pull := r.reload(t, res.Created[0].ID)
	if !strings.HasPrefix(pull.EdgeUUID, "core-l1-") || pull.SourceNode != market.Name || pull.DeliveryNode != h2.Name ||
		pull.Status != protocol.StatusQueued {
		t.Fatalf("fixture: the loop's ask %s from %s to %s is %s, want a core-l1- pull from %s to %s queued dry",
			pull.EdgeUUID, pull.SourceNode, pull.DeliveryNode, pull.Status, market.Name, h2.Name)
	}

	ans := r.tap(t)
	if ans.Status != http.StatusOK || ans.Body.Arm != protocol.ClearArmDemand {
		t.Fatalf("tap: %d %+v, want 200 demand (the loader's own pull takes the carrier)", ans.Status, ans.Body)
	}
	pull = r.reload(t, pull.ID)
	if pull.BinID == nil || *pull.BinID != r.e.ID || pull.VendorOrderID == "" {
		t.Fatalf("pull: bin=%v vendor=%q, want it dispatched with %s", pull.BinID, pull.VendorOrderID, r.e.Label)
	}
	parent := r.parent(t)
	if partial, empties := r.children(t, parent.ID); len(empties) != 0 || partial == nil {
		t.Fatalf("demand arm: empties=%d partial=%v, want no empty's move", len(empties), partial)
	}
}

// TestClearHome_FullBufferNoPull_BirthsTheTrade: no open spot and nothing
// waiting — the two-robot trade (was: the zero recorded and nothing born).
// The zero is recorded and named in the answer, the arm is "trade", and the
// confirm's own advance sends both legs: the empty's move E H→S and the
// partial's move P S→H, sibling-linked, each create its lift alone and
// unsealed, each plan [pickup, wait(vacate), drop] stamped on the bin the other
// lifts. Each drop is appended only once the other bin has left; both land,
// the parent is confirmed, E stands on S and P on H, and nothing is held.
//
// The confirm's statements are counted here (a measurement for the report,
// not a budget pin).
func TestClearHome_FullBufferNoPull_BirthsTheTrade(t *testing.T) {
	t.Parallel()
	r := newClearRigWith(t, "CHF", false, clearRigOpts{counting: true})
	r.counter.Reset()
	ans := r.tap(t)
	t.Logf("COST: the trade's confirm issued %d statements", r.counter.Count())
	if ans.Status != http.StatusOK || ans.Body.Arm != protocol.ClearArmTrade || ans.Body.BinID != r.e.ID || ans.Body.Error != "" {
		t.Fatalf("tap: %d %+v, want 200 arm trade naming the carrier", ans.Status, ans.Body)
	}
	e, err := r.db.GetBin(r.e.ID)
	testutil.MustNoErr(t, err, "E")
	if e.PayloadCode != "" || e.UOPRemaining != 0 || e.DeltaEpoch != ans.Body.DeltaEpoch || e.DeltaEpoch <= r.e.DeltaEpoch {
		t.Errorf("the zero: payload=%q uop=%d epoch=%d (answer %d, before %d)",
			e.PayloadCode, e.UOPRemaining, e.DeltaEpoch, ans.Body.DeltaEpoch, r.e.DeltaEpoch)
	}
	parent := r.parent(t)
	if parent == nil || parent.Status != protocol.StatusReshuffling || parent.EdgeUUID != ans.Body.ParentUUID {
		t.Fatalf("parent %+v, want one clear parent in reshuffling named by the answer", parent)
	}
	l1, l2 := r.tradeLegs(t, parent.ID)
	if l2.Sequence != 1 || l2.SourceNode != r.home.Name || l2.DeliveryNode != r.s.Name ||
		l2.SiblingOrderUUID != l1.EdgeUUID || l1.SiblingOrderUUID != l2.EdgeUUID {
		t.Errorf("empty's move seq=%d %s→%s sibling=%q, partial's move sibling=%q: want E %s→%s at seq 1, the two naming each other",
			l2.Sequence, l2.SourceNode, l2.DeliveryNode, l2.SiblingOrderUUID, l1.SiblingOrderUUID, r.home.Name, r.s.Name)
	}
	for _, c := range []struct {
		o        *orders.Order
		from, to string
		stampBin int64
	}{{l1, r.s.Name, r.home.Name, r.e.ID}, {l2, r.home.Name, r.s.Name, r.p.ID}} {
		if c.o.VendorOrderID == "" {
			t.Fatalf("order %d was not sent in the confirm (status %s cause %q)", c.o.ID, c.o.Status, c.o.QueueCause)
		}
		var steps []struct {
			Action   string `json:"action"`
			Node     string `json:"node"`
			WaitKind string `json:"wait_kind"`
			Vacate   *struct {
				Node string `json:"node"`
				Bin  int64  `json:"bin"`
			} `json:"vacate"`
		}
		testutil.MustNoErr(t, json.Unmarshal([]byte(c.o.StepsJSON), &steps), "steps")
		if len(steps) != 3 || steps[0].Action != protocol.ActionPickup || steps[0].Node != c.from ||
			steps[1].Action != protocol.ActionWait || steps[1].WaitKind != protocol.WaitKindVacate || steps[1].Node != "" ||
			steps[2].Action != protocol.ActionDropoff || steps[2].Node != c.to || steps[2].Vacate == nil ||
			steps[2].Vacate.Node != c.to || steps[2].Vacate.Bin != c.stampBin {
			t.Errorf("order %d plan %s, want [pickup %s, bare vacate wait, drop %s stamped on bin %d]",
				c.o.ID, c.o.StepsJSON, c.from, c.to, c.stampBin)
		}
		sent := r.sim.GetOrder(c.o.VendorOrderID)
		if sent == nil || sent.Complete || len(sent.Blocks) != 1 || sent.Blocks[0].Location != c.from || !IsPickupBlock(sent.Blocks[0].BinTask) {
			t.Errorf("order %d's create = %+v, want its lift at %s alone, unsealed", c.o.ID, sent, c.from)
		}
	}

	r.lift(t, l2.ID)
	if got := r.reload(t, l1.ID); got.WaitIndex != 1 {
		t.Errorf("E has left the home and the partial's move's drop is not appended (wait %d, status %s)", got.WaitIndex, got.Status)
	}
	if got := r.reload(t, l2.ID); got.WaitIndex != 0 {
		t.Fatalf("the empty's move's drop on %s was appended before P left it", r.s.Name)
	}
	r.lift(t, l1.ID)
	if got := r.reload(t, l2.ID); got.WaitIndex != 1 {
		t.Fatalf("P has left %s and the empty's move's drop is not appended (wait %d, status %s)", r.s.Name, got.WaitIndex, got.Status)
	}
	r.finish(t, l2.ID)
	r.finish(t, l1.ID)
	r.eng.after.settle()
	if got := r.reload(t, parent.ID); got.Status != protocol.StatusConfirmed {
		t.Errorf("parent after both landed: %s, want confirmed", got.Status)
	}
	r.binOn(t, r.e.ID, r.s)
	r.binOn(t, r.p.ID, r.home)
	testdb.AssertNoHoldsAtRest(t, r.db, "after the trade")
}

// TestClearHome_CandidatesDecline_BirthTheTrade: a pull that could see the
// home's carrier exists but its own finder declines it (its part rides another
// carrier type), and no spot is open. Choose falls through to the trade (was:
// the job ended and the zero stood): both legs go, the pull holds nothing.
func TestClearHome_CandidatesDecline_BirthTheTrade(t *testing.T) {
	t.Parallel()
	r := newClearRig(t, "CHN", false)
	other := &bins.BinType{Code: "CHN-OTHER"}
	testutil.MustNoErr(t, r.db.CreateBinType(other), "another carrier type")
	pickyPart := &payloads.Payload{Code: "CHN-PICKY", Description: "rides the other type", UOPCapacity: 5}
	testutil.MustNoErr(t, r.db.CreatePayload(pickyPart), "picky part")
	testutil.MustNoErr(t, r.db.SetPayloadBinTypes(pickyPart.ID, []int64{other.ID}), "the part rides the other type only")
	far := &nodes.Node{Name: "CHN-FAR", Enabled: true}
	testutil.MustNoErr(t, r.db.CreateNode(far), "far node")
	r.d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{OrderUUID: "chn-pull",
		OrderType: dispatch.OrderTypeRetrieveEmpty, PayloadCode: pickyPart.Code, DeliveryNode: far.Name, Quantity: 1})
	if pull := testdb.RequireOrder(t, r.db, "chn-pull"); pull.Status != protocol.StatusQueued {
		t.Fatalf("fixture: the pull must be queued dry, is %s", pull.Status)
	}

	ans := r.tap(t)
	if ans.Status != http.StatusOK || ans.Body.Arm != protocol.ClearArmTrade || ans.Body.BinID != r.e.ID || ans.Body.DeltaEpoch == 0 {
		t.Fatalf("tap: %d %+v, want 200 arm trade with the zero carried", ans.Status, ans.Body)
	}
	if pull := testdb.RequireOrder(t, r.db, "chn-pull"); pull.BinID != nil {
		t.Errorf("the declining pull holds bin %d", *pull.BinID)
	}
	parent := r.parent(t)
	if parent == nil || parent.Status != protocol.StatusReshuffling {
		t.Fatalf("parent %+v, want the job live", parent)
	}
	l1, l2 := r.tradeLegs(t, parent.ID)
	if l1.VendorOrderID == "" || l2.VendorOrderID == "" {
		t.Fatalf("trade legs: partial's move vendor=%q, empty's move vendor=%q, want both sent", l1.VendorOrderID, l2.VendorOrderID)
	}
}

// TestClearHome_TheExemptionClosesWhenTheJobEnds_WithNoWrite: the exemption in
// the empty finders' loader arm is keyed on state — the carrier unheld AND the
// clear's parent live (STOP 7: no flag, no column). With the job born and
// nothing yet holding the carrier, an empty find sees it on the home; the job
// ends, and the next find does not see it, with no write to open or close
// anything but the job's own end.
func TestClearHome_TheExemptionClosesWhenTheJobEnds_WithNoWrite(t *testing.T) {
	t.Parallel()
	r := newClearRig(t, "CHZ", false)
	job, refusal := r.eng.resolveClearJob(r.home, clearStation, protocol.ClearHomeRequest{HomeNode: r.home.Name})
	if refusal != nil {
		t.Fatalf("resolve: %+v", refusal.Body)
	}
	birth, _, err := r.eng.issueClear(job)
	testutil.MustNoErr(t, err, "birth")
	found, err := r.db.FindEmptyCompatibleBin(r.part, "", 0, bins.EmptyFence{}, reservations.Anyone, bins.Height{})
	if err != nil || found == nil || found.ID != r.e.ID {
		t.Fatalf("with the job live and the carrier unheld, the find took %v (%v), want the home's carrier %d", found, err, r.e.ID)
	}
	r.d.EndClear(birth.Parent, "ended for the pin")
	got, err := r.db.FindEmptyCompatibleBin(r.part, "", 0, bins.EmptyFence{}, reservations.Anyone, bins.Height{})
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("the find after the end: %v", err)
	}
	if got != nil && got.ID == r.e.ID {
		t.Errorf("the job has ended and the find still takes the home's carrier %d", r.e.ID)
	}
}

// TestClearHome_SecondTapRefused: a clear of the home is live; the second tap
// is 409, one sentence, and births nothing.
func TestClearHome_SecondTapRefused(t *testing.T) {
	t.Parallel()
	r := newClearRig(t, "CH2", true)
	if ans := r.tap(t); ans.Status != http.StatusOK {
		t.Fatalf("first tap: %d %+v", ans.Status, ans.Body)
	}
	ans := r.tap(t)
	if ans.Status != http.StatusConflict || ans.Body.Error != r.home.Name+" is already being cleared" {
		t.Fatalf("second tap: %d %+v, want 409 'already being cleared'", ans.Status, ans.Body)
	}
	var parents int
	testutil.MustNoErr(t, r.db.DB.QueryRow(`SELECT COUNT(*) FROM orders WHERE edge_uuid LIKE 'core-clear-%'`).Scan(&parents), "parents")
	if parents != 1 {
		t.Errorf("%d clear parents, want 1", parents)
	}
}

// TestClearHome_PullCancelledBeforeLift_RerunsToSpot: the pull that took the
// carrier is cancelled before it lifts; the clear's re-run (on the order's
// end) finds the carrier on the home held by nobody and the partial's move
// waiting, and chooses again — a spot has opened, so the empty's move is born.
// The partial's move is reused, never a second one; the carrier is not zeroed
// again.
func TestClearHome_PullCancelledBeforeLift_RerunsToSpot(t *testing.T) {
	t.Parallel()
	r := newClearRig(t, "CHC", false)
	far := &nodes.Node{Name: "CHC-FAR", Enabled: true}
	testutil.MustNoErr(t, r.db.CreateNode(far), "far node")
	r.d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{OrderUUID: "chc-pull",
		OrderType: dispatch.OrderTypeRetrieveEmpty, DeliveryNode: far.Name, Quantity: 1})
	ans := r.tap(t)
	if ans.Body.Arm != protocol.ClearArmDemand {
		t.Fatalf("tap: %+v, want demand", ans.Body)
	}
	parent := r.parent(t)
	partial, _ := r.children(t, parent.ID)
	epoch := ans.Body.DeltaEpoch

	testutil.MustNoErr(t, r.db.DeleteBin(r.xBin.ID), "a spot opens")
	pull := testdb.RequireOrder(t, r.db, "chc-pull")
	r.d.Lifecycle().CancelOrder(pull, pull.StationID, "operator cancelled", dispatch.CancelCause{})
	r.eng.after.settle()

	_, empties := r.children(t, parent.ID)
	if len(empties) != 1 || r.reload(t, empties[0].ID).VendorOrderID == "" {
		t.Fatalf("after the pull's cancel: %d empty's moves, want one, dispatched", len(empties))
	}
	if again, _ := r.children(t, parent.ID); again.ID != partial.ID {
		t.Errorf("a second partial's move %d (first %d)", again.ID, partial.ID)
	}
	e, err := r.db.GetBin(r.e.ID)
	testutil.MustNoErr(t, err, "E")
	if e.DeltaEpoch != epoch {
		t.Errorf("the carrier was zeroed again: epoch %d, confirm's %d", e.DeltaEpoch, epoch)
	}
}

// TestClearHome_PullDemoted_RerunOnTheFloor: a fleet refusal demotes the pull
// (its claim becomes a soft hold): not an end, so nothing re-runs. When the
// hold is gone (reaped), the floor's re-run chooses again.
func TestClearHome_PullDemoted_RerunOnTheFloor(t *testing.T) {
	t.Parallel()
	r := newClearRig(t, "CHR", false)
	far := &nodes.Node{Name: "CHR-FAR", Enabled: true}
	testutil.MustNoErr(t, r.db.CreateNode(far), "far node")
	r.d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{OrderUUID: "chr-pull",
		OrderType: dispatch.OrderTypeRetrieveEmpty, DeliveryNode: far.Name, Quantity: 1})
	if ans := r.tap(t); ans.Body.Arm != protocol.ClearArmDemand {
		t.Fatalf("tap: %+v, want demand", ans.Body)
	}
	parent := r.parent(t)
	pull := testdb.RequireOrder(t, r.db, "chr-pull")
	r.d.DemoteAfterFleetRefusal(pull, dispatch.QueueWait{}, protocol.QueueFleetUnavailable, dispatch.CauseFleetRefusedCreate, dispatch.QueueParams{})
	testutil.MustNoErr(t, r.db.DeleteBin(r.xBin.ID), "a spot opens")

	if n := r.d.RerunClears(); n != 0 {
		t.Fatalf("re-ran %d with the demoted pull still holding the carrier", n)
	}
	if _, err := r.db.DB.Exec(`DELETE FROM reservations WHERE bin_id=$1`, r.e.ID); err != nil {
		t.Fatalf("reap the hold: %v", err)
	}
	if n := r.d.RerunClears(); n != 1 {
		t.Fatalf("the floor re-ran %d, want 1", n)
	}
	if _, empties := r.children(t, parent.ID); len(empties) != 1 {
		t.Fatalf("%d empty's moves after the floor's re-run, want 1", len(empties))
	}
}

// TestClearHome_ParentCancelled: the station cancels the job; the children not
// carrying their bin are cancelled with it and the partial's hold goes.
func TestClearHome_ParentCancelled(t *testing.T) {
	t.Parallel()
	r := newClearRig(t, "CHX", true)
	if ans := r.tap(t); ans.Status != http.StatusOK {
		t.Fatalf("tap: %+v", ans.Body)
	}
	parent := r.parent(t)
	r.d.CancelOrderWithCascade(parent, parent.StationID, "operator cancelled", dispatch.CancelCause{})
	partial, empties := r.children(t, parent.ID)
	for _, c := range append(empties, partial) {
		if got := r.reload(t, c.ID); got.Status != protocol.StatusCancelled {
			t.Errorf("child %d is %s after the parent's cancel, want cancelled", c.ID, got.Status)
		}
	}
	var holds int
	testutil.MustNoErr(t, r.db.DB.QueryRow(`SELECT COUNT(*) FROM reservations WHERE bin_id=$1`, r.p.ID).Scan(&holds), "holds")
	if holds != 0 {
		t.Errorf("%d holds left on the partial", holds)
	}
}

// TestClearHome_SpotRobotFault: the empty's move fails before it lifts. The
// partial's move is NOT taken down with it (a dig's failure would cancel its
// siblings); the re-run finds the carrier on the home held by nobody and births
// a new empty's move to the spot.
func TestClearHome_SpotRobotFault(t *testing.T) {
	t.Parallel()
	r := newClearRig(t, "CHT", true)
	if ans := r.tap(t); ans.Status != http.StatusOK {
		t.Fatalf("tap: %+v", ans.Body)
	}
	parent := r.parent(t)
	partial, empties := r.children(t, parent.ID)
	first := r.reload(t, empties[0].ID)
	testutil.MustNoErr(t, r.d.Lifecycle().Fail(first, first.StationID, "robot_fault", "the robot faulted"), "fail the empty's move")
	r.eng.after.settle()

	if got := r.reload(t, partial.ID); protocol.IsTerminal(got.Status) {
		t.Fatalf("the partial's move went %s with the empty's move", got.Status)
	}
	_, empties = r.children(t, parent.ID)
	var live int
	for _, c := range empties {
		if got := r.reload(t, c.ID); !protocol.IsTerminal(got.Status) {
			live++
		}
	}
	if len(empties) != 2 || live != 1 {
		t.Errorf("empty's moves: %d (live %d), want the failed one and one new", len(empties), live)
	}
	if got := r.reload(t, parent.ID); got.Status != protocol.StatusReshuffling {
		t.Errorf("parent %s, want still reshuffling", got.Status)
	}
}

// TestClearHome_CrashInsideTheBirth_NothingHalfBorn: the birth's transaction
// fails at its last write (the carrier's zero) — a crash before its commit.
// Nothing of the clear exists: no parent, no child, no hold, the carrier
// untouched. And a committed parent is never in the scanner's population.
func TestClearHome_CrashInsideTheBirth_NothingHalfBorn(t *testing.T) {
	t.Parallel()
	r := newClearRig(t, "CHK", true)
	_, err := r.db.DB.Exec(fmt.Sprintf(`
		CREATE FUNCTION chk_refuse() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'crash'; END $$ LANGUAGE plpgsql;
		CREATE TRIGGER chk_refuse BEFORE UPDATE OF payload_code ON bins FOR EACH ROW
		  WHEN (OLD.id = %d) EXECUTE FUNCTION chk_refuse();`, r.e.ID))
	testutil.MustNoErr(t, err, "the crash")
	if ans := r.tap(t); ans.Status != http.StatusInternalServerError {
		t.Fatalf("tap: %d %+v, want a fault", ans.Status, ans.Body)
	}
	var orderRows, holds int
	testutil.MustNoErr(t, r.db.DB.QueryRow(`SELECT COUNT(*) FROM orders`).Scan(&orderRows), "orders")
	testutil.MustNoErr(t, r.db.DB.QueryRow(`SELECT COUNT(*) FROM reservations WHERE bin_id IN ($1, $2)`, r.e.ID, r.p.ID).Scan(&holds), "holds")
	e, err := r.db.GetBin(r.e.ID)
	testutil.MustNoErr(t, err, "E")
	if orderRows != 0 || holds != 0 || e.PayloadCode != r.part || e.DeltaEpoch != r.e.DeltaEpoch {
		t.Errorf("half-born: orders=%d holds=%d E payload=%q epoch=%d", orderRows, holds, e.PayloadCode, e.DeltaEpoch)
	}

	_, err = r.db.DB.Exec(`DROP TRIGGER chk_refuse ON bins`)
	testutil.MustNoErr(t, err, "the restart")
	if ans := r.tap(t); ans.Status != http.StatusOK {
		t.Fatalf("tap after: %d %+v", ans.Status, ans.Body)
	}
	acq, err := r.db.ListAcquiringOrders()
	testutil.MustNoErr(t, err, "acquiring")
	for _, o := range acq {
		if dispatch.IsClearParent(o) {
			t.Errorf("the clear's parent %d is in the scanner's population (%s)", o.ID, o.Status)
		}
	}
}

// TestClearHome_EdgeRestartReconcileCarriesTheRows: an Edge that missed the
// pushes gets the parent and its children from the status reconcile — every
// active order for its station (ListActiveOrdersByStation). The children are
// plain rows; the wire carries no parent marker.
func TestClearHome_EdgeRestartReconcileCarriesTheRows(t *testing.T) {
	t.Parallel()
	r := newClearRig(t, "CHE", true)
	if ans := r.tap(t); ans.Status != http.StatusOK {
		t.Fatalf("tap: %+v", ans.Body)
	}
	parent := r.parent(t)
	active, err := r.db.ListActiveOrdersByStation(clearStation)
	testutil.MustNoErr(t, err, "reconcile set")
	seen := map[int64]bool{}
	for _, o := range active {
		seen[o.ID] = true
	}
	partial, empties := r.children(t, parent.ID)
	for _, id := range []int64{parent.ID, partial.ID, empties[0].ID} {
		if !seen[id] {
			t.Errorf("order %d is not in the station's reconcile set", id)
		}
	}
}

// TestClearHome_StalledChapterSweepWaitsOnAHeldPartial: a demand-arm clear
// whose family has gone quiet past the stall window — the partial's move held
// on the vacate fact, nothing handed to the fleet under the parent. The
// chapter watchdog reads it as a wait (S-f), not a stale plan: no dissolve,
// the partial's move stays, the parent stays `reshuffling`.
func TestClearHome_StalledChapterSweepWaitsOnAHeldPartial(t *testing.T) {
	t.Parallel()
	r := newClearRig(t, "CHW", false)
	far := &nodes.Node{Name: "CHW-FAR", Enabled: true}
	testutil.MustNoErr(t, r.db.CreateNode(far), "far node")
	r.d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{OrderUUID: "chw-pull",
		OrderType: dispatch.OrderTypeRetrieveEmpty, DeliveryNode: far.Name, Quantity: 1})
	if ans := r.tap(t); ans.Body.Arm != protocol.ClearArmDemand {
		t.Fatalf("tap: %+v, want demand", ans.Body)
	}
	parent := r.parent(t)
	partial, _ := r.children(t, parent.ID)
	_, err := r.db.DB.Exec(`UPDATE orders SET updated_at = NOW() - INTERVAL '1 hour' WHERE id = $1 OR parent_order_id = $1`, parent.ID)
	testutil.MustNoErr(t, err, "the family goes quiet")
	res := r.d.SweepStalledChapters()
	if res.Dissolved != 0 || res.Waiting != 1 {
		t.Errorf("sweep: %+v, want one waiting and nothing dissolved", res)
	}
	if got := r.reload(t, partial.ID); protocol.IsTerminal(got.Status) {
		t.Errorf("the partial's move went %s", got.Status)
	}
	if got := r.reload(t, parent.ID); got.Status != protocol.StatusReshuffling {
		t.Errorf("parent %s, want reshuffling", got.Status)
	}
}

// TestClearHome_CrashAfterTheBirth_TheFloorChooses: Core stops between the
// birth's commit and its choice — the parent and the partial's move exist, the
// carrier is zeroed, and no arm was taken. That is exactly the re-run's state
// (the carrier on the home held by nobody, the partial's move waiting), so the
// floor's re-run chooses, without re-zeroing.
func TestClearHome_CrashAfterTheBirth_TheFloorChooses(t *testing.T) {
	t.Parallel()
	r := newClearRig(t, "CHA", true)
	job, refusal := r.eng.resolveClearJob(r.home, clearStation, protocol.ClearHomeRequest{HomeNode: r.home.Name})
	if refusal != nil {
		t.Fatalf("resolve: %+v", refusal.Body)
	}
	birth, epoch, err := r.eng.issueClear(job)
	testutil.MustNoErr(t, err, "birth, then the crash")
	if n := r.d.RerunClears(); n != 1 {
		t.Fatalf("the floor re-ran %d, want 1", n)
	}
	_, empties := r.children(t, birth.Parent.ID)
	if len(empties) != 1 || r.reload(t, empties[0].ID).VendorOrderID == "" {
		t.Fatalf("after the floor: %d empty's moves, want one, dispatched", len(empties))
	}
	e, err := r.db.GetBin(r.e.ID)
	testutil.MustNoErr(t, err, "E")
	if e.DeltaEpoch != epoch {
		t.Errorf("re-zeroed: epoch %d, birth's %d", e.DeltaEpoch, epoch)
	}
}

// TestClearPin_RerunNeverTouchesACommittedPartial: the re-run acts only on a
// partial's move nothing has sent. With the partial's move already with the
// fleet (a vendor order on its row) and the empty standing on the home held by
// nobody (its move failed before the lift), neither the failure's own re-drive
// nor the floor re-runs: the vacate re-drive reads a sent child as an order of
// its own (redriveVacateOrder), never as a held partial, and rerunClearLocked
// refuses a partial's move that carries a vendor order. No second empty's move
// is born and the partial's move is left as it was.
func TestClearPin_RerunNeverTouchesACommittedPartial(t *testing.T) {
	t.Parallel()
	r := newClearRig(t, "CPR", true)
	if ans := r.tap(t); ans.Status != http.StatusOK || ans.Body.Arm != protocol.ClearArmSpot {
		t.Fatalf("tap: %d %+v, want 200 spot", ans.Status, ans.Body)
	}
	parent := r.parent(t)
	partial, empties := r.children(t, parent.ID)
	testutil.MustNoErr(t, r.db.UpdateOrderVendor(partial.ID, "sg-committed-partial", "RUNNING", "AMR-01"),
		"the partial's move is with the fleet")
	first := r.reload(t, empties[0].ID)
	testutil.MustNoErr(t, r.d.Lifecycle().Fail(first, first.StationID, "robot_fault", "the robot faulted"), "fail the empty's move")
	r.eng.after.settle()
	if n := r.d.RerunClears(); n != 0 {
		t.Errorf("the floor re-ran %d clears with the partial's move committed, want 0", n)
	}
	if _, empties = r.children(t, parent.ID); len(empties) != 1 {
		t.Errorf("%d empty's moves, want only the failed one", len(empties))
	}
	if got := r.reload(t, partial.ID); got.VendorOrderID != "sg-committed-partial" || protocol.IsTerminal(got.Status) {
		t.Errorf("the partial's move: vendor=%q status=%s, want it left as it was", got.VendorOrderID, got.Status)
	}
}

// clearDoorSnap is what a home clear can write, read before the tap: the
// rig's bins whole, and the row counts of the tables a clear writes to.
type clearDoorSnap struct {
	bins   map[int64]string
	counts map[string]int
}

func (r *clearRig) snapDoor(t *testing.T) clearDoorSnap {
	t.Helper()
	s := clearDoorSnap{bins: map[int64]string{}, counts: map[string]int{}}
	ids := []int64{r.e.ID, r.p.ID}
	if r.xBin != nil {
		ids = append(ids, r.xBin.ID)
	}
	for _, id := range ids {
		var row string
		testutil.MustNoErr(t, r.db.DB.QueryRow(`SELECT to_jsonb(b)::text FROM bins b WHERE id=$1`, id).Scan(&row), "bin row")
		s.bins[id] = row
	}
	for _, tbl := range []string{"orders", "reservations", "bin_uop_ledger", "audit_log", "outbox", "cms_transactions"} {
		var n int
		testutil.MustNoErr(t, r.db.DB.QueryRow(`SELECT COUNT(*) FROM `+tbl).Scan(&n), "count "+tbl)
		s.counts[tbl] = n
	}
	return s
}

// TestClearHomeDoor_Refusals pins every refusal of the home clear's door
// (Engine.ClearLoaderHome, resolveClearJob, dispatch.ClearTradeRefusal): the
// status, the whole answer — status "error", the sentence, Core's current rows
// for the home and any spot named, and no arm, parent, bin or epoch — and that nothing was written: no bin row moved, no order,
// hold, ledger, audit, outbox or CMS row added. The second tap's refusal is
// TestClearHome_SecondTapRefused; the promised partial's is
// TestClearHome_PromisedPartial_RefusedBeforeAnyWrite.
func TestClearHomeDoor_Refusals(t *testing.T) {
	t.Parallel()
	mkNode := func(t *testing.T, r *clearRig, name string) *nodes.Node {
		n := &nodes.Node{Name: name, Enabled: true}
		testutil.MustNoErr(t, r.db.CreateNode(n), "node "+name)
		return n
	}
	setUOP := func(t *testing.T, r *clearRig, b *bins.Bin, uop int) {
		testutil.MustNoErr(t, r.db.SetBinManifest(b.ID, `{"items":[]}`, b.PayloadCode, uop), "manifest "+b.Label)
	}
	home := func(r *clearRig) protocol.ClearHomeRequest {
		return protocol.ClearHomeRequest{HomeNode: r.home.Name}
	}
	for _, tc := range []struct {
		name     string
		prefix   string
		openSpot bool
		arrange  func(t *testing.T, r *clearRig) protocol.ClearHomeRequest
		status   int
		sentence func(r *clearRig) string
	}{
		{"an unknown home", "CRA", true,
			func(t *testing.T, r *clearRig) protocol.ClearHomeRequest {
				return protocol.ClearHomeRequest{HomeNode: "CRA-NOPE"}
			},
			http.StatusNotFound, func(*clearRig) string { return `node "CRA-NOPE" not found` }},
		{"a node no loader has", "CRB", true,
			func(t *testing.T, r *clearRig) protocol.ClearHomeRequest {
				mkNode(t, r, "CRB-PLAIN")
				return protocol.ClearHomeRequest{HomeNode: "CRB-PLAIN"}
			},
			http.StatusBadRequest, func(*clearRig) string { return "CRB-PLAIN is not a loader position" }},
		{"a home of a shared-window loader", "CRC", true,
			func(t *testing.T, r *clearRig) protocol.ClearHomeRequest {
				n := mkNode(t, r, "CRC-WIN")
				id, err := r.db.CreateLoader(loaders.Loader{Name: "CRC-SHARED", Role: "produce",
					Layout: protocol.LoaderLayoutSharedWindow, Replenishment: "operator"})
				testutil.MustNoErr(t, err, "shared loader")
				testutil.MustNoErr(t, r.db.UpsertLoaderHome(loaders.Home{LoaderID: id, PositionNodeID: n.ID,
					PayloadCode: r.part, Kind: loaders.HomeKindHome}), "its window")
				return protocol.ClearHomeRequest{HomeNode: n.Name}
			},
			http.StatusBadRequest, func(*clearRig) string { return "CRC-WIN is not a dedicated-position loader" }},
		{"a buffer position", "CRD", true,
			func(t *testing.T, r *clearRig) protocol.ClearHomeRequest {
				return protocol.ClearHomeRequest{HomeNode: r.s.Name}
			},
			http.StatusBadRequest, func(*clearRig) string { return "CRD-S is a buffer position, not a home" }},
		{"a home with no carrier", "CRE", true,
			func(t *testing.T, r *clearRig) protocol.ClearHomeRequest {
				away := mkNode(t, r, "CRE-AWAY")
				_, err := r.db.DB.Exec(`UPDATE bins SET node_id=$1 WHERE id=$2`, away.ID, r.e.ID)
				testutil.MustNoErr(t, err, "the carrier leaves")
				return home(r)
			},
			http.StatusBadRequest, func(*clearRig) string { return "CRE-H has no bin with parts to clear" }},
		{"a home whose carrier holds no parts", "CRF", true,
			func(t *testing.T, r *clearRig) protocol.ClearHomeRequest {
				setUOP(t, r, r.e, 0)
				return home(r)
			},
			http.StatusBadRequest, func(*clearRig) string { return "CRF-H has no bin with parts to clear" }},
		{"a home holding two carriers", "CRG", true,
			func(t *testing.T, r *clearRig) protocol.ClearHomeRequest {
				clearBin(t, r.db, r.part, r.home.ID, "CRG-E2", 10)
				return home(r)
			},
			http.StatusBadRequest, func(*clearRig) string { return "CRG-H has no bin with parts to clear" }},
		{"a carrier an order has claimed", "CRH", true,
			func(t *testing.T, r *clearRig) protocol.ClearHomeRequest {
				o := testdb.CreateOrder(t, r.db, func(o *orders.Order) { o.Status = protocol.StatusDispatched })
				testdb.ClaimBinForTest(t, r.db, r.e.ID, o.ID)
				return home(r)
			},
			http.StatusBadRequest, func(*clearRig) string { return "CRH-E is already being moved" }},
		{"a held partial on the home", "CRI", true,
			func(t *testing.T, r *clearRig) protocol.ClearHomeRequest {
				testutil.MustNoErr(t, r.db.ReplacePlantClaims("CRI-PROC",
					[]plantclaims.StyleRow{{ProcessID: "CRI-PROC", StyleID: "CRI-STY", ConfigGen: 1, IsActive: true}},
					[]plantclaims.ClaimRow{{ProcessID: "CRI-PROC", StyleID: "CRI-STY", CoreNodeName: "CRI-LINE",
						Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeTwoRobot, PayloadCode: r.part,
						PartialDestination: r.home.Name}}, 0), "the home keeps the part's partials")
				setUOP(t, r, r.e, 4)
				return home(r)
			},
			http.StatusConflict, func(*clearRig) string {
				return "CRI-E holds 4 of CRI-PART, unfinished and held here to be called back to its line"
			}},
		{"a named spot that is not the loader's", "CRJ", true,
			func(t *testing.T, r *clearRig) protocol.ClearHomeRequest {
				req := home(r)
				req.PartialNode = "CRJ-ELSEWHERE"
				return req
			},
			http.StatusBadRequest, func(*clearRig) string { return "CRJ-ELSEWHERE is not a buffer position of the loader at CRJ-H" }},
		{"a named spot holding no partial", "CRK", true,
			func(t *testing.T, r *clearRig) protocol.ClearHomeRequest {
				req := home(r)
				req.PartialNode = r.x.Name
				return req
			},
			http.StatusBadRequest, func(*clearRig) string { return "CRK-X holds no partial of CRK-PART" }},
		{"no spot holding a partial", "CRL", true,
			func(t *testing.T, r *clearRig) protocol.ClearHomeRequest {
				setUOP(t, r, r.p, 0)
				return home(r)
			},
			http.StatusBadRequest, func(*clearRig) string { return "no buffer spot holds a partial of CRL-PART" }},
		{"a trade whose spot does not take the carrier", "CRM", false,
			func(t *testing.T, r *clearRig) protocol.ClearHomeRequest {
				other := &bins.BinType{Code: "CRM-OTHER"}
				testutil.MustNoErr(t, r.db.CreateBinType(other), "another carrier type")
				testutil.MustNoErr(t, r.db.SetLoaderHomeBinTypes(r.loaderID, r.s.ID, []int64{other.ID}), "S takes the other type only")
				return home(r)
			},
			http.StatusBadRequest, func(*clearRig) string {
				return "no buffer spot is open and CRM-S does not take CRM-E's carrier type DEFAULT"
			}},
		{"a trade whose home does not take the partial", "CRN", false,
			func(t *testing.T, r *clearRig) protocol.ClearHomeRequest {
				other := &bins.BinType{Code: "CRN-OTHER"}
				testutil.MustNoErr(t, r.db.CreateBinType(other), "another carrier type")
				testutil.MustNoErr(t, r.db.SetLoaderHomeBinTypes(r.loaderID, r.home.ID, []int64{other.ID}), "H takes the other type only")
				return home(r)
			},
			http.StatusBadRequest, func(*clearRig) string {
				return "no buffer spot is open and CRN-H does not take CRN-P's carrier type DEFAULT"
			}},
		{"a trade inside a lane", "CRO", false,
			func(t *testing.T, r *clearRig) protocol.ClearHomeRequest {
				_, lanes, _ := laneGroup(t, r.db, "CRO", 1, 1)
				_, err := r.db.DB.Exec(`UPDATE nodes SET parent_id=$1, depth=2 WHERE id=$2`, lanes[0].ID, r.s.ID)
				testutil.MustNoErr(t, err, "S is a slot of the lane")
				return home(r)
			},
			http.StatusBadRequest, func(*clearRig) string {
				return "no buffer spot is open and CRO-S is a slot of lane CRO-L0, and two robots cannot trade inside a lane"
			}},
		{"a birth the database refuses", "CRP", true,
			func(t *testing.T, r *clearRig) protocol.ClearHomeRequest {
				_, err := r.db.DB.Exec(fmt.Sprintf(`
					CREATE FUNCTION crp_refuse() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'door fault'; END $$ LANGUAGE plpgsql;
					CREATE TRIGGER crp_refuse BEFORE UPDATE OF payload_code ON bins FOR EACH ROW
					  WHEN (OLD.id = %d) EXECUTE FUNCTION crp_refuse();`, r.e.ID))
				testutil.MustNoErr(t, err, "the fault")
				return home(r)
			},
			http.StatusInternalServerError, func(r *clearRig) string {
				return fmt.Sprintf("could not start the clear: clear manifest bin %d: ERROR: door fault (SQLSTATE P0001)", r.e.ID)
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newClearRig(t, tc.prefix, tc.openSpot)
			rows := messaging.NewCoreDataService(r.db, nil, service.EpochAnnounce{}).NodeBinsRows
			r.eng.SetBinsRowsFunc(rows)
			req := tc.arrange(t, r)
			before := r.snapDoor(t)
			ans := r.eng.ClearLoaderHome(clearStation, req)
			// The refusal carries Core's current rows for the home and the spot it
			// named: the rows the snapshot's own code reads now (the refusal
			// wrote nothing), the home's first.
			touched := []string{req.HomeNode}
			if req.PartialNode != "" {
				touched = append(touched, req.PartialNode)
			}
			now, err := rows(touched)
			testutil.MustNoErr(t, err, "the rows now")
			if len(now) == 0 || now[0].Node != req.HomeNode {
				t.Fatalf("rows now = %+v, want the home's first", now)
			}
			want := ClearHomeAnswer{Status: tc.status, Body: protocol.ClearHomeResponse{Status: "error", Error: tc.sentence(r), Rows: now}}
			if !reflect.DeepEqual(ans, want) {
				t.Errorf("answer:\n got %d %+v\nwant %d %+v", ans.Status, ans.Body, want.Status, want.Body)
			}
			if after := r.snapDoor(t); !reflect.DeepEqual(after, before) {
				t.Errorf("a refusal wrote:\n before %+v\n  after %+v", before, after)
			}
			testdb.AssertNoHoldsAtRest(t, r.db, "after the refusal")
		})
	}
}
