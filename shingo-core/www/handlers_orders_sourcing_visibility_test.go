//go:build docker

package www

import (
	"html/template"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
	"shingocore/store"
	"shingocore/store/orders"
)

// renderOrdersPage runs the real handler — the page's data is assembled there,
// so a test that rendered the template with a hand-built map would be asserting
// its own fixture. The template set is loaded exactly as router.go loads it.
func renderOrdersPage(t *testing.T, h *Handlers, query string) string {
	t.Helper()
	if len(h.tmpls) == 0 {
		base := template.New("").Funcs(templateFuncs(h.engine.NodeService()))
		base = template.Must(base.ParseFS(templateFS, "templates/layout.html", "templates/partials/*.html"))
		pages, err := fs.Glob(templateFS, "templates/*.html")
		testutil.MustNoErr(t, err, "glob templates")
		for _, p := range pages {
			name := p[len("templates/"):]
			if name == "layout.html" {
				continue
			}
			clone := template.Must(base.Clone())
			h.tmpls[name] = template.Must(clone.ParseFS(templateFS, p))
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/orders"+query, nil)
	rec := httptest.NewRecorder()
	h.handleOrders(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("orders page status = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

// parkOrder seeds an order resting in an acquiring status under a cause, the way
// a real park leaves it: the live columns written through the one writer, so the
// history row for the episode gets its code too.
func parkOrder(t *testing.T, db *store.DB, uuid string, status protocol.Status, code protocol.QueueCode, reason string) *orders.Order {
	t.Helper()
	o := &orders.Order{
		EdgeUUID: uuid, StationID: "line-1", OrderType: protocol.OrderTypeRetrieve,
		Status: status, Quantity: 1, DeliveryNode: "LINE1-IN", PayloadCode: "PART-A",
	}
	testutil.MustNoErr(t, db.CreateOrder(o), "create "+uuid)
	testutil.MustNoErr(t, db.SetOrderQueueDetail(o.ID, reason, code, "test-cause"), "park "+uuid)
	return o
}

// TestOrdersPage_ASourcingOrderSaysWhyItIsWaiting is the owner requirement on
// the Core board: "I want the shingo operators to see what's sourcing and why."
//
// The facts have always been there — every sourcing park writes a cause through
// the same one writer a queued park does — so what this asserts is the
// RENDERING: a sourcing order reads on the page exactly as a queued one does,
// its own wait sentence and its own clock, and the summary that counts both does
// not describe itself as being about only one of them.
func TestOrdersPage_ASourcingOrderSaysWhyItIsWaiting(t *testing.T) {
	t.Parallel()
	h, db := testHandlersWithSim(t, simulator.New())

	parkOrder(t, db, "sv-queued", protocol.StatusQueued,
		protocol.QueueStorageRearranging, "Storage is being rearranged at ALN_001")
	parkOrder(t, db, "sv-sourcing", protocol.StatusSourcing,
		protocol.QueueWaitingForMaterial, "Waiting for material: PART-A")

	body := renderOrdersPage(t, h, "?status=all")

	if !strings.Contains(body, "Storage is being rearranged at ALN_001") {
		t.Error("the queued order's wait sentence is not on the page at all")
	}
	if !strings.Contains(body, "Waiting for material: PART-A") {
		t.Error("a SOURCING order's wait sentence is not on the page. It is written by the same " +
			"writer as a queued order's, and the operator has no way to see what is sourcing or why")
	}

	// The summary chips count the whole acquiring set (countQueueCodes keys on
	// IsAcquiring), so a label naming one half of it describes the wrong number.
	if strings.Contains(body, "Why queued:") {
		t.Error("the wait summary still calls itself \"Why queued\" while counting sourcing orders " +
			"too — the label names one rung of the set it is a summary of")
	}
}

// TestOrdersPage_AWaitCarriesItsOwnClock pins the third part of the sentence:
// status word, wait sentence, HOW LONG.
//
// The clock has to come from the history row of the episode the order is resting
// in. orders.updated_at is the wrong instrument and reconciliation.go says why at
// length: about twenty writers move it, several on the retry loop, so the order
// going nowhere fastest refreshes its own timer. The episode row is the one
// SetQueueDetail stamps the cause onto, which is what makes it the start of THIS
// wait rather than of the order.
func TestOrdersPage_AWaitCarriesItsOwnClock(t *testing.T) {
	t.Parallel()
	h, db := testHandlersWithSim(t, simulator.New())

	o := parkOrder(t, db, "sv-clock", protocol.StatusSourcing,
		protocol.QueueWaitingForMaterial, "Waiting for material: PART-A")
	// Resting in this episode for twenty minutes.
	_, err := db.DB.Exec(
		`UPDATE order_history SET created_at = NOW() - INTERVAL '20 minutes' WHERE order_id=$1`, o.ID)
	testutil.MustNoErr(t, err, "backdate the episode")

	body := renderOrdersPage(t, h, "?status=all")

	if !strings.Contains(body, "orders-wait-since") {
		t.Fatalf("the parked order carries no wait clock. A wait sentence with no duration beside it "+
			"cannot tell an operator whether the plant is busy or wedged.\nPage:\n%s", body)
	}
	if !strings.Contains(body, "data-since=") {
		t.Error("the clock is not a live duration span — data-since is the contract " +
			"shared/utils.js installLiveDurations reads, and every other clock on this page uses it")
	}
}

// TestOrdersPage_ADispatchedOrderCarriesNoWaitClock is the selectivity half.
// The clock belongs to a wait; an order the fleet is carrying is not waiting,
// and hanging a ticking duration on it would read as a stall.
func TestOrdersPage_ADispatchedOrderCarriesNoWaitClock(t *testing.T) {
	t.Parallel()
	h, db := testHandlersWithSim(t, simulator.New())

	o := parkOrder(t, db, "sv-moving", protocol.StatusQueued,
		protocol.QueueWaitingForMaterial, "Waiting for material: PART-A")
	moved, err := db.UpdateOrderStatusFromWithReason(o.ID,
		string(protocol.StatusQueued), string(protocol.StatusDispatched), "on its way", store.HistoryReason{})
	testutil.MustNoErr(t, err, "queued→dispatched")
	if !moved {
		t.Fatal("queued→dispatched refused")
	}

	body := renderOrdersPage(t, h, "?status=all")
	if strings.Contains(body, "orders-wait-since") {
		t.Errorf("a dispatched order carries a wait clock. The clock is for an order that is "+
			"waiting; on one the fleet is already carrying it reads as a stall.\nPage:\n%s", body)
	}
}
