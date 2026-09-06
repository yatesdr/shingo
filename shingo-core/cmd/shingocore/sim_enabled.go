//go:build sim

package main

import (
	"context"
	"log"
	"math/rand"
	"os"
	"time"

	"shingo/protocol/clock"
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
	// The anchor belongs to the RUN, not to the config file — a hardcoded
	// epoch drifts (speed-1)x further from wall time every day it sits in the
	// tree. clock.ResolveAnchor prefers the run's shared env anchor and is the
	// same function on both binaries, so they cannot read it differently. See
	// its doc comment for the arithmetic and for why re-anchoring is tied to
	// fresh volumes.
	simEpoch, simAnchor, anchorSrc, err := clock.ResolveAnchor(cfg.Sim.Epoch, cfg.Sim.AnchorWall)
	if err != nil {
		log.Fatalf("[sim] %v", err)
	}
	if anchorSrc == clock.AnchorEnv {
		log.Printf("[sim] anchor from %s=%s (shared by every process in this run)",
			clock.AnchorEnv, simAnchor.Format(time.RFC3339))
	}
	clk, mode := clock.BuildSimClock(simEpoch, simAnchor, cfg.Sim.Speed, cfg.Sim.MaxSpeed)
	switch mode {
	case clock.SimRunning:
		log.Printf("[sim] live clock: running %.1f× wall, per-process anchor (set sim.anchor_wall in BOTH "+
			"core+edge to stop clock drift; change speed live via POST /api/sim/speed)", clk.Speed())
	case clock.SimSyncedRunning:
		// THE ONE THAT GENUINELY SUSTAINS THE MULTIPLIER. No clamp, so Now() and
		// the tickers agree, and a shared anchor, so Core and Edge do too.
		log.Printf("[sim] synced running clock: %.0f× wall, sustained (epoch=%s anchor=%s; no wall clamp, "+
			"so Now() and every ticker run at the same speed) — Core/Edge in lockstep",
			clk.Speed(), simEpoch.Format(time.RFC3339), simAnchor.Format(time.RFC3339))
	case clock.SimSyncedFastForward:
		// THE BANNER NAMES THE CATCH-UP, because that is the only window in which
		// this clock is faster than wall. Once simulated time passes the wall it
		// clamps and Now() tracks real time at 1× — while tickers keep dividing by
		// speed. Saying "speed=N×" alone is what made a two-speed run look like a
		// tuned one.
		log.Printf("[sim] fast-forward clock (synced): epoch=%s anchor=%s speed=%.0f× while catching up "+
			"to wall, then Now() clamps to 1× (tickers stay at %.0f×) — Core/Edge in lockstep",
			simEpoch.Format(time.RFC3339), simAnchor.Format(time.RFC3339), clk.Speed(), clk.Speed())
	case clock.SimUnsyncedFastForward:
		log.Printf("[sim] fast-forward clock (UNSYNCED — set sim.anchor_wall in BOTH core+edge to stop clock drift): "+
			"epoch=%s speed=%.0f× while catching up to wall, then Now() clamps to 1× (tickers stay at %.0f×)",
			simEpoch.Format(time.RFC3339), clk.Speed(), clk.Speed())
	}
	if clk.RequestedSpeed() > clk.Speed() {
		log.Printf("[sim] REQUESTED %.0f x BUT RUNNING %.0f x - capped at the MEASURED ceiling.\n"+
			"      Dev rig 2026-09-06, orders finished per WALL minute at steady state:\n"+
			"        2x   9.83/wall-min  backlog +0  11 open  104 sim-s mean order life\n"+
			"        5x  21.50/wall-min  backlog +2   9 open  102 sim-s  <- fastest that holds the 2x shape\n"+
			"       10x   4.65/wall-min  backlog +6  33 open  <- HALF of 2x, at 0.44 load: not CPU\n"+
			"      The binding leg is the Edge outbox drain (5s WALL, protocol/outbox/drainer.go:170):\n"+
			"      at N x that is 5N SIMULATED seconds of backstop latency per hop. Dropping it to\n"+
			"      500ms took 10x from 4.65 to 34.50/wall-min - measured - but the same interval is\n"+
			"      also the dead-letter budget (MaxRetries x interval), so lowering it divides a REAL\n"+
			"      broker's recovery window by the same factor. docs/dev-env/sim-speed-ceiling.md.",
			clk.RequestedSpeed(), clk.Speed())
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
