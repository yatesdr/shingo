package main

import (
	"fmt"
	"math"
	"testing"

	"shingocore/plantspec"
)

// carrierPlant builds a two-station loop: a press fills PANEL bins, a weld cell
// empties them. One payload, one producer, one consumer — the smallest thing
// that can have a carrier balance at all.
func carrierPlant(pressCap, weldCap int64) *plantspec.Plant {
	return &plantspec.Plant{
		Payloads: []plantspec.Payload{{Code: "PANEL", UOPCapacity: 30}},
		Processes: []plantspec.Process{
			{Name: "PRESS", ActiveStyle: "PRESS-RUN"},
			{Name: "WELD", ActiveStyle: "WELD-RUN"},
		},
		Styles: []plantspec.Style{
			{Name: "PRESS-RUN", Process: "PRESS", Payload: "PANEL"},
			{Name: "WELD-RUN", Process: "WELD", Payload: "PANEL"},
		},
		Claims: []plantspec.Claim{
			{CoreNode: "PLN_1", Style: "PRESS-RUN", Role: "produce", SwapMode: "single_robot", Payload: "PANEL", UOPCapacity: pressCap},
			{CoreNode: "ALN_1", Style: "WELD-RUN", Role: "consume", SwapMode: "single_robot", Payload: "PANEL", UOPCapacity: weldCap},
		},
	}
}

// twoPoolPlant builds TWO independent carrier economies that share a plant file:
// a panel loop whose stations name SYN_STAMP, and a component loop whose stations
// name SYN_COMP. Each zone has one lane of seeded empties.
//
// This is lane-stress in miniature, and the shape the plant-wide check could not
// see: no station in one loop ever names the other's pool, so an empty parked in
// SYN_COMP can never serve the press drawing from SYN_STAMP.
func twoPoolPlant(stampEmpties, compEmpties int) *plantspec.Plant {
	p := &plantspec.Plant{
		Payloads: []plantspec.Payload{
			{Code: "PANEL", UOPCapacity: 30},
			{Code: "COMP", UOPCapacity: 30},
		},
		Processes: []plantspec.Process{
			{Name: "PRESS", ActiveStyle: "PRESS-RUN"},
			{Name: "WELD", ActiveStyle: "WELD-RUN"},
			{Name: "CPRESS", ActiveStyle: "CPRESS-RUN"},
			{Name: "CWELD", ActiveStyle: "CWELD-RUN"},
		},
		Styles: []plantspec.Style{
			{Name: "PRESS-RUN", Process: "PRESS", Payload: "PANEL"},
			{Name: "WELD-RUN", Process: "WELD", Payload: "PANEL"},
			{Name: "CPRESS-RUN", Process: "CPRESS", Payload: "COMP"},
			{Name: "CWELD-RUN", Process: "CWELD", Payload: "COMP"},
		},
		Claims: []plantspec.Claim{
			{CoreNode: "PLN_1", Style: "PRESS-RUN", Role: "produce", SwapMode: "single_robot",
				Payload: "PANEL", UOPCapacity: 30, InboundSource: "SYN_STAMP", OutboundDestination: "SYN_STAMP"},
			{CoreNode: "ALN_1", Style: "WELD-RUN", Role: "consume", SwapMode: "single_robot",
				Payload: "PANEL", UOPCapacity: 30, InboundSource: "SYN_STAMP", OutboundDestination: "SYN_STAMP"},
			{CoreNode: "PLN_2", Style: "CPRESS-RUN", Role: "produce", SwapMode: "single_robot",
				Payload: "COMP", UOPCapacity: 30, InboundSource: "SYN_COMP", OutboundDestination: "SYN_COMP"},
			{CoreNode: "ALN_2", Style: "CWELD-RUN", Role: "consume", SwapMode: "single_robot",
				Payload: "COMP", UOPCapacity: 30, InboundSource: "SYN_COMP", OutboundDestination: "SYN_COMP"},
		},
	}
	mk := func(zone, prefix string, n int) plantspec.Zone {
		var slots []plantspec.Slot
		for i := 1; i <= 8; i++ {
			slots = append(slots, plantspec.Slot{Name: fmt.Sprintf("%s_%d", prefix, i), Depth: i})
		}
		z := plantspec.Zone{Name: zone, Lanes: []plantspec.Lane{{Name: prefix + "_L1", Slots: slots}}}
		for i := 1; i <= n; i++ {
			p.Bins = append(p.Bins, plantspec.Bin{Name: fmt.Sprintf("MT-%s-%d", prefix, i),
				Slot: fmt.Sprintf("%s_%d", prefix, i)})
		}
		return z
	}
	p.Zones = []plantspec.Zone{mk("SYN_STAMP", "STMP", stampEmpties), mk("SYN_COMP", "COMP", compEmpties)}
	return p
}

// TestCarriers_APoolCanStarveWhileThePlantTotalLooksFine is why the check is per
// pool, and it is the lane-stress wedge of 2026-08-10 reduced to its bones.
//
// Both loops need the same number of carriers. All of them are seeded into ONE
// zone. Plant-wide the total is generous; the other zone has nothing, and the
// press drawing from it cannot swap — an empty in SYN_COMP is not reachable by a
// station whose inbound_source is SYN_STAMP.
//
// On the real rig this read as "REQUIRED 22, seeded 24, ok" while the run wedged
// with 15 empties in SYN_COMP, ZERO in SYN_STAMP, and three presses queued on
// "waiting for an empty bin". The global number was arithmetically correct and
// answered a question nobody had asked.
func TestCarriers_APoolCanStarveWhileThePlantTotalLooksFine(t *testing.T) {
	t.Parallel()
	// Every carrier in SYN_COMP, none in SYN_STAMP.
	plant := twoPoolPlant(0, 8)
	rate := map[string]float64{"PRESS": 6.0, "WELD": 6.0, "CPRESS": 6.0, "CWELD": 6.0}

	p := computeCarriers(plant, rate, 0, 0)

	if p.seededEmpty != 8 {
		t.Fatalf("fixture: %d seeded empties, want 8", p.seededEmpty)
	}
	// The plant-wide floor this generous total would have satisfied.
	if p.emptySupply != p.emptyDemand {
		t.Fatalf("fixture: the plant is meant to be RATE-balanced, got demand %.3f supply %.3f",
			p.emptyDemand, p.emptySupply)
	}

	stamp, comp := p.pools["SYN_STAMP"], p.pools["SYN_COMP"]
	if stamp == nil || comp == nil {
		t.Fatalf("pools not attributed: %v", p.pools)
	}
	if stamp.seededEmpty != 0 {
		t.Errorf("SYN_STAMP seeded %d empties, want 0 — the fixture puts them all in the other zone",
			stamp.seededEmpty)
	}
	if comp.seededEmpty != 8 {
		t.Errorf("SYN_COMP seeded %d empties, want 8", comp.seededEmpty)
	}
	// The starved pool must still show real DEMAND — that is what makes it a
	// deadlock rather than an idle zone.
	if stamp.emptyDemand <= 0 {
		t.Errorf("SYN_STAMP demand is %.3f: a pool with no demand cannot starve, so this fixture "+
			"would not be reproducing anything", stamp.emptyDemand)
	}
	if stamp.producePoint != 1 {
		t.Errorf("SYN_STAMP producePoint = %d, want 1 — the press draws from this pool and must be "+
			"charged to it", stamp.producePoint)
	}
	// The floor the runCarriers verdict uses, computed the same way.
	floor := int(math.Ceil(stamp.emptyDemand*10)) + stamp.producePoint
	if stamp.seededEmpty >= floor {
		t.Errorf("SYN_STAMP: seeded %d against floor %d — the per-pool check would PASS a zone with "+
			"no carriers at all, which is the defect this test exists for", stamp.seededEmpty, floor)
	}
}

// TestCarriers_PoolsAreKeyedOnTheSourceNotTheZoneOfTheBin pins the attribution
// rule, because it is the half that is easy to get backwards: a produce station's
// demand belongs to the pool it DRAWS FROM (inbound_source), and a consume
// station's supply to the pool it RETURNS TO (outbound_destination). Keying
// either on the station's own location would put both loops' flows in whichever
// zone the machine happens to sit in.
func TestCarriers_PoolsAreKeyedOnTheSourceNotTheZoneOfTheBin(t *testing.T) {
	t.Parallel()
	plant := twoPoolPlant(4, 4)
	rate := map[string]float64{"PRESS": 6.0, "WELD": 6.0, "CPRESS": 6.0, "CWELD": 6.0}

	p := computeCarriers(plant, rate, 0, 0)
	for _, name := range []string{"SYN_STAMP", "SYN_COMP"} {
		q := p.pools[name]
		if q == nil {
			t.Fatalf("%s missing from pools", name)
		}
		if q.emptyDemand <= 0 || q.emptySupply <= 0 {
			t.Errorf("%s: demand %.3f supply %.3f — each loop names its own pool on both sides, so "+
				"both must carry flow", name, q.emptyDemand, q.emptySupply)
		}
		if !q.isZone {
			t.Errorf("%s should resolve to a seeded zone", name)
		}
	}
	// And the two pools must not be sharing: each loop is 0.2 bins/min.
	if got := p.pools["SYN_STAMP"].emptyDemand; got > 0.21 {
		t.Errorf("SYN_STAMP demand %.3f — it has absorbed the other loop's flow too; the pools are "+
			"not being kept apart", got)
	}
}

// TestCarriers_BalancedLoopHasNoDrain — when a payload is made and drained at the
// same rate, its carriers turn over with no net loss. This is the case a healthy
// plant must land in, and the baseline the deficit case is measured against.
func TestCarriers_BalancedLoopHasNoDrain(t *testing.T) {
	t.Parallel()
	plant := carrierPlant(30, 30)
	rate := map[string]float64{"PRESS": 6.0, "WELD": 6.0}

	p := computeCarriers(plant, rate, 0, 0)
	if p.emptyDemand != p.emptySupply {
		t.Errorf("balanced loop reported demand %.3f vs supply %.3f — a plant that makes and "+
			"drains at the same rate cannot be losing carriers", p.emptyDemand, p.emptySupply)
	}
	if p.emptyDemand == 0 {
		t.Fatal("no carrier flow computed at all")
	}
	if p.producePoint != 1 {
		t.Errorf("producePoint = %d, want 1 (the press is the only station needing an empty)", p.producePoint)
	}
}

// TestCarriers_SlowerConsumerDrainsThePool is the deadlock, in miniature.
//
// The press keeps filling bins while the cell empties them more slowly, so
// empties are spent faster than they are freed and the pool goes to zero no
// matter how many it started with. Adding bins cannot fix this shape — the
// message says so, because the first instinct is always to add bins.
func TestCarriers_SlowerConsumerDrainsThePool(t *testing.T) {
	t.Parallel()
	plant := carrierPlant(30, 30)
	rate := map[string]float64{"PRESS": 6.0, "WELD": 3.0} // cell runs at half the press

	p := computeCarriers(plant, rate, 0, 0)
	if p.emptyDemand <= p.emptySupply {
		t.Fatalf("a producer outrunning its consumer reported no drain (demand %.3f, supply %.3f)",
			p.emptyDemand, p.emptySupply)
	}
}

// TestCarriers_ManualPointIsNotChargedAtItsCeiling pins the correction that
// mattered most.
//
// A manual_swap loader's operator cadence is a CAPACITY CEILING — the sim sets
// it deliberately high so the operator is never the bottleneck. Charging
// carriers at that ceiling claims the loader eats bins whether or not anything
// downstream wants them, which reported a 40 bins/min deficit on a plant whose
// real imbalance was a fifth of a bin. In steady state the loader matches the
// draw, so the tick side sets the rate.
func TestCarriers_ManualPointIsNotChargedAtItsCeiling(t *testing.T) {
	t.Parallel()
	plant := &plantspec.Plant{
		Payloads:  []plantspec.Payload{{Code: "CLIP", UOPCapacity: 40}},
		Processes: []plantspec.Process{{Name: "WELD", ActiveStyle: "WELD-RUN"}, {Name: "LOADER", ActiveStyle: "LOADER-RUN"}},
		Styles: []plantspec.Style{
			{Name: "WELD-RUN", Process: "WELD", Payload: "CLIP"},
			{Name: "LOADER-RUN", Process: "LOADER", Payload: "CLIP"},
		},
		Claims: []plantspec.Claim{
			{CoreNode: "ALN_1", Style: "WELD-RUN", Role: "consume", SwapMode: "single_robot", Payload: "CLIP", UOPCapacity: 40},
			{CoreNode: "PLK_1", Style: "LOADER-RUN", Role: "produce", SwapMode: "manual_swap", Payload: "CLIP", UOPCapacity: 40},
		},
	}
	rate := map[string]float64{"WELD": 4.0} // the loader has no tick

	// 12 bins/min ceiling — 120× the real draw of 4/40 = 0.1 bins/min.
	p := computeCarriers(plant, rate, 12.0, 12.0)

	if p.emptyDemand > 0.2 {
		t.Errorf("the manual loader was charged %.2f bins/min. It should match the downstream "+
			"draw (0.10), not its operator ceiling — that mistake turns a healthy plant into a "+
			"reported deficit and sends the reader off fixing rates that are fine", p.emptyDemand)
	}
	if p.emptyDemand != p.emptySupply {
		t.Errorf("loader-fed loop is unbalanced: demand %.3f vs supply %.3f — a loader that keeps "+
			"up with its consumer neither gains nor loses carriers", p.emptyDemand, p.emptySupply)
	}
}

// TestHeadroom_GatedFreeSlotsDoNotCount — a dig cannot park a blocker in a gated
// lane it is not allowed to enter, so gated free space is not headroom. Counting
// it is how a plant reads as roomy and then waits constantly.
// TestHeadroom_GatedFreeSlotsDoCount pins the rule the DISPATCHER now applies,
// which is the opposite of what this test used to assert.
//
// It was TestHeadroom_GatedFreeSlotsDoNotCount, and it was right when it was
// written: shuffleSlotsFrom excluded gated lanes from the shuffle pool. That
// exclusion was DELETED on 2026-08-31 — the function now opens "A GATED DIG MAY
// PARK ITS BLOCKER IN ANOTHER GATED LANE. IT USED NOT TO." — because with every
// lane in demo.yaml marked, "park in an ungated lane" named no slot in the plant
// and six digs held from the first minute of the run.
//
// The test kept passing the whole time, which is the point worth keeping: it was
// pinning simcalc against simcalc, not against the dispatcher, so nothing went
// red when the behaviour it described stopped being true. What it cost was a
// permanent false SHORT on the only fixture anyone runs.
func TestHeadroom_GatedFreeSlotsDoCount(t *testing.T) {
	t.Parallel()
	plant := &plantspec.Plant{
		Zones: []plantspec.Zone{{
			Name: "Z",
			Lanes: []plantspec.Lane{
				// Deep lane, full: a four-blocker dig.
				{Name: "DEEP", Slots: []plantspec.Slot{
					{Name: "S1", Depth: 1}, {Name: "S2", Depth: 2},
					{Name: "S3", Depth: 3}, {Name: "S4", Depth: 4}, {Name: "S5", Depth: 5},
				}},
				// Empty but GATED — invisible to that dig.
				{Name: "GATED", GatePoint: "MARK", Slots: []plantspec.Slot{
					{Name: "G1", Depth: 1}, {Name: "G2", Depth: 2}, {Name: "G3", Depth: 3},
					{Name: "G4", Depth: 4}, {Name: "G5", Depth: 5},
				}},
			},
		}},
		Bins: []plantspec.Bin{
			{Name: "b1", Slot: "S1", Payload: "P"}, {Name: "b2", Slot: "S2", Payload: "P"},
			{Name: "b3", Slot: "S3", Payload: "P"}, {Name: "b4", Slot: "S4", Payload: "P"},
			{Name: "b5", Slot: "S5", Payload: "P"},
		},
	}

	zones := computeHeadroom(plant)
	if len(zones) != 1 {
		t.Fatalf("got %d zones, want 1", len(zones))
	}
	z := zones[0]
	if z.freeUngated != 0 {
		t.Errorf("freeUngated = %d, want 0 — the only free slots are in a gated lane", z.freeUngated)
	}
	if z.freeGated != 5 {
		t.Errorf("freeGated = %d, want 5", z.freeGated)
	}
	if z.deepestDig != 4 {
		t.Errorf("deepestDig = %d, want 4 (a depth-5 lane has four blockers)", z.deepestDig)
	}
	// THE SPLIT IS STILL MEASURED — a second mark holds the dug corridor shut
	// while that lane is congested, and that duration has never been bounded — so
	// the columns stay separate even though both now count.
	if z.deepestDig <= z.freeUngated {
		t.Fatal("fixture: the free slots must all be GATED for this to test anything")
	}

	// AND THE VERDICT ITSELF IS ASSERTED, not re-derived here. The version of
	// this test that computed `freeUngated + freeGated` and checked its own sum
	// went on passing when reportHeadroom was mutated back to the old rule —
	// which is the same way the original test survived the dispatcher change it
	// was supposed to be describing. A checker's test has to call the checker.
	if !reportHeadroom(zones) {
		t.Error("reportHeadroom failed a zone whose only free space is GATED. shuffleSlotsFrom " +
			"parks blockers in gated lanes now, so this plant has room — and calling it short " +
			"fails every fixture in this repo, all of which are marked")
	}
}

// TestFlatPositionsAreCountedAsSlots pins the shape a MAINTAINED group is
// always in: a zone that holds its slots DIRECTLY, with no lane between.
//
// THE TOOL WALKED z.Lanes AND NOTHING ELSE, so such a zone contributed zero
// slots and its seeded carriers were attributed to no pool. On demo.yaml that
// made SYN_PRESS_EMPTIES — eight positions, six of them seeded — report as a
// zone with no capacity holding nothing, and the per-pool check said "SHORT BY
// 6" about a bank that was exactly at its level. A tool that raises a false
// alarm on the healthiest pool in the plant is worse than one that says nothing:
// the next real shortage reads as more of the same noise.
//
// The save-time rules REFUSE a maintained group with lanes, so this is not an
// exotic shape the tool could reasonably not know about — it is the only shape
// a maintained group can have.
func TestFlatPositionsAreCountedAsSlots(t *testing.T) {
	t.Parallel()
	p := carrierPlant(30, 30)
	p.Zones = []plantspec.Zone{{
		Name: "FLATBANK",
		Positions: []plantspec.Slot{
			{Name: "P1", Depth: 1}, {Name: "P2", Depth: 1},
			{Name: "P3", Depth: 1}, {Name: "P4", Depth: 1},
		},
	}}
	p.Bins = []plantspec.Bin{
		{Name: "B1", Slot: "P1"}, // empty carrier, in the flat bank
		{Name: "B2", Slot: "P2"},
	}

	plan := computeCarriers(p, map[string]float64{}, 0, 0)
	if plan.totalSlots != 4 {
		t.Errorf("totalSlots = %d, want 4. A zone that holds its positions directly has no "+
			"lane to walk, and walking only z.Lanes counts its whole capacity as zero",
			plan.totalSlots)
	}
	if plan.slotsUsed != 2 {
		t.Errorf("slotsUsed = %d, want 2 — the two seeded carriers stand in real slots",
			plan.slotsUsed)
	}
	pool := plan.pools["FLATBANK"]
	if pool == nil {
		t.Fatalf("no pool for FLATBANK: a seeded empty in a flat zone was attributed to no " +
			"pool at all, so the per-pool stock check judged the bank as holding nothing")
	}
	if pool.seededEmpty != 2 {
		t.Errorf("FLATBANK seededEmpty = %d, want 2", pool.seededEmpty)
	}

	hs := computeHeadroom(p)
	if len(hs) != 1 {
		t.Fatalf("computeHeadroom returned %d zones, want 1", len(hs))
	}
	zh := hs[0]
	if zh.slots != 4 || zh.seeded != 2 || zh.freeUngated != 2 {
		t.Errorf("headroom = {slots:%d seeded:%d freeUngated:%d}, want {4 2 2}",
			zh.slots, zh.seeded, zh.freeUngated)
	}
	if zh.deepestDig != 0 {
		t.Errorf("deepestDig = %d, want 0. Flat positions have nothing in front of anything, "+
			"so no dig can be raised against them and they must not inflate the depth a zone "+
			"needs shuffle room for", zh.deepestDig)
	}
}

// dedicatedLoopPlant builds the SHIM shape from demo.yaml: a dedicated-position
// loader whose home is refilled by the very cell it feeds.
//
//	LOADER (produce, home_of=G, NO outbound) fills a carrier on H1
//	CELL   (consume, in=H1, out=H1)          takes it and hands the empty back
//
// MARKET is named as the loader's inbound_source and is the top-up path only —
// the plant file says so: "in steady state it should not be needed".
func dedicatedLoopPlant() *plantspec.Plant {
	return &plantspec.Plant{
		Payloads: []plantspec.Payload{{Code: "SHIM", UOPCapacity: 40}},
		Processes: []plantspec.Process{
			{Name: "LOADER", ActiveStyle: "LOADER-RUN"},
			{Name: "CELL", ActiveStyle: "CELL-RUN"},
		},
		Styles: []plantspec.Style{
			{Name: "LOADER-RUN", Process: "LOADER", Payload: "SHIM"},
			{Name: "CELL-RUN", Process: "CELL", Payload: "SHIM"},
		},
		Claims: []plantspec.Claim{
			// The pinned home and one buffer: same circuit, both naming the market.
			{CoreNode: "H1", Style: "LOADER-RUN", Role: "produce", SwapMode: "manual_swap",
				Payload: "SHIM", UOPCapacity: 40, InboundSource: "MARKET", HomeOf: "G"},
			{CoreNode: "H2", Style: "LOADER-RUN", Role: "produce", SwapMode: "manual_swap",
				Payload: "SHIM", UOPCapacity: 40, InboundSource: "MARKET", HomeOf: "G", HomeKind: "buffer"},
			{CoreNode: "ALN_8", Style: "CELL-RUN", Role: "consume", SwapMode: "two_robot",
				Payload: "SHIM", UOPCapacity: 40, InboundSource: "H1", OutboundDestination: "H1"},
		},
		Zones: []plantspec.Zone{{
			Name:  "MARKET",
			Lanes: []plantspec.Lane{{Name: "L1", Slots: []plantspec.Slot{{Name: "M1", Depth: 1}, {Name: "M2", Depth: 2}}}},
		}},
		Bins: []plantspec.Bin{{Name: "mt1", Slot: "M1"}, {Name: "mt2", Slot: "M2"}},
	}
}

// TestCarriers_ClosedDedicatedLoopIsNotADrawOnTheMarket pins the first of the two
// mis-models that invented demo.yaml's 0.45 bins/min SYN_MARKET deficit.
//
// The loop conserves every carrier it holds: the empty the loader fills next is
// the one the cell just returned. Reading inbound_source literally charged the
// whole spend to the market and credited the whole supply to the home, so a
// closed circuit read as a drain on a pool it never touches — and the matching
// surplus landed on a pool that is "not a seeded zone", where it went unjudged.
//
// The tell was there all along and nobody could see it: the plant-wide balance
// was EXACTLY 0.00 while a pool showed a deficit. Material was conserved; only
// the attribution leaked.
func TestCarriers_ClosedDedicatedLoopIsNotADrawOnTheMarket(t *testing.T) {
	t.Parallel()
	plant := dedicatedLoopPlant()
	// The cell draws 10 parts/min against a 40-UOP carrier: 0.25 bins/min.
	p := computeCarriers(plant, map[string]float64{"CELL": 10.0}, 0, 0)

	market, home := p.pools["MARKET"], p.pools["H1"]
	if home == nil {
		t.Fatalf("the loop's own pool was never created: %v", p.pools)
	}
	if market != nil && market.emptyDemand > 0.001 {
		t.Errorf("MARKET is charged %.2f bins/min of spend for a CLOSED loop. Its inbound_source "+
			"is the top-up path for a carrier lost out of the circuit, not a steady-state draw",
			market.emptyDemand)
	}
	if home.emptyDemand < 0.001 || home.emptySupply < 0.001 {
		t.Fatalf("the circuit shows spend %.2f supply %.2f — both sides must land on the loop",
			home.emptyDemand, home.emptySupply)
	}
	if net := home.emptySupply - home.emptyDemand; math.Abs(net) > 0.001 {
		t.Errorf("the closed loop nets %+.2f bins/min. A circuit where the consumer hands its empty "+
			"straight back to the producer neither gains nor loses carriers", net)
	}
	if !home.isClosedLoop {
		t.Error("the loop was not marked as a closed circuit, so it will be judged against a " +
			"transit floor — a REQUIRED with no referent beside a SEEDED that was never counted")
	}
	// ONE standing carrier for the circuit, not one per position.
	if home.producePoint != 1 {
		t.Errorf("producePoint = %d, want 1. The home and its buffers are one dedicated loader; "+
			"charging each a carrier to wait on counts the same circuit twice", home.producePoint)
	}
	if market != nil && market.producePoint != 0 {
		t.Errorf("MARKET carries %d produce points for stations inside a closed loop — it is being "+
			"asked to keep a spare carrier for a station that never comes for one", market.producePoint)
	}
}

// TestCarriers_MaintainedGroupSurplusOverflowsToItsCounterparty pins the second
// mis-model. A maintained group is a LEVEL-CONTROLLED buffer: Core holds it at
// its declared want, so a surplus does not pile up in it forever — it goes to the
// overflow zone, which is where the carriers actually end up.
//
// Modelling the group as closed stranded that surplus and left the overflow pool
// wearing a deficit for carriers it was in fact receiving.
func TestCarriers_MaintainedGroupSurplusOverflowsToItsCounterparty(t *testing.T) {
	t.Parallel()
	plant := carrierPlant(30, 30)
	// The press draws its empties from the kept bank; the weld cell returns them
	// there too, but the fixture makes the return side faster so the bank runs a
	// surplus that has to go somewhere.
	plant.Claims[0].InboundSource = "BANK"
	plant.Claims[1].OutboundDestination = "BANK"
	plant.Zones = append(plant.Zones, plantspec.Zone{
		Name:      "BANK",
		Positions: []plantspec.Slot{{Name: "B1", Depth: 1}, {Name: "B2", Depth: 1}, {Name: "B3", Depth: 1}},
	}, plantspec.Zone{
		Name:  "MARKET",
		Lanes: []plantspec.Lane{{Name: "ML", Slots: []plantspec.Slot{{Name: "M1", Depth: 1}}}},
	})
	plant.Bins = append(plant.Bins,
		plantspec.Bin{Name: "mt1", Slot: "B1"}, plantspec.Bin{Name: "mt2", Slot: "B2"})
	plant.MaintainedGroups = []plantspec.MaintainedGroup{{
		Group: "BANK", Station: "edge1.line1", Overflow: "MARKET",
		Levels: []plantspec.MaintainLevel{{BinType: "STANDARD", Want: 2}},
	}}

	// Consumer twice the producer: the bank frees more than it spends.
	p := computeCarriers(plant, map[string]float64{"PRESS": 6.0, "WELD": 12.0}, 0, 0)

	bank, market := p.pools["BANK"], p.pools["MARKET"]
	if bank == nil || market == nil {
		t.Fatalf("pools not attributed: %v", p.pools)
	}
	if !bank.isMaintained || bank.levelWant != 2 {
		t.Errorf("BANK isMaintained=%v levelWant=%d, want true/2 — a kept group is judged against "+
			"its declared level, not against demand x transit", bank.isMaintained, bank.levelWant)
	}
	net := bank.emptySupply - bank.emptyDemand
	if net < 0.001 {
		t.Fatalf("fixture: the bank must run a SURPLUS to test the overflow, got %+.3f", net)
	}
	if math.Abs(bank.keeperOut-net) > 0.001 {
		t.Errorf("keeperOut = %.3f, want %.3f — the surplus leaves the group for its overflow zone "+
			"rather than accumulating in positions declared to hold a fixed level",
			bank.keeperOut, net)
	}
	if math.Abs(market.keeperIn-net) > 0.001 {
		t.Errorf("MARKET keeperIn = %.3f, want %.3f. The carriers the bank sheds arrive HERE, and "+
			"without this term the overflow pool wears a deficit for empties it is being handed",
			market.keeperIn, net)
	}
	// The two must settle against each other, which is the whole point.
	if eff := (bank.emptySupply + bank.keeperIn) - (bank.emptyDemand + bank.keeperOut); math.Abs(eff) > 0.001 {
		t.Errorf("the kept group nets %+.3f after the keeper flow; a level-held pool settles to zero", eff)
	}
}

// coupledPlant builds demo.yaml's WELD-2 shape: a press feeding a weld cell that
// also waits on a second part from somewhere else.
//
//	PRESS  ──PANEL──▶ WELD ◀──BRKT── LOADER
//
// The press has one way to stop. The weld cell has two, and only one of them is
// the press's problem.
func coupledPlant() *plantspec.Plant {
	return &plantspec.Plant{
		BinTypes: []string{"STANDARD", "STANDARD-SM"},
		Payloads: []plantspec.Payload{
			{Code: "PANEL", UOPCapacity: 30, BinType: "STANDARD-SM"},
			{Code: "BRKT", UOPCapacity: 40, BinType: "STANDARD"},
		},
		Processes: []plantspec.Process{
			{Name: "PRESS", ActiveStyle: "PRESS-RUN"},
			{Name: "WELD", ActiveStyle: "WELD-RUN"},
			{Name: "LOADER", ActiveStyle: "LOADER-RUN"},
		},
		Styles: []plantspec.Style{
			{Name: "PRESS-RUN", Process: "PRESS", Payload: "PANEL"},
			{Name: "WELD-RUN", Process: "WELD", Payload: "PANEL"},
			{Name: "LOADER-RUN", Process: "LOADER", Payload: "BRKT"},
		},
		Claims: []plantspec.Claim{
			{CoreNode: "PLN_1", Style: "PRESS-RUN", Role: "produce", SwapMode: "single_robot",
				Payload: "PANEL", UOPCapacity: 30},
			{CoreNode: "ALN_1", Style: "WELD-RUN", Role: "consume", SwapMode: "single_robot",
				Payload: "PANEL", UOPCapacity: 30},
			// The SECOND input, on the same process. This is the whole fixture.
			{CoreNode: "ALN_2", Style: "WELD-RUN", Role: "consume", SwapMode: "single_robot",
				Payload: "BRKT", UOPCapacity: 40},
			{CoreNode: "PLK_1", Style: "LOADER-RUN", Role: "produce", SwapMode: "manual_swap",
				Payload: "BRKT", UOPCapacity: 40},
		},
		// Six carriers, four of them the small type PANEL rides.
		Bins: []plantspec.Bin{
			{Name: "sm1", Slot: "X1", BinType: "STANDARD-SM"},
			{Name: "sm2", Slot: "X2", BinType: "STANDARD-SM"},
			{Name: "sm3", Slot: "X3", BinType: "STANDARD-SM"},
			{Name: "sm4", Slot: "X4", BinType: "STANDARD-SM", Payload: "PANEL", UOP: 30},
			// No bin_type: falls back to the FIRST declared type, as seed_core.go does.
			{Name: "st1", Slot: "X5"},
			{Name: "st2", Slot: "X6"},
		},
	}
}

// TestCoupling_FindsTheConsumerWithMoreWaysToStop pins the shape that drained
// demo.yaml's STANDARD-SM pool, and the arithmetic that sizes how long it takes.
//
// The rate check passes this plant at every moment: PANEL is filled and emptied
// at the same configured rate. What it cannot see is that only one side is ABLE
// to hit its rate — the weld cell also waits on BRKT — so the difference lands
// as full carriers and the small-carrier pool goes to zero.
func TestCoupling_FindsTheConsumerWithMoreWaysToStop(t *testing.T) {
	t.Parallel()
	// The press fills 6 parts/min into 30-UOP carriers: 0.20 bins/min.
	cs := computeCoupling(coupledPlant(), map[string]float64{"PRESS": 6.0, "WELD": 6.0}, nil)

	if len(cs) != 1 {
		t.Fatalf("got %d couplings, want exactly 1 (PANEL): %+v", len(cs), cs)
	}
	c := cs[0]
	if c.payload != "PANEL" || c.producer != "PRESS" || c.consumer != "WELD" {
		t.Errorf("got %s %s->%s, want PANEL PRESS->WELD", c.payload, c.producer, c.consumer)
	}
	if len(c.siblings) != 1 || c.siblings[0] != "BRKT" {
		t.Errorf("siblings = %v, want [BRKT] — naming the OTHER input is the actionable half; "+
			"without it the report says a cell stalls and not what it is waiting for", c.siblings)
	}
	// THE CARRIER TYPE IS THE PAYLOAD'S, AND THE POOL IS COUNTED OFF THE BIN'S
	// OWN FIELD. An empty bin has no payload to infer a type from, and empties
	// are the entire population this measures.
	if c.binType != "STANDARD-SM" {
		t.Errorf("binType = %q, want STANDARD-SM", c.binType)
	}
	if c.poolEmpty != 3 || c.poolTotal != 4 {
		t.Errorf("pool = %d empty of %d, want 3 of 4 — the two untyped carriers belong to "+
			"STANDARD (the first declared type, which is what seed_core.go falls back to) and "+
			"must not be counted as room for a small one", c.poolEmpty, c.poolTotal)
	}
	if math.Abs(c.fillRate-0.20) > 0.001 {
		t.Errorf("fillRate = %.3f bins/min, want 0.20", c.fillRate)
	}
	// The drain, at the 95% column: 0.20 x 0.05 = 0.01 bins/min against 3
	// empties is 300 minutes.
	if drain := c.fillRate * 0.05; math.Abs(float64(c.poolEmpty)/drain-300) > 1 {
		t.Errorf("hours-to-jam arithmetic drifted: %.1f min at 95%%, want 300", float64(c.poolEmpty)/drain)
	}
}

// TestCoupling_SaysNothingWhenBothSidesAreEquallyExposed keeps the section quiet
// on the ordinary case. A report that fires on every plant is one people stop
// reading, and the balanced two-station loop is most of every fixture.
func TestCoupling_SaysNothingWhenBothSidesAreEquallyExposed(t *testing.T) {
	t.Parallel()
	if cs := computeCoupling(carrierPlant(30, 30), map[string]float64{"PRESS": 6.0, "WELD": 6.0}, nil); len(cs) != 0 {
		t.Errorf("got %d couplings on a one-in/one-out loop, want 0: %+v", len(cs), cs)
	}

	// And a LOADER-fed payload is not an accumulation risk either: a manual_swap
	// producer has no counter to outrun its consumer with — it fills on demand.
	p := coupledPlant()
	p.Claims[0].SwapMode = "manual_swap" // the press becomes a loader
	if cs := computeCoupling(p, map[string]float64{"PRESS": 6.0, "WELD": 6.0}, nil); len(cs) != 0 {
		t.Errorf("got %d couplings with no tick producer, want 0. A loader fills what is asked "+
			"for; it cannot run ahead of a stopped consumer: %+v", len(cs), cs)
	}
}
