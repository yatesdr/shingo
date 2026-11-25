package www

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingoedge/domain"
)

// TestEdgeOrders_AStagedOrderWhoseReleaseWentOutOffersNoRelease: a Core that
// versions its orders keeps a released leg staged until its robot moves, so
// the admin Orders row reads the sent release (domain.Order.ReleaseSent) to
// drop its Release button; a staged order whose release has not gone out keeps
// it. Both rows still offer Abort.
func TestEdgeOrders_AStagedOrderWhoseReleaseWentOutOffersNoRelease(t *testing.T) {
	t.Parallel()
	html := renderOrdersBody(t, map[string]any{
		"ActiveOrders": []domain.Order{
			{ID: 21, UUID: "edge-staged-released", OrderType: protocol.OrderTypeComplex,
				Status: protocol.StatusStaged, DeliveryNode: "ALN_001", ReleaseSent: true},
			{ID: 22, UUID: "edge-staged-waiting", OrderType: protocol.OrderTypeComplex,
				Status: protocol.StatusStaged, DeliveryNode: "ALN_002"},
		},
		"WaitSince": map[int64]string{},
	})
	for _, c := range []struct {
		action string
		want   bool
	}{
		{`data-action="releaseOrder:21"`, false},
		{`data-action="releaseOrder:22"`, true},
		{`data-action="abortOrder:21"`, true},
		{`data-action="abortOrder:22"`, true},
	} {
		if got := strings.Contains(html, c.action); got != c.want {
			t.Errorf("the rows carry %s: %v, want %v\n%s", c.action, got, c.want, html)
		}
	}
}

// TestEdgeOrders_ADeliveredOrderOffersConfirmAndNoCancel: no cancel on a
// delivered order, or past it (the owner's ruling of 2026-11-25, final brief
// 1e). A delivered row offers Confirm and not the cancel, which the abort door
// refuses there (orders.Manager.abortOrder); a row whose cart is still on its
// way offers the cancel, and an ended one neither. RED at 37146992, where the
// row hid the cancel only for an order that had ended.
func TestEdgeOrders_ADeliveredOrderOffersConfirmAndNoCancel(t *testing.T) {
	t.Parallel()
	html := renderOrdersBody(t, map[string]any{
		"ActiveOrders": []domain.Order{
			{ID: 31, UUID: "edge-in-transit", OrderType: protocol.OrderTypeRetrieve,
				Status: protocol.StatusInTransit, DeliveryNode: "ALN_001", Quantity: 1},
			{ID: 32, UUID: "edge-delivered", OrderType: protocol.OrderTypeRetrieve,
				Status: protocol.StatusDelivered, DeliveryNode: "ALN_002", Quantity: 1},
			{ID: 33, UUID: "edge-confirmed", OrderType: protocol.OrderTypeRetrieve,
				Status: protocol.StatusConfirmed, DeliveryNode: "ALN_003", Quantity: 1},
		},
		"WaitSince": map[int64]string{},
	})
	for _, c := range []struct {
		action string
		want   bool
	}{
		{`data-action="abortOrder:31"`, true},
		{`data-action="confirmDelivery:31:`, false},
		{`data-action="abortOrder:32"`, false},
		{`data-action="confirmDelivery:32:1"`, true},
		{`data-action="abortOrder:33"`, false},
		{`data-action="confirmDelivery:33:`, false},
	} {
		if got := strings.Contains(html, c.action); got != c.want {
			t.Errorf("the rows carry %s: %v, want %v\n%s", c.action, got, c.want, html)
		}
	}
}
