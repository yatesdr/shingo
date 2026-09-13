package store

import (
	"path/filepath"
	"testing"

	"shingoedge/domain"
)

func sceneFixture(rev string) *domain.SceneGeometry {
	return &domain.SceneGeometry{
		Revision: rev,
		Points: map[string]domain.ScenePointGeom{
			"PLN_ORIGIN": {InstanceName: "PLN_ORIGIN", ClassName: "GeneralLocation", X: 0, Y: 0, Dir: 0},
			"PLN_01":     {InstanceName: "PLN_01", ClassName: "GeneralLocation", X: -16.929, Y: 59.549, Dir: 1.5708},
		},
		Edges: []domain.SceneEdgeGeom{
			{From: "LM9", To: "PP224", FromX: -0.604, FromY: 22.449, ToX: 0.986, ToY: 22.169, Handles: &[4]float64{-0.287, 22.094, 0.303, 22.142}},
			{From: "CP51", To: "LM54", FromX: -9.338, FromY: 6.23, ToX: -8.855, ToY: 6.221},
		},
	}
}

// TestSceneGeometry_SurvivesRestart is the reason the cache is on disk: an
// Edge that reboots during a Core partition still has the map it last held,
// so the station's picture does not go blank until Core is reachable.
func TestSceneGeometry_SurvivesRestart(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "edge.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.ReplaceSceneGeometry(sceneFixture("rev-1")); err != nil {
		t.Fatalf("replace: %v", err)
	}
	db.Close()

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	got, err := reopened.LoadSceneGeometry()
	if err != nil {
		t.Fatalf("load after restart: %v", err)
	}
	if got == nil || got.Revision != "rev-1" {
		t.Fatalf("after restart the cache holds %+v, want revision rev-1", got)
	}
	origin, ok := got.Points["PLN_ORIGIN"]
	if !ok || origin.X != 0 || origin.Y != 0 || origin.ClassName != "GeneralLocation" {
		t.Errorf("origin after restart = %+v ok=%v — (0,0) must be stored as a coordinate, not as NULL", origin, ok)
	}
	if p := got.Points["PLN_01"]; p.X != -16.929 || p.Y != 59.549 || p.Dir != 1.5708 {
		t.Errorf("PLN_01 after restart = %+v", p)
	}
	if len(got.Edges) != 2 {
		t.Fatalf("%d edges after restart, want 2", len(got.Edges))
	}
	for _, e := range got.Edges {
		switch e.From {
		case "LM9":
			if e.Handles == nil || *e.Handles != [4]float64{-0.287, 22.094, 0.303, 22.142} {
				t.Errorf("bezier handles after restart = %v", e.Handles)
			}
			if e.FromX != -0.604 || e.ToY != 22.169 {
				t.Errorf("bezier endpoints after restart = %+v", e)
			}
		case "CP51":
			if e.Handles != nil {
				t.Errorf("straight segment grew handles on disk: %v", *e.Handles)
			}
		default:
			t.Errorf("unexpected edge %+v", e)
		}
	}
}

// TestSceneGeometry_ReplaceIsWholesale: a replace holds the new scene and
// nothing of the old — a point that vanished from the map must not linger
// in the cache under the new revision.
func TestSceneGeometry_ReplaceIsWholesale(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	if err := db.ReplaceSceneGeometry(sceneFixture("rev-1")); err != nil {
		t.Fatalf("replace 1: %v", err)
	}
	next := &domain.SceneGeometry{
		Revision: "rev-2",
		Points:   map[string]domain.ScenePointGeom{"PLN_01": {InstanceName: "PLN_01", ClassName: "GeneralLocation", X: -16.5, Y: 59.549}},
		Edges:    []domain.SceneEdgeGeom{{From: "CP51", To: "LM54", FromX: -9.338, FromY: 6.23, ToX: -8.855, ToY: 6.221}},
	}
	if err := db.ReplaceSceneGeometry(next); err != nil {
		t.Fatalf("replace 2: %v", err)
	}
	got, err := db.LoadSceneGeometry()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Revision != "rev-2" || len(got.Points) != 1 || len(got.Edges) != 1 {
		t.Errorf("after replace: rev=%q points=%d edges=%d, want rev-2 / 1 / 1", got.Revision, len(got.Points), len(got.Edges))
	}
	if _, lingering := got.Points["PLN_ORIGIN"]; lingering {
		t.Error("a point from the previous scene survived the replace")
	}
	if got.Points["PLN_01"].X != -16.5 {
		t.Errorf("PLN_01 = %+v, want the moved coordinate", got.Points["PLN_01"])
	}
}

// TestSceneGeometry_EmptyCacheLoadsAsNil: before the first sync there is
// nothing, and nothing must read as nil — not as an empty scene with a
// blank revision, which the heartbeater would quote and Core would honour.
func TestSceneGeometry_EmptyCacheLoadsAsNil(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	got, err := db.LoadSceneGeometry()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != nil {
		t.Errorf("an unsynced cache loaded as %+v, want nil", got)
	}
}
