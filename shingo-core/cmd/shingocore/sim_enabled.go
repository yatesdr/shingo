//go:build sim

package main

import (
	"context"
	"log"
	"math/rand"
	"os"
	"time"

	"shingo/shared/clock"
	"shingocore/config"
	"shingocore/fleet"
	"shingocore/fleet/simulator"
)

// simGuard enforces the SHINGO_ALLOW_SIM env gate and prints the loud
// not-for-production banner (brief D6). Called from main right after config
// load when cfg.Sim.Enabled. This file is compiled only into -tags sim builds.
func simGuard() {
	if os.Getenv("SHINGO_ALLOW_SIM") != "1" {
		log.Fatal("[sim] sim.enabled=true but SHINGO_ALLOW_SIM=1 is not set; refusing to start")
	}
	log.Printf("[sim] ================ SIMULATION MODE — NOT FOR PRODUCTION ================")
}

// newSimBackend constructs the in-memory fleet simulator and starts the driver
// goroutine that advances orders on a clock in place of the SEER RDS poller
// (brief T1.1 + T2.3). The driver runs until ctx is done; core main currently
// passes context.Background(), so it lives for the process — fine for a dev sim
// (graceful driver shutdown can be wired later if needed).
//
// The PRNG is seeded from cfg.Sim.Seed for reproducible runs; a 0 seed is
// derived from the clock and logged so any run can be replayed by pinning it.
func newSimBackend(ctx context.Context, cfg *config.Config) (fleet.TrackingBackend, error) {
	seed := cfg.Sim.Seed
	if seed == 0 {
		seed = time.Now().UnixNano()
		log.Printf("[sim] no sim.seed set; derived seed %d (set sim.seed to reproduce this run)", seed)
	}
	// Build the sim clock. Always a SimClock so the dev speed toggle can change
	// the multiplier live via SetSpeed (the re-pacing tickers pick it up). With
	// sim.epoch set it fast-forwards from that epoch and clamps at the present;
	// otherwise it's a running clock that sustains cfg.Sim.Speed × real time
	// (orders/transit actually speed up — no clamp). clock.SetDefault wires the
	// global now-provider so every clock.Now() uses sim time.
	// Cap the effective speed so over-cranking the dev top-strip degrades to the
	// sustainable rate instead of wedging the loop — the integration sim can't run
	// faster than the real Core+Edge+Kafka+DB choreography processes, and a clock
	// that outruns it makes sim-time timeouts misfire. Default 15×; must match edge.
	maxSpeed := cfg.Sim.MaxSpeed
	if maxSpeed <= 0 {
		maxSpeed = 15
	}
	var clk *clock.SimClock
	switch {
	case cfg.Sim.Epoch.IsZero():
		clk = clock.NewRunningClock(cfg.Sim.Speed)
		clk.SetMaxSpeed(maxSpeed)
		log.Printf("[sim] live clock: running %.1f× wall (set sim.epoch for fast-forward; change live via POST /api/sim/speed)", clk.Speed())
	case !cfg.Sim.AnchorWall.IsZero():
		// Fast-forward SYNCED to a shared wall anchor: Core and Edge given the same
		// (epoch, anchor_wall, speed) compute identical sim-now, so the two clocks
		// stay in lockstep and cross-process message expiry stays correct. The cap is
		// baked in by NewSimClockAnchored — NOT via SetMaxSpeed, which re-anchors to
		// per-process wall-now and would reintroduce the drift this avoids.
		clk = clock.NewSimClockAnchored(cfg.Sim.Epoch, cfg.Sim.AnchorWall, cfg.Sim.Speed, maxSpeed)
		log.Printf("[sim] fast-forward clock (synced): epoch=%s anchor=%s speed=%.0f× (Core/Edge in lockstep)",
			cfg.Sim.Epoch.Format(time.RFC3339), cfg.Sim.AnchorWall.Format(time.RFC3339), clk.Speed())
	default:
		clk = clock.NewSimClock(cfg.Sim.Epoch, cfg.Sim.Speed)
		clk.SetMaxSpeed(maxSpeed)
		log.Printf("[sim] fast-forward clock (UNSYNCED — set sim.anchor_wall in BOTH core+edge to stop clock drift): epoch=%s speed=%.0f×",
			cfg.Sim.Epoch.Format(time.RFC3339), clk.Speed())
	}
	if clk.RequestedSpeed() > clk.Speed() {
		log.Printf("[sim] requested %.0f× capped to max_speed %.0f× (raise sim.max_speed only if the box keeps up)", clk.RequestedSpeed(), clk.Speed())
	}
	clock.SetDefault(clk)
	rng := rand.New(rand.NewSource(seed))

	sim := simulator.New(simulator.WithClock(clk))
	simulator.StartDriver(ctx, sim, cfg.Sim, clk, rng)

	log.Printf("[sim] fleet simulator + driver started (seed=%d transit=%s jitter=%.0f%% fail_rate=%.2f)",
		seed, cfg.Sim.TransitTime, cfg.Sim.JitterPct*100, cfg.Sim.FailRate)
	// The simulator implements RobotLister (T2.4). Scene-sync is intentionally
	// unimplemented — SceneSync treats the backend scene as authoritative and
	// would delete seeded nodes — so the robot-map stays empty. These other
	// optional fleet capabilities also have no sim equivalent.
	log.Printf("[sim] fleet capabilities unavailable: scene-sync, vendor-proxy, vendor-commander, fire-alarm, node-occupancy")
	return sim, nil
}
