package engine

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"shingo/protocol"
)

// A counter tick that no node accepts used to fall off the end of
// handleCounterDelta in silence. The consume path has the A/B fallthrough
// net; produce has none. So a produce cell could count parts all shift
// against a claim whose style had moved on, and the only evidence was a
// runtime row that never changed — which is why diagnosing it took a
// database instead of a log line.
//
// These tests pin the instrument, not the arithmetic: the tick must name
// the process, the style, the delta, and the reason every node it walked
// declined to take it.
//
// No t.Parallel in this file — stdlib log.SetOutput is global.

// captureLog redirects the stdlib logger for one test and hands back the
// buffer. Restores the previous writer and flags, never nil: setting the
// output to nil poisons the global logger for every later test in the
// binary.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevW, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevW); log.SetFlags(prevFlags) })
	return &buf
}

// TestCounterDelta_UnattributedTickIsLoud is the instrument the rest of the
// resident-vs-requested stream is measured with. A produce node claimed for
// style A, ticked for style B, takes nothing — and must say so.
func TestCounterDelta_UnattributedTickIsLoud(t *testing.T) {
	db := testEngineDB(t)
	processID, _, styleID, _ := seedProduceNode(t, db, protocol.SwapModeSimple)

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	buf := captureLog(t)

	// A style that is not the claim's. The node is walked and declines.
	eng.Events.Emit(Event{Type: EventCounterDelta, Payload: CounterDeltaEvent{
		ProcessID: processID, StyleID: styleID + 999, Delta: 7,
	}})

	logged := buf.String()
	if !strings.Contains(logged, "unattributed") {
		t.Fatalf("a tick that no node took was silent. The whole point of this "+
			"instrument is that it cannot be. Log was:\n%s", logged)
	}
	// Everything needed to diagnose without opening the database.
	for _, want := range []string{"process=", "style=", "delta=7", "nodes=1", "style_mismatch=1"} {
		if !strings.Contains(logged, want) {
			t.Errorf("unattributed line is missing %q — it must name the process, the "+
				"style, the delta, how many nodes it walked, and why each declined.\nGot:\n%s",
				want, logged)
		}
	}
}

// A tick that DOES land stays quiet. An instrument that fires on the happy
// path is noise, and a log people learn to ignore is worse than the silence
// it replaced.
func TestCounterDelta_AttributedTickIsQuiet(t *testing.T) {
	db := testEngineDB(t)
	processID, _, styleID, _ := seedProduceNode(t, db, protocol.SwapModeSimple)

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	buf := captureLog(t)

	eng.Events.Emit(Event{Type: EventCounterDelta, Payload: CounterDeltaEvent{
		ProcessID: processID, StyleID: styleID, Delta: 7,
	}})

	if logged := buf.String(); strings.Contains(logged, "unattributed") {
		t.Errorf("a tick the produce node took was reported unattributed:\n%s", logged)
	}
}

// The manual_swap skip is the one an operator is most likely to hit by
// configuration rather than by accident, so the reason has to be nameable
// in the line rather than inferred from its absence.
func TestCounterDelta_UnattributedNamesManualSwapSkip(t *testing.T) {
	db := testEngineDB(t)
	processID, _, styleID, _ := seedProduceNode(t, db, protocol.SwapModeManualSwap)

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	buf := captureLog(t)

	eng.Events.Emit(Event{Type: EventCounterDelta, Payload: CounterDeltaEvent{
		ProcessID: processID, StyleID: styleID, Delta: 3,
	}})

	logged := buf.String()
	if !strings.Contains(logged, "manual_swap=1") {
		t.Errorf("a manual_swap node swallowed the tick without naming the reason.\nGot:\n%s", logged)
	}
}

// A tick for a process with no nodes at all is the emptiest case and the
// one most likely to be a wiring mistake — a reporting point pointed at a
// process that has none.
func TestCounterDelta_UnattributedNamesEmptyProcess(t *testing.T) {
	db := testEngineDB(t)
	processID, err := db.CreateProcess("NO-NODES", "no nodes", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	buf := captureLog(t)

	eng.Events.Emit(Event{Type: EventCounterDelta, Payload: CounterDeltaEvent{
		ProcessID: processID, StyleID: 1, Delta: 5,
	}})

	logged := buf.String()
	if !strings.Contains(logged, "nodes=0") || !strings.Contains(logged, "process has no nodes") {
		t.Errorf("a tick against a process with no nodes must say so plainly.\nGot:\n%s", logged)
	}
}

// A malformed tick is not a quiet no-op either: the PLC path only emits
// delta > 0 with both ids set, so one arriving here means the count moved
// upstream and reached us unusable.
func TestCounterDelta_MalformedTickIsLoud(t *testing.T) {
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.wireEventHandlers()
	buf := captureLog(t)

	eng.Events.Emit(Event{Type: EventCounterDelta, Payload: CounterDeltaEvent{
		ProcessID: 0, StyleID: 4, Delta: 2,
	}})

	if logged := buf.String(); !strings.Contains(logged, "malformed") {
		t.Errorf("a malformed tick was dropped silently.\nGot:\n%s", logged)
	}
}
