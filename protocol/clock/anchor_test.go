package clock_test

import (
	"testing"
	"time"

	"shingo/protocol/clock"
)

// The env anchor becomes BOTH epoch and anchorWall, which is what makes
// BuildSimClock pick SimSyncedRunning — no wall clamp, so the multiplier is
// genuinely sustained, and a shared origin, so two processes agree.
func TestResolveAnchor_EnvBecomesEpochAndAnchorAndSelectsSyncedRunning(t *testing.T) {
	t.Setenv(clock.AnchorEnv, "2026-09-06T14:00:00Z")

	// Config values that would otherwise have won, and that are the exact shape
	// of the stale literal this replaces.
	stale := time.Date(2026, 7, 12, 2, 30, 0, 0, time.UTC)
	epoch, anchor, src, err := clock.ResolveAnchor(stale, stale)
	if err != nil {
		t.Fatalf("ResolveAnchor: %v", err)
	}
	if src != clock.AnchorEnv {
		t.Errorf("source = %q, want %q", src, clock.AnchorEnv)
	}
	want := time.Date(2026, 9, 6, 14, 0, 0, 0, time.UTC)
	if !epoch.Equal(want) || !anchor.Equal(want) {
		t.Fatalf("epoch=%s anchor=%s, want both %s", epoch, anchor, want)
	}
	if _, mode := clock.BuildSimClock(epoch, anchor, 2, 0); mode != clock.SimSyncedRunning {
		t.Errorf("mode = %v, want SimSyncedRunning", mode)
	}
}

// Two processes booting at DIFFERENT wall instants must compute the same
// simulated time — the whole reason the anchor is shared rather than
// per-process. This is the property a per-boot anchor loses, at boot-skew x
// speed, which is what expired the cross-process coordination messages.
func TestResolveAnchor_TwoProcessesAgreeDespiteBootSkew(t *testing.T) {
	t.Setenv(clock.AnchorEnv, "2026-09-06T14:00:00Z")
	epoch, anchor, _, err := clock.ResolveAnchor(time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("ResolveAnchor: %v", err)
	}
	a, _ := clock.BuildSimClock(epoch, anchor, 10, 0)
	time.Sleep(20 * time.Millisecond) // stand in for boot skew
	b, _ := clock.BuildSimClock(epoch, anchor, 10, 0)

	// Both read at the same instant; a per-process anchor would put them
	// ~200ms apart here (20ms of skew at 10x) and further as speed rises.
	skew := a.Now().Sub(b.Now())
	if skew < 0 {
		skew = -skew
	}
	if skew > 5*time.Millisecond {
		t.Errorf("clocks disagree by %s; a shared anchor must make them identical", skew)
	}
}

// Absent the env the config still decides, so replaying history (an epoch
// deliberately BEFORE its anchor) keeps working exactly as before.
func TestResolveAnchor_UnsetLeavesTheConfigAlone(t *testing.T) {
	t.Setenv(clock.AnchorEnv, "")
	epoch := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	anchor := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	gotE, gotA, src, err := clock.ResolveAnchor(epoch, anchor)
	if err != nil {
		t.Fatalf("ResolveAnchor: %v", err)
	}
	if src != "config" || !gotE.Equal(epoch) || !gotA.Equal(anchor) {
		t.Fatalf("got (%s, %s, %s), want the config pair back unchanged", gotE, gotA, src)
	}
	if _, mode := clock.BuildSimClock(gotE, gotA, 2, 0); mode != clock.SimSyncedFastForward {
		t.Errorf("mode = %v, want SimSyncedFastForward (epoch before anchor still replays)", mode)
	}
}

// A malformed anchor is refused rather than silently falling back. Falling back
// would restore the stale-literal clock under an env var whose presence says
// somebody meant to control it — and the two binaries might not fail alike.
func TestResolveAnchor_MalformedIsRefused(t *testing.T) {
	t.Setenv(clock.AnchorEnv, "yesterday")
	if _, _, _, err := clock.ResolveAnchor(time.Time{}, time.Time{}); err == nil {
		t.Fatal("want an error for a non-RFC3339 anchor, got nil")
	}
}

// The defect this whole unit exists for, stated as arithmetic: with a FIXED
// origin the sim/wall offset is (speed-1) x elapsed and grows without bound,
// whether or not the stack ever ran. A run-minted anchor starts at zero.
func TestFixedOriginDriftsAndAFreshAnchorDoesNot(t *testing.T) {
	const speed = 2.0
	origin := time.Date(2026, 7, 12, 2, 30, 0, 0, time.UTC)
	elapsed := 56 * 24 * time.Hour // the literal's age on 2026-09-06
	wallNow := origin.Add(elapsed)

	// sim_now = epoch + speed x (wallNow - anchor)
	simNow := origin.Add(time.Duration(float64(elapsed) * speed))
	drift := simNow.Sub(wallNow)
	if want := time.Duration(float64(elapsed) * (speed - 1)); drift != want {
		t.Fatalf("drift = %s, want (speed-1) x elapsed = %s", drift, want)
	}
	if drift < 55*24*time.Hour {
		t.Fatalf("drift = %s; the recorded rig was ~8 weeks ahead", drift)
	}

	// Every live duration in the UI was Date.now() - simStamp, i.e. negative by
	// the drift, and formatDuration clamps negatives to zero. That is the "0 s".
	if wallNow.Sub(simNow) >= 0 {
		t.Fatal("expected a negative browser-minus-sim difference")
	}

	// Minted at bring-up, the same arithmetic starts at zero.
	fresh := wallNow
	if d := fresh.Add(time.Duration(float64(0) * speed)).Sub(wallNow); d != 0 {
		t.Fatalf("a run-minted anchor must start with zero drift, got %s", d)
	}
}
