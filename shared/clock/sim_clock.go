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
	SimRunning             SimMode = iota // live: no epoch → runs speed× wall, never clamps
	SimSyncedFastForward                  // epoch + shared anchor → Core/Edge in lockstep
	SimUnsyncedFastForward                // epoch, no shared anchor → drifts vs the other binary
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
func BuildSimClock(epoch, anchorWall time.Time, speed, maxSpeed float64) (*SimClock, SimMode) {
	if maxSpeed <= 0 {
		maxSpeed = DefaultSimMaxSpeed
	}
	switch {
	case epoch.IsZero():
		clk := NewRunningClock(speed)
		clk.SetMaxSpeed(maxSpeed)
		return clk, SimRunning
	case !anchorWall.IsZero():
		return NewSimClockAnchored(epoch, anchorWall, speed, maxSpeed), SimSyncedFastForward
	default:
		clk := NewSimClock(epoch, speed)
		clk.SetMaxSpeed(maxSpeed)
		return clk, SimUnsyncedFastForward
	}
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
	s.mu.Lock()
	speed := s.speed
	s.mu.Unlock()

	realDur := time.Duration(float64(d) / speed)
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.speed <= 0 {
		return 1.0
	}
	return s.speed
}
