package sourceability

import (
	"math"
	"testing"
)

// Pure fixtures for the aggregate — no DB, so this file has no build tag. The
// SQL half is proven against a real database in tte_score_docker_test.go.

func score(kind, place, payload string, errSecs float64, has bool) TTEScore {
	s := TTEScore{Kind: kind, PayloadCode: payload, HasSample: has, ErrorSeconds: errSecs}
	if kind == "cell" {
		s.ProcessID = place
	} else {
		s.CoreNodeName = place
	}
	return s
}

func aggFor(t *testing.T, aggs []TTEScoreAggregate, place, payload string) TTEScoreAggregate {
	t.Helper()
	for _, a := range aggs {
		if a.Place == place && a.PayloadCode == payload {
			return a
		}
	}
	t.Fatalf("no aggregate for %s/%s in %+v", place, payload, aggs)
	return TTEScoreAggregate{}
}

// A BLIND EPISODE IS NOT A ZERO-ERROR EPISODE. It is counted, excluded from the
// error figures, and reported as a share — the first number to read, because a
// forecast that is accurate on the episodes it covers says nothing about the
// ones it missed.
func TestAggregateTTEScores_BlindSpotsCountedNotScored(t *testing.T) {
	aggs := AggregateTTEScores([]TTEScore{
		score("threshold", "N1", "BIN-A", 10, true),
		score("threshold", "N1", "BIN-A", 30, true),
		score("threshold", "N1", "BIN-A", 0, false), // no prior sample
	})

	a := aggFor(t, aggs, "N1", "BIN-A")
	if a.Episodes != 3 {
		t.Errorf("episodes = %d, want 3", a.Episodes)
	}
	if a.Blind != 1 {
		t.Errorf("blind = %d, want 1", a.Blind)
	}
	if math.Abs(a.BlindFraction-1.0/3.0) > 1e-9 {
		t.Errorf("blind fraction = %v, want 1/3", a.BlindFraction)
	}
	// Median over the COVERED two only — 20, not 10 (which is what including
	// the blind episode as a zero would give).
	if a.MedianErrorSeconds != 20 {
		t.Errorf("median = %v, want 20 (over covered episodes only)", a.MedianErrorSeconds)
	}
}

// P90 is over the ABSOLUTE error: a badly early call is as much a miss as a
// badly late one, and a signed tail would hide the first behind the second.
func TestAggregateTTEScores_P90IsAbsolute(t *testing.T) {
	aggs := AggregateTTEScores([]TTEScore{
		score("threshold", "N1", "BIN-A", 1, true),
		score("threshold", "N1", "BIN-A", 2, true),
		score("threshold", "N1", "BIN-A", -900, true), // called dry 15 min early
	})

	a := aggFor(t, aggs, "N1", "BIN-A")
	if a.P90AbsErrorSeconds != 900 {
		t.Errorf("p90 abs = %v, want 900 — the early call is the tail", a.P90AbsErrorSeconds)
	}
	if a.MedianErrorSeconds != 1 {
		t.Errorf("median = %v, want 1 (signed, so direction survives)", a.MedianErrorSeconds)
	}
}

// A CELL EPISODE IS GROUPED BY ITS PROCESS. demand_origins.core_node_name
// defaults to ” and a cell episode names a process, so grouping every kind on
// the node would collapse every cell episode in the plant into one nameless
// bucket.
func TestAggregateTTEScores_CellGroupsByProcessNotNode(t *testing.T) {
	aggs := AggregateTTEScores([]TTEScore{
		score("cell", "PRESS-1", "BIN-A", 5, true),
		score("cell", "PRESS-2", "BIN-A", 7, true),
	})

	if len(aggs) != 2 {
		t.Fatalf("aggregates = %d, want 2 — one per process: %+v", len(aggs), aggs)
	}
	if got := aggFor(t, aggs, "PRESS-1", "BIN-A"); got.MedianErrorSeconds != 5 {
		t.Errorf("PRESS-1 median = %v, want 5", got.MedianErrorSeconds)
	}
	if got := aggFor(t, aggs, "PRESS-2", "BIN-A"); got.MedianErrorSeconds != 7 {
		t.Errorf("PRESS-2 median = %v, want 7", got.MedianErrorSeconds)
	}
}

// Places and payloads do not pool. Two payloads at one node are two forecasts.
func TestAggregateTTEScores_SplitsByPayload(t *testing.T) {
	aggs := AggregateTTEScores([]TTEScore{
		score("threshold", "N1", "BIN-A", 10, true),
		score("threshold", "N1", "BIN-B", 500, true),
	})

	if len(aggs) != 2 {
		t.Fatalf("aggregates = %d, want 2: %+v", len(aggs), aggs)
	}
	if got := aggFor(t, aggs, "N1", "BIN-A"); got.MedianErrorSeconds != 10 {
		t.Errorf("BIN-A median = %v, want 10", got.MedianErrorSeconds)
	}
	if got := aggFor(t, aggs, "N1", "BIN-B"); got.MedianErrorSeconds != 500 {
		t.Errorf("BIN-B median = %v, want 500", got.MedianErrorSeconds)
	}
}

// An all-blind place reports its coverage honestly rather than a median of 0,
// which would read as a perfect forecast.
func TestAggregateTTEScores_AllBlindIsNotPerfect(t *testing.T) {
	aggs := AggregateTTEScores([]TTEScore{
		score("threshold", "N1", "BIN-A", 0, false),
		score("threshold", "N1", "BIN-A", 0, false),
	})

	a := aggFor(t, aggs, "N1", "BIN-A")
	if a.BlindFraction != 1 {
		t.Errorf("blind fraction = %v, want 1", a.BlindFraction)
	}
	if a.Episodes != 2 || a.Blind != 2 {
		t.Errorf("episodes/blind = %d/%d, want 2/2", a.Episodes, a.Blind)
	}
}

func TestPercentile_NearestRankNotInterpolated(t *testing.T) {
	v := []float64{10, 20, 30, 40}
	// Interpolating would give 37; nearest rank gives a value that was actually
	// observed.
	if got := percentile(v, 0.90); got != 40 {
		t.Errorf("p90 = %v, want 40 (nearest rank)", got)
	}
	if got := percentile(nil, 0.90); got != 0 {
		t.Errorf("p90 of empty = %v, want 0", got)
	}
}

func TestMedian_EvenAndOdd(t *testing.T) {
	if got := median([]float64{3, 1, 2}); got != 2 {
		t.Errorf("median odd = %v, want 2", got)
	}
	if got := median([]float64{4, 1, 2, 3}); got != 2.5 {
		t.Errorf("median even = %v, want 2.5", got)
	}
	if got := median(nil); got != 0 {
		t.Errorf("median empty = %v, want 0", got)
	}
}
