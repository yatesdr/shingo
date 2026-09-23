//go:build docker

package engine

import (
	"context"
	"strings"
	"testing"

	"shingocore/store/demands"
)

// threshold_engage_doors_test.go — WHAT EACH DOOR INTO THE MONITOR DOES.
//
// The two notification doors — a loader config edit (OnThresholdChanges) and an
// Edge (re)connect (Resync) — are somebody telling Core its config is out of
// date, and both evaluate on the spot: a binding already below threshold has
// no delta coming to wake it. The startup sweep is the third door and evaluates
// every binding at boot.
//
// What each does when the authoritative total CANNOT be read is pinned for all
// of them together in threshold_readthrough_test.go
// (TestReadThrough_ReadErrorOpensNothingOrdersNothing): nothing. The two
// notification doors used to fall through to a total of 0 and order off it.

// engageDoorFixture is the common setup: a logging engine, its own monitor, a
// recorder on the fire decision, and one registered binding below threshold.
//
// It returns the monitor rather than building one with NewThresholdMonitor
// because captureThresholdFires installs its recorder on the ENGINE's monitor,
// and a test that watched one monitor while driving another would assert
// nothing at all. The engine is unstarted, so no background sweep is running
// underneath the door being tested.
func engageDoorFixture(t *testing.T, payload, station, node string) (*ThresholdMonitor, *logSink, *fireLog, thresholdEntry) {
	t.Helper()
	db := testDB(t)
	sink := &logSink{}
	eng := newLoggingEngine(t, db, sink)
	m := eng.thresholdMonitor
	fires := captureThresholdFires(t, eng)
	b := stationBinding(t, eng, station, node, payload, 18)
	registerBinding(t, db, b)
	return m, sink, fires, b
}

// assertEngaged is the shape both notification doors end in: exactly one
// episode is open for the place, and exactly one replenishment decision was
// made.
func assertEngaged(t *testing.T, m *ThresholdMonitor, fires *fireLog, b thresholdEntry) {
	t.Helper()
	if open := openThresholdEpisodes(t, m.eng.db); len(open) != 1 {
		t.Errorf("the door opened %d episodes, want 1", len(open))
	}
	if got := fires.count(b.stationID); got != 1 {
		t.Errorf("the door made %d replenishment decisions for %s, want 1",
			got, b.stationID)
	}
}

// THE (RE)CONNECT DOOR EVALUATES.
//
// Resync is what turns a registry written out-of-band — seeddev, or Core's own
// re-derivation on an Edge register — into live bindings, and a binding that is
// already below threshold has to fire on the spot: there is no delta coming for
// a payload with no stock to move.
func TestThresholdEngage_ResyncEvaluates(t *testing.T) {
	t.Parallel()

	m, _, fires, b := engageDoorFixture(t, "PANEL-EG1", "PLANT.LINE1", "SLN_311")

	m.Resync(b.stationID)

	assertEngaged(t, m, fires, b)
}

// THE CONFIG-EDIT DOOR EVALUATES.
//
// OnThresholdChanges is the loader UI's door: SyncDemandRegistry reports the
// threshold moving off 0 and the engineer expects the loader to start being
// served without waiting for a delta. Springfield 6883 is the case where it did
// not.
func TestThresholdEngage_ThresholdChangeEvaluates(t *testing.T) {
	t.Parallel()

	m, _, fires, b := engageDoorFixture(t, "PANEL-EG2", "PLANT.LINE1", "SLN_312")

	m.OnThresholdChanges([]demands.RegistryChange{{
		StationID: b.stationID, CoreNodeName: b.coreNodeName, PayloadCode: b.payloadCode,
		OldThreshold: 0, NewThreshold: b.threshold,
	}})

	assertEngaged(t, m, fires, b)
}

// THE RESTART DOOR SKIPS A PAYLOAD WHOSE TOTAL IT CANNOT READ, and logs it, so
// a restart against a sick database orders nothing and the next delta
// re-evaluates. The log line is asserted so the silence cannot be a sweep that
// never ran.
func TestThresholdEngage_StartupSweepSkipsOnAUOPReadError(t *testing.T) {
	t.Parallel()

	m, sink, fires, b := engageDoorFixture(t, "PANEL-EG4", "PLANT.LINE1", "SLN_314")

	withTableHidden(t, m.eng.db, "lineside_buckets", func() {
		m.startupSweep(context.Background())
	})

	if got := fires.count(b.stationID); got != 0 {
		t.Errorf("the startup sweep made %d replenishment decisions through a failed UOP read, want 0 — it skips the payload, it does not fall through to zero",
			got)
	}
	if open := openThresholdEpisodes(t, m.eng.db); len(open) != 0 {
		t.Errorf("the startup sweep opened %d episodes off a failed read, want 0", len(open))
	}
	if lines := sink.linesContaining("startup sweep SystemUOPForPayload"); len(lines) != 1 {
		t.Errorf("%d log lines reporting the sweep's failed read, want 1 — otherwise the silence above could be a sweep that never ran:\n%s",
			len(lines), strings.Join(lines, "\n"))
	}
}
