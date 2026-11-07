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
	mgr := plc.NewManager(nil, config.Defaults().WarLink, config.Defaults().PollRate, config.Defaults().Counter.JumpThreshold, nil, fake)
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

// TestSimProgramChange_RouteChangesAndReadsLine pins POST /api/sim/program-change
// and GET /api/sim/line over the fake WarLink: a forced change sets the identity
// and resets the counter at once, a drain change is taken, the line read names
// the process with its identity, and a malformed body or mode (400), a process
// the fake does not count and one with no identity tag (502) are refused.
func TestSimProgramChange_RouteChangesAndReadsLine(t *testing.T) {
	h, r := newTestHandlers(t)
	cfg := config.SimConfig{Enabled: true, Processes: []config.SimProcessConfig{
		{PLCName: "WELD-6", TagName: "WELD-6_COUNTER", TickInterval: time.Second,
			StationTag: "WELD-6_STATION", LineDepth: 2, IdentityTag: "WELD-6_CATID"},
		{PLCName: "PRESS-1", TagName: "PRESS-1_COUNTER", TickInterval: time.Second},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := simwarlink.NewFakeClient(ctx, cfg, clock.NewManual(time.Unix(0, 0)))
	mgr := plc.NewManager(nil, config.Defaults().WarLink, config.Defaults().PollRate, config.Defaults().Counter.JumpThreshold, nil, fake)
	mgr.SetTagValueForTest("WELD-6", "WELD-6_COUNTER", int64(0))
	h.engine.(*stubEngine).plcMgr = mgr
	r.Route("/api", func(r chi.Router) { h.registerSimRoutes(r) })

	call := func(method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	const post = "/api/sim/program-change"

	if code, body := call(http.MethodPost, post, `{"plc_name":"WELD-6","tag_name":"WELD-6_COUNTER","value":1002,"mode":"forced"}`); code != http.StatusOK {
		t.Fatalf("forced = %d %s", code, body)
	}
	if v, _ := fake.ReadTagValue(ctx, "WELD-6", "WELD-6_CATID"); v != int64(1002) {
		t.Errorf("identity after the forced change = %v; want 1002", v)
	}
	if code, body := call(http.MethodPost, post, `{"plc_name":"WELD-6","tag_name":"WELD-6_COUNTER","value":1003,"mode":"drain"}`); code != http.StatusOK {
		t.Fatalf("drain = %d %s", code, body)
	}
	code, body := call(http.MethodGet, "/api/sim/line", "")
	var got struct {
		Lines []simwarlink.ProcessLine `json:"lines"`
	}
	if code != http.StatusOK || json.Unmarshal([]byte(body), &got) != nil {
		t.Fatalf("GET line = %d %s", code, body)
	}
	if len(got.Lines) != 1 || got.Lines[0].PLCName != "WELD-6" || got.Lines[0].TagName != "WELD-6_COUNTER" || got.Lines[0].Identity != 1002 {
		t.Errorf("line read = %s; want WELD-6/WELD-6_COUNTER at 1002 (the drain lands on a tick)", body)
	}

	for _, bad := range []string{
		`{"plc_name":"WELD-6","tag_name":"WELD-6_COUNTER","value":1002}`,
		`{"plc_name":"WELD-6","tag_name":"WELD-6_COUNTER","value":1002,"mode":"later"}`,
		`{"plc_name":"WELD-6","tag_name":"WELD-6_COUNTER","mode":"drain"}`,
		`{"tag_name":"WELD-6_COUNTER","value":1002,"mode":"drain"}`,
		`not json`,
	} {
		if code, _ := call(http.MethodPost, post, bad); code != http.StatusBadRequest {
			t.Errorf("body %s = %d; want 400", bad, code)
		}
	}
	if code, _ := call(http.MethodPost, post, `{"plc_name":"WELD-6","tag_name":"NO_SUCH_COUNTER","value":1002,"mode":"drain"}`); code != http.StatusBadGateway {
		t.Errorf("an unknown process = %d; want 502 (refused by the fake)", code)
	}
	if code, _ := call(http.MethodPost, post, `{"plc_name":"PRESS-1","tag_name":"PRESS-1_COUNTER","value":1002,"mode":"forced"}`); code != http.StatusBadGateway {
		t.Errorf("a process with no identity_tag = %d; want 502 (refused by the fake)", code)
	}
}

// TestSimCountingSlot_RouteMovesTheCountAndTheLineSaysSo pins POST
// /api/sim/counting-slot over the fake WarLink: the count moves to slot 2 of
// the process's MES struct (the line read names the slot), a malformed body is
// refused (400), and a process with no MES struct or a slot past its five is
// refused by the fake (502).
func TestSimCountingSlot_RouteMovesTheCountAndTheLineSaysSo(t *testing.T) {
	h, r := newTestHandlers(t)
	cfg := config.SimConfig{Enabled: true, Processes: []config.SimProcessConfig{
		{PLCName: "WELD-6", TagName: "MES_WELD_6.Prod_Counter_01", TickInterval: time.Second,
			StationTag: "STA_W6_LOAD.Load_Count", LineDepth: 2, IdentityTag: "MES_WELD_6.CATID_01"},
		{PLCName: "PRESS-1", TagName: "PRESS-1_COUNTER", TickInterval: time.Second},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := simwarlink.NewFakeClient(ctx, cfg, clock.NewManual(time.Unix(0, 0)))
	mgr := plc.NewManager(nil, config.Defaults().WarLink, config.Defaults().PollRate, config.Defaults().Counter.JumpThreshold, nil, fake)
	mgr.SetTagValueForTest("WELD-6", "MES_WELD_6.Prod_Counter_01", int64(0))
	h.engine.(*stubEngine).plcMgr = mgr
	r.Route("/api", func(r chi.Router) { h.registerSimRoutes(r) })

	call := func(method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	const post = "/api/sim/counting-slot"

	if code, body := call(http.MethodPost, post, `{"plc_name":"WELD-6","tag_name":"MES_WELD_6.Prod_Counter_01","slot":2}`); code != http.StatusOK {
		t.Fatalf("slot 2 = %d %s", code, body)
	}
	code, body := call(http.MethodGet, "/api/sim/line", "")
	var got struct {
		Lines []simwarlink.ProcessLine `json:"lines"`
	}
	if code != http.StatusOK || json.Unmarshal([]byte(body), &got) != nil {
		t.Fatalf("GET line = %d %s", code, body)
	}
	if len(got.Lines) != 1 || got.Lines[0].TagName != "MES_WELD_6.Prod_Counter_01" || got.Lines[0].Slot != 2 {
		t.Errorf("line read = %s; want WELD-6's line counting on slot 2", body)
	}
	for _, bad := range []string{
		`{"plc_name":"WELD-6","tag_name":"MES_WELD_6.Prod_Counter_01"}`,
		`{"tag_name":"MES_WELD_6.Prod_Counter_01","slot":2}`,
		`not json`,
	} {
		if code, _ := call(http.MethodPost, post, bad); code != http.StatusBadRequest {
			t.Errorf("body %s = %d; want 400", bad, code)
		}
	}
	for _, refused := range []string{
		`{"plc_name":"PRESS-1","tag_name":"PRESS-1_COUNTER","slot":2}`,
		`{"plc_name":"WELD-6","tag_name":"MES_WELD_6.Prod_Counter_01","slot":6}`,
	} {
		if code, _ := call(http.MethodPost, post, refused); code != http.StatusBadGateway {
			t.Errorf("body %s = %d; want 502 (refused by the fake)", refused, code)
		}
	}
}

// TestSimPauseLoading_RoutePausesAndResumesTheLine pins POST
// /api/sim/pause-loading over the fake WarLink: paused true pauses the line's
// loading and the line read says so, false loads again; a malformed body is
// refused (400), and a tag no process counts on by the fake (502).
func TestSimPauseLoading_RoutePausesAndResumesTheLine(t *testing.T) {
	h, r := newTestHandlers(t)
	cfg := config.SimConfig{Enabled: true, Processes: []config.SimProcessConfig{
		{PLCName: "WELD-6", TagName: "WELD-6_COUNTER", TickInterval: time.Second,
			StationTag: "WELD-6_STATION", LineDepth: 2, IdentityTag: "WELD-6_CATID"},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := simwarlink.NewFakeClient(ctx, cfg, clock.NewManual(time.Unix(0, 0)))
	mgr := plc.NewManager(nil, config.Defaults().WarLink, config.Defaults().PollRate, config.Defaults().Counter.JumpThreshold, nil, fake)
	mgr.SetTagValueForTest("WELD-6", "WELD-6_COUNTER", int64(0))
	h.engine.(*stubEngine).plcMgr = mgr
	r.Route("/api", func(r chi.Router) { h.registerSimRoutes(r) })

	call := func(method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	line := func() simwarlink.ProcessLine {
		t.Helper()
		code, body := call(http.MethodGet, "/api/sim/line", "")
		var got struct {
			Lines []simwarlink.ProcessLine `json:"lines"`
		}
		if code != http.StatusOK || json.Unmarshal([]byte(body), &got) != nil || len(got.Lines) != 1 {
			t.Fatalf("GET line = %d %s", code, body)
		}
		return got.Lines[0]
	}
	const post = "/api/sim/pause-loading"

	if code, body := call(http.MethodPost, post, `{"plc_name":"WELD-6","tag_name":"WELD-6_COUNTER","paused":true}`); code != http.StatusOK {
		t.Fatalf("pause = %d %s", code, body)
	}
	if l := line(); !l.Paused {
		t.Fatalf("the line read after the pause = %+v; want paused", l.LineState)
	}
	if code, body := call(http.MethodPost, post, `{"plc_name":"WELD-6","tag_name":"WELD-6_COUNTER","paused":false}`); code != http.StatusOK {
		t.Fatalf("resume = %d %s", code, body)
	}
	if l := line(); l.Paused {
		t.Errorf("the line read after the resume = %+v; want loading", l.LineState)
	}
	for _, bad := range []string{
		`{"plc_name":"WELD-6","tag_name":"WELD-6_COUNTER"}`,
		`{"tag_name":"WELD-6_COUNTER","paused":true}`,
		`{"plc_name":"WELD-6","paused":true}`,
		`not json`,
	} {
		if code, _ := call(http.MethodPost, post, bad); code != http.StatusBadRequest {
			t.Errorf("body %s = %d; want 400", bad, code)
		}
	}
	if code, _ := call(http.MethodPost, post, `{"plc_name":"WELD-6","tag_name":"NO_SUCH_COUNTER","paused":true}`); code != http.StatusBadGateway {
		t.Errorf("a tag no process counts on = %d; want 502 (refused by the fake)", code)
	}
}
