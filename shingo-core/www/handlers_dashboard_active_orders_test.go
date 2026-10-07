//go:build docker

package www

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/store/orders"
)

// W4 (ui-cleanup 2026-10-07): the Dashboard's Active Orders card is swapped in
// from GET /dashboard-active-orders on order-update. A refreshed card must be
// the reloaded one, byte for byte, and the count the answer carries must be
// the strip's "Active orders" figure, or the two disagree after the first swap.

func dashboardPage(t *testing.T, h *Handlers) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.handleDashboard(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard: status %d", rec.Code)
	}
	return rec.Body.String()
}

func dashboardCard(t *testing.T, h *Handlers) (string, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.handleDashboardActiveOrders(rec, httptest.NewRequest(http.MethodGet, "/dashboard-active-orders", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard-active-orders: status %d, body %q", rec.Code, rec.Body.String())
	}
	return rec.Body.String(), rec.Header().Get("X-Active-Orders-Count")
}

func TestDashboardActiveOrders_RefreshedCardIsTheReloadedCard(t *testing.T) {
	t.Parallel()
	h, db := testHandlersForPages(t)

	// Empty plant: the "No active orders." card, count 0.
	card, count := dashboardCard(t, h)
	if !strings.Contains(card, `<div class="card" id="dash-active-orders">`) || !strings.Contains(card, "No active orders.") {
		t.Fatalf("empty card = %q", card)
	}
	if count != "0" {
		t.Errorf("empty X-Active-Orders-Count = %q, want 0", count)
	}
	if page := dashboardPage(t, h); !strings.Contains(page, strings.TrimSpace(card)) {
		t.Errorf("the empty card the GET answers is not the one the page renders")
	}

	// One order the fleet is carrying, one parked with a wait sentence and its
	// wait clock (waitSinceFor's history read), so every cell kind is in it.
	moving := &orders.Order{
		EdgeUUID: "w4-moving", StationID: "line-1", OrderType: "complex",
		Status: "in_transit", Quantity: 1, SourceNode: "UTN_014", ProcessNode: "ALN_003",
		DeliveryNode: "UTN_013", PayloadCode: "SHIM",
	}
	testutil.MustNoErr(t, db.CreateOrder(moving), "create moving order")
	parked := parkOrder(t, db, "w4-parked", protocol.StatusSourcing,
		protocol.QueueWaitingForMaterial, "Waiting for material: PART-A")
	_, err := db.DB.Exec(`UPDATE order_history SET created_at = NOW() - INTERVAL '20 minutes' WHERE order_id=$1`, parked.ID)
	testutil.MustNoErr(t, err, "backdate the episode")

	card, count = dashboardCard(t, h)
	page := dashboardPage(t, h)
	trimmed := strings.TrimSpace(card)
	if !strings.HasPrefix(trimmed, `<div class="card" id="dash-active-orders">`) {
		t.Fatalf("card does not open with the card element: %q", trimmed[:min(len(trimmed), 120)])
	}
	if !strings.Contains(page, trimmed) {
		t.Errorf("W4: the refreshed card differs from the reloaded one.\nGET:\n%s\nPAGE:\n%s", card, page)
	}
	for _, want := range []string{
		`<a href="/orders?open=` + strconv.FormatInt(moving.ID, 10) + `">`,
		`<a href="/orders?open=` + strconv.FormatInt(parked.ID, 10) + `">`,
		"Waiting for material: PART-A",
		`class="tnum orders-wait-since" data-since="`,
	} {
		if !strings.Contains(card, want) {
			t.Errorf("card lacks %q", want)
		}
	}

	// The count rides the answer and is the strip's figure.
	if count != "2" {
		t.Errorf("X-Active-Orders-Count = %q, want 2", count)
	}
	if !strings.Contains(page, `id="cs-orders-val">`+count+`</span><span class="cs-lbl">Active orders</span>`) {
		t.Errorf("the strip's Active orders figure is not the count the card's answer carries (%s)", count)
	}
}
