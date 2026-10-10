//go:build sim

package engine

import (
	"testing"
	"time"

	"shingo/protocol/clock"
)

// The sim operator at a loader that holds a labelled carrier, ruling (a) of
// RULINGS-C2b-rebase6-and-loader-orphan §3: at 0 units it clears the carrier
// through the operator's CLEAR door and loads it, as a trained person would; a
// labelled carrier with units is never cleared and waits for its outbound move.

func simLoaderAction(t *testing.T, window string, payload string, uop int) (func() error, *scCore) {
	t.Helper()
	eng, _, core, _, nodeID := scLoaderEngine(t, window)
	core.setCount(window, true, payload, uop)
	op := newTestSimOperator(clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))
	op.e = eng
	_, label, action, ok := op.classifyFromClaim(nodeID)
	if !ok || label != "load" {
		t.Fatalf("fixture: classifyFromClaim = (%q, %v), want a load action", label, ok)
	}
	return action, core
}

func TestSimOperator_ClearsAndLoadsALabelledCarrierAtZero(t *testing.T) {
	action, core := simLoaderAction(t, "SC-SLZ-W1", "SHIM", 0)
	if err := action(); err != nil {
		t.Fatalf("load at a labelled carrier reading 0: %v; the sim operator clears it and loads", err)
	}
	if n := core.clearCount(); n != 1 {
		t.Errorf("clears = %d, want 1 (the operator's CLEAR before the load)", n)
	}
	core.mu.Lock()
	defer core.mu.Unlock()
	if len(core.loadBinIDs) != 1 {
		t.Errorf("bin-load requests = %d, want 1 after the clear", len(core.loadBinIDs))
	}
}

func TestSimOperator_NeverClearsALabelledCarrierWithUnits(t *testing.T) {
	action, core := simLoaderAction(t, "SC-SLZ-W2", "SHIM", 5)
	if err := action(); err == nil {
		t.Fatal("load onto a labelled carrier with 5 units succeeded; it waits for its outbound move")
	}
	if n := core.clearCount(); n != 0 {
		t.Errorf("clears = %d, want 0: a labelled carrier with units is never cleared", n)
	}
}
