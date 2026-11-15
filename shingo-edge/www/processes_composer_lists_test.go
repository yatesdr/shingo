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
	get, base := plantAComposerReads(t, "fixture-a")
	full, _ := get(base)
	if _, ok := full["map"]; !ok {
		t.Fatal("the full read carries no map; the scene did not load, and this test would prove nothing")
	}
	lists, _ := get(base + "/lists")
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

// TestProcessComposer_AMapThePageHoldsIsNotSentAgain pins map_rev on the
// composer's full read. The page quotes the revision of the plant map it
// already holds; when that is the Edge's current one, the reply leaves the map
// off and says "map_same": true, and every other field is the read without
// it, byte for byte. A revision that is not the current one, an empty one, or
// none at all gets the map, and no map_same. An Edge Core has sent no map is
// unaffected: no map, and no map_same whatever the page quotes, because there
// is nothing the page could be holding that is the same.
func TestProcessComposer_AMapThePageHoldsIsNotSentAgain(t *testing.T) {
	t.Parallel()
	get, base := plantAComposerReads(t, "fixture-a")
	full, fullBytes := get(base)
	var held struct {
		Revision string `json:"revision"`
	}
	if err := json.Unmarshal(full["map"], &held); err != nil || held.Revision != "fixture-a" {
		t.Fatalf("the full read's map revision = %q (%v), want fixture-a; this test would prove nothing", held.Revision, err)
	}
	if _, ok := full["map_same"]; ok {
		t.Errorf("a read quoting no revision says map_same %s", full["map_same"])
	}

	same, sameBytes := get(base + "?map_rev=fixture-a")
	if _, ok := same["map"]; ok {
		t.Error("a read quoting the current revision carries the map again")
	}
	if string(same["map_same"]) != "true" {
		t.Errorf("a read quoting the current revision says map_same %q, want true", same["map_same"])
	}
	// The cell's version token hashes store/processes.NodeGeneration, a
	// counter global to the process: a parallel test writing process_nodes
	// between the two reads moves it (in production, one Edge is one database).
	// The token is the poll's business, not the map's, so it is left out here.
	same["cell"], full["cell"] = withoutVersion(t, same["cell"]), withoutVersion(t, full["cell"])
	for k, v := range full {
		if k != "map" && string(same[k]) != string(v) {
			at := firstDiff(same[k], v)
			t.Errorf("%s differs when the map is left off, at byte %d:\n  without: %s\n  with:    %s", k, at, around(same[k], at), around(v, at))
		}
	}
	for k := range same {
		if _, ok := full[k]; !ok && k != "map_same" {
			t.Errorf("leaving the map off added %s", k)
		}
	}
	t.Logf("plant A composer reply: %d bytes with the map, %d with map_rev matching", fullBytes, sameBytes)

	for _, q := range []string{"?map_rev=fixture-0", "?map_rev="} {
		got, _ := get(base + q)
		var m struct {
			Revision string `json:"revision"`
		}
		if err := json.Unmarshal(got["map"], &m); err != nil || m.Revision != "fixture-a" {
			t.Errorf("%s: map revision %q (%v), want the map at fixture-a", q, m.Revision, err)
		}
		if _, ok := got["map_same"]; ok {
			t.Errorf("%s: says map_same %s", q, got["map_same"])
		}
	}

	getBare, baseBare := plantAComposerReads(t, "")
	bare, _ := getBare(baseBare)
	quoted, _ := getBare(baseBare + "?map_rev=fixture-a")
	for name, got := range map[string]map[string]json.RawMessage{"no revision": bare, "fixture-a quoted": quoted} {
		if _, ok := got["map"]; ok {
			t.Errorf("an Edge with no map, %s: the read carries a map", name)
		}
		if _, ok := got["map_same"]; ok {
			t.Errorf("an Edge with no map, %s: says map_same %s", name, got["map_same"])
		}
	}
}

// plantAComposerReads is plant A's press ("Press A1") behind the real router,
// and a GET of it that answers the decoded top-level fields and the reply's
// size in bytes. mapRevision is the plant map Core has sent the Edge, at that
// revision; "" is an Edge Core has sent none. base is the press's composer
// read.
func plantAComposerReads(t *testing.T, mapRevision string) (get func(path string) (map[string]json.RawMessage, int), base string) {
	t.Helper()
	fx := scenefixtures.A()
	db := testdb.Open(t)
	seeded := testdb.SeedPlant(t, db, fx, "Press A1")
	eng := engine.New(engine.Config{AppConfig: config.Defaults(), DB: db, LogFunc: func(string, ...any) {}})
	if mapRevision != "" {
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
		eng.SetSceneGeometry(mapRevision, points, edges)
	}
	dbg, err := debuglog.New(64, nil)
	if err != nil {
		t.Fatalf("debuglog: %v", err)
	}
	h, router, stop := NewRouter(eng, dbg, nil)
	t.Cleanup(stop)
	cookie := authCookie(t, h)
	get = func(path string) (map[string]json.RawMessage, int) {
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
		return out, w.Body.Len()
	}
	return get, "/api/processes/" + strconv.FormatInt(seeded.ProcessID, 10) + "/composer"
}

// firstDiff is the first byte at which a and b differ (the shorter's length
// when one is the other's prefix).
func firstDiff(a, b []byte) int {
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	return i
}

// around is b's 120 bytes either side of at.
func around(b []byte, at int) string {
	lo, hi := max(at-120, 0), min(at+120, len(b))
	return string(b[lo:hi])
}

// withoutVersion is a JSON object with its "version" key taken out.
func withoutVersion(t *testing.T, raw json.RawMessage) json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("the cell is not an object: %v", err)
	}
	delete(m, "version")
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("re-encode the cell: %v", err)
	}
	return out
}
