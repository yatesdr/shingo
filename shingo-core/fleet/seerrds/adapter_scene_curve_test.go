package seerrds

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"shingocore/fleet"
)

// sprCurveScene is a /scene response carrying four advanced curves cut
// verbatim from Springfield's live scene (2026-07-26), trimmed of the
// bindRobotMap property that is all `property` ever holds.
//
// Two of them are the same aisle in opposite directions (AP102-LM100 and
// LM100-AP102) spelling "no control handles" two different ways, because the
// adapter has to get both spellings right or 54 straight aisles at Springfield
// come out of this function bent through the origin.
const sprCurveScene = `{"code":0,"msg":"ok","scene":{"areas":[{"name":"SPR","logicalMap":{"advancedCurves":[
{"className":"BezierPath","controlPos1":{"x":-0.287,"y":22.094,"z":0},"controlPos2":{"x":0.303,"y":22.142,"z":0},"endPos":{"instanceName":"PP224","pos":{"x":0.986,"y":22.169,"z":0}},"instanceName":"LM9-PP224","startPos":{"instanceName":"LM9","pos":{"x":-0.604,"y":22.449,"z":0}}},
{"className":"DegenerateBezier","controlPos1":{"x":-0.198,"y":36.401,"z":0},"controlPos2":{"x":-5.065,"y":36.951,"z":0},"endPos":{"instanceName":"LM113","pos":{"x":-6.572,"y":36.951,"z":0}},"instanceName":"LM10-LM113","startPos":{"instanceName":"LM10","pos":{"x":-0.254,"y":33.554,"z":0}}},
{"className":"StraightPath","controlPos1":{"x":0,"y":0,"z":0},"controlPos2":{"x":0,"y":0,"z":0},"endPos":{"instanceName":"AP102","pos":{"x":0.886,"y":11.807,"z":0}},"instanceName":"LM100-AP102","startPos":{"instanceName":"LM100","pos":{"x":-0.544,"y":11.787,"z":0}}},
{"className":"StraightPath","endPos":{"instanceName":"LM100","pos":{"x":-0.544,"y":11.787,"z":0}},"instanceName":"AP102-LM100","startPos":{"instanceName":"AP102","pos":{"x":0.886,"y":11.807,"z":0}}}
]}}]}}`

func sceneAdapter(t *testing.T, body string) *Adapter {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return New(Config{BaseURL: srv.URL, Timeout: 5 * time.Second, DebugLog: func(string, ...any) {}})
}

// TestGetSceneAreas_CarriesControlHandles asserts the adapter forwards the
// handles with their VALUES, and forwards nil for both no-handle spellings.
//
// The adapter is where this was previously claimed to be broken, and it was:
// not because it discarded `property`, but because rds.AdvancedCurve had
// nowhere to put the handles in the first place. This test would have gone
// green against that tree only by asserting nothing.
func TestGetSceneAreas_CarriesControlHandles(t *testing.T) {
	t.Parallel()

	areas, err := sceneAdapter(t, sprCurveScene).GetSceneAreas()
	if err != nil {
		t.Fatalf("GetSceneAreas: %v", err)
	}
	if len(areas) != 1 {
		t.Fatalf("areas = %d, want 1", len(areas))
	}
	byName := map[string]fleet.SceneEdge{}
	for _, e := range areas[0].Edges {
		byName[e.InstanceName] = e
	}
	if len(byName) != 4 {
		t.Fatalf("edges = %d, want 4: %v", len(byName), byName)
	}

	// A free-handle curve: exact coordinates, in the From→To direction.
	if got := byName["LM9-PP224"]; got.Ctrl1 == nil || got.Ctrl2 == nil {
		t.Errorf("LM9-PP224 lost its handles: %+v", got)
	} else if *got.Ctrl1 != (fleet.ScenePos{X: -0.287, Y: 22.094}) ||
		*got.Ctrl2 != (fleet.ScenePos{X: 0.303, Y: 22.142}) {
		t.Errorf("LM9-PP224 handles = %+v/%+v, want {-0.287 22.094}/{0.303 22.142}",
			*got.Ctrl1, *got.Ctrl2)
	}

	// A DegenerateBezier that genuinely bows. The class name says "degenerate";
	// the geometry says 1.30 m.
	if got := byName["LM10-LM113"]; got.Ctrl1 == nil || got.Ctrl2 == nil {
		t.Errorf("LM10-LM113 lost its handles: %+v", got)
	} else if *got.Ctrl1 != (fleet.ScenePos{X: -0.198, Y: 36.401}) ||
		*got.Ctrl2 != (fleet.ScenePos{X: -5.065, Y: 36.951}) {
		t.Errorf("LM10-LM113 handles = %+v/%+v, want {-0.198 36.401}/{-5.065 36.951}",
			*got.Ctrl1, *got.Ctrl2)
	}

	// Both no-handle spellings, both nil. If the zero sentinel leaked through,
	// this aisle would be drawn bending 8.8 m through the origin.
	for _, name := range []string{"LM100-AP102", "AP102-LM100"} {
		if got := byName[name]; got.Ctrl1 != nil || got.Ctrl2 != nil {
			t.Errorf("%s is a straight aisle but carries handles %+v/%+v",
				name, got.Ctrl1, got.Ctrl2)
		}
	}

	// The endpoints must be untouched by any of this.
	if got := byName["LM9-PP224"]; got.FromX != -0.604 || got.FromY != 22.449 ||
		got.ToX != 0.986 || got.ToY != 22.169 {
		t.Errorf("LM9-PP224 endpoints = (%v,%v)->(%v,%v), want (-0.604,22.449)->(0.986,22.169)",
			got.FromX, got.FromY, got.ToX, got.ToY)
	}
}
