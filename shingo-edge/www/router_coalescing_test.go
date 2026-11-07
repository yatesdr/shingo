package www

import (
	"testing"
	"time"

	"shingo/protocol/debuglog"
	"shingoedge/config"
	"shingoedge/engine"
	"shingoedge/internal/testdb"
)

// router_coalescing_test.go — the router's two coalescing windows: what the
// shipped Edge waits, and the option a test passes to wait less.

// windowsRouter is the real router over an unstarted engine, stopped when the
// test ends. Not parallel: NewRouter writes the package's plantLocation.
func windowsRouter(t *testing.T, opts ...RouterOption) *Handlers {
	t.Helper()
	eng := engine.New(engine.Config{AppConfig: config.Defaults(), DB: testdb.Open(t), LogFunc: func(string, ...any) {}})
	dbg, err := debuglog.New(64, nil)
	if err != nil {
		t.Fatalf("debuglog: %v", err)
	}
	h, _, stop := NewRouter(eng, dbg, nil, opts...)
	t.Cleanup(stop)
	return h
}

// TestNewRouter_CoalescesAQuarterSecond pins the shipped windows. The boards'
// nudge waits 250 ms for the rest of its burst (a preset applied to forty parts
// is one nudge, TestSSE_NudgeIsCoalesced), and a plant-claims publish waits
// 250 ms (forty saves are one report,
// TestSpecChangeCoalescer_ASequentialBurstIsOnePublish). The door walk
// (cmd/shingoedge) runs both short through WithCoalescingWindows; this is what
// keeps the shipped value from moving with it.
func TestNewRouter_CoalescesAQuarterSecond(t *testing.T) {
	h := windowsRouter(t)
	if got := h.eventHub.nudgeWindow; got != 250*time.Millisecond {
		t.Errorf("the router's nudge window is %v, want 250ms", got)
	}
	if got := h.specChangeWindow; got != 250*time.Millisecond {
		t.Errorf("the router's plant-claims window is %v, want 250ms", got)
	}
}

// TestNewRouter_WithCoalescingWindowsSetsEach: each window lands on its own
// loop, not the other's.
func TestNewRouter_WithCoalescingWindowsSetsEach(t *testing.T) {
	h := windowsRouter(t, WithCoalescingWindows(7*time.Millisecond, 11*time.Millisecond))
	if got := h.eventHub.nudgeWindow; got != 7*time.Millisecond {
		t.Errorf("nudge window = %v, want the option's 7ms", got)
	}
	if got := h.specChangeWindow; got != 11*time.Millisecond {
		t.Errorf("plant-claims window = %v, want the option's 11ms", got)
	}
}
