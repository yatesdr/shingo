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
	// Build the sim clock via the shared builder so Core and Edge construct it
	// IDENTICALLY — they must agree on epoch/anchor/cap or their fast-forward clocks
	// drift apart (clock.BuildSimClock owns that logic + the default 15× cap). Always
	// a SimClock so the dev speed toggle (POST /api/sim/speed) re-paces live;
	// SetDefault wires clock.Now() to sim time.
	clk, mode := clock.BuildSimClock(cfg.Sim.Epoch, cfg.Sim.AnchorWall, cfg.Sim.Speed, cfg.Sim.MaxSpeed)
	switch mode {
	case clock.SimRunning:
		log.Printf("[sim] live clock: running %.1f× wall (set sim.epoch for fast-forward; change live via POST /api/sim/speed)", clk.Speed())
	case clock.SimSyncedFastForward:
		log.Printf("[sim] fast-forward clock (synced): epoch=%s anchor=%s speed=%.0f× (Core/Edge in lockstep)",
			cfg.Sim.Epoch.Format(time.RFC3339), cfg.Sim.AnchorWall.Format(time.RFC3339), clk.Speed())
	case clock.SimUnsyncedFastForward:
		log.Printf("[sim] fast-forward clock (UNSYNCED — set sim.anchor_wall in BOTH core+edge to stop clock drift): epoch=%s speed=%.0f×",
			cfg.Sim.Epoch.Format(time.RFC3339), clk.Speed())
	}
	if clk.RequestedSpeed() > clk.Speed() {
		log.Printf("[sim] requested %.0f× capped to max_speed %.0f× (raise sim.max_speed only if the box keeps up)", clk.RequestedSpeed(), clk.Speed())
	}
	clock.SetDefault(clk)
	rng := rand.New(rand.NewSource(seed))

	sim := simulator.New(simulator.WithClock(clk))
	sim.NewDriverFromConfig(cfg.Sim, clk, rng)

	log.Printf("[sim] fleet simulator ready (seed=%d transit=%s jitter=%.0f%% fail_rate=%.2f) — driver starts after engine wiring",
		seed, cfg.Sim.TransitTime, cfg.Sim.JitterPct*100, cfg.Sim.FailRate)
	// The simulator implements RobotLister (T2.4). Scene-sync is intentionally
	// unimplemented — SceneSync treats the backend scene as authoritative and
	// would delete seeded nodes — so the robot-map stays empty. These other
	// optional fleet capabilities also have no sim equivalent.
	log.Printf("[sim] fleet capabilities unavailable: scene-sync, vendor-proxy, vendor-commander, fire-alarm, node-occupancy")
	return sim, nil
}
