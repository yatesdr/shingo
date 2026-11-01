//go:build docker

package engine

import (
	"encoding/json"
	"errors"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/dispatch/binresolver"
	"shingocore/dispatch/plan"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/kanban"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// kanban_pins_keeper_docker_test.go — characterization pins of the keeper and of
// what an order records of where its carrier came from, at today's schema: the
// gaps the existing suites leave.

// kanbanAsksOf is the orders an episode asked.
func kanbanAsksOf(t *testing.T, db *store.DB, origin string) []*orders.Order {
	t.Helper()
	asks, _, err := db.ListOrdersByOrigin(origin, 10)
	testutil.MustNoErr(t, err, "the episode's asks")
	return asks
}

// TestKanbanPin_TheKeeperFollowsTheDeclaredLevel: the asks, the episode's stored
// threshold and the settle edge each read the row's want (maintainer.go:273, :301).
// Raised, the same episode asks the difference and keeps the threshold it opened
// with. Lowered to what stands there with nothing outstanding, the episode closes.
func TestKanbanPin_TheKeeperFollowsTheDeclaredLevel(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, simulator.New())
	grpID, btID := mntFixture(t, db, "KPW", 4, "KPW-T", 1)
	for _, n := range []string{"KPW-OUT1", "KPW-OUT2", "KPW-OUT3"} {
		mntOutsideEmpty(t, db, btID, n)
	}
	m := eng.Maintainer()

	m.Tick()
	st := mntState(t, m, "KPW", "KPW-T")
	if st.Want != 1 || st.Created != 1 {
		t.Fatalf("at a level of 1: want %d, created %d; want 1 and 1", st.Want, st.Created)
	}
	origin := st.OriginID
	opened, err := db.GetDemandOrigin(origin)
	testutil.MustNoErr(t, err, "the episode")
	if opened.Threshold != 1 {
		t.Errorf("the episode's threshold = %d, want the level it opened at, 1", opened.Threshold)
	}

	testutil.MustNoErr(t, db.SetMaintainLevel(store.MaintainLevel{GroupNodeID: grpID, BinTypeID: btID, Want: 3}), "raise")
	m.Tick()
	st = mntState(t, m, "KPW", "KPW-T")
	if st.Want != 3 || st.Created != 2 || st.OriginID != origin {
		t.Errorf("raised to 3: want %d, created %d, origin %s; want 3, 2, the same episode %s", st.Want, st.Created, st.OriginID, origin)
	}
	if after, err := db.GetDemandOrigin(origin); err != nil || after.Threshold != 1 {
		t.Errorf("after the raise the stored threshold is %+v (%v), want still 1", after, err)
	}

	// The asks done with, one carrier standing, the level lowered to it.
	terminaliseAsks(t, db, origin)
	landCarrier(t, db, btID, freePosition(t, db, grpID), "KPW-LANDED")
	testutil.MustNoErr(t, db.SetMaintainLevel(store.MaintainLevel{GroupNodeID: grpID, BinTypeID: btID, Want: 1}), "lower")
	m.Tick()
	st = mntState(t, m, "KPW", "KPW-T")
	if st.Want != 1 || st.OriginID != "" {
		t.Errorf("lowered to what stands there: want %d, origin %q; want 1 and the episode closed", st.Want, st.OriginID)
	}
	closed, err := db.GetDemandOrigin(origin)
	testutil.MustNoErr(t, err, "the episode after")
	if closed.CloseReason != protocol.CloseReasonRecovered {
		t.Errorf("close reason %q, want %q", closed.CloseReason, protocol.CloseReasonRecovered)
	}
}

// TestKanbanPin_ALevelZeroRowRefusesEveryStore: a row at 0 is a declared value and
// no one's room: the resolver and the capacity gate both refuse a store of its
// type into a group with free positions, and the keeper keeps the line and asks
// nothing. A group whose only row is at 0 refuses a store that states no type as
// well (the group-total arm, 0 of 0), and one with another row short does not.
func TestKanbanPin_ALevelZeroRowRefusesEveryStore(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, simulator.New())
	grpID, types := campaignGroup(t, db, "KPZ", 4, map[string]int{"KPZ-ZERO": 0, "KPZ-ONE": 1})
	grp, err := db.GetNode(grpID)
	testutil.MustNoErr(t, err, "the group")
	d := eng.Dispatcher()
	store0 := func(g *nodes.Node, stated binresolver.BinTypeStatement) error {
		_, err := d.PlanGroupStoreSlot(g, "", stated)
		return err
	}

	if err := store0(grp, binresolver.KnownBinType(types["KPZ-ZERO"])); !errors.Is(err, kanban.ErrAtLevel) {
		t.Errorf("a store of the type at 0, with four positions free: %v, want at its declared level", err)
	}
	if err := store0(grp, binresolver.KnownBinType(types["KPZ-ONE"])); err != nil {
		t.Errorf("a store of the type short beside it: %v, want room", err)
	}
	if err := store0(grp, binresolver.NoBinType); err != nil {
		t.Errorf("a store stating no type, the group's total short: %v, want room", err)
	}

	// The keeper: the line stands, at 0, with nothing asked.
	mntOutsideEmpty(t, db, types["KPZ-ZERO"], "KPZ-OUT")
	m := eng.Maintainer()
	m.Tick()
	st := mntState(t, m, "KPZ", "KPZ-ZERO")
	if st.Want != 0 || st.Gap != 0 || st.Created != 0 || st.OriginID != "" {
		t.Errorf("the keeper at 0: %+v, want a line with nothing short, asked or open", st)
	}

	// The gate, on a push holding a carrier of the type at 0.
	dock := &nodes.Node{Name: "KPZ-DOCK", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dock), "the pusher's node")
	cart := landAt(t, db, types["KPZ-ZERO"], dock.ID, "KPZ-PUSHED")
	push := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "kpz-push"
		o.OrderType = dispatch.OrderTypeMove
		o.SourceNode = dock.Name
		o.DeliveryNode = grp.Name
		o.HoldFromBirth(cart)
	})
	testdb.ReserveBin(t, db, push.ID, cart)
	if blocked, v := dispatch.CheckDropoffCapacity(db, grp.Name, push.ID); !blocked || v.Cause != dispatch.CauseNGRPAtLevel {
		t.Errorf("the gate: blocked %v, cause %q; want refused at its level", blocked, v.Cause)
	}

	// Only the row at 0: the total is 0 of 0.
	only, typesOnly := campaignGroup(t, db, "KPZ2", 4, map[string]int{"KPZ2-ZERO": 0})
	onlyGrp, err := db.GetNode(only)
	testutil.MustNoErr(t, err, "the second group")
	if err := store0(onlyGrp, binresolver.NoBinType); !errors.Is(err, kanban.ErrAtLevel) {
		t.Errorf("a store stating no type into a group whose only row is at 0: %v, want at its declared level", err)
	}
	if err := store0(onlyGrp, binresolver.KnownBinType(typesOnly["KPZ2-ZERO"])); !errors.Is(err, kanban.ErrAtLevel) {
		t.Errorf("a store of the type at 0: %v, want at its declared level", err)
	}
}

// TestKanbanPin_TheKeepersAskIsBornWithNoSource: the keeper's order names no
// source at its birth (the concrete slot it fills is its delivery), the scanner
// writes the node the chosen bin stands on at acquire, the slot and never its
// group, and the delivered row keeps that through its confirm. An unstarted engine
// with no scanner shows the row as it is born; a started one takes it from there.
func TestKanbanPin_TheKeepersAskIsBornWithNoSource(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sim := simulator.New()
	eng := newUnstartedEngine(t, db, sim)
	resolver := &dispatch.DefaultResolver{DB: db}
	eng.dispatcher = eng.newDispatcher(&dispatchEmitter{bus: eng.Events, engine: eng}, resolver)
	grpID, btID := mntFixture(t, db, "KPB", 3, "KPB-T", 1)
	lenderID, err := nodes.CreateGroup(db.DB, "KPB-LENDER")
	testutil.MustNoErr(t, err, "the lending group")
	testdb.MarkGroup(t, db, lenderID)
	lendSlot := &nodes.Node{Name: "KPB-LENDER-P1", Enabled: true, ParentID: &lenderID}
	testutil.MustNoErr(t, db.CreateNode(lendSlot), "the lending group's slot")
	bin := landAt(t, db, btID, lendSlot.ID, "KPB-LENT")

	m := eng.Maintainer()
	m.Tick()
	st := mntState(t, m, "KPB", "KPB-T")
	asks := kanbanAsksOf(t, db, st.OriginID)
	if st.Created != 1 || len(asks) != 1 {
		t.Fatalf("created %d, %d asks; want one", st.Created, len(asks))
	}
	ask := asks[0]
	if ask.SourceNode != "" {
		t.Errorf("the ask is born sourcing %q, want no source", ask.SourceNode)
	}
	slot, err := db.GetNodeByDotName(ask.DeliveryNode)
	testutil.MustNoErr(t, err, "the ask's delivery")
	if slot.ParentID == nil || *slot.ParentID != grpID || ask.Status != protocol.StatusPending {
		t.Errorf("the ask is %s to %s, want pending to a position of the group", ask.Status, ask.DeliveryNode)
	}

	started := newTestEngine(t, db, sim)
	started.RunFulfillmentScan()
	got, err := db.GetOrder(ask.ID)
	testutil.MustNoErr(t, err, "the ask, acquired")
	gotCart := testdb.OnlyCart(t, got)
	if got.SourceNode != lendSlot.Name || gotCart == nil || *gotCart != bin {
		t.Fatalf("acquired: source %q, bin %v; want the slot %s the bin %d stands on", got.SourceNode, gotCart, lendSlot.Name, bin)
	}

	sim.DriveSimpleLifecycle(got.VendorOrderID)
	started.Dispatcher().HandleOrderReceipt(&protocol.Envelope{Src: protocol.Address{Role: protocol.RoleEdge, Station: got.StationID}},
		&protocol.OrderReceipt{OrderUUID: got.EdgeUUID, ReceiptType: "confirmed", FinalCount: 1})
	done, err := db.GetOrder(ask.ID)
	testutil.MustNoErr(t, err, "the ask, done")
	if done.Status != protocol.StatusConfirmed || done.SourceNode != lendSlot.Name {
		t.Errorf("done: %s from %q; want confirmed, still from the slot %s", done.Status, done.SourceNode, lendSlot.Name)
	}
}

// p8RefillLeg is the leg BuildTwoRobotPressIndexSwapSteps authors for a produce
// process's R1, unflipped: hold at the press, lift the full off it, take it out,
// fetch an empty from the claim's inbound source (a group), set it on the rear.
func p8RefillLeg(press, out, source, rear string) []protocol.ComplexOrderStep {
	return []protocol.ComplexOrderStep{
		{Action: "wait", Node: press, WaitKind: dispatch.WaitKindStation},
		{Action: "pickup", Node: press},
		{Action: "dropoff", Node: out},
		{Action: "pickup", Node: source, Empty: true},
		{Action: "dropoff", Node: rear},
	}
}

// TestKanbanPin_P8_ARefillStepRecordsItsGroupInStepsJSONAlone: which part of an
// order names the group a press-index refill sourced from. The stored step does,
// and nothing else: steps_json's pickup carries the slot in "node" and the group in
// "group" (plan.Step.Group), through dispatch, release and confirm. source_node is
// the leg's first pickup, the press; order_bins names the slot; no other column of
// the row holds the group's name. A leg that could not resolve at intake stores the
// group as its "node" and no "group" until a pass resolves it, then the same.
func TestKanbanPin_P8_ARefillStepRecordsItsGroupInStepsJSONAlone(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		dryAtFirst bool
	}{
		{"resolved at intake", false},
		{"parked at intake, resolved on a later pass", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testDB(t)
			sd := testdb.SetupStandardData(t, db)
			sim := simulator.New()
			eng := newTestEngine(t, db, sim)
			d := eng.Dispatcher()

			press := sd.LineNode
			out := &nodes.Node{Name: "P8-OUT", Enabled: true}
			testutil.MustNoErr(t, db.CreateNode(out), "the outbound node")
			rear := &nodes.Node{Name: "P8-REAR", Enabled: true}
			testutil.MustNoErr(t, db.CreateNode(rear), "the rear position")
			testdb.CreateBinAtNode(t, db, sd.Payload.Code, press.ID, "P8-FULL")
			grpID, btID := mntFixture(t, db, "P8-K", 3, "P8-T", 1)
			kids, err := db.ListChildNodes(grpID)
			testutil.MustNoErr(t, err, "the group's positions")
			if !tc.dryAtFirst {
				landAt(t, db, btID, kids[1].ID, "P8-EMPTY")
			}

			d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
				OrderUUID: "p8-r1", PayloadCode: sd.Payload.Code, Quantity: 1, ProcessNode: press.Name,
				Steps: p8RefillLeg(press.Name, out.Name, "P8-K", rear.Name)})
			o, err := db.GetOrderByUUID("p8-r1")
			testutil.MustNoErr(t, err, "the leg")

			if tc.dryAtFirst {
				parked := p8Refill(t, o)
				if parked.Node != "P8-K" || parked.Group != "" {
					t.Errorf("parked at intake the refill step is %+v, want the group as its node and no group stamp", parked)
				}
				landAt(t, db, btID, kids[1].ID, "P8-EMPTY")
				eng.RunFulfillmentScan()
				o, err = db.GetOrderByUUID("p8-r1")
				testutil.MustNoErr(t, err, "the leg after the pass")
			}
			if o.Status != dispatch.StatusDispatched {
				t.Fatalf("the leg is %s (%s %q), want dispatched", o.Status, o.QueueCause, o.QueueReason)
			}

			check := func(when string) {
				t.Helper()
				row, err := db.GetOrderByUUID("p8-r1")
				testutil.MustNoErr(t, err, "the leg "+when)
				step := p8Refill(t, row)
				if step.Node != kids[1].Name || step.Group != "P8-K" || !step.Empty {
					t.Errorf("%s: the refill step is %+v, want node %s, group P8-K, empty", when, step, kids[1].Name)
				}
				if row.SourceNode != press.Name {
					t.Errorf("%s: source_node = %q, want the leg's first pickup, %s", when, row.SourceNode, press.Name)
				}
				if row.DeliveryNode != rear.Name {
					t.Errorf("%s: delivery_node = %q, want %s", when, row.DeliveryNode, rear.Name)
				}
				var cols map[string]any
				var raw []byte
				testutil.MustNoErr(t, db.QueryRow(`SELECT to_jsonb(o) FROM orders o WHERE id = $1`, row.ID).Scan(&raw), "the row")
				testutil.MustNoErr(t, json.Unmarshal(raw, &cols), "the row's columns")
				for col, v := range cols {
					if s, ok := v.(string); ok && col != "steps_json" && s == "P8-K" {
						t.Errorf("%s: orders.%s = %q names the group", when, col, s)
					}
				}
				for _, l := range testdb.CartLifts(t, db, row.ID) {
					if l.From == "P8-K" || l.To == "P8-K" {
						t.Errorf("%s: a cart row's step names the group: %+v", when, l)
					}
				}
			}
			check("dispatched")

			sim.DriveState(o.VendorOrderID, "RUNNING")
			sim.DriveState(o.VendorOrderID, "WAITING")
			d.HandleOrderRelease(testEnvelope(), &protocol.OrderRelease{OrderUUID: "p8-r1"})
			check("released")
			lifts := testdb.CartLifts(t, db, o.ID)
			found := false
			for _, l := range lifts {
				found = found || l.From == kids[1].Name || l.To == kids[1].Name
			}
			if !found {
				t.Errorf("no cart row's step names the slot %s: %+v", kids[1].Name, lifts)
			}

			done := driveToConfirmed(t, sim, d, db, "p8-r1")
			if done.Status != dispatch.StatusConfirmed {
				t.Fatalf("the leg ended %s, want confirmed", done.Status)
			}
			check("confirmed")
		})
	}
}

// p8Refill is the leg's refill pickup as stored: its second pickup.
func p8Refill(t *testing.T, o *orders.Order) plan.Step {
	t.Helper()
	var steps []plan.Step
	testutil.MustNoErr(t, json.Unmarshal([]byte(o.StepsJSON), &steps), "the stored steps")
	seen := 0
	for _, s := range steps {
		if s.Action == protocol.ActionPickup {
			if seen++; seen == 2 {
				return s
			}
		}
	}
	t.Fatalf("no second pickup in %s", o.StepsJSON)
	return plan.Step{}
}
