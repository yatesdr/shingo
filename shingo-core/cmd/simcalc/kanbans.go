package main

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"shingocore/plantspec"
)

// kanbans.go — what the carrier check needs to know about a kanban: its want is
// not its level, it borrows from named groups, and leaving a carrier's floor
// leaves carts with no level to land on.
//
// THE WANT IS CORE'S: level + cover × the distinct supported processes running
// the type (store.KanbanPictureSQL, kanban.Level.Want). Read off the plant file
// here: a process runs its style's produce parts, and a part counts only with a
// carrier (a part declared no_bin_type contributes nothing).

// kanbanLine is one (kanban, type) under one choice of running styles.
type kanbanLine struct {
	binType string
	base    int
	cover   int
	running int
	want    int
	seeded  int
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
	floorOf    map[string]string            // type → floor key
	zoneSlots  map[string]int               // zone → its slots, positions and lanes
}

func newKanbanView(plant *plantspec.Plant) *kanbanView {
	v := &kanbanView{
		plant: plant, binTypeOf: map[string]string{}, capOf: map[string]int64{},
		seeded: map[string]map[string]int{}, stylesOf: map[string][]string{}, styleParts: map[string][]string{},
		claimsOf: map[string][]plantspec.Claim{}, nodesOf: map[string][]string{}, cartsAt: map[string]string{},
		floorOf: map[string]string{}, zoneSlots: map[string]int{},
	}
	for _, pl := range plant.Payloads {
		v.capOf[pl.Code] = pl.UOPCapacity
		if pl.BinType != "" {
			v.binTypeOf[pl.Code] = pl.BinType
		}
	}
	for _, bt := range plant.BinTypes {
		// A type with no size is its own floor: nothing else is known to share it.
		v.floorOf[bt] = bt
		if s, ok := plant.CarrierSizes[bt]; ok {
			v.floorOf[bt] = fmt.Sprintf("%gx%g", min(s.WidthIn, s.LengthIn), max(s.WidthIn, s.LengthIn))
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
		for _, n := range []string{c.CoreNode, c.PairedCoreNode, c.SecondPairedCoreNode} {
			if n != "" {
				v.nodesOf[proc] = appendUniq(v.nodesOf[proc], n)
			}
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

// styleTypes is the carrier types a style runs. Empty for a style whose parts all
// have no carrier.
func (v *kanbanView) styleTypes(style string) []string {
	var out []string
	for _, part := range v.styleParts[style] {
		if bt := v.binTypeOf[part]; bt != "" {
			out = appendUniq(out, bt)
		}
	}
	return out
}

// lines is a kanban's levels under the running styles given (process → style).
func (v *kanbanView) lines(g plantspec.MaintainedGroup, running map[string]string) []kanbanLine {
	out := make([]kanbanLine, 0, len(g.Levels))
	for _, lv := range g.Levels {
		n := 0
		for _, proc := range g.Supports {
			for _, bt := range v.styleTypes(running[proc]) {
				if bt == lv.BinType {
					n++
					break
				}
			}
		}
		out = append(out, kanbanLine{binType: lv.BinType, base: lv.Want, cover: g.RunningCover, running: n,
			want: lv.Want + g.RunningCover*n, seeded: v.seeded[g.Group][lv.BinType]})
	}
	return out
}

func (v *kanbanView) groupNamed(name string) (plantspec.MaintainedGroup, bool) {
	for _, g := range v.plant.MaintainedGroups {
		if g.Group == name {
			return g, true
		}
	}
	return plantspec.MaintainedGroup{}, false
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
	for _, g := range v.plant.MaintainedGroups {
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

// roomLine is one (style, floor) of a kanban's loop: the carts on that floor and
// the wants that could hold them while the style runs.
type roomLine struct {
	style     string
	floor     string
	carts     int      // in the kanban, its sources and at the supported positions
	lineside  int      // the kanban's wants on this floor + the positions the style fills
	levels    int      // lineside + the sources' levels on this floor
	spare     int      // the slots of the plain groups this floor overflows to
	spareZone []string // those groups
	toSpare   int      // carts past the levels that land there
	excess    int      // carts past the levels and the spare room: they park, on a window
	short     int      // lineside − carts, above 0: the kanban's want cannot be covered
}

// plainOverflow is where a kanban's types on a floor overflow to that holds no
// level: a store there is refused by nothing but its slots, so they are room for
// carts no level wants. An overflow with a level is not counted: it refuses at
// that level (TestOverflow_AFullOverflowDoesNotChain), and one hop ends there.
func (v *kanbanView) plainOverflow(g plantspec.MaintainedGroup, floor string) (zones []string, slots int) {
	for _, lv := range g.Levels {
		if v.floorOf[lv.BinType] != floor {
			continue
		}
		target := lv.Overflow
		if target == "" {
			target = g.Overflow
		}
		if _, levelled := v.groupNamed(target); target == "" || levelled || slices.Contains(zones, target) {
			continue
		}
		zones = append(zones, target)
		slots += v.zoneSlots[target]
	}
	return zones, slots
}

// changeoverRoom walks every style of a kanban's supported processes. A style
// whose parts have no carrier is left out: it changes no want.
func (v *kanbanView) changeoverRoom(g plantspec.MaintainedGroup) []roomLine {
	var out []roomLine
	active := v.activeStyles()

	cartsByFloor := map[string]int{}
	for _, grp := range append([]string{g.Group}, g.Sources...) {
		for bt, n := range v.seeded[grp] {
			cartsByFloor[v.floorOf[bt]] += n
		}
	}
	positions := 0
	for _, proc := range g.Supports {
		for _, n := range v.nodesOf[proc] {
			if bt, ok := v.cartsAt[n]; ok {
				cartsByFloor[v.floorOf[bt]]++
			}
			positions++
		}
	}
	floors := make([]string, 0, len(cartsByFloor))
	for f := range cartsByFloor {
		floors = append(floors, f)
	}
	sort.Strings(floors)

	for _, proc := range g.Supports {
		for _, style := range v.stylesOf[proc] {
			types := v.styleTypes(style)
			if len(types) == 0 {
				continue
			}
			running := map[string]string{}
			for k, s := range active {
				running[k] = s
			}
			running[proc] = style
			for _, f := range floors {
				r := roomLine{style: style, floor: f, carts: cartsByFloor[f]}
				for _, l := range v.lines(g, running) {
					if v.floorOf[l.binType] == f {
						r.lineside += l.want
					}
				}
				for _, bt := range types {
					if v.floorOf[bt] == f {
						r.lineside += positions
						break
					}
				}
				r.levels = r.lineside
				for _, src := range g.Sources {
					if sg, ok := v.groupNamed(src); ok {
						for _, l := range v.lines(sg, running) {
							if v.floorOf[l.binType] == f {
								r.levels += l.want
							}
						}
					}
				}
				r.spareZone, r.spare = v.plainOverflow(g, f)
				past := max(0, r.carts-r.levels)
				r.toSpare = min(past, r.spare)
				r.excess = past - r.toSpare
				r.short = max(0, r.lineside-r.carts)
				out = append(out, r)
			}
		}
	}
	return out
}

// reportKanbans prints the kanbans' wants, THE CLOCK and CHANGEOVER ROOM. It
// fails the check when a kanban starts below its want, or when a pool's rate
// deficit drains its stock inside the run. The room table is a statement about a
// changeover, an event of a run rather than its steady state, so it reports and
// does not fail the command.
func reportKanbans(plant *plantspec.Plant, plan carrierPlan, rate map[string]float64, runMins float64) bool {
	if len(plant.MaintainedGroups) == 0 {
		return true
	}
	ok := true
	v := newKanbanView(plant)
	active := v.activeStyles()

	fmt.Printf("\nKANBANS (want = level + cover × supported processes running the type)\n")
	fmt.Printf("  %-18s %-12s %-6s %-6s %-8s %-6s %-7s %s\n", "KANBAN", "TYPE", "LEVEL", "COVER", "RUNNING", "WANT", "SEEDED", "VERDICT")
	fmt.Println(strings.Repeat("─", 84))
	for _, g := range plant.MaintainedGroups {
		for _, l := range v.lines(g, active) {
			verdict := "ok"
			if l.seeded < l.want {
				ok = false
				verdict = fmt.Sprintf("BELOW WANT by %d", l.want-l.seeded)
			}
			fmt.Printf("  %-18s %-12s %-6d %-6d %-8d %-6d %-7d %s\n", g.Group, l.binType, l.base, l.cover, l.running, l.want, l.seeded, verdict)
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

	printed := false
	for _, g := range plant.MaintainedGroups {
		if len(g.Supports) == 0 {
			continue
		}
		rows := v.changeoverRoom(g)
		if !printed && len(rows) > 0 {
			printed = true
			fmt.Printf("\nCHANGEOVER ROOM (CARTS in the kanban, its sources and the positions; LINESIDE = kanban's wants +\n")
			fmt.Printf("positions; LEVELS = LINESIDE + the sources' levels; SPARE = the slots of a plain overflow, a group\n")
			fmt.Printf("with no level, counted as room. Reports; a changeover is an event, not the steady state)\n")
			fmt.Printf("  %-18s %-12s %-8s %-6s %-9s %-7s %-6s %s\n", "KANBAN", "STYLE", "FLOOR", "CARTS", "LINESIDE", "LEVELS", "SPARE", "SAYS")
			fmt.Println(strings.Repeat("─", 84))
		}
		for _, r := range rows {
			var says []string
			if r.toSpare > 0 {
				says = append(says, fmt.Sprintf("%d carts past the levels land in %s", r.toSpare, strings.Join(r.spareZone, ",")))
			}
			if r.excess > 0 {
				says = append(says, fmt.Sprintf("%d carts with nowhere to land: they park on the windows", r.excess))
			}
			if r.short > 0 {
				says = append(says, fmt.Sprintf("kanban short %d: cannot be covered, cover over the pool and not a defect", r.short))
			}
			if len(says) == 0 {
				says = append(says, "fits")
			}
			fmt.Printf("  %-18s %-12s %-8s %-6d %-9d %-7d %-6d %s\n", g.Group, r.style, r.floor, r.carts, r.lineside, r.levels, r.spare, strings.Join(says, "; "))
		}
	}
	return ok
}
