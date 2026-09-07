//go:build sim

package main

import (
	"context"
	"database/sql"
	"log"
	"os"
	"time"

	"shingo/protocol/clock"
	"shingoedge/config"
	"shingoedge/engine"
	"shingoedge/plc"
	"shingoedge/plc/simwarlink"
)

// simClock holds the clock constructed for sim mode, shared between
// simWarlinkClient and startSimSubsystems so the downtime model gets the
// same SimClock for scaled After() calls.
var simClock clock.Clock

// simGuard enforces the SHINGO_ALLOW_SIM env gate and prints the loud
// not-for-production banner (brief D6). Called from main right after config
// load when cfg.Sim.Enabled. Compiled only into -tags sim builds.
func simGuard() {
	if os.Getenv("SHINGO_ALLOW_SIM") != "1" {
		log.Fatal("[sim] sim.enabled=true but SHINGO_ALLOW_SIM=1 is not set; refusing to start")
	}
	log.Printf("[sim] ================ SIMULATION MODE — NOT FOR PRODUCTION ================")
}

// simWarlinkClient builds the fake WarLink client injected into the PLC manager
// in sim mode (brief T3.1, resolving the J7-deferred injection). Returns nil
// when sim is disabled so engine.Config.Warlink stays nil and NewManager builds
// the real HTTP client. The fake's counter tickers run on context.Background()
// — they live for the process, like the core sim driver (J13). The clock is
// constructed here; T3.2's sim operator will share it when it lands.
func simWarlinkClient(cfg *config.Config) plc.WarlinkClient {
	if !cfg.Sim.Enabled {
		return nil
	}
	log.Printf("[sim] injecting fake WarLink client (%d sim process(es))", len(cfg.Sim.Processes))
	// Build the sim clock via the shared builder so Core and Edge construct it
	// IDENTICALLY — divergence in epoch/anchor/cap is silent clock drift across the
	// Kafka seam (clock.BuildSimClock owns the logic + the 15× cap that MUST match
	// core). SetDefault wires clock.Now() to sim time; simClock is shared with the
	// downtime model + operator.
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
		log.Printf("[sim] live clock: running %.1f× wall, per-process anchor (change live via POST /api/sim/speed)", clk.Speed())
	case clock.SimSyncedRunning:
		log.Printf("[sim] synced running clock: %.0f× wall, sustained (epoch=%s anchor=%s; no wall clamp, "+
			"so Now() and every ticker run at the same speed) — must match core",
			clk.Speed(), simEpoch.Format(time.RFC3339), simAnchor.Format(time.RFC3339))
	case clock.SimSyncedFastForward:
		// The banner names the catch-up window, because that is the only time this
		// clock is faster than wall — see the matching sentence in core's.
		log.Printf("[sim] fast-forward clock (synced): epoch=%s anchor=%s speed=%.0f× while catching up "+
			"to wall, then Now() clamps to 1× (tickers stay at %.0f×) — must match core",
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
	clock.SetDefault(clk) // wire the global now-provider so all clock.Now() use sim time
	simClock = clk
	return simwarlink.NewFakeClient(context.Background(), cfg.Sim, clk)
}

// startSimSubsystems runs the sim-only edge startup sequence after eng.Start()
// (brief T1.3). It exists behind this indirection so main.go never references
// the //go:build sim simoperator package directly.
//
//   - Explicitly start the WarLink poller. engine.Start() skips it because the
//     dev config sets warlink.enabled=false, but the poll loop is what populates
//     m.plcs / drives the production counter pipeline. Without this the
//     production sim is silently inert (blocker S1).
//   - Wire the readiness gate (G3) into the fake PLC so starved machines stop
//     ticking (symmetric with the produce-side no_bin_bound hold).
//   - Start the sim operator (auto LOAD/CLEAR via the EventBus) when enabled
//     (T3.2). It runs on context.Background() — process-lived, like the core
//     driver (J13) — on its own real clock (J16: a shared clock for a
//     manual-clock integration harness is deferred).
//
// The WarLink fake is injected at engine.New() (T3.1), so the poller dials it.
func startSimSubsystems(eng *engine.Engine, cfg *config.Config, wlClient plc.WarlinkClient) {
	log.Printf("[sim] starting WarLink poller explicitly (warlink.enabled=false in dev config)")
	eng.PLCManager().StartWarLinkPoller()

	// Wire the readiness gate (G3) into the fake PLC client.
	// Compose: calendar gate AND machine-readiness gate AND downtime gate.
	if fake, ok := wlClient.(*simwarlink.FakeClient); ok && eng.DB() != nil {
		machineGate := makeReadinessGate(eng.DB().DB)
		cal := simwarlink.NewProductionCalendar(simwarlink.CalendarConfig{
			Enabled: cfg.Sim.Calendar.Enabled,
			Weekend: cfg.Sim.Calendar.Weekend,
			Shifts:  convertShifts(cfg.Sim.Calendar.Shifts),
		})

		// Downtime model (G9): per-machine clustered random outages.
		// The model forces the readiness gate off during downtime and
		// emits start/end events to Core via the outbox.
		var downtimeModel *simwarlink.DowntimeModel
		station := cfg.StationID()
		outboxDB := eng.DB() // store.DB with EnqueueOutbox

		downEmitEvent := func(envData []byte, subject string) {
			if _, err := outboxDB.EnqueueOutbox(envData, subject); err != nil {
				log.Printf("downtime: enqueue outbox: %v", err)
			}
		}
		// setDown callback: no-op placeholder, the downtime check is
		// integrated into the readiness gate below.
		setDown := func(plcName string, down bool) {
			// The readiness gate reads IsDown() directly.
		}

		// Reuse the clock built in simWarlinkClient (saved to the package var)
		// so the downtime model's scaled After() waits run on the SAME timeline
		// as the global clock.Now() — not a second SimClock anchored at a
		// different wall instant (which, under a 100–300× fast-forward, drifts
		// minutes of sim-time apart). simWarlinkClient runs first — it builds the
		// injected WarLink client at engine.New — so simClock is populated here.
		clk := simClock
		if clk == nil {
			clk = clock.Real()
		}
		downtimeModel = simwarlink.NewDowntimeModel(cfg.Sim, clk, station, setDown, downEmitEvent)

		gate := func(plcName string) bool {
			// Downtime gate: if the machine is in a downtime outage, suppress.
			if downtimeModel != nil && downtimeModel.IsDown(plcName) {
				return false
			}
			// Calendar gate: off-shift = no tick.
			if cal != nil && !cal.IsOnShift(clock.Now()) {
				return false
			}
			return machineGate(plcName)
		}

		if cal != nil {
			log.Printf("[sim] readiness gate wired (G3 + calendar + downtime: %d shifts, weekends=%v)",
				len(cfg.Sim.Calendar.Shifts), cfg.Sim.Calendar.Weekend)
		} else {
			log.Printf("[sim] readiness gate wired (G3 + downtime, no calendar)")
		}
		log.Printf("[sim] %s", simwarlink.FormatDowntimeParams(cfg.Sim))
		fake.SetReadinessFunc(gate)

		// Start the downtime model after the gate is wired.
		if downtimeModel != nil {
			downtimeModel.Start(context.Background())
		}
	}

	if cfg.Sim.Operators.Enabled {
		// Share the sim clock so operator LOAD/CLEAR delays re-pace with live
		// speed changes too (not a separate real clock).
		opClk := simClock
		if opClk == nil {
			opClk = clock.Real()
		}
		eng.StartSimOperator(context.Background(), cfg.Sim, opClk)
	}
}

// makeReadinessGate builds a function that checks whether a process's machine
// is ready to tick. A real PLC only increments its counter when the machine
// cycles; if the machine is starved (consume node empty) or has no output bin
// (produce node), it stops. The fake PLC uses this to suppress ticks that would
// otherwise produce the -237 starved-line artifact (deep-dive Issue 2).
//
// The check: for every process_node of the process identified by plcName,
//   - consume nodes: need active_bin_id set AND remaining_uop_cached > 0
//   - produce nodes: need active_bin_id set
//   - manual_swap nodes: skipped (operator-managed, not PLC-ticked)
//
// Returns true if ALL non-manual_swap nodes are ready, false otherwise.
// Returns true on any error (fail-open — a DB glitch shouldn't stop the sim).
func makeReadinessGate(db *sql.DB) simwarlink.ReadinessFunc {
	// The process and active style are resolved per-call from reporting_points
	// (keyed by the PLC name the fake WarLink passes) — no startup cache, so a
	// changeover that re-points rp.style_id is picked up on the very next tick.
	return func(plcName string) bool {
		// Resolve the process and active style for this PLC name from the
		// reporting_points table (seeded by seeddev).
		var processID, styleID int64
		err := db.QueryRow(`
			SELECT p.id, s.id
			FROM reporting_points rp
			JOIN styles s ON rp.style_id = s.id
			JOIN processes p ON s.process_id = p.id
			WHERE rp.plc_name = ? AND rp.enabled = 1 AND s.deleted_at IS NULL
			LIMIT 1`, plcName).Scan(&processID, &styleID)
		if err != nil {
			return true // fail-open on DB error
		}

		// ONE SPELLING. The per-node conditions used to live here, and the sim
		// OPERATOR needs the same answer to decide when to press RELEASE — a
		// carrier is done exactly when the machine has stopped for want of it.
		// Two copies of that is how the two drift apart, and a drift at the zero
		// boundary deadlocks the rig. See engine.SimMachineReady.
		return engine.SimMachineReady(db, processID, styleID)
	}
}

// convertShifts adapts config shift types to calendar shift types.
func convertShifts(shifts []config.SimShiftConfig) []simwarlink.ShiftConfig {
	out := make([]simwarlink.ShiftConfig, len(shifts))
	for i, s := range shifts {
		out[i] = simwarlink.ShiftConfig{Start: s.Start, End: s.End}
		for _, b := range s.Break {
			out[i].Breaks = append(out[i].Breaks, simwarlink.BreakConfig{Start: b.Start, End: b.End})
		}
	}
	return out
}
