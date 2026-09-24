package uop

import (
	"testing"
	"time"

	"shingo/protocol/clock"
)

// repair_window_test.go — how long repairEpoch's debounce holds a reply back.
//
// NOT PARALLEL, deliberately. The debounce reads the process clock
// (clock.Now), and these tests install a manual one to step it. Go runs every
// sequential top-level test before it releases the parallel ones, so the
// swapped clock is restored before any parallel test in this package runs.

func withManualClock(t *testing.T) *clock.Manual {
	t.Helper()
	prev := clock.Default()
	m := clock.NewManual(time.Date(2026, 9, 24, 6, 0, 0, 0, time.UTC))
	clock.SetDefault(m)
	t.Cleanup(func() { clock.SetDefault(prev) })
	return m
}

// TestPin_P0h_RepairIsOncePerGenerationUntilRestart pins V9's second half at
// base: once a reply for (bin, epoch) is recorded, no later discard of that
// carrier's counts at that generation is ever answered again, however much time
// passes, until Core restarts and the in-memory map is empty. A reply that is
// lost (a dead-lettered or expired BinEpochRefresh) leaves the station behind
// for good.
//
// Verify-red: S5b (at most once per (bin, epoch) per 60 s) inverts the 61 s
// assertion — the window expires and the next discard is answered again.
func TestPin_P0h_RepairIsOncePerGenerationUntilRestart(t *testing.T) {
	m := withManualClock(t)
	s := &InventoryDeltaService{}

	if s.alreadyRepaired(7, 2) {
		t.Fatal("a carrier never answered reads as already answered")
	}
	s.markRepaired(7, 2)

	m.Advance(30 * time.Second)
	if !s.alreadyRepaired(7, 2) {
		t.Error("30 s after a reply, the same (bin, epoch) is answered again; want held back")
	}
	m.Advance(31 * time.Second)
	if !s.alreadyRepaired(7, 2) {
		t.Error("61 s after a reply, the same (bin, epoch) is answered again; at base it never is")
	}
	if s.alreadyRepaired(7, 3) {
		t.Error("a new generation of the carrier reads as already answered")
	}
}
