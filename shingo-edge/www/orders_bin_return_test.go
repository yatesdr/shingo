package www

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingoedge/domain"
)

// A cancelled order with a bin-return notice prints the one line; an order
// without one prints nothing extra, and a page built without the map renders.
func TestEdgeOrders_CancelledOrderShowsItsBinReturnLine(t *testing.T) {
	t.Parallel()
	orders := []domain.Order{
		{ID: 3, UUID: "edge-cancelled-3", OrderType: protocol.OrderTypeRetrieve, Status: protocol.StatusCancelled},
		{ID: 4, UUID: "edge-cancelled-4", OrderType: protocol.OrderTypeRetrieve, Status: protocol.StatusCancelled},
	}
	html := renderOrdersBody(t, map[string]any{
		"ActiveOrders": orders,
		"BinReturns":   map[int64]string{3: "Bin B7 returning to SMN_001"},
	})
	if strings.Count(html, "Bin B7 returning to SMN_001") != 1 {
		t.Errorf("the bin-return line is not rendered exactly once:\n%s", html)
	}

	html = renderOrdersBody(t, map[string]any{"ActiveOrders": orders})
	if strings.Contains(html, "returning to") {
		t.Errorf("a line rendered with no notice:\n%s", html)
	}
}

// An ended order prints why it ended — Core's reason, one line — and an order
// with none prints nothing extra; a page built without the map renders.
func TestEdgeOrders_AnEndedOrderShowsWhyItEnded(t *testing.T) {
	t.Parallel()
	const why = "fleet order stopped — RDS error 60009: Cannot find path to new block target [PLN_05]"
	orders := []domain.Order{
		{ID: 5, UUID: "edge-cancelled-5", OrderType: protocol.OrderTypeComplex, Status: protocol.StatusCancelled},
		{ID: 6, UUID: "edge-cancelled-6", OrderType: protocol.OrderTypeComplex, Status: protocol.StatusCancelled},
	}
	html := renderOrdersBody(t, map[string]any{
		"ActiveOrders": orders,
		"EndReasons":   map[int64]string{5: why},
	})
	if strings.Count(html, "RDS error 60009") != 1 {
		t.Errorf("the end reason is not rendered exactly once:\n%s", html)
	}
	html = renderOrdersBody(t, map[string]any{"ActiveOrders": orders})
	if strings.Contains(html, "RDS error") {
		t.Errorf("a reason rendered with no map:\n%s", html)
	}
}
