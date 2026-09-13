package service

import (
	"encoding/json"
	"strings"
	"testing"
)

// composer_map_endpoints_test.go — an edge names its endpoints and the map
// places them, once.
//
// ComposerMapEdge used to carry fx/fy/tx/ty beside from/to, which is
// Points[from] and Points[to] written out again — once per incident edge, so a
// point on six lanes shipped its coordinates seven times. Measured on the S0
// fixture's 383-point/635-edge map, the Map block went 108.6 kB → 77.2 kB when
// the copies went.
//
// Two rules hold the removal up, and the first is what makes the second safe.

// TestComposerMap_EveryEndpointIsPlaced — the lookup the page now does cannot
// miss.
//
// Core sends points and edges as two lists and NewSceneGeometry validates them
// apart, so an edge may name a point the point list does not carry. Before
// this the page drew such a lane from the edge's own copy of the coordinates
// and drew no point at either end — a lane arriving from nowhere. composerMap
// places the endpoint from the edge instead.
func TestComposerMap_EveryEndpointIsPlaced(t *testing.T) {
	t.Parallel()
	fx := seedBudgetFixture(t)
	data, err := fx.svc.ComposerForProcess(fx.processID)
	if err != nil {
		t.Fatalf("composer for process: %v", err)
	}
	if data.Map == nil || len(data.Map.Edges) == 0 {
		t.Fatal("the fixture's desktop read carries no map — this test would pass vacuously")
	}
	for _, e := range data.Map.Edges {
		if _, ok := data.Map.Points[e.From]; !ok {
			t.Errorf("edge %s->%s: %s is not in points, so the page cannot place it", e.From, e.To, e.From)
		}
		if _, ok := data.Map.Points[e.To]; !ok {
			t.Errorf("edge %s->%s: %s is not in points, so the page cannot place it", e.From, e.To, e.To)
		}
	}
}

// TestComposerMap_AnEdgeCarriesNoCoordinates is the byte rule, stated as the
// shape rather than as a number: an edge is two names, a length and an
// optional pair of handles. A coordinate key coming back here is the 68.8 kB
// coming back with it.
func TestComposerMap_AnEdgeCarriesNoCoordinates(t *testing.T) {
	t.Parallel()
	fx := seedBudgetFixture(t)
	data, err := fx.svc.ComposerForProcess(fx.processID)
	if err != nil {
		t.Fatalf("composer for process: %v", err)
	}
	if data.Map == nil || len(data.Map.Edges) == 0 {
		t.Fatal("the fixture's desktop read carries no map — this test would pass vacuously")
	}
	raw, err := json.Marshal(data.Map.Edges[0])
	if err != nil {
		t.Fatalf("marshal edge: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("unmarshal edge: %v", err)
	}
	for k := range keys {
		switch k {
		case "from", "to", "len", "h":
		default:
			t.Errorf("a map edge carries %q (%s). An endpoint is a name; the place is Points[name], "+
				"and a second copy of it ships once per incident edge.", k, strings.TrimSpace(string(raw)))
		}
	}
}
