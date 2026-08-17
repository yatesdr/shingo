package clock

import (
	"sync"
	"time"
)

// SimClock is a clock whose simulated time advances at a speed multiplier
// relative to real time. Two modes:
//
//   - Fast-forward (NewSimClock): epoch is typically `now - 30d`; the clock
//     advances at 100-300× until it catches up to wall-now, then clamps and
//     tracks real time. Use this to generate a month of history quickly.
//   - Running / live (NewRunningClock): epoch is now and the clock keeps
//     advancing at `speed` past wall-now (no clamp), so a cranked sim sustains
//     N× indefinitely — a 20-minute transit completes in 20/speed real minutes.
//
// When speed = 1 a running clock behaves like Real(). Thread-safe.
type SimClock struct {
	mu          sync.Mutex
	epoch       time.Time        // simulated start time
	speed       float64          // EFFECTIVE time multiplier (the rate the clock actually advances; clamped to maxSpeed)
	requested   float64          // last requested multiplier before clamping — for an honest "asked N×, running M×" readout
	maxSpeed    float64          // effective-speed cap; 0 = uncapped. Past this the clock degrades to the cap instead of outrunning the real choreography and wedging.
	start       time.Time        // real wall time when the clock was created
	wallFn      func() time.Time // injectable for tests; defaults to time.Now
	clampToWall bool             // fast-forward stops at wall-now once caught up; a running clock does not
}

// clampLocked returns speed bounded by maxSpeed (when set). Caller holds s.mu.
func (s *SimClock) clampLocked(speed float64) float64 {
	if s.maxSpeed > 0 && speed > s.maxSpeed {
		return s.maxSpeed
	}
	return speed
}

// NewSimClock creates a fast-forward clock starting at epoch, advancing at the
// given multiplier, that clamps to wall-now once it catches up. speed <= 0
// defaults to 1.0.
func NewSimClock(epoch time.Time, speed float64) *SimClock {
	if speed <= 0 {
		speed = 1.0
	}
	return &SimClock{
		epoch:       epoch,
		speed:       speed,
		requested:   speed,
		start:       time.Now(),
		wallFn:      time.Now,
		clampToWall: true,
	}
}

// NewSimClockAnchored creates a fast-forward clock pinned to a SHARED wall
// anchor: sim-now = epoch + effectiveSpeed × (wallNow − anchorWall). Because the
// anchor (and epoch and speed) are passed in rather than captured per-process at
// construction, two processes given the SAME (epoch, anchorWall, speed, maxSpeed)
// compute IDENTICAL simulated time at any wall instant — regardless of when each
// one booted. That's what keeps Core's and Edge's fast-forward clocks in lockstep,
// so envelope timestamps/expiry agree across the Kafka seam (without it, a few
// seconds of boot skew × a 15× multiplier becomes minutes of clock drift, which
// silently expires cross-process coordination messages and stalls the swap loop).
//
// The maxSpeed cap is baked into the effective speed here, deliberately WITHOUT
// the re-anchoring that SetMaxSpeed does — re-anchoring would reset start to the
// per-process wall-now and reintroduce the drift this constructor exists to avoid.
// speed <= 0 defaults to 1.0; maxSpeed <= 0 means uncapped.
//
// Caveat: a live SetSpeed (dev top-strip) re-anchors per-process and breaks the
// sync — don't change speed live during a synchronized fast-forward history run.
func NewSimClockAnchored(epoch, anchorWall time.Time, speed, maxSpeed float64) *SimClock {
	if speed <= 0 {
		speed = 1.0
	}
	s := &SimClock{
		epoch:       epoch,
		requested:   speed,
		maxSpeed:    maxSpeed,
		start:       anchorWall,
		wallFn:      time.Now,
		clampToWall: true,
	}
	s.speed = s.clampLocked(speed)
	return s
}

// NewRunningClockAnchored is NewRunningClock's synchronized twin: the same
// no-clamp, genuinely-speed× behaviour, computed from a SHARED (epoch, anchor)
// pair so two processes given the same config agree exactly.
//
// ── IT IS THE CLOCK THE RIG'S CONFIG HAS ALWAYS BEEN ASKING FOR ───────────
//
// The dev config sets `epoch == anchor_wall` and says, in its own comment, that
// this makes the sim "fast-forward briefly then run 10× live". Both halves of
// that sentence cannot be true of a CLAMPING clock: after the catch-up a
// fast-forward clock tracks wall time at 1×, forever. What the config wanted was
// lockstep AND speed, and until §R.98 the only construction offering lockstep
// also silently withdrew the speed — Now() at 1× while every ticker ran at 10×.
//
// So the two intents get two constructors instead of one that quietly does the
// wrong half. An epoch BEFORE its anchor means "replay history and then join the
// present" and still clamps, which is correct for what it is for. An epoch at or
// after its anchor means "start here and run fast", and that is this.
//
// speed <= 0 defaults to 1.0; maxSpeed <= 0 means uncapped. The cap is baked in
// here rather than applied via SetMaxSpeed for the same reason
// NewSimClockAnchored does it: SetMaxSpeed re-anchors to a per-process wall-now
// and reintroduces exactly the drift a shared anchor exists to prevent.
func NewRunningClockAnchored(epoch, anchorWall time.Time, speed, maxSpeed float64) *SimClock {
	if speed <= 0 {
		speed = 1.0
	}
	s := &SimClock{
		epoch:       epoch,
		requested:   speed,
		maxSpeed:    maxSpeed,
		start:       anchorWall,
		wallFn:      time.Now,
		clampToWall: false,
	}
	s.speed = s.clampLocked(speed)
	return s
}

// NewRunningClock creates a live clock that starts now and advances at `speed` ×
// real time with NO clamp — so a cranked sim keeps running N× faster than the
// wall clock instead of pinning to the present. speed <= 0 defaults to 1.0
// (≈ real time).
func NewRunningClock(speed float64) *SimClock {
	if speed <= 0 {
		speed = 1.0
	}
	now := time.Now()
	return &SimClock{
		epoch:       now,
		speed:       speed,
		requested:   speed,
		start:       now,
		wallFn:      time.Now,
		clampToWall: false,
	}
}

// DefaultSimMaxSpeed caps the effective sim multiplier. The integration sim (real
// Core+Edge+Kafka+DBs) can only process the choreography so fast; a clock that
// outruns it makes sim-time timeouts (release/abandon) misfire and the loop wedges.
// Core and Edge MUST cap at the SAME value or their fast-forward clocks drift —
// sharing this const through BuildSimClock is what guarantees they can't diverge.
const DefaultSimMaxSpeed = 15.0

// SimMode is which kind of clock BuildSimClock constructed, returned so the caller
// can log a binary-appropriate banner without re-deriving (and re-risking) the
// construction switch.
type SimMode int

const (
	SimRunning             SimMode = iota // live: no epoch → runs speed× wall, never clamps, per-process anchor (drifts)
	SimSyncedFastForward                  // epoch BEFORE a shared anchor → replays history at speed×, then clamps to wall at 1×
	SimUnsyncedFastForward                // same, no shared anchor → drifts vs the other binary
	// SimSyncedRunning is epoch AT OR AFTER a shared anchor: no clamp, so it
	// genuinely sustains speed×, and shared-anchor so two binaries agree. It is
	// what the dev config has always been describing and never got (§R.98).
	SimSyncedRunning
)

// BuildSimClock constructs the sim clock from the (epoch, anchorWall, speed,
// maxSpeed) quartet IDENTICALLY for every binary. Core and Edge call this with the
// same sim config, so they cannot diverge in how they cap or anchor — divergence is
// silent cross-process clock drift (the exact failure NewSimClockAnchored exists to
// prevent; see docs/dev-env/sim.md). maxSpeed <= 0 defaults to DefaultSimMaxSpeed.
// Returns the clock and the mode it built.
//
// The cap is applied per-mode and that distinction is load-bearing: a synced
// fast-forward bakes it into NewSimClockAnchored, NOT via SetMaxSpeed (which
// re-anchors to the per-process wall-now and reintroduces the very drift the shared
// anchor avoids); the other two modes have no shared anchor to preserve, so they
// SetMaxSpeed after construction.
// ── THE CLAMP IS NO LONGER SILENT, AND IT NO LONGER EATS `speed` (§R.98 D) ──
//
// A clamping clock runs at speed× only while it is CATCHING UP: `nowLocked` pins
// to wall the moment simulated time passes it. That is correct for what it was
// built for — replaying a month of history and then joining the present.
//
// It was a trap for every configuration that was not that, and the rig's was not
// that: it set `epoch == anchor_wall`, so simulated time overtook wall on the
// first tick and the clamp was permanent from t=0. `Now()` returned wall time
// forever, at 1×, while `After` and `NewTicker` went on dividing by speed and
// running at 10×. One clock, two speeds, structurally, since the day the epoch
// was written. Production simulated at 10×; transit, Core's floors and every
// sweep ran at 1×; the banner said 10×; and the rig never once measured the
// economy its config was tuned for.
//
// Nothing detected it because each half is individually correct. So the SHAPE
// picks the construction, rather than one construction quietly doing the wrong
// half of what was asked:
//
//	epoch BEFORE anchor → "replay history, then join the present". Clamps, and
//	                      the banner now says when it stops being fast.
//	epoch AT/AFTER anchor → "start here and run fast, in lockstep". No clamp;
//	                      genuinely speed×, which is what the config's own
//	                      comment claims and never got.
//	no epoch            → running, per-process anchor. Unchanged.
//
// AND NO CONFIG IS REFUSED, because after this stage none of them is
// contradictory. §R.98 asked for "Now() and After() agree, or the config is
// refused"; the first branch is the stronger one and it is what is built. A
// clamped clock now slows its waits along with its Now() (effectiveNowRate), so
// the two halves cannot disagree for ANY config — where a refusal would only
// have removed the configs where they happened to, and left the disagreement in
// the code for the ones that passed. The refusal was drafted and dropped: its
// predicate has to read time.Now(), which makes the builder answer differently
// on different days and would reject a replay config purely for having aged.
func BuildSimClock(epoch, anchorWall time.Time, speed, maxSpeed float64) (*SimClock, SimMode) {
	if maxSpeed <= 0 {
		maxSpeed = DefaultSimMaxSpeed
	}
	if epoch.IsZero() {
		clk := NewRunningClock(speed)
		clk.SetMaxSpeed(maxSpeed)
		return clk, SimRunning
	}

	// No shared anchor means the clock anchors on this process's own wall-now,
	// so that is the anchor every question below is asked about.
	anchor, synced := anchorWall, true
	if anchorWall.IsZero() {
		anchor, synced = time.Now(), false
	}

	if !epoch.Before(anchor) {
		// "Start here and run fast." Only the synced form is reachable — without a
		// shared anchor this is NewRunningClock, which the epoch.IsZero() arm
		// already built.
		if !synced {
			clk := NewRunningClock(speed)
			clk.SetMaxSpeed(maxSpeed)
			return clk, SimRunning
		}
		return NewRunningClockAnchored(epoch, anchor, speed, maxSpeed), SimSyncedRunning
	}

	if synced {
		return NewSimClockAnchored(epoch, anchor, speed, maxSpeed), SimSyncedFastForward
	}
	clk := NewSimClock(epoch, speed)
	clk.SetMaxSpeed(maxSpeed)
	return clk, SimUnsyncedFastForward
}

// effectiveNowRate is the rate at which `Now()` is ACTUALLY advancing right now,
// and it is what every waiting primitive on this clock divides by.
//
// ── THE ONE ANSWER TO "HOW FAST IS THE WORLD GOING" (§R.98 stage D) ───────
//
// `After` and `NewTicker` used to divide by `s.speed` unconditionally, while
// `Now()` returned wall time whenever the clamp was in force. That is not a
// clock, it is two clocks: on the rig it made production tick at 10× while
// transit, every Core floor and every sweep ran at 1×, and the rig has never
// measured the economy its config was tuned for.
//
// A clamped clock is advancing at exactly wall rate — that is what the clamp
// MEANS — so anything waiting on it must wait in wall time too. Asking the same
// function on both paths is what makes them incapable of disagreeing, which is
// stronger than refusing the configs where they happened to. The alternative
// considered and rejected was refusing a spent replay at construction: that
// predicate has to read `time.Now()`, so it makes the builder answer differently
// on different days and leaves the disagreement in the code for every config
// that passes.
//
// Caller must NOT hold s.mu.
func (s *SimClock) effectiveNowRate() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clampToWall {
		wallNow := s.wallFn()
		simNow := s.epoch.Add(time.Duration(float64(wallNow.Sub(s.start)) * s.speed))
		if simNow.After(wallNow) {
			return 1 // caught up: this clock is tracking wall, and so do its waits
		}
	}
	return s.speed
}

// Now returns the current simulated time.
func (s *SimClock) Now() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nowLocked()
}

func (s *SimClock) nowLocked() time.Time {
	wallNow := s.wallFn()
	elapsed := wallNow.Sub(s.start)

	// Simulated time = epoch + speed × elapsed
	simNow := s.epoch.Add(time.Duration(float64(elapsed) * s.speed))

	// Fast-forward clocks clamp at wall-now once caught up; a running (live)
	// clock keeps advancing past wall-now so the sim sustains N×.
	if s.clampToWall && simNow.After(wallNow) {
		return wallNow
	}
	return simNow
}

// After returns a channel that fires after simulated duration d.
// The real wait is d/speed.
func (s *SimClock) After(d time.Duration) <-chan time.Time {
	// The rate Now() is actually advancing at, not the configured multiplier —
	// see effectiveNowRate. A clamped clock waits in wall time because it is
	// PASSING wall time.
	realDur := time.Duration(float64(d) / s.effectiveNowRate())
	return time.After(realDur)
}

// NewTicker returns a ticker that fires every simulated duration d. The real
// interval is d/speed, recomputed on every tick so a live SetSpeed re-paces the
// ticker on the next cycle — this is what lets the dev speed toggle change the
// production/transit rate mid-run. The channel delivers the current simulated
// time at each tick.
func (s *SimClock) NewTicker(d time.Duration) Ticker {
	t := &simTicker{
		clk:     s,
		baseDur: d,
		ch:      make(chan time.Time, 1),
		stop:    make(chan struct{}),
	}
	go t.pump()
	return t
}

type simTicker struct {
	clk      *SimClock
	baseDur  time.Duration // simulated interval between ticks
	ch       chan time.Time
	stop     chan struct{}
	stopOnce sync.Once
}

func (t *simTicker) C() <-chan time.Time { return t.ch }

// Stop halts the ticker. Idempotent — safe to call more than once, matching
// time.Ticker.Stop, so a defer plus an explicit Stop can't double-close.
func (t *simTicker) Stop() {
	t.stopOnce.Do(func() { close(t.stop) })
}

// pump arms a one-shot timer each cycle at the CURRENT speed, so a live
// SetSpeed takes effect on the very next tick instead of staying frozen at the
// rate in force when the ticker was created.
func (t *simTicker) pump() {
	for {
		realDur := time.Duration(float64(t.baseDur) / t.clk.currentSpeed())
		if realDur < time.Millisecond {
			realDur = time.Millisecond // floor to avoid spinning
		}
		timer := time.NewTimer(realDur)
		select {
		case <-t.stop:
			timer.Stop()
			return
		case <-timer.C:
			now := t.clk.Now()
			select {
			case t.ch <- now:
			default: // non-blocking: coalesce if consumer is slow
			}
		}
	}
}

// SetSpeed changes the speed multiplier live. Takes effect on the next
// Now() / After() call. Existing tickers continue at their original real
// interval (they were already created with a fixed duration). To change
// ticker speed, create a new one.
func (s *SimClock) SetSpeed(speed float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if speed <= 0 {
		speed = 1.0
	}
	s.requested = speed
	// Re-anchor: the new speed starts from the current sim time. A running
	// clock keeps its accumulated lead over wall-now; only a fast-forward clock
	// clamps the re-anchor point.
	simNow := s.nowLocked()
	wallNow := s.wallFn()
	if s.clampToWall && simNow.After(wallNow) {
		simNow = wallNow
	}
	s.epoch = simNow
	s.start = wallNow
	// Effective speed is bounded by maxSpeed: the integration sim (real
	// Core+Edge+Kafka+DBs) can only process the choreography so fast, and a clock
	// that outruns it makes sim-time timeouts (release/abandon) misfire and the
	// loop wedge. Past the cap we record the request (RequestedSpeed) but run at
	// the cap.
	s.speed = s.clampLocked(speed)
}

// SetMaxSpeed sets the effective-speed cap and re-clamps the current speed,
// re-anchoring so the change is seamless. 0 = uncapped. Wired from
// sim.max_speed at startup so over-cranking the dev top-strip degrades to the
// real sustainable rate instead of wedging the loop.
func (s *SimClock) SetMaxSpeed(max float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maxSpeed = max
	simNow := s.nowLocked()
	wallNow := s.wallFn()
	if s.clampToWall && simNow.After(wallNow) {
		simNow = wallNow
	}
	s.epoch = simNow
	s.start = wallNow
	s.speed = s.clampLocked(s.requested)
}

// Epoch returns the simulated start time.
func (s *SimClock) Epoch() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.epoch
}

// Speed returns the current EFFECTIVE multiplier (the rate the clock actually
// advances, after the maxSpeed clamp) — for the dev speed readout / endpoint.
func (s *SimClock) Speed() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.speed
}

// RequestedSpeed returns the last requested multiplier (pre-clamp). It exceeds
// Speed() when a request was capped by SetMaxSpeed — lets the dev top-strip show
// "asked N×, running M×" honestly.
func (s *SimClock) RequestedSpeed() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requested
}

// MaxSpeed returns the effective-speed cap (0 = uncapped).
func (s *SimClock) MaxSpeed() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxSpeed
}

// currentSpeed returns the live speed, treated as 1.0 if unset, under lock —
// used by the re-pacing ticker.
func (s *SimClock) currentSpeed() float64 {
	// effectiveNowRate, not s.speed: a clamped clock is advancing at wall rate, so
	// its tickers must tick at wall rate too — see effectiveNowRate for why this is
	// one function and not two (§R.98 stage D).
	if r := s.effectiveNowRate(); r > 0 {
		return r
	}
	return 1.0
}
