package main

import (
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"shingocore/plantspec"
)

// kanbans_test.go — the rig's numbers as simcalc computes them, off the shipped
// plant and dev config: this is the check `simcalc -carriers` prints, as a test.

// kanbanPress is the master as simcalc reads it, with its kanbans narrowed to the
// [kanban] section's K and K2: their pools are their own, and the other sections'
// kanbans have their own pins (the [demo] one reads as demo-sim's did).
func kanbanPress(t *testing.T) (*plantspec.Plant, map[string]float64) {
	t.Helper()
	p, err := plantspec.Load(filepath.Join("..", "..", "..", "plants", "master.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	kept := p.Kanbans[:0:0]
	for _, k := range p.Kanbans {
		if k.Group == "PRESS-KANBAN" || k.Group == "WELD-KANBAN" {
			kept = append(kept, k)
		}
	}
	p.Kanbans = kept
	edge, err := loadEdge(filepath.Join("..", "..", "..", "shingo-edge", "shingoedge.dev.yaml"))
	if err != nil {
		t.Fatalf("load edge: %v", err)
	}
	return p, processRates(p, edgeRates(edge))
}

// A loader's allowed list is its parts: each flowing part is drained, one no style
// runs makes no flow of its own, so the unloader reads DRAIN and not NO-SOURCE.
func TestWalkClaims_AnUnloadersAllowedListDrainsWhatFlows(t *testing.T) {
	t.Parallel()
	p, rate := kanbanPress(t)
	flows := walkClaims(p, rate)
	for _, part := range []string{"KP-SM-L", "KP-SM-R", "KW-PART"} {
		f := flows[part]
		if f == nil {
			t.Fatalf("%s has no flow", part)
		}
		if label, healthy, detail := f.verdict(7.5, 7.5); !healthy || label != "DRAIN" {
			t.Errorf("%s = %s (%s), want a healthy DRAIN", part, label, detail)
		}
	}
	for _, idle := range []string{"KP-BIG", "KP-TOTE", "KP-32", "KP-UNTYPED", ""} {
		if f := flows[idle]; f != nil {
			t.Errorf("part %q, which no running style makes, has a flow: %+v", idle, f)
		}
	}
}

// Sized for the run: both pools balance at the configured ticks, the level the pool
// is judged on is the want and not the bare level, and nothing crosses a boundary in
// steady state.
func TestKanbanPress_PoolsBalanceAndAreJudgedOnTheirWant(t *testing.T) {
	t.Parallel()
	p, rate := kanbanPress(t)
	plan := computeCarriers(p, rate, 7.5, 7.5)
	for name, want := range map[string]int{"PRESS-KANBAN": 8, "WELD-KANBAN": 2} {
		q := plan.pools[name]
		if q == nil {
			t.Fatalf("no pool %s", name)
		}
		if !q.isKanban || q.levelWant != want {
			t.Errorf("%s: kanban %v, want %d; expected %d", name, q.isKanban, q.levelWant, want)
		}
		if q.seededEmpty < q.levelWant {
			t.Errorf("%s seeded %d against a want of %d", name, q.seededEmpty, q.levelWant)
		}
		if q.keeperIn != 0 || q.keeperOut != 0 {
			t.Errorf("%s keeper in %.2f out %.2f; a balanced loop moves nothing across a boundary", name, q.keeperIn, q.keeperOut)
		}
		if q.emptyDemand <= 0 || q.emptyDemand != q.emptySupply {
			t.Errorf("%s spends %.3f and frees %.3f, want equal and above 0", name, q.emptyDemand, q.emptySupply)
		}
	}
}

// K's cover is the 4 carts on the positions, and they count: K's small want of 6 is its 2 and
// the line's 4, and the plant's own file passes.
func TestKanbanPress_KHoldsItsWantOfTheRunningType(t *testing.T) {
	t.Parallel()
	p, rate := kanbanPress(t)
	v := newKanbanView(p)
	g, ok := v.groupNamed("PRESS-KANBAN")
	if !ok {
		t.Fatal("no PRESS-KANBAN")
	}
	want := map[string][3]int{"SMALL-45": {6, 2, 4}, "BIG-45": {2, 2, 0}, "TOTES-45": {2, 2, 0}, "CART-32": {2, 6, 0}}
	for _, l := range v.lines(g, v.activeStyles()) {
		if w := want[l.binType]; l.want != w[0] || l.seeded != w[1] || l.held != w[2] {
			t.Errorf("K's %s = want %d, seeded %d, held %d; expected %v", l.binType, l.want, l.seeded, l.held, w)
		}
	}
	if !reportKanbans(p, computeCarriers(p, rate, 7.5, 7.5), rate, 200) {
		t.Error("the plant's own file did not pass the kanban check")
	}
}

// A kanban's shortfall settles against its first source, not its overflow. The rig's
// kanbans have no source, so this adds one.
func TestComputeCarriers_AShortfallSettlesAgainstTheFirstSource(t *testing.T) {
	t.Parallel()
	p, rate := kanbanPress(t)
	p.Zones = append(p.Zones, plantspec.Zone{Name: "LENDER", Positions: []plantspec.Slot{{Name: "LND_001", Depth: 1}}})
	p.Kanbans = append(p.Kanbans, plantspec.Kanban{Group: "LENDER", Levels: []plantspec.KanbanLevel{{BinType: "SMALL-45", Want: 1}}})
	for i := range p.Kanbans {
		if p.Kanbans[i].Group == "WELD-KANBAN" {
			p.Kanbans[i].Sources = []string{"LENDER"}
		}
	}
	// Cut the weld's unloader off, so K2 spends and nothing returns to it.
	kept := p.Claims[:0:0]
	for _, c := range p.Claims {
		if c.WindowOf != "KW-UNLOADER" {
			kept = append(kept, c)
		}
	}
	p.Claims = kept
	plan := computeCarriers(p, rate, 7.5, 7.5)
	in, out := plan.pools["WELD-KANBAN"].keeperIn, plan.pools["LENDER"].keeperOut
	if in <= 0 || in != out {
		t.Errorf("K2 keeper in %.3f, LENDER keeper out %.3f, want the same shortfall on both", in, out)
	}
}

// THE CLOCK: what starves first with its returns stopped, the arithmetic the file's
// head and plants/README.md quote. No lender behind either: K's 2 small, K2's 2.
func TestKanbanPress_TheClock(t *testing.T) {
	t.Parallel()
	p, rate := kanbanPress(t)
	clock := newKanbanView(p).clock(rate)
	if len(clock) != 2 {
		t.Fatalf("clock = %+v, want the two running types", clock)
	}
	first, second := clock[0], clock[1]
	if first.kanban != "PRESS-KANBAN" || first.binType != "SMALL-45" || first.stock != 2 || int(first.mins+0.5) != 10 {
		t.Errorf("first to starve = %+v, want PRESS-KANBAN's small, stock 2, 10 sim-min", first)
	}
	if second.kanban != "WELD-KANBAN" || second.binType != "SMALL-45" || second.stock != 2 || int(second.mins+0.5) != 15 {
		t.Errorf("second = %+v, want WELD-KANBAN's small, stock 2, 15 sim-min", second)
	}
}

// THE FAMILIES, the arithmetic behind R2, R3 and the walk: 45x48 is 10 carts on a minimum of
// 6 + 4 and 10 slots, 32x30 is 6 on 2 + 4 and 6, and the weld's small is its own 3.
func TestKanbanPress_TheFamilies(t *testing.T) {
	t.Parallel()
	p, _ := kanbanPress(t)
	v := newKanbanView(p)
	want := map[string]map[string]familyLine{
		"PRESS-KANBAN": {
			"45x48":   {family: "45x48", pool: 10, minimum: 10, room: 10, carts: 10},
			"CART-32": {family: "CART-32", pool: 6, minimum: 6, room: 6, carts: 6},
		},
		"WELD-KANBAN": {"45x48": {family: "45x48", pool: 3, minimum: 3, room: 4, carts: 3}},
	}
	for group, families := range want {
		g, ok := v.groupNamed(group)
		if !ok {
			t.Fatalf("no %s", group)
		}
		got := v.families(g)
		if len(got) != len(families) {
			t.Errorf("%s has %d families, want %d: %+v", group, len(got), len(families), got)
		}
		for _, f := range got {
			if f != families[f.family] {
				t.Errorf("%s %s = %+v, want %+v", group, f.family, f, families[f.family])
			}
		}
	}
}

// Red: a kanban seeded under its want, or a pool draining inside the run, fails the
// command. K made whole by hand first, because the fixture seats it short.
func TestReportKanbans_FailsBelowWantAndOnADrainInsideTheRun(t *testing.T) {
	t.Parallel()
	p, rate := kanbanPress(t)
	whole := *p
	if !reportKanbans(&whole, computeCarriers(&whole, rate, 7.5, 7.5), rate, 200) {
		t.Error("the plant's own file did not pass")
	}
	short := whole
	short.Bins = nil
	for _, b := range whole.Bins {
		if b.Name != "KB-K-S2" {
			short.Bins = append(short.Bins, b)
		}
	}
	if reportKanbans(&short, computeCarriers(&short, rate, 7.5, 7.5), rate, 200) {
		t.Error("K one under its want and its pool passed")
	}

	plan := computeCarriers(&whole, rate, 7.5, 7.5)
	plan.pools["WELD-KANBAN"].emptySupply = 0
	if reportKanbans(&whole, plan, rate, 200) {
		t.Error("K2 with nothing returning passed a 200 sim-minute run")
	}
	if !reportKanbans(&whole, plan, rate, 10) {
		t.Error("a 10 sim-minute run is inside K2's stock and should pass")
	}
}

// TestMasterKanbans_EveryKanbanHoldsTheRun: every kanban of the master, [demo]'s
// press empties included, at its want and every family at its pool for a 40-minute
// run at 10x. RED while [demo]'s kanban was seeded at its level: its families stood
// 6 of 8 and 4 of 6.
func TestMasterKanbans_EveryKanbanHoldsTheRun(t *testing.T) {
	t.Parallel()
	p, err := plantspec.Load(filepath.Join("..", "..", "..", "plants", "master.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	edge, err := loadEdge(filepath.Join("..", "..", "..", "shingo-edge", "shingoedge.dev.yaml") + "," +
		filepath.Join("..", "..", "..", "shingo-edge", "shingoedge.edge2.yaml"))
	if err != nil {
		t.Fatalf("load edges: %v", err)
	}
	rate := processRates(p, edgeRates(edge))
	if !reportKanbans(p, computeCarriers(p, rate, 7.5, 7.5), rate, 400) {
		t.Error("a kanban of the master is under its want or its pool, or drains inside the run")
	}
}

// The two repairs of the master, as data a test can set either way.
const (
	oldUnloaderOutbound = "SYN_PRESS_EMPTIES" // UNLOADER-A pushed into the press empties
	newUnloaderOutbound = "SYN_MARKET"        // it returns them where ASSY drew them
	oldP5SmallSpares    = 2                   // 45x48 pool 26
	newP5SmallSpares    = 12                  // 45x48 pool 36
	oldMarketStandard   = 12                  // empty STANDARD standing in SYN_MARKET
	newMarketStandard   = 16                  // 10 in flight and its 6 stations, by type
	simcalcChildEnv     = "SIMCALC_CHILD_MASTER"
)

// setRepairs puts the master at one state of the two repairs, whichever state
// the file ships in, so "the old one" stays a case after the file is repaired.
func setRepairs(t *testing.T, p *plantspec.Plant, outbound string, spares, market int) {
	t.Helper()
	setMarketStandard(t, p, market)
	const k = "P5-KANBAN"
	claims, zones, kanbans := 0, 0, 0
	for i := range p.Claims {
		if p.Claims[i].CoreNode == "FGN_001" && p.Claims[i].Style == "UNLOADER-A-RUN" {
			p.Claims[i].OutboundDestination = outbound
			claims++
		}
	}
	smallSlot, otherSlot := map[string]bool{}, map[string]bool{}
	for _, b := range p.Bins {
		if b.BinType == "SMALL-45" {
			smallSlot[b.Slot] = true
		} else {
			otherSlot[b.Slot] = true
		}
	}
	inK := map[string]bool{}
	for i := range p.Zones {
		z := &p.Zones[i]
		if z.Name != k {
			continue
		}
		zones++
		// The slots the other types stand on, then one for each small cart of
		// the loop, standing or on the line: room is the pool.
		var slots []plantspec.Slot
		for _, s := range z.Positions {
			inK[s.Name] = true
			if otherSlot[s.Name] {
				slots = append(slots, s)
			}
		}
		for j := range spares + 20 {
			slots = append(slots, plantspec.Slot{Name: fmt.Sprintf("P5T_%03d", j), Depth: 1, Admits: []string{"45x48"}})
		}
		z.Positions = slots
	}
	p.Bins = slices.DeleteFunc(p.Bins, func(b plantspec.Bin) bool { return inK[b.Slot] && smallSlot[b.Slot] })
	for j := range spares {
		p.Bins = append(p.Bins, plantspec.Bin{Name: fmt.Sprintf("P5-T-%03d", j), Slot: fmt.Sprintf("P5T_%03d", j), BinType: "SMALL-45"})
	}
	for i := range p.Kanbans {
		if p.Kanbans[i].Group == k {
			p.Kanbans[i].Pools["45x48"] = 20 + 4 + spares // the line, 2 big and 2 totes, the small
			kanbans++
		}
	}
	if claims != 1 || zones != 1 || kanbans != 1 {
		t.Fatalf("fixture: %d UNLOADER-A claims, %d zones and %d kanbans named %s, want one of each", claims, zones, kanbans, k)
	}
}

// runCarriersAsTheCommand runs `simcalc -carriers -run 400m` on the master at a
// state of the repairs in a child of this binary, because the failing verdict
// of the check is os.Exit(1). state is "shipped", or "<outbound> <spares>".
func runCarriersAsTheCommand(t *testing.T, state string) (int, string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("find the test binary: %v", err)
	}
	cmd := exec.Command(self, "-test.run=^TestSimcalcChild_CarriersOnTheMaster$")
	cmd.Env = append(os.Environ(), simcalcChildEnv+"="+state)
	out, err := cmd.CombinedOutput()
	code := 0
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatalf("run the child: %v", err)
	}
	// A verdict, not a child that died on the way to one: both exit 1.
	headlines := map[int]string{0: "SUSTAINS", 1: "WILL JAM"}
	if want, ok := headlines[code]; !ok || !strings.Contains(string(out), "\n"+want+"\n") {
		t.Fatalf("the child printed no verdict for exit %d:\n%s", code, out)
	}
	return code, string(out)
}

// TestSimcalcChild_CarriersOnTheMaster is the child of runCarriersAsTheCommand,
// and does nothing as a test of its own.
func TestSimcalcChild_CarriersOnTheMaster(t *testing.T) {
	state := os.Getenv(simcalcChildEnv)
	if state == "" {
		return
	}
	root := filepath.Join("..", "..", "..")
	p, err := plantspec.Load(filepath.Join(root, "plants", "master.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if state != "shipped" {
		var outbound string
		var spares, market int
		if _, err := fmt.Sscanf(state, "%s %d %d", &outbound, &spares, &market); err != nil {
			t.Fatalf("state %q: %v", state, err)
		}
		setRepairs(t, p, outbound, spares, market)
	}
	edge, err := loadEdge(filepath.Join(root, "shingo-edge", "shingoedge.dev.yaml") + "," +
		filepath.Join(root, "shingo-edge", "shingoedge.edge2.yaml"))
	if err != nil {
		t.Fatalf("load edges: %v", err)
	}
	runCarriers(p, processRates(p, edgeRates(edge)), "", "plants/master.yaml", 400,
		capacityPerMin(edge.Sim.Operators.LoaderAutoLoad), capacityPerMin(edge.Sim.Operators.UnloaderAutoClear))
	os.Exit(0)
}

// failsNaming says the line of the named pool carries the verdict.
func failsNaming(out, pool, verdict string) bool {
	return regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(pool) + ` .*` + regexp.QuoteMeta(verdict)).MatchString(out)
}

// TestMasterCarriers_TheShippedPlantPasses: the command passes the plant the
// repo ships. RED at 6bd55616, the unrepaired plant.
func TestMasterCarriers_TheShippedPlantPasses(t *testing.T) {
	t.Parallel()
	if code, out := runCarriersAsTheCommand(t, "shipped"); code != 0 {
		t.Errorf("simcalc -carriers -run 400m on plants/master.yaml exits %d, want 0: %v", code,
			regexp.MustCompile(`(?m)^.*(RATE DEFICIT|RATE SURPLUS|UNDER ITS|SHORT BY|BELOW WANT|BELOW LEVEL).*$`).FindAllString(out, -1))
	}
}

// TestMasterCarriers_EachRepairIsNeededOnItsOwn: the three repairs, every one of
// the eight ways to have some: all three pass, and each missing one fails by its
// own finding and no other's. RED at 6bd55616: neither kanban finding existed,
// and the repaired plant read as draining. RED at 1c3df0ec: a plain pool's
// floor summed its types, and SYN_MARKET's 5 STANDARD-SM stood in for the 4
// STANDARD it is short.
func TestMasterCarriers_EachRepairIsNeededOnItsOwn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		edge, carts, market bool
	}{
		{true, true, true}, {false, true, true}, {true, false, true}, {true, true, false},
		{false, false, true}, {false, true, false}, {true, false, false}, {false, false, false},
	} {
		outbound, spares, market := oldUnloaderOutbound, oldP5SmallSpares, oldMarketStandard
		if tc.edge {
			outbound = newUnloaderOutbound
		}
		if tc.carts {
			spares = newP5SmallSpares
		}
		if tc.market {
			market = newMarketStandard
		}
		t.Run(fmt.Sprintf("edge=%v,P5=%v,market=%v", tc.edge, tc.carts, tc.market), func(t *testing.T) {
			t.Parallel()
			code, out := runCarriersAsTheCommand(t, fmt.Sprintf("%s %d %d", outbound, spares, market))
			if wantFail := !tc.edge || !tc.carts || !tc.market; (code == 1) != wantFail {
				t.Errorf("exit %d, want failing = %v", code, wantFail)
			}
			if got := failsNaming(out, "SYN_PRESS_EMPTIES", "RATE SURPLUS"); got != !tc.edge {
				t.Errorf("SYN_PRESS_EMPTIES reads RATE SURPLUS = %v, want %v", got, !tc.edge)
			}
			if got := failsNaming(out, "P5-KANBAN", "UNDER ITS FLOOR"); got != !tc.carts {
				t.Errorf("P5-KANBAN reads UNDER ITS FLOOR = %v, want %v", got, !tc.carts)
			}
			if got := failsNaming(out, "SYN_MARKET", "STANDARD SHORT BY"); got != !tc.market {
				t.Errorf("SYN_MARKET reads STANDARD SHORT BY = %v, want %v", got, !tc.market)
			}
			// The deficit of the market under the old edge is the same missing
			// repair; with the edge repaired no line may read a deficit.
			if tc.edge && strings.Contains(out, "RATE DEFICIT") {
				t.Errorf("a pool reads RATE DEFICIT with the edge repaired: %v",
					regexp.MustCompile(`(?m)^.*RATE DEFICIT.*$`).FindAllString(out, -1))
			}
		})
	}
}

// TestMasterCarriers_P5FloorOverABand: with the edge repaired, the floor of P5
// is 1.00 a minute over a 10 minute transit, and its spares stand either side
// of it by 1/5/10/25/50%, to the cart. RED at 6bd55616: no count of spares
// changed the verdict.
func TestMasterCarriers_P5FloorOverABand(t *testing.T) {
	t.Parallel()
	const floor = 10
	spares := []int{floor}
	for _, m := range checkMargins {
		n := int(math.Ceil(floor * m))
		spares = append(spares, floor-n, floor+n)
	}
	slices.Sort(spares)
	for _, n := range slices.Compact(spares) {
		t.Run(fmt.Sprintf("spares=%d", n), func(t *testing.T) {
			t.Parallel()
			code, out := runCarriersAsTheCommand(t, fmt.Sprintf("%s %d %d", newUnloaderOutbound, n, newMarketStandard))
			short := n < floor
			if (code == 1) != short || failsNaming(out, "P5-KANBAN", "UNDER ITS FLOOR") != short {
				t.Errorf("%d spares against a floor of %d: exit %d, want failing on the floor = %v", n, floor, code, short)
			}
		})
	}
}

// setMarketStandard stands n empty STANDARD in SYN_MARKET, whichever count the
// file ships: the last seeded go, or each one more stands in a one-slot lane of
// its own, so no dig and no other lane changes.
func setMarketStandard(t *testing.T, p *plantspec.Plant, n int) {
	t.Helper()
	const market = "SYN_MARKET"
	var z *plantspec.Zone
	inMarket := map[string]bool{}
	for i := range p.Zones {
		if p.Zones[i].Name == market {
			z = &p.Zones[i]
			for _, l := range z.Lanes {
				for _, s := range l.Slots {
					inMarket[s.Name] = true
				}
			}
		}
	}
	if z == nil {
		t.Fatalf("fixture: no zone %s", market)
	}
	standard := func(b plantspec.Bin) bool {
		return inMarket[b.Slot] && b.Payload == "" && (b.BinType == "" || b.BinType == "STANDARD")
	}
	var stands []int
	for i, b := range p.Bins {
		if standard(b) {
			stands = append(stands, i)
		}
	}
	if len(stands) > n {
		drop := map[int]bool{}
		for _, i := range stands[n:] {
			drop[i] = true
		}
		var kept []plantspec.Bin
		for i, b := range p.Bins {
			if !drop[i] {
				kept = append(kept, b)
			}
		}
		p.Bins = kept
	}
	for j := len(stands); j < n; j++ {
		slot := fmt.Sprintf("MKT_T_%02d", j)
		z.Lanes = append(z.Lanes, plantspec.Lane{Name: "MKT_T_L" + slot[6:], Slots: []plantspec.Slot{{Name: slot, Depth: 1}}})
		p.Bins = append(p.Bins, plantspec.Bin{Name: "BIN-MKT-T-" + slot[6:], Slot: slot, BinType: "STANDARD"})
	}
	got := 0
	for _, b := range p.Bins {
		if standard(b) || strings.HasPrefix(b.Slot, "MKT_T_") {
			got++
		}
	}
	if got != n {
		t.Fatalf("fixture: %d empty STANDARD in %s, want %d", got, market, n)
	}
}

// TestMasterCarriers_MarketStandardFloorOverABand: with the kanban repairs in,
// SYN_MARKET's STANDARD floor is 1.00 a minute over a 10 minute transit and its 6
// stations, 16, and its empty STANDARD stand either side of it by 1/5/10/25/50%,
// to the cart, beside 5 STANDARD-SM that no draw there takes. RED at 1c3df0ec
// on the short side: the floor summed the types.
func TestMasterCarriers_MarketStandardFloorOverABand(t *testing.T) {
	t.Parallel()
	const floor = newMarketStandard
	stands := []int{floor}
	for _, m := range checkMargins {
		n := int(math.Ceil(floor * m))
		stands = append(stands, floor-n, floor+n)
	}
	slices.Sort(stands)
	for _, n := range slices.Compact(stands) {
		t.Run(fmt.Sprintf("standard=%d", n), func(t *testing.T) {
			t.Parallel()
			code, out := runCarriersAsTheCommand(t, fmt.Sprintf("%s %d %d", newUnloaderOutbound, newP5SmallSpares, n))
			short := n < floor
			if (code == 1) != short || failsNaming(out, "SYN_MARKET", "STANDARD SHORT BY") != short {
				t.Errorf("%d empty STANDARD against a floor of %d: exit %d, want failing on the floor = %v", n, floor, code, short)
			}
		})
	}
}
