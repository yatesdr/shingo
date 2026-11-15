package www

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"shingo/shared/scenefixtures"
	"shingoedge/domain"
	"shingoedge/internal/testdb"
)

// station_scene_test.go — GET /api/operator-stations/{id}/scene, the robot
// panel's map (apiStationScene, LEAD-NOTES D7, D11, D12): the geometry cache
// whole, in the shared scene shape, ETag = the cache's revision.

// sceneRouter is the operator-station router with a station and, when g is not
// nil, the cache holding g.
func sceneRouter(t *testing.T, g *domain.SceneGeometry) (*stubEngine, http.Handler, string) {
	t.Helper()
	h, router := newOperatorStationsRouter(t)
	db := dbOf(h)
	sid := seedOperatorStation(t, db, seedProcess(t, db, "SceneLine"), "OS-SCENE-1", "SceneStation")
	stub := h.engine.(*stubEngine)
	stub.sceneGeometry = g
	return stub, router, "/api/operator-stations/" + itoa(sid) + "/scene"
}

// getScene GETs path, quoting etag in If-None-Match when it is not "".
func getScene(t *testing.T, router http.Handler, path, etag string) (*http.Response, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	resp := w.Result()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return resp, body
}

// syntheticScene is an invented scene of n points in a row a metre apart,
// driven both ways, every third segment curved, and one segment out to a
// point the point list does not carry (Core validates the two lists apart).
func syntheticScene(revision string, n int) *domain.SceneGeometry {
	g := &domain.SceneGeometry{Revision: revision, Points: map[string]domain.ScenePointGeom{}}
	name := func(i int) string { return fmt.Sprintf("SP%03d", i) }
	for i := 1; i <= n; i++ {
		class := "LocationMark"
		if i%5 == 0 {
			class = "GeneralLocation"
		}
		g.Points[name(i)] = domain.ScenePointGeom{InstanceName: name(i), ClassName: class, X: float64(i), Y: 2}
	}
	for i := 1; i < n; i++ {
		var h *[4]float64
		if i%3 == 0 {
			h = &[4]float64{float64(i) + 0.3, 2.4, float64(i) + 0.7, 2.4}
		}
		g.Edges = append(g.Edges,
			domain.SceneEdgeGeom{From: name(i), To: name(i + 1), FromX: float64(i), FromY: 2, ToX: float64(i + 1), ToY: 2, Handles: h},
			domain.SceneEdgeGeom{From: name(i + 1), To: name(i), FromX: float64(i + 1), FromY: 2, ToX: float64(i), ToY: 2, Handles: h})
	}
	g.Edges = append(g.Edges, domain.SceneEdgeGeom{From: name(n), To: "SP999", FromX: float64(n), FromY: 2, ToX: float64(n), ToY: 5})
	return g
}

// TestStationScene_TheSharedShapeWholeAndNoLength: the door serves the cache
// whole in D11's shape: every point with its class and place, every segment in
// the cache's order with its two names and, on a curved one, all four handles,
// every endpoint placed (one the point list left out, from the segment), and no
// length or coordinate copy on a segment (no shared module reads one). The ETag
// is the cache's revision. Sizes are logged for the S0 fixture (plant A), plant
// B and a 22-point synthetic map.
func TestStationScene_TheSharedShapeWholeAndNoLength(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		g    *domain.SceneGeometry
	}{
		{"S0 fixture (plant A)", testdb.SceneGeometryOf(scenefixtures.A())},
		{"plant B", testdb.SceneGeometryOf(scenefixtures.B())},
		{"22-point synthetic", syntheticScene("synthetic-22", 22)},
	} {
		_, router, path := sceneRouter(t, c.g)
		resp, body := getScene(t, router, path, "")
		assertStatus(t, resp, http.StatusOK)
		if got, want := resp.Header.Get("ETag"), `W/"`+c.g.Revision+`"`; got != want {
			t.Errorf("%s: ETag %q, want the cache's revision %q", c.name, got, want)
		}
		var m domain.ComposerMap
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("%s: decode: %v", c.name, err)
		}
		if m.Revision != c.g.Revision {
			t.Errorf("%s: revision %q, cache %q", c.name, m.Revision, c.g.Revision)
		}
		for name, p := range c.g.Points {
			if got := m.Points[name]; got != (domain.ComposerMapPoint{Class: p.ClassName, X: p.X, Y: p.Y}) {
				t.Errorf("%s: point %s served as %+v, cache %+v", c.name, name, got, p)
			}
		}
		if len(m.Edges) != len(c.g.Edges) {
			t.Fatalf("%s: %d segments served, cache holds %d", c.name, len(m.Edges), len(c.g.Edges))
		}
		for i, e := range m.Edges {
			want := c.g.Edges[i]
			if e.From != want.From || e.To != want.To || (e.Handles == nil) != (want.Handles == nil) ||
				(e.Handles != nil && *e.Handles != *want.Handles) {
				t.Errorf("%s: segment %d served as %+v, cache %+v", c.name, i, e, want)
			}
			for _, end := range []string{e.From, e.To} {
				if _, ok := m.Points[end]; !ok {
					t.Errorf("%s: segment %s->%s names %s, which the map does not place", c.name, e.From, e.To, end)
				}
			}
		}
		assertSceneKeys(t, c.name, body)
		t.Logf("%s: %d points, %d segments: %d bytes, %d gzipped",
			c.name, len(m.Points), len(m.Edges), len(body), gzipLen(t, body))
	}
}

// assertSceneKeys fails on any key a segment or point carries beyond D11's.
func assertSceneKeys(t *testing.T, name string, body []byte) {
	t.Helper()
	var raw struct {
		Points map[string]map[string]json.RawMessage `json:"points"`
		Edges  []map[string]json.RawMessage          `json:"edges"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("%s: decode keys: %v", name, err)
	}
	for _, e := range raw.Edges {
		for k := range e {
			if k != "from" && k != "to" && k != "h" {
				t.Fatalf("%s: a segment carries %q; D11's segment is from, to and h", name, k)
			}
		}
	}
	for p, keys := range raw.Points {
		for k := range keys {
			if k != "class" && k != "x" && k != "y" {
				t.Fatalf("%s: point %s carries %q; D11's point is x, y and class", name, p, k)
			}
		}
	}
}

// gzipLen is body gzipped at the router's level (middleware.Compress(5)).
func gzipLen(t *testing.T, body []byte) int {
	t.Helper()
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, 5)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if _, err := zw.Write(body); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	return buf.Len()
}

// TestStationScene_AnswersAConditionalGetOnTheRevision: quoting the ETag gets
// 304 and no body while the cache holds that revision; once Core's scene moves,
// the same quote gets the new map and the new ETag.
func TestStationScene_AnswersAConditionalGetOnTheRevision(t *testing.T) {
	t.Parallel()
	stub, router, path := sceneRouter(t, syntheticScene("rev-1", 6))
	first, _ := getScene(t, router, path, "")
	assertStatus(t, first, http.StatusOK)
	etag := first.Header.Get("ETag")
	if etag != `W/"rev-1"` {
		t.Fatalf("ETag %q, want W/\"rev-1\"", etag)
	}
	again, body := getScene(t, router, path, etag)
	if again.StatusCode != http.StatusNotModified || len(body) != 0 {
		t.Fatalf("the held revision quoted back: status %d, %d bytes; want 304 and no body", again.StatusCode, len(body))
	}
	stub.sceneGeometry = syntheticScene("rev-2", 7)
	moved, body := getScene(t, router, path, etag)
	assertStatus(t, moved, http.StatusOK)
	var m domain.ComposerMap
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if moved.Header.Get("ETag") != `W/"rev-2"` || m.Revision != "rev-2" || len(m.Points) != 8 {
		t.Fatalf("after the scene moved: ETag %q, revision %q, %d points; want rev-2's map (7 points and SP999)",
			moved.Header.Get("ETag"), m.Revision, len(m.Points))
	}
}

// TestStationScene_AnEmptyCacheIsAnAnswerNotAnError: with nothing held the door
// answers 200 with revision "", no points, no segments and no ETag; a station
// that is not there is 404 and an id that is not one is 400, so the panel can
// tell "no map yet" from "could not ask".
func TestStationScene_AnEmptyCacheIsAnAnswerNotAnError(t *testing.T) {
	t.Parallel()
	_, router, path := sceneRouter(t, nil)
	resp, body := getScene(t, router, path, "")
	assertStatus(t, resp, http.StatusOK)
	if got := string(bytes.TrimSpace(body)); got != `{"revision":"","points":{},"edges":[]}` {
		t.Errorf("empty cache answered %s", got)
	}
	if etag := resp.Header.Get("ETag"); etag != "" {
		t.Errorf("an empty cache sent ETag %q; there is no revision to match", etag)
	}
	if again, _ := getScene(t, router, path, `W/""`); again.StatusCode != http.StatusOK {
		t.Errorf("an empty cache answered %d to a quoted empty revision; want 200", again.StatusCode)
	}
	missing, _ := getScene(t, router, "/api/operator-stations/987654/scene", "")
	assertStatus(t, missing, http.StatusNotFound)
	bad, _ := getScene(t, router, "/api/operator-stations/x/scene", "")
	assertStatus(t, bad, http.StatusBadRequest)
}
