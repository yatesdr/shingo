package clock

import (
	"testing"
	"time"
)

// sim_clock_agreement_test.go — §R.98 stage D.
//
// The finding this pins: the sim clock ran TWO SPEEDS AT ONCE. `Now()` was
// wall-clamped to 1× while `After` and `NewTicker` divided by the configured
// multiplier and ran at 10×. Production ticked at 10×, transit and every Core
// floor and sweep at 1×, and the banner said 10× — so the rig never measured the
// economy its config was tuned for, and nobody could see it because each half was
// individually correct.

// A clamped clock is advancing at wall rate. Anything waiting on it must wait in
// wall time, or the two halves of one clock disagree about how fast the world is
// going.
func TestClampedClockSlowsItsWaitsToo(t *testing.T) {
	epoch := time.Date(2026, 6, 7, 0, 0, 0, 0, time.UTC)
	anchor := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	clk := NewSimClockAnchored(epoch, anchor, 10, 0)

	// Still catching up: three days of history to replay at 10×, one minute in.
	catchingUp := anchor.Add(time.Minute)
	clk.wallFn = func() time.Time { return catchingUp }
	if got := clk.effectiveNowRate(); got != 10 {
		t.Fatalf("while catching up the clock advances at 10×; effective rate = %v", got)
	}
	if !clk.Now().Before(catchingUp) {
		t.Fatalf("sanity: a catching-up clock is behind wall; Now=%v wall=%v", clk.Now(), catchingUp)
	}

	// Caught up: simulated time has passed the wall, so Now() is pinned to wall.
	caughtUp := anchor.Add(30 * 24 * time.Hour)
	clk.wallFn = func() time.Time { return caughtUp }
	if !clk.Now().Equal(caughtUp) {
		t.Fatalf("sanity: a caught-up clamped clock returns wall time; Now=%v wall=%v", clk.Now(), caughtUp)
	}
	if got := clk.effectiveNowRate(); got != 1 {
		t.Fatalf("a clamped clock advances at wall rate, so its waits must too; effective rate = %v — "+
			"this is the two-speed clock: Now() at 1× while After()/NewTicker() divide by 10", got)
	}
	if got := clk.currentSpeed(); got != 1 {
		t.Fatalf("the ticker pump reads a different rate from After(); currentSpeed = %v", got)
	}
}

// A running clock is never clamped, so it sustains its multiplier and both halves
// agree at speed× throughout.
func TestRunningClockKeepsItsMultiplier(t *testing.T) {
	clk := NewRunningClock(10)
	clk.wallFn = func() time.Time { return clk.start.Add(time.Hour) }
	if got := clk.effectiveNowRate(); got != 10 {
		t.Fatalf("a running clock never clamps; effective rate = %v", got)
	}
	if want := clk.start.Add(10 * time.Hour); !clk.Now().Equal(want) {
		t.Fatalf("Now=%v want=%v (10× one hour)", clk.Now(), want)
	}
}

// epoch == anchor is what the dev config has always set, and what its own comment
// has always described: lockstep AND a real multiplier. It used to build a
// clamping clock whose clamp was permanent from the first tick — the exact
// two-speed shape above. It now builds the clock the comment describes.
func TestEpochAtAnchorIsASyncedRunningClock(t *testing.T) {
	at := time.Date(2026, 7, 12, 2, 30, 0, 0, time.UTC)

	clk, mode := BuildSimClock(at, at, 10, 15)
	if mode != SimSyncedRunning {
		t.Fatalf("mode=%v, want SimSyncedRunning", mode)
	}
	if clk.clampToWall {
		t.Fatal("epoch == anchor must not clamp — the clamp is what made this config 1× for Now() " +
			"and 10× for every ticker, on every run this rig has ever done")
	}

	wall := at.Add(time.Minute)
	clk.wallFn = func() time.Time { return wall }
	if want := at.Add(10 * time.Minute); !clk.Now().Equal(want) {
		t.Fatalf("Now=%v want=%v — the multiplier has to be real for Now(), not just for tickers", clk.Now(), want)
	}
	if got := clk.effectiveNowRate(); got != 10 {
		t.Fatalf("effective rate = %v, want 10", got)
	}
}

// And it keeps the property the shared anchor exists for: two separately-built
// binaries agree exactly. Losing this is 400+ expired-message drops.
func TestSyncedRunningClocksAgreeAcrossBinaries(t *testing.T) {
	at := time.Date(2026, 7, 12, 2, 30, 0, 0, time.UTC)
	core, coreMode := BuildSimClock(at, at, 50, 0) // capped to DefaultSimMaxSpeed
	edge, edgeMode := BuildSimClock(at, at, 50, 0)
	if coreMode != SimSyncedRunning || edgeMode != SimSyncedRunning {
		t.Fatalf("modes = %v / %v, want SimSyncedRunning", coreMode, edgeMode)
	}

	wall := at.Add(90 * time.Second)
	core.wallFn = func() time.Time { return wall }
	edge.wallFn = func() time.Time { return wall }
	if !core.Now().Equal(edge.Now()) {
		t.Fatalf("the builder produced drifting clocks: core=%v edge=%v", core.Now(), edge.Now())
	}
	if want := at.Add(90 * DefaultSimMaxSpeed * time.Second); !core.Now().Equal(want) {
		t.Fatalf("Now=%v want=%v (90s × %v)", core.Now(), want, DefaultSimMaxSpeed)
	}
}

// An epoch BEFORE its anchor is still a history replay and still clamps — that
// mode is not being taken away, it is being told apart from the other one.
func TestEpochBeforeAnchorStillReplaysAndClamps(t *testing.T) {
	epoch := time.Date(2026, 6, 7, 0, 0, 0, 0, time.UTC)
	anchor := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	clk, mode := BuildSimClock(epoch, anchor, 10, 15)
	if mode != SimSyncedFastForward || !clk.clampToWall {
		t.Fatalf("mode=%v clampToWall=%v, want SimSyncedFastForward + clamp", mode, clk.clampToWall)
	}
}
