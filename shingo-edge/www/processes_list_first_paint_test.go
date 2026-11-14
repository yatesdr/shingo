package www

import (
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"shingo/protocol/debuglog"
	"shingoedge/config"
	"shingoedge/domain"
	"shingoedge/engine"
	"shingoedge/internal/testdb"
	"shingoedge/store/processes"
)

// TestProcessesPage_FirstPaintRowsAreTheListRows pins that the rows the
// Processes page is served with are the rows /api/processes answers: the
// list's Curtain column and Settings' Quality hold read the first paint, and
// a first paint without curtain_nodes or quality_hold_enabled reads "off"
// whatever is set, until something re-reads the list.
func TestProcessesPage_FirstPaintRowsAreTheListRows(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	fx := testdb.BigLinePlant()
	seeded := testdb.SeedPlant(t, db, fx, "BIG-PRESS")
	on := true
	for _, n := range testdb.BigLineCurtains {
		if err := processes.SetNodeCurtain(db.DB, seeded.Nodes[n], true, "PLC_BIG", "Curtain_"+n, &on); err != nil {
			t.Fatalf("curtain %s: %v", n, err)
		}
	}
	// Started: the page reads the PLC manager, which Start builds.
	cfg := config.Defaults()
	cfg.StationUID = "test.firstpaint"
	cfg.Messaging.StationID = cfg.StationUID
	eng := engine.New(engine.Config{AppConfig: cfg, DB: db, LogFunc: func(string, ...any) {}})
	eng.Start()
	t.Cleanup(eng.Stop)
	if err := eng.ProcessService().SetContainment(domain.CoreNodeKinds{}, seeded.ProcessID, true, "BXN_001", "test"); err != nil {
		t.Fatalf("quality hold: %v", err)
	}
	dbg, err := debuglog.New(64, nil)
	if err != nil {
		t.Fatalf("debuglog: %v", err)
	}
	h, router, stop := NewRouter(eng, dbg, nil)
	t.Cleanup(stop)
	cookie := authCookie(t, h)
	get := func(path string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(cookie)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: %d", path, w.Code)
		}
		return w.Body.String()
	}

	var api []map[string]any
	if err := json.Unmarshal([]byte(get("/api/processes")), &api); err != nil {
		t.Fatalf("decode /api/processes: %v", err)
	}
	page := get("/processes")
	const open = `data-processes='`
	i := strings.Index(page, open)
	if i < 0 {
		t.Fatal("the page carries no data-processes")
	}
	rest := page[i+len(open):]
	var painted []map[string]any
	if err := json.Unmarshal([]byte(html.UnescapeString(rest[:strings.IndexByte(rest, '\'')])), &painted); err != nil {
		t.Fatalf("decode data-processes: %v", err)
	}

	if len(api) != 1 || api[0]["quality_hold_enabled"] != true || len(api[0]["curtain_nodes"].([]any)) != 2 {
		t.Fatalf("the list API itself is not what the seed set: %v", api)
	}
	if !reflect.DeepEqual(painted, api) {
		t.Errorf("the first paint is not the list's rows\n  page: %v\n  api:  %v", painted, api)
	}
}
