//go:build docker

package www

import (
	"encoding/json"
	"html/template"
	"net/http"
	"net/http/httptest"
	"regexp"
	"shingo/protocol/testutil"
	"strings"
	"sync"
	"testing"
	"time"

	"shingo/protocol/debuglog"
	"shingocore/config"
	"shingocore/engine"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/messaging"
)

// countingFleet is the simulator with a Ping that counts and can hang.
type countingFleet struct {
	*simulator.SimulatorBackend
	mu    sync.Mutex
	pings int
	hang  time.Duration
}

func (f *countingFleet) Ping() error {
	f.mu.Lock()
	f.pings++
	d := f.hang
	f.mu.Unlock()
	if d > 0 {
		time.Sleep(d)
	}
	return f.SimulatorBackend.Ping()
}

func (f *countingFleet) arm(hang time.Duration) {
	f.mu.Lock()
	f.pings, f.hang = 0, hang
	f.mu.Unlock()
}

func (f *countingFleet) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pings
}

func testHandlersWithCountingFleet(t *testing.T) (*Handlers, *countingFleet) {
	t.Helper()
	db := testdb.Open(t)
	fl := &countingFleet{SimulatorBackend: simulator.New()}
	cfg := config.Defaults()
	cfg.Messaging.StationID = "test-www"
	eng := engine.New(engine.Config{
		AppConfig: cfg, DB: db, Fleet: fl,
		MsgClient: messaging.NewClient(&cfg.Messaging), LogFunc: t.Logf,
	})
	eng.Start()
	t.Cleanup(func() { eng.Stop() })
	hub := NewEventHub()
	hub.Start()
	t.Cleanup(func() { hub.Stop() })
	dbgLog, err := debuglog.New(64, nil)
	testutil.MustNoErr(t, err, "debuglog.New")
	h := &Handlers{
		engine: eng, orchestration: eng, sessions: newSessionStore("test-secret"),
		tmpls: make(map[string]*template.Template), eventHub: hub, debugLog: dbgLog,
	}
	loadTestTemplates(t, h)
	return h, fl
}

// TestPinDashboard_HealthChecksPerView (LC10): one Dashboard render pings the
// fleet once (before: twice, handlers_dashboard.go Ping + dependencyState),
// and carries that reading as JSON so the page does not ask
// /api/core/health again at load (before: once more).
func TestPinDashboard_HealthChecksPerView(t *testing.T) {
	h, fl := testHandlersWithCountingFleet(t)
	fl.arm(0)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.handleDashboard(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if n := fl.count(); n != 1 {
		t.Errorf("fleet pings per render = %d, want 1", n)
	}
	m := regexp.MustCompile(`data-health-json='([^']*)'`).FindStringSubmatch(rec.Body.String())
	if m == nil {
		t.Fatal("core strip carries no data-health-json")
	}
	var health map[string]any
	raw := strings.NewReplacer("&#34;", `"`, "&quot;", `"`, "&amp;", "&", "&lt;", "<", "&gt;", ">", "&#39;", "'", "&#43;", "+").Replace(m[1])
	if err := json.Unmarshal([]byte(raw), &health); err != nil {
		t.Fatalf("data-health-json is not JSON: %v (%s)", err, raw)
	}
	if health["verdict"] == nil {
		t.Errorf("data-health-json lacks the verdict: %v", health)
	}
}

// TestPinDashboard_UnreachableFleetAnswers (LC10): a fleet that does not
// answer costs the render at most fleetPingTimeout, and the strip says the
// fleet is down. Before, two serial pings each waited the fleet client's
// timeout (10 s by default): about 20 s.
func TestPinDashboard_UnreachableFleetAnswers(t *testing.T) {
	h, fl := testHandlersWithCountingFleet(t)
	fl.arm(4 * fleetPingTimeout)
	defer fl.arm(0)
	start := time.Now()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.handleDashboard(rec, req)
	el := time.Since(start)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if el > fleetPingTimeout+2*time.Second {
		t.Errorf("render took %v with the fleet hanging, want ≤ %v", el, fleetPingTimeout+2*time.Second)
	}
	if !strings.Contains(rec.Body.String(), "fleet down") {
		t.Error("strip does not say the fleet is down")
	}
}

// TestPinLogs_Paged (R25, LC11): the Logs page renders the newest 100 and says
// which rows of how many; Older reaches the rest.
func TestPinLogs_Paged(t *testing.T) {
	t.Parallel()
	h, _ := testHandlersForPages(t)
	dbg, err := debuglog.New(1000, nil)
	testutil.MustNoErr(t, err, "debuglog.New")
	dbg.SetStderr(nil)
	h.debugLog = dbg
	for i := 0; i < 250; i++ {
		dbg.Log("pin", "line %03d", i)
	}
	rows := regexp.MustCompile(`<tr class="debug-row"`)

	rec := getPlain(t, h.handleDiagnostics, "/diagnostics")
	body := rec.Body.String()
	if n := len(rows.FindAllString(body, -1)); n != 100 {
		t.Errorf("page 1 rows = %d, want 100 (before: every entry, 250 here)", n)
	}
	if !strings.Contains(body, "Rows 1–100 of 250") || !strings.Contains(body, `href="/diagnostics?page=2"`) || strings.Contains(body, ">Newer<") {
		t.Errorf("page 1 pager wrong")
	}
	if !strings.Contains(body, "line 249") || strings.Contains(body, "line 149") {
		t.Errorf("page 1 is not the newest 100")
	}

	rec = getPlain(t, h.handleDiagnostics, "/diagnostics?page=3")
	body = rec.Body.String()
	if n := len(rows.FindAllString(body, -1)); n != 50 {
		t.Errorf("page 3 rows = %d, want 50", n)
	}
	if !strings.Contains(body, "Rows 201–250 of 250") || !strings.Contains(body, `href="/diagnostics?page=2"`) || strings.Contains(body, ">Older<") {
		t.Errorf("page 3 pager wrong")
	}
}
