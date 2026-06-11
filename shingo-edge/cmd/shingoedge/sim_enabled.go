//go:build sim

package main

import (
	"context"
	"database/sql"
	"log"
	"os"
	"time"

	"shingo/protocol"
	"shingo/shared/clock"
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
	// Always a SimClock so the dev speed toggle can change the multiplier live
	// via SetSpeed. With sim.epoch set it fast-forwards; otherwise it's a running
	// clock that sustains cfg.Sim.Speed × real time (no clamp). Saved to the
	// package var so startSimSubsystems shares the SAME clock with the downtime
	// model + operator.
	// Cap effective speed (must match core's max_speed) so over-cranking degrades
	// to the sustainable rate instead of wedging — see core sim_enabled.go.
	maxSpeed := cfg.Sim.MaxSpeed
	if maxSpeed <= 0 {
		maxSpeed = 15
	}
	var clk *clock.SimClock
	switch {
	case cfg.Sim.Epoch.IsZero():
		clk = clock.NewRunningClock(cfg.Sim.Speed)
		clk.SetMaxSpeed(maxSpeed)
		log.Printf("[sim] live clock: running %.1f× wall (change live via POST /api/sim/speed)", clk.Speed())
	case !cfg.Sim.AnchorWall.IsZero():
		// Fast-forward SYNCED to a shared wall anchor — must match core's epoch +
		// anchor_wall + speed so the two clocks compute identical sim-now and don't
		// drift (cross-process expiry stays correct). Cap baked in by the constructor,
		// NOT SetMaxSpeed (which re-anchors per-process and would reintroduce drift).
		clk = clock.NewSimClockAnchored(cfg.Sim.Epoch, cfg.Sim.AnchorWall, cfg.Sim.Speed, maxSpeed)
		log.Printf("[sim] fast-forward clock (synced): epoch=%s anchor=%s speed=%.0f× (must match core)",
			cfg.Sim.Epoch.Format(time.RFC3339), cfg.Sim.AnchorWall.Format(time.RFC3339), clk.Speed())
	default:
		clk = clock.NewSimClock(cfg.Sim.Epoch, cfg.Sim.Speed)
		clk.SetMaxSpeed(maxSpeed)
		log.Printf("[sim] fast-forward clock (UNSYNCED — set sim.anchor_wall in BOTH core+edge to stop clock drift): epoch=%s speed=%.0f×",
			cfg.Sim.Epoch.Format(time.RFC3339), clk.Speed())
	}
	if clk.RequestedSpeed() > clk.Speed() {
		log.Printf("[sim] requested %.0f× capped to max_speed %.0f×", clk.RequestedSpeed(), clk.Speed())
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
		machineGate := makeReadinessGate(eng.DB().DB, cfg.Sim)
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

		var gate simwarlink.ReadinessFunc
		gate = func(plcName string) bool {
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
func makeReadinessGate(db *sql.DB, cfg config.SimConfig) simwarlink.ReadinessFunc {
	// Build a map from plcName → the process's reporting point style_id so we
	// can look up the process at runtime. plcName → style_id mapping is fixed
	// at startup (the seed creates it once).
	type procInfo struct {
		processID int64
		styleID   int64
	}
	procs := make(map[string]procInfo)
	for _, p := range cfg.Processes {
		procs[p.PLCName] = procInfo{} // placeholder, resolved below
	}

	return func(plcName string) bool {
		// Resolve the process and active style for this PLC name from the
		// reporting_points table (seeded by seeddev).
		var processID, styleID int64
		err := db.QueryRow(`
			SELECT p.id, s.id
			FROM reporting_points rp
			JOIN styles s ON rp.style_id = s.id
			JOIN processes p ON s.process_id = p.id
			WHERE rp.plc_name = ? AND rp.enabled = 1
			LIMIT 1`, plcName).Scan(&processID, &styleID)
		if err != nil {
			return true // fail-open on DB error
		}

		// Check every non-manual_swap node of this process under the active style.
		rows, err := db.Query(`
			SELECT c.role, c.swap_mode, c.uop_capacity, r.active_bin_id, r.remaining_uop_cached, r.active_pull
			FROM process_nodes pn
			JOIN style_node_claims c ON c.style_id = ? AND c.core_node_name = pn.core_node_name
			JOIN process_node_runtime_states r ON r.process_node_id = pn.id
			WHERE pn.process_id = ?`, styleID, processID)
		if err != nil {
			return true // fail-open
		}
		defer rows.Close()

		for rows.Next() {
			var role, swapMode string
			var uopCap, remainingUOP int
			var activeBinID sql.NullInt64
			var activePull bool
			if err := rows.Scan(&role, &swapMode, &uopCap, &activeBinID, &remainingUOP, &activePull); err != nil {
				return true
			}
			// manual_swap nodes are operator-managed, not PLC-ticked.
			if swapMode == string(protocol.SwapModeManualSwap) {
				continue
			}
			// Parked A/B side (active_pull=false): the line isn't filling/draining
			// it right now, so its fill level doesn't gate the cell — the active
			// partner does. Skip it entirely (it may legitimately sit full while
			// parked, awaiting its swap-out). The bound-bin checks below only apply
			// to nodes the line is actually working.
			if !activePull {
				continue
			}
			// All active non-manual_swap nodes need a bound bin.
			if !activeBinID.Valid || activeBinID.Int64 == 0 {
				return false // no bin bound
			}
			// Consume nodes need UOP > 0 (not starved) — a real cell can't cycle an
			// empty input, so the counter must stop rather than drive the count
			// negative.
			if role == "consume" && remainingUOP <= 0 {
				return false // starved
			}
			// Produce nodes must stop when the output bin is full — a real machine
			// can't cycle into a full bin. The relief swap (or A/B flip) carries it
			// out and binds an empty, then the gate reopens. Without this the count
			// drives past capacity. A/B headroom comes from the parked partner, which
			// is skipped above and becomes active on the flip.
			if role == "produce" && uopCap > 0 && remainingUOP >= uopCap {
				return false // output full
			}
		}
		return true // all checks passed
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
