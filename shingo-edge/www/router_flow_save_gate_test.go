package www

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"shingo/protocol/debuglog"
	"shingoedge/config"
	"shingoedge/engine"
	"shingoedge/internal/testdb"
)

// TestRouter_FlowSaveIsBehindLogin pins that saving a style's flow needs a
// signed-in person, and that the station's two flow doors do not. Only the
// desktop's Processes page calls flow/save; the operator station's composer
// previews (preflight) and mints, and has no login.
func TestRouter_FlowSaveIsBehindLogin(t *testing.T) {
	t.Parallel()
	eng := engine.New(engine.Config{AppConfig: config.Defaults(), DB: testdb.Open(t), LogFunc: func(string, ...any) {}})
	dbg, err := debuglog.New(64, nil)
	if err != nil {
		t.Fatalf("debuglog: %v", err)
	}
	_, router, stop := NewRouter(eng, dbg, nil)
	t.Cleanup(stop)
	post := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(`{"station_id":1}`)))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	toLogin := func(w *httptest.ResponseRecorder) bool {
		return w.Code == http.StatusSeeOther && strings.HasPrefix(w.Header().Get("Location"), "/login")
	}
	if w := post("/api/processes/1/flow/save"); !toLogin(w) {
		t.Errorf("flow/save with no session answered %d (Location %q), want the login bounce", w.Code, w.Header().Get("Location"))
	}
	for _, path := range []string{"/api/processes/1/flow/preview?preflight=1", "/api/processes/1/flow/mint"} {
		if w := post(path); toLogin(w) || w.Code == http.StatusUnauthorized {
			t.Errorf("%s with no session was refused for login (%d): the station calls it and has no login", path, w.Code)
		}
	}
}
