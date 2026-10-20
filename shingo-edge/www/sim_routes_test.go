//go:build sim

package www

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"shingo/protocol/clock"
	"shingoedge/config"
	"shingoedge/plc"
	"shingoedge/plc/simwarlink"
)

// TestSimOverrun_RouteSetsListsAndClears pins POST/GET /api/sim/overrun over
// the fake WarLink: a POST sets and clears the named PLC's switch, the GET lists
// the PLCs whose switch is on, and a malformed body or a PLC with no counter is
// refused.
func TestSimOverrun_RouteSetsListsAndClears(t *testing.T) {
	h, r := newTestHandlers(t)
	cfg := config.SimConfig{Enabled: true, Processes: []config.SimProcessConfig{
		{PLCName: "PRESS-1", TagName: "PRESS-1_COUNTER", TickInterval: time.Second},
		{PLCName: "WELD-1", TagName: "WELD-1_COUNTER", TickInterval: time.Second},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := simwarlink.NewFakeClient(ctx, cfg, clock.NewManual(time.Unix(0, 0)))
	mgr := plc.NewManager(nil, config.Defaults(), nil, fake)
	// Discovered as the poll would discover them.
	mgr.SetTagValueForTest("PRESS-1", "PRESS-1_COUNTER", int64(0))
	mgr.SetTagValueForTest("WELD-1", "WELD-1_COUNTER", int64(0))
	h.engine.(*stubEngine).plcMgr = mgr
	r.Route("/api", func(r chi.Router) { h.registerSimRoutes(r) })

	call := func(method, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, "/api/sim/overrun", strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	listed := func() []string {
		t.Helper()
		code, body := call(http.MethodGet, "")
		var got struct {
			PLCs []string `json:"plcs"`
		}
		if code != http.StatusOK || json.Unmarshal([]byte(body), &got) != nil {
			t.Fatalf("GET = %d %s", code, body)
		}
		return got.PLCs
	}

	if got := listed(); len(got) != 0 {
		t.Fatalf("listed %v before any set; want none", got)
	}
	if code, body := call(http.MethodPost, `{"plc_name":"PRESS-1","on":true}`); code != http.StatusOK {
		t.Fatalf("set = %d %s", code, body)
	}
	if !fake.Overrun("PRESS-1") || fake.Overrun("WELD-1") {
		t.Fatalf("after set PRESS-1=%v WELD-1=%v; want true, false", fake.Overrun("PRESS-1"), fake.Overrun("WELD-1"))
	}
	if got := listed(); len(got) != 1 || got[0] != "PRESS-1" {
		t.Errorf("listed %v; want [PRESS-1]", got)
	}
	if code, body := call(http.MethodPost, `{"plc_name":"PRESS-1","on":false}`); code != http.StatusOK {
		t.Fatalf("clear = %d %s", code, body)
	}
	if fake.Overrun("PRESS-1") || len(listed()) != 0 {
		t.Error("the clear did not take")
	}

	for _, bad := range []string{`{"plc_name":"PRESS-1"}`, `{"on":true}`, `not json`} {
		if code, _ := call(http.MethodPost, bad); code != http.StatusBadRequest {
			t.Errorf("body %s = %d; want 400", bad, code)
		}
	}
	if code, _ := call(http.MethodPost, `{"plc_name":"NO-SUCH-PLC","on":true}`); code != http.StatusBadGateway {
		t.Errorf("an unknown PLC = %d; want 502 (refused by the fake)", code)
	}
}
