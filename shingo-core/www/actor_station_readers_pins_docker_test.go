//go:build docker

package www

import (
	"encoding/json"
	"net/http"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/dispatch/plan"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// actor_station_readers_pins_docker_test.go — characterisation pins taken on
// the unchanged base (v0.16.0, e06b29b0) before Core's order station column is
// split into a station and an actor (requested_by): what the pages a person
// reads show for five orders. The engine package's
// actor_station_readers_pins_docker_test.go pins the board scope, the scope
// picker, the missions filter and the delivered notice for the same five.
//
// THE FIVE ORDERS (wwwActorOrders) are each born through the door their
// sender really uses, never by writing a station into a row:
//
//	edge      an Edge's retrieve from stn-aaaa1111: the Kafka intake
//	          (Dispatcher.HandleOrderRequest);
//	operator  the orders page's Manual Order: apiManualOrderSubmit, a retrieve;
//	direct    the test-orders page's direct tab: apiDirectOrderSubmit;
//	test      the test-orders page's Kafka tab: the envelope apiTestOrderSubmit
//	          publishes (Edge role, station core-test), taken by the intake the
//	          Kafka loop calls, since these handlers have no broker;
//	core      a Core-born order with no actor: a recovery, issued as the bins
//	          page's Return button issues it (engine orderCarriedBinDown: no
//	          station, no station knowledge, recovers_order_id the cancelled
//	          Edge order whose bin it returns). The issue is called directly
//	          because the button needs a robot in the engine's cache, which this
//	          package cannot place.

// pinStation and pinStationLabel are the fixture's enrolled Edge: a synthetic
// uid and its display name.
const (
	pinStation      = "stn-aaaa1111"
	pinStationLabel = "SYNTH / EDGE-1"
)

// pinKinds names the five orders.
var pinKinds = []string{"edge", "operator", "direct", "test", "core"}

// wwwActorOrders builds the five orders on a started engine and returns their
// ids by kind.
func wwwActorOrders(t *testing.T) (*Handlers, *store.DB, map[string]int64) {
	t.Helper()
	h, db := testHandlersWithSim(t, simulator.New())
	sd := testdb.SetupStandardData(t, db)
	if _, err := db.EnrollEdge(pinStation, pinStationLabel); err != nil {
		t.Fatalf("enroll %s: %v", pinStation, err)
	}
	storType, err := db.GetNodeTypeByCode("STOR")
	testutil.MustNoErr(t, err, "STOR type")
	node := func(name string, storage bool) *nodes.Node {
		n := &nodes.Node{Name: name, Enabled: true}
		if storage {
			n.NodeTypeID, n.Zone = &storType.ID, "A"
		}
		testutil.MustNoErr(t, db.CreateNode(n), "create node "+name)
		return n
	}
	ids := map[string]int64{}
	orderID := func(kind string, body []byte) {
		var out struct {
			OrderID int64 `json:"order_id"`
		}
		if err := json.Unmarshal(body, &out); err != nil || out.OrderID == 0 {
			t.Fatalf("fixture: the %s door made no order: %s", kind, body)
		}
		ids[kind] = out.OrderID
	}

	// direct first, so no retrieve takes the bin its node offers.
	directSrc := node("WPIN-SD", true)
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, directSrc.ID, "WPIN-BIN-D")
	rec := postJSON(t, h.apiDirectOrderSubmit, "/api/test-orders/direct",
		map[string]any{"from_node_id": directSrc.ID, "to_node_id": node("WPIN-L-direct", false).ID})
	orderID("direct", rec.Body.Bytes())

	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "WPIN-BIN-1")
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, node("WPIN-S2", true).ID, "WPIN-BIN-2")
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, node("WPIN-S3", true).ID, "WPIN-BIN-3")

	rec = postJSON(t, h.apiManualOrderSubmit, "/api/orders/spot", map[string]any{
		"order_type": "retrieve", "payload_code": sd.Payload.Code, "delivery_node": node("WPIN-L-operator", false).Name,
	})
	orderID("operator", rec.Body.Bytes())

	intake := func(kind, station, desc string) {
		uuid := "wpin-" + kind
		h.engine.Dispatcher().HandleOrderRequest(
			&protocol.Envelope{Src: protocol.Address{Role: protocol.RoleEdge, Station: station},
				Dst: protocol.Address{Role: protocol.RoleCore, Station: h.engine.AppConfig().Messaging.StationID}},
			&protocol.OrderRequest{OrderUUID: uuid, OrderType: protocol.OrderTypeRetrieve, PayloadCode: sd.Payload.Code,
				Quantity: 1, DeliveryNode: node("WPIN-L-"+kind, false).Name, PayloadDesc: desc})
		ids[kind] = testdb.RequireOrder(t, db, uuid).ID
	}
	intake("edge", pinStation, "")
	intake("test", "core-test", "test order from shingo core")

	carrier := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID, o.StationUID, o.Status = "wpin-carrier", pinStation, protocol.StatusCancelled
	})
	core, _, err := h.engine.Dispatcher().IssueOrder(dispatch.Need{
		Type: protocol.OrderTypeMove, Ref: "wpin-core", Quantity: 1,
		OriginClass: protocol.OriginClassNoDemand, SourceIntent: dispatch.SourceIntentOnDeck,
		Source: sd.StorageNode.Name, Delivery: node("WPIN-L-core", false).Name, Recovers: &carrier.ID,
	}, plan.Plan{})
	testutil.MustNoErr(t, err, "issue the recovery")
	ids["core"] = core.ID

	for _, k := range pinKinds {
		o, err := db.GetOrder(ids[k])
		testutil.MustNoErr(t, err, "read the "+k+" order")
		if protocol.IsTerminal(o.Status) {
			t.Fatalf("fixture: the %s order is %s at birth; the board shows live orders", k, o.Status)
		}
	}
	return h, db, ids
}

// kindsOf sorts the fixture kinds of the order ids, dropping any other order.
func kindsOf(ids map[string]int64, got []int64) []string {
	out := []string{}
	for _, id := range got {
		for _, k := range pinKinds {
			if ids[k] == id {
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out
}

// ordersRowStation matches one orders-board row and its fourth cell, the
// station's label (partials/orders-rows.html).
var ordersRowStation = regexp.MustCompile(`(?s)<tr data-order-id="(\d+)".*?<td[^>]*>.*?</td>\s*<td[^>]*>.*?</td>\s*<td[^>]*>.*?</td>\s*<td>(.*?)</td>`)

// TestPinActorReaders_OrdersBoardStationLabel pins the station cell of the
// orders board (partials/orders-rows.html over NodeService.StationName and
// CoreSourceLabels): the label a person reads for each of the five orders.
// Today an enrolled Edge reads as its display name, each Core actor as its
// "Core · …" label, and the Core-born recovery as "Core · Recovery".
//
// Red when an actor's order loses its label (the cell resolving a station the
// actor has left, which reads blank), when the Edge's order stops resolving
// to its display name, or when the recovery's label changes.
func TestPinActorReaders_OrdersBoardStationLabel(t *testing.T) {
	t.Parallel()
	h, _, ids := wwwActorOrders(t)

	body := renderOrdersPage(t, h, "")
	got := map[string]string{}
	for _, m := range ordersRowStation.FindAllStringSubmatch(body, -1) {
		id, err := strconv.ParseInt(m[1], 10, 64)
		testutil.MustNoErr(t, err, "an order id on the orders page")
		for _, k := range pinKinds {
			if ids[k] == id {
				got[k] = strings.TrimSpace(m[2])
			}
		}
	}
	want := map[string]string{
		"edge":     pinStationLabel,
		"operator": "Core · Manual",
		"direct":   "Core · Direct",
		"test":     "Core · Test",
		"core":     "Core · Recovery",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the orders board's station cells = %q, want %q", got, want)
	}
}

// TestPinActorReaders_ByStationReads pins the two other reads that find an
// order by its station: the test-orders page's two lists
// (apiTestOrdersList, apiDirectOrdersList over ListOrdersByStation) and the
// orders board's search box (?q=, ListFiltered's searchColumns, which carry
// station_id). Today each test-page list shows its own actor's orders and
// nothing else, and a search for a station's stored value (an actor's name,
// an Edge's uid) finds that order; a search for a label finds nothing, since
// the label is never stored.
//
// Red when a test-page list goes empty (the read keyed on a station the actor
// has left) or picks up another order, or when the search stops finding an
// order by its actor.
func TestPinActorReaders_ByStationReads(t *testing.T) {
	t.Parallel()
	h, _, ids := wwwActorOrders(t)

	list := func(handler http.HandlerFunc, path string) []string {
		t.Helper()
		rec := getPlain(t, handler, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: HTTP %d: %s", path, rec.Code, rec.Body.String())
		}
		var rows []struct {
			ID int64 `json:"id"`
		}
		testutil.MustNoErr(t, json.Unmarshal(rec.Body.Bytes(), &rows), "decode "+path)
		got := make([]int64, 0, len(rows))
		for _, r := range rows {
			got = append(got, r.ID)
		}
		return kindsOf(ids, got)
	}
	if got := list(h.apiTestOrdersList, "/api/test-orders"); !reflect.DeepEqual(got, []string{"test"}) {
		t.Errorf("the test page's Kafka list shows %v, want [test]", got)
	}
	if got := list(h.apiDirectOrdersList, "/api/test-orders/direct"); !reflect.DeepEqual(got, []string{"direct"}) {
		t.Errorf("the test page's direct list shows %v, want [direct]", got)
	}

	search := func(q string) []string {
		t.Helper()
		body := renderOrdersPage(t, h, "?status=all&q="+q)
		var got []int64
		for _, m := range ordersRowStation.FindAllStringSubmatch(body, -1) {
			id, err := strconv.ParseInt(m[1], 10, 64)
			testutil.MustNoErr(t, err, "an order id on the orders page")
			got = append(got, id)
		}
		return kindsOf(ids, got)
	}
	for q, want := range map[string][]string{
		pinStation:      {"edge"},
		"core-operator": {"operator"},
		"core-direct":   {"direct"},
		"core-test":     {"test"},
		"Recovery":      {},
	} {
		if got := search(q); !reflect.DeepEqual(got, want) {
			t.Errorf("the orders board searched for %q shows %v, want %v", q, got, want)
		}
	}
}
