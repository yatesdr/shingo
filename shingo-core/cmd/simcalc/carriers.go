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

	// ── 2. the empty-bin balance ─────────────────────────────────────────────
	fmt.Printf("\nEMPTY-BIN BALANCE (an empty is made by a consume swap, spent by a produce swap)\n")
	fmt.Printf("%-26s %-12s %s\n", "SIDE", "BINS/min", "FROM")
	fmt.Println(strings.Repeat("─", 84))
	fmt.Printf("%-26s %-12.2f %s\n", "freed (bins emptied)", plan.emptySupply, topContributors(plan.supplyBy))
	fmt.Printf("%-26s %-12.2f %s\n", "spent (bins filled)", plan.emptyDemand, topContributors(plan.demandBy))

	net := plan.emptySupply - plan.emptyDemand
	ok := true
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
	fmt.Printf("  %-14s %-9s %-9s %-9s %-9s %s\n", "POOL", "SPENDS", "FREES", "REQUIRED", "SEEDED", "VERDICT")
	fmt.Println(strings.Repeat("─", 84))
	for _, q := range sortedPools(plan.pools) {
		pInFlight := q.emptyDemand * mins
		pFloor := int(math.Ceil(pInFlight)) + q.producePoint
		verdict := "ok"
		switch {
		case !q.isZone:
			// The pool names something that is not a seeded zone (a concrete node,
			// a dedicated home). We cannot count its stock, so we do not judge it —
			// and we say so rather than scoring it 0 and crying wolf.
			verdict = "not a seeded zone — stock not judged"
		case q.emptySupply-q.emptyDemand < -0.001:
			ok = false
			verdict = fmt.Sprintf("RATE DEFICIT %.2f/min — drains regardless of size",
				q.emptyDemand-q.emptySupply)
		case q.seededEmpty < pFloor:
			ok = false
			verdict = fmt.Sprintf("SHORT BY %d", pFloor-q.seededEmpty)
		}
		fmt.Printf("  %-14s %-9.2f %-9.2f %-9d %-9d %s\n",
			q.name, q.emptyDemand, q.emptySupply, pFloor, q.seededEmpty, verdict)
	}
	if len(plan.unpooled) > 0 {
		fmt.Printf("\n  NOT ATTRIBUTED: %s — these payloads' claims name no inbound_source /\n",
			strings.Join(plan.unpooled, ", "))
		fmt.Printf("  outbound_destination, so their carriers belong to no pool this can check.\n")
	}
	fmt.Printf("\n  A pool short here deadlocks EVEN IF the plant total is comfortable: the\n")
	fmt.Printf("  empties exist, in the wrong zone, and nothing routes them back. That is how\n")
	fmt.Printf("  lane-stress passed at 24/22 and wedged with 15 empties in SYN_COMP and 0 in\n")
	fmt.Printf("  SYN_STAMP, three presses queued on \"waiting for an empty bin\".\n")

	// ── 4. shuffle headroom ──────────────────────────────────────────────────
	fmt.Printf("\nSHUFFLE HEADROOM (a dig on a depth-N lane relocates N-1 blockers)\n")
	fmt.Printf("%-14s %-7s %-8s %-9s %-9s %s\n", "ZONE", "SLOTS", "SEEDED", "FREE-UNG", "DEEPEST", "VERDICT")
	fmt.Println(strings.Repeat("─", 84))
	for _, z := range zones {
		verdict := "ok"
		if z.deepestDig > z.freeUngated {
			ok = false
			verdict = fmt.Sprintf("SHORT — %s needs %d, has %d", z.deepestLane, z.deepestDig, z.freeUngated)
		}
		fmt.Printf("%-14s %-7d %-8d %-9d %-9d %s\n",
			z.name, z.slots, z.seeded, z.freeUngated, z.deepestDig, verdict)
	}
	fmt.Printf("\n  FREE-UNG counts only slots in UNMARKED lanes: a dig cannot park a blocker in\n")
	fmt.Printf("  a gated lane it is not allowed to enter, so gated free space does not count.\n")

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

	// Count the swap points that need a carrier to be waiting on, regardless of
	// rate — a station with a slow cadence still occupies one. Charged to the
	// pool it DRAWS FROM, which is the one that has to have it.
	for _, ac := range activeClaims(plant) {
		if ac.claim.Role == "produce" && ac.claim.IsActivePull() {
			p.producePoint++
			if src := ac.claim.InboundSource; src != "" {
				pool(src).producePoint++
			}
		}
	}
	// Which pool each payload's two sides act on. A produce claim spends an empty
	// from its inbound_source; a consume claim frees one into its
	// outbound_destination. Collected per payload because the rates below are per
	// payload.
	drawsFrom := map[string][]string{} // payload → pools its produce side spends from
	returnsTo := map[string][]string{} // payload → pools its consume side frees into
	for _, ac := range activeClaims(plant) {
		c := ac.claim
		switch c.Role {
		case "produce":
			if c.InboundSource != "" {
				drawsFrom[c.Payload] = appendUniq(drawsFrom[c.Payload], c.InboundSource)
			}
		case "consume":
			if c.OutboundDestination != "" {
				returnsTo[c.Payload] = appendUniq(returnsTo[c.Payload], c.OutboundDestination)
			}
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
		for _, l := range z.Lanes {
			gated := l.GatePoint != ""
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
