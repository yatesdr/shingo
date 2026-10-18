package domain

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"shingo/protocol"
	"shingoedge/domain/flowspec"
)

// flow_preset_generate_test.go — the generator, table-driven over six cells
// (pick-a-part brief U2): WELD-2 in both its modes, a 4+1 weld cell, PRESS-1,
// PRESS-2 and a two-produce LH/RH press; the fake-Required pin; and the demo
// plant's own cells, read from plants/demo.yaml, as the generator's oracle.

func rn(name, role string, seq int) RoutingNode {
	return RoutingNode{CoreNodeName: name, Role: role, Sequence: seq, Enabled: true}
}

func claimAt(node string, role protocol.ClaimRole) NodeClaim {
	return NodeClaim{CoreNodeName: node, Role: role}
}

func cellsByNode(cells []FlowCell) map[string]FlowCell {
	out := map[string]FlowCell{}
	for _, c := range cells {
		out[c.CoreNodeName] = c
	}
	return out
}

func only(t *testing.T, got []ShapeCandidate, mode protocol.SwapMode) ShapeCandidate {
	t.Helper()
	for _, c := range got {
		if c.Mode == mode {
			return c
		}
	}
	t.Fatalf("no %s candidate; got %d candidates", mode, len(got))
	return ShapeCandidate{}
}

func TestGenerateShapes_SixCells(t *testing.T) {
	market := []RoutingNode{rn("SYN_MARKET", RoutingRoleSource, 0), rn("SYN_MARKET", RoutingRoleDestination, 0)}
	weld2Routing := append(append([]RoutingNode(nil), market...),
		rn("SLN_004", RoutingRoleStaging, 1), rn("SLN_003", RoutingRoleStaging, 0),
		rn("SLN_005", RoutingRoleStaging, 2), rn("SLN_006", RoutingRoleStaging, 3),
		rn("SLN_007", RoutingRoleStaging, 4), rn("SLN_008", RoutingRoleStaging, 5))
	weld2Prior := []NodeClaim{
		claimAt("ALN_003", protocol.ClaimRoleConsume), claimAt("ALN_004", protocol.ClaimRoleConsume),
		claimAt("ALN_005", protocol.ClaimRoleProduce),
	}

	t.Run("WELD-2 single_robot: two lanes per position, by name, in picture order", func(t *testing.T) {
		c := only(t, GenerateShapes([]string{"ALN_003", "ALN_004", "ALN_005"}, weld2Routing, weld2Prior,
			ShapePicks{Mode: protocol.SwapModeSingleRobot}), protocol.SwapModeSingleRobot)
		by := cellsByNode(c.Cells)
		want := map[string][2]string{"ALN_003": {"SLN_003", "SLN_004"}, "ALN_004": {"SLN_005", "SLN_006"}, "ALN_005": {"SLN_007", "SLN_008"}}
		for n, w := range want {
			if got := [2]string{by[n].InboundStaging, by[n].OutboundStaging}; got != w {
				t.Errorf("%s staging = %v, want %v", n, got, w)
			}
		}
		if by["ALN_005"].Role != protocol.ClaimRoleProduce || by["ALN_003"].Role != protocol.ClaimRoleConsume {
			t.Errorf("roles = %s/%s, want the prior claims' consume/produce", by["ALN_003"].Role, by["ALN_005"].Role)
		}
		if len(c.Notes) != 0 {
			t.Errorf("notes = %v, want none (six lanes for six legs)", c.Notes)
		}
	})

	t.Run("WELD-2 two_robot: inbound only, outbound left blank (Forbidden)", func(t *testing.T) {
		c := only(t, GenerateShapes([]string{"ALN_003", "ALN_004", "ALN_005"}, weld2Routing, weld2Prior,
			ShapePicks{}), protocol.SwapModeTwoRobot)
		for _, cell := range c.Cells {
			if cell.InboundStaging == "" || cell.OutboundStaging != "" {
				t.Errorf("%s staging = %q/%q, want inbound only", cell.CoreNodeName, cell.InboundStaging, cell.OutboundStaging)
			}
		}
	})

	t.Run("4+1 weld cell: four consume, one produce, lanes shared with a note", func(t *testing.T) {
		positions := []string{"W_A", "W_B", "W_C", "W_D", "W_OUT"}
		routing := append(append([]RoutingNode(nil), market...),
			rn("STG_1", RoutingRoleStaging, 0), rn("STG_2", RoutingRoleStaging, 1), rn("STG_3", RoutingRoleStaging, 2))
		c := only(t, GenerateShapes(positions, routing, nil, ShapePicks{
			Mode: protocol.SwapModeTwoRobot, Produce: map[string]bool{"W_OUT": true},
		}), protocol.SwapModeTwoRobot)
		if len(c.Cells) != 5 {
			t.Fatalf("%d cells, want 5", len(c.Cells))
		}
		produce := 0
		for _, cell := range c.Cells {
			if cell.Role == protocol.ClaimRoleProduce {
				produce++
			}
		}
		if produce != 1 {
			t.Errorf("%d produce cells, want 1", produce)
		}
		if len(c.Notes) != 1 || !strings.Contains(c.Notes[0], "shares 2 staging lanes") {
			t.Errorf("notes = %v, want one saying the pairing shares 2 lanes", c.Notes)
		}
	})

	t.Run("4+1 weld cell with no claims and no Produce pick asks the role", func(t *testing.T) {
		c := only(t, GenerateShapes([]string{"W_A", "W_OUT"}, append(market, rn("STG_1", RoutingRoleStaging, 0)), nil,
			ShapePicks{Mode: protocol.SwapModeTwoRobot}), protocol.SwapModeTwoRobot)
		if len(c.Notes) == 0 || !strings.Contains(c.Notes[0], "which positions make parts") {
			t.Errorf("notes = %v, want the role question", c.Notes)
		}
	})

	pressRouting := []RoutingNode{rn("SYN_PRESS_EMPTIES", RoutingRoleSource, 0),
		rn("SYN_MARKET", RoutingRoleDestination, 0), rn("SYN_CLEAR", RoutingRoleDestination, 1)}

	t.Run("PRESS-1 press-index: one row, the back as partner", func(t *testing.T) {
		c := only(t, GenerateShapes([]string{"PLN_001", "PLN_002"}, pressRouting,
			[]NodeClaim{claimAt("PLN_001", protocol.ClaimRoleProduce)}, ShapePicks{}), protocol.SwapModeTwoRobotPressIndex)
		if len(c.Cells) != 1 || c.Cells[0].CoreNodeName != "PLN_001" || c.Cells[0].PairedCoreNode != "PLN_002" {
			t.Fatalf("cells = %+v, want one row on PLN_001 paired with PLN_002", c.Cells)
		}
		if c.Cells[0].OutboundDestination != "SYN_MARKET" || c.Cells[0].InboundSource != "SYN_PRESS_EMPTIES" {
			t.Errorf("source/destination = %q/%q, want the lowest rows of each role", c.Cells[0].InboundSource, c.Cells[0].OutboundDestination)
		}
	})

	t.Run("PRESS-2 sequential: two mutual rows", func(t *testing.T) {
		c := only(t, GenerateShapes([]string{"PLN_003", "PLN_004"}, pressRouting,
			[]NodeClaim{claimAt("PLN_003", protocol.ClaimRoleProduce), claimAt("PLN_004", protocol.ClaimRoleProduce)},
			ShapePicks{}), protocol.SwapModeSequential)
		by := cellsByNode(c.Cells)
		if len(c.Cells) != 2 || by["PLN_003"].PairedCoreNode != "PLN_004" || by["PLN_004"].PairedCoreNode != "PLN_003" {
			t.Fatalf("cells = %+v, want PLN_003↔PLN_004", c.Cells)
		}
	})

	t.Run("LH/RH press: two press-index pairs, both produce — two produce slots", func(t *testing.T) {
		c := only(t, GenerateShapes([]string{"P_LH", "P_LH_B", "P_RH", "P_RH_B"}, pressRouting,
			[]NodeClaim{claimAt("P_LH", protocol.ClaimRoleProduce), claimAt("P_RH", protocol.ClaimRoleProduce)},
			ShapePicks{}), protocol.SwapModeTwoRobotPressIndex)
		by := cellsByNode(c.Cells)
		if len(c.Cells) != 2 || by["P_LH"].PairedCoreNode != "P_LH_B" || by["P_RH"].PairedCoreNode != "P_RH_B" {
			t.Fatalf("cells = %+v, want LH and RH rows each paired with its back", c.Cells)
		}
		for _, cell := range c.Cells {
			if cell.Role != protocol.ClaimRoleProduce {
				t.Errorf("%s role %s, want produce", cell.CoreNodeName, cell.Role)
			}
		}
	})
}

// TestGenerateShapes_ARequiredFieldNobodyFillsIsNotOffered: the table decides,
// not a mode name. A fake Required field the generator has no way to fill
// takes the mode off the offer.
func TestGenerateShapes_ARequiredFieldNobodyFillsIsNotOffered(t *testing.T) {
	positions := []string{"ALN_003"}
	routing := []RoutingNode{rn("SYN_MARKET", RoutingRoleSource, 0), rn("SYN_MARKET", RoutingRoleDestination, 0), rn("SLN_003", RoutingRoleStaging, 0)}
	before := GenerateShapes(positions, routing, nil, ShapePicks{Mode: protocol.SwapModeTwoRobot})
	if len(before) != 1 {
		t.Fatalf("two_robot is not offered on a cell that satisfies it: %d candidates", len(before))
	}
	real := steadyFor
	t.Cleanup(func() { steadyFor = real })
	steadyFor = func(role protocol.ClaimRole, mode protocol.SwapMode) map[flowspec.Field]flowspec.Need {
		row := real(role, mode)
		if mode == protocol.SwapModeTwoRobot {
			row[flowspec.Field("a_field_nobody_fills")] = flowspec.Required
			row[flowspec.ChangeoverEvacDestination] = flowspec.Required
		}
		return row
	}
	if got := GenerateShapes(positions, routing, nil, ShapePicks{Mode: protocol.SwapModeTwoRobot}); len(got) != 0 {
		t.Fatalf("two_robot is still offered with a Required field nothing fills: %+v", got)
	}
}

// ── the demo plant as the oracle ─────────────────────────────────────────

type demoClaim struct {
	CoreNode                  string   `yaml:"core_node"`
	Style                     string   `yaml:"style"`
	Role                      string   `yaml:"role"`
	SwapMode                  string   `yaml:"swap_mode"`
	PairedCoreNode            string   `yaml:"paired_core_node"`
	InboundSource             string   `yaml:"inbound_source"`
	OutboundDestination       string   `yaml:"outbound_destination"`
	InboundStaging            string   `yaml:"inbound_staging"`
	OutboundStaging           string   `yaml:"outbound_staging"`
	ChangeoverEvacDestination string   `yaml:"changeover_evac_destination"`
	ChangeoverEvacNodes       []string `yaml:"changeover_evac_nodes"`
	PartialDestination        string   `yaml:"partial_destination"`
	KeyRoute                  []string `yaml:"key_route"`
}

type demoPlant struct {
	Styles []struct {
		Name    string `yaml:"name"`
		Process string `yaml:"process"`
	} `yaml:"styles"`
	Claims []demoClaim `yaml:"claims"`
}

// TestGenerateShapes_ReproducesEveryDemoCell: for every demo cell, the
// generator — given the engineer's pairing and any Used staging the style
// picked — produces exactly the shape the plant runs, with Forbidden fields
// cleared as the editor clears them on save. A style carrying fields a recipe
// generator never authors (evacuation marks, an evac override, a partial
// destination, a key route) is skipped; each cell must compare at least one.
// Read from plants/demo.yaml, as the seeder does, so the oracle is the plant.
func TestGenerateShapes_ReproducesEveryDemoCell(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "plants", "demo.yaml"))
	if err != nil {
		t.Fatalf("read plants/demo.yaml: %v", err)
	}
	var p demoPlant
	if err := yaml.Unmarshal(raw, &p); err != nil {
		t.Fatalf("parse plants/demo.yaml: %v", err)
	}
	styleProc := map[string]string{}
	for _, s := range p.Styles {
		styleProc[s.Name] = s.Process
	}
	byProc := map[string][]demoClaim{}
	var procs []string
	for _, c := range p.Claims {
		if c.SwapMode == string(protocol.SwapModeManualSwap) {
			continue
		}
		proc := styleProc[c.Style]
		if _, ok := byProc[proc]; !ok {
			procs = append(procs, proc)
		}
		byProc[proc] = append(byProc[proc], c)
	}
	if len(procs) < 6 {
		t.Fatalf("demo.yaml gave %d non-loader cells; this oracle was written against at least six", len(procs))
	}

	for _, proc := range procs {
		claims := byProc[proc]
		// Positions as the seeder makes them: one per claimed node, sequence
		// 0, so picture order is by name.
		posSet := map[string]bool{}
		var positions []string
		for _, c := range claims {
			if !posSet[c.CoreNode] {
				posSet[c.CoreNode] = true
				positions = append(positions, c.CoreNode)
			}
		}
		sort.Strings(positions)
		// The routing set as the seeder writes it: every leg, in first-seen
		// order per role, minus positions.
		var routing []RoutingNode
		seq := map[string]int{}
		seen := map[string]bool{}
		add := func(name, role string) {
			if name == "" || posSet[name] || seen[role+name] {
				return
			}
			seen[role+name] = true
			routing = append(routing, rn(name, role, seq[role]))
			seq[role]++
		}
		var prior []NodeClaim
		for _, c := range claims {
			add(c.InboundSource, RoutingRoleSource)
			add(c.OutboundDestination, RoutingRoleDestination)
			add(c.ChangeoverEvacDestination, RoutingRoleDestination)
			add(c.InboundStaging, RoutingRoleStaging)
			add(c.OutboundStaging, RoutingRoleStaging)
			prior = append(prior, claimAt(c.CoreNode, protocol.ClaimRole(c.Role)))
		}

		byStyle := map[string][]demoClaim{}
		var styles []string
		for _, c := range claims {
			if _, ok := byStyle[c.Style]; !ok {
				styles = append(styles, c.Style)
			}
			byStyle[c.Style] = append(byStyle[c.Style], c)
		}
		compared := 0
		for _, style := range styles {
			sc := byStyle[style]
			authored := false
			picks := ShapePicks{Mode: protocol.SwapMode(sc[0].SwapMode), Pairs: map[string]string{}, Staging: map[string][2]string{}}
			var want []FlowCell
			for _, c := range sc {
				if c.ChangeoverEvacDestination != "" || len(c.ChangeoverEvacNodes) > 0 || c.PartialDestination != "" || len(c.KeyRoute) > 0 {
					authored = true
				}
				if c.PairedCoreNode != "" {
					picks.Pairs[c.CoreNode] = c.PairedCoreNode
				}
				need := flowspec.Steady(protocol.ClaimRole(c.Role), protocol.SwapMode(c.SwapMode))
				cell := FlowCell{
					CoreNodeName: c.CoreNode, Role: protocol.ClaimRole(c.Role), SwapMode: protocol.SwapMode(c.SwapMode),
					PairedCoreNode: c.PairedCoreNode, InboundSource: c.InboundSource, OutboundDestination: c.OutboundDestination,
					InboundStaging: c.InboundStaging, OutboundStaging: c.OutboundStaging,
				}
				// The editor clears a Forbidden field on save; that is the
				// shape this flow saves as.
				if need[flowspec.InboundStaging] == flowspec.Forbidden {
					cell.InboundStaging = ""
				}
				if need[flowspec.OutboundStaging] == flowspec.Forbidden {
					cell.OutboundStaging = ""
				}
				// A Used staging lane is the engineer's pick, never a proposal.
				var st [2]string
				if need[flowspec.InboundStaging] == flowspec.Used {
					st[0] = cell.InboundStaging
				}
				if need[flowspec.OutboundStaging] == flowspec.Used {
					st[1] = cell.OutboundStaging
				}
				picks.Staging[c.CoreNode] = st
				want = append(want, cell)
			}
			if authored {
				continue
			}
			got := GenerateShapes(positions, routing, prior, picks)
			if len(got) != 1 {
				t.Errorf("%s/%s: %d candidates for %s, want 1", proc, style, len(got), picks.Mode)
				continue
			}
			if FlowShapeKey(got[0].Cells) != FlowShapeKey(want) {
				t.Errorf("%s/%s: generated\n  %+v\nwant (the plant's own shape)\n  %+v", proc, style, got[0].Cells, want)
			}
			compared++
		}
		if compared == 0 {
			t.Errorf("%s: no style of this cell was compared — every one carries a field the generator does not author", proc)
		}
	}
}
