//go:build docker

package www

import (
	"fmt"
	"html/template"
	"net/http"
	"net/http/httptest"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/store"
	"shingocore/store/orders"
)

// TestRouteSurfaces_Statements counts what the three route surfaces read: the
// task board's feed (/api/board/orders), the orders board's rows (/orders/rows)
// and the order pop-up (/api/orders/enriched). The route is drawn from the
// order row and the fleet's robot cache (domain.RouteStops reads nothing), so
// the boards read as many statements for three rows as for one, and the pop-up
// as many for an eight-stop order as for a two-stop one. The counts are logged:
// they are the base a change to how the route is drawn is measured against.
func TestRouteSurfaces_Statements(t *testing.T) {
	t.Parallel()
	h, db, counter := countingHandlers(t)
	page := template.Must(template.New("").Funcs(templateFuncs(h.engine.NodeService(), h.engine)).
		ParseFS(templateFS, "templates/layout.html", "templates/partials/*.html"))
	h.tmpls["orders.html"] = template.Must(page.ParseFS(templateFS, "templates/orders.html"))

	count := func(what string, handler http.HandlerFunc, url string) int {
		t.Helper()
		counter.Reset()
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodGet, url, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", what, rec.Code, rec.Body.String())
		}
		n := len(counter.Statements())
		t.Logf("%s: %d statements", what, n)
		return n
	}

	two := routeOrder(t, db, "route-two", "")
	// The first render reads the station registry for the station names, and
	// the node service keeps it: a statement once, not once a render.
	count("orders rows, first render", h.handleOrdersRows, "/orders/rows")
	boardOne := count("task board, one row", h.handleBoardOrders, "/api/board/orders")
	rowsOne := count("orders rows, one row", h.handleOrdersRows, "/orders/rows")

	four := routeOrder(t, db, "route-four", `[{"action":"wait","node":"PLN_012","wait_kind":"station"},`+
		`{"action":"pickup","node":"PLN_012"},{"action":"dropoff","node":"ULN_002"},`+
		`{"action":"pickup","node":"SMN_BUF_100","empty":true},{"action":"dropoff","node":"PLN_008"}]`)
	eight := routeOrder(t, db, "route-eight", `[{"action":"pickup","node":"SMN_013"},{"action":"dropoff","node":"SLN_005"},`+
		`{"action":"wait","node":"ALN_004"},{"action":"pickup","node":"ALN_004"},{"action":"dropoff","node":"SLN_006"},`+
		`{"action":"pickup","node":"SLN_005"},{"action":"dropoff","node":"ALN_004"},{"action":"pickup","node":"SLN_006"},`+
		`{"action":"dropoff","node":"SMN_007"}]`)
	if got := count("task board, three rows", h.handleBoardOrders, "/api/board/orders"); got != boardOne {
		t.Errorf("task board: %d statements for three rows, %d for one", got, boardOne)
	}
	if got := count("orders rows, three rows", h.handleOrdersRows, "/orders/rows"); got != rowsOne {
		t.Errorf("orders rows: %d statements for three rows, %d for one", got, rowsOne)
	}

	popup := func(o *orders.Order, stops int) int {
		return count(fmt.Sprintf("pop-up, %d stops", stops), h.apiGetOrderEnriched, fmt.Sprintf("/api/orders/enriched?id=%d", o.ID))
	}
	popupTwo := popup(two, 2)
	for stops, o := range map[int]*orders.Order{4: four, 8: eight} {
		if got := popup(o, stops); got != popupTwo {
			t.Errorf("pop-up: %d statements for %d stops, %d for two", got, stops, popupTwo)
		}
	}
}

// routeOrder is an in-transit order with a robot, so the task board lists it,
// and with both ends named, so the pop-up reads the same nodes whatever its
// route.
func routeOrder(t *testing.T, db *store.DB, uuid, steps string) *orders.Order {
	t.Helper()
	o := &orders.Order{EdgeUUID: uuid, StationID: "line-route", OrderType: "move", Status: protocol.StatusInTransit,
		Quantity: 1, SourceNode: "UTN_014", DeliveryNode: "UTN_013", StepsJSON: steps}
	testutil.MustNoErr(t, db.CreateOrder(o), "create "+uuid)
	testutil.MustNoErr(t, db.UpdateOrderRobotID(o.ID, "AMR-01"), "robot for "+uuid)
	return o
}
