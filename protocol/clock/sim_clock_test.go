package clock

import (
	"testing"
	"time"
)

func TestSimClockAdvances(t *testing.T) {
	epoch := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	wallStart := time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC)

	s := &SimClock{
		epoch:  epoch,
		speed:  100.0,
		start:  wallStart,
		wallFn: func() time.Time { return wallStart },
	}

	// At start: sim time = epoch
	got := s.Now()
	if !got.Equal(epoch) {
		t.Fatalf("at start: want %s, got %s", epoch, got)
	}

	// After 1 real second at 100×: sim time = epoch + 100s
	s.wallFn = func() time.Time { return wallStart.Add(time.Second) }
	got = s.Now()
	want := epoch.Add(100 * time.Second)
	if !got.Equal(want) {
		t.Fatalf("after 1s wall at 100×: want %s, got %s", want, got)
	}

	// After 10 real seconds at 100×: sim time = epoch + 1000s
	s.wallFn = func() time.Time { return wallStart.Add(10 * time.Second) }
	got = s.Now()
	want = epoch.Add(1000 * time.Second)
	if !got.Equal(want) {
		t.Fatalf("after 10s wall at 100×: want %s, got %s", want, got)
	}
}

func TestSimClockClampsAtWallNow(t *testing.T) {
	epoch := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	wallStart := time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC)

	s := &SimClock{
		epoch:       epoch,
		speed:       1000.0, // very fast
		start:       wallStart,
		wallFn:      func() time.Time { return wallStart },
		clampToWall: true, // fast-forward clamps at present
	}

	// At 1000×, after 1 real hour (3600s), sim would advance 3_600_000s ≈ 41 days.
	// But wall-now is only wallStart + 3600s. Sim should clamp.
	wallNow := wallStart.Add(time.Hour)
	s.wallFn = func() time.Time { return wallNow }
	got := s.Now()
	if got.After(wallNow) {
		t.Fatalf("sim time should not exceed wall-now: got %s, wall %s", got, wallNow)
	}
	if !got.Equal(wallNow) {
		t.Fatalf("sim time should have clamped to wall-now: got %s, want %s", got, wallNow)
	}
}

func TestSimClockSpeedOne(t *testing.T) {
	epoch := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	wallStart := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	s := &SimClock{
		epoch:  epoch,
		speed:  1.0,
		start:  wallStart,
		wallFn: func() time.Time { return wallStart.Add(5 * time.Second) },
	}

	got := s.Now()
	want := epoch.Add(5 * time.Second)
	if !got.Equal(want) {
		t.Fatalf("at speed=1: want %s, got %s", want, got)
	}
}

func TestSimClockTickerUsesSpeed(t *testing.T) {
	// A 10-simulated-minute tick at 6000× is 100ms of real time. If the ticker
	// applied no speed it would take 10 real minutes, so a fire within 3s proves
	// the ticker is paced by the multiplier.
	s := NewRunningClock(6000)
	tk := s.NewTicker(10 * time.Minute)
	defer tk.Stop()
	select {
	case <-tk.C():
		// fired fast — speed is applied to the ticker
	case <-time.After(3 * time.Second):
		t.Fatal("cranked ticker did not fire within 3s; speed not applied")
	}
}

func TestSimTickerStopIdempotent(t *testing.T) {
	s := NewRunningClock(1.0)
	tk := s.NewTicker(time.Second)
	tk.Stop()
	tk.Stop() // must not panic — Stop is idempotent (audit F2)
}

// A running clock keeps advancing past wall-now (no clamp), so transit timed off
// Now() actually speeds up — the whole point of the live crank.
func TestRunningClockDoesNotClamp(t *testing.T) {
	s := NewRunningClock(10)
	start := s.start
	s.wallFn = func() time.Time { return start.Add(time.Second) }
	got := s.Now()
	want := start.Add(10 * time.Second) // epoch(=start) + 10×1s, unclamped
	if !got.Equal(want) {
		t.Fatalf("running clock should advance to %s (10× ahead of wall), got %s", want, got)
	}
}

func TestSimClockSetSpeed(t *testing.T) {
	// Epoch 7 days in the past — sim is catching up.
	epoch := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	wallStart := time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC)

	s := &SimClock{
		epoch:  epoch,
		speed:  10.0,
		start:  wallStart,
		wallFn: func() time.Time { return wallStart.Add(10 * time.Second) },
	}

	// After 10s wall at 10×: sim = epoch + 100s = June 1 00:01:40
	got := s.Now()
	want := epoch.Add(100 * time.Second)
	if !got.Equal(want) {
		t.Fatalf("before speed change: want %s, got %s", want, got)
	}

	// Change speed to 1× from this point
	s.SetSpeed(1.0)

	// The epoch was re-anchored to current sim time (June 1 00:01:40),
	// start re-anchored to current wall (June 8 00:00:10).
	// After another 10s wall at 1×: sim = June 1 00:01:40 + 10s = 00:01:50
	s.wallFn = func() time.Time { return wallStart.Add(20 * time.Second) }
	got = s.Now()
	want = epoch.Add(110 * time.Second)
	if !got.Equal(want) {
		t.Fatalf("after speed change: want %s, got %s", want, got)
	}
}

// TestBuildSimClock_Modes pins the construction switch: empty epoch → running
// (no clamp), epoch+anchor → synced fast-forward, epoch alone → unsynced.
func TestBuildSimClock_Modes(t *testing.T) {
	epoch := time.Date(2026, 6, 7, 0, 0, 0, 0, time.UTC)
	anchor := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)

	if clk, mode := BuildSimClock(time.Time{}, time.Time{}, 5, 15); mode != SimRunning || clk.clampToWall {
		t.Errorf("empty epoch: mode=%v clampToWall=%v, want SimRunning + no clamp", mode, clk.clampToWall)
	}
	if clk, mode := BuildSimClock(epoch, anchor, 5, 15); mode != SimSyncedFastForward || !clk.clampToWall {
		t.Errorf("epoch+anchor: mode=%v clampToWall=%v, want SimSyncedFastForward + clamp", mode, clk.clampToWall)
	}
	if clk, mode := BuildSimClock(epoch, time.Time{}, 5, 15); mode != SimUnsyncedFastForward || !clk.clampToWall {
		t.Errorf("epoch alone: mode=%v clampToWall=%v, want SimUnsyncedFastForward + clamp", mode, clk.clampToWall)
	}
}

// TestBuildSimClock_MaxSpeedDefault: maxSpeed<=0 falls back to DefaultSimMaxSpeed
// and the cap is actually enforced across all three modes.
func TestBuildSimClock_MaxSpeedDefault(t *testing.T) {
	epoch := time.Date(2026, 6, 7, 0, 0, 0, 0, time.UTC)
	anchor := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name          string
		epoch, anchor time.Time
	}{
		{"running", time.Time{}, time.Time{}},
		{"synced", epoch, anchor},
		{"unsynced", epoch, time.Time{}},
	} {
		clk, _ := BuildSimClock(tc.epoch, tc.anchor, 50, 0) // request 50×, maxSpeed unset
		if clk.Speed() != DefaultSimMaxSpeed {
			t.Errorf("%s: effective speed=%v, want capped to DefaultSimMaxSpeed=%v", tc.name, clk.Speed(), DefaultSimMaxSpeed)
		}
		if clk.RequestedSpeed() != 50 {
			t.Errorf("%s: requested=%v, want 50 (pre-cap)", tc.name, clk.RequestedSpeed())
		}
	}
}

// TestBuildSimClock_TwoBinariesAgree is the regression guard for the Core/Edge
// drift bug, AT THE BUILDER LEVEL: two BuildSimClock calls with the SAME config
// (as separately-built Core and Edge make) must report identical sim time at the
// same wall instant. This is what makes deduping the two binaries' bootstrap safe.
func TestBuildSimClock_TwoBinariesAgree(t *testing.T) {
	epoch := time.Date(2026, 6, 7, 0, 0, 0, 0, time.UTC)
	anchor := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)

	core, _ := BuildSimClock(epoch, anchor, 50, 0) // maxSpeed default 15
	edge, _ := BuildSimClock(epoch, anchor, 50, 0)

	wall := anchor.Add(60 * time.Second)
	core.wallFn = func() time.Time { return wall }
	edge.wallFn = func() time.Time { return wall }

	if !core.Now().Equal(edge.Now()) {
		t.Fatalf("builder produced drifting clocks: core=%v edge=%v", core.Now(), edge.Now())
	}
	if want := epoch.Add(900 * time.Second); !core.Now().Equal(want) { // 60s × 15
		t.Errorf("Now=%v want=%v (epoch + 15×60s)", core.Now(), want)
	}
}

// TestSimClockAnchored_TwoProcessesAgree is the regression test for the Core/Edge
// fast-forward clock-drift bug: two anchored clocks with the SAME (epoch, anchor,
// speed, maxSpeed) must report identical sim time at the same wall instant, even
// though they were constructed separately (as Core and Edge are). The maxSpeed cap
// is baked in WITHOUT re-anchoring, so the shared anchor survives.
func TestSimClockAnchored_TwoProcessesAgree(t *testing.T) {
	epoch := time.Date(2026, 6, 7, 0, 0, 0, 0, time.UTC)
	anchor := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)

	core := NewSimClockAnchored(epoch, anchor, 50, 15) // requested 50, capped to 15
	edge := NewSimClockAnchored(epoch, anchor, 50, 15)

	// Pin both to the SAME wall instant, 60s after the shared anchor.
	wall := anchor.Add(60 * time.Second)
	core.wallFn = func() time.Time { return wall }
	edge.wallFn = func() time.Time { return wall }

	if !core.Now().Equal(edge.Now()) {
		t.Fatalf("clocks drifted: core=%v edge=%v", core.Now(), edge.Now())
	}
	// 60 wall-seconds × 15 (capped) = 900 sim-seconds past epoch.
	if want := epoch.Add(900 * time.Second); !core.Now().Equal(want) {
		t.Errorf("Now=%v want=%v (epoch + 15×60s)", core.Now(), want)
	}
	if core.Speed() != 15 || core.RequestedSpeed() != 50 {
		t.Errorf("speed=%v requested=%v, want effective 15 / requested 50", core.Speed(), core.RequestedSpeed())
	}
}

// TestSimClockAnchored_DriftWithoutSharedAnchor documents WHY the shared anchor
// matters: the legacy per-process anchor (start = construction wall-now) drifts by
// (boot skew × speed). Here a 10s "boot skew" at 15× becomes 150s of clock drift —
// enough to expire 30s/90s-TTL coordination messages across the Core/Edge seam.
func TestSimClockAnchored_DriftWithoutSharedAnchor(t *testing.T) {
	epoch := time.Date(2026, 6, 7, 0, 0, 0, 0, time.UTC)
	wall := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)

	// Two clocks anchored 10s apart (the boot skew) — the legacy behavior.
	core := NewSimClockAnchored(epoch, wall, 15, 15)
	edge := NewSimClockAnchored(epoch, wall.Add(-10*time.Second), 15, 15)
	core.wallFn = func() time.Time { return wall }
	edge.wallFn = func() time.Time { return wall }

	drift := edge.Now().Sub(core.Now())
	if drift != 150*time.Second {
		t.Errorf("drift = %v, want 150s (10s boot skew × 15×) — confirms why anchors must match", drift)
	}
}
