package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	_ "modernc.org/sqlite"

	"shingocore/plantspec"
)

// kanban_press_fixture_test.go — the arithmetic at the head of the master's
// [kanban] section, against plants/master.yaml, so neither can drift from the
// other. It reads the shipped file for the reason edge2_fixture_test.go gives.

// kanbanPressPlant is the whole master: the kanban family's docker runs seed all
// of it, so the press's loop runs beside every other section's.
func kanbanPressPlant(t *testing.T) *plantspec.Plant {
	t.Helper()
	p, err := plantspec.Load(filepath.Join("..", "..", "..", "plants", "master.yaml"))
	if err != nil {
		t.Fatalf("load plants/master.yaml: %v", err)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("validate plants/master.yaml: %v", err)
	}
	return p
}

// kanbanZones and kanbanCells are the [kanban] section's storage and cells.
var (
	kanbanZones = map[string]bool{"PRESS-KANBAN": true, "WELD-KANBAN": true, "KP-SM": true, "PRESS-UNLOAD": true, "WELD-UNLOAD": true}
	kanbanCells = map[string]bool{"PLN_021": true, "PLN_022": true, "PLN_023": true, "PLN_024": true, "ALN_021": true, "SLN_021": true}
)

// kanbanPressGroupOf is every position's zone.
func kanbanPressGroupOf(p *plantspec.Plant) map[string]string {
	out := map[string]string{}
	for _, z := range p.Zones {
		for _, s := range z.Positions {
			out[s.Name] = z.Name
		}
		for _, l := range z.Lanes {
			for _, s := range l.Slots {
				out[s.Name] = z.Name
			}
		}
	}
	return out
}

// TestKanbanPressFixture_ThePool: every carrier where the file's head says: 10 of 45x48
// (K's 2/2/2 and the 4 positions), 6 of 32x30 in K, the weld's own 3 and nothing in the
// plain supermarket, because a keeper with no sources would pull it in at once. K's small
// is seated at 2 of its want of 6: its cover is the carts on the positions.
func TestKanbanPressFixture_ThePool(t *testing.T) {
	t.Parallel()
	p := kanbanPressPlant(t)
	groupOf := kanbanPressGroupOf(p)

	type place struct{ where, binType string }
	got := map[place]int{}
	for _, b := range p.Bins {
		if !kanbanZones[groupOf[b.Slot]] && !kanbanCells[b.Slot] {
			continue // another section's cart
		}
		if b.Payload != "" {
			t.Errorf("bin %s is seeded full; the rig's loop starts on empties", b.Name)
		}
		if b.BinType == "" {
			t.Errorf("bin %s names no type and would fall back to the first one", b.Name)
		}
		where := groupOf[b.Slot]
		if where == "" {
			where = b.Slot
		}
		got[place{where, b.BinType}]++
	}
	want := map[place]int{
		{"PRESS-KANBAN", "SMALL-45"}: 2, {"PRESS-KANBAN", "BIG-45"}: 2, {"PRESS-KANBAN", "TOTES-45"}: 2,
		{"PRESS-KANBAN", "CART-32"}: 6,
		{"PLN_021", "SMALL-45"}:     1, {"PLN_022", "SMALL-45"}: 1, {"PLN_023", "SMALL-45"}: 1, {"PLN_024", "SMALL-45"}: 1,
		{"WELD-KANBAN", "SMALL-45"}: 2, {"ALN_021", "SMALL-45"}: 1,
		{"KP-SM", "SMALL-45"}: 2, {"KP-SM", "BIG-45"}: 2,
	}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("%s holds %d %s, the file's head says %d", k.where, got[k], k.binType, n)
		}
	}
	for k, n := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("%s holds %d %s, which the file's head does not count", k.where, n, k.binType)
		}
	}

	// Each seated cart stands on a slot its type is allowed, by the table the driver and
	// the docker tests apply, and K has the sixteen slots the table names.
	slots := map[string]int{}
	for _, z := range p.Zones {
		slots[z.Name] = len(z.Positions)
	}
	for group, n := range map[string]int{"PRESS-KANBAN": 16, "PRESS-UNLOAD": 4} {
		if slots[group] != n {
			t.Errorf("%s has %d slots, the file's head says %d", group, slots[group], n)
		}
	}
	table := rigSlotTypes()
	if len(table) != 16 {
		t.Errorf("the slot table names %d slots, want 16", len(table))
	}
	for _, b := range p.Bins {
		if groupOf[b.Slot] != "PRESS-KANBAN" {
			continue
		}
		if !slices.Contains(table[b.Slot], b.BinType) {
			t.Errorf("%s stands on %s, which allows %v, not %s", b.Name, b.Slot, table[b.Slot], b.BinType)
		}
	}
}

// rigSlotTypes is what each of K's slots allows, the table plantspec.Slot cannot state:
// the master's [kanban] section states the same in each slot's admits.
func rigSlotTypes() map[string][]string {
	out := map[string][]string{}
	for i := 1; i <= 16; i++ {
		types := []string{"SMALL-45", "BIG-45", "TOTES-45"}
		if i > 10 {
			types = []string{"CART-32"}
		}
		out[fmt.Sprintf("PKB_%03d", i)] = types
	}
	return out
}

// devSim is the slice of shingoedge.dev.yaml the rig reads.
type devSim struct {
	Sim struct {
		Processes []struct {
			PLCName string `yaml:"plc_name"`
			TagName string `yaml:"tag_name"`
		} `yaml:"processes"`
		Operators struct {
			UnloaderTaps []struct {
				Station string `yaml:"station"`
				Tap     string `yaml:"tap"`
			} `yaml:"unloader_taps"`
		} `yaml:"operators"`
	} `yaml:"sim"`
}

// TestKanbanPressFixture_DevConfigMatches: a reporting point whose counter the fake
// PLC of its process's Edge does not tick never counts, with no error anywhere, and a
// tap setting on a station the plant does not build keeps the plain CLEAR, so the rig
// would run and light nothing. On the plant's own Edge plc_name is the process name.
func TestKanbanPressFixture_DevConfigMatches(t *testing.T) {
	t.Parallel()
	p := kanbanPressPlant(t)
	read := func(name string) devSim {
		raw, err := os.ReadFile(filepath.Join("..", "..", "..", "shingo-edge", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		var cfg devSim
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		return cfg
	}
	cfg := read("shingoedge.dev.yaml")
	configOf := map[string]string{p.Station(): "shingoedge.dev.yaml", "edge2.line1": "shingoedge.edge2.yaml"}
	ticks := map[string]map[string]bool{}
	for station, file := range configOf {
		ticks[station] = map[string]bool{}
		for _, pr := range read(file).Sim.Processes {
			ticks[station][pr.PLCName+"/"+pr.TagName] = true
		}
	}
	processOf := map[string]string{}
	for _, s := range p.Styles {
		processOf[s.Name] = s.Process
	}
	for _, rp := range p.ReportingPoints {
		edge := p.EdgeOf(processOf[rp.Style])
		if !ticks[edge][rp.PLCName+"/"+rp.TagName] {
			t.Errorf("reporting point %s/%s has no tick in %s", rp.PLCName, rp.TagName, configOf[edge])
		}
		if edge == p.Station() && rp.PLCName != processOf[rp.Style] {
			t.Errorf("reporting point %s is on process %s; the dev stack names a PLC for its process", rp.PLCName, processOf[rp.Style])
		}
	}
	// The unloaders' windows: a produce loader's windows (PLK_LOADER's) are tapped by
	// no one.
	windowStations := map[string]bool{}
	for _, c := range p.Claims {
		if c.WindowOf != "" && c.Role == "consume" {
			windowStations[c.OperatorStation] = true
		}
	}
	taps := map[string]string{}
	for _, tap := range cfg.Sim.Operators.UnloaderTaps {
		taps[tap.Station] = tap.Tap
		if !windowStations[tap.Station] {
			t.Errorf("shingoedge.dev.yaml sets a tap on %s, which holds no window of this plant", tap.Station)
		}
	}
	for station := range windowStations {
		if taps[station] != "lit" {
			t.Errorf("unloader station %s: tap %q in shingoedge.dev.yaml, want lit", station, taps[station])
		}
	}
}

// TestSeedKanbanPress_EdgeBuildsTheBoards: what the Edge half seeds for the rig: each
// unloader's station holds only its windows, which is what draws its board as kind
// grid (operator-board-model.js boardLayout), the windows carry no claim row (Core
// owns a loader), and both cells count, on their one counter.
func TestSeedKanbanPress_EdgeBuildsTheBoards(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "edge.db"))
	if err != nil {
		t.Fatalf("open edge db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(edgeDDL); err != nil {
		t.Fatalf("apply edge DDL: %v", err)
	}
	plant := kanbanPressPlant(t)
	if err := seedEdgeInTx(t, db, plant.ForEdge(plant.Station())); err != nil {
		t.Fatalf("seedEdgeDB: %v", err)
	}

	onStation := func(code string) []string {
		rows, err := db.Query(`SELECT pn.core_node_name FROM process_nodes pn
			JOIN operator_stations s ON s.id = pn.operator_station_id
			WHERE s.code = ? ORDER BY pn.core_node_name`, code)
		if err != nil {
			t.Fatalf("nodes on %s: %v", code, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, n)
		}
		return out
	}
	if got := strings.Join(onStation("KP-UNLOAD-OPS"), ","); got != "ULN_021,ULN_022,ULN_023,ULN_024" {
		t.Errorf("KP-UNLOAD-OPS holds %s, want its four windows and nothing else", got)
	}
	if got := strings.Join(onStation("KP-PRESS-OPS"), ","); got != "PLN_021,PLN_022,PLN_023,PLN_024" {
		t.Errorf("KP-PRESS-OPS holds %s, want the four positions", got)
	}

	var windowClaims, pressClaims int
	if err := db.QueryRow(`SELECT count(*) FROM style_node_claims WHERE core_node_name LIKE 'ULN_%'`).Scan(&windowClaims); err != nil {
		t.Fatalf("count window claims: %v", err)
	}
	if windowClaims != 0 {
		t.Errorf("%d claim rows on the windows; Core owns a loader", windowClaims)
	}
	if err := db.QueryRow(`SELECT count(*) FROM style_node_claims WHERE core_node_name IN ('PLN_021', 'PLN_022', 'PLN_023', 'PLN_024')`).Scan(&pressClaims); err != nil {
		t.Fatalf("count press claims: %v", err)
	}
	if pressClaims != 10 {
		t.Errorf("press claims = %d, want two per style on five styles", pressClaims)
	}

	for proc, want := range map[string]string{"KP-PRESS": "KP-PRESS_COUNTER", "KW-WELD": "KW-WELD_COUNTER"} {
		var tag string
		var enabled int
		if err := db.QueryRow(`SELECT counter_tag_name, counter_enabled FROM processes WHERE name = ?`, proc).Scan(&tag, &enabled); err != nil {
			t.Fatalf("process %s: %v", proc, err)
		}
		if tag != want || enabled != 1 {
			t.Errorf("process %s counter = %q enabled %d, want %q", proc, tag, enabled, want)
		}
	}
}
