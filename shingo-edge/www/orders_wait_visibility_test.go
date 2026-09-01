package www

import (
	"bytes"
	"html/template"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingoedge/domain"
)

// renderOrdersBody renders the orders table partial exactly as the page and the
// htmx refresh do — one template set, one data map, no second copy of the row
// markup.
func renderOrdersBody(t *testing.T, data map[string]any) string {
	t.Helper()
	tmpl := template.Must(template.New("").Funcs(templateFuncs()).ParseFS(
		templatesFS, "templates/*.html", "templates/partials/*.html"))
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "orders-body", data); err != nil {
		t.Fatalf("render orders-body: %v", err)
	}
	return buf.String()
}

// TestEdgeOrders_ASourcingOrderSaysWhyAndForHowLong is the owner requirement on
// the station's own board: "I want the shingo operators to see what's sourcing
// and why."
//
// The sentence already crosses the wire and already renders — Core pushes a
// reason for the whole acquiring set (engine/wiring.go) and the Edge keeps one
// for the same set (messaging/edge_handler.go), both keyed on IsAcquiring rather
// than on `queued`. What the row could not say is HOW LONG, and a cause with no
// duration beside it reads the same at forty seconds and at four hours.
//
// The clock is a LOCAL read: the Edge writes its own order_history row on every
// transition it applies, including the sourcing pushes, so nothing needs to be
// added to the wire to answer it.
func TestEdgeOrders_ASourcingOrderSaysWhyAndForHowLong(t *testing.T) {
	since := time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC)
	orders := []domain.Order{{
		ID: 7, UUID: "edge-sourcing-1", OrderType: protocol.OrderTypeRetrieve,
		Status: protocol.StatusSourcing, DeliveryNode: "ALN_001",
		QueueReason: "Waiting for material: PART-A", QueueCode: "waiting_for_material",
	}}

	html := renderOrdersBody(t, map[string]any{
		"ActiveOrders": orders,
		"WaitSince":    map[int64]string{7: since.Format(time.RFC3339)},
	})

	if !strings.Contains(html, "Waiting for material: PART-A") {
		t.Error("a sourcing order's wait sentence is not rendered")
	}
	if !strings.Contains(html, "orders-wait-since") {
		t.Errorf("the parked order carries no wait clock. An operator cannot tell a busy plant "+
			"from a wedged one without one.\n%s", html)
	}
	if !strings.Contains(html, `data-since="`+since.Format(time.RFC3339)+`"`) {
		t.Errorf("the clock is not a live duration span at the wait's own instant — data-since is "+
			"the contract shared/utils.js installLiveDurations reads, and the staged countdown and "+
			"fault line on this same row already use it.\n%s", html)
	}
}

// TestEdgeOrders_AnOrderNotWaitingCarriesNoClock is the selectivity half: the
// clock belongs to a wait, and a ticking duration on an order the fleet is
// carrying reads as a stall.
func TestEdgeOrders_AnOrderNotWaitingCarriesNoClock(t *testing.T) {
	orders := []domain.Order{{
		ID: 8, UUID: "edge-moving-1", OrderType: protocol.OrderTypeRetrieve,
		Status: protocol.StatusInTransit, DeliveryNode: "ALN_001",
	}}
	html := renderOrdersBody(t, map[string]any{
		"ActiveOrders": orders,
		"WaitSince":    map[int64]string{},
	})
	if strings.Contains(html, "orders-wait-since") {
		t.Errorf("an in-transit order carries a wait clock:\n%s", html)
	}
}
