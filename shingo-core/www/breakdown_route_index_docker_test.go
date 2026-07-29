//go:build docker

package www

import (
	"encoding/json"
	"net/http"
	"testing"

	"shingo/protocol/testutil"
)

// The by=robot breakdown response contract (U3).
//
// Three fields carry decisions the client cannot make for itself, and each one
// exists because the alternative is a wrong render rather than a missing one:
//
//	index_available    false when NO route cleared the sample floor. The client
//	                   drops the COLUMN. Without this field it would have to infer
//	                   the state from every row's index being null, which is also
//	                   what "these particular robots ran on thin routes" looks
//	                   like — two different facts, one appearance.
//	min_route_samples  the floor the decision was taken against, so the panel can
//	                   say why the column is gone. A surface that silently loses a
//	                   column is indistinguishable from one that never had it.
//	min_robot_samples  the greying floor for a row's own index (5.4 / 5.9).
//
// And per row, route_index must be JSON null and never 0 where the robot has no
// index — 0.00x reads as a robot faster than instantaneous and sorts to the top
// of an ascending column.
func TestApiMissionBreakdown_RobotCarriesIndexContract(t *testing.T) {
	t.Parallel()
	h, _ := testHandlers(t)

	rec := getPlain(t, h.apiMissionBreakdown, "/api/missions/breakdown?by=robot")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Decode into a map so a MISSING field is distinguishable from a false one.
	// Decoding into a struct would give `index_available: false` for both, which
	// is the same absence-as-value mistake the field exists to prevent.
	var raw map[string]json.RawMessage
	testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&raw), "decode")

	for _, key := range []string{"by", "rows", "index_available", "min_route_samples", "min_robot_samples"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("response has no %q — the client cannot render the index column without it, and its "+
				"absence is indistinguishable from a false/zero value once decoded into a struct", key)
		}
	}

	// On an empty database no route can qualify, so the column must be dropped
	// rather than shown empty.
	var available bool
	if b, ok := raw["index_available"]; ok {
		testutil.MustNoErr(t, json.Unmarshal(b, &available), "index_available")
		if available {
			t.Error("index_available is true on an empty database — no route can have cleared the sample floor")
		}
	}

	// The floors must be the configured values, not zero. A zero floor would mean
	// every route qualifies and every index is 1.0 by construction.
	for _, key := range []string{"min_route_samples", "min_robot_samples"} {
		var n int
		if b, ok := raw[key]; ok {
			testutil.MustNoErr(t, json.Unmarshal(b, &n), key)
			if n <= 0 {
				t.Errorf("%s = %d — a non-positive floor makes every route its own median and every index exactly 1.0", key, n)
			}
		}
	}
}

// TestApiMissionBreakdown_RouteOmitsTheIndexFields.
//
// by=route has no index and must not claim one. Emitting index_available:false on
// the route panel would read as "the index is unavailable here", when the truth is
// that a route cannot have a route index — the figure is per-robot by definition.
// n/a and unavailable are different states; this is that distinction at the API.
func TestApiMissionBreakdown_RouteOmitsTheIndexFields(t *testing.T) {
	t.Parallel()
	h, _ := testHandlers(t)

	rec := getPlain(t, h.apiMissionBreakdown, "/api/missions/breakdown?by=route")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var raw map[string]json.RawMessage
	testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&raw), "decode")

	for _, key := range []string{"index_available", "min_route_samples", "min_robot_samples"} {
		if _, ok := raw[key]; ok {
			t.Errorf("by=route response carries %q, but a route has no route index — the field reads as "+
				"'unavailable' where the truth is 'not applicable'", key)
		}
	}
	if _, ok := raw["rows"]; !ok {
		t.Error("by=route response has no rows key")
	}
}
