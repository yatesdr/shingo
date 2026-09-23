//go:build docker

package engine

import (
	"context"
	"testing"

	"shingo/protocol"
	"shingocore/fleet/simulator"
	"shingocore/store"
	"shingocore/store/demands"
)

// threshold_sweep_rebuild_test.go — A BINDING THE SWEEP ENDED, COMING BACK.
//
// The reconciling sweep closes the episode of a binding demand_registry no
// longer has. What has to hold afterwards is that the binding returning, through
// any door that engages one, is a NEW demand: exactly one closed episode for the
// withdrawal and one open one for the return. A floor that quietly un-monitors
// a loader is the same outage as the one it was built to prevent, just harder
// to see.
//
// This file used to pin the sweep's rebuild of the monitor's binding memory,
// and the STALE BINDING IN MEMORY line that announced it. Both are gone with
// the memory: the monitor reads demand_registry, so there is nothing for a
// sweep to rebuild and no disagreement for a line to report. What the line
// was for — naming the writer that empties a station — is now the registry
// derive's own line (store.DeriveDemandRegistry).

// sweptAwayBinding engages a binding the way production does, takes its
// demand_registry rows away with nothing firing, and lets the SWEEP be the thing
// that notices. Returns nothing: every test that uses it then reopens the
// question from the database.
//
// The removal is deliberately NOT followed by a Resync: what these tests need
// is the state a plant is in when the writer that emptied the registry is one
// nobody has identified — rows gone, no notification, the sweep running
// underneath.
func sweptAwayBinding(t *testing.T, db *store.DB, m *ThresholdMonitor, b thresholdEntry) {
	t.Helper()
	registerBinding(t, db, b)
	m.Resync(b.stationID)
	if open := openThresholdEpisodes(t, db); len(open) != 1 {
		t.Fatalf("Resync opened %d episodes for one below-threshold binding, want 1", len(open))
	}
	emptyStationRegistry(t, db, b.stationID)
	if closed := m.reconcileThresholdBindings(); closed != 1 {
		t.Fatalf("the sweep closed %d episodes whose binding is gone, want 1", closed)
	}
}

// assertOneClosedOneOpen is the shape every "the binding came back" test ends
// in: the withdrawn config ended exactly one demand, and the return opened
// exactly one new one. Two rows, never three, and never one.
func assertOneClosedOneOpen(t *testing.T, db *store.DB, payload string) {
	t.Helper()
	rows := episodesForPayload(t, db, payload)
	if len(rows) != 2 {
		t.Fatalf("withdraw then return produced %d episodes, want 2 (one ended by the withdrawal, one opened by the return)", len(rows))
	}
	open, closed := 0, 0
	for _, r := range rows {
		if r.open {
			open++
			continue
		}
		closed++
		if r.closeReason != protocol.CloseReasonThresholdRemoved {
			t.Errorf("the withdrawn binding's episode closed %q, want %q — the need did not recover, it stopped being watched",
				r.closeReason, protocol.CloseReasonThresholdRemoved)
		}
	}
	if open != 1 || closed != 1 {
		t.Fatalf("%d open and %d closed episodes after the binding returned, want exactly 1 and 1", open, closed)
	}
}

// THE RETURN, THROUGH RESYNC. An Edge reconnects (or seeddev writes the rows and
// something resyncs the station) and the binding is back. The place is still
// hungry, so that is a NEW demand: re-joining the closed one would make a single
// row span an outage the plant knows nothing about.
func TestThresholdSweep_ReturnedBindingReEngagesViaResync(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	eng := newUnstartedEngine(t, db, simulator.New())
	m := NewThresholdMonitor(eng)
	const payload = "PANEL-SW2"
	b := stationBinding(t, eng, "PLANT.LINE1", "SLN_302", payload, 18)
	sweptAwayBinding(t, db, m, b)

	registerBinding(t, db, b)
	m.Resync(b.stationID)

	assertOneClosedOneOpen(t, db, payload)
}

// THE RETURN, THROUGH A CONFIG EDIT. OnThresholdChanges is the loader UI's door:
// an engineer re-adds the payload to the loader and SyncDemandRegistry reports
// the threshold moving off 0.
func TestThresholdSweep_ReturnedBindingReEngagesViaThresholdChange(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	eng := newUnstartedEngine(t, db, simulator.New())
	m := NewThresholdMonitor(eng)
	const payload = "PANEL-SW3"
	b := stationBinding(t, eng, "PLANT.LINE1", "SLN_303", payload, 18)
	sweptAwayBinding(t, db, m, b)

	registerBinding(t, db, b)
	m.OnThresholdChanges([]demands.RegistryChange{{
		StationID: b.stationID, CoreNodeName: b.coreNodeName, PayloadCode: b.payloadCode,
		OldThreshold: 0, NewThreshold: b.threshold,
	}})

	assertOneClosedOneOpen(t, db, payload)
}

// THE RETURN, THROUGH A RESTART. startupSweep is the door that needs no wire at
// all, which is why it is the one an operator reaches for — and the sweep must
// not have left anything behind that makes a restart mint twice or not at all.
//
// A WHOLE NEW MONITOR, because that is what a restart is.
func TestThresholdSweep_ReturnedBindingReEngagesViaStartupSweep(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	eng := newUnstartedEngine(t, db, simulator.New())
	m := NewThresholdMonitor(eng)
	const payload = "PANEL-SW4"
	b := stationBinding(t, eng, "PLANT.LINE1", "SLN_304", payload, 18)
	sweptAwayBinding(t, db, m, b)

	registerBinding(t, db, b)
	restarted := NewThresholdMonitor(eng)
	restarted.startupSweep(context.Background())

	assertOneClosedOneOpen(t, db, payload)
}

// loggingMonitor builds a monitor over an UNSTARTED engine whose log is
// captured, so a test can assert on the PROVER itself and not only on what the
// database ended up holding.
//
// It reuses stranded_fixround_docker_test.go's logSink rather than standing up a
// second one: a package with two capture helpers is a package where the next
// person picks whichever they find first, and they drift.
func loggingMonitor(t *testing.T, db *store.DB) (*ThresholdMonitor, *logSink) {
	t.Helper()
	sink := &logSink{}
	return NewThresholdMonitor(newLoggingEngine(t, db, sink)), sink
}
