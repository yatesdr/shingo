//go:build docker

package engine

import (
	"context"
	"strings"
	"testing"

	"shingocore/store/demands"
)

// threshold_engage_doors_test.go — WHAT EACH DOOR INTO THE BINDING CACHE DOES,
// pinned while the rebuild and the evaluation are still one function.
//
// engagePayloads does two separable things in one pass: it makes
// thresholdsByPayload agree with demand_registry for a payload (the REBUILD),
// and it then reads the authoritative in-loop total and decides whether to
// order (the EVALUATION). Every caller today gets both, and one of them —
// the reconciling sweep — has no business getting the second.
//
// These tests are the before-picture. They say, for each door, whether the
// cache was rebuilt, whether an episode was minted, and whether a replenishment
// decision was made, so that a later separation of the two halves can be
// checked against a record rather than against a reading of the diff.
//
// THE READ ERROR IS THE INTERESTING AXIS, and it is why two of the four tests
// force one. engagePayloads deliberately falls through to total = 0 when the
// authoritative read fails, so a config edit against a transient Postgres error
// still arms a zero-stock payload instead of going silent. That choice is
// defensible at a door an engineer is standing at and indefensible on a timer,
// and the only way to tell the two apart afterwards is to have pinned what each
// one does with the same failure.
//
// THE READ IS BROKEN BY HIDING lineside_buckets, not bins. SystemUOPForPayload
// sums both, so either breaks it — but the sweep path also closes episodes,
// rebuilds from demand_registry and reads the payload catalog, and bins is
// entangled with more of that than lineside_buckets is. Hiding the narrower
// table makes the authoritative read the ONLY thing that fails, so a test that
// says "nothing fired" cannot be passing because some earlier query fell over.

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

// assertEngaged is the shape both notification doors end in: the payload is in
// the cache exactly once, exactly one episode is open for it, and exactly one
// replenishment decision was made.
func assertEngaged(t *testing.T, m *ThresholdMonitor, fires *fireLog, b thresholdEntry) {
	t.Helper()
	m.mu.Lock()
	cached := len(m.thresholdsByPayload[b.payloadCode])
	m.mu.Unlock()
	if cached != 1 {
		t.Errorf("the door left %d binding(s) in thresholdsByPayload for %s, want 1 — the rebuild is half of what a door does",
			cached, b.payloadCode)
	}
	if open := openThresholdEpisodes(t, m.eng.db); len(open) != 1 {
		t.Errorf("the door opened %d episodes, want 1", len(open))
	}
	if got := fires.count(b.stationID); got != 1 {
		t.Errorf("the door made %d replenishment decisions for %s, want 1 — the evaluation is the other half",
			got, b.stationID)
	}
}

// THE (RE)CONNECT DOOR REBUILDS AND THEN EVALUATES.
//
// Resync is what turns a registry written out-of-band — seeddev, or Core's own
// re-derivation on an Edge register — into live bindings, and a binding that is
// already below threshold has to fire on the spot: there is no delta coming for
// a payload with no stock to move.
func TestThresholdEngage_ResyncRebuildsAndEvaluates(t *testing.T) {
	t.Parallel()

	m, _, fires, b := engageDoorFixture(t, "PANEL-EG1", "PLANT.LINE1", "SLN_311")

	m.Resync(b.stationID)

	assertEngaged(t, m, fires, b)
}

// THE CONFIG-EDIT DOOR REBUILDS AND THEN EVALUATES.
//
// OnThresholdChanges is the loader UI's door: SyncDemandRegistry reports the
// threshold moving off 0 and the engineer expects the loader to start being
// served without waiting for a delta. Springfield 6883 is the case where it did
// not.
func TestThresholdEngage_ThresholdChangeRebuildsAndEvaluates(t *testing.T) {
	t.Parallel()

	m, _, fires, b := engageDoorFixture(t, "PANEL-EG2", "PLANT.LINE1", "SLN_312")

	m.OnThresholdChanges([]demands.RegistryChange{{
		StationID: b.stationID, CoreNodeName: b.coreNodeName, PayloadCode: b.payloadCode,
		OldThreshold: 0, NewThreshold: b.threshold,
	}})

	assertEngaged(t, m, fires, b)
}

// THE NOTIFICATION DOORS FIRE OFF A ZERO WHEN THE AUTHORITATIVE READ FAILS, AND
// THAT IS DELIBERATE.
//
// A person has just told the system something — an Edge came back, or a loader
// was edited — and a transient Postgres error on the UOP sum is not a reason to
// leave a zero-stock payload unserved until the next delta, which for a payload
// with no stock may never come. The reading logged is 0, which is what the
// binding is evaluated against, and the episode records it.
//
// The assertion is on the READING, not merely on the fire: with no bins for
// this payload a successful read also returns 0, so "it fired" alone would pass
// whether or not the error branch ran. The log line is what says the read
// failed and the fall-through happened.
func TestThresholdEngage_NotificationDoorsFireOnAUOPReadError(t *testing.T) {
	t.Parallel()

	const editPayload = "PANEL-EG3B"
	m, sink, fires, resynced := engageDoorFixture(t, "PANEL-EG3A", "PLANT.LINE1", "SLN_313")
	edited := stationBinding(t, m.eng, "PLANT.LINE2", "SLN_413", editPayload, 18)
	registerBinding(t, m.eng.db, edited)

	withTableHidden(t, m.eng.db, "lineside_buckets", func() {
		m.Resync(resynced.stationID)
		m.OnThresholdChanges([]demands.RegistryChange{{
			StationID: edited.stationID, CoreNodeName: edited.coreNodeName,
			PayloadCode: edited.payloadCode, OldThreshold: 0, NewThreshold: edited.threshold,
		}})
	})

	for _, b := range []thresholdEntry{resynced, edited} {
		if got := fires.count(b.stationID); got != 1 {
			t.Errorf("%s made %d replenishment decisions through a failed UOP read, want 1 — the fall-through to zero is the point",
				b.stationID, got)
		}
		if hit := fires.find(b.stationID); hit != nil && hit.CurrentUOP != 0 {
			t.Errorf("%s decided off a reading of %d, want 0 — the failed read is supposed to fall through to zero",
				b.stationID, hit.CurrentUOP)
		}
		if lines := sink.linesContaining("read for " + b.payloadCode); len(lines) != 1 {
			t.Errorf("%d log lines reporting a failed read for %s, want 1 — without one, the fire above proves nothing about the error branch:\n%s",
				len(lines), b.payloadCode, strings.Join(lines, "\n"))
		}
	}
	if open := openThresholdEpisodes(t, m.eng.db); len(open) != 2 {
		t.Errorf("%d episodes open after two doors fired off a failed read, want 2", len(open))
	}
}

// THE RESTART DOOR DOES NOT SHARE THAT ZERO, AND IT IS NOT THE SAME CODE.
//
// startupSweep is named alongside Resync and OnThresholdChanges whenever these
// paths are discussed, but it does not go through engagePayloads at all: it
// builds thresholdsByPayload from its own ListDemandThresholds result and calls
// checkBindings directly. The visible consequence is exactly this test — a
// failed authoritative read makes it SKIP the payload rather than fall through
// to zero, so a restart against a sick database orders nothing and re-evaluates
// on the next delta.
//
// Pinned because the difference is invisible at a glance and load-bearing to
// any claim about "the three callers".
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
	// AND THE CACHE IS BUILT ANYWAY, which is the structural half of the point:
	// the startup sweep's rebuild is its own code, ahead of and independent of
	// the evaluation, so skipping the evaluation leaves a fully monitored
	// binding behind. engagePayloads does not work that way today.
	m.mu.Lock()
	cached := len(m.thresholdsByPayload[b.payloadCode])
	m.mu.Unlock()
	if cached != 1 {
		t.Errorf("the startup sweep left %d binding(s) in thresholdsByPayload, want 1 — it builds the cache before it evaluates anything",
			cached)
	}
}
