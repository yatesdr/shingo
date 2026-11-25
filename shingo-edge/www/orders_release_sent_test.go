package www

import (
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingoedge/domain"
)

// TestEdgeOrders_AStagedOrderWhoseReleaseWentOutOffersNoRelease: a Core that
// versions its orders keeps a released leg staged until its robot moves, so
// the admin Orders row reads the sent release (domain.Order.ReleaseSent) to
// drop its Release button; a staged order whose release has not gone out keeps
// it. Both rows still offer the cancel.
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
		{`data-action="cancelOrder:21"`, true},
		{`data-action="cancelOrder:22"`, true},
	} {
		if got := strings.Contains(html, c.action); got != c.want {
			t.Errorf("the rows carry %s: %v, want %v\n%s", c.action, got, c.want, html)
		}
	}
}

// TestEdgeOrders_ADeliveredOrderOffersConfirmAndNoCancel: no cancel on a
// delivered order, or past it (the owner's ruling of 2026-11-25, final brief
// 1e). A delivered row offers Confirm and not the cancel, which the abort door
// refuses there (orders.Manager.cancelOrder); a row whose cart is still on its
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
		{`data-action="cancelOrder:31"`, true},
		{`data-action="confirmDelivery:31:`, false},
		{`data-action="cancelOrder:32"`, false},
		{`data-action="confirmDelivery:32:1"`, true},
		{`data-action="cancelOrder:33"`, false},
		{`data-action="confirmDelivery:33:`, false},
	} {
		if got := strings.Contains(html, c.action); got != c.want {
			t.Errorf("the rows carry %s: %v, want %v\n%s", c.action, got, c.want, html)
		}
	}
}

// TestEdgeOrders_ACancelSentWaitsForCoreOnTheRow: a cancel this station sent
// and Core has not answered (domain.Order.CancelSent) keeps the row at Core's
// status, and the row says "Cancel sent" with the clock since the tap in the
// Cancel button's place, so a second tap cannot send it again. A cancel Core
// refused (domain.Order.CancelRefused) shows Core's sentence under the row,
// which offers what its status allows: a delivered row its Confirm.
func TestEdgeOrders_ACancelSentWaitsForCoreOnTheRow(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	html := renderOrdersBody(t, map[string]any{
		"ActiveOrders": []domain.Order{
			{ID: 41, UUID: "edge-cancel-sent", OrderType: protocol.OrderTypeRetrieve,
				Status: protocol.StatusInTransit, DeliveryNode: "ALN_001", Quantity: 1, CancelSent: true, CancelSentAt: &at},
			{ID: 42, UUID: "edge-cancel-refused", OrderType: protocol.OrderTypeRetrieve,
				Status: protocol.StatusDelivered, DeliveryNode: "ALN_002", Quantity: 1,
				CancelRefused: "the cart was set down at ALN_002"},
		},
		"WaitSince": map[int64]string{},
	})
	for _, c := range []struct {
		text string
		want bool
	}{
		{`data-action="cancelOrder:41"`, false},
		{`Cancel sent · <span class="tnum" data-since="2026-11-25T12:00:00Z"></span>`, true},
		{`data-action="cancelOrder:42"`, false},
		{`data-action="confirmDelivery:42:1"`, true},
		{`Not cancelled: the cart was set down at ALN_002`, true},
	} {
		if got := strings.Contains(html, c.text); got != c.want {
			t.Errorf("the rows carry %s: %v, want %v\n%s", c.text, got, c.want, html)
		}
	}
}
