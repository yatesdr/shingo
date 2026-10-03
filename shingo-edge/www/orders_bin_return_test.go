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
