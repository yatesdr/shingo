package main

// ───────────────────────────── CARRIERS ──────────────────────────────────────
//
// THE CHECK THAT WOULD HAVE SAVED THE TWEAKING.
//
// simcalc already balances every PAYLOAD: made vs drained, starve vs overfill.
// What it never modelled is the EMPTY BIN — and an empty bin is a resource with
// its own producers and its own consumers, so it has its own balance and its own
// way of running out.
//
//	a PRODUCE claim (press, loader) takes an empty IN and sends a full OUT
//	                                → it CONSUMES one empty per swap
//	a CONSUME claim (weld cell, unloader) takes a full IN and sends an empty OUT
//	                                → it PRODUCES one empty per swap
//
// Every payload can balance perfectly while the carrier pool drains to zero, and
// when it does the failure is a DEADLOCK rather than a starve: a cell that has
// spent its bin needs a swap, the swap needs an empty to bring in, and empties
// only reappear when a swap completes. Nothing is broken and nothing moves.
//
// Worse, it does not announce itself. The lane-stress rig ran 126 orders and
// then stopped, and every cause on the remaining orders named the wrong thing —
// finder-node-empty pointed at a loader that was empty because no empty had
// arrived to be filled; ngrp-resolve read as a resolver fault while twelve of
// the payload's bins sat available and unlooked-for.
//
// TWO NUMBERS DECIDE IT, and both fall out of the plant file:
//
//  1. RATE. Empties generated per minute must at least match empties consumed.
//     A deficit drains the pool no matter how large it starts.
//  2. STOCK. Even in balance, a swap cannot start until an empty ARRIVES, so the
//     pool must cover everything in flight: demand × transit, plus one per
//     produce station so each always has one it can be waiting on.
//
// The same walk answers the other question this rig kept getting wrong: whether
// enough slots stay FREE for a dig to put its blockers somewhere. A dig on a
// depth-N lane relocates N-1 blockers, and it cannot relocate them into a gated
// lane it is not allowed to enter — so the pool that matters is the UNGATED free
// slots in the same group.

import (
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"shingocore/dispatch"
	"shingocore/plantspec"
)

// ── AND IT IS PER POOL, BECAUSE A CARRIER IS NOT FUNGIBLE ACROSS THE PLANT ──
//
// This check was written plant-wide and passed a plant that then deadlocked on
// carriers in fifteen minutes. lane-stress, 2026-08-10: REQUIRED 22, seeded 24,
// "ok" — and the run wedged with 15 empty carriers parked in SYN_COMP and ZERO
// in SYN_STAMP, while three press stations sat queued on "waiting for an empty
// bin".
//
// The empties were real and available and in the wrong place. A produce station
// draws its empty from ONE named pool — its `inbound_source` — and a consume
// station returns its empty to ONE named pool, its `outbound_destination`. Two
// zones whose claims never name each other are two separate carrier economies
// that happen to share a plant file, and summing them answers a question nobody
// asked. The global total was right and the plant still stopped.
//
// So the balance and the stock floor are computed per pool, and the plant fails
// if ANY pool is short. The plant-wide line stays as context, clearly marked as
// context — it is the number that lied.
type poolPlan struct {
	name         string
	seededEmpty  int
	emptyDemand  float64 // empties consumed per minute (produce swaps drawing here)
	emptySupply  float64 // empties freed per minute (consume swaps returning here)
	producePoint int     // stations drawing from this pool that need one to wait on
	isZone       bool    // false = the pool names something that is not a seeded zone

	// ── WHAT MOVES BETWEEN POOLS, WHICH THE FIRST VERSION HAD NO TERM FOR ────
	//
	// The split above assumed every pool is closed: empties are freed into it by
	// consume swaps and spent out of it by produce swaps, and nothing else. Two
	// shapes in the demo plant break that, and between them they invented the
	// whole 0.45 bins/min deficit this check reported on SYN_MARKET while the
	// plant-wide balance sat at exactly 0.00. Material was conserved; the model
	// had lost track of which pool was holding it.
	keeperIn  float64 // empties arriving from a level keeper or a group overflow
	keeperOut float64 // empties leaving to one
	// isMaintained marks a pool Core holds at a declared level. Its stock is not
	// a function of transit — the keeper sets it — so it is judged against
	// levelWant instead of the in-flight floor.
	isMaintained bool
	levelWant    int
	// isClosedLoop marks a pool that is a dedicated-loader circuit rather than a
	// stocked zone: the same carriers go round it and none enter or leave.
	isClosedLoop bool
}

// carrierPlan is the computed carrier picture for one plant.
type carrierPlan struct {
	seededEmpty  int
	seededFull   int
	totalSlots   int
	slotsUsed    int
	emptyDemand  float64 // empties consumed per minute (produce swaps)
	emptySupply  float64 // empties freed per minute (consume swaps)
	demandBy     map[string]float64
	supplyBy     map[string]float64
	producePoint int // stations that need an empty to swap
	pools        map[string]*poolPlan
	unpooled     []string // payloads whose claims name no pool — cannot be attributed
}

// zoneHeadroom is the shuffle picture for one zone.
type zoneHeadroom struct {
	name        string
	slots       int
	seeded      int
	freeUngated int
	freeGated   int
	deepestDig  int // blockers a dig must relocate (max depth-1)
	deepestLane string
}

func runCarriers(plant *plantspec.Plant, rate map[string]float64, transit, plantPath string, loaderCap, unloaderCap float64) {
	d := 10 * time.Minute
	if transit != "" {
		parsed, err := time.ParseDuration(transit)
		if err != nil || parsed <= 0 {
			fail("bad -transit %q", transit)
		}
		d = parsed
	}
	mins := d.Minutes()

	plan := computeCarriers(plant, rate, loaderCap, unloaderCap)
	zones := computeHeadroom(plant)

	fmt.Printf("\nCARRIER + HEADROOM CHECK — %s (transit %s)\n", plantPath, d)
	fmt.Println(strings.Repeat("═", 84))

	// ── 1. census ────────────────────────────────────────────────────────────
	fmt.Printf("\nCarriers seeded: %d empty, %d full (%d bins into %d slots, %d slots free)\n",
		plan.seededEmpty, plan.seededFull, plan.slotsUsed, plan.totalSlots,
		plan.totalSlots-plan.slotsUsed)

	// Three separate calls then one AND, deliberately: short-circuiting with
	// `reportBalance(plan) && reportStockFloor(...)` would stop printing the
	// moment a section failed, and the sections an operator most needs are the
	// ones after the first failure.
	balanceOK := reportBalance(plan)
	floorOK := reportStockFloor(plan, mins)
	headroomOK := reportHeadroom(zones)
	ok := balanceOK && floorOK && headroomOK

	fmt.Printf("\n%s\n", headline(ok))
	if !ok {
		// Same contract as the fill/starve check: a bad plant fails the command,
		// so this can gate a seed instead of being read and ignored.
		os.Exit(1)
	}
}

// computeCarriers walks the active claims and accumulates the empty-bin sides.
func computeCarriers(plant *plantspec.Plant, rate map[string]float64, loaderCap, unloaderCap float64) carrierPlan {
	p := carrierPlan{
		demandBy: map[string]float64{}, supplyBy: map[string]float64{},
		pools: map[string]*poolPlan{},
	}
	pool := func(name string) *poolPlan {
		q := p.pools[name]
		if q == nil {
			q = &poolPlan{name: name}
			p.pools[name] = q
		}
		return q
	}

	// slot → zone, so a seeded empty can be attributed to the pool it sits in.
	// The pool key IS the zone name, because that is what a claim's
	// inbound_source / outbound_destination names.
	slotZone := map[string]string{}
	isZone := map[string]bool{}
	for _, z := range plant.Zones {
		isZone[z.Name] = true
		for _, l := range z.Lanes {
			for _, s := range l.Slots {
				slotZone[s.Name] = z.Name
			}
			p.totalSlots += len(l.Slots)
		}
		// FLAT POSITIONS COUNT TOO, and they were invisible. A zone can hold its
		// slots directly, with no lane between — that is the shape a MAINTAINED
		// group must be in, because the save-time rules refuse a group with lanes
		// — and demo.yaml's SYN_PRESS_EMPTIES is exactly that: EIGHT POSITIONS,
		// SIX OF THEM SEEDED, two left free (plants/demo.yaml, BIN-PEB-01..06 on
		// PEB_001..006; PEB_007/008 stay open as the spare positions the
		// pre-resolve loop needs). Walking only z.Lanes counted its slot capacity
		// as zero and attributed its seeded empties to no pool, so the tool
		// reported a plant short of the eight slots and six carriers it actually
		// has, and reported the press empties bank as holding nothing.
		for _, s := range z.Positions {
			slotZone[s.Name] = z.Name
		}
		p.totalSlots += len(z.Positions)
	}
	for _, b := range plant.Bins {
		if b.Payload == "" {
			p.seededEmpty++
			if z := slotZone[b.Slot]; z != "" {
				pool(z).seededEmpty++
			}
		} else {
			p.seededFull++
		}
		if slotZone[b.Slot] != "" {
			p.slotsUsed++
		}
	}

	// Which pool each payload's two sides act on. A produce claim spends an empty
	// from its inbound_source; a consume claim frees one into its
	// outbound_destination. Collected per payload because the rates below are per
	// payload.
	//
	// THE RETURN SIDE IS WALKED FIRST because the draw side consults it: whether
	// a loader's inbound_source is a real draw depends on whether something hands
	// its empties straight back to it.
	returnsTo := map[string][]string{} // payload → pools its consume side frees into
	homeGroup := map[string]string{}   // dedicated position → the loader identity it belongs to
	for _, ac := range activeClaims(plant) {
		c := ac.claim
		if c.Role == "consume" && c.OutboundDestination != "" {
			returnsTo[c.Payload] = appendUniq(returnsTo[c.Payload], c.OutboundDestination)
		}
		if c.HomeOf != "" {
			homeGroup[c.CoreNode] = c.HomeOf
		}
	}

	// ── A CLOSED LOOP IS NOT A DRAW ON THE MARKET ────────────────────────────
	//
	// A dedicated-position loader with NO outbound_destination does not push the
	// carriers it fills anywhere. The carrier stands on its home until the line's
	// supply leg comes for it, and the line hands the empty back to that same
	// home — demo.yaml's ALN_008 names PLK_H1 as BOTH its inbound_source and its
	// outbound_destination, which is what that node block exists to exercise. The
	// empty the loader fills next is the empty the cell just returned. Nothing
	// enters the circuit and nothing leaves it.
	//
	// Reading inbound_source literally is what made this wrong. It charged the
	// loop's entire spend to SYN_MARKET while crediting its supply to PLK_H1, so
	// a circuit that conserves every carrier it holds read as a 0.25 bins/min
	// drain on a pool it never touches — and PLK_H1 collected the matching
	// surplus, which then went unjudged because it is not a seeded zone. Half a
	// loop counted twice, in opposite directions, in two different columns.
	//
	// The plant file already said so in words: "inbound_source stays SYN_MARKET
	// as a top-up path if a carrier is lost out of the loop; in steady state it
	// should not be needed." A top-up path is not a steady-state rate.
	//
	// So the draw is charged where the empty actually comes from — the position
	// the consume side returns it to. The produce-station floor moves with it: a
	// closed loop's standing carrier is its own, and charging it to the market
	// asks that pool to keep one spare for a station that never comes for one.
	//
	// GROUP-WIDE, NOT PER POSITION. A return to any position of a dedicated
	// loader closes the circuit for the whole of it, because the buffers are
	// spare parking inside that circuit rather than independent draws.
	closedLoopPool := func(c plantspec.Claim) string {
		if c.HomeOf == "" || c.OutboundDestination != "" {
			return ""
		}
		for _, dest := range returnsTo[c.Payload] {
			if homeGroup[dest] == c.HomeOf {
				return dest
			}
		}
		return ""
	}
	drawPool := func(c plantspec.Claim) string {
		if q := closedLoopPool(c); q != "" {
			return q
		}
		return c.InboundSource
	}

	// Count the swap points that need a carrier to be waiting on, regardless of
	// rate — a station with a slow cadence still occupies one. Charged to the
	// pool it DRAWS FROM, which is the one that has to have it.
	drawsFrom := map[string][]string{} // payload → pools its produce side spends from
	for _, ac := range activeClaims(plant) {
		c := ac.claim
		if c.Role != "produce" {
			continue
		}
		src := drawPool(c)
		if loop := closedLoopPool(c); loop != "" {
			q := pool(loop)
			q.isClosedLoop = true
			// ONE STANDING CARRIER PER CIRCUIT, NOT ONE PER POSITION. The four
			// PLK_* claims are one dedicated loader: a pinned home and three
			// buffers that are spare parking inside the same loop. Charging each
			// of them a carrier to wait on counts the same circuit four times.
			if c.IsActivePull() && q.producePoint == 0 {
				q.producePoint++
			}
			if c.IsActivePull() {
				p.producePoint++
			}
			drawsFrom[c.Payload] = appendUniq(drawsFrom[c.Payload], loop)
			continue
		}
		if c.IsActivePull() {
			p.producePoint++
			if src != "" {
				pool(src).producePoint++
			}
		}
		if src != "" {
			drawsFrom[c.Payload] = appendUniq(drawsFrom[c.Payload], src)
		}
	}

	// ── THE BALANCE IS PER PAYLOAD, NOT PER CLAIM ────────────────────────────
	//
	// An empty is SPENT the moment a bin of some payload is filled, and FREED the
	// moment one is emptied. So the carrier flow for a payload is just its part
	// flow divided by what a bin holds, and the whole balance is the sum over
	// payloads of (bins filled) against (bins emptied).
	//
	// WHICH SIDE SETS THE RATE. A manual_swap loader/unloader has no counter, and
	// its operator cadence (loader_auto_load: 5s → 12 bins/min) is a CAPACITY
	// CEILING, not a demand — the sim sets it high precisely so the operator is
	// never the bottleneck. Charging carriers at that ceiling claims a loader
	// eats twelve bins a minute regardless of whether anything downstream wants
	// them, which is how this check first reported a 40 bins/min deficit on a
	// plant whose real imbalance is a fifth of a bin.
	//
	// In steady state the TICK-DRIVEN side sets the throughput and the manual
	// side matches it: a loader-fed payload turns over as fast as its consumers
	// pull, and an unloader-drained payload as fast as its producers make. So the
	// tick rate is the answer on both sides, and the cadences are used only to
	// flag a manual point that genuinely cannot keep up.
	flows := walkClaims(plant, rate, nil)
	for _, name := range sortedKeys(flows) {
		f := flows[name]
		cap := float64(f.uopCap)
		if cap <= 0 {
			continue
		}
		// Bins FILLED per minute: by tick producers, or by a loader matching the
		// draw when there is no tick producer.
		filled := f.produce / cap
		if filled == 0 && len(f.loaders) > 0 {
			filled = f.consume / cap
			if ceiling := loaderCap * float64(len(f.loaders)); ceiling > 0 && filled > ceiling {
				filled = ceiling // the operator really is the bottleneck
			}
		}
		// Bins EMPTIED per minute: by tick consumers, or by an unloader matching
		// what is made when there is no tick consumer.
		emptied := f.consume / cap
		if emptied == 0 && len(f.unloaders) > 0 {
			emptied = f.produce / cap
			if ceiling := unloaderCap * float64(len(f.unloaders)); ceiling > 0 && emptied > ceiling {
				emptied = ceiling
			}
		}
		p.emptyDemand += filled
		p.emptySupply += emptied
		if filled > 0 {
			p.demandBy[name] = filled
		}
		if emptied > 0 {
			p.supplyBy[name] = emptied
		}

		// ATTRIBUTE BOTH SIDES TO THEIR POOLS. Split evenly when a payload's
		// claims name more than one — every plant so far names exactly one per
		// side, and an even split is the honest default for a shape that has not
		// happened yet. A payload whose claims name NO pool cannot be attributed
		// at all, and is reported rather than dropped.
		if filled > 0 {
			if pools := drawsFrom[name]; len(pools) > 0 {
				each := filled / float64(len(pools))
				for _, q := range pools {
					pool(q).emptyDemand += each
				}
			} else {
				p.unpooled = appendUniq(p.unpooled, name+" (spends)")
			}
		}
		if emptied > 0 {
			if pools := returnsTo[name]; len(pools) > 0 {
				each := emptied / float64(len(pools))
				for _, q := range pools {
					pool(q).emptySupply += each
				}
			} else {
				p.unpooled = appendUniq(p.unpooled, name+" (frees)")
			}
		}
	}
	// ── A MAINTAINED GROUP DOES NOT ACCUMULATE; IT OVERFLOWS ─────────────────
	//
	// A group named in maintained_groups is a LEVEL-CONTROLLED buffer, not a free
	// pool. Core holds it at its declared want: surplus above the level goes out
	// to the overflow zone, and a shortfall is asked for from the plant at large
	// — Maintainer.createAsks deliberately sends its ask with SourceNode "" so
	// the finder's tiers pick the source, because "naming the group here would
	// make the keeper source from the group it is trying to fill". Either way the
	// difference MOVES, and the only pool the config names as its counterparty is
	// the overflow.
	//
	// Modelling the group as closed strands that difference. demo.yaml's
	// SYN_PRESS_EMPTIES takes 0.60 bins/min back from the ASSY unloader and gives
	// 0.40 to the two presses; the 0.20 surplus does not pile up forever in eight
	// positions declared to hold six. It overflows to SYN_MARKET — the same
	// SYN_MARKET this check was reporting a deficit on.
	//
	// The group's own rate is therefore not a verdict. Absorbing that difference
	// is exactly what the keeper is for, so a group in rate deficit is a group
	// doing its job. What CAN go wrong is the pool behind it running out, and
	// that now lands there, where it belongs and where it can be seeded.
	for _, g := range plant.MaintainedGroups {
		q := pool(g.Group)
		q.isMaintained = true
		for _, lv := range g.Levels {
			q.levelWant += lv.Want
		}
		if g.Overflow == "" {
			// No counterparty declared. The keeper still sources plant-wide, but
			// nothing in the file says from where, and putting the number against
			// a pool the config never paired it with is how this check got its
			// last wrong answer.
			continue
		}
		o := pool(g.Overflow)
		switch net := q.emptySupply - q.emptyDemand; {
		case net > 0:
			q.keeperOut += net
			o.keeperIn += net
		case net < 0:
			q.keeperIn += -net
			o.keeperOut += -net
		}
	}

	for name := range p.pools {
		p.pools[name].isZone = isZone[name]
	}
	return p
}

// sortedPools returns the pools in a stable order for reporting.
func sortedPools(m map[string]*poolPlan) []*poolPlan {
	var out []*poolPlan
	for _, q := range m {
		out = append(out, q)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// computeHeadroom measures each zone's free ungated slots against its deepest dig.
func computeHeadroom(plant *plantspec.Plant) []zoneHeadroom {
	seededIn := map[string]int{} // slot name → 1
	for _, b := range plant.Bins {
		seededIn[b.Slot]++
	}
	var out []zoneHeadroom
	for _, z := range plant.Zones {
		zh := zoneHeadroom{name: z.Name}
		// THE ZONE'S OWN WAIT POINTS GATE EVERY LANE IN IT. Read here rather
		// than per lane because that is where the property lives now; a lane's
		// own gate_point is the legacy override and still wins for that lane.
		// Reading only the lane key — which is what this did — scores a
		// group-gated plant as ENTIRELY UNGATED, and simcalc's blindness has
		// already cost one round: its "verdict unchanged" on the PANEL-B
		// unloader was read as evidence when the model had no term for it.
		zoneGated := len(dispatch.ParseWaitPoints(strings.Join(z.WaitPoints, ","))) > 0
		// Flat positions first: a zone that holds its slots directly has no lane
		// to walk, and a maintained group is always that shape. They are never
		// gated (a gate is a lane's admission, and a flat position has no lane
		// to be admitted to) and they can never be dug (there is nothing in
		// front of anything), so they contribute free UNGATED slots and leave
		// deepestDig alone.
		for _, s := range z.Positions {
			zh.slots++
			if seededIn[s.Name] > 0 {
				zh.seeded++
			} else {
				zh.freeUngated++
			}
		}
		for _, l := range z.Lanes {
			gated := l.GatePoint != "" || zoneGated
			free := 0
			for _, s := range l.Slots {
				zh.slots++
				if seededIn[s.Name] > 0 {
					zh.seeded++
				} else {
					free++
				}
			}
			if gated {
				zh.freeGated += free
			} else {
				zh.freeUngated += free
			}
			// The deepest dig this zone can be asked to perform: the blockers in
			// front of the last slot of its deepest lane.
			if blockers := len(l.Slots) - 1; blockers > zh.deepestDig {
				zh.deepestDig = blockers
				zh.deepestLane = l.Name
			}
		}
		out = append(out, zh)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// topContributors renders the largest few contributors to a side.
func topContributors(m map[string]float64) string {
	if len(m) == 0 {
		return "—"
	}
	type kv struct {
		k string
		v float64
	}
	var all []kv
	for k, v := range m {
		all = append(all, kv{k, v})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].v != all[j].v {
			return all[i].v > all[j].v
		}
		return all[i].k < all[j].k
	})
	var parts []string
	for i, e := range all {
		if i == 3 {
			parts = append(parts, fmt.Sprintf("+%d more", len(all)-3))
			break
		}
		parts = append(parts, fmt.Sprintf("%s %.2f", e.k, e.v))
	}
	return strings.Join(parts, ", ")
}

// reportBalance reports the empty-bin balance: whether empties are freed as fast as they are
// consumed, plant-wide.
//
// Returns false when the section found a problem; the caller ANDs the three.
func reportBalance(plan carrierPlan) bool {
	ok := true
	// ── 2. the empty-bin balance ─────────────────────────────────────────────
	fmt.Printf("\nEMPTY-BIN BALANCE (an empty is made by a consume swap, spent by a produce swap)\n")
	fmt.Printf("%-26s %-12s %s\n", "SIDE", "BINS/min", "FROM")
	fmt.Println(strings.Repeat("─", 84))
	fmt.Printf("%-26s %-12.2f %s\n", "freed (bins emptied)", plan.emptySupply, topContributors(plan.supplyBy))
	fmt.Printf("%-26s %-12.2f %s\n", "spent (bins filled)", plan.emptyDemand, topContributors(plan.demandBy))

	net := plan.emptySupply - plan.emptyDemand
	switch {
	case plan.emptyDemand == 0 && plan.emptySupply == 0:
		fmt.Printf("\n  no tick-driven swap points — nothing to balance\n")
	case net < -0.001:
		ok = false
		fmt.Printf("\n  DEFICIT %.2f bins/min: the carrier pool drains no matter how large it starts.\n", -net)
		fmt.Printf("  Every empty is spent %.0f%% faster than one is freed, so the plant deadlocks\n",
			100*(plan.emptyDemand/math.Max(plan.emptySupply, 0.0001)-1))
		fmt.Printf("  once the seeded pool is gone. Fix the RATES, not the bin count.\n")
	default:
		fmt.Printf("\n  balanced (+%.2f bins/min headroom)\n", net)
	}

	return ok
}

// reportStockFloor reports the stock floor per pool -- the seeded empties each pool needs to
// cover one transit window.
//
// Returns false when the section found a problem; the caller ANDs the three.
func reportStockFloor(plan carrierPlan, mins float64) bool {
	ok := true
	// ── 3. the stock floor, PER POOL ─────────────────────────────────────────
	//
	// The plant-wide figure is printed first and labelled as what it is: context,
	// and the number that passed lane-stress while SYN_STAMP sat at zero empties.
	// The VERDICT is per pool.
	inFlight := plan.emptyDemand * mins
	floor := int(math.Ceil(inFlight)) + plan.producePoint
	fmt.Printf("\nEMPTY-BIN STOCK FLOOR\n")
	fmt.Println(strings.Repeat("─", 84))
	fmt.Printf("  plant-wide (CONTEXT ONLY — a carrier is not fungible across pools):\n")
	fmt.Printf("    %-44s %.2f × %.0fm = %.1f\n", "in flight (demand × transit)", plan.emptyDemand, mins, inFlight)
	fmt.Printf("    %-44s %d\n", "one per produce station", plan.producePoint)
	fmt.Printf("    %-44s %d, seeded %d\n", "REQUIRED ≥", floor, plan.seededEmpty)

	fmt.Printf("\n  PER POOL — a produce station draws its empty from ONE named pool\n")
	fmt.Printf("  %-18s %-8s %-8s %-8s %-9s %-8s %s\n",
		"POOL", "SPENDS", "FREES", "KEEPER", "REQUIRED", "SEEDED", "VERDICT")
	fmt.Println(strings.Repeat("─", 84))
	for _, q := range sortedPools(plan.pools) {
		// SPENDS and FREES are what the swap points do; KEEPER is what crosses the
		// pool boundary on top of that — a maintained group's overflow, or the
		// keeper's ask coming the other way. The balance is judged on the sum,
		// because a pool that gives away its surplus is not in surplus.
		spends := q.emptyDemand + q.keeperOut
		frees := q.emptySupply + q.keeperIn
		keeper := q.keeperIn - q.keeperOut

		pInFlight := spends * mins
		pFloor := int(math.Ceil(pInFlight)) + q.producePoint
		verdict := "ok"
		switch {
		case q.isClosedLoop:
			// A closed circuit's carrier count is fixed by what was seeded into
			// it, and its positions are nodes rather than zone slots, so there is
			// no seeded-empty figure to judge and no floor that means anything.
			// Printing demand x transit here put a REQUIRED 7 beside a SEEDED 0
			// for a loop that holds three carriers and needs no more — a number
			// with no referent, next to a number that was never counted.
			verdict = "closed circuit — carriers conserved, no floor applies"
			pFloor = 0
		case q.isMaintained:
			// A LEVEL IS NOT A FLOOR. Core holds this pool at its declared want,
			// so what it needs seeded is that want — not demand x transit, which
			// describes a pool nobody is topping up. Judging a kept group on the
			// transit floor asks it to carry stock the keeper exists to deliver.
			pFloor = q.levelWant
			if q.isZone && q.seededEmpty < pFloor {
				ok = false
				verdict = fmt.Sprintf("BELOW LEVEL by %d — the keeper starts behind", pFloor-q.seededEmpty)
			} else {
				verdict = fmt.Sprintf("level-kept (want %d)", q.levelWant)
			}
		case !q.isZone:
			// The pool names something that is not a seeded zone (a concrete node,
			// a dedicated home). We cannot count its stock, so we do not judge it —
			// and we say so rather than scoring it 0 and crying wolf.
			verdict = "not a seeded zone — stock not judged"
		case frees-spends < -0.001:
			ok = false
			verdict = fmt.Sprintf("RATE DEFICIT %.2f/min — drains regardless of size", spends-frees)
		case q.seededEmpty < pFloor:
			ok = false
			verdict = fmt.Sprintf("SHORT BY %d", pFloor-q.seededEmpty)
		}
		keeperCol := "—"
		if math.Abs(keeper) > 0.001 {
			keeperCol = fmt.Sprintf("%+.2f", keeper)
		}
		floorCol, seededCol := fmt.Sprintf("%d", pFloor), fmt.Sprintf("%d", q.seededEmpty)
		if q.isClosedLoop {
			floorCol, seededCol = "—", "—"
		}
		fmt.Printf("  %-18s %-8.2f %-8.2f %-8s %-9s %-8s %s\n",
			q.name, spends, frees, keeperCol, floorCol, seededCol, verdict)
	}
	if len(plan.unpooled) > 0 {
		fmt.Printf("\n  NOT ATTRIBUTED: %s — these payloads' claims name no inbound_source /\n",
			strings.Join(plan.unpooled, ", "))
		fmt.Printf("  outbound_destination, so their carriers belong to no pool this can check.\n")
	}
	fmt.Printf("\n  KEEPER is carriers crossing a pool boundary outside the swap points: a\n")
	fmt.Printf("  maintained group sends its surplus to the overflow zone and asks the plant\n")
	fmt.Printf("  for its shortfall, so the two pools settle against each other rather than\n")
	fmt.Printf("  one draining while the other piles up. Without this term SYN_PRESS_EMPTIES\n")
	fmt.Printf("  read +0.20/min forever and SYN_MARKET wore the matching deficit.\n")
	fmt.Printf("\n  A pool short here deadlocks EVEN IF the plant total is comfortable: the\n")
	fmt.Printf("  empties exist, in the wrong zone, and nothing routes them back. That is how\n")
	fmt.Printf("  lane-stress passed at 24/22 and wedged with 15 empties in SYN_COMP and 0 in\n")
	fmt.Printf("  SYN_STAMP, three presses queued on \"waiting for an empty bin\".\n")

	return ok
}

// reportHeadroom reports shuffle headroom: whether each zone has enough free ungated slots for
// its deepest dig.
//
// Returns false when the section found a problem; the caller ANDs the three.
func reportHeadroom(zones []zoneHeadroom) bool {
	ok := true
	// ── 4. shuffle headroom ──────────────────────────────────────────────────
	//
	// A GATED LANE IS SHUFFLE SPACE. IT USED NOT TO BE, AND THIS CHECK WAS THE
	// LAST PLACE STILL SAYING SO.
	//
	// This counted only slots in UNMARKED lanes, on the reasoning that "a dig
	// cannot park a blocker in a gated lane it is not allowed to enter". That
	// rule was real in the dispatcher and it was DELETED on 2026-08-31:
	// shuffleSlotsFrom now opens with "A GATED DIG MAY PARK ITS BLOCKER IN
	// ANOTHER GATED LANE. IT USED NOT TO." The refusal that forced the exclusion
	// — spliceLaneWait allowing only one gated lane per plan — stopped existing
	// when rule 2 became "a wait per gated lane the plan enters".
	//
	// The measurement that settled it is the same shape as the failure here:
	// with every lane marked, "park in an ungated lane" names no slot in the
	// plant, so every dig held. Six stuck from the first minute of the run.
	//
	// And demo.yaml is that plant. SYN_MARKET carries fifteen wait_points and
	// SYN_CLEAR one, so EVERY lane in this fixture is marked and FREE-UNG was
	// structurally zero — the check reported SYN_MARKET "SHORT — Lane_01 needs 2,
	// has 0" while thirteen free slots sat in it, ready, and the dispatcher was
	// perfectly willing to use them. A static checker enforcing a constraint the
	// runtime dropped is worse than no checker: it fails a healthy plant, and a
	// gate that cries wolf is a gate people learn to skip.
	//
	// THE SPLIT STAYS IN THE TABLE, because the objection the exclusion was
	// protecting is explicitly still unmeasured: a dig holds its lane
	// exclusively, so a leg dwelling at a second lane's mark keeps the dug
	// corridor shut while that lane is congested. Lawful and self-clearing, but
	// not known to be BOUNDED. Showing which half of the headroom is gated keeps
	// that cost visible without failing a plant for it.
	fmt.Printf("\nSHUFFLE HEADROOM (a dig on a depth-N lane relocates N-1 blockers)\n")
	fmt.Printf("%-18s %-7s %-7s %-9s %-8s %-8s %s\n",
		"ZONE", "SLOTS", "SEEDED", "FREE-UNG", "FREE-GT", "DEEPEST", "VERDICT")
	fmt.Println(strings.Repeat("─", 84))
	for _, z := range zones {
		free := z.freeUngated + z.freeGated
		verdict := "ok"
		switch {
		case z.deepestDig > free:
			ok = false
			verdict = fmt.Sprintf("SHORT — %s needs %d, has %d", z.deepestLane, z.deepestDig, free)
		case z.deepestDig > z.freeUngated:
			// It fits, but only by using marked lanes — which is lawful and is
			// the case whose dwell cost has never been measured.
			verdict = "ok — leans on gated space (dwell cost unmeasured)"
		}
		fmt.Printf("%-18s %-7d %-7d %-9d %-8d %-8d %s\n",
			z.name, z.slots, z.seeded, z.freeUngated, z.freeGated, z.deepestDig, verdict)
	}
	fmt.Printf("\n  BOTH columns count toward a dig: shuffleSlotsFrom lets a gated dig park its\n")
	fmt.Printf("  blocker in another gated lane, each leg waiting at its own lane's mark. The\n")
	fmt.Printf("  split is kept because a second mark holds the dug corridor shut for as long\n")
	fmt.Printf("  as that lane is congested, and that duration is not known to be bounded.\n")
	return ok
}
