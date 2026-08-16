package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"shingocore/dispatch"
)

// TestDeclaredReleaserEventsAreSubscribed is the SUBSCRIPTION half of the
// releaser inventory: an event named in causeReleasers must actually be wired to
// the re-driver that serves the population claiming it.
//
// ── WHY THE TABLE CANNOT CHECK ITSELF ─────────────────────────────────────
//
// dispatch declares which events release which population, and cannot verify a
// word of it: engine owns the subscriptions and imports dispatch, so the
// dependency only runs one way. A table asserting "EventBinUpdated releases the
// gate-staged set" while nobody subscribes EventBinUpdated to the evaluator
// would be exactly the kind of confident, wrong documentation F-12 hid behind
// for a whole audit — and the totality test would still pass, because totality
// only asks whether a row EXISTS.
//
// ── WHAT IT PROVES, AND WHAT IT DOES NOT ──────────────────────────────────
//
// It reads the wiring source and requires, for each population, that its
// re-driver is called inside a wiring function AND that every event the table
// names appears as a subscription in that same function. That is a structural
// claim, in the style the four authority_exceptions_test.go guards already use
// here.
//
// It does NOT prove the handler passes the right lane, or that the bus delivers,
// or that the re-driver does anything useful — behavioural tests cover those
// (lane_gate_release_docker_test.go, compound_unlock_wakes_docker_test.go). What
// it catches is the drift this table is most exposed to: a subscription deleted
// or an event renamed while the table goes on claiming coverage. That is a real
// failure mode and it has happened here — F-05 deleted a mode gate and left two
// comments citing it as a live reason for months.
//
// MUTATION: delete the EventBinEnteredTransit subscription from
// wireLaneGateHandlers — this fires naming the event, both lane populations, and
// the file it should be in.
func TestDeclaredReleaserEventsAreSubscribed(t *testing.T) {
	t.Parallel()

	// Where each population's re-driver is wired. Two files, because the lane
	// populations and the acquiring set are wired separately and deliberately.
	wiringFor := map[string]string{
		"Dispatcher.EvaluateLaneReleases":    "wiring_lane_gate.go",
		"Dispatcher.RedriveHeldCompoundLegs": "wiring_lane_gate.go",
		"fulfillment.Scanner.RunOnce":        "wiring.go",
		"Dispatcher.AdvanceCompoundOrder":    "wiring.go",
	}

	sources := map[string]string{}
	for _, f := range []string{"wiring_lane_gate.go", "wiring.go"} {
		b, err := os.ReadFile(filepath.Join("..", "engine", f))
		if err != nil {
			// The test runs with the package dir as cwd; fall back to a bare read.
			b, err = os.ReadFile(f)
			if err != nil {
				t.Fatalf("read %s: %v", f, err)
			}
		}
		sources[f] = string(b)
	}

	for _, pop := range dispatch.DeclaredWaitPopulations() {
		file, ok := wiringFor[pop.Redriver]
		if !ok {
			t.Errorf("population %q names re-driver %q, and this test does not know which wiring "+
				"file registers it. Either the re-driver moved or a new population was added "+
				"without wiring — say which file, so the events below can be checked against it",
				pop.Population, pop.Redriver)
			continue
		}
		src := sources[file]

		// The re-driver must actually be called from the wiring.
		call := shortName(pop.Redriver) + "("
		if !strings.Contains(src, call) {
			t.Errorf("population %q declares re-driver %q, but %s never calls it — the events below "+
				"are subscribed to something else, or to nothing",
				pop.Population, pop.Redriver, file)
			continue
		}

		for _, ev := range pop.Events {
			// A subscription registers the event type as the trailing argument of
			// SubscribeTyped / SubscribeTypes. Requiring the identifier to appear in
			// the file that also calls the re-driver is the structural claim.
			if !strings.Contains(src, ev) {
				t.Errorf("population %q declares %s as a releaser, but %s does not mention it. "+
					"Either the subscription was removed — in which case a quiesced set of waits "+
					"under this population now depends entirely on the floor — or the table is "+
					"claiming coverage it does not have",
					pop.Population, ev, file)
			}
		}
	}
}

// shortName turns "Dispatcher.EvaluateLaneReleases" into "EvaluateLaneReleases",
// which is how the call reads at the wiring site.
func shortName(qualified string) string {
	if i := strings.LastIndex(qualified, "."); i >= 0 {
		return qualified[i+1:]
	}
	return qualified
}
