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
