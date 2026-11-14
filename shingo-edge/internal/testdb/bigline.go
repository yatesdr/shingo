package testdb

import (
	"fmt"
	"strings"

	"shingo/protocol"
	"shingo/shared/scenefixtures"
)

// bigline.go — a big line for tests that must be checked at a real press's
// size: 23 positions on two screens, 108 plant nodes with four groups, 46
// parts and 13 styles. It is Press 6's shape at Springfield with every name
// invented (BIG-PRESS, BPN_*, BUN_*, BSM_*, BP-PART-*).
//
// WHY. The Processes page's 23-row wall shipped because every pin was taken
// on a line of four to twelve positions. A slice of the one-process-page build
// is checked on this line before and after.
//
// THE NODE HARNESS HAS ITS TWIN. www/testdata/process-setup-harness/bigline.js
// builds the same line for the page pins under node (same names, same
// counts); a change here is made there too. The recipes (3, in 12 versions),
// the two light curtains, the part set and the one Next carrier are not in
// this plant: the shots harness adds them through the real API, so the real
// handlers validate them.

// BigLineMain is the main screen's 21 positions; BigLineUnloader the unloader
// screen's two. Their order is the plant's sequence.
var (
	BigLineMain = []string{"BPN_008", "BPN_005", "BPN_006BCD", "BPN_006EFG", "BPN_006HIJ", "BPN_006KLM", "BPN_006NOP",
		"BPN_007BCD", "BPN_007EFG", "BPN_007HIJ", "BPN_007KLM", "BPN_007NOP", "BPN_009BCD", "BPN_009EFG",
		"BPN_009HIJ", "BPN_009KLM", "BPN_009NOP", "BPN_006PAL", "BPN_005PAL", "BPN_008PAL", "BPN_009PAL"}
	BigLineUnloader = []string{"BUN_002", "BUN_005"}
	// BigLineCurtains are the two positions the shots give a light curtain.
	BigLineCurtains = []string{"BPN_006PAL", "BPN_009PAL"}
)

// The line's two groups, as its claims name them.
const (
	BigLineSource      = "BIG Buffer Zone"
	BigLineDestination = "BIG Unloader Zone"
)

// BigLineParts is the 46 parts the line may run.
func BigLineParts() []string {
	out := make([]string, 46)
	for i := range out {
		out[i] = fmt.Sprintf("BP-PART-%02d", i+1)
	}
	return out
}

// BigLinePlant is the big line as a scenefixtures plant, for SeedPlant with
// process name "BIG-PRESS". Style 1 runs.
func BigLinePlant() scenefixtures.Plant {
	i64 := func(n int64) *int64 { return &n }
	parts := BigLineParts()
	p := scenefixtures.Plant{Plant: "bigline", Note: "Press 6's shape, every name invented"}
	p.Processes = []scenefixtures.Process{{ID: 1, Name: "BIG-PRESS", ActiveStyleID: i64(1)}}
	p.OperatorStations = []scenefixtures.OperatorStation{
		{ID: 61, ProcessID: 1, Code: "big-press", Name: "BIG-PRESS Operator Screen"},
		{ID: 62, ProcessID: 1, Code: "big-unl", Name: "BIG Unloader"},
	}
	seq := 0
	for _, list := range []struct {
		station int64
		names   []string
	}{{61, BigLineMain}, {62, BigLineUnloader}} {
		for _, n := range list.names {
			seq++
			st := list.station
			p.ProcessNodes = append(p.ProcessNodes, scenefixtures.ProcessNode{
				ID: int64(500 + seq), ProcessID: 1, OperatorStationID: &st, CoreNodeName: n, Code: strings.ToLower(n),
				Name: n, Sequence: seq, Enabled: 1,
			})
		}
	}
	bigLineStyles(&p, parts)
	bigLineNodes(&p)
	for i, code := range parts {
		p.PayloadCatalog = append(p.PayloadCatalog, scenefixtures.CatalogEntry{ID: int64(i + 1), Code: code, Name: "Synthetic " + code, UOPCapacity: 200})
		p.Payloads = append(p.Payloads, scenefixtures.Payload{ID: int64(i + 1), Code: code})
		p.PayloadBinTypes = append(p.PayloadBinTypes, scenefixtures.PayloadBinType{PayloadID: int64(i + 1), BinTypeID: 1, BinTypeCode: "BIG-CART"})
	}
	return p
}

// bigLineStyles adds the line's 13 styles and their claims.
func bigLineStyles(p *scenefixtures.Plant, parts []string) {
	// 13 styles. Style 1 works BPN_007KLM decked by BPN_008; the others each
	// work one press position, every fourth a second one beside it.
	var press []string
	for _, n := range BigLineMain {
		if n != "BPN_008" && n != "BPN_005" {
			press = append(press, n)
		}
	}
	deckOf := func(n string) string {
		if strings.HasPrefix(n, "BPN_009") {
			return "BPN_008"
		}
		return "BPN_005"
	}
	claimID := int64(900)
	for i := 0; i < 13; i++ {
		node := press[i%len(press)]
		if i == 0 {
			node = "BPN_007KLM"
		}
		deck := deckOf(node)
		if i == 0 {
			deck = "BPN_008"
		}
		part := parts[(i*3)%len(parts)]
		name := part
		nodes := [][2]string{{node, deck}}
		if i%4 == 1 && strings.HasPrefix(node, "BPN_006") {
			nodes = append(nodes, [2]string{strings.Replace(node, "BPN_006", "BPN_009", 1), "BPN_008"})
			name += " (two spots)"
		}
		p.Styles = append(p.Styles, scenefixtures.Style{ID: int64(i + 1), ProcessID: 1, Name: name})
		for _, nd := range nodes {
			claimID++
			p.StyleNodeClaims = append(p.StyleNodeClaims, scenefixtures.StyleNodeClaim{
				ID: claimID, StyleID: int64(i + 1), CoreNodeName: nd[0], Role: "produce", SwapMode: string(protocol.SwapModeTwoRobotPressIndex),
				PayloadCode: part, PairedCoreNode: nd[1], InboundSource: BigLineSource, OutboundDestination: BigLineDestination,
			})
		}
	}
}

// bigLineNodes adds the plant's 108 nodes.
func bigLineNodes(p *scenefixtures.Plant) {
	strp := func(s string) *string { return &s }
	// 108 plant nodes: four groups, their members, the line's positions, and
	// the rest of a plant.
	nid := int64(0)
	seen := map[string]bool{}
	node := func(name, typ, parent string) {
		if seen[name] {
			return
		}
		seen[name] = true
		nid++
		n := scenefixtures.Node{ID: nid, Name: name, NodeTypeCode: strp(typ)}
		if parent != "" {
			n.ParentName = strp(parent)
		}
		p.Nodes = append(p.Nodes, n)
	}
	groups := []struct {
		name    string
		members []string
	}{
		{BigLineSource, []string{"BSM_038", "BSM_039", "BSM_040", "BSM_041"}},
		{BigLineDestination, []string{"BUN_002", "BUN_003", "BUN_004", "BUN_005"}},
		{"BIG Supermarket", []string{"BSM_001", "BSM_002", "BSM_003", "BSM_004", "BSM_005", "BSM_006", "BSM_007", "BSM_008"}},
		{"BIG FG stage 1", []string{"BFG_001", "BFG_002", "BFG_003", "BFG_004"}},
	}
	for _, g := range groups {
		node(g.name, "NGRP", "")
	}
	for _, g := range groups {
		for _, m := range g.members {
			node(m, "storage", g.name)
		}
	}
	for _, n := range BigLineMain {
		node(n, "station", "")
	}
	for i := 1; len(p.Nodes) < 108; i++ {
		node(fmt.Sprintf("BXN_%03d", i), "storage", "")
	}
}
