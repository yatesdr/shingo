package main

import (
	"fmt"
	"sort"
	"strings"

	"shingocore/plantspec"
)

// kanbans.go — what the carrier check needs to know about a kanban: its want is not its level,
// and its pool per family must fit the line, the mix and its slots.
//
// THE WANT IS CORE'S: level + cover, the positions the running style's claims put on the line in
// the type (store.KanbanPictureSQL, kanban.Level.Want). Read off the plant file here. A part
// counts only with a carrier (a part declared no_bin_type contributes nothing).

// kanbanLine is one (kanban, type) under one choice of running styles.
type kanbanLine struct {
	binType string
	base    int
	cover   int
	want    int
	seeded  int
	held    int // seeded on the supported positions
}

// kanbanView reads a plant's kanbans once, for the three questions below.
type kanbanView struct {
	plant      *plantspec.Plant
	binTypeOf  map[string]string            // part → carrier, absent for no_bin_type
	capOf      map[string]int64             // part → pack
	seeded     map[string]map[string]int    // group → type → seeded empties
	stylesOf   map[string][]string          // process → its styles, in spec order
	styleParts map[string][]string          // style → its produce claims' parts, once each
	claimsOf   map[string][]plantspec.Claim // style → its produce claims
	nodesOf    map[string][]string          // process → the positions its claims name
	cartsAt    map[string]string            // seeded node → the cart's type
	familyOf   map[string]string            // type → family code
	zoneSlots  map[string]int               // zone → its slots, positions and lanes
}

func newKanbanView(plant *plantspec.Plant) *kanbanView {
	v := &kanbanView{
		plant: plant, binTypeOf: map[string]string{}, capOf: map[string]int64{},
		seeded: map[string]map[string]int{}, stylesOf: map[string][]string{}, styleParts: map[string][]string{},
		claimsOf: map[string][]plantspec.Claim{}, nodesOf: map[string][]string{}, cartsAt: map[string]string{},
		familyOf: map[string]string{}, zoneSlots: map[string]int{},
	}
	for _, pl := range plant.Payloads {
		v.capOf[pl.Code] = pl.UOPCapacity
		if pl.BinType != "" {
			v.binTypeOf[pl.Code] = pl.BinType
		}
	}
	for _, bt := range plant.BinTypes {
		// A type no family names is a family of one, coded by its own code.
		v.familyOf[bt] = bt
	}
	for _, f := range plant.CarrierFamilies {
		for _, t := range f.Types {
			v.familyOf[t] = f.Code
		}
	}
	groupOf := map[string]string{}
	for _, z := range plant.Zones {
		v.zoneSlots[z.Name] = len(z.Positions)
		for _, s := range z.Positions {
			groupOf[s.Name] = z.Name
		}
		for _, l := range z.Lanes {
			v.zoneSlots[z.Name] += len(l.Slots)
			for _, s := range l.Slots {
				groupOf[s.Name] = z.Name
			}
		}
	}
	defaultType := ""
	if len(plant.BinTypes) > 0 {
		defaultType = plant.BinTypes[0]
	}
	for _, b := range plant.Bins {
		bt := b.BinType
		if bt == "" {
			bt = defaultType
		}
		v.cartsAt[b.Slot] = bt
		if g := groupOf[b.Slot]; g != "" && b.Payload == "" {
			if v.seeded[g] == nil {
				v.seeded[g] = map[string]int{}
			}
			v.seeded[g][bt]++
		}
	}
	processOf := map[string]string{}
	for _, s := range plant.Styles {
		processOf[s.Name] = s.Process
		v.stylesOf[s.Process] = append(v.stylesOf[s.Process], s.Name)
	}
	for _, c := range plant.Claims {
		proc := processOf[c.Style]
		for _, n := range c.Positions() {
			v.nodesOf[proc] = appendUniq(v.nodesOf[proc], n)
		}
		if c.Role == "produce" && !c.IsLoader() {
			v.claimsOf[c.Style] = append(v.claimsOf[c.Style], c)
			if c.Payload != "" {
				v.styleParts[c.Style] = appendUniq(v.styleParts[c.Style], c.Payload)
			}
		}
	}
	return v
}

// activeStyles is each process's seed style.
func (v *kanbanView) activeStyles() map[string]string {
	out := map[string]string{}
	for _, p := range v.plant.Processes {
		out[p.Name] = p.ActiveStyle
	}
	return out
}

// lines is a kanban's levels under the running styles given (process → style).
func (v *kanbanView) lines(g plantspec.Kanban, running map[string]string) []kanbanLine {
	out := make([]kanbanLine, 0, len(g.Levels))
	for _, lv := range g.Levels {
		cover := 0
		held := 0
		for _, proc := range g.Supports {
			cover += v.claimedPositions(running[proc], func(bt string) bool { return bt == lv.BinType })
			for _, n := range v.nodesOf[proc] {
				if v.cartsAt[n] == lv.BinType {
					held++
				}
			}
		}
		out = append(out, kanbanLine{binType: lv.BinType, base: lv.Want, cover: cover,
			want: lv.Want + cover, seeded: v.seeded[g.Group][lv.BinType], held: held})
	}
	return out
}

func (v *kanbanView) groupNamed(name string) (plantspec.Kanban, bool) {
	for _, g := range v.plant.Kanbans {
		if g.Group == name {
			return g, true
		}
	}
	return plantspec.Kanban{}, false
}

// clockLine is how long one kanban's running type lasts with its returns stopped.
type clockLine struct {
	kanban  string
	binType string
	spend   float64 // carts/min its supported processes draw
	stock   int     // seeded in the kanban and its sources
	mins    float64 // stock / spend
}

// clock is THE CLOCK: every running type of every kanban with supports, the
// shortest first. A balanced loop never runs this down; it says what starves
// first, and when, if a return path stops.
func (v *kanbanView) clock(rate map[string]float64) []clockLine {
	active := v.activeStyles()
	var out []clockLine
	for _, g := range v.plant.Kanbans {
		for _, lv := range g.Levels {
			spend := 0.0
			for _, proc := range g.Supports {
				for _, c := range v.claimsOf[active[proc]] {
					if v.binTypeOf[c.Payload] == lv.BinType && c.IsActivePull() && v.capOf[c.Payload] > 0 {
						spend += rate[proc] / float64(v.capOf[c.Payload])
					}
				}
			}
			if spend <= 0 {
				continue
			}
			stock := v.seeded[g.Group][lv.BinType]
			for _, src := range g.Sources {
				stock += v.seeded[src][lv.BinType]
			}
			out = append(out, clockLine{kanban: g.Group, binType: lv.BinType, spend: spend, stock: stock,
				mins: float64(stock) / spend})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].mins != out[j].mins {
			return out[i].mins < out[j].mins
		}
		return out[i].kanban < out[j].kanban
	})
	return out
}

// claimedPositions is the DISTINCT positions a style's produce claims put on the line in the types
// asked: a claim's core, paired and second paired nodes, by the claim's own part.
func (v *kanbanView) claimedPositions(style string, ofType func(string) bool) int {
	seen := map[string]bool{}
	for _, c := range v.claimsOf[style] {
		if bt, ok := v.binTypeOf[c.Payload]; ok && ofType(bt) {
			for _, n := range c.Positions() {
				seen[n] = true
			}
		}
	}
	return len(seen)
}

// familyLine is one (kanban, family): what its pool has to fit.
type familyLine struct {
	family  string
	pool    int
	minimum int
	room    int
	carts   int // seeded in the kanban, on its positions and on its unloader's windows
}

// families is the kanban's family lines: the mix of each family's types, the most positions each
// supported process's styles put on the line in it, the slots admitting it, and the carts the file
// seeds in the loop. A family with no pool stands at its minimum, as Core holds it.
func (v *kanbanView) families(g plantspec.Kanban) []familyLine {
	byFamily := map[string]*familyLine{}
	var order []string
	line := func(family string) *familyLine {
		f := byFamily[family]
		if f == nil {
			f = &familyLine{family: family}
			byFamily[family] = f
			order = append(order, family)
		}
		return f
	}
	for _, lv := range g.Levels {
		line(v.familyOf[lv.BinType]).minimum += lv.Want
	}
	for _, f := range order {
		for _, proc := range g.Supports {
			most := 0
			for _, style := range v.stylesOf[proc] {
				most = max(most, v.claimedPositions(style, func(bt string) bool { return v.familyOf[bt] == f }))
			}
			byFamily[f].minimum += most
		}
	}
	for _, z := range v.plant.Zones {
		if z.Name != g.Group {
			continue
		}
		for _, s := range z.Positions {
			if len(s.Admits) == 0 {
				for _, f := range order {
					byFamily[f].room++
				}
				continue
			}
			hit := map[string]bool{}
			for _, t := range v.plant.AdmitsTypes(s.Admits) {
				hit[v.familyOf[t]] = true
			}
			for f := range hit {
				if byFamily[f] != nil {
					byFamily[f].room++
				}
			}
		}
	}
	places := map[string]bool{}
	for _, z := range v.plant.Zones {
		if z.Name == g.Group {
			for _, s := range z.Positions {
				places[s.Name] = true
			}
		}
	}
	for _, proc := range g.Supports {
		for _, n := range v.nodesOf[proc] {
			places[n] = true
		}
	}
	for _, c := range v.plant.Claims {
		if c.WindowOf != "" && c.OutboundDestination == g.Group {
			places[c.CoreNode] = true
		}
	}
	for n, bt := range v.cartsAt {
		if places[n] && byFamily[v.familyOf[bt]] != nil {
			byFamily[v.familyOf[bt]].carts++
		}
	}
	out := make([]familyLine, 0, len(order))
	for _, f := range order {
		l := byFamily[f]
		l.pool = l.minimum
		if pool, ok := g.Pools[f]; ok {
			l.pool = pool
		}
		out = append(out, *l)
	}
	return out
}

// reportKanbans prints the kanbans' wants, their families and THE CLOCK. It fails the check when a
// family's file does not hold what its pool says, when a pool will not fit the line and the mix or
// its slots, when a kanban starts below its want, or when a pool's rate deficit drains its stock
// inside the run.
func reportKanbans(plant *plantspec.Plant, plan carrierPlan, rate map[string]float64, runMins float64) bool {
	if len(plant.Kanbans) == 0 {
		return true
	}
	ok := true
	v := newKanbanView(plant)
	active := v.activeStyles()

	fmt.Printf("\nKANBANS (want = level + cover, the positions the running styles put on the line)\n")
	fmt.Printf("  %-18s %-12s %-6s %-6s %-6s %-7s %-6s %s\n", "KANBAN", "TYPE", "LEVEL", "COVER", "WANT", "SEEDED", "HELD", "VERDICT")
	fmt.Println(strings.Repeat("─", 84))
	for _, g := range plant.Kanbans {
		for _, l := range v.lines(g, active) {
			verdict := "ok"
			if l.seeded+l.held < l.want {
				ok = false
				verdict = fmt.Sprintf("BELOW WANT by %d", l.want-l.seeded-l.held)
			}
			fmt.Printf("  %-18s %-12s %-6d %-6d %-6d %-7d %-6d %s\n", g.Group, l.binType, l.base, l.cover, l.want, l.seeded, l.held, verdict)
		}
	}

	fmt.Printf("\nFAMILIES (pool = carts the loop owns; minimum = mix + each process's most positions; room = slots admitting it)\n")
	fmt.Printf("  %-18s %-12s %-6s %-8s %-6s %-6s %s\n", "KANBAN", "FAMILY", "POOL", "MINIMUM", "ROOM", "CARTS", "VERDICT")
	fmt.Println(strings.Repeat("─", 84))
	for _, g := range plant.Kanbans {
		for _, f := range v.families(g) {
			var says []string
			if f.carts != f.pool {
				says = append(says, fmt.Sprintf("SEEDS %d OF ITS POOL OF %d", f.carts, f.pool))
			}
			if f.pool < f.minimum {
				says = append(says, fmt.Sprintf("POOL UNDER ITS MINIMUM by %d: the line eats the mix", f.minimum-f.pool))
			}
			if f.room < f.pool {
				says = append(says, fmt.Sprintf("ROOM UNDER ITS POOL by %d: returns cannot all land", f.pool-f.room))
			}
			verdict := "ok"
			if len(says) > 0 {
				ok = false
				verdict = strings.Join(says, "; ")
			}
			fmt.Printf("  %-18s %-12s %-6d %-8d %-6d %-6d %s\n", g.Group, f.family, f.pool, f.minimum, f.room, f.carts, verdict)
		}
	}

	fmt.Printf("\nTHE CLOCK (a %.0f sim-minute run; carts/min drawn against the kanban and its sources)\n", runMins)
	fmt.Println(strings.Repeat("─", 84))
	clock := v.clock(rate)
	for _, c := range clock {
		verdict := "balanced: nothing drains while its returns run"
		if q := plan.pools[c.kanban]; q != nil {
			if deficit := q.emptyDemand - q.emptySupply; deficit > 0.001 {
				if lasts := float64(c.stock) / deficit; lasts < runMins {
					ok = false
					verdict = fmt.Sprintf("RATE DEFICIT %.2f/min: gone at %.0f sim-min, inside the run", deficit, lasts)
				} else {
					verdict = fmt.Sprintf("rate deficit %.2f/min: lasts %.0f sim-min", deficit, lasts)
				}
			}
		}
		fmt.Printf("  %-18s %-12s draws %.2f/min, stock %d → %.0f sim-min if its returns stop; %s\n",
			c.kanban, c.binType, c.spend, c.stock, c.mins, verdict)
	}
	if len(clock) > 0 {
		fmt.Printf("  starves first: %s %s, at %.0f sim-min with its returns stopped\n", clock[0].kanban, clock[0].binType, clock[0].mins)
	}
	return ok
}
