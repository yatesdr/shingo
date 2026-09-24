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

// TestRepairIsOncePerGenerationPerMinute is S5b. A reply for (bin, epoch) holds
// back the next one for that pair for 60 s, not until Core restarts. A reply
// that is lost (the station was down, or an older build ignored it) is sent
// again on the first discarded count after the window.
//
// Inverted pin: at base (TestPin_P0h_RepairIsOncePerGenerationUntilRestart) the
// pair stayed answered forever, 61 s later included.
func TestRepairIsOncePerGenerationPerMinute(t *testing.T) {
	m := withManualClock(t)
	s := &InventoryDeltaService{}

	if s.alreadyRepaired(7, 2) {
		t.Fatal("a carrier never answered reads as already answered")
	}
	s.markRepaired(7, 2)

	m.Advance(30 * time.Second)
	if !s.alreadyRepaired(7, 2) {
		t.Error("30 s after a reply, the same (bin, epoch) is answered again; want held back for 60 s")
	}
	m.Advance(31 * time.Second)
	if s.alreadyRepaired(7, 2) {
		t.Error("61 s after a reply, the same (bin, epoch) is still held back; a lost reply is never resent")
	}
	if s.alreadyRepaired(7, 3) {
		t.Error("a new generation of the carrier reads as already answered")
	}

	// Answered again, and held again.
	s.markRepaired(7, 2)
	if !s.alreadyRepaired(7, 2) {
		t.Error("a fresh reply does not hold back the next one")
	}
}

// TestRepairMapIsBounded: the debounce keeps no entry older than its window
// once it is next written, so a plant that cycles thousands of carriers does
// not grow it for the life of the process.
func TestRepairMapIsBounded(t *testing.T) {
	m := withManualClock(t)
	s := &InventoryDeltaService{}

	for bin := int64(1); bin <= 100; bin++ {
		s.markRepaired(bin, 1)
	}
	m.Advance(61 * time.Second)
	s.markRepaired(101, 1)

	if n := len(s.repaired); n != 1 {
		t.Errorf("debounce holds %d entries after the window passed, want 1 — expired entries "+
			"must go when the map is next written", n)
	}
}
