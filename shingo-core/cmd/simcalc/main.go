// Command simcalc is the fill/starve helper for sim plant building.
//
// Two modes:
//
//   - CHECK (default): given a plant spec (plants/demo.yaml) and the edge sim tick
//     rates (shingo-edge/shingoedge.dev.yaml), compute the produce vs consume rate
//     of every payload and flag imbalances — a payload consumed faster than it's
//     made STARVES; one made faster than it's drained OVERFILLS. The cheap check
//     you run after editing a plant instead of bringing the stack up for an hour
//     and watching it jam. Exits non-zero if anything is unbalanced (gates a seed).
//
//   - SOLVE (-solve): don't read the tick rates — DERIVE them. Anchor the lines
//     (cells that consume) at a reference rate and compute the tick each pure
//     producer (press) needs so every payload balances, then print a paste-ready
//     sim.processes block. With -transit, also sizes reorder points + bins so a
//     cell doesn't starve waiting for a bin to cross the floor (round-2 §2.3).
//
//   - FLEET (-fleet): the robot estimator (round-2 §3.1 / G16). Same rate model,
//     pointed at the AMR fleet: each cell/press swaps a bin every capacity/rate
//     minutes, each swap occupies robots for its choreography's floor crossings ×
//     transit, and the summed offered load (erlangs) ÷ target utilization is the
//     recommended fleet. The dual of the fill/starve check — that sizes inventory,
//     this sizes the fleet that moves it. Tune with -transit and -util.
//
// Run:  make dev-rates            (check)
//
//	    make dev-rates-solve      (solve)
//	    make dev-fleet            (fleet)
//	or: (cd shingo-core && go run ./cmd/simcalc [-solve] -plant ../plants/demo.yaml \
//	                                            -edge ../shingo-edge/shingoedge.dev.yaml)
//
// The rate model mirrors the engine: a PLC counter tick is applied to every node
// of the active (process, style) EXCEPT parked A/B sides (active_pull=false are
// skipped — see wiring_counter_delta.go). Honoring plantspec's IsActivePull()
// gives exact per-payload rates with no special A/B math. manual_swap loaders /
// unloaders carry no counter; they're modeled as C-push supply / end-gate drain
// whose capacity is set by the operator cadence in the edge config.
//
// SIMULATION / DEV TOOLING ONLY — not compiled into or used by production.
package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"shingocore/plantspec"
)

// edgeSim is the minimal slice of shingoedge.dev.yaml the rate math needs: the
// per-process tick cadence and the operator/downtime knobs. Parsed directly (not
// via the edge config package) so this core-module dev tool needs no cross-module
// import.
type edgeSim struct {
	Sim struct {
		Speed     float64 `yaml:"speed"`
		Processes []struct {
			PLCName      string `yaml:"plc_name"`
			TagName      string `yaml:"tag_name"`
			TickInterval string `yaml:"tick_interval"`
			UOPPerTick   int    `yaml:"uop_per_tick"`
		} `yaml:"processes"`
		Operators struct {
			LoaderAutoLoad    string `yaml:"loader_auto_load"`
			UnloaderAutoClear string `yaml:"unloader_auto_clear"`
		} `yaml:"operators"`
		Downtime struct {
			Enabled  bool `yaml:"enabled"`
			Machines []struct {
				PLCName      string  `yaml:"plc_name"`
				Availability float64 `yaml:"availability"`
			} `yaml:"machines"`
		} `yaml:"downtime"`
	} `yaml:"sim"`
}

// flow accumulates everything known about one payload's material balance.
type flow struct {
	payload   string
	uopCap    int64
	produce   float64  // parts/min from tick-driven produce claims
	consume   float64  // parts/min from tick-driven consume claims
	producers []string // process names feeding it (tick)
	consumers []string // process names draining it (tick)
	loaders   []string // manual_swap produce claims (C-push supply)
	unloaders []string // manual_swap consume claims (end-gate drain)
	bufferUOP int64    // seeded inventory of this payload at t0
	minAvail  float64  // lowest availability across its machines (1.0 = none)
}

// procClass records, per process, what kind of node it is — the basis for the
// solver's anchor-lines / derive-presses split.
type procClass struct {
	hasProduce bool
	hasConsume bool
	hasTick    bool     // has any non-manual claim (carries a counter)
	produces   []string // payloads it produces (tick, active-pull)
}

func main() {
	plantPath := flag.String("plant", "plants/demo.yaml", "path to the plant spec YAML")
	edgePath := flag.String("edge", "shingo-edge/shingoedge.dev.yaml", "path to the edge sim config YAML")
	solve := flag.Bool("solve", false, "derive balanced tick rates instead of checking the configured ones")
	fleet := flag.Bool("fleet", false, "estimate the AMR fleet the plant's swap cadence needs (robot estimator)")
	lineRate := flag.Float64("line-rate", 6.0, "solve: anchor rate (parts/min) for line/cell processes")
	transit := flag.String("transit", "", "solve/fleet: robot transit per move (e.g. 15m). fleet defaults to 10m")
	util := flag.Float64("util", 0.75, "fleet: target robot utilization (0..1); headroom for queueing + empty travel")
	flag.Parse()

	plant, err := plantspec.Load(*plantPath)
	if err != nil {
		fail("load plant: %v", err)
	}

	if *solve {
		runSolve(plant, *lineRate, *transit, *plantPath)
		return
	}

	edge, err := loadEdge(*edgePath)
	if err != nil {
		fail("load edge config: %v", err)
	}
	rate, avail := edgeRates(edge)

	if *fleet {
		runFleet(plant, rate, *transit, *util, *plantPath)
		return
	}
	flows := walkClaims(plant, rate, avail)
	report(flows, capacityPerMin(edge.Sim.Operators.LoaderAutoLoad),
		capacityPerMin(edge.Sim.Operators.UnloaderAutoClear),
		edge.Sim.Downtime.Enabled, *plantPath, *edgePath)
}

// ───────────────────────── rate model (shared by both modes) ─────────────────

// activeClaims returns the claims whose style is the active style of its process —
// the only ones that tick — together with each claim's owning process.
func activeClaims(plant *plantspec.Plant) []struct {
	claim plantspec.Claim
	proc  string
} {
	styleProcess := map[string]string{}
	for _, s := range plant.Styles {
		styleProcess[s.Name] = s.Process
	}
	active := map[string]string{}
	for _, p := range plant.Processes {
		active[p.Name] = p.ActiveStyle
	}
	var out []struct {
		claim plantspec.Claim
		proc  string
	}
	for _, c := range plant.Claims {
		proc := styleProcess[c.Style]
		if active[proc] == c.Style {
			out = append(out, struct {
				claim plantspec.Claim
				proc  string
			}{c, proc})
		}
	}
	return out
}

// walkClaims accumulates per-payload flows given a process→rate (parts/min) map.
func walkClaims(plant *plantspec.Plant, rate, avail map[string]float64) map[string]*flow {
	uopCap := map[string]int64{}
	for _, p := range plant.Payloads {
		uopCap[p.Code] = p.UOPCapacity
	}
	flows := map[string]*flow{}
	get := func(payload string) *flow {
		f := flows[payload]
		if f == nil {
			f = &flow{payload: payload, uopCap: uopCap[payload], minAvail: 1.0}
			flows[payload] = f
		}
		return f
	}

	for _, ac := range activeClaims(plant) {
		c, proc := ac.claim, ac.proc
		f := get(c.Payload)
		if c.IsManualSwap() {
			switch c.Role {
			case "produce":
				f.loaders = appendUniq(f.loaders, proc)
			case "consume":
				f.unloaders = appendUniq(f.unloaders, proc)
			}
			continue
		}
		if !c.IsActivePull() { // parked A/B side — the counter skips it
			continue
		}
		r := rate[proc]
		if a, ok := avail[proc]; ok && a < f.minAvail {
			f.minAvail = a
		}
		switch c.Role {
		case "produce":
			f.produce += r
			f.producers = appendUniq(f.producers, proc)
		case "consume":
			f.consume += r
			f.consumers = appendUniq(f.consumers, proc)
		}
	}
	for _, b := range plant.Bins {
		if b.Payload != "" {
			if f := flows[b.Payload]; f != nil {
				f.bufferUOP += b.UOP
			}
		}
	}
	return flows
}

// edgeRates builds the process→rate and process→availability maps from the edge
// sim config (CHECK mode reads the configured ticks).
func edgeRates(edge *edgeSim) (rate, avail map[string]float64) {
	rate, avail = map[string]float64{}, map[string]float64{}
	for _, p := range edge.Sim.Processes {
		d, err := time.ParseDuration(p.TickInterval)
		if err != nil || d <= 0 {
			fail("process %s: bad tick_interval %q", p.PLCName, p.TickInterval)
		}
		per := p.UOPPerTick
		if per == 0 {
			per = 1
		}
		rate[p.PLCName] = 60.0 / d.Seconds() * float64(per)
		avail[p.PLCName] = 1.0
	}
	if edge.Sim.Downtime.Enabled {
		for _, m := range edge.Sim.Downtime.Machines {
			avail[m.PLCName] = m.Availability
		}
	}
	return rate, avail
}

// classify partitions processes into produce-only (presses, derived by the solver)
// vs consumers (lines, anchored at the reference rate).
func classify(plant *plantspec.Plant) map[string]*procClass {
	cls := map[string]*procClass{}
	for _, ac := range activeClaims(plant) {
		c, proc := ac.claim, ac.proc
		if c.IsManualSwap() {
			continue
		}
		pc := cls[proc]
		if pc == nil {
			pc = &procClass{}
			cls[proc] = pc
		}
		pc.hasTick = true
		if !c.IsActivePull() {
			continue
		}
		switch c.Role {
		case "produce":
			pc.hasProduce = true
			pc.produces = appendUniq(pc.produces, c.Payload)
		case "consume":
			pc.hasConsume = true
		}
	}
	return cls
}

// ───────────────────────────────── SOLVE ─────────────────────────────────────

// solveRates derives a balanced process→rate (parts/min) map: anchor every line
// (a process that consumes) at lineRate, then set each pure producer (press) to
// the demand its output payload sees from those lines. Co-producers of a payload
// split its demand. This IS the calc — it replaces the hand math.
func solveRates(plant *plantspec.Plant, lineRate float64) map[string]float64 {
	cls := classify(plant)
	rate := map[string]float64{}
	for proc, pc := range cls {
		if pc.hasConsume {
			rate[proc] = lineRate
		}
	}
	demand := walkClaims(plant, rate, nil) // what the anchored lines consume
	pureOf := pureProducerCount(cls)
	for proc, pc := range cls {
		if pc.hasProduce && !pc.hasConsume {
			best := 0.0
			for _, pl := range pc.produces {
				if f := demand[pl]; f != nil {
					if d := f.consume / math.Max(1, float64(pureOf[pl])); d > best {
						best = d
					}
				}
			}
			rate[proc] = best
		}
	}
	return rate
}

// pureProducerCount counts how many pure-producer processes make each payload, so
// co-producers (two presses feeding one market) split its demand.
func pureProducerCount(cls map[string]*procClass) map[string]int {
	pureOf := map[string]int{}
	for _, pc := range cls {
		if pc.hasProduce && !pc.hasConsume {
			for _, pl := range pc.produces {
				pureOf[pl]++
			}
		}
	}
	return pureOf
}

func runSolve(plant *plantspec.Plant, lineRate float64, transit, plantPath string) {
	cls := classify(plant)
	rate := solveRates(plant, lineRate)

	// Demand (for the DERIVATION column) = what the anchored lines consume.
	anchor := map[string]float64{}
	for proc, pc := range cls {
		if pc.hasConsume {
			anchor[proc] = lineRate
		}
	}
	demand := walkClaims(plant, anchor, nil)
	pureOf := pureProducerCount(cls)

	type derivation struct {
		proc, kind, why string
		rate            float64
	}
	var rows []derivation
	for proc, pc := range cls {
		switch {
		case pc.hasProduce && !pc.hasConsume: // press — derived
			best, why := 0.0, ""
			for _, pl := range pc.produces {
				if f := demand[pl]; f != nil {
					if d := f.consume / math.Max(1, float64(pureOf[pl])); d >= best {
						best = d
						if n := pureOf[pl]; n > 1 {
							why = fmt.Sprintf("feeds %s: %.0f/min demand ÷ %d presses", pl, f.consume, n)
						} else {
							why = fmt.Sprintf("feeds %s: %.0f/min demand", pl, f.consume)
						}
					}
				}
			}
			rows = append(rows, derivation{proc, "press (derived)", why, rate[proc]})
		case pc.hasConsume: // line — anchored
			rows = append(rows, derivation{proc, "line (anchor)", "reference line rate", rate[proc]})
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].proc < rows[j].proc })

	fmt.Printf("Solve — plant %s, line rate %.0f/min\n\n", plantPath, lineRate)
	fmt.Printf("%-12s %-16s %-8s %-9s %s\n", "PROCESS", "ROLE", "TICK", "RATE/min", "DERIVATION")
	fmt.Println(strings.Repeat("─", 92))
	for _, r := range rows {
		fmt.Printf("%-12s %-16s %-8s %-9.0f %s\n", r.proc, r.kind, tickFor(r.rate), r.rate, r.why)
	}

	// Confirm the derived rates actually balance (run the check on them).
	fmt.Println()
	flows := walkClaims(plant, rate, nil)
	allOK := true
	for _, n := range sortedKeys(flows) {
		if _, ok, _ := flows[n].verdict(math.Inf(1), math.Inf(1)); !ok {
			allOK = false
		}
	}
	fmt.Printf("Result: %s at these rates.\n", headline(allOK))

	tag := map[string]string{}
	for _, rp := range plant.ReportingPoints {
		tag[rp.PLCName] = rp.TagName
	}
	fmt.Println("\nPaste into shingoedge.dev.yaml  sim.processes:")
	for _, r := range rows {
		t := tag[r.proc]
		if t == "" {
			t = r.proc + "_COUNTER"
		}
		fmt.Printf("    - {plc_name: %s, tag_name: %s, tick_interval: %s, uop_per_tick: 1}\n",
			r.proc, t, tickFor(r.rate))
	}

	if transit != "" {
		sizeFromTransit(plant, rate, transit)
	}
}

// sizeFromTransit prints reorder-point + bin-size suggestions so a consume node
// doesn't starve while a replenishment bin crosses the floor (round-2 §2.3: a bin
// must hold more than peak_transit × pull_rate; reorder early enough to launch the
// move before the bin empties).
func sizeFromTransit(plant *plantspec.Plant, rate map[string]float64, transit string) {
	d, err := time.ParseDuration(transit)
	if err != nil || d <= 0 {
		fail("bad -transit %q", transit)
	}
	mins := d.Minutes()
	flows := walkClaims(plant, rate, nil)

	fmt.Printf("\nSizing for %s worst-case transit (per consumed payload):\n", transit)
	fmt.Printf("%-10s %-10s %-14s %-12s %s\n", "PAYLOAD", "PULL/min", "REORDER≥", "BIN CAP≥", "WHY")
	fmt.Println(strings.Repeat("─", 84))
	for _, n := range sortedKeys(flows) {
		f := flows[n]
		if f.consume <= 0 {
			continue
		}
		reorder := int64(math.Ceil(f.consume * mins))            // launch move before empty
		binCap := int64(math.Ceil(f.consume*mins*1.5)) + reorder // hold > transit draw + safety
		fmt.Printf("%-10s %-10.0f %-14d %-12d %s\n", n, f.consume, reorder, binCap,
			fmt.Sprintf("%.0f/min × %.0fm transit", f.consume, mins))
	}
}

// ───────────────────────────────── FLEET ─────────────────────────────────────
//
// The robot estimator (round-2 §3.1 / G16). Every cell and press swaps a bin on a
// cadence set by its rate and bin capacity: a node making/consuming R parts/min
// with a C-uop bin swaps R/C times a minute. Each swap occupies robots for the
// loaded legs of its choreography (material_orders.go) × the per-move transit.
// Summing swaps/min × loaded-moves × transit across every swap point gives the
// OFFERED robot-load in erlangs (the average number of robots busy); the fleet is
// that load divided by a target utilization so the queue doesn't blow up. This is
// the dual of the fill/starve check: that one sizes inventory, this one sizes the
// fleet that moves it — both fall straight out of the same rate model.

// fleetMovesPerSwap returns the floor-spanning loaded crossings in one swap cycle
// and the peak concurrent robots the cycle needs, per swap mode. A "crossing" is a
// material-carrying leg that traverses the floor (≈ one transit); short staging
// hops, the back↔front index hop, and empty repositioning are NOT counted — they're
// small and fall into the util headroom. Crossing counts track the long legs of the
// step builders in engine/material_orders.go.
func fleetMovesPerSwap(mode string) (crossings float64, robots int) {
	switch mode {
	case "simple":
		return 1, 1 // one floor crossing (old out / new in folded)
	case "sequential":
		return 2, 1 // removal (node→market) + backfill (source→node), one robot, serialized
	case "single_robot":
		return 2, 1 // fetch in (source→…→node) + ship out (node→…→market); staging hops are short
	case "two_robot":
		return 2, 2 // supply robot crosses in, removal robot crosses out — one long leg each
	case "two_robot_press_index":
		return 2, 2 // output→market + source→back; the back→front index hop is short
	case "manual_swap":
		return 1, 1 // one loaded crossing per push (loader→market) / pull (line→market)
	default:
		return 1, 1
	}
}

func runFleet(plant *plantspec.Plant, rate map[string]float64, transit string, util float64, plantPath string) {
	d := 10 * time.Minute
	if transit != "" {
		var err error
		if d, err = time.ParseDuration(transit); err != nil || d <= 0 {
			fail("bad -transit %q", transit)
		}
	}
	if util <= 0 || util > 1 {
		fail("bad -util %.2f (want 0..1)", util)
	}
	tmin := d.Minutes()
	flows := walkClaims(plant, rate, nil) // for manual_swap throughput (loader/unloader)

	type frow struct {
		proc, role, payload, mode string
		swaps, moves               float64
		robots                     int
		load                       float64
	}
	var rows []frow
	var offered, peak float64

	for _, ac := range activeClaims(plant) {
		c, proc := ac.claim, ac.proc
		if !c.IsManualSwap() && !c.IsActivePull() {
			continue // parked A/B side — the active partner's cadence already counts the pair's swaps
		}
		cap := c.UOPCapacity
		if cap <= 0 {
			if f := flows[c.Payload]; f != nil {
				cap = f.uopCap
			}
		}
		if cap <= 0 {
			continue
		}
		// parts/min flowing through this node sets its swap cadence.
		var tp float64
		switch {
		case c.IsManualSwap() && c.Role == "produce": // loader: supplies its payload's draw
			if f := flows[c.Payload]; f != nil {
				tp = f.consume
			}
		case c.IsManualSwap() && c.Role == "consume": // unloader: drains its payload's make
			if f := flows[c.Payload]; f != nil {
				tp = f.produce
			}
		default:
			tp = rate[proc]
		}
		if tp <= 0 {
			continue
		}
		swaps := tp / float64(cap)
		moves, robots := fleetMovesPerSwap(c.SwapMode)
		load := swaps * moves * tmin // erlangs: avg robots busy on this node's loaded legs
		offered += load
		peak += float64(robots)
		rows = append(rows, frow{proc, c.Role, c.Payload, c.SwapMode, swaps, moves, robots, load})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].proc != rows[j].proc {
			return rows[i].proc < rows[j].proc
		}
		return rows[i].role < rows[j].role
	})

	fmt.Printf("Fleet sizing — plant %s, transit %s/move, target utilization %.0f%%\n\n", plantPath, d, util*100)
	fmt.Printf("%-12s %-8s %-10s %-22s %-10s %-6s %-7s %s\n",
		"PROCESS", "ROLE", "PAYLOAD", "SWAP MODE", "SWAPS/min", "CROSS", "ROBOTS", "LOAD(erlang)")
	fmt.Println(strings.Repeat("─", 104))
	for _, r := range rows {
		fmt.Printf("%-12s %-8s %-10s %-22s %-10.2f %-6.0f %-7d %.2f\n",
			r.proc, r.role, r.payload, r.mode, r.swaps, r.moves, r.robots, r.load)
	}
	fmt.Println(strings.Repeat("─", 100))

	required := int(math.Ceil(offered / util))
	fmt.Printf("Offered robot-load: %.2f erlang (avg robots continuously busy on loaded legs)\n", offered)
	fmt.Printf("Peak if every swap overlapped: %.0f robots (upper bound, never all at once)\n", peak)
	fmt.Printf("Recommended fleet: ⌈%.2f ÷ %.2f⌉ = %d AMRs\n", offered, util, required)
	fmt.Println("note: counts loaded legs only at the given transit; empty repositioning, queueing, and")
	fmt.Println("      downtime bunching are covered by the utilization headroom. Re-run with -transit to")
	fmt.Println("      match the floor and -util to trade fleet size against queue wait.")
}

// ──────────────────────────────── CHECK ──────────────────────────────────────

// verdict classifies a payload's balance and returns (label, healthy, detail).
func (f *flow) verdict(loaderClearsPerMin, unloaderClearsPerMin float64) (string, bool, string) {
	const eps = 0.01
	hasProd, hasCons := f.produce > eps, f.consume > eps
	hasLoader, hasUnloader := len(f.loaders) > 0, len(f.unloaders) > 0

	if hasLoader && hasCons && !hasProd { // C-push loader feeding a tick consumer (BRKT)
		supply := loaderClearsPerMin * float64(f.uopCap)
		if supply+eps >= f.consume {
			return "C-PUSH", true, fmt.Sprintf("loader supplies %.0f/min ≥ %.0f/min draw", supply, f.consume)
		}
		return "LOADER-BOUND", false, fmt.Sprintf("loader only %.0f/min < %.0f/min draw", supply, f.consume)
	}
	if hasProd && hasUnloader && !hasCons { // tick producer drained by an unloader (ASSY-X/Y)
		drain := unloaderClearsPerMin * float64(f.uopCap)
		if drain+eps >= f.produce {
			return "DRAIN", true, fmt.Sprintf("unloader drains %.0f/min ≥ %.0f/min made", drain, f.produce)
		}
		return "DRAIN-BOUND", false, fmt.Sprintf("unloader only %.0f/min < %.0f/min made", drain, f.produce)
	}
	if hasProd && hasCons { // the core parity case
		net := f.produce - f.consume
		switch {
		case net < -eps:
			return "STARVE", false, fmt.Sprintf("−%.1f/min; buffer drains in %s", -net, runway(f.bufferUOP, -net))
		case net > eps:
			return "OVERFILL", false, fmt.Sprintf("+%.1f/min; no drain keeps up", net)
		default:
			return "BALANCED", true, fmt.Sprintf("buffer covers %s", runway(f.bufferUOP, f.consume))
		}
	}
	if hasProd || hasLoader {
		return "NO-DRAIN", false, "produced/loaded but nothing consumes it"
	}
	return "NO-SOURCE", false, "consumed but nothing produces it"
}

func report(flows map[string]*flow, loaderCap, unloaderCap float64, downtime bool, plantPath, edgePath string) {
	fmt.Printf("Fill/starve — plant %s, rates %s\n\n", plantPath, edgePath)
	fmt.Printf("%-10s %-22s %-22s %-8s %-12s %s\n",
		"PAYLOAD", "PRODUCE/min", "CONSUME/min", "BUFFER", "VERDICT", "DETAIL")
	fmt.Println(strings.Repeat("─", 110))

	allHealthy := true
	var balanced, problems int
	for _, n := range sortedKeys(flows) {
		f := flows[n]
		label, healthy, detail := f.verdict(loaderCap, unloaderCap)
		if healthy {
			balanced++
		} else {
			problems++
			allHealthy = false
		}
		fmt.Printf("%-10s %-22s %-22s %-8s %-12s %s\n",
			n,
			sideStr(f.produce, f.producers, f.loaders, "C-push"),
			sideStr(f.consume, f.consumers, f.unloaders, "drain"),
			fmt.Sprintf("%d uop", f.bufferUOP),
			label, detail)
	}

	fmt.Println(strings.Repeat("─", 110))
	fmt.Printf("%d balanced, %d problem(s).  →  %s\n", balanced, problems, headline(allHealthy))
	if downtime {
		fmt.Println("note: downtime is ON — nominal parity holds in expectation, but buffers must cover the 85%-uptime variance.")
	}
	if !allHealthy {
		os.Exit(1)
	}
}

// ──────────────────────────────── helpers ────────────────────────────────────

func headline(ok bool) string {
	if ok {
		return "SUSTAINS"
	}
	return "WILL JAM"
}

func sortedKeys(m map[string]*flow) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// sideStr renders a produce/consume side: tick rate + the processes driving it,
// or the manual_swap label (C-push / drain) when there's no tick rate.
func sideStr(rate float64, tickProcs, manualProcs []string, manualLabel string) string {
	if rate > 0.01 {
		return fmt.Sprintf("%.0f (%s)", rate, strings.Join(tickProcs, ","))
	}
	if len(manualProcs) > 0 {
		return fmt.Sprintf("%s (%s)", manualLabel, strings.Join(manualProcs, ","))
	}
	return "—"
}

// runway formats how long a buffer lasts at a given drain rate (parts/min).
func runway(bufferUOP int64, ratePerMin float64) string {
	if ratePerMin <= 0 {
		return "∞"
	}
	mins := float64(bufferUOP) / ratePerMin
	if mins >= 60 {
		return fmt.Sprintf("%.1fh", mins/60)
	}
	return fmt.Sprintf("%.0fm", mins)
}

// tickFor turns a parts/min rate into a tick_interval string (60/rate seconds).
func tickFor(ratePerMin float64) string {
	if ratePerMin <= 0 {
		return "—"
	}
	return time.Duration(60.0 / ratePerMin * float64(time.Second)).String()
}

// capacityPerMin turns an operator cadence ("8s") into clears/min. A bad/empty
// value yields 0 (treated as "no operator" by the verdict).
func capacityPerMin(interval string) float64 {
	d, err := time.ParseDuration(interval)
	if err != nil || d <= 0 {
		return 0
	}
	return 60.0 / d.Seconds()
}

func loadEdge(path string) (*edgeSim, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var e edgeSim
	if err := yaml.Unmarshal(data, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

func appendUniq(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "simcalc: "+format+"\n", a...)
	os.Exit(2)
}
