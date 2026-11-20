//go:build docker

package engine

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/telemetry"
)

// actor_station_readers_pins_docker_test.go — characterisation pins taken on
// the unchanged base (v0.16.0, e06b29b0) before Core's order station column is
// split into a station and an actor (requested_by). Each pin is what one reader
// gives a person for the same five orders; the split must leave every answer
// here as it is, or the commit that changes one names it as a ruling.
//
// THE FIVE ORDERS (bornThroughTheirDoors) are each born through the door their
// sender really uses, never by writing a station into a row:
//
//	edge      an Edge's retrieve: the Kafka intake (Dispatcher.HandleOrderRequest)
//	          from the Edge station stn-aaaa1111;
//	operator  the orders page's Manual Order (www apiManualOrderSubmit's transport
//	          arm): the same intake, from the Core-role envelope it builds with
//	          station core-operator, stamped no_demand;
//	direct    the test-orders page's direct tab (www apiDirectOrderSubmit): the
//	          one bin-move door, CreateBinMove, with station core-direct;
//	test      the test-orders page's Kafka tab (www apiTestOrderSubmit): the
//	          Edge-role envelope it publishes from station core-test, taken by the
//	          intake the Kafka loop calls;
//	core      a Core-born order with no actor: the bins page's Return button
//	          (RecoverCarriedBin) on a bin left on a robot's deck.
//
// The recovery's carrier is a cancelled Edge order of station edge.test
// (seedCancelledCarry); it is terminal, so it is on no live reader here, but it
// is an order, so ListOrderStations names its station.
//
// Every order is dispatched to the simulator and given a robot (RUNNING), so it
// is on the board; finish then drives each to FINISHED, which writes its
// mission summary and sends its order.delivered.

// pinEdgeStation is the fixture's Edge station: a synthetic uid.
const pinEdgeStation = "stn-aaaa1111"

// actorKinds names the five orders, in the order the pins report them.
var actorKinds = []string{"edge", "operator", "direct", "test", "core"}

// actorOrders is the five orders and the engine that made them.
type actorOrders struct {
	db    *store.DB
	eng   *Engine
	sim   *simulator.SimulatorBackend
	order map[string]*orders.Order
}

// kindOf names the fixture order with this id, or "" for none of them.
func (a *actorOrders) kindOf(id int64) string {
	for _, k := range actorKinds {
		if a.order[k].ID == id {
			return k
		}
	}
	return ""
}

// kinds sorts the fixture kinds of ids, dropping ids that are none of them.
func (a *actorOrders) kinds(ids []int64) []string {
	out := []string{}
	for _, id := range ids {
		if k := a.kindOf(id); k != "" {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// bornThroughTheirDoors builds the five orders, each dispatched to the
// simulator with a robot on it (in_transit).
func bornThroughTheirDoors(t *testing.T) *actorOrders {
	t.Helper()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	stor := storNodeType(t, db)

	node := func(name string, storage bool) *nodes.Node {
		n := &nodes.Node{Name: name, Enabled: true}
		if storage {
			n.NodeTypeID, n.Zone = &stor.ID, "A"
		}
		testutil.MustNoErr(t, db.CreateNode(n), "create node "+name)
		return n
	}
	// One line per order, so no dropoff gate holds one behind another, and a
	// bin of the payload in storage for each retrieve and for the move.
	directSrc := node("APIN-SD", true)
	lines := map[string]*nodes.Node{}
	for _, k := range []string{"edge", "operator", "direct", "test"} {
		lines[k] = node("APIN-L-"+k, false)
	}
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, directSrc.ID, "APIN-BIN-D")

	a := &actorOrders{db: db, eng: eng, sim: sim, order: map[string]*orders.Order{}}

	// direct first, so no retrieve takes the bin its node offers.
	res, err := eng.CreateBinMove(BinMoveRequest{
		Selection:    BinSelectionAuto,
		SourceNodeID: directSrc.ID,
		DestNodeID:   lines["direct"].ID,
		StationID:    "core-direct",
		Desc:         "direct test order from shingo core",
	})
	testutil.MustNoErr(t, err, "the direct tab's bin move")
	a.order["direct"] = mustOrder(t, db, res.OrderID)

	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "APIN-BIN-1")
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, node("APIN-S2", true).ID, "APIN-BIN-2")
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, node("APIN-S3", true).ID, "APIN-BIN-3")
	retrieve := func(kind string, src protocol.Address, origin, desc string) {
		uuid := "apin-" + kind
		d.HandleOrderRequest(&protocol.Envelope{Src: src, Dst: protocol.Address{Role: protocol.RoleCore}},
			&protocol.OrderRequest{OrderUUID: uuid, OrderType: dispatch.OrderTypeRetrieve,
				PayloadCode: sd.Payload.Code, PayloadDesc: desc, Quantity: 1,
				DeliveryNode: lines[kind].Name, OriginClass: origin})
		a.order[kind] = testdb.RequireOrderStatus(t, db, uuid, dispatch.StatusDispatched)
	}
	retrieve("edge", protocol.Address{Role: protocol.RoleEdge, Station: pinEdgeStation}, "", "")
	retrieve("operator", protocol.Address{Role: protocol.RoleCore, Station: "core-operator"},
		protocol.OriginClassNoDemand, "")
	retrieve("test", protocol.Address{Role: protocol.RoleEdge, Station: "core-test"}, "",
		"test order from shingo core")

	grp, _ := storeGroupWithSlot(t, db, "APINR")
	seedClaim(t, db, "PROC-APINR", "STY", "LINE-APINR", "APINR-P", grp.Name, true)
	carried, _ := seedCancelledCarry(t, db, "AMR-APINR", "APINR-P", "LINE-APINR")
	cacheRobot(eng, dispatchableRobot("AMR-APINR"))
	core, _, err := eng.RecoverCarriedBin(carried.ID, "operator:test")
	testutil.MustNoErr(t, err, "the Return button's recovery")
	a.order["core"] = mustOrder(t, db, core.ID)

	// A robot on each, as the fleet reports one: the board lists only orders
	// with a robot.
	for _, k := range actorKinds {
		o := a.order[k]
		robot := "AMR-APIN-" + k
		if k == "core" {
			robot = "AMR-APINR"
		}
		if o.VendorOrderID == "" {
			t.Fatalf("fixture: the %s order (%d, %s) has no fleet order", k, o.ID, o.Status)
		}
		a.sim.DriveStateWithRobot(o.VendorOrderID, "RUNNING", robot)
		a.order[k] = mustOrder(t, db, o.ID)
		if a.order[k].RobotID == "" || a.order[k].Status != protocol.StatusInTransit {
			t.Fatalf("fixture: the %s order is %s with robot %q, want in_transit with a robot",
				k, a.order[k].Status, a.order[k].RobotID)
		}
	}
	return a
}

// finish drives every fixture order to the fleet's FINISHED: each is
// delivered, its mission summary is written and its order.delivered is sent.
func (a *actorOrders) finish(t *testing.T) {
	t.Helper()
	for _, k := range actorKinds {
		o := a.order[k]
		a.sim.DriveState(o.VendorOrderID, "FINISHED")
		if got := mustOrder(t, a.db, o.ID); got.Status != protocol.StatusDelivered && !protocol.IsTerminal(got.Status) {
			t.Fatalf("fixture: the %s order is %s after FINISHED, want delivered or past it", k, got.Status)
		}
	}
}

func mustOrder(t *testing.T, db *store.DB, id int64) *orders.Order {
	t.Helper()
	o, err := db.GetOrder(id)
	testutil.MustNoErr(t, err, "read order")
	return o
}

// TestPinActorReaders_BoardScope pins the dashboard board's area scope
// (GetActiveOrdersWithRobotLocationFiltered over store
// orders.ListActiveBoardFiltered): which of the five orders a board scoped to a
// station shows. Today the scope is an exact match on the order's station, so
// an actor reads as a station: a board scoped to core-operator shows the
// operator's order, and the Core-born order is on no scoped board, only the
// plant-wide one.
//
// Red when a scope stops matching an actor's orders (the filter reading a
// station column the actor has left), when a uid scope picks up an actor's
// order, or when the Core-born order lands on a scoped board.
func TestPinActorReaders_BoardScope(t *testing.T) {
	t.Parallel()
	a := bornThroughTheirDoors(t)

	board := func(scope []string) []string {
		t.Helper()
		rows, err := a.eng.GetActiveOrdersWithRobotLocationFiltered(scope)
		testutil.MustNoErr(t, err, "board")
		ids := make([]int64, 0, len(rows))
		for _, r := range rows {
			ids = append(ids, r.OrderID)
		}
		return a.kinds(ids)
	}
	for _, c := range []struct {
		name  string
		scope []string
		want  []string
	}{
		{"plant-wide", nil, []string{"core", "direct", "edge", "operator", "test"}},
		{"the Edge's uid", []string{pinEdgeStation}, []string{"edge"}},
		{"core-operator", []string{"core-operator"}, []string{"operator"}},
		{"core-direct", []string{"core-direct"}, []string{"direct"}},
		{"core-test", []string{"core-test"}, []string{"test"}},
		{"a uid and an actor", []string{pinEdgeStation, "core-test"}, []string{"edge", "test"}},
		{"every actor", []string{"core-operator", "core-direct", "core-test"}, []string{"direct", "operator", "test"}},
		{"an unknown station", []string{"stn-nobody"}, []string{}},
	} {
		if got := board(c.scope); !reflect.DeepEqual(got, c.want) {
			t.Errorf("board scoped to %s (%v) shows %v, want %v", c.name, c.scope, got, c.want)
		}
	}
}

// TestPinActorReaders_ListOrderStations pins the stations the dashboard scope
// picker is offered from orders (OrderService.ListOrderStations over store
// ListOrderStations): every non-blank station an order carries, sorted. Today
// the three Core actors are offered beside the Edge's uid, and the Core-born
// order, having none, adds nothing. edge.test is the recovery's carrier, a
// cancelled Edge order.
//
// Red when an actor drops out of the list (the read skipping a column the
// actor moved to), when a blank is offered, or when the list stops being the
// distinct sorted set.
func TestPinActorReaders_ListOrderStations(t *testing.T) {
	t.Parallel()
	a := bornThroughTheirDoors(t)
	got, err := a.eng.OrderService().ListOrderStations()
	testutil.MustNoErr(t, err, "list order stations")
	want := []string{"core-direct", "core-operator", "core-test", "edge.test", pinEdgeStation}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListOrderStations = %q, want %q", got, want)
	}
}

// TestPinActorReaders_TelemetryByStation pins the /missions list's station
// filter (MissionService.List over store telemetry.ListMissions) in both of
// its halves: an order in flight, read from orders, and a finished mission,
// read from its mission_telemetry summary. Filtered by a station, the list
// shows exactly the orders that station is today: an actor's filter shows its
// own order, and the Core-born order is under no station, only in the
// unfiltered list.
//
// Red when either half stops matching an actor (the summary writer or the
// filter moving the actor out of the column the other reads), when a uid
// filter picks up an actor's order, or when the Core-born order lands under a
// station.
func TestPinActorReaders_TelemetryByStation(t *testing.T) {
	t.Parallel()
	a := bornThroughTheirDoors(t)

	list := func(station string, inFlight bool) []string {
		t.Helper()
		rows, _, err := a.eng.MissionService().List(telemetry.Filter{StationID: station, Limit: 100})
		testutil.MustNoErr(t, err, "list missions")
		var ids []int64
		for _, r := range rows {
			if r.InFlight != inFlight {
				t.Errorf("order %d (%s) in_flight = %v in the list, want %v", r.OrderID, a.kindOf(r.OrderID), r.InFlight, inFlight)
			}
			ids = append(ids, r.OrderID)
		}
		return a.kinds(ids)
	}
	cases := []struct {
		station string
		want    []string
	}{
		{"", []string{"core", "direct", "edge", "operator", "test"}},
		{pinEdgeStation, []string{"edge"}},
		{"core-operator", []string{"operator"}},
		{"core-direct", []string{"direct"}},
		{"core-test", []string{"test"}},
		{"stn-nobody", []string{}},
	}
	for _, c := range cases {
		if got := list(c.station, true); !reflect.DeepEqual(got, c.want) {
			t.Errorf("in flight: missions filtered by station %q = %v, want %v", c.station, got, c.want)
		}
	}

	a.finish(t)
	for _, k := range actorKinds {
		if m, err := a.eng.MissionService().Telemetry(a.order[k].ID); err != nil || m == nil {
			t.Fatalf("fixture: the %s order has no mission summary after FINISHED (%v)", k, err)
		}
	}
	for _, c := range cases {
		if got := list(c.station, false); !reflect.DeepEqual(got, c.want) {
			t.Errorf("finished: missions filtered by station %q = %v, want %v", c.station, got, c.want)
		}
	}
}

// TestPinActorReaders_MaterialStation pins where an order's order.delivered
// goes (materialStation and handleOrderDelivered's broadcast): the station
// named on the outbox envelope, per order, once the fleet says FINISHED.
//
// Today a station-known order sends to its own station whatever it is, so the
// Edge's order goes to its uid and the two intake actors' orders go to
// core-operator and core-test, stations no Edge answers to. The bin move is
// station-unaware and projects nowhere, and so is a recovery into a storage
// slot: neither sends one. The broadcast "*" is a station-known order with a
// blank station, which today only an Edge envelope with no station on it
// makes; the sixth order here is that one.
//
// Red when an actor's notice is broadcast or dropped (materialStation reading a
// station the actor has left), when the bin move or the recovery starts
// sending, or when the blank-station order stops broadcasting.
func TestPinActorReaders_MaterialStation(t *testing.T) {
	t.Parallel()
	a := bornThroughTheirDoors(t)

	// The sixth order: an Edge envelope that names no station.
	line := &nodes.Node{Name: "APIN-L-blank", Enabled: true}
	testutil.MustNoErr(t, a.db.CreateNode(line), "create line")
	src := &nodes.Node{Name: "APIN-S4", Enabled: true, NodeTypeID: &storNodeType(t, a.db).ID, Zone: "A"}
	testutil.MustNoErr(t, a.db.CreateNode(src), "create source")
	sd := testdb.SetupStandardData(t, a.db)
	testdb.CreateBinAtNode(t, a.db, sd.Payload.Code, src.ID, "APIN-BIN-4")
	a.eng.Dispatcher().HandleOrderRequest(
		&protocol.Envelope{Src: protocol.Address{Role: protocol.RoleEdge}, Dst: protocol.Address{Role: protocol.RoleCore}},
		&protocol.OrderRequest{OrderUUID: "apin-blank", OrderType: dispatch.OrderTypeRetrieve,
			PayloadCode: sd.Payload.Code, Quantity: 1, DeliveryNode: line.Name})
	blank := testdb.RequireOrderStatus(t, a.db, "apin-blank", dispatch.StatusDispatched)
	a.sim.DriveStateWithRobot(blank.VendorOrderID, "RUNNING", "AMR-APIN-blank")

	a.finish(t)
	a.sim.DriveState(blank.VendorOrderID, "FINISHED")

	// The destination as the wire carries it: the envelope's dst station.
	msgs, err := a.db.ListPendingOutbox(1000)
	testutil.MustNoErr(t, err, "list outbox")
	sent := map[string][]string{}
	for _, m := range msgs {
		if m.MsgType != protocol.TypeOrderDelivered {
			continue
		}
		var env protocol.Envelope
		var del protocol.OrderDelivered
		if json.Unmarshal(m.Payload, &env) != nil || json.Unmarshal(env.Payload, &del) != nil {
			t.Fatalf("an order.delivered row does not decode: %s", m.Payload)
		}
		sent[del.OrderUUID] = append(sent[del.OrderUUID], env.Dst.Station)
	}

	want := map[string][]string{
		"edge":     {pinEdgeStation},
		"operator": {"core-operator"},
		"direct":   nil,
		"test":     {"core-test"},
		"core":     nil,
	}
	for _, k := range actorKinds {
		if got := sent[a.order[k].EdgeUUID]; !reflect.DeepEqual(got, want[k]) {
			t.Errorf("the %s order's order.delivered went to %q, want %q", k, got, want[k])
		}
	}
	if got := sent["apin-blank"]; !reflect.DeepEqual(got, []string{protocol.StationBroadcast}) {
		t.Errorf("the blank-station Edge order's order.delivered went to %q, want the broadcast %q",
			got, protocol.StationBroadcast)
	}
}
