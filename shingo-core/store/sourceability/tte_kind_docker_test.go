//go:build docker

package sourceability_test

import (
	"testing"
	"time"

	"shingocore/internal/testdb"
	"shingocore/store/sourceability"
)

// tte_kind_docker_test.go — the two kinds of sample do not read each other's
// forecasts (v122).
//
// BEFORE B8 THIS COULD NOT HAPPEN, because every sample was a cell's and the
// threshold arm simply found nothing. B8 writes a second kind keyed on a NODE,
// and the moment both exist a cell sample standing at the same node as a
// monitored binding, carrying the same payload, satisfies the threshold arm's
// join key.
//
// IT WOULD BE THE WRONG NUMBER RATHER THAN AN EXTRA ONE. A cell's projection is
// one node's staged bin divided by that node's rate; a loader's is the
// payload's whole in-loop total divided by the plant-wide rate. They are
// forecasts about different stock, and a score that mixed them would read as
// one figure about neither — which is exactly the failure the score exists to
// detect, arriving as data instead of as a bug.

// TestScoreTTE_CellSampleIsNotScoredAsALoaderForecast is verify-red (c).
//
// RED AT BASE — with no kind on the row, the cell sample at NODE-K satisfies
// the threshold arm and the episode comes back scored against a projection that
// was never made about it.
func TestScoreTTE_CellSampleIsNotScoredAsALoaderForecast(t *testing.T) {
	db := testdb.Open(t)

	const node = "NODE-K"
	const payload = "BIN-K"

	// A CELL sample that happens to stand at the binding's node with the
	// binding's payload. Nothing stops this on a real plant: a cell claim and a
	// loader binding can name the same node, and at Hopkinsville the loader
	// nodes are claim nodes.
	insertSample(t, db, scoreBase.Add(-30*time.Minute), "cell", "PRESS-K", node, payload, 1800.0)

	// A THRESHOLD episode at that node. Its forecast should be a loader sample,
	// and there is none — so it is a blind spot, and saying so is the honest
	// answer.
	insertEpisode(t, db, "88888888-8888-8888-8888-888888888888",
		"thr|"+node+"|"+payload, "threshold", "PRESS-K", node, payload,
		scoreBase, "autoreorder")

	scores, err := sourceability.ScoreTTE(db.DB, scoreBase.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("ScoreTTE: %v", err)
	}

	s := scoreFor(t, scores, "thr|"+node+"|"+payload)
	if s.HasSample {
		t.Errorf("a CELL sample was scored as the loader's forecast (error %v) — "+
			"one node's staged bin read as the payload's whole in-loop total: %+v",
			s.ErrorSeconds, s)
	}
}

// TestScoreTTE_LoaderSampleIsNotScoredAsACellForecast is the same guard from
// the other side: the cell arm must not pick up a threshold row either. The
// pair is what makes the filter a partition rather than one-way filtering.
func TestScoreTTE_LoaderSampleIsNotScoredAsACellForecast(t *testing.T) {
	db := testdb.Open(t)

	const payload = "BIN-L"

	// A loader sample carries no process (LoaderSample leaves it empty), so the
	// cell arm's join on process_id would not reach it anyway. This fixture
	// writes one WITH a process to prove the kind filter is what refuses it,
	// not the empty string — otherwise the guard would pass for a reason that
	// disappears the moment anything stamps a process on a loader row.
	insertSample(t, db, scoreBase.Add(-30*time.Minute), "threshold", "PRESS-L", "NODE-L", payload, 1800.0)

	insertEpisode(t, db, "99999999-9999-9999-9999-999999999999",
		"cell|PRESS-L|"+payload+"|consume", "cell", "PRESS-L", "", payload,
		scoreBase, "operator")

	scores, err := sourceability.ScoreTTE(db.DB, scoreBase.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("ScoreTTE: %v", err)
	}

	s := scoreFor(t, scores, "cell|PRESS-L|"+payload+"|consume")
	if s.HasSample {
		t.Errorf("a THRESHOLD sample was scored as the cell's forecast: %+v", s)
	}
}
