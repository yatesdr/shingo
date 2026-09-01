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
func TestHeadroom_GatedFreeSlotsDoNotCount(t *testing.T) {
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
	if z.deepestDig <= z.freeUngated {
		t.Error("this plant must read as SHORT: a four-blocker dig with zero reachable free slots " +
			"is the shape that waits forever while the free-slot total looks healthy")
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
