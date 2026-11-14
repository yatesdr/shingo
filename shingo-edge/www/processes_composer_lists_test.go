package www

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"

	"shingo/protocol"
	"shingo/protocol/debuglog"
	"shingo/shared/scenefixtures"
	"shingoedge/config"
	"shingoedge/engine"
	"shingoedge/internal/testdb"
)

// TestComposerLists_ThePollCarriesTheListsAndNotTheMap pins the Styles tab's
// 10-second poll read: the process's style and recipe lists exactly as the
// composer's full read draws them, and none of what the poll never reads —
// above all the plant map, which is the whole scene (plant A: 383 points and
// 635 lanes) on every answer while Core has sent one.
func TestComposerLists_ThePollCarriesTheListsAndNotTheMap(t *testing.T) {
	t.Parallel()
	fx := scenefixtures.A()
	db := testdb.Open(t)
	seeded := testdb.SeedPlant(t, db, fx, "Press A1")
	eng := engine.New(engine.Config{AppConfig: config.Defaults(), DB: db, LogFunc: func(string, ...any) {}})
	var points []protocol.ScenePointInfo
	for _, p := range fx.ScenePoints {
		x, y, d := p.PosX, p.PosY, p.Dir
		points = append(points, protocol.ScenePointInfo{InstanceName: p.InstanceName, ClassName: p.ClassName, PosX: &x, PosY: &y, Dir: &d})
	}
	var edges []protocol.SceneEdgeInfo
	for _, e := range fx.SceneEdges {
		fx1, fy1, tx, ty := e.FromX, e.FromY, e.ToX, e.ToY
		edges = append(edges, protocol.SceneEdgeInfo{From: e.FromName, To: e.ToName, FromX: &fx1, FromY: &fy1, ToX: &tx, ToY: &ty})
	}
	eng.SetSceneGeometry("fixture-a", points, edges)
	dbg, err := debuglog.New(64, nil)
	if err != nil {
		t.Fatalf("debuglog: %v", err)
	}
	h, router, stop := NewRouter(eng, dbg, nil)
	t.Cleanup(stop)
	cookie := authCookie(t, h)
	get := func(path string) map[string]json.RawMessage {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(cookie)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, w.Code, w.Body.String())
		}
		var out map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		return out
	}
	base := "/api/processes/" + strconv.FormatInt(seeded.ProcessID, 10) + "/composer"
	full := get(base)
	if _, ok := full["map"]; !ok {
		t.Fatal("the full read carries no map; the scene did not load, and this test would prove nothing")
	}
	lists := get(base + "/lists")
	keys := map[string]bool{}
	for k := range lists {
		keys[k] = true
	}
	if !reflect.DeepEqual(keys, map[string]bool{"styles": true, "presets": true}) && !reflect.DeepEqual(keys, map[string]bool{"styles": true}) {
		t.Errorf("the poll's read carries %v; want the styles and recipes only", keys)
	}
	for _, k := range []string{"styles", "presets"} {
		if string(lists[k]) != string(full[k]) {
			t.Errorf("the poll's %s differ from the full read's:\n  poll: %.200s\n  full: %.200s", k, lists[k], full[k])
		}
	}
}
