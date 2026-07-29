//go:build docker

package www

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/debuglog"
	"shingo/protocol/testutil"
	"shingocore/engine"
	"shingocore/store"
	"shingocore/store/orders"
)

// handlers_demand_episode_detail_test.go — the episode detail page end to end
// (5.12).
//
// The view rules are pinned without Docker in demand_episode_detail_view_test.go.
// What needs a database is everything BETWEEN the rules and the reader: that the
// route is wired, that a bad id is a bad request rather than an outage, that a
// missing episode is a 404 rather than an empty page, and that the links the
// view builds actually reach the pages they name.

func seedEpisode(t *testing.T, db *store.DB, originID string, opened time.Time) store.DemandOrigin {
	t.Helper()
	o := store.DemandOrigin{
		OriginID: originID,
		Revision: 1,
		// Keyed off originID: demand_origins enforces ONE OPEN EPISODE PER KEY
		// (idx_demand_origins_open_key), so two open fixtures cannot share one.
		EpisodeKey:  "cell|devplant.line1|3|PANEL-" + originID[len(originID)-12:] + "|supply",
		Kind:        "cell",
		Direction:   "supply",
		Trigger:     "autoreorder",
		TriggerRef:  "claim-77",
		StationID:   "devplant.line1",
		ProcessID:   3,
		PayloadCode: "PANEL-A",
		OpenedAt:    opened,
	}
	testutil.MustNoErr(t, db.UpsertDemandOrigin(o), "seed episode")
	return o
}

func seedChild(t *testing.T, db *store.DB, originID, uuid string) *orders.Order {
	t.Helper()
	o := &orders.Order{
		EdgeUUID:     uuid,
		StationID:    "devplant.line1",
		OrderType:    "move",
		Status:       protocol.StatusPending,
		Quantity:     1,
		SourceNode:   "SMN_001",
		DeliveryNode: "PLN_01",
		PayloadCode:  "PANEL-A",
		OriginID:     originID,
		OriginClass:  protocol.OriginClassAttached,
	}
	testutil.MustNoErr(t, db.CreateOrder(o), "seed child order")
	return o
}

// TestEpisodeDetail_MalformedIDIsABadRequestNotAnOutage is the guard on the
// validation that looks like boilerplate and is not.
//
// origin_id is a UUID column. Without the parse guard a typo in the URL reaches
// Postgres, which raises `invalid input syntax for type uuid` — an ERROR, which
// this page renders as "could not read this episode". That is a bad request
// dressed as an outage: the reader is told the system is broken when the truth
// is their link is wrong. Same conflation the whole surface exists to prevent,
// one level up.
func TestEpisodeDetail_MalformedIDIsABadRequestNotAnOutage(t *testing.T) {
	t.Parallel()
	h, _ := testHandlersForPages(t)

	req := chiReq(http.MethodGet, "/demand-episodes/not-a-uuid",
		map[string]string{"originID": "not-a-uuid"})
	rec := httptest.NewRecorder()
	h.handleDemandEpisodeDetail(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "Could not read") {
		t.Error("a malformed id rendered as a read failure — that tells the reader the " +
			"system is broken when their link is simply wrong")
	}
}

// TestEpisodeDetail_UnknownEpisodeIs404 pins the other half of the same
// distinction: the query ran, and there is no such episode.
func TestEpisodeDetail_UnknownEpisodeIs404(t *testing.T) {
	t.Parallel()
	h, _ := testHandlersForPages(t)

	const missing = "0f9a1c22-dead-4000-8000-00000000beef"
	req := chiReq(http.MethodGet, "/demand-episodes/"+missing,
		map[string]string{"originID": missing})
	rec := httptest.NewRecorder()
	h.handleDemandEpisodeDetail(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestEpisodeDetail_RendersChildrenLinkedToTheirMissions is the page doing its
// job: one demand, the orders it spawned, each a way in to the detail that
// already exists for it.
func TestEpisodeDetail_RendersChildrenLinkedToTheirMissions(t *testing.T) {
	t.Parallel()
	h, db := testHandlersForPages(t)

	const originID = "0f9a1c22-1111-4000-8000-000000000001"
	seedEpisode(t, db, originID, time.Now().UTC().Add(-20*time.Minute))
	first := seedChild(t, db, originID, "detail-child-1")
	second := seedChild(t, db, originID, "detail-child-2")

	// An order for a DIFFERENT episode must not appear. The join is origin_id
	// and nothing else; a page that leaked a neighbour's orders would misattribute
	// cost to the demand someone is reading.
	other := "0f9a1c22-2222-4000-8000-000000000002"
	seedEpisode(t, db, other, time.Now().UTC().Add(-20*time.Minute))
	stranger := seedChild(t, db, other, "detail-child-stranger")

	req := chiReq(http.MethodGet, "/demand-episodes/"+originID,
		map[string]string{"originID": originID})
	rec := httptest.NewRecorder()
	h.handleDemandEpisodeDetail(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	for _, o := range []*orders.Order{first, second} {
		if !strings.Contains(body, fmt.Sprintf(`href="%s"`, MissionHref(o.ID))) {
			t.Errorf("child order %d has no link to its mission detail (%s)", o.ID, MissionHref(o.ID))
		}
	}
	if strings.Contains(body, fmt.Sprintf(`href="%s"`, MissionHref(stranger.ID))) {
		t.Errorf("order %d belongs to a different episode and was rendered on this one",
			stranger.ID)
	}
	if !strings.Contains(body, "Orders this demand caused") {
		t.Error("the children section did not render")
	}
	// Two orders, counted at read time.
	if !strings.Contains(body, `<div class="kpi-value tnum">2</div>`) {
		t.Errorf("the order count did not render as a measured 2; body excerpt=%q",
			excerptNear(body, "Orders"))
	}
}

// TestEpisodeDetail_ZeroChildrenIsMeasuredNotUnread pins the other side of the
// rule the view test pins in isolation — that the RENDERED page says which one
// it is.
//
// A demand with no orders against it is the worst reading on this surface. It
// has to be unmistakably a measurement, or the day the child query breaks it
// will be indistinguishable from it.
func TestEpisodeDetail_ZeroChildrenIsMeasuredNotUnread(t *testing.T) {
	t.Parallel()
	h, db := testHandlersForPages(t)

	const originID = "0f9a1c22-3333-4000-8000-000000000003"
	seedEpisode(t, db, originID, time.Now().UTC().Add(-5*time.Minute))

	req := chiReq(http.MethodGet, "/demand-episodes/"+originID,
		map[string]string{"originID": originID})
	rec := httptest.NewRecorder()
	h.handleDemandEpisodeDetail(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "No orders were created for this demand") {
		t.Error("a real zero must get its own empty state, not a bare empty table")
	}
	if !strings.Contains(body, "measured zero") {
		t.Error("the empty state must say the query RAN — otherwise it reads identically " +
			"to the day the query stopped running")
	}
	if strings.Contains(body, "Could not read this episode's orders") {
		t.Error("a measured zero rendered as a read failure")
	}
}

// TestDemandEpisodesListLinksToTheDetailPage pins that the surface is reachable.
//
// A page nobody can navigate to is a page that did not ship. The assertion
// compares against EpisodeDetailHref rather than a literal, so the template and
// the Go helper cannot drift apart into a link that renders and goes nowhere.
func TestDemandEpisodesListLinksToTheDetailPage(t *testing.T) {
	t.Parallel()
	h, db := testHandlersForPages(t)

	const originID = "0f9a1c22-4444-4000-8000-000000000004"
	seedEpisode(t, db, originID, time.Now().UTC().Add(-3*time.Minute))

	req := httptest.NewRequest(http.MethodGet, "/demand-episodes", nil)
	rec := httptest.NewRecorder()
	h.handleDemandEpisodes(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	want := fmt.Sprintf(`href="%s"`, EpisodeDetailHref(originID))
	if !strings.Contains(rec.Body.String(), want) {
		t.Errorf("the list page does not link to %s — the detail surface is unreachable",
			EpisodeDetailHref(originID))
	}
}

// TestEpisodeDetailRouteIsWiredUnderItsOwnParamName is the test that catches the
// way this page could ship completely dead.
//
// The route pattern and chi.URLParam's key are two strings in two files that
// must agree. If they do not, every request gets an empty id, fails the UUID
// parse and returns 400 — with every unit test still green, because they bind
// the param themselves. Only the REAL router can report that, so this builds it.
func TestEpisodeDetailRouteIsWiredUnderItsOwnParamName(t *testing.T) {
	t.Parallel()
	h, db := testHandlersForPages(t)

	const originID = "0f9a1c22-5555-4000-8000-000000000005"
	seedEpisode(t, db, originID, time.Now().UTC().Add(-time.Minute))

	router := realRouterFor(t, h)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, EpisodeDetailHref(originID), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s through the real router: got %d, want 200 — the route pattern and "+
			"chi.URLParam's key have drifted apart; body=%s",
			EpisodeDetailHref(originID), rec.Code, excerptNear(rec.Body.String(), "id"))
	}

	// And the list route still resolves rather than being shadowed by the new
	// pattern beneath it.
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/demand-episodes", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /demand-episodes: got %d, want 200", rec.Code)
	}
}

// realRouterFor builds the PRODUCTION router around the fixture's engine.
//
// Every other handler test in this package binds its chi params by hand, which
// is right for testing a handler and blind to the one thing only the router can
// answer: whether the pattern in router.go and the key the handler reads are the
// same string.
func realRouterFor(t *testing.T, h *Handlers) http.Handler {
	t.Helper()
	eng, ok := h.engine.(*engine.Engine)
	if !ok {
		t.Fatalf("fixture engine is %T, not *engine.Engine — cannot build the real router", h.engine)
	}
	dbg, err := debuglog.New(64, nil)
	if err != nil {
		t.Fatalf("debuglog: %v", err)
	}
	router, stop, err := NewRouter(eng, dbg)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	t.Cleanup(stop)
	return router
}

func excerptNear(body, near string) string {
	i := strings.Index(body, near)
	if i < 0 {
		return body[:min(200, len(body))]
	}
	return body[i:min(i+200, len(body))]
}
