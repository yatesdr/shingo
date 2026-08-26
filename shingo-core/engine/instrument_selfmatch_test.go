package engine

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"shingocore/service"
	"shingocore/store/bins"
	"shingocore/store/orders"
)

// TestShouldBeZeroTalliesDoNotMatchThemselves is the standing guard for B1, and
// it is the fourth appearance of the same lesson on this branch.
//
// A periodic tally line that QUOTES the string it tells the reader to grep for is
// matched by that grep. The reading is then tally-lines-plus-events, and since the
// tally re-emits every sweep for as long as the count is non-zero, the number
// climbs with uptime. Measured: `grep -c "WARN: arrival refused at"` returned 148
// against a true count of 2 (PLAN §R.9), and the same trap had already been
// diagnosed once on `bypass_events=157` before that.
//
// The lesson was applied to the mirror-jump ticket instrument and pointed at with
// some pride in §R.19 — and never back-applied to the two instruments it was
// learned on. Hence a test rather than another comment: this fails the moment a
// tally line contains its own marker, whoever writes it and whichever instrument
// it belongs to.
//
// Adding an instrument? Add a row. The markers are exported constants precisely so
// the emitter, the summariser and this test cannot hold three different opinions
// about what the string is.
func TestShouldBeZeroTalliesDoNotMatchThemselves(t *testing.T) {
	lines, svc := captureTallyLines(t)

	cases := []struct {
		instrument string
		marker     string
	}{
		{"burial-shadow bypass", service.BurialBypassMarker},
		{"arrival-guard refusals", ArrivalRefusalMarker},
		{"bin-state drift", DestNodeDriftMarker},
	}

	for _, tc := range cases {
		t.Run(tc.instrument, func(t *testing.T) {
			if tc.marker == "" {
				t.Fatal("marker constant is empty — the guard would pass vacuously")
			}
			for _, line := range lines {
				if strings.Contains(line, tc.marker) {
					t.Errorf("a tally line contains %q, the exact string it points the reader at, so "+
						"grepping that string counts this line too and the should-be-zero can never "+
						"read zero again (PLAN §R.9). Split the pattern in the tally.\n  line: %s",
						tc.marker, line)
				}
			}
		})
	}

	// The guard is only worth having if the lines it inspects actually exist —
	// otherwise a refactor that stops emitting them turns this test green for the
	// wrong reason.
	if len(lines) < 4 {
		t.Fatalf("captured %d tally lines, want at least 4 (bypass, churn, refusals, drift): %v",
			len(lines), lines)
	}
	_ = svc
}

// captureTallyLines forces every should-be-zero counter non-zero and returns the
// periodic lines the reconciliation sweep emits for them.
func captureTallyLines(t *testing.T) ([]string, *ReconciliationService) {
	t.Helper()
	resetArrivalRefusalTally()
	resetDestNodeDriftTally()
	t.Cleanup(resetArrivalRefusalTally)
	t.Cleanup(resetDestNodeDriftTally)

	var mu sync.Mutex
	var lines []string
	logFn := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}

	// An arrival refusal, through the real recorder.
	eng := &Engine{logFn: logFn}
	node := int64(7)
	eng.recordArrivalRefusal(refuseArrival(
		&orders.Order{ID: 3},
		&bins.Bin{ID: 5, NodeID: &node},
		9, arrivalSiteDelivery))

	// A destination drift, through the real detector.
	eng.noteDestNodeDrift(
		&orders.Order{ID: 3, StepsJSON: driftPlan},
		[]*orders.OrderBin{{BinID: 9, NodeName: "D-EMPTIES", DestNode: "D-LANE-S0"}},
		driftSiteDelivery)

	svc := &ReconciliationService{logFn: logFn}
	svc.burialTally = func() service.BurialTally {
		return service.BurialTally{Bypass: 3, Churn: 4, Soft: 1}
	}

	// Only the periodic lines matter for this question, so the per-event lines
	// captured above are dropped: they are SUPPOSED to contain the markers.
	mu.Lock()
	lines = nil
	mu.Unlock()

	svc.logBurialShadow()
	svc.logArrivalRefusals()
	svc.logDestNodeDrift()

	mu.Lock()
	defer mu.Unlock()
	out := make([]string, len(lines))
	copy(out, lines)
	return out, svc
}
